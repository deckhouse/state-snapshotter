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

	// Terminal Expired latch: once every data leaf's DataExport idled out we set Ready=False/Expired
	// and freed the heavy children. Any later reconcile must not re-ensure them, so we stop here. We
	// still re-run the (idempotent) free pass in case a prior cleanup was interrupted before the
	// latch was observed; remaining children are also owner-GC'd when the export is deleted.
	if isExpiredLatched(export) {
		if leaves, lerr := r.collectDataLeaves(ctx, export.Namespace, export.Spec.SnapshotRef.Name); lerr == nil {
			if cerr := r.freeHeavyChildren(ctx, export, leaves); cerr != nil {
				return ctrl.Result{}, cerr
			}
		}
		return ctrl.Result{}, nil
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
	anyConverging := false
	expiredCount := 0
	var details []string
	for _, leaf := range leaves {
		res, perr := r.reconcileLeaf(ctx, export, owner, leaf)
		if perr != nil {
			return ctrl.Result{}, perr
		}
		entries = append(entries, res.entry)
		if res.detail != "" {
			details = append(details, res.detail)
		}
		switch {
		case res.expired:
			allReady = false
			expiredCount++
		case !res.ready:
			allReady = false
			if res.failed {
				anyFailed = true
			} else {
				anyConverging = true
			}
		}
	}

	// All data leaves idled out: free the heavy children and latch the export into terminal Expired.
	// The latch is written first so an interrupted free pass cannot resurrect children on requeue.
	if len(leaves) > 0 && expiredCount == len(leaves) {
		logger.Info("SnapshotExport idle TTL elapsed for all data leaves; freeing children", "leaves", len(leaves))
		if _, err := r.setExpired(ctx, export, entries); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.freeHeavyChildren(ctx, export, leaves); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
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
	case anyConverging:
		logger.V(1).Info("SnapshotExport waiting for data endpoints", "leaves", len(leaves))
		return ctrl.Result{RequeueAfter: requeueShort}, nil
	default:
		// Some leaves expired while the rest stay ready: not terminal (live endpoints keep serving),
		// nothing left to converge. Poll slowly until the remaining leaves also idle out.
		return ctrl.Result{RequeueAfter: requeueReadyRefresh}, nil
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
// backs off and reports it instead of hot-looping silently; expired marks that the leaf's DataExport
// idled out past spec.ttl, in which case the controller stops re-ensuring it and lets Reconcile decide
// whether the whole export is terminal.
type leafResult struct {
	entry   storagev1alpha1.SnapshotExportDataEntry
	ready   bool
	failed  bool
	expired bool
	detail  string
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

	// If this leaf's DataExport already idled out past spec.ttl, report it as expired without
	// re-ensuring the VRR/DataExport. Re-ensuring would resurrect the torn-down endpoint and churn a
	// fresh VRR every reconcile; the existing Expired DataExport is what keeps that from happening.
	if expired, err := r.dataExportExpired(ctx, export.Namespace, base); err != nil {
		return res, err
	} else if expired {
		res.expired = true
		res.detail = fmt.Sprintf("%s: DataExport idle TTL elapsed", leaf.snapshotID)
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

	// 3. Ensure the DataExport serving the PVC. spec.ttl is propagated verbatim from the export so the
	//    SVDM pod owns the idle countdown; a wrong-format value is rejected by the DataExport CRD.
	de, err := r.ensureUnstructured(ctx, dataExportGVK, export.Namespace, base, func() *unstructured.Unstructured {
		return newDataExport(export.Namespace, base, owner, pvcName, export.Spec.TTL, export.Spec.Publish)
	})
	if err != nil {
		return res, err
	}
	deReady, deReason := readReadyCondition(de)
	if !deReady {
		// The DataExport may have idled out between the pre-check above and now; treat Expired as the
		// idle-TTL signal (handled by Reconcile), everything else as converging/terminal failure.
		if deReason == reasonExpired {
			res.expired = true
			res.detail = fmt.Sprintf("%s: DataExport idle TTL elapsed", leaf.snapshotID)
			return res, nil
		}
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

// setExpired latches the export into the terminal Expired state: Ready=False and DataReady=False with
// reason Expired. It records the (no-longer-serving) entries so status reflects what was exported, and
// returns no requeue. The export survives as a tombstone for the user/CLI to delete.
func (r *SnapshotExportReconciler) setExpired(ctx context.Context, export *storagev1alpha1.SnapshotExport, entries []storagev1alpha1.SnapshotExportDataEntry) (ctrl.Result, error) {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &storagev1alpha1.SnapshotExport{}
		if gerr := r.Client.Get(ctx, client.ObjectKey{Namespace: export.Namespace, Name: export.Name}, cur); gerr != nil {
			return gerr
		}
		cur.Status.ObservedGeneration = cur.Generation
		cur.Status.DataSnapshots = entries
		meta.SetStatusCondition(&cur.Status.Conditions, metav1.Condition{
			Type:               storagev1alpha1.SnapshotExportConditionDataReady,
			Status:             metav1.ConditionFalse,
			Reason:             storagev1alpha1.SnapshotExportReasonExpired,
			Message:            "data endpoints idle TTL elapsed",
			ObservedGeneration: cur.Generation,
		})
		meta.SetStatusCondition(&cur.Status.Conditions, metav1.Condition{
			Type:               storagev1alpha1.SnapshotExportConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             storagev1alpha1.SnapshotExportReasonExpired,
			Message:            "export idle TTL elapsed; delete it manually",
			ObservedGeneration: cur.Generation,
		})
		return r.Client.Status().Update(ctx, cur)
	})
	return ctrl.Result{}, err
}

// isExpiredLatched reports whether the export already carries the terminal Ready=False/Expired
// condition, in which case Reconcile must not re-ensure its children.
func isExpiredLatched(export *storagev1alpha1.SnapshotExport) bool {
	c := meta.FindStatusCondition(export.Status.Conditions, storagev1alpha1.SnapshotExportConditionReady)
	return c != nil && c.Status == metav1.ConditionFalse && c.Reason == storagev1alpha1.SnapshotExportReasonExpired
}

// dataExportExpired reports whether a DataExport with the given base name exists and has idled out
// (Ready=False / reason=Expired). A missing DataExport is not expired (NotFound -> false).
func (r *SnapshotExportReconciler) dataExportExpired(ctx context.Context, namespace, name string) (bool, error) {
	de := &unstructured.Unstructured{}
	de.SetGroupVersionKind(dataExportGVK)
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, de); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get DataExport %s/%s: %w", namespace, name, err)
	}
	ready, reason := readReadyCondition(de)
	return !ready && reason == reasonExpired, nil
}

// freeHeavyChildren proactively deletes the resource-heavy children of an idled-out export (per leaf:
// DataExport, then VolumeRestoreRequest, then the restored PVC) so the underlying storage is reclaimed
// while the export lingers as a tombstone. Order matters: the DataExport pod mounts the restored PVC,
// so deleting the DataExport first lets pvc-protection release the PVC. All deletes are idempotent;
// NotFound is ignored so a re-run after a partial pass is safe.
func (r *SnapshotExportReconciler) freeHeavyChildren(ctx context.Context, export *storagev1alpha1.SnapshotExport, leaves []dataLeaf) error {
	for _, leaf := range leaves {
		base := resourceBaseName(export, leaf.snapshotID+"/"+leaf.artifactName)
		if err := r.deleteUnstructured(ctx, dataExportGVK, export.Namespace, base); err != nil {
			return err
		}
		if err := r.deleteUnstructured(ctx, volumeRestoreRequestGVK, export.Namespace, base); err != nil {
			return err
		}
		if err := r.deletePVC(ctx, export.Namespace, base); err != nil {
			return err
		}
	}
	return nil
}

// deleteUnstructured deletes a named cross-repo object by GVK, ignoring NotFound (idempotent).
func (r *SnapshotExportReconciler) deleteUnstructured(ctx context.Context, gvk schema.GroupVersionKind, namespace, name string) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetNamespace(namespace)
	obj.SetName(name)
	if err := r.Client.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete %s %s/%s: %w", gvk.Kind, namespace, name, err)
	}
	return nil
}

// deletePVC deletes the restored PVC by name, ignoring NotFound (idempotent).
func (r *SnapshotExportReconciler) deletePVC(ctx context.Context, namespace, name string) error {
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
	if err := r.Client.Delete(ctx, pvc); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete PVC %s/%s: %w", namespace, name, err)
	}
	return nil
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
