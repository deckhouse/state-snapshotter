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
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/deckhouse/state-snapshotter/api/names"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/manifestcapture"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/snapshotcontent"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
)

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
	// vsUID is the live orphan VolumeSnapshot UID. It keys the child volume-node SnapshotContent and its
	// per-PVC ManifestCaptureRequest under the unified wave4C naming scheme (api/names), so the per-PVC
	// leaf identity is the VS (not the PVC).
	vsUID   types.UID
	ready   bool
	failed  bool
	reason  string
	message string
}

// ensureOrphanPVCVolumeSnapshots creates standard CSI VolumeSnapshots for root residual PVC targets and
// records the current set as Snapshot.status.childrenSnapshotRefs[] visibility leaves. The orphan
// VolumeSnapshot is durable (it is the snapshot of the PVC) and is NOT pruned mid-life: it is removed only
// by ownerRef GC when the Snapshot is deleted. Orphan volume capture is sequenced after all domain
// children are Ready (the "late Planned" pre-barrier wave gates on it, see
// ensureOrphanVolumeSnapshotsPrePlanned), so the targets here are the genuinely uncovered PVCs and there
// is no "became covered -> prune" churn.
//
// content may be nil: in the "late Planned" pre-barrier wave the root SnapshotContent is not bound yet, so
// a terminal VolumeSnapshotClass failure is routed onto the Snapshot's own Ready (failOrphanCaptureTerminal)
// rather than onto the (absent) content. Domain/non-root controllers keep the VCR path; this helper is the
// namespace-root carve-out only.
func (r *SnapshotReconciler) ensureOrphanPVCVolumeSnapshots(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	content *storagev1alpha1.SnapshotContent,
	targets []vcpkg.Target,
) error {
	desired := make([]storagev1alpha1.SnapshotChildRef, 0, len(targets))
	for _, target := range targets {
		name := orphanPVCVolumeSnapshotName(nsSnap.UID, target)
		treason, tmsg, err := r.ensureOrphanPVCVolumeSnapshot(ctx, nsSnap, target, name)
		if err != nil {
			return err
		}
		if treason != "" {
			// VolumeSnapshotClass resolution/validation failed (annotation/class/PV-CSI/driver):
			// write a terminal capture-failure condition instead of requeueing forever. Pre-Planned
			// ("late Planned" wave, content==nil) there is no bound root content to carry it, so the
			// terminal is written on the Snapshot's own Ready.
			return r.failOrphanCaptureTerminal(ctx, nsSnap, content, treason, tmsg)
		}
		desired = append(desired, storagev1alpha1.SnapshotChildRef{
			APIVersion: snapshotpkg.CSISnapshotAPIVersion,
			Kind:       snapshotpkg.KindVolumeSnapshot,
			Name:       name,
		})
	}
	return r.reconcileOrphanPVCVolumeSnapshotChildLeaves(ctx, nsSnap, desired)
}

// failOrphanCaptureTerminal writes a terminal orphan-PVC capture failure. Post-bind it degrades the bound
// root content (failCapture); pre-bind ("late Planned" wave, content==nil) it degrades the Snapshot's own
// Ready directly, since no root content exists yet to carry the failure.
func (r *SnapshotReconciler) failOrphanCaptureTerminal(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	content *storagev1alpha1.SnapshotContent,
	reason, message string,
) error {
	if content != nil {
		_, err := r.failCapture(ctx, nsSnap, content, reason, message)
		return err
	}
	key := types.NamespacedName{Namespace: nsSnap.Namespace, Name: nsSnap.Name}
	return r.patchSnapshotReadyLocal(ctx, key, metav1.ConditionFalse, reason, message)
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
		// No labelSnapshotUID on the orphan-PVC VolumeSnapshot: the VS is always resolved by its
		// deterministic name (orphanPVCVolumeSnapshotName) and its lifecycle/GC hangs off the ownerRef to
		// the Snapshot; the label was observability-only, never read, and only introduced drift. (The same
		// label REMAINS a functional ownership/anti-spoof guard on the root MCR — see capture.go.)
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": nsSnap.Namespace,
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

// orphanPVCVolumeSnapshotName returns the deterministic orphan/residual-PVC CSI VolumeSnapshot name,
// keyed by the root Snapshot UID and the captured PVC UID (unified wave4C scheme, see api/names).
func orphanPVCVolumeSnapshotName(snapshotUID types.UID, target vcpkg.Target) string {
	return names.OrphanVolumeSnapshotName(snapshotUID, types.UID(target.UID))
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

// reconcileOrphanPVCVolumeSnapshotChildLeaves rewrites the VolumeSnapshot visibility leaves on
// Snapshot.status.childrenSnapshotRefs[] to exactly the desired set, preserving real domain child refs.
// This is a status-refs-only write; it touches no conditions.
//
// wave5 note (PR-C): this REPLACE-of-the-leaf-partition (drop a VolumeSnapshot leaf ref no longer in the
// desired set, while the durable VS object itself is never pruned — see
// TestEnsureOrphanPVCVolumeSnapshots_DurableVSNotPruned) is deliberately kept in the controller rather
// than routed through the generic SDK EnsureChildren/PublishChildRefs: the SDK is delete-free (additive
// union) and kind-agnostic, so it cannot express "replace only the VolumeSnapshot-leaf partition" without
// leaking root-specific leaf semantics into the shared module. The single-field-correctness the wave5
// single-writer goal targets is already met: EnsureChildren unions additively (preserving these leaves)
// and this writer preserves the non-leaf domain refs, so the two partitions co-write without conflict.
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

// reconcileOrphanPVCVolumeSnapshotPublish materializes one standalone child volume node per loose/orphan
// PVC (Variant A): instead of publishing a dataRefs[] list onto the root content, each ready orphan PVC
// becomes its own child SnapshotContent (own dataRef + own ManifestCheckpoint holding that PVC's
// manifest), linked under the root. The root aggregator keeps dataRef=nil. Returns done (stale-VCR
// cleared) only once every orphan child node's data + manifest legs are observable.
func (r *SnapshotReconciler) reconcileOrphanPVCVolumeSnapshotPublish(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	content *storagev1alpha1.SnapshotContent,
	targets []vcpkg.Target,
	allowRequeue bool,
) (ctrl.Result, error) {
	if len(targets) == 0 {
		// Residual/orphan-PVC wave is done with zero orphan targets (reachable only after the domain gate
		// opened, so there really are no orphans): the orphan-link gate on the aggregator is vacuously open
		// (no VS leaves declared), so no latch is needed. Still clear any stale VCR /
		// status.volumeCaptureRequestName left behind by a previous (VCR-based) run or migration.
		return r.clearOrphanPVCStaleVCR(ctx, nsSnap, content)
	}
	// Re-read the root content so childrenSnapshotContentRefs / UID are fresh for child linking and ownerRefs.
	if err := r.Client.Get(ctx, client.ObjectKey{Name: content.Name}, content); err != nil {
		return ctrl.Result{}, err
	}

	allReady := true
	for _, target := range targets {
		res, err := r.orphanPVCVolumeSnapshotBinding(ctx, nsSnap, target)
		if err != nil {
			return ctrl.Result{}, err
		}
		if res.failed {
			return r.failCapture(ctx, nsSnap, content, res.reason, res.message)
		}
		if !res.ready {
			allReady = false
			continue
		}
		ready, terr := r.ensureOrphanVolumeChildNode(ctx, nsSnap, content, target, res.vsUID, res.binding)
		if terr != nil {
			return ctrl.Result{}, terr
		}
		if !ready {
			allReady = false
		}
	}
	if !allReady {
		return requeueVolumeCaptureIf(allowRequeue, "waiting for orphan PVC child volume nodes")
	}
	// Residual/orphan-PVC wave finished: every orphan child volume node is linked and ready. The aggregator
	// observes this directly (each orphan child content is now linked into childrenSnapshotContentRefs), so
	// the fail-closed orphan-link gate (ChildrenReady) opens on its own — no separate latch to stamp. Clear
	// any stale VCR left by a previous run.
	return r.clearOrphanPVCStaleVCR(ctx, nsSnap, content)
}

// ensureOrphanVolumeChildNode materializes the standalone child volume node for one loose/orphan PVC
// (Variant A): a dedicated child SnapshotContent that owns this PVC's dataRef (its bound VSC) and its own
// ManifestCheckpoint (the PVC manifest), linked under the root content; the namespaced CSI VolumeSnapshot
// becomes the handle (status.boundSnapshotContentName -> child, INV-ORPHAN4). The PVC manifest and its
// dataRef therefore live on one content (co-ownership invariant), and the dataRef is NOT published on the
// root aggregator. Returns ready=true once the child node's data and manifest legs are both observable.
func (r *SnapshotReconciler) ensureOrphanVolumeChildNode(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	root *storagev1alpha1.SnapshotContent,
	target vcpkg.Target,
	vsUID types.UID,
	binding *storagev1alpha1.SnapshotDataBinding,
) (ready bool, err error) {
	enriched, eerr := snapshotcontent.EnrichDataBindingsWithVolumeMetadata(ctx, r.Client, r.directReader(), []storagev1alpha1.SnapshotDataBinding{*binding})
	if eerr != nil {
		return false, eerr
	}
	// snapshotRef points back at the orphan CSI VolumeSnapshot that binds this child via its
	// status.boundSnapshotContentName (INV-ORPHAN4) — that is the handshake subject, not the root
	// content ownerRef (which is only the GC lifecycle link). The orphan VolumeSnapshot is durable for
	// the life of the Snapshot (it is the snapshot of the PVC), removed only by ownerRef GC.
	//
	// UID is stamped with the live orphan VolumeSnapshot UID (wave4B): it pins the leaf<->VS identity so
	// the resolver's anti-spoof handshake (verifyOrphanContentSnapshotRef) matches UID, and — crucially —
	// gives recycle-bin restore a concrete uid to re-point (relaxed-CEL) when the durable leaf content is
	// re-attached to a freshly re-created VolumeSnapshot handle (which carries a NEW uid). Under the
	// unified wave4C scheme this same VS UID also keys the child content name and per-PVC MCR, so the leaf
	// identity is consistently the VS across name, MCR, and back-reference.
	orphanVSRef := &storagev1alpha1.SnapshotSubjectRef{
		APIVersion: snapshotpkg.CSISnapshotAPIVersion,
		Kind:       snapshotpkg.KindVolumeSnapshot,
		Namespace:  nsSnap.Namespace,
		Name:       orphanPVCVolumeSnapshotName(nsSnap.UID, target),
		UID:        vsUID,
	}
	child, err := snapshotcontent.EnsureVolumeChildContent(ctx, r.Client, root, vsUID, orphanVSRef)
	if err != nil {
		return false, err
	}
	// Hand off the bound VSC to the CHILD content (ownerRef + Retain), then publish the single dataRef.
	if err := snapshotcontent.EnsureVolumeSnapshotContentsOwnedByContent(ctx, r.Client, child, enriched); err != nil {
		return false, err
	}
	if err := snapshotcontent.PublishSnapshotContentDataRef(ctx, r.Client, child.Name, &enriched[0]); err != nil {
		return false, err
	}
	mcpReady, err := r.ensureOrphanVolumeChildManifestCheckpoint(ctx, nsSnap, child, target, vsUID)
	if err != nil {
		return false, err
	}
	if err := snapshotcontent.LinkChildVolumeContentRef(ctx, r.Client, r.directReader(), root.Name, child.Name); err != nil {
		return false, err
	}
	if err := r.bindOrphanVSToChildContent(ctx, nsSnap, target, child.Name); err != nil {
		return false, err
	}
	return mcpReady, nil
}

// ensureOrphanVolumeChildManifestCheckpoint ensures the per-orphan ManifestCaptureRequest that captures
// just this PVC's manifest into the child volume node's own ManifestCheckpoint, and publishes its name
// onto the child content. Returns ready=true once that MCP is Ready (the common SnapshotContent
// controller re-parents the MCP ownerRef to the child content). Idempotent: once the child's manifest is
// persisted it does not recreate the MCR (a fresh MCR UID would derive a different MCP name).
func (r *SnapshotReconciler) ensureOrphanVolumeChildManifestCheckpoint(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	child *storagev1alpha1.SnapshotContent,
	target vcpkg.Target,
	vsUID types.UID,
) (ready bool, err error) {
	mcrKey := types.NamespacedName{Namespace: nsSnap.Namespace, Name: namespacemanifest.SnapshotVolumeMCRName(vsUID)}

	// Already persisted: MCP Ready under the child's published name. Do not recreate the per-orphan MCR.
	cur := &storagev1alpha1.SnapshotContent{}
	if gErr := r.Client.Get(ctx, client.ObjectKey{Name: child.Name}, cur); gErr != nil {
		return false, gErr
	}
	if cur.Status.ManifestCheckpointName != "" {
		if mcpReady, mErr := r.orphanVolumeChildManifestCheckpointReady(ctx, cur.Status.ManifestCheckpointName); mErr != nil {
			return false, mErr
		} else if mcpReady {
			if safe, sErr := manifestcapture.ManifestCaptureRequestSafeToDelete(ctx, r.Client, mcrKey, child.Name); sErr == nil && safe {
				if dErr := r.deleteOrphanVolumeMCR(ctx, mcrKey); dErr != nil {
					return false, dErr
				}
			}
			return true, nil
		}
	}

	mcr := &ssv1alpha1.ManifestCaptureRequest{}
	gErr := r.Client.Get(ctx, mcrKey, mcr)
	if apierrors.IsNotFound(gErr) {
		if cur.Status.ManifestCheckpointName != "" {
			// The child already published its manifestCheckpointName and the per-orphan MCR was deleted
			// after a successful capture (above). Reaching here means that published MCP is now NOT Ready —
			// a post-success degradation (Phase 2a), not a first capture. Do NOT recreate the MCR with a
			// fresh UID: that would derive a different MCP name, abandon the published one, and re-target a
			// PVC that may no longer exist. Leave the published name in place and let the child content's
			// ManifestsReady aggregation surface the degraded MCP. Mirrors the root path's
			// reconcileIfRootManifestCheckpointAlreadyReady guard.
			return false, nil
		}
		apiVersion := target.APIVersion
		if apiVersion == "" {
			apiVersion = corev1.SchemeGroupVersion.String()
		}
		mcr = &ssv1alpha1.ManifestCaptureRequest{
			ObjectMeta: metav1.ObjectMeta{
				Name:            mcrKey.Name,
				Namespace:       mcrKey.Namespace,
				OwnerReferences: []metav1.OwnerReference{snapshotOwnerReferenceForMCR(nsSnap)},
				Labels:          map[string]string{labelSnapshotUID: string(nsSnap.UID)},
			},
			Spec: ssv1alpha1.ManifestCaptureRequestSpec{
				Targets: []ssv1alpha1.ManifestTarget{{
					APIVersion: apiVersion,
					Kind:       "PersistentVolumeClaim",
					Name:       target.Name,
				}},
			},
		}
		if cErr := r.Client.Create(ctx, mcr); cErr != nil && !apierrors.IsAlreadyExists(cErr) {
			return false, cErr
		}
		if rErr := r.Client.Get(ctx, mcrKey, mcr); rErr != nil {
			// Created but not yet readable; the MCP is not ready this round.
			return false, nil
		}
	} else if gErr != nil {
		return false, gErr
	}

	mcpName := namespacemanifest.GenerateManifestCheckpointNameFromUID(mcr.UID)
	if mcpName == "" {
		return false, nil
	}
	if pErr := snapshotcontent.PublishSnapshotContentManifestCheckpointName(ctx, r.Client, child.Name, mcpName); pErr != nil {
		return false, pErr
	}
	return r.orphanVolumeChildManifestCheckpointReady(ctx, mcpName)
}

func (r *SnapshotReconciler) orphanVolumeChildManifestCheckpointReady(ctx context.Context, mcpName string) (bool, error) {
	mcp := &ssv1alpha1.ManifestCheckpoint{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: mcpName}, mcp); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	cond := meta.FindStatusCondition(mcp.Status.Conditions, ssv1alpha1.ManifestCheckpointConditionTypeReady)
	return cond != nil && cond.Status == metav1.ConditionTrue, nil
}

func (r *SnapshotReconciler) deleteOrphanVolumeMCR(ctx context.Context, key types.NamespacedName) error {
	mcr := &ssv1alpha1.ManifestCaptureRequest{}
	if err := r.Client.Get(ctx, key, mcr); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if err := r.Client.Delete(ctx, mcr); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// bindOrphanVSToChildContent writes the orphan CSI VolumeSnapshot's status.boundSnapshotContentName to
// its child volume content (INV-ORPHAN4 handle), using an optimistic-lock merge patch (D4a): the VS
// status is co-owned with the external snapshot-controller, so a concurrent writer yields a 409 and the
// read-modify-write retries instead of racing on a stale resourceVersion.
func (r *SnapshotReconciler) bindOrphanVSToChildContent(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	target vcpkg.Target,
	childContentName string,
) error {
	vsName := orphanPVCVolumeSnapshotName(nsSnap.UID, target)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		vs := &unstructured.Unstructured{}
		vs.SetGroupVersionKind(csiVolumeSnapshotGVK)
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: nsSnap.Namespace, Name: vsName}, vs); err != nil {
			return err
		}
		cur, _, _ := unstructured.NestedString(vs.Object, "status", "boundSnapshotContentName")
		if cur == childContentName {
			return nil
		}
		base := vs.DeepCopy()
		if err := unstructured.SetNestedField(vs.Object, childContentName, "status", "boundSnapshotContentName"); err != nil {
			return err
		}
		if err := r.Client.Status().Patch(ctx, vs, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		// Verify the field actually persisted. status.boundSnapshotContentName is a Deckhouse extended-VS
		// fork field (storage-foundation); on a cluster running the upstream VolumeSnapshot CRD the API
		// server silently prunes this unknown status field, so the patch "succeeds" but the handle is lost
		// and restore can never resolve this orphan child volume node (terminal ErrNotReady with no clear
		// cause). controller-runtime decodes the server response back into vs, so re-read it and fail loudly
		// with an actionable diagnostic instead of leaving a Ready-looking but unrestorable snapshot.
		persisted, _, perr := unstructured.NestedString(vs.Object, "status", "boundSnapshotContentName")
		if perr != nil {
			return fmt.Errorf("read back VolumeSnapshot %s/%s boundSnapshotContentName: %w", nsSnap.Namespace, vsName, perr)
		}
		if persisted != childContentName {
			return fmt.Errorf("VolumeSnapshot %s/%s status.boundSnapshotContentName did not persist (got %q, want %q): the extended VolumeSnapshot CRD is missing this field — install the storage-foundation extended-VS fork (status.boundSnapshotContentName)",
				nsSnap.Namespace, vsName, persisted, childContentName)
		}
		return nil
	})
}

// clearOrphanPVCStaleVCR removes any leftover root VCR (the orphan path never creates one) so a prior
// VCR-based run does not leave dangling state. The VCR name is a deterministic core-internal handle
// (not tracked in status), so only the object itself needs removing.
func (r *SnapshotReconciler) clearOrphanPVCStaleVCR(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	content *storagev1alpha1.SnapshotContent,
) (ctrl.Result, error) {
	_ = r.deleteSnapshotVolumeCaptureRequest(ctx, types.NamespacedName{Namespace: nsSnap.Namespace, Name: vcpkg.SnapshotContentVCRName(content.UID)})
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
		vsUID: vs.GetUID(),
		binding: &storagev1alpha1.SnapshotDataBinding{
			Source: storagev1alpha1.SnapshotSubjectRef{
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
				UID:        vsc.GetUID(),
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
