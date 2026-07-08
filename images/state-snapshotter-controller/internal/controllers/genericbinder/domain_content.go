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

package genericbinder

import (
	"context"
	"reflect"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/snapshotcontent"
)

// The domain-capture request lifecycle that used to live here — the capture-leg eager-init, the
// commonController.manifestCaptured/dataCaptured latches, the subtreeManifestsPersisted snapshot-mirror,
// and the MCR/VCR reap — moved to the SnapshotContentController aggregator
// (snapshotcontent/capture_legs.go): main-owned commonController, content-single-writer design §2/§3,
// decision #10. The binder is a pure creator; it keeps only the leaf status.data mirror below (top-level
// export descriptor for d8, not part of captureState).

// mirrorLeafDataFromContent mirrors the bound SnapshotContent's self-contained data binding
// (SnapshotContent.status.data: source + artifact + volume metadata) verbatim onto the namespaced data
// leaf's top-level status.data, so d8 can read the captured-volume descriptor namespaced without touching
// the cluster-scoped SnapshotContent. On import the content data carries no storageClassName (it is not
// derived from a live PVC), so the caller passes scOverride from DataImport.spec.storageClassName; on
// capture scOverride is empty and the live content storageClassName is used. No-op until the content has
// a published data binding.
func (r *GenericSnapshotBinderController) mirrorLeafDataFromContent(
	ctx context.Context,
	obj *unstructured.Unstructured,
	contentName string,
	scOverride string,
) error {
	content := &storagev1alpha1.SnapshotContent{}
	if err := r.Get(ctx, client.ObjectKey{Name: contentName}, content); err != nil {
		return err
	}
	if content.Status.Data == nil {
		return nil
	}
	data := *content.Status.Data
	if scOverride != "" {
		data.StorageClassName = scOverride
	}
	return r.mirrorDataToLeaf(ctx, obj, &data)
}

// mirrorDataToLeaf writes the self-contained data binding onto the leaf snapshot's top-level status.data
// under an optimistic-lock merge patch (D4a: demo.status is co-owned). Idempotent — it re-reads and
// short-circuits when status.data already equals the desired block.
func (r *GenericSnapshotBinderController) mirrorDataToLeaf(
	ctx context.Context,
	obj *unstructured.Unstructured,
	data *storagev1alpha1.SnapshotDataBinding,
) error {
	desired := snapshotcontent.SnapshotDataBindingToUnstructuredMap(data)
	gvk := obj.GetObjectKind().GroupVersionKind()
	key := client.ObjectKey{Namespace: obj.GetNamespace(), Name: obj.GetName()}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &unstructured.Unstructured{}
		fresh.SetGroupVersionKind(gvk)
		if err := r.Get(ctx, key, fresh); err != nil {
			return err
		}
		if cur, found, _ := unstructured.NestedMap(fresh.Object, "status", "data"); found && reflect.DeepEqual(cur, desired) {
			return nil
		}
		base := fresh.DeepCopy()
		if err := unstructured.SetNestedMap(fresh.Object, desired, "status", "data"); err != nil {
			return err
		}
		return r.Status().Patch(ctx, fresh, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	})
}

// commonControllerLegCaptured reports whether the core-written capture-leg success latch
// status.captureState.commonController.<leg> is present and true.
func commonControllerLegCaptured(obj *unstructured.Unstructured, leg string) bool {
	v, found, _ := unstructured.NestedBool(obj.Object, "status", "captureState", "commonController", leg)
	return found && v
}

// domainCaptureStateString reads a string field from status.captureState.domainSpecificController
// (the domain-written half of captureState: MCR/VCR names, phase reason/message).
func domainCaptureStateString(obj *unstructured.Unstructured, field string) string {
	v, _, _ := unstructured.NestedString(obj.Object, "status", "captureState", "domainSpecificController", field)
	return v
}

// domainCapturePhase reads status.captureState.domainSpecificController.phase off a domain snapshot.
func domainCapturePhase(obj *unstructured.Unstructured) string {
	return domainCaptureStateString(obj, "phase")
}

// domainCaptureAtLeastPlanned reports whether the domain reached capture barrier 1 (phase Planned or
// Finished): objects created and refs published, so the binder may take over the SnapshotContent.
func domainCaptureAtLeastPlanned(obj *unstructured.Unstructured) bool {
	switch storagev1alpha1.SnapshotCapturePhase(domainCapturePhase(obj)) {
	case storagev1alpha1.SnapshotCapturePhasePlanned, storagev1alpha1.SnapshotCapturePhaseFinished:
		return true
	default:
		return false
	}
}

// domainHasClaimed reports whether a domain controller has CLAIMED this snapshot by writing ANY part of
// status.captureState.domainSpecificController (its MCR/VCR names, children/excluded refs, or phase). The
// binder gates eager content-shell creation for domain-capture kinds on this claim so that a domain which
// plans only a SUBSET of a registered kind's instances (e.g. the storage-foundation VolumeSnapshot domain,
// which skips legacy/unlabeled, vetoed, import-mode, and pre-provisioned VolumeSnapshots — design §11.3)
// leaves the rest unclaimed; the binder then materializes NEITHER an ObjectKeeper NOR a SnapshotContent for
// them, so a pre-existing/legacy CSI VolumeSnapshot stays a plain CSI object.
//
// Deadlock-safety (design §9): the claim is written on the domain's FIRST reconcile, independent of the
// content existing (for the namespace root it is the step-3 EnsureChildren write, strictly BEFORE the
// step-4 orphan-wave Ready gate; for a leaf domain it is the first EnsureManifestCapture/MarkPlanned). It
// is therefore strictly EARLIER than the phase>=Planned projection barrier, so gating on it does not
// reintroduce the eager-shell creation cycle — parent/child content still appear before any Ready/bind
// edge is read.
func domainHasClaimed(obj *unstructured.Unstructured) bool {
	m, found, err := unstructured.NestedMap(obj.Object, "status", "captureState", "domainSpecificController")
	return err == nil && found && m != nil
}

// Barrier-2 (phase=Finished) finalization and the phase=Failed bubble are applied by the single post-bind
// Ready writer in the SnapshotContentController (ready_mirror.go: ownerDomainCapturePhase /
// ownerDomainCaptureFailed), not here — wave7 final-wave-1 removed the binder's steady-state Ready mirror.
// domainCapturePhase / domainCaptureAtLeastPlanned above remain for the Step-1 barrier (isDomainPlanningComplete).
