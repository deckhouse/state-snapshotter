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

// Package snapshotexport implements the SnapshotExport reconciler: it walks a root Snapshot's bound
// SnapshotContent tree and, for every data leaf, restores the durable VolumeSnapshotContent into a
// PVC (VolumeRestoreRequest) and serves that PVC over a DataExport. All intermediate objects (VRR,
// restored PVC, DataExport) live in the SnapshotExport namespace and are owner-tied to it for GC.
package snapshotexport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase/restore"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

const (
	// finalizer guards explicit cleanup; child objects are also owner-GC'd.
	finalizer = snapshotpkg.FinalizerSnapshotExport
	// defaultDataExportTTL bounds how long a served export lives after the last request.
	defaultDataExportTTL = "24h"
	// requeueShort is the steady-state poll interval while VRR/DataExport converge.
	requeueShort = 5 * time.Second
	// requeueFailed backs off when a leaf reports a terminal VRR/DataExport failure, so a stuck
	// export does not hot-loop the API server while staying observable in status.
	requeueFailed = 30 * time.Second
	// requeueReadyRefresh re-checks a published export so a later DataExport TTL expiry is reflected
	// in status (there is no watch on the cross-repo DataExport).
	requeueReadyRefresh = 5 * time.Minute
)

// RBAC is maintained in templates/.../rbac-for-us.yaml (repo RBAC SSOT); no +kubebuilder:rbac
// markers live in controller code.

// SnapshotExportReconciler reconciles SnapshotExport resources.
type SnapshotExportReconciler struct {
	Client    client.Client
	APIReader client.Reader
	Scheme    *runtime.Scheme
	resolver  *restore.Resolver
}

// AddSnapshotExportControllerToManager registers the reconciler.
func AddSnapshotExportControllerToManager(mgr ctrl.Manager) error {
	r := &SnapshotExportReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Scheme:    mgr.GetScheme(),
		resolver:  restore.NewResolver(mgr.GetClient()),
	}
	// No Owns(PVC): the restored PVC is controller-owned by the VRR/ObjectKeeper, not by this
	// reconciler (it only holds a non-controller ownerRef), so an Owns watch would never fire here.
	// Progress is driven by the requeue cadence instead.
	return ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.SnapshotExport{}).
		Complete(r)
}

// dataLeaf is one data snapshot collected from the tree walk.
type dataLeaf struct {
	snapshotID       string
	artifactName     string
	volumeMode       string
	fsType           string
	accessModes      []string
	storageClassName string
}

func (r *SnapshotExportReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	export := &storagev1alpha1.SnapshotExport{}
	if err := r.Client.Get(ctx, req.NamespacedName, export); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if export.DeletionTimestamp != nil {
		// Child VRR/DataExport/PVC are owner-GC'd with the SnapshotExport; just drop the finalizer.
		if controllerutilRemoveFinalizer(export) {
			if err := r.Client.Update(ctx, export); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	if controllerutilAddFinalizer(export) {
		if err := r.Client.Update(ctx, export); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	snapName := export.Spec.SnapshotRef.Name
	if snapName == "" {
		return r.setReady(ctx, export, metav1.ConditionFalse,
			storagev1alpha1.SnapshotExportReasonInvalidSpec, "spec.snapshotRef.name is empty")
	}

	leaves, err := r.collectDataLeaves(ctx, export.Namespace, snapName)
	if err != nil {
		// The tree is not resolvable yet (snapshot not Ready, content missing). Surface and requeue.
		if _, serr := r.setReady(ctx, export, metav1.ConditionFalse,
			storagev1alpha1.SnapshotExportReasonSnapshotNotReady, err.Error()); serr != nil {
			return ctrl.Result{}, serr
		}
		return ctrl.Result{RequeueAfter: requeueShort}, nil
	}

	owner := r.ownerRef(export)
	entries := make([]storagev1alpha1.SnapshotExportDataEntry, 0, len(leaves))
	allReady := true
	anyFailed := false
	var details []string
	for _, leaf := range leaves {
		res, perr := r.reconcileLeaf(ctx, export, owner, leaf)
		if perr != nil {
			return ctrl.Result{}, perr
		}
		entries = append(entries, res.entry)
		if !res.ready {
			allReady = false
			if res.detail != "" {
				details = append(details, res.detail)
			}
			if res.failed {
				anyFailed = true
			}
		}
	}

	indexURL := aggregatedSubresourceURL(export.Namespace, snapName, "index")
	manifestsURL := aggregatedSubresourceURL(export.Namespace, snapName, "manifests")
	if err := r.publishStatus(ctx, export, indexURL, manifestsURL, entries, allReady, anyFailed, details); err != nil {
		return ctrl.Result{}, err
	}

	switch {
	case allReady:
		// Re-check periodically so a later DataExport TTL expiry is reflected (no DataExport watch).
		return ctrl.Result{RequeueAfter: requeueReadyRefresh}, nil
	case anyFailed:
		logger.Info("SnapshotExport has failing data leaves", "details", strings.Join(details, "; "))
		return ctrl.Result{RequeueAfter: requeueFailed}, nil
	default:
		logger.V(1).Info("SnapshotExport waiting for data endpoints", "leaves", len(leaves))
		return ctrl.Result{RequeueAfter: requeueShort}, nil
	}
}

// collectDataLeaves resolves the snapshot run tree and flattens every data binding into a leaf.
func (r *SnapshotExportReconciler) collectDataLeaves(ctx context.Context, namespace, snapName string) ([]dataLeaf, error) {
	root, err := r.resolver.ResolveRestoreTree(ctx, namespace, snapName)
	if err != nil {
		return nil, err
	}
	var leaves []dataLeaf
	var walk func(node *restore.RestoreNode)
	walk = func(node *restore.RestoreNode) {
		id := snapshotID(node.SnapshotRef)
		// Unified model: a snapshot carries at most one data volume, and the index (v1) keys data by
		// node taking the first binding deterministically. Mirror that here so every status entry has a
		// unique snapshotID (status.dataSnapshots is a listMap keyed on snapshotID); emitting one entry
		// per binding would produce duplicate keys for a multi-volume node and wedge the status write.
		for _, b := range node.DataBindings {
			if b.Artifact.Kind != kindVolumeSnapshotContent || b.Artifact.Name == "" {
				continue
			}
			// volumeMode is intentionally not defaulted: a wrong Block/Filesystem mode silently
			// mis-restores the volume, so an unknown mode fails closed in reconcileLeaf.
			leaves = append(leaves, dataLeaf{
				snapshotID:       id,
				artifactName:     b.Artifact.Name,
				volumeMode:       b.VolumeMode,
				fsType:           b.FsType,
				accessModes:      b.AccessModes,
				storageClassName: b.StorageClassName,
			})
			break
		}
		for _, c := range node.Children {
			walk(c)
		}
	}
	walk(root)
	sort.Slice(leaves, func(i, j int) bool {
		if leaves[i].snapshotID == leaves[j].snapshotID {
			return leaves[i].artifactName < leaves[j].artifactName
		}
		return leaves[i].snapshotID < leaves[j].snapshotID
	})
	return leaves, nil
}

// leafResult is the per-leaf reconcile outcome surfaced into status. detail carries a human-readable
// progress/failure message; failed marks a terminal child (VRR/DataExport) failure so the controller
// backs off and reports it instead of hot-looping silently.
type leafResult struct {
	entry  storagev1alpha1.SnapshotExportDataEntry
	ready  bool
	failed bool
	detail string
}

// reconcileLeaf drives VRR(VSC)->PVC->DataExport for a single data leaf and returns its outcome.
func (r *SnapshotExportReconciler) reconcileLeaf(ctx context.Context, export *storagev1alpha1.SnapshotExport, owner metav1.OwnerReference, leaf dataLeaf) (leafResult, error) {
	base := resourceBaseName(export, leaf.snapshotID+"/"+leaf.artifactName)
	pvcName := base
	res := leafResult{entry: storagev1alpha1.SnapshotExportDataEntry{SnapshotID: leaf.snapshotID}}

	// Fail closed when capture did not record a volumeMode: defaulting (Block vs Filesystem) risks a
	// silent wrong-mode restore.
	if leaf.volumeMode == "" {
		res.failed = true
		res.detail = fmt.Sprintf("%s: volumeMode unknown (capture did not record it)", leaf.snapshotID)
		return res, nil
	}

	// 1. Ensure the VRR (VSC -> PVC).
	vrr, err := r.ensureUnstructured(ctx, volumeRestoreRequestGVK, export.Namespace, base, func() *unstructured.Unstructured {
		return newVolumeRestoreRequest(export.Namespace, base, owner, leaf.artifactName, export.Namespace, pvcName,
			leaf.storageClassName, leaf.volumeMode, leaf.fsType, leaf.accessModes)
	})
	if err != nil {
		return res, err
	}
	vrrReady, vrrReason := readReadyCondition(vrr)
	if !vrrReady || vrrReason != reasonCompleted {
		res.failed = isTerminalReason(vrrReason)
		res.detail = fmt.Sprintf("%s: VolumeRestoreRequest not ready (%s)", leaf.snapshotID, reasonOrUnknown(vrrReason))
		return res, nil
	}

	// 2. Hold the restored PVC: add the SnapshotExport as a non-controller owner so it survives the
	//    SF TTL-scanner deleting the VRR and is GC'd when the export goes away.
	if err := r.holdRestoredPVC(ctx, export, pvcName); err != nil {
		if apierrors.IsNotFound(err) {
			res.detail = fmt.Sprintf("%s: restored PVC not present yet", leaf.snapshotID)
			return res, nil
		}
		return res, err
	}

	// 3. Ensure the DataExport serving the PVC.
	de, err := r.ensureUnstructured(ctx, dataExportGVK, export.Namespace, base, func() *unstructured.Unstructured {
		return newDataExport(export.Namespace, base, owner, pvcName, defaultDataExportTTL, export.Spec.Publish)
	})
	if err != nil {
		return res, err
	}
	deReady, deReason := readReadyCondition(de)
	if !deReady {
		res.failed = isTerminalReason(deReason)
		res.detail = fmt.Sprintf("%s: DataExport not ready (%s)", leaf.snapshotID, reasonOrUnknown(deReason))
		return res, nil
	}

	// A published endpoint carries an externally-trusted cert (no CA needed); the internal endpoint
	// publishes its CA in status.ca, which the client must trust to download over TLS.
	url := nestedStr(de, "status", "publicURL")
	ca := ""
	if url == "" {
		url = nestedStr(de, "status", "url")
		ca = nestedStr(de, "status", "ca")
	}
	res.entry.DataURL = url
	res.entry.DataCA = ca
	res.entry.Ready = url != ""
	res.ready = res.entry.Ready
	if !res.ready {
		res.detail = fmt.Sprintf("%s: DataExport ready but no URL published", leaf.snapshotID)
	}
	return res, nil
}

// ensureUnstructured gets the object, creating it from build() when absent. Returns the live object.
func (r *SnapshotExportReconciler) ensureUnstructured(ctx context.Context, gvk schema.GroupVersionKind, namespace, name string, build func() *unstructured.Unstructured) (*unstructured.Unstructured, error) {
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

// holdRestoredPVC adds the SnapshotExport as an additional owner of the restored PVC (retention +
// GC anchor). It is idempotent.
func (r *SnapshotExportReconciler) holdRestoredPVC(ctx context.Context, export *storagev1alpha1.SnapshotExport, pvcName string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		pvc := &corev1.PersistentVolumeClaim{}
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: export.Namespace, Name: pvcName}, pvc); err != nil {
			return err
		}
		owner := r.holdOwnerRef(export)
		for _, ref := range pvc.OwnerReferences {
			if ref.UID == owner.UID {
				return nil
			}
		}
		base := pvc.DeepCopy()
		pvc.OwnerReferences = append(pvc.OwnerReferences, owner)
		return r.Client.Patch(ctx, pvc, client.MergeFrom(base))
	})
}

func (r *SnapshotExportReconciler) publishStatus(ctx context.Context, export *storagev1alpha1.SnapshotExport, indexURL, manifestsURL string, entries []storagev1alpha1.SnapshotExportDataEntry, allReady, anyFailed bool, details []string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &storagev1alpha1.SnapshotExport{}
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: export.Namespace, Name: export.Name}, cur); err != nil {
			return err
		}
		cur.Status.ObservedGeneration = cur.Generation
		cur.Status.IndexURL = indexURL
		cur.Status.ManifestsURL = manifestsURL
		cur.Status.DataSnapshots = entries

		dataStatus := metav1.ConditionFalse
		dataReason := storagev1alpha1.SnapshotExportReasonDataPending
		dataMsg := fmt.Sprintf("%d data endpoint(s)", len(entries))
		switch {
		case allReady:
			dataStatus = metav1.ConditionTrue
			dataReason = storagev1alpha1.SnapshotExportReasonAllDataReady
		case anyFailed:
			dataReason = storagev1alpha1.SnapshotExportReasonDataExportFailed
		}
		if !allReady && len(details) > 0 {
			dataMsg = capJoin(details)
		}
		meta.SetStatusCondition(&cur.Status.Conditions, metav1.Condition{
			Type:               storagev1alpha1.SnapshotExportConditionDataReady,
			Status:             dataStatus,
			Reason:             dataReason,
			Message:            dataMsg,
			ObservedGeneration: cur.Generation,
		})

		readyStatus := metav1.ConditionFalse
		readyReason := storagev1alpha1.SnapshotExportReasonDataPending
		readyMsg := "waiting for index, manifests and data endpoints"
		switch {
		case allReady && indexURL != "" && manifestsURL != "":
			readyStatus = metav1.ConditionTrue
			readyReason = storagev1alpha1.SnapshotExportReasonPublished
			readyMsg = "index, manifests and data endpoints published"
		case anyFailed:
			readyReason = storagev1alpha1.SnapshotExportReasonDataExportFailed
			readyMsg = capJoin(details)
		}
		meta.SetStatusCondition(&cur.Status.Conditions, metav1.Condition{
			Type:               storagev1alpha1.SnapshotExportConditionReady,
			Status:             readyStatus,
			Reason:             readyReason,
			Message:            readyMsg,
			ObservedGeneration: cur.Generation,
		})
		return r.Client.Status().Update(ctx, cur)
	})
}

func (r *SnapshotExportReconciler) setReady(ctx context.Context, export *storagev1alpha1.SnapshotExport, status metav1.ConditionStatus, reason, message string) (ctrl.Result, error) {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &storagev1alpha1.SnapshotExport{}
		if gerr := r.Client.Get(ctx, client.ObjectKey{Namespace: export.Namespace, Name: export.Name}, cur); gerr != nil {
			return gerr
		}
		cur.Status.ObservedGeneration = cur.Generation
		meta.SetStatusCondition(&cur.Status.Conditions, metav1.Condition{
			Type:               storagev1alpha1.SnapshotExportConditionReady,
			Status:             status,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: cur.Generation,
		})
		return r.Client.Status().Update(ctx, cur)
	})
	return ctrl.Result{}, err
}

// ownerRef is the controlling owner stamped on objects this reconciler exclusively creates (VRR,
// DataExport). BlockOwnerDeletion is intentionally false so creating children does not require
// update on snapshotexports/finalizers under OwnerReferencesPermissionEnforcement.
func (r *SnapshotExportReconciler) ownerRef(export *storagev1alpha1.SnapshotExport) metav1.OwnerReference {
	t, f := true, false
	return metav1.OwnerReference{
		APIVersion:         storagev1alpha1.SchemeGroupVersion.String(),
		Kind:               "SnapshotExport",
		Name:               export.Name,
		UID:                export.UID,
		Controller:         &t,
		BlockOwnerDeletion: &f,
	}
}

// holdOwnerRef is a NON-controller owner added to the restored PVC. The VRR/ObjectKeeper is already
// the PVC's controller owner, so a second controller=true ref would be rejected ("only one reference
// can have Controller set to true"). This ref provides retention plus owner-GC of the PVC with the
// export without claiming control.
func (r *SnapshotExportReconciler) holdOwnerRef(export *storagev1alpha1.SnapshotExport) metav1.OwnerReference {
	f := false
	return metav1.OwnerReference{
		APIVersion:         storagev1alpha1.SchemeGroupVersion.String(),
		Kind:               "SnapshotExport",
		Name:               export.Name,
		UID:                export.UID,
		Controller:         &f,
		BlockOwnerDeletion: &f,
	}
}

func snapshotID(ref snapshotpkg.ObjectRef) string {
	return ref.Kind + "--" + ref.Namespace + "--" + ref.Name
}

// resourceBaseName derives an RFC1123-safe, stable, short, export-unique name for the per-leaf
// VRR/PVC/DataExport. The export UID is folded into the hash so two SnapshotExports whose names share
// the first 40 chars cannot collide on (and silently adopt) each other's intermediate objects.
func resourceBaseName(export *storagev1alpha1.SnapshotExport, key string) string {
	prefix := export.Name
	if len(prefix) > 40 {
		prefix = prefix[:40]
	}
	h := sha256.Sum256([]byte(string(export.UID) + "/" + key))
	return fmt.Sprintf("se-%s-%s", prefix, hex.EncodeToString(h[:8]))
}

// capJoin joins detail strings with "; ", capping the total length so the status message stays bounded.
func capJoin(details []string) string {
	msg := strings.Join(details, "; ")
	const maxLen = 1024
	if len(msg) > maxLen {
		return msg[:maxLen] + "..."
	}
	return msg
}

func aggregatedSubresourceURL(namespace, snapName, sub string) string {
	return fmt.Sprintf("/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/%s/snapshots/%s/%s", namespace, snapName, sub)
}

func controllerutilAddFinalizer(export *storagev1alpha1.SnapshotExport) bool {
	for _, f := range export.Finalizers {
		if f == finalizer {
			return false
		}
	}
	export.Finalizers = append(export.Finalizers, finalizer)
	return true
}

func controllerutilRemoveFinalizer(export *storagev1alpha1.SnapshotExport) bool {
	out := export.Finalizers[:0]
	removed := false
	for _, f := range export.Finalizers {
		if f == finalizer {
			removed = true
			continue
		}
		out = append(out, f)
	}
	export.Finalizers = out
	return removed
}
