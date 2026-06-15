/*
Copyright 2026 Flant JSC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package snapshotimport implements the SnapshotImport reconciler. It publishes resumable upload
// endpoints (index, per-node manifests, per-data volume data), populates a PVC per data snapshot via
// DataImport, captures each populated PVC into a durable VolumeSnapshotContent via
// VolumeCaptureRequest, reconstructs a per-node ManifestCheckpoint from the uploaded manifests, then
// pre-provisions the cluster-scoped SnapshotContent tree and the statically-bound snapshot CRs.
//
// All intermediate objects (DataImport, populated PVC, VolumeCaptureRequest) live in the
// SnapshotImport namespace and are owner-tied to it for GC; the recreated snapshot tree
// (SnapshotContent, ManifestCheckpoint, Snapshot/demo CRs) is the durable deliverable and is NOT
// owned by the transient SnapshotImport.
package snapshotimport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	manifestv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/volumecapture"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase/restore"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

const (
	finalizer            = snapshotpkg.FinalizerSnapshotImport
	defaultDataImportTTL = "24h"
	// requeueShort is the steady-state poll interval while DataImport/capture converge and the
	// recovery interval for fail-closed validation branches (no StorageClass watch).
	requeueShort = 5 * time.Second
)

// RBAC is maintained in templates/.../rbac-for-us.yaml (repo RBAC SSOT); no +kubebuilder:rbac
// markers live in controller code.

// SnapshotImportReconciler reconciles SnapshotImport resources.
type SnapshotImportReconciler struct {
	Client client.Client
	// Direct is a cache-bypassing client used for the internal blob chunks and ManifestCheckpoint
	// reconstruction (those types are not watched by informers).
	Direct client.Client
	Scheme *runtime.Scheme
	blobs  *usecase.ImportBlobStore
}

// AddSnapshotImportControllerToManager registers the reconciler.
func AddSnapshotImportControllerToManager(mgr ctrl.Manager) error {
	direct, err := client.New(mgr.GetConfig(), client.Options{Scheme: mgr.GetScheme(), Mapper: mgr.GetRESTMapper()})
	if err != nil {
		return fmt.Errorf("create direct client for SnapshotImport: %w", err)
	}
	blobLogger, err := logger.NewLogger("info")
	if err != nil {
		return fmt.Errorf("create logger for SnapshotImport: %w", err)
	}
	r := &SnapshotImportReconciler{
		Client: mgr.GetClient(),
		Direct: direct,
		Scheme: mgr.GetScheme(),
		blobs:  usecase.NewImportBlobStore(direct, blobLogger),
	}
	// No Owns(PVC): the populated PVC is created and owned by SVDM's DataImport, not by this
	// reconciler, so an Owns watch would never fire here. Progress is driven by the requeue cadence.
	return ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.SnapshotImport{}).
		Complete(r)
}

// dataNode is one data-bearing snapshot node derived from the uploaded index.
type dataNode struct {
	id               string
	storageClassName string // mapped target StorageClass
	volumeMode       string
	fsType           string
	accessModes      []string
	size             int64
}

func (r *SnapshotImportReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	imp := &storagev1alpha1.SnapshotImport{}
	if err := r.Client.Get(ctx, req.NamespacedName, imp); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if imp.DeletionTimestamp != nil {
		return r.reconcileDelete(ctx, imp)
	}
	if addFinalizer(imp) {
		if err := r.Client.Update(ctx, imp); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Stage 0: always publish the index/manifests upload endpoints.
	if err := r.publishUploadURLs(ctx, imp); err != nil {
		return ctrl.Result{}, err
	}

	// Stage 1: wait for the index upload (the upload handler flips IndexReceived, which re-triggers us).
	if !meta.IsStatusConditionTrue(imp.Status.Conditions, storagev1alpha1.SnapshotImportConditionIndexReceived) {
		return ctrl.Result{}, nil
	}

	// Stage 2: read + parse the uploaded index.
	idx, err := r.readIndex(ctx, imp)
	if err != nil {
		// Recoverable (a re-upload or transient read fixes it); requeue since nothing else wakes us.
		if serr := r.setCondition(ctx, imp, storagev1alpha1.SnapshotImportConditionUploadsPrepared,
			metav1.ConditionFalse, storagev1alpha1.SnapshotImportReasonIndexUnreadable, err.Error()); serr != nil {
			return ctrl.Result{}, serr
		}
		return ctrl.Result{RequeueAfter: requeueShort}, nil
	}

	// The capture leg latches: once Captured=True, the populating DataImports/PVCs have been deleted
	// (Stage 8) and MUST NOT be re-created, or the import would regress and leak a PVC every requeue.
	// Reuse the already-published per-node entries (they carry capturedSnapshotContentName) instead.
	captured := meta.IsStatusConditionTrue(imp.Status.Conditions, storagev1alpha1.SnapshotImportConditionCaptured)
	var entries []storagev1alpha1.SnapshotImportDataEntry

	if captured {
		entries = imp.Status.DataSnapshots
	} else {
		// Stage 3: resolve + validate target StorageClasses and volume sizes (fail closed).
		nodes, missing, rerr := r.resolveDataNodes(ctx, imp, idx)
		if rerr != nil {
			return ctrl.Result{}, rerr
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			msg := fmt.Sprintf("unresolved target StorageClasses: %v (set spec.storageClassMapping)", missing)
			if serr := r.setCondition(ctx, imp, storagev1alpha1.SnapshotImportConditionUploadsPrepared,
				metav1.ConditionFalse, storagev1alpha1.SnapshotImportReasonStorageClassMappingRequired, msg); serr != nil {
				return ctrl.Result{}, serr
			}
			// No StorageClass watch wakes us; requeue so a newly-created SC is picked up.
			return ctrl.Result{RequeueAfter: requeueShort}, nil
		}
		if sizeless := nodesMissingSize(nodes); len(sizeless) > 0 {
			sort.Strings(sizeless)
			msg := fmt.Sprintf("data nodes with unknown volume size: %v (index missing restoreSize)", sizeless)
			if serr := r.setCondition(ctx, imp, storagev1alpha1.SnapshotImportConditionUploadsPrepared,
				metav1.ConditionFalse, storagev1alpha1.SnapshotImportReasonDataSizeUnknown, msg); serr != nil {
				return ctrl.Result{}, serr
			}
			return ctrl.Result{RequeueAfter: requeueShort}, nil
		}

		// Stage 4-7: drive per-node DataImport -> upload -> capture.
		var allUploadReady, allUploaded, allCaptured bool
		entries, allUploadReady, allUploaded, allCaptured, err = r.reconcileDataNodes(ctx, imp, nodes)
		if err != nil {
			return ctrl.Result{}, err
		}
		if err := r.publishDataSnapshots(ctx, imp, entries); err != nil {
			return ctrl.Result{}, err
		}

		uploadsPrepared := metav1.ConditionFalse
		if allUploadReady {
			uploadsPrepared = metav1.ConditionTrue
		}
		if err := r.setCondition(ctx, imp, storagev1alpha1.SnapshotImportConditionUploadsPrepared,
			uploadsPrepared, uploadsReason(allUploadReady), fmt.Sprintf("%d upload endpoint(s)", len(entries))); err != nil {
			return ctrl.Result{}, err
		}
		if !allUploadReady {
			return ctrl.Result{RequeueAfter: requeueShort}, nil
		}
		if !allUploaded {
			logger.V(1).Info("SnapshotImport waiting for data uploads to finish")
			return ctrl.Result{RequeueAfter: requeueShort}, nil
		}
		if err := r.setCondition(ctx, imp, storagev1alpha1.SnapshotImportConditionDataReceived, metav1.ConditionTrue,
			storagev1alpha1.SnapshotImportReasonAllDataUploaded, "all data uploads finished"); err != nil {
			return ctrl.Result{}, err
		}
		if !allCaptured {
			return ctrl.Result{RequeueAfter: requeueShort}, nil
		}
		if err := r.setCondition(ctx, imp, storagev1alpha1.SnapshotImportConditionCaptured, metav1.ConditionTrue,
			storagev1alpha1.SnapshotImportReasonAllCaptured, "all populated PVCs captured into VolumeSnapshotContent"); err != nil {
			return ctrl.Result{}, err
		}

		// Stage 8: cleanup populated PVCs once, right after capture (DataImport first to release the
		// SVDM finalizer). Subsequent reconciles take the captured branch and never re-create them.
		if err := r.cleanupPopulatedPVCs(ctx, imp, nodes); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Stage 9: manifests must be uploaded before reconstructing the tree.
	if !meta.IsStatusConditionTrue(imp.Status.Conditions, storagev1alpha1.SnapshotImportConditionManifestsReceived) {
		logger.V(1).Info("SnapshotImport waiting for manifests upload")
		return ctrl.Result{RequeueAfter: requeueShort}, nil
	}

	// Stage 10: reconstruct per-node ManifestCheckpoints and pre-provision the snapshot tree
	// (idempotent: re-running on an already-imported tree is a no-op).
	if err := r.reconstructTree(ctx, imp, idx, entries); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, r.setCondition(ctx, imp, storagev1alpha1.SnapshotImportConditionReady,
		metav1.ConditionTrue, storagev1alpha1.SnapshotImportReasonImported, "snapshot tree pre-provisioned")
}

// nodesMissingSize returns the ids of data nodes whose volume size is unknown (<=0). A sizeless PVC
// template would be rejected by SVDM, so the import fails closed instead of provisioning a bad PVC.
func nodesMissingSize(nodes []dataNode) []string {
	var out []string
	for i := range nodes {
		if nodes[i].size <= 0 {
			out = append(out, nodes[i].id)
		}
	}
	return out
}

// publishUploadURLs writes the index/manifests upload endpoints into status (idempotent).
func (r *SnapshotImportReconciler) publishUploadURLs(ctx context.Context, imp *storagev1alpha1.SnapshotImport) error {
	indexURL := importSubresourceURL(imp.Namespace, imp.Name, "index")
	manifestsURL := importSubresourceURL(imp.Namespace, imp.Name, "manifests")
	if imp.Status.IndexUploadURL == indexURL && imp.Status.ManifestsUploadURL == manifestsURL {
		return nil
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &storagev1alpha1.SnapshotImport{}
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(imp), cur); err != nil {
			return err
		}
		cur.Status.IndexUploadURL = indexURL
		cur.Status.ManifestsUploadURL = manifestsURL
		cur.Status.ObservedGeneration = cur.Generation
		return r.Client.Status().Update(ctx, cur)
	})
}

// readIndex loads and parses the uploaded index blob.
func (r *SnapshotImportReconciler) readIndex(ctx context.Context, imp *storagev1alpha1.SnapshotImport) (*restore.Index, error) {
	key := usecase.ImportBlobKey(imp.Namespace, imp.Name, usecase.ImportBlobKindIndex)
	raw, err := r.blobs.Read(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("read index blob: %w", err)
	}
	idx := &restore.Index{}
	if err := json.Unmarshal(raw, idx); err != nil {
		return nil, fmt.Errorf("parse index: %w", err)
	}
	if idx.Version == "" || len(idx.Snapshots) == 0 {
		return nil, fmt.Errorf("index is empty or missing version")
	}
	return idx, nil
}

// resolveDataNodes maps each data-bearing index node to a target StorageClass and returns the nodes
// plus the set of unresolved target StorageClasses.
func (r *SnapshotImportReconciler) resolveDataNodes(ctx context.Context, imp *storagev1alpha1.SnapshotImport, idx *restore.Index) ([]dataNode, []string, error) {
	var nodes []dataNode
	missingSet := map[string]struct{}{}
	for i := range idx.Snapshots {
		s := &idx.Snapshots[i]
		if !s.HasData || s.Data == nil {
			continue
		}
		source := s.Data.StorageClassName
		target := source
		if mapped, ok := imp.Spec.StorageClassMapping[source]; ok && mapped != "" {
			target = mapped
		}
		if target == "" {
			missingSet[fmt.Sprintf("<empty for %s>", s.ID)] = struct{}{}
		} else {
			sc := &storagev1.StorageClass{}
			if err := r.Client.Get(ctx, client.ObjectKey{Name: target}, sc); err != nil {
				if apierrors.IsNotFound(err) {
					missingSet[target] = struct{}{}
				} else {
					return nil, nil, fmt.Errorf("get StorageClass %s: %w", target, err)
				}
			}
		}
		nodes = append(nodes, dataNode{
			id:               s.ID,
			storageClassName: target,
			volumeMode:       s.Data.VolumeMode,
			fsType:           s.Data.FsType,
			accessModes:      s.Data.AccessModes,
			size:             s.Data.Size,
		})
	}
	missing := make([]string, 0, len(missingSet))
	for k := range missingSet {
		missing = append(missing, k)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].id < nodes[j].id })
	return nodes, missing, nil
}

// reconcileDataNodes ensures a DataImport (and, once uploaded, a VolumeCaptureRequest) per data node
// and reports aggregate readiness. It returns one status entry per node.
func (r *SnapshotImportReconciler) reconcileDataNodes(ctx context.Context, imp *storagev1alpha1.SnapshotImport, nodes []dataNode) ([]storagev1alpha1.SnapshotImportDataEntry, bool, bool, bool, error) {
	owner := r.ownerRef(imp)
	entries := make([]storagev1alpha1.SnapshotImportDataEntry, 0, len(nodes))
	allUploadReady, allUploaded, allCaptured := true, true, true

	for _, n := range nodes {
		base := resourceBaseName(imp.Name, n.id)
		entry := storagev1alpha1.SnapshotImportDataEntry{SnapshotID: n.id}

		di, err := r.ensureUnstructured(ctx, dataImportGVK, imp.Namespace, base, func() *unstructured.Unstructured {
			return newDataImport(imp.Namespace, base, owner, dataImportSpec{
				pvcName:          base,
				storageClassName: n.storageClassName,
				volumeMode:       n.volumeMode,
				accessModes:      n.accessModes,
				sizeQuantity:     sizeToQuantity(n.size),
				ttl:              defaultDataImportTTL,
				publish:          imp.Spec.Publish,
			})
		})
		if err != nil {
			return nil, false, false, false, err
		}
		// Prefer a published (externally-trusted) endpoint; otherwise use the internal URL and surface
		// its CA so the client can trust it over TLS.
		if pub := nestedStr(di, "status", "publicURL"); pub != "" {
			entry.UploadURL = pub
			entry.UploadCA = ""
		} else {
			entry.UploadURL = nestedStr(di, "status", "url")
			entry.UploadCA = nestedStr(di, "status", "ca")
		}
		entry.UploadReady = readConditionTrue(di, conditionTypeReady) && entry.UploadURL != ""
		if !entry.UploadReady {
			allUploadReady = false
			entries = append(entries, entry)
			allUploaded, allCaptured = false, false
			continue
		}

		entry.Uploaded = readConditionTrue(di, conditionTypeUploadFinished)
		if !entry.Uploaded {
			allUploaded = false
			entries = append(entries, entry)
			allCaptured = false
			continue
		}

		// Capture the populated PVC into a durable VolumeSnapshotContent.
		captured, cerr := r.captureNode(ctx, imp, owner, base, &entry)
		if cerr != nil {
			return nil, false, false, false, cerr
		}
		if !captured {
			allCaptured = false
		}
		entries = append(entries, entry)
	}
	return entries, allUploadReady, allUploaded, allCaptured, nil
}

// captureNode ensures a VolumeCaptureRequest over the populated PVC and records the captured VSC.
func (r *SnapshotImportReconciler) captureNode(ctx context.Context, imp *storagev1alpha1.SnapshotImport, owner metav1.OwnerReference, base string, entry *storagev1alpha1.SnapshotImportDataEntry) (bool, error) {
	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: imp.Namespace, Name: base}, pvc); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	target := vcpkg.Target{
		UID:        string(pvc.UID),
		APIVersion: "v1",
		Kind:       "PersistentVolumeClaim",
		Name:       pvc.Name,
		Namespace:  pvc.Namespace,
	}
	vcr, err := r.ensureUnstructured(ctx, vcpkg.VolumeCaptureRequestGVK, imp.Namespace, base, func() *unstructured.Unstructured {
		return volumecapture.NewVolumeCaptureRequestObject(imp.Namespace, base, owner, []vcpkg.Target{target})
	})
	if err != nil {
		return false, err
	}
	if !readConditionTrue(vcr, conditionTypeReady) {
		return false, nil
	}
	refs, err := volumecapture.ParseVolumeCaptureDataRefs(vcr)
	if err != nil {
		return false, err
	}
	for _, ref := range refs {
		if ref.Artifact.Name != "" {
			entry.CapturedSnapshotContentName = ref.Artifact.Name
			return true, nil
		}
	}
	return false, nil
}

// cleanupPopulatedPVCs deletes the DataImport (releasing the SVDM finalizer) then the populated PVC,
// in that order, so the PVC does not hang in Terminating.
func (r *SnapshotImportReconciler) cleanupPopulatedPVCs(ctx context.Context, imp *storagev1alpha1.SnapshotImport, nodes []dataNode) error {
	for _, n := range nodes {
		base := resourceBaseName(imp.Name, n.id)

		di := &unstructured.Unstructured{}
		di.SetGroupVersionKind(dataImportGVK)
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: imp.Namespace, Name: base}, di); err == nil {
			if err := r.Client.Delete(ctx, di); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("delete DataImport %s: %w", base, err)
			}
		} else if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get DataImport %s: %w", base, err)
		}

		pvc := &corev1.PersistentVolumeClaim{}
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: imp.Namespace, Name: base}, pvc); err == nil {
			if pvc.DeletionTimestamp == nil {
				if err := r.Client.Delete(ctx, pvc); err != nil && !apierrors.IsNotFound(err) {
					return fmt.Errorf("delete populated PVC %s: %w", base, err)
				}
			}
		} else if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get populated PVC %s: %w", base, err)
		}
	}
	return nil
}

// reconstructTree reconstructs per-node ManifestCheckpoints from the uploaded manifests and
// pre-provisions the cluster-scoped SnapshotContent tree plus the statically-bound snapshot CRs.
func (r *SnapshotImportReconciler) reconstructTree(ctx context.Context, imp *storagev1alpha1.SnapshotImport, idx *restore.Index, entries []storagev1alpha1.SnapshotImportDataEntry) error {
	capturedByID := map[string]string{}
	for _, e := range entries {
		capturedByID[e.SnapshotID] = e.CapturedSnapshotContentName
	}
	dataByID := map[string]*restore.IndexData{}
	for i := range idx.Snapshots {
		s := &idx.Snapshots[i]
		if s.HasData && s.Data != nil {
			dataByID[s.ID] = s.Data
		}
	}
	scNameByID := map[string]string{}
	for i := range idx.Snapshots {
		scNameByID[idx.Snapshots[i].ID] = r.contentName(imp, idx.Snapshots[i].ID)
	}

	captureRef := &manifestv1alpha1.ObjectReference{Name: imp.Name, Namespace: imp.Namespace, UID: string(imp.UID)}

	// 1. Reconstruct each node's ManifestCheckpoint and create its SnapshotContent.
	for i := range idx.Snapshots {
		s := &idx.Snapshots[i]
		mcpName := usecase.ReconstructedManifestCheckpointName(imp.UID, s.ID)
		raw, err := r.blobs.Read(ctx, usecase.ImportManifestsBlobKey(imp.Namespace, imp.Name, s.ID))
		if err != nil {
			return fmt.Errorf("read manifests for node %s: %w", s.ID, err)
		}
		if err := usecase.ReconstructManifestCheckpoint(ctx, r.Direct, mcpName, imp.Namespace, captureRef, nil, raw); err != nil {
			return fmt.Errorf("reconstruct ManifestCheckpoint for node %s: %w", s.ID, err)
		}

		childRefs := make([]storagev1alpha1.SnapshotContentChildRef, 0, len(s.Children))
		for _, childID := range s.Children {
			if name, ok := scNameByID[childID]; ok {
				childRefs = append(childRefs, storagev1alpha1.SnapshotContentChildRef{Name: name})
			}
		}

		var dataRefs []storagev1alpha1.SnapshotDataBinding
		if data := dataByID[s.ID]; data != nil {
			vscName := capturedByID[s.ID]
			dataRefs = []storagev1alpha1.SnapshotDataBinding{{
				TargetUID: importDataTargetUID(imp.UID, s.ID),
				Target: storagev1alpha1.SnapshotSubjectRef{
					APIVersion: "v1",
					Kind:       "PersistentVolumeClaim",
					Name:       resourceBaseName(imp.Name, s.ID),
					Namespace:  imp.Namespace,
				},
				Artifact: storagev1alpha1.SnapshotDataArtifactRef{
					APIVersion: csiSnapshotAPIVersion,
					Kind:       "VolumeSnapshotContent",
					Name:       vscName,
				},
				VolumeMode:       data.VolumeMode,
				FsType:           data.FsType,
				AccessModes:      data.AccessModes,
				StorageClassName: data.StorageClassName,
			}}
		}

		if err := r.ensureSnapshotContent(ctx, scNameByID[s.ID], s, imp, idx, mcpName, dataRefs, childRefs); err != nil {
			return err
		}
	}

	// 2. Recreate the snapshot CRs statically bound to their content.
	for i := range idx.Snapshots {
		s := &idx.Snapshots[i]
		if err := r.ensureBoundSnapshot(ctx, imp, idx, s, scNameByID[s.ID]); err != nil {
			return err
		}
	}
	return nil
}

// ensureSnapshotContent creates a cluster-scoped SnapshotContent for one node if absent and publishes
// its status (manifestCheckpointName, dataRefs, children). The spec (incl. snapshotRef) is immutable.
// The back-reference apiVersion/kind/name must match the recreated snapshot CR so the static-bind
// handshake succeeds, so they are taken from the index node (s) and the import's recreated naming.
func (r *SnapshotImportReconciler) ensureSnapshotContent(ctx context.Context, name string, s *restore.IndexSnapshot, imp *storagev1alpha1.SnapshotImport, idx *restore.Index, mcpName string, dataRefs []storagev1alpha1.SnapshotDataBinding, childRefs []storagev1alpha1.SnapshotContentChildRef) error {
	content := &storagev1alpha1.SnapshotContent{}
	err := r.Direct.Get(ctx, client.ObjectKey{Name: name}, content)
	if apierrors.IsNotFound(err) {
		content = &storagev1alpha1.SnapshotContent{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: storagev1alpha1.SnapshotContentSpec{
				DeletionPolicy: storagev1alpha1.SnapshotContentDeletionPolicyRetain,
				SnapshotRef: &storagev1alpha1.SnapshotSubjectRef{
					APIVersion: s.APIVersion,
					Kind:       s.Kind,
					Name:       recreatedName(imp, idx, s),
					Namespace:  imp.Namespace,
				},
			},
		}
		if err := r.Client.Create(ctx, content); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create SnapshotContent %s: %w", name, err)
		}
	} else if err != nil {
		return fmt.Errorf("get SnapshotContent %s: %w", name, err)
	}

	// Read-after-write through the cache-bypassing client so a just-created content is visible.
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &storagev1alpha1.SnapshotContent{}
		if err := r.Direct.Get(ctx, client.ObjectKey{Name: name}, cur); err != nil {
			return err
		}
		cur.Status.ManifestCheckpointName = mcpName
		cur.Status.DataRefs = dataRefs
		cur.Status.ChildrenSnapshotContentRefs = childRefs
		meta.SetStatusCondition(&cur.Status.Conditions, metav1.Condition{
			Type:    snapshotpkg.ConditionReady,
			Status:  metav1.ConditionTrue,
			Reason:  storagev1alpha1.SnapshotImportReasonImported,
			Message: "pre-provisioned from import",
		})
		return r.Direct.Status().Update(ctx, cur)
	})
}

// ensureBoundSnapshot recreates a snapshot CR statically bound to its SnapshotContent. The root node
// becomes the typed Snapshot named spec.snapshotName; other nodes are created as unstructured CRs of
// their original kind, both carrying spec.source.snapshotContentName.
func (r *SnapshotImportReconciler) ensureBoundSnapshot(ctx context.Context, imp *storagev1alpha1.SnapshotImport, idx *restore.Index, s *restore.IndexSnapshot, contentName string) error {
	name := recreatedName(imp, idx, s)
	if isRootNode(idx, s) && s.Kind == "Snapshot" {
		snap := &storagev1alpha1.Snapshot{}
		err := r.Client.Get(ctx, client.ObjectKey{Namespace: imp.Namespace, Name: name}, snap)
		if apierrors.IsNotFound(err) {
			snap = &storagev1alpha1.Snapshot{
				ObjectMeta: metav1.ObjectMeta{Namespace: imp.Namespace, Name: name},
				Spec:       storagev1alpha1.SnapshotSpec{Source: &storagev1alpha1.SnapshotSource{SnapshotContentName: contentName}},
			}
			if err := r.Client.Create(ctx, snap); err != nil && !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("create Snapshot %s: %w", name, err)
			}
			return nil
		}
		return err
	}

	// Non-root / domain snapshot CR (unstructured): set spec.source.snapshotContentName.
	gv, err := schema.ParseGroupVersion(s.APIVersion)
	if err != nil {
		return fmt.Errorf("parse apiVersion %q for node %s: %w", s.APIVersion, s.ID, err)
	}
	gvk := gv.WithKind(s.Kind)
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	gerr := r.Client.Get(ctx, client.ObjectKey{Namespace: imp.Namespace, Name: name}, obj)
	if gerr == nil {
		return nil
	}
	if !apierrors.IsNotFound(gerr) {
		return fmt.Errorf("get %s %s: %w", s.Kind, name, gerr)
	}
	created := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": s.APIVersion,
		"kind":       s.Kind,
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": imp.Namespace,
		},
		"spec": map[string]interface{}{
			"source": map[string]interface{}{
				"snapshotContentName": contentName,
			},
		},
	}}
	created.SetGroupVersionKind(gvk)
	if err := r.Client.Create(ctx, created); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create %s %s: %w", s.Kind, name, err)
	}
	return nil
}

// reconcileDelete drops the finalizer after clearing the internal upload blobs. The recreated tree is
// the deliverable and is intentionally retained; intermediate DataImport/PVC/VCR are owner-GC'd.
//
// The per-node manifests blobs are cluster-scoped ManifestCheckpointContentChunks with no namespaced
// owner to garbage-collect them, so they are deleted explicitly here. Their keys are derived from the
// node ids in the index, which is read (best-effort) before the index blob itself is removed.
func (r *SnapshotImportReconciler) reconcileDelete(ctx context.Context, imp *storagev1alpha1.SnapshotImport) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	indexKey := usecase.ImportBlobKey(imp.Namespace, imp.Name, usecase.ImportBlobKindIndex)

	if raw, err := r.blobs.Read(ctx, indexKey); err == nil {
		idx := &restore.Index{}
		if jerr := json.Unmarshal(raw, idx); jerr == nil {
			for i := range idx.Snapshots {
				mk := usecase.ImportManifestsBlobKey(imp.Namespace, imp.Name, idx.Snapshots[i].ID)
				if derr := r.blobs.Delete(ctx, mk); derr != nil {
					logger.Error(derr, "delete import manifests blob during cleanup", "node", idx.Snapshots[i].ID)
				}
			}
		} else {
			logger.Error(jerr, "parse index during import cleanup; per-node manifests blobs may leak")
		}
	} else {
		logger.V(1).Info("index blob unavailable during import cleanup; per-node manifests blobs may leak", "error", err.Error())
	}

	if err := r.blobs.Delete(ctx, indexKey); err != nil {
		logger.Error(err, "delete import index blob during cleanup")
	}

	if removeFinalizer(imp) {
		if err := r.Client.Update(ctx, imp); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

// ensureUnstructured gets the object, creating it from build() when absent. Returns the live object.
func (r *SnapshotImportReconciler) ensureUnstructured(ctx context.Context, gvk schema.GroupVersionKind, namespace, name string, build func() *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, obj)
	if err == nil {
		return obj, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get %s %s/%s: %w", gvk.Kind, namespace, name, err)
	}
	created := build()
	if cerr := r.Client.Create(ctx, created); cerr != nil {
		if apierrors.IsAlreadyExists(cerr) {
			again := &unstructured.Unstructured{}
			again.SetGroupVersionKind(gvk)
			if gerr := r.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, again); gerr != nil {
				return nil, gerr
			}
			return again, nil
		}
		return nil, fmt.Errorf("create %s %s/%s: %w", gvk.Kind, namespace, name, cerr)
	}
	return created, nil
}

func (r *SnapshotImportReconciler) publishDataSnapshots(ctx context.Context, imp *storagev1alpha1.SnapshotImport, entries []storagev1alpha1.SnapshotImportDataEntry) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &storagev1alpha1.SnapshotImport{}
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(imp), cur); err != nil {
			return err
		}
		cur.Status.DataSnapshots = entries
		cur.Status.ObservedGeneration = cur.Generation
		return r.Client.Status().Update(ctx, cur)
	})
}

func (r *SnapshotImportReconciler) setCondition(ctx context.Context, imp *storagev1alpha1.SnapshotImport, condType string, status metav1.ConditionStatus, reason, message string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &storagev1alpha1.SnapshotImport{}
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(imp), cur); err != nil {
			return err
		}
		meta.SetStatusCondition(&cur.Status.Conditions, metav1.Condition{
			Type:               condType,
			Status:             status,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: cur.Generation,
		})
		cur.Status.ObservedGeneration = cur.Generation
		return r.Client.Status().Update(ctx, cur)
	})
}

func (r *SnapshotImportReconciler) ownerRef(imp *storagev1alpha1.SnapshotImport) metav1.OwnerReference {
	t := true
	return metav1.OwnerReference{
		APIVersion:         storagev1alpha1.SchemeGroupVersion.String(),
		Kind:               "SnapshotImport",
		Name:               imp.Name,
		UID:                imp.UID,
		Controller:         &t,
		BlockOwnerDeletion: &t,
	}
}

// contentName derives a deterministic cluster-scoped SnapshotContent name for one node.
func (r *SnapshotImportReconciler) contentName(imp *storagev1alpha1.SnapshotImport, nodeID string) string {
	h := sha256.Sum256([]byte(string(imp.UID) + "/" + nodeID))
	return "si-" + hex.EncodeToString(h[:10])
}

const csiSnapshotAPIVersion = "snapshot.storage.k8s.io/v1"

// recreatedName is the recreated CR name. The root node is (re)created under the requested
// spec.snapshotName; non-root/domain nodes keep their original name from the index. NOTE: non-root
// names are not import-namespaced, so importing into a namespace that already holds a tree with the
// same node names will collide — import into a clean namespace.
func recreatedName(imp *storagev1alpha1.SnapshotImport, idx *restore.Index, s *restore.IndexSnapshot) string {
	if isRootNode(idx, s) && imp.Spec.SnapshotName != "" {
		return imp.Spec.SnapshotName
	}
	return s.Name
}

func isRootNode(idx *restore.Index, s *restore.IndexSnapshot) bool {
	return s.ID == idx.RootSnapshot.ID
}

func importDataTargetUID(importUID types.UID, nodeID string) string {
	h := sha256.Sum256([]byte(string(importUID) + "/data/" + nodeID))
	return hex.EncodeToString(h[:16])
}

func sizeToQuantity(size int64) string {
	if size <= 0 {
		return ""
	}
	return fmt.Sprintf("%d", size)
}

func resourceBaseName(importName, key string) string {
	prefix := importName
	if len(prefix) > 40 {
		prefix = prefix[:40]
	}
	h := sha256.Sum256([]byte(key))
	return fmt.Sprintf("si-%s-%s", prefix, hex.EncodeToString(h[:8]))
}

func importSubresourceURL(namespace, name, sub string) string {
	return fmt.Sprintf("/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/%s/snapshotimports/%s/%s", namespace, name, sub)
}

func uploadsReason(ready bool) string {
	if ready {
		return storagev1alpha1.SnapshotImportReasonAllUploadsReady
	}
	return storagev1alpha1.SnapshotImportReasonUploadsPending
}

func addFinalizer(imp *storagev1alpha1.SnapshotImport) bool {
	for _, f := range imp.Finalizers {
		if f == finalizer {
			return false
		}
	}
	imp.Finalizers = append(imp.Finalizers, finalizer)
	return true
}

func removeFinalizer(imp *storagev1alpha1.SnapshotImport) bool {
	out := imp.Finalizers[:0]
	removed := false
	for _, f := range imp.Finalizers {
		if f == finalizer {
			removed = true
			continue
		}
		out = append(out, f)
	}
	imp.Finalizers = out
	return removed
}
