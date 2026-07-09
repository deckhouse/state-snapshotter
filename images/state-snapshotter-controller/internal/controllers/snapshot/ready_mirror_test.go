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
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

func readyMirrorScheme(t *testing.T) *fake.ClientBuilder {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme)
}

// Pending window: Snapshot.Ready is a verbatim mirror of the bound SnapshotContent.Ready,
// overwriting any stale local reason, gen-gated on the Snapshot.
func TestMirrorSnapshotReadyFromBoundContentCopiesContentReady(t *testing.T) {
	ctx := context.Background()

	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "root-content"}}
	meta.SetStatusCondition(&content.Status.Conditions, metav1.Condition{
		Type:    snapshotpkg.ConditionReady,
		Status:  metav1.ConditionFalse,
		Reason:  snapshotpkg.ReasonChildrenPending,
		Message: "child SnapshotContent leaf-x not ready: reason=ManifestCapturePending",
	})

	parent := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: "ns", Generation: 3}}
	meta.SetStatusCondition(&parent.Status.Conditions, metav1.Condition{
		Type:    snapshotpkg.ConditionReady,
		Status:  metav1.ConditionFalse,
		Reason:  snapshotpkg.ReasonSubtreeManifestCapturePending,
		Message: "stale local reason",
	})

	cl := readyMirrorScheme(t).WithObjects(content, parent).WithStatusSubresource(parent).Build()
	r := &SnapshotReconciler{Client: cl, APIReader: cl}

	if err := r.mirrorSnapshotReadyFromBoundContent(ctx, parent, content, errors.New("subtree pending")); err != nil {
		t.Fatalf("mirror: %v", err)
	}

	fresh := &storagev1alpha1.Snapshot{}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "root"}, fresh); err != nil {
		t.Fatalf("get parent: %v", err)
	}
	got := meta.FindStatusCondition(fresh.Status.Conditions, snapshotpkg.ConditionReady)
	want := meta.FindStatusCondition(content.Status.Conditions, snapshotpkg.ConditionReady)
	if got == nil {
		t.Fatalf("parent has no Ready after mirror")
	}
	if got.Status != want.Status || got.Reason != want.Reason || got.Message != want.Message {
		t.Fatalf("not a verbatim mirror:\n got  (%s/%s/%q)\n want (%s/%s/%q)",
			got.Status, got.Reason, got.Message, want.Status, want.Reason, want.Message)
	}
	if got.ObservedGeneration != 3 {
		t.Fatalf("observedGeneration=%d, want 3", got.ObservedGeneration)
	}
}

// If the bound content has no Ready condition yet, the mirror falls back to Ready=False/
// ManifestCapturePending carrying the transient error message.
func TestMirrorSnapshotReadyFromBoundContentFallbackNoContentReady(t *testing.T) {
	ctx := context.Background()

	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "root-content"}}
	parent := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: "ns", Generation: 1}}

	cl := readyMirrorScheme(t).WithObjects(content, parent).WithStatusSubresource(parent).Build()
	r := &SnapshotReconciler{Client: cl, APIReader: cl}

	if err := r.mirrorSnapshotReadyFromBoundContent(ctx, parent, content, errors.New("subtree manifest capture pending")); err != nil {
		t.Fatalf("mirror: %v", err)
	}

	fresh := &storagev1alpha1.Snapshot{}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "root"}, fresh); err != nil {
		t.Fatalf("get parent: %v", err)
	}
	got := meta.FindStatusCondition(fresh.Status.Conditions, snapshotpkg.ConditionReady)
	if got == nil || got.Status != metav1.ConditionFalse || got.Reason != snapshotpkg.ReasonContentBindingPending {
		t.Fatalf("fallback Ready = %#v, want False/%s", got, snapshotpkg.ReasonContentBindingPending)
	}
	if got.Message != "subtree manifest capture pending" {
		t.Fatalf("fallback message = %q, want transient error text", got.Message)
	}
}

// ManifestsArchived is mirrored verbatim from the bound SnapshotContent onto the root Snapshot,
// gen-gated on the Snapshot. This is the latch the e2e (E1/E3/E6) and the capture RBAC hook read.
func TestMirrorSnapshotManifestsArchivedCopiesContentLatch(t *testing.T) {
	ctx := context.Background()

	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "root-content"}}
	meta.SetStatusCondition(&content.Status.Conditions, metav1.Condition{
		Type:    snapshotpkg.ConditionManifestsArchived,
		Status:  metav1.ConditionTrue,
		Reason:  snapshotpkg.ReasonManifestsArchived,
		Message: "subtree manifests archived",
	})
	parent := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: "ns", Generation: 4}}

	cl := readyMirrorScheme(t).WithObjects(content, parent).WithStatusSubresource(parent).Build()
	r := &SnapshotReconciler{Client: cl, APIReader: cl}

	key := types.NamespacedName{Namespace: "ns", Name: "root"}
	if err := r.mirrorSnapshotManifestsArchivedFromBoundContent(ctx, key, content.Name); err != nil {
		t.Fatalf("mirror: %v", err)
	}

	fresh := &storagev1alpha1.Snapshot{}
	if err := cl.Get(ctx, key, fresh); err != nil {
		t.Fatalf("get parent: %v", err)
	}
	got := meta.FindStatusCondition(fresh.Status.Conditions, snapshotpkg.ConditionManifestsArchived)
	if got == nil || got.Status != metav1.ConditionTrue || got.Reason != snapshotpkg.ReasonManifestsArchived {
		t.Fatalf("ManifestsArchived = %#v, want True/%s", got, snapshotpkg.ReasonManifestsArchived)
	}
	if got.ObservedGeneration != 4 {
		t.Fatalf("observedGeneration=%d, want 4", got.ObservedGeneration)
	}
}

// Latch: once the Snapshot's ManifestsArchived is True it is never downgraded, even if the bound content
// momentarily reports a non-True status (e.g. transient child-content degradation — E3).
func TestMirrorSnapshotManifestsArchivedLatchNeverDowngrades(t *testing.T) {
	ctx := context.Background()

	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "root-content"}}
	meta.SetStatusCondition(&content.Status.Conditions, metav1.Condition{
		Type:    snapshotpkg.ConditionManifestsArchived,
		Status:  metav1.ConditionFalse,
		Reason:  snapshotpkg.ReasonManifestsCapturing,
		Message: "recomputing after child degradation",
	})
	parent := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: "ns", Generation: 2}}
	meta.SetStatusCondition(&parent.Status.Conditions, metav1.Condition{
		Type:    snapshotpkg.ConditionManifestsArchived,
		Status:  metav1.ConditionTrue,
		Reason:  snapshotpkg.ReasonManifestsArchived,
		Message: "subtree manifests archived",
	})

	cl := readyMirrorScheme(t).WithObjects(content, parent).WithStatusSubresource(parent).Build()
	r := &SnapshotReconciler{Client: cl, APIReader: cl}

	key := types.NamespacedName{Namespace: "ns", Name: "root"}
	if err := r.mirrorSnapshotManifestsArchivedFromBoundContent(ctx, key, content.Name); err != nil {
		t.Fatalf("mirror: %v", err)
	}

	fresh := &storagev1alpha1.Snapshot{}
	if err := cl.Get(ctx, key, fresh); err != nil {
		t.Fatalf("get parent: %v", err)
	}
	got := meta.FindStatusCondition(fresh.Status.Conditions, snapshotpkg.ConditionManifestsArchived)
	if got == nil || got.Status != metav1.ConditionTrue {
		t.Fatalf("latch broken: ManifestsArchived = %#v, want still True", got)
	}
}

// If the bound content has no ManifestsArchived condition yet, the mirror is a no-op (capture has not
// archived for the first time): the Snapshot must not gain a ManifestsArchived condition.
func TestMirrorSnapshotManifestsArchivedNoopWhenContentHasNone(t *testing.T) {
	ctx := context.Background()

	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "root-content"}}
	parent := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: "ns", Generation: 1}}

	cl := readyMirrorScheme(t).WithObjects(content, parent).WithStatusSubresource(parent).Build()
	r := &SnapshotReconciler{Client: cl, APIReader: cl}

	key := types.NamespacedName{Namespace: "ns", Name: "root"}
	if err := r.mirrorSnapshotManifestsArchivedFromBoundContent(ctx, key, content.Name); err != nil {
		t.Fatalf("mirror: %v", err)
	}

	fresh := &storagev1alpha1.Snapshot{}
	if err := cl.Get(ctx, key, fresh); err != nil {
		t.Fatalf("get parent: %v", err)
	}
	if got := meta.FindStatusCondition(fresh.Status.Conditions, snapshotpkg.ConditionManifestsArchived); got != nil {
		t.Fatalf("expected no ManifestsArchived on Snapshot, got %#v", got)
	}
}

// The bridge is the single non-mirror writer: Ready=False/ChildrenFailed.
func TestPatchSnapshotChildSnapshotFailedBridge(t *testing.T) {
	ctx := context.Background()

	parent := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: "ns", Generation: 5}}
	cl := readyMirrorScheme(t).WithObjects(parent).WithStatusSubresource(parent).Build()
	r := &SnapshotReconciler{Client: cl, APIReader: cl}

	if _, err := r.patchSnapshotChildSnapshotFailedBridge(ctx, types.NamespacedName{Namespace: "ns", Name: "root"}, "child snapshot ns/child failed: reason=CapturePlanDrift"); err != nil {
		t.Fatalf("bridge patch: %v", err)
	}

	fresh := &storagev1alpha1.Snapshot{}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "root"}, fresh); err != nil {
		t.Fatalf("get parent: %v", err)
	}
	got := meta.FindStatusCondition(fresh.Status.Conditions, snapshotpkg.ConditionReady)
	if got == nil || got.Status != metav1.ConditionFalse || got.Reason != snapshotpkg.ReasonChildrenFailed {
		t.Fatalf("bridge Ready = %#v, want False/%s", got, snapshotpkg.ReasonChildrenFailed)
	}
	if got.ObservedGeneration != 5 {
		t.Fatalf("observedGeneration=%d, want 5", got.ObservedGeneration)
	}
}
