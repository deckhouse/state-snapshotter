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

package snapshotcontent

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// vscBoundProtectionFinalizer is the CSI external-snapshotter finalizer on a bound VolumeSnapshotContent.
// In these tests it merely keeps the object present after Delete so the post-conditions are observable —
// only the sidecar (absent here) removes it once the physical reclaim completes.
const vscBoundProtectionFinalizer = "snapshot.storage.kubernetes.io/volumesnapshotcontent-bound-protection"

// reclaimTestClient wires a fake client that counts Patch and Delete calls and returns a controller bound to
// it. VSCs are added as unstructured objects (their GVK is carried on the object, so no VSC scheme type is
// needed — only the storage scheme, matching the rest of this package's tests).
func reclaimTestClient(t *testing.T, patchCalls, deleteCalls *int, objs ...client.Object) *SnapshotContentController {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			*patchCalls++
			return c.Patch(ctx, obj, patch, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			*deleteCalls++
			return c.Delete(ctx, obj, opts...)
		},
	}).Build()
	return &SnapshotContentController{Client: cl, APIReader: cl}
}

func getReclaimedVSC(t *testing.T, r *SnapshotContentController, name string) *unstructured.Unstructured {
	t.Helper()
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(volumeSnapshotContentGVK())
	if err := r.Get(context.Background(), client.ObjectKey{Name: name}, got); err != nil {
		t.Fatalf("get VSC %s: %v", name, err)
	}
	return got
}

// Happy path: a Retain-policy VSC carrying the bound-protection finalizer is flipped to Delete, stamped with
// the being-deleted annotation, and Delete is issued (the finalizer keeps it present so we can observe it).
func TestReclaimVolumeSnapshotContent_FlipsRetainToDeleteAndStampsAnnotation(t *testing.T) {
	var patchCalls, deleteCalls int
	vsc := vscWithDeletionPolicy("vsc-retain", volumeSnapshotContentRetainPolicy)
	vsc.SetFinalizers([]string{vscBoundProtectionFinalizer})
	r := reclaimTestClient(t, &patchCalls, &deleteCalls, vsc)

	if err := r.reclaimVolumeSnapshotContent(context.Background(), "vsc-retain"); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if patchCalls != 1 {
		t.Fatalf("patchCalls = %d, want 1 (single flip+stamp patch)", patchCalls)
	}
	if deleteCalls != 1 {
		t.Fatalf("deleteCalls = %d, want 1", deleteCalls)
	}

	got := getReclaimedVSC(t, r, "vsc-retain")
	if policy, _, _ := unstructured.NestedString(got.Object, "spec", "deletionPolicy"); policy != volumeSnapshotContentDeletePolicy {
		t.Fatalf("deletionPolicy = %q, want Delete", policy)
	}
	if v := got.GetAnnotations()[volumeSnapshotBeingDeletedAnnotation]; v != volumeSnapshotBeingDeletedValue {
		t.Fatalf("being-deleted annotation = %q, want %q", v, volumeSnapshotBeingDeletedValue)
	}
	if got.GetDeletionTimestamp().IsZero() {
		t.Fatalf("expected a deletionTimestamp (delete issued) on the finalizer-held VSC")
	}
}

// Recovery / regression guard for the old early-return: a VSC already flipped to Delete but left unannotated
// (e.g. reclaimed by the pre-fix code) still gets the being-deleted annotation stamped.
func TestReclaimVolumeSnapshotContent_RecoversAlreadyDeleteButUnannotated(t *testing.T) {
	var patchCalls, deleteCalls int
	vsc := vscWithDeletionPolicy("vsc-delete-unannotated", volumeSnapshotContentDeletePolicy)
	vsc.SetFinalizers([]string{vscBoundProtectionFinalizer})
	r := reclaimTestClient(t, &patchCalls, &deleteCalls, vsc)

	if err := r.reclaimVolumeSnapshotContent(context.Background(), "vsc-delete-unannotated"); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if patchCalls != 1 {
		t.Fatalf("patchCalls = %d, want 1 (annotation-only patch)", patchCalls)
	}

	got := getReclaimedVSC(t, r, "vsc-delete-unannotated")
	if policy, _, _ := unstructured.NestedString(got.Object, "spec", "deletionPolicy"); policy != volumeSnapshotContentDeletePolicy {
		t.Fatalf("deletionPolicy = %q, want unchanged Delete", policy)
	}
	if v := got.GetAnnotations()[volumeSnapshotBeingDeletedAnnotation]; v != volumeSnapshotBeingDeletedValue {
		t.Fatalf("being-deleted annotation = %q, want %q", v, volumeSnapshotBeingDeletedValue)
	}
}

// A non-canonical annotation value is canonicalized to "yes", and unrelated annotations are preserved.
func TestReclaimVolumeSnapshotContent_CanonicalizesWrongAnnotationValuePreservingOthers(t *testing.T) {
	var patchCalls, deleteCalls int
	vsc := vscWithDeletionPolicy("vsc-wrong-anno", volumeSnapshotContentDeletePolicy)
	vsc.SetFinalizers([]string{vscBoundProtectionFinalizer})
	vsc.SetAnnotations(map[string]string{
		volumeSnapshotBeingDeletedAnnotation: "true", // wrong value; sidecar gate wants exactly "yes"
		"unrelated.example.io/keep":          "value",
	})
	r := reclaimTestClient(t, &patchCalls, &deleteCalls, vsc)

	if err := r.reclaimVolumeSnapshotContent(context.Background(), "vsc-wrong-anno"); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if patchCalls != 1 {
		t.Fatalf("patchCalls = %d, want 1", patchCalls)
	}

	got := getReclaimedVSC(t, r, "vsc-wrong-anno")
	annos := got.GetAnnotations()
	if v := annos[volumeSnapshotBeingDeletedAnnotation]; v != volumeSnapshotBeingDeletedValue {
		t.Fatalf("being-deleted annotation = %q, want %q", v, volumeSnapshotBeingDeletedValue)
	}
	if v := annos["unrelated.example.io/keep"]; v != "value" {
		t.Fatalf("unrelated annotation lost/altered: got %q, want %q", v, "value")
	}
}

// No-op when already satisfied: a Delete-policy VSC already carrying the canonical annotation is not patched,
// but the idempotent Delete is still issued exactly once.
func TestReclaimVolumeSnapshotContent_NoOpWhenAlreadyDeleteAndAnnotated(t *testing.T) {
	var patchCalls, deleteCalls int
	vsc := vscWithDeletionPolicy("vsc-satisfied", volumeSnapshotContentDeletePolicy)
	vsc.SetFinalizers([]string{vscBoundProtectionFinalizer})
	vsc.SetAnnotations(map[string]string{volumeSnapshotBeingDeletedAnnotation: volumeSnapshotBeingDeletedValue})
	r := reclaimTestClient(t, &patchCalls, &deleteCalls, vsc)

	if err := r.reclaimVolumeSnapshotContent(context.Background(), "vsc-satisfied"); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if patchCalls != 0 {
		t.Fatalf("patchCalls = %d, want 0 (already satisfied)", patchCalls)
	}
	if deleteCalls != 1 {
		t.Fatalf("deleteCalls = %d, want 1 (idempotent delete)", deleteCalls)
	}
}

// The actual wedge shape: a VSC already terminating (deletionTimestamp + bound-protection finalizer),
// already Delete-policy, but missing the being-deleted annotation. The stamp patch must still succeed so the
// sidecar can complete the physical reclaim and drop its finalizer.
func TestReclaimVolumeSnapshotContent_StampsTerminatingUnannotatedVSC(t *testing.T) {
	var patchCalls, deleteCalls int
	vsc := vscWithDeletionPolicy("vsc-wedged", volumeSnapshotContentDeletePolicy)
	vsc.SetFinalizers([]string{vscBoundProtectionFinalizer})
	now := metav1.NewTime(time.Now())
	vsc.SetDeletionTimestamp(&now)
	r := reclaimTestClient(t, &patchCalls, &deleteCalls, vsc)

	if err := r.reclaimVolumeSnapshotContent(context.Background(), "vsc-wedged"); err != nil {
		t.Fatalf("reclaim on terminating VSC must succeed: %v", err)
	}
	if patchCalls != 1 {
		t.Fatalf("patchCalls = %d, want 1 (annotation stamp on terminating VSC)", patchCalls)
	}

	got := getReclaimedVSC(t, r, "vsc-wedged")
	if v := got.GetAnnotations()[volumeSnapshotBeingDeletedAnnotation]; v != volumeSnapshotBeingDeletedValue {
		t.Fatalf("being-deleted annotation = %q, want %q", v, volumeSnapshotBeingDeletedValue)
	}
	if got.GetDeletionTimestamp().IsZero() {
		t.Fatalf("terminating VSC lost its deletionTimestamp")
	}
}
