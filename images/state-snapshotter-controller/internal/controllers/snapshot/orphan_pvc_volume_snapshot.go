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

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/deckhouse/state-snapshotter/api/names"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk"
)

var (
	csiVolumeSnapshotGVK = schema.GroupVersionKind{
		Group:   snapshotpkg.CSISnapshotGroup,
		Version: snapshotpkg.CSISnapshotVersion,
		Kind:    snapshotpkg.KindVolumeSnapshot,
	}
	csiVolumeSnapshotClassGVK = schema.GroupVersionKind{
		Group:   snapshotpkg.CSISnapshotGroup,
		Version: snapshotpkg.CSISnapshotVersion,
		Kind:    snapshotpkg.KindVolumeSnapshotClass,
	}
)

// ensureOrphanPVCVolumeSnapshots creates a standard CSI VolumeSnapshot for each root residual (orphan) PVC
// target and declares them as REGULAR domain children via the SDK EnsureChildren (content-single-writer
// design §11.6). From here on the orphan VolumeSnapshot is an ordinary domain snapshot: the
// storage-foundation VolumeSnapshot domain controller adopts + plans it, the generic binder creates + binds
// its SnapshotContent, and the aggregator projects ALL of that content's status (data from the bound VSC,
// manifestCheckpointName from the VS domain's MCR, childrenSnapshotContentRefs, Ready). The namespace
// domain therefore writes NO SnapshotContent (INV-CONTENT-WRITER-1 STRICT) and does NOT prune the orphan
// VolumeSnapshot — its lifecycle hangs off the ownerRef to the root Snapshot (stamped by EnsureChildren).
//
// The class is resolved + validated explicitly on the build path (PVC StorageClass annotation, checked
// against the PV CSI driver): a non-empty terminalReason from that resolution is a non-recoverable
// configuration problem surfaced on the Snapshot's own Ready (failOrphanCaptureTerminal) — pre-Planned
// there is no bound root content to carry it. excluded is the domain planning pass's exclusion set,
// re-passed to EnsureChildren so the additive child-ref union does not clobber status.excludedRefs.
func (r *SnapshotReconciler) ensureOrphanPVCVolumeSnapshots(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	adapter NamespaceSnapshotAdapter,
	sdk snapshotsdk.CaptureSDK,
	excluded []storagev1alpha1.ExcludedObjectRef,
	targets []vcpkg.Target,
) error {
	specs := make([]snapshotsdk.ChildSpec, 0, len(targets))
	for _, target := range targets {
		name := orphanPVCVolumeSnapshotName(nsSnap.UID, target)
		className, treason, tmsg, err := r.orphanPVCVolumeSnapshotClass(ctx, target)
		if err != nil {
			return err
		}
		if treason != "" {
			// VolumeSnapshotClass resolution/validation failed (annotation/class/PV-CSI/driver): write a
			// terminal capture-failure condition instead of requeueing forever. Pre-Planned there is no bound
			// root content to carry it, so the terminal is written on the Snapshot's own Ready.
			return r.failOrphanCaptureTerminal(ctx, nsSnap, treason, tmsg)
		}
		// Guard adoption: EnsureChildren create/adopts BY NAME and its adopt path only stamps the ownerRef —
		// it never rewrites spec (a CSI VolumeSnapshot spec is immutable anyway). So a pre-existing object at
		// the deterministic name (a stale/legacy VolumeSnapshot, a bare shell, or a pre-provisioned VS) would
		// be adopted + published as a domain child WITH ITS OWN spec, capturing the wrong volume / wrong class
		// / nothing at all. The name keys on (rootUID, pvcUID) so this is practically unreachable, but validate
		// the desired source PVC AND resolved class against any existing object and fail closed on a mismatch
		// (restores the pre-dismantling validateExistingOrphanPVCVolumeSnapshot guard).
		if treason, tmsg, err := r.orphanPVCVolumeSnapshotSpecMismatch(ctx, nsSnap.Namespace, name, target.Name, className); err != nil {
			return err
		} else if treason != "" {
			return r.failOrphanCaptureTerminal(ctx, nsSnap, treason, tmsg)
		}
		specs = append(specs, snapshotsdk.ChildSpec{Object: orphanPVCVolumeSnapshotObject(nsSnap, target, name, className)})
	}
	if len(specs) == 0 {
		return nil
	}
	// EnsureChildren create/adopts each orphan VolumeSnapshot (stamping the controller ownerRef to the root
	// Snapshot) and UNIONS their refs into status.childrenSnapshotRefs. Re-passing the planning excluded set
	// keeps status.excludedRefs intact (EnsureChildren rewrites that field on every call).
	return sdk.EnsureChildren(ctx, adapter, specs, excluded)
}

// orphanPVCVolumeSnapshotSpecMismatch validates a pre-existing orphan VolumeSnapshot at the deterministic
// name against the DESIRED capture spec, so the caller fails closed instead of adopting (via EnsureChildren,
// whose adopt path never rewrites spec) an object that would capture the wrong volume or capture nothing.
// It returns a terminal (reason, message) when an object exists whose
// spec.source.persistentVolumeClaimName != wantPVCName (including empty/shell/pre-provisioned). NotFound —
// the common case, the object will be created with the correct spec — returns "".
//
// The resolved class is validated ONLY while the object is not yet bound: once a durable snapshot exists
// (status.boundVolumeSnapshotContentName set), the class has already served its sole purpose (selecting the
// driver/params at creation) and no longer affects correctness. Re-resolving the class (e.g. an operator
// edits the StorageClass storage.deckhouse.io/volumesnapshotclass annotation between reconciles) MUST NOT
// flip an already-captured snapshot to terminal VolumeCaptureFailed — this mirrors the pre-dismantling
// validateExistingOrphanPVCVolumeSnapshot guard, which skipped class re-validation once bound. The source
// PVC, by contrast, is validated even when bound: a bound handle capturing a DIFFERENT volume is a real
// correctness fault (the deterministic name would misattribute another PVC's data), so it stays fail-closed.
func (r *SnapshotReconciler) orphanPVCVolumeSnapshotSpecMismatch(
	ctx context.Context,
	namespace, name, wantPVCName, wantClass string,
) (terminalReason, terminalMessage string, err error) {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(csiVolumeSnapshotGVK)
	if gerr := r.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, existing); gerr != nil {
		if apierrors.IsNotFound(gerr) {
			return "", "", nil
		}
		return "", "", fmt.Errorf("get existing orphan VolumeSnapshot %s/%s: %w", namespace, name, gerr)
	}
	gotPVC, _, gerr := unstructured.NestedString(existing.Object, "spec", "source", "persistentVolumeClaimName")
	if gerr != nil {
		return "", "", fmt.Errorf("read orphan VolumeSnapshot %s/%s spec.source.persistentVolumeClaimName: %w", namespace, name, gerr)
	}
	if gotPVC != wantPVCName {
		return snapshotpkg.ReasonVolumeCaptureFailed,
			fmt.Sprintf("existing VolumeSnapshot %s/%s sources PVC %q but the residual target is PVC %q (spec is immutable)", namespace, name, gotPVC, wantPVCName), nil
	}
	// A bound VolumeSnapshot is a durable, valid capture regardless of the class currently resolved — do
	// not re-validate its class (see the doc comment). Only an unbound handle's class is checked, so a
	// genuinely mis-created (wrong-class, never-binding) shell still fails closed before adoption.
	boundContent, _, gerr := unstructured.NestedString(existing.Object, "status", "boundVolumeSnapshotContentName")
	if gerr != nil {
		return "", "", fmt.Errorf("read orphan VolumeSnapshot %s/%s status.boundVolumeSnapshotContentName: %w", namespace, name, gerr)
	}
	if boundContent != "" {
		return "", "", nil
	}
	gotClass, _, gerr := unstructured.NestedString(existing.Object, "spec", "volumeSnapshotClassName")
	if gerr != nil {
		return "", "", fmt.Errorf("read orphan VolumeSnapshot %s/%s spec.volumeSnapshotClassName: %w", namespace, name, gerr)
	}
	if gotClass != wantClass {
		return snapshotpkg.ReasonVolumeCaptureFailed,
			fmt.Sprintf("existing VolumeSnapshot %s/%s uses VolumeSnapshotClass %q but the resolved class is %q (spec is immutable)", namespace, name, gotClass, wantClass), nil
	}
	return "", "", nil
}

// failOrphanCaptureTerminal writes a terminal orphan-PVC capture failure onto the Snapshot's own Ready.
// The orphan wave runs pre-Planned (no bound root content exists yet to carry the failure), so it always
// degrades the Snapshot directly.
func (r *SnapshotReconciler) failOrphanCaptureTerminal(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	reason, message string,
) error {
	key := types.NamespacedName{Namespace: nsSnap.Namespace, Name: nsSnap.Name}
	return r.patchSnapshotNotReadyLocal(ctx, key, reason, message)
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

// orphanPVCVolumeSnapshotObject builds the desired orphan-PVC CSI VolumeSnapshot template (no ownerRef:
// the SDK EnsureChildren stamps the controlling ownerRef to the root Snapshot). volumeSnapshotClassName is
// resolved explicitly from the PVC StorageClass storage.deckhouse.io/volumesnapshotclass annotation
// (validated against the PV CSI driver), mirroring the VCR path rather than relying on the cluster default
// class / mutating webhook.
func orphanPVCVolumeSnapshotObject(nsSnap *storagev1alpha1.Snapshot, target vcpkg.Target, name, className string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": snapshotpkg.CSISnapshotAPIVersion,
		"kind":       snapshotpkg.KindVolumeSnapshot,
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": nsSnap.Namespace,
		},
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
// keyed by the root Snapshot UID and the captured PVC UID (unified naming scheme, see api/names).
func orphanPVCVolumeSnapshotName(snapshotUID types.UID, target vcpkg.Target) string {
	return names.OrphanVolumeSnapshotName(snapshotUID, types.UID(target.UID))
}
