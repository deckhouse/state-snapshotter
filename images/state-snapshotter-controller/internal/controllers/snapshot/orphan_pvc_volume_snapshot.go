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

package snapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/snapshotcontent"
	volumecapturectrl "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/volumecapture"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
)

// orphanPVCVolumeSnapshotNamePrefix marks VolumeSnapshots created by the namespace-root orphan-PVC
// data leg. The prefix is the ownership signal for stale-leaf cleanup.
const orphanPVCVolumeSnapshotNamePrefix = "nss-vs-"

// volumeSnapshotContentRetainPolicy is the deletionPolicy that keeps the bound VSC durable after the
// per-run VolumeSnapshot is deleted (durable-artifact contract, ADR 2026-06-09 / spec §3.9.11).
const volumeSnapshotContentRetainPolicy = "Retain"

var (
	csiVolumeSnapshotGVK = schema.GroupVersionKind{
		Group:   snapshotpkg.CSISnapshotGroup,
		Version: snapshotpkg.CSISnapshotVersion,
		Kind:    snapshotpkg.KindVolumeSnapshot,
	}
	csiVolumeSnapshotContentGVK = schema.GroupVersionKind{
		Group:   snapshotpkg.CSISnapshotGroup,
		Version: snapshotpkg.CSISnapshotVersion,
		Kind:    snapshotpkg.KindVolumeSnapshotContent,
	}
	csiVolumeSnapshotClassGVK = schema.GroupVersionKind{
		Group:   snapshotpkg.CSISnapshotGroup,
		Version: snapshotpkg.CSISnapshotVersion,
		Kind:    snapshotpkg.KindVolumeSnapshotClass,
	}
)

// orphanVSBindingResult classifies the per-PVC orphan VolumeSnapshot state for the publish loop.
//   - ready: the bound VSC is durable (Retain) and can be published into dataRefs[].
//   - failed: a terminal CSI/policy failure; the caller writes a capture-failure condition (no requeue loop).
//   - otherwise: still pending (caller requeues without writing a terminal condition).
type orphanVSBindingResult struct {
	binding *storagev1alpha1.SnapshotDataBinding
	ready   bool
	failed  bool
	reason  string
	message string
}

// ensureOrphanPVCVolumeSnapshots creates standard CSI VolumeSnapshots for root residual PVC targets,
// prunes stale leaves for PVCs that are no longer orphan, and records the current set as
// Snapshot.status.childrenSnapshotRefs[] visibility leaves.
// Domain/non-root controllers keep the VCR path; this helper is the namespace-root carve-out only.
func (r *SnapshotReconciler) ensureOrphanPVCVolumeSnapshots(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	content *storagev1alpha1.SnapshotContent,
	targets []vcpkg.Target,
) error {
	desired := make([]storagev1alpha1.SnapshotChildRef, 0, len(targets))
	desiredNames := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		name := orphanPVCVolumeSnapshotName(nsSnap.UID, target)
		treason, tmsg, err := r.ensureOrphanPVCVolumeSnapshot(ctx, nsSnap, target, name)
		if err != nil {
			return err
		}
		if treason != "" {
			// VolumeSnapshotClass resolution/validation failed (annotation/class/PV-CSI/driver):
			// write a terminal capture-failure condition instead of requeueing forever.
			_, ferr := r.failCapture(ctx, nsSnap, content, treason, tmsg)
			return ferr
		}
		desiredNames[name] = struct{}{}
		desired = append(desired, storagev1alpha1.SnapshotChildRef{
			APIVersion: snapshotpkg.CSISnapshotAPIVersion,
			Kind:       snapshotpkg.KindVolumeSnapshot,
			Name:       name,
		})
	}
	if err := r.cleanupStaleOrphanPVCVolumeSnapshots(ctx, nsSnap, desiredNames); err != nil {
		return err
	}
	return r.reconcileOrphanPVCVolumeSnapshotChildLeaves(ctx, nsSnap, desired)
}

// ensureOrphanPVCVolumeSnapshot creates (or adopts) the orphan-PVC VolumeSnapshot for a target.
// On the create path it resolves and validates the VolumeSnapshotClass explicitly; a non-empty
// (terminalReason, terminalMessage) means a non-recoverable configuration problem and the caller must
// write a terminal capture-failure condition. A non-nil error is transient (requeue/backoff).
func (r *SnapshotReconciler) ensureOrphanPVCVolumeSnapshot(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	target vcpkg.Target,
	name string,
) (terminalReason, terminalMessage string, err error) {
	key := types.NamespacedName{Namespace: nsSnap.Namespace, Name: name}
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(csiVolumeSnapshotGVK)
	gerr := r.Client.Get(ctx, key, existing)
	if gerr != nil && !apierrors.IsNotFound(gerr) {
		return "", "", fmt.Errorf("get VolumeSnapshot %s: %w", key, gerr)
	}
	if apierrors.IsNotFound(gerr) {
		// Resolve the VolumeSnapshotClass explicitly (PVC StorageClass annotation, validated against the
		// PV CSI driver) instead of relying on the cluster default class / mutating webhook.
		className, treason, tmsg, rerr := r.orphanPVCVolumeSnapshotClass(ctx, target)
		if rerr != nil {
			return "", "", rerr
		}
		if treason != "" {
			return treason, tmsg, nil
		}
		obj := orphanPVCVolumeSnapshotObject(nsSnap, target, name, className)
		if cerr := r.Client.Create(ctx, obj); cerr != nil && !apierrors.IsAlreadyExists(cerr) {
			return "", "", fmt.Errorf("create VolumeSnapshot %s: %w", key, cerr)
		}
		return "", "", nil
	}
	return r.validateExistingOrphanPVCVolumeSnapshot(ctx, nsSnap, target, key, existing)
}

// validateExistingOrphanPVCVolumeSnapshot adopts an already-present orphan VolumeSnapshot: it ensures the
// ownerRef to this Snapshot, then checks the object actually matches the intended target (namespace and
// source PVC). While the VS is not yet bound, the resolved VolumeSnapshotClass is also validated so a VS
// created with a wrong/empty class (e.g. by a previous controller version, or relying on the default
// class) is not silently accepted. Once the VS is bound the class is moot (the durable artifact already
// exists), so it is no longer re-validated — this avoids flipping an already-published snapshot to
// terminal on a later StorageClass/VolumeSnapshotClass change or deletion.
func (r *SnapshotReconciler) validateExistingOrphanPVCVolumeSnapshot(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	target vcpkg.Target,
	key types.NamespacedName,
	existing *unstructured.Unstructured,
) (terminalReason, terminalMessage string, err error) {
	if volumeSnapshotConflictingSnapshotOwner(existing.GetOwnerReferences(), nsSnap) {
		return "", "", fmt.Errorf("VolumeSnapshot %s owned by another Snapshot", key)
	}
	if !volumeSnapshotHasOwnerRefToSnapshot(existing.GetOwnerReferences(), nsSnap) {
		base := existing.DeepCopy()
		existing.SetOwnerReferences(append(existing.GetOwnerReferences(), volumeSnapshotOwnerReferenceForSnapshot(nsSnap)))
		if perr := r.Client.Patch(ctx, existing, client.MergeFrom(base)); perr != nil {
			return "", "", perr
		}
	}
	if existing.GetNamespace() != nsSnap.Namespace {
		return snapshotpkg.ReasonVolumeCaptureFailed,
			fmt.Sprintf("VolumeSnapshot %s namespace=%q, want %q", key, existing.GetNamespace(), nsSnap.Namespace), nil
	}
	pvcName, _, perr := unstructured.NestedString(existing.Object, "spec", "source", "persistentVolumeClaimName")
	if perr != nil {
		return "", "", fmt.Errorf("read VolumeSnapshot %s source PVC: %w", key, perr)
	}
	if pvcName != target.Name {
		return snapshotpkg.ReasonVolumeCaptureFailed,
			fmt.Sprintf("VolumeSnapshot %s spec.source.persistentVolumeClaimName=%q, want %q", key, pvcName, target.Name), nil
	}

	boundName, _, berr := unstructured.NestedString(existing.Object, "status", "boundVolumeSnapshotContentName")
	if berr != nil {
		return "", "", fmt.Errorf("read VolumeSnapshot %s boundVolumeSnapshotContentName: %w", key, berr)
	}
	if boundName != "" {
		// Already bound: the durable VSC exists; the class is irrelevant now and must not be re-validated.
		return "", "", nil
	}

	className, treason, tmsg, rerr := r.orphanPVCVolumeSnapshotClass(ctx, target)
	if rerr != nil {
		return "", "", rerr
	}
	if treason != "" {
		return treason, tmsg, nil
	}
	existingClass, _, cerr := unstructured.NestedString(existing.Object, "spec", "volumeSnapshotClassName")
	if cerr != nil {
		return "", "", fmt.Errorf("read VolumeSnapshot %s volumeSnapshotClassName: %w", key, cerr)
	}
	if existingClass != className {
		return snapshotpkg.ReasonVolumeCaptureFailed,
			fmt.Sprintf("VolumeSnapshot %s spec.volumeSnapshotClassName=%q, want %q (no silent adoption of a mismatched/legacy class)", key, existingClass, className), nil
	}
	return "", "", nil
}

// directReader returns the non-cached API reader (falling back to the cached client) for cluster-scoped
// read-only lookups (PersistentVolume, StorageClass, VolumeSnapshotClass). Reading these directly keeps
// RBAC at "get" only and avoids starting cluster-wide informers/caches for them.
func (r *SnapshotReconciler) directReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// orphanPVCVolumeSnapshotClass resolves the VolumeSnapshotClass for an orphan PVC the same way the VCR
// path does: PVC -> StorageClass -> storage.deckhouse.io/volumesnapshotclass annotation ->
// VolumeSnapshotClass, then validates the class driver against the bound PV CSI driver.
//
// Returns (className, "", "", nil) on success. A non-empty terminalReason means the configuration can
// never succeed as-is (missing annotation/class, non-CSI PV, driver mismatch) and the caller writes a
// terminal capture-failure condition instead of requeueing. A non-nil err is transient (requeue),
// e.g. the PVC is not yet bound to a PV.
func (r *SnapshotReconciler) orphanPVCVolumeSnapshotClass(
	ctx context.Context,
	target vcpkg.Target,
) (className, terminalReason, terminalMessage string, err error) {
	pvc := &corev1.PersistentVolumeClaim{}
	if gerr := r.Client.Get(ctx, client.ObjectKey{Namespace: target.Namespace, Name: target.Name}, pvc); gerr != nil {
		if apierrors.IsNotFound(gerr) {
			return "", snapshotpkg.ReasonVolumeCaptureFailed,
				fmt.Sprintf("PVC %s/%s not found", target.Namespace, target.Name), nil
		}
		return "", "", "", fmt.Errorf("get PVC %s/%s: %w", target.Namespace, target.Name, gerr)
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName == "" {
		return "", snapshotpkg.ReasonVolumeCaptureFailed,
			fmt.Sprintf("PVC %s/%s has no storageClassName", target.Namespace, target.Name), nil
	}
	scName := *pvc.Spec.StorageClassName

	sc := &storagev1.StorageClass{}
	if gerr := r.directReader().Get(ctx, client.ObjectKey{Name: scName}, sc); gerr != nil {
		if apierrors.IsNotFound(gerr) {
			return "", snapshotpkg.ReasonVolumeCaptureFailed,
				fmt.Sprintf("StorageClass %s (PVC %s/%s) not found", scName, target.Namespace, target.Name), nil
		}
		if apierrors.IsForbidden(gerr) {
			return "", snapshotpkg.ReasonVolumeCaptureFailed,
				fmt.Sprintf("access denied to StorageClass %s", scName), nil
		}
		return "", "", "", fmt.Errorf("get StorageClass %s: %w", scName, gerr)
	}
	vscClassName, ok := sc.Annotations[snapshotpkg.AnnotationStorageClassVolumeSnapshotClass]
	if !ok || vscClassName == "" {
		return "", snapshotpkg.ReasonVolumeCaptureFailed,
			fmt.Sprintf("StorageClass %s has no %s annotation", scName, snapshotpkg.AnnotationStorageClassVolumeSnapshotClass), nil
	}

	// A bound PV is required to validate the CSI driver. An unbound PVC is transient: requeue until it
	// binds (the VCR path likewise requires a bound PV).
	if pvc.Spec.VolumeName == "" {
		return "", "", "", fmt.Errorf("PVC %s/%s not bound to a PV yet", target.Namespace, target.Name)
	}
	pv := &corev1.PersistentVolume{}
	if gerr := r.directReader().Get(ctx, client.ObjectKey{Name: pvc.Spec.VolumeName}, pv); gerr != nil {
		if apierrors.IsNotFound(gerr) {
			return "", snapshotpkg.ReasonVolumeCaptureFailed,
				fmt.Sprintf("PV %s (PVC %s/%s) not found", pvc.Spec.VolumeName, target.Namespace, target.Name), nil
		}
		return "", "", "", fmt.Errorf("get PV %s: %w", pvc.Spec.VolumeName, gerr)
	}
	if pv.Spec.CSI == nil {
		return "", snapshotpkg.ReasonVolumeCaptureFailed,
			fmt.Sprintf("PV %s is not a CSI volume; orphan PVC capture requires a CSI driver", pv.Name), nil
	}

	vscClass := &unstructured.Unstructured{}
	vscClass.SetGroupVersionKind(csiVolumeSnapshotClassGVK)
	if gerr := r.directReader().Get(ctx, client.ObjectKey{Name: vscClassName}, vscClass); gerr != nil {
		if apierrors.IsNotFound(gerr) {
			return "", snapshotpkg.ReasonVolumeCaptureFailed,
				fmt.Sprintf("VolumeSnapshotClass %s (from StorageClass %s annotation) not found", vscClassName, scName), nil
		}
		if apierrors.IsForbidden(gerr) {
			return "", snapshotpkg.ReasonVolumeCaptureFailed,
				fmt.Sprintf("access denied to VolumeSnapshotClass %s", vscClassName), nil
		}
		return "", "", "", fmt.Errorf("get VolumeSnapshotClass %s: %w", vscClassName, gerr)
	}
	driver, _, derr := unstructured.NestedString(vscClass.Object, "driver")
	if derr != nil {
		return "", "", "", fmt.Errorf("read VolumeSnapshotClass %s driver: %w", vscClassName, derr)
	}
	if driver != pv.Spec.CSI.Driver {
		return "", snapshotpkg.ReasonVolumeCaptureFailed,
			fmt.Sprintf("VolumeSnapshotClass %s driver %q does not match PV %s CSI driver %q", vscClassName, driver, pv.Name, pv.Spec.CSI.Driver), nil
	}
	return vscClassName, "", "", nil
}

func orphanPVCVolumeSnapshotObject(nsSnap *storagev1alpha1.Snapshot, target vcpkg.Target, name, className string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": snapshotpkg.CSISnapshotAPIVersion,
		"kind":       snapshotpkg.KindVolumeSnapshot,
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": nsSnap.Namespace,
			"labels": map[string]interface{}{
				labelSnapshotUID: string(nsSnap.UID),
			},
			"ownerReferences": []interface{}{
				ownerRefToMap(volumeSnapshotOwnerReferenceForSnapshot(nsSnap)),
			},
		},
		// volumeSnapshotClassName is resolved explicitly from the PVC StorageClass
		// storage.deckhouse.io/volumesnapshotclass annotation (validated against the PV CSI driver),
		// mirroring the VCR path rather than relying on the cluster default class / mutating webhook.
		// Durability still does not depend on the class deletionPolicy: the bound VSC is forced to Retain
		// during handoff (ensureVolumeSnapshotContentRetain), so a Delete-policy class cannot drop the
		// durable artifact.
		"spec": map[string]interface{}{
			"volumeSnapshotClassName": className,
			"source": map[string]interface{}{
				"persistentVolumeClaimName": target.Name,
			},
		},
	}}
	obj.SetGroupVersionKind(csiVolumeSnapshotGVK)
	return obj
}

func orphanPVCVolumeSnapshotName(snapshotUID types.UID, target vcpkg.Target) string {
	sum := sha256.Sum256([]byte(string(snapshotUID) + "|" + target.UID + "|" + target.Namespace + "/" + target.Name))
	return orphanPVCVolumeSnapshotNamePrefix + hex.EncodeToString(sum[:])[:20]
}

func ownerRefToMap(ref metav1.OwnerReference) map[string]interface{} {
	out := map[string]interface{}{
		"apiVersion": ref.APIVersion,
		"kind":       ref.Kind,
		"name":       ref.Name,
		"uid":        string(ref.UID),
	}
	if ref.Controller != nil {
		out["controller"] = *ref.Controller
	}
	return out
}

// volumeSnapshotOwnerReferenceForSnapshot returns the ownerRef placed on an orphan-PVC VolumeSnapshot.
//
// VolumeSnapshot is a CSI object used as a visibility leaf.
// Snapshot owns it for lifecycle/GC purposes but is not its controller owner.
func volumeSnapshotOwnerReferenceForSnapshot(ns *storagev1alpha1.Snapshot) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
		Kind:       "Snapshot",
		Name:       ns.Name,
		UID:        ns.UID,
		// Controller intentionally unset: the VS is a visibility leaf, not a controller-owned child.
	}
}

func volumeSnapshotHasOwnerRefToSnapshot(refs []metav1.OwnerReference, ns *storagev1alpha1.Snapshot) bool {
	for i := range refs {
		ref := refs[i]
		if ref.APIVersion == storagev1alpha1.SchemeGroupVersion.String() && ref.Kind == "Snapshot" &&
			ref.Name == ns.Name && ref.UID == ns.UID {
			return true
		}
	}
	return false
}

func volumeSnapshotConflictingSnapshotOwner(refs []metav1.OwnerReference, ns *storagev1alpha1.Snapshot) bool {
	for i := range refs {
		ref := refs[i]
		if ref.APIVersion == storagev1alpha1.SchemeGroupVersion.String() && ref.Kind == "Snapshot" &&
			(ref.Name != ns.Name || ref.UID != ns.UID) {
			return true
		}
	}
	return false
}

// cleanupStaleOrphanPVCVolumeSnapshots deletes VolumeSnapshots we created for PVCs that are no longer
// orphan (e.g. a domain controller now covers the PVC). Only our own (nss-vs-* + ownerRef→this Snapshot)
// objects are deleted; the corresponding stale leaf refs are dropped by reconcileOrphanPVCVolumeSnapshotChildLeaves.
func (r *SnapshotReconciler) cleanupStaleOrphanPVCVolumeSnapshots(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	desiredNames map[string]struct{},
) error {
	cur := &storagev1alpha1.Snapshot{}
	if err := r.snapshotReader().Get(ctx, types.NamespacedName{Namespace: nsSnap.Namespace, Name: nsSnap.Name}, cur); err != nil {
		return err
	}
	for _, ref := range cur.Status.ChildrenSnapshotRefs {
		if !snapshotpkg.IsVolumeSnapshotVisibilityLeaf(ref) {
			continue
		}
		if _, keep := desiredNames[ref.Name]; keep {
			continue
		}
		if !strings.HasPrefix(ref.Name, orphanPVCVolumeSnapshotNamePrefix) {
			continue
		}
		if err := r.deleteOwnedOrphanPVCVolumeSnapshot(ctx, nsSnap, ref.Name); err != nil {
			return err
		}
	}
	return nil
}

func (r *SnapshotReconciler) deleteOwnedOrphanPVCVolumeSnapshot(ctx context.Context, nsSnap *storagev1alpha1.Snapshot, name string) error {
	key := types.NamespacedName{Namespace: nsSnap.Namespace, Name: name}
	vs := &unstructured.Unstructured{}
	vs.SetGroupVersionKind(csiVolumeSnapshotGVK)
	if err := r.Client.Get(ctx, key, vs); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get VolumeSnapshot %s: %w", key, err)
	}
	if !volumeSnapshotHasOwnerRefToSnapshot(vs.GetOwnerReferences(), nsSnap) {
		// Not ours (no ownerRef to this Snapshot): leave the object, only the stale leaf ref is dropped.
		return nil
	}
	if err := r.Client.Delete(ctx, vs); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete stale VolumeSnapshot %s: %w", key, err)
	}
	return nil
}

// reconcileOrphanPVCVolumeSnapshotChildLeaves rewrites the VolumeSnapshot visibility leaves on
// Snapshot.status.childrenSnapshotRefs[] to exactly the desired set, preserving real domain child refs.
// It does not touch observedGeneration (this is a status-refs-only write, not a generation observation).
func (r *SnapshotReconciler) reconcileOrphanPVCVolumeSnapshotChildLeaves(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	desired []storagev1alpha1.SnapshotChildRef,
) error {
	key := types.NamespacedName{Namespace: nsSnap.Namespace, Name: nsSnap.Name}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &storagev1alpha1.Snapshot{}
		if err := r.Client.Get(ctx, key, cur); err != nil {
			return err
		}
		nonLeaf := make([]storagev1alpha1.SnapshotChildRef, 0, len(cur.Status.ChildrenSnapshotRefs))
		for _, ref := range cur.Status.ChildrenSnapshotRefs {
			if snapshotpkg.IsVolumeSnapshotVisibilityLeaf(ref) {
				continue
			}
			nonLeaf = append(nonLeaf, ref)
		}
		effective := mergeSnapshotChildRefs(nonLeaf, desired)
		if snapshotChildRefsEqualIgnoreOrder(cur.Status.ChildrenSnapshotRefs, effective) {
			return nil
		}
		base := cur.DeepCopy()
		cur.Status.ChildrenSnapshotRefs = effective
		return r.Client.Status().Patch(ctx, cur, client.MergeFrom(base))
	})
}

func (r *SnapshotReconciler) reconcileOrphanPVCVolumeSnapshotPublish(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	content *storagev1alpha1.SnapshotContent,
	targets []vcpkg.Target,
	allowRequeue bool,
) (ctrl.Result, error) {
	if len(targets) == 0 {
		// No orphan PVCs in residual root scope: still clear any stale VCR / status.volumeCaptureRequestName
		// left behind by a previous (VCR-based) run or migration.
		return r.clearOrphanPVCStaleVCR(ctx, nsSnap, content)
	}
	if snapshotcontentDataRefsCoverTargets(content.Status.DataRefs, targets) {
		return r.clearOrphanPVCStaleVCR(ctx, nsSnap, content)
	}

	bindings := make([]storagev1alpha1.SnapshotDataBinding, 0, len(targets))
	for _, target := range targets {
		res, err := r.orphanPVCVolumeSnapshotBinding(ctx, nsSnap, target)
		if err != nil {
			return ctrl.Result{}, err
		}
		if res.failed {
			return r.failCapture(ctx, nsSnap, content, res.reason, res.message)
		}
		if !res.ready {
			return requeueVolumeCaptureIf(allowRequeue, "waiting for orphan PVC VolumeSnapshot ready")
		}
		bindings = append(bindings, *res.binding)
	}

	if err := r.Client.Get(ctx, client.ObjectKey{Name: content.Name}, content); err != nil {
		return ctrl.Result{}, err
	}
	if err := snapshotcontent.EnsureVolumeSnapshotContentsOwnedByContent(ctx, r.Client, content, bindings); err != nil {
		return requeueVolumeCaptureIf(allowRequeue, fmt.Sprintf("VolumeSnapshotContent handoff: %v", err))
	}
	if err := snapshotcontent.PublishSnapshotContentDataRefs(ctx, r.Client, content.Name, bindings); err != nil {
		return ctrl.Result{}, err
	}
	return r.clearOrphanPVCStaleVCR(ctx, nsSnap, content)
}

// clearOrphanPVCStaleVCR removes any leftover root VCR (the orphan path never creates one) and clears
// the snapshot's volumeCaptureRequestName so a prior VCR-based run does not leave dangling state.
func (r *SnapshotReconciler) clearOrphanPVCStaleVCR(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	content *storagev1alpha1.SnapshotContent,
) (ctrl.Result, error) {
	_ = r.deleteSnapshotVolumeCaptureRequest(ctx, types.NamespacedName{Namespace: nsSnap.Namespace, Name: vcpkg.SnapshotContentVCRName(content.UID)})
	if nsSnap.Status.VolumeCaptureRequestName != "" {
		if err := r.patchSnapshotVolumeCaptureRequestName(ctx, nsSnap, ""); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

func (r *SnapshotReconciler) orphanPVCVolumeSnapshotBinding(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	target vcpkg.Target,
) (orphanVSBindingResult, error) {
	vsName := orphanPVCVolumeSnapshotName(nsSnap.UID, target)
	vs := &unstructured.Unstructured{}
	vs.SetGroupVersionKind(csiVolumeSnapshotGVK)
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: nsSnap.Namespace, Name: vsName}, vs); err != nil {
		if apierrors.IsNotFound(err) {
			return orphanVSBindingResult{}, nil
		}
		return orphanVSBindingResult{}, err
	}
	// Terminal CSI failure on the VolumeSnapshot itself (status.error) — surface as a capture failure
	// instead of requeueing forever (spec §3.9.10 data-leg failure taxonomy).
	if msg, ok := volumeSnapshotStatusErrorMessage(vs); ok {
		return orphanVSBindingResult{
			failed:  true,
			reason:  snapshotpkg.ReasonVolumeCaptureFailed,
			message: fmt.Sprintf("VolumeSnapshot %s/%s: %s", nsSnap.Namespace, vsName, msg),
		}, nil
	}

	boundName, _, err := unstructured.NestedString(vs.Object, "status", "boundVolumeSnapshotContentName")
	if err != nil {
		return orphanVSBindingResult{}, fmt.Errorf("read VolumeSnapshot %s boundVolumeSnapshotContentName: %w", vsName, err)
	}
	readyToUse, _, err := unstructured.NestedBool(vs.Object, "status", "readyToUse")
	if err != nil {
		return orphanVSBindingResult{}, fmt.Errorf("read VolumeSnapshot %s readyToUse: %w", vsName, err)
	}
	if boundName == "" || !readyToUse {
		return orphanVSBindingResult{}, nil
	}

	vsc := &unstructured.Unstructured{}
	vsc.SetGroupVersionKind(csiVolumeSnapshotContentGVK)
	if err := r.Client.Get(ctx, client.ObjectKey{Name: boundName}, vsc); err != nil {
		if apierrors.IsNotFound(err) {
			return orphanVSBindingResult{}, nil
		}
		return orphanVSBindingResult{}, err
	}
	if msg, ok := volumeSnapshotStatusErrorMessage(vsc); ok {
		return orphanVSBindingResult{
			failed:  true,
			reason:  snapshotpkg.ReasonVolumeCaptureFailed,
			message: fmt.Sprintf("VolumeSnapshotContent %s: %s", boundName, msg),
		}, nil
	}

	// Make the bound VSC durable independent of the per-run VolumeSnapshot by forcing
	// deletionPolicy=Retain. A class-default Delete policy would otherwise drop the artifact when the VS
	// is GC'd. If the policy cannot be made Retain, fail terminally (no endless requeue).
	if reason, msg, err := r.ensureVolumeSnapshotContentRetain(ctx, boundName); err != nil {
		return orphanVSBindingResult{}, err
	} else if reason != "" {
		return orphanVSBindingResult{failed: true, reason: reason, message: msg}, nil
	}

	return orphanVSBindingResult{
		ready: true,
		binding: &storagev1alpha1.SnapshotDataBinding{
			TargetUID: target.UID,
			Target: storagev1alpha1.SnapshotSubjectRef{
				UID:        types.UID(target.UID),
				APIVersion: target.APIVersion,
				Kind:       target.Kind,
				Namespace:  target.Namespace,
				Name:       target.Name,
			},
			Artifact: storagev1alpha1.SnapshotDataArtifactRef{
				APIVersion: snapshotpkg.CSISnapshotAPIVersion,
				Kind:       snapshotpkg.KindVolumeSnapshotContent,
				Name:       boundName,
			},
			// TODO: later add snapshotRef to dataRefs:
			// snapshotRef:
			//   apiVersion: snapshot.storage.k8s.io/v1
			//   kind: VolumeSnapshot
			//   namespace: <ns>
			//   name: <vs-name>
		},
	}, nil
}

// ensureVolumeSnapshotContentRetain patches the bound VSC's spec.deletionPolicy to Retain when needed.
// Returns a non-empty (reason, message) for a terminal failure (e.g. the API rejects the policy change),
// or an error for a transient failure that should be retried.
func (r *SnapshotReconciler) ensureVolumeSnapshotContentRetain(ctx context.Context, vscName string) (reason, message string, err error) {
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		vsc := &unstructured.Unstructured{}
		vsc.SetGroupVersionKind(csiVolumeSnapshotContentGVK)
		if gerr := r.Client.Get(ctx, client.ObjectKey{Name: vscName}, vsc); gerr != nil {
			return gerr
		}
		policy, _, perr := unstructured.NestedString(vsc.Object, "spec", "deletionPolicy")
		if perr != nil {
			return fmt.Errorf("read VolumeSnapshotContent %s deletionPolicy: %w", vscName, perr)
		}
		if policy == volumeSnapshotContentRetainPolicy {
			return nil
		}
		base := vsc.DeepCopy()
		if serr := unstructured.SetNestedField(vsc.Object, volumeSnapshotContentRetainPolicy, "spec", "deletionPolicy"); serr != nil {
			return serr
		}
		return r.Client.Patch(ctx, vsc, client.MergeFrom(base))
	})
	if err == nil {
		return "", "", nil
	}
	if isTerminalAPIError(err) {
		return snapshotpkg.ReasonDataArtifactInvalid,
			fmt.Sprintf("VolumeSnapshotContent %s deletionPolicy cannot be set to Retain: %v", vscName, err),
			nil
	}
	return "", "", err
}

// isTerminalAPIError reports whether an API error means the request will never succeed as-is
// (validation/permission/shape rejections), as opposed to a transient/retryable error.
func isTerminalAPIError(err error) bool {
	return apierrors.IsInvalid(err) ||
		apierrors.IsBadRequest(err) ||
		apierrors.IsForbidden(err) ||
		apierrors.IsMethodNotSupported(err)
}

// volumeSnapshotStatusErrorMessage reads status.error.message from a VolumeSnapshot or
// VolumeSnapshotContent (same shape). ok=false when there is no error message.
func volumeSnapshotStatusErrorMessage(obj *unstructured.Unstructured) (string, bool) {
	msg, found, err := unstructured.NestedString(obj.Object, "status", "error", "message")
	if err != nil || !found || strings.TrimSpace(msg) == "" {
		return "", false
	}
	return msg, true
}

func snapshotcontentDataRefsCoverTargets(refs []storagev1alpha1.SnapshotDataBinding, targets []vcpkg.Target) bool {
	return len(targets) > 0 && volumecapturectrl.ContentDataRefsCoverExpectedTargets(refs, targets)
}
