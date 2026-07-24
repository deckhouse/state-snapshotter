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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// lostOwnerSnapshot builds a domain-capture owning Snapshot at the given phase that has adopted content
// "root", optionally carrying status.childrenSnapshotRefs (the declared-ref/not-yet-frozen mode). The
// owner namespace/name are fixed to the values barrier2OwnedContent's spec.snapshotRef points at.
func lostOwnerSnapshot(t *testing.T, phase string, declaredChildNames ...string) *unstructured.Unstructured {
	t.Helper()
	owner := &unstructured.Unstructured{}
	owner.SetGroupVersionKind(storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot"))
	owner.SetNamespace(lostTestOwnerNS)
	owner.SetName(lostTestOwnerName)
	if err := unstructured.SetNestedField(owner.Object, "root", "status", "boundSnapshotContentName"); err != nil {
		t.Fatalf("set boundSnapshotContentName: %v", err)
	}
	if phase != "" {
		if err := unstructured.SetNestedField(owner.Object, phase, "status", "captureState", "domainSpecificController", "phase"); err != nil {
			t.Fatalf("set phase: %v", err)
		}
	}
	if len(declaredChildNames) > 0 {
		refs := make([]interface{}, 0, len(declaredChildNames))
		for _, cn := range declaredChildNames {
			refs = append(refs, map[string]interface{}{
				"apiVersion": storagev1alpha1.SchemeGroupVersion.String(),
				"kind":       "Snapshot",
				"name":       cn,
			})
		}
		if err := unstructured.SetNestedSlice(owner.Object, refs, "status", "childrenSnapshotRefs"); err != nil {
			t.Fatalf("set childrenSnapshotRefs: %v", err)
		}
	}
	return owner
}

const (
	lostTestOwnerNS   = "ns1"
	lostTestOwnerName = "owner"
)

// lostFrozenContent builds the owner's bound content with a frozen status.childrenSnapshotContentRefs
// edge set (the frozen-edge mode).
func lostFrozenContent(t *testing.T, childContentNames ...string) *unstructured.Unstructured {
	t.Helper()
	content := barrier2OwnedContent(t)
	edges := make([]interface{}, 0, len(childContentNames))
	for _, cn := range childContentNames {
		edges = append(edges, map[string]interface{}{"name": cn})
	}
	if err := unstructured.SetNestedSlice(content.Object, edges, "status", "childrenSnapshotContentRefs"); err != nil {
		t.Fatalf("set childrenSnapshotContentRefs: %v", err)
	}
	return content
}

// childCRName is the deterministic owning-CR name lostChildContent wires into a child content's
// spec.snapshotRef. Seed a Snapshot with this name (lostChildSnapshotCR) to simulate the child CR being
// present — or alive again after a manual restore; omit it to simulate the child CR being deleted.
func childCRName(contentName string) string { return contentName + "-cr" }

// lostChildContent builds a child SnapshotContent whose spec.snapshotRef points at its owning child CR
// (childCRName). Its Ready condition is set from ready; CR presence is controlled separately by seeding
// (or not) the corresponding Snapshot in the fake client.
func lostChildContent(name string, ready bool) *storagev1alpha1.SnapshotContent {
	c := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: name}}
	c.Spec.SnapshotRef = &storagev1alpha1.SnapshotSubjectRef{
		APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
		Kind:       "Snapshot",
		Name:       childCRName(name),
		Namespace:  lostTestOwnerNS,
	}
	readyStatus := metav1.ConditionFalse
	if ready {
		readyStatus = metav1.ConditionTrue
	}
	c.Status.Conditions = append(c.Status.Conditions, metav1.Condition{
		Type:               snapshot.ConditionReady,
		Status:             readyStatus,
		Reason:             "x",
		Message:            "x",
		LastTransitionTime: metav1.Now(),
	})
	return c
}

// lostChildSnapshotCR builds a namespaced child Snapshot CR in the owner's namespace, used to seed a
// present (not-lost / restored) child in either detection mode.
func lostChildSnapshotCR(name string) *unstructured.Unstructured {
	child := &unstructured.Unstructured{}
	child.SetGroupVersionKind(storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot"))
	child.SetNamespace(lostTestOwnerNS)
	child.SetName(name)
	return child
}

// lostChildSnapshotCRTerminating builds a child Snapshot CR that is present but Terminating (has a
// deletionTimestamp). A finalizer is required so the fake client keeps the object around instead of
// dropping it on create.
func lostChildSnapshotCRTerminating(name string) *unstructured.Unstructured {
	child := lostChildSnapshotCR(name)
	child.SetFinalizers([]string{"test.state-snapshotter.deckhouse.io/hold"})
	now := metav1.Now()
	child.SetDeletionTimestamp(&now)
	return child
}

// lostChildContentTerminating builds a child SnapshotContent that is present but Terminating.
func lostChildContentTerminating(name string, ready bool) *storagev1alpha1.SnapshotContent {
	c := lostChildContent(name, ready)
	c.Finalizers = []string{"test.state-snapshotter.deckhouse.io/hold"}
	now := metav1.Now()
	c.DeletionTimestamp = &now
	return c
}

func newLostChildrenController(t *testing.T, objs ...client.Object) *SnapshotContentController {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}
	statusObjs := make([]client.Object, len(objs))
	copy(statusObjs, objs)
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(statusObjs...).
		Build()
	return &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}
}

func TestLatchBoundSnapshotDeletedForMissingOwner(t *testing.T) {
	ctx := context.Background()

	t.Run("UID-bearing binding latches once", func(t *testing.T) {
		content := lostChildContent("child-1", true)
		content.Spec.SnapshotRef.UID = "snapshot-uid"
		raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(content)
		if err != nil {
			t.Fatalf("convert content: %v", err)
		}
		contentObj := &unstructured.Unstructured{Object: raw}
		contentObj.SetGroupVersionKind(storagev1alpha1.SchemeGroupVersion.WithKind("SnapshotContent"))
		r := newLostChildrenController(t, content)

		latched, changed, err := r.latchBoundSnapshotDeletedForMissingOwner(ctx, contentObj)
		if err != nil {
			t.Fatalf("latch: %v", err)
		}
		if !latched || !changed {
			t.Fatalf("first latch = latched:%t changed:%t, want true/true", latched, changed)
		}

		fresh := &storagev1alpha1.SnapshotContent{}
		if err := r.APIReader.Get(ctx, client.ObjectKey{Name: content.Name}, fresh); err != nil {
			t.Fatalf("get content: %v", err)
		}
		if !fresh.Status.BoundSnapshotDeleted {
			t.Fatal("boundSnapshotDeleted = false, want true")
		}

		latched, changed, err = r.latchBoundSnapshotDeletedForMissingOwner(ctx, contentObj)
		if err != nil {
			t.Fatalf("repeat latch: %v", err)
		}
		if !latched || changed {
			t.Fatalf("repeat latch = latched:%t changed:%t, want true/false", latched, changed)
		}
	})

	t.Run("incomplete legacy binding stays fail-soft", func(t *testing.T) {
		content := lostChildContent("child-legacy", true)
		raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(content)
		if err != nil {
			t.Fatalf("convert content: %v", err)
		}
		contentObj := &unstructured.Unstructured{Object: raw}
		contentObj.SetGroupVersionKind(storagev1alpha1.SchemeGroupVersion.WithKind("SnapshotContent"))
		r := newLostChildrenController(t, content)

		latched, changed, err := r.latchBoundSnapshotDeletedForMissingOwner(ctx, contentObj)
		if err != nil {
			t.Fatalf("latch: %v", err)
		}
		if latched || changed {
			t.Fatalf("incomplete binding latch = latched:%t changed:%t, want false/false", latched, changed)
		}
	})
}

// detectLostDeclaredchildren covers the full detection matrix in isolation.
func TestDetectLostDeclaredChildren(t *testing.T) {
	ctx := context.Background()
	finished := string(storagev1alpha1.SnapshotCapturePhaseFinished)
	planned := string(storagev1alpha1.SnapshotCapturePhasePlanned)

	t.Run("pre-Planned no-op even with a missing frozen edge", func(t *testing.T) {
		owner := lostOwnerSnapshot(t, string(storagev1alpha1.SnapshotCapturePhasePlanning))
		content := lostFrozenContent(t, "child-1")
		r := newLostChildrenController(t)
		reason, _, err := r.detectLostDeclaredChildren(ctx, owner, content)
		if err != nil {
			t.Fatalf("detect: %v", err)
		}
		if reason != "" {
			t.Fatalf("reason = %q, want empty (pre-Planned)", reason)
		}
	})

	t.Run("owner being deleted is a no-op", func(t *testing.T) {
		owner := lostOwnerSnapshot(t, finished)
		if err := unstructured.SetNestedField(owner.Object, "2026-01-01T00:00:00Z", "metadata", "deletionTimestamp"); err != nil {
			t.Fatalf("set deletionTimestamp: %v", err)
		}
		content := lostFrozenContent(t, "child-1")
		r := newLostChildrenController(t)
		reason, _, err := r.detectLostDeclaredChildren(ctx, owner, content)
		if err != nil {
			t.Fatalf("detect: %v", err)
		}
		if reason != "" {
			t.Fatalf("reason = %q, want empty (owner deleting)", reason)
		}
	})

	t.Run("frozen edge: child content missing -> Lost", func(t *testing.T) {
		owner := lostOwnerSnapshot(t, finished)
		content := lostFrozenContent(t, "child-1")
		r := newLostChildrenController(t) // child-1 not seeded
		reason, msg, err := r.detectLostDeclaredChildren(ctx, owner, content)
		if err != nil {
			t.Fatalf("detect: %v", err)
		}
		if reason != snapshot.ReasonChildSnapshotLost {
			t.Fatalf("reason = %q, want %q", reason, snapshot.ReasonChildSnapshotLost)
		}
		if msg == "" {
			t.Fatalf("expected a non-empty message naming the child")
		}
	})

	t.Run("frozen edge: child alive -> none", func(t *testing.T) {
		owner := lostOwnerSnapshot(t, finished)
		content := lostFrozenContent(t, "child-1")
		r := newLostChildrenController(t, lostChildContent("child-1", true), lostChildSnapshotCR(childCRName("child-1")))
		reason, _, err := r.detectLostDeclaredChildren(ctx, owner, content)
		if err != nil {
			t.Fatalf("detect: %v", err)
		}
		if reason != "" {
			t.Fatalf("reason = %q, want empty (child alive)", reason)
		}
	})

	t.Run("frozen edge: child CR deleted + content Ready -> Deleted", func(t *testing.T) {
		owner := lostOwnerSnapshot(t, finished)
		content := lostFrozenContent(t, "child-1")
		r := newLostChildrenController(t, lostChildContent("child-1", true)) // CR not seeded (deleted)
		reason, msg, err := r.detectLostDeclaredChildren(ctx, owner, content)
		if err != nil {
			t.Fatalf("detect: %v", err)
		}
		if reason != snapshot.ReasonChildSnapshotDeleted {
			t.Fatalf("reason = %q, want %q", reason, snapshot.ReasonChildSnapshotDeleted)
		}
		if msg == "" {
			t.Fatalf("expected a non-empty message naming the deleted child and its surviving content")
		}
	})

	t.Run("frozen edge: child CR deleted + content not Ready -> Lost", func(t *testing.T) {
		owner := lostOwnerSnapshot(t, finished)
		content := lostFrozenContent(t, "child-1")
		r := newLostChildrenController(t, lostChildContent("child-1", false)) // CR not seeded, content not Ready
		reason, _, err := r.detectLostDeclaredChildren(ctx, owner, content)
		if err != nil {
			t.Fatalf("detect: %v", err)
		}
		if reason != snapshot.ReasonChildSnapshotLost {
			t.Fatalf("reason = %q, want %q", reason, snapshot.ReasonChildSnapshotLost)
		}
	})

	t.Run("frozen edge: Lost wins over Deleted", func(t *testing.T) {
		owner := lostOwnerSnapshot(t, finished)
		content := lostFrozenContent(t, "deleted-in-bin", "gone")
		// deleted-in-bin: content Ready + CR absent -> recoverable; gone: content missing -> Lost must win.
		r := newLostChildrenController(t, lostChildContent("deleted-in-bin", true))
		reason, _, err := r.detectLostDeclaredChildren(ctx, owner, content)
		if err != nil {
			t.Fatalf("detect: %v", err)
		}
		if reason != snapshot.ReasonChildSnapshotLost {
			t.Fatalf("reason = %q, want %q", reason, snapshot.ReasonChildSnapshotLost)
		}
	})

	t.Run("frozen edge: owning child CR Terminating + content Ready -> Deleted (fail-fast)", func(t *testing.T) {
		owner := lostOwnerSnapshot(t, finished)
		content := lostFrozenContent(t, "child-1")
		// Owning CR present but Terminating (doomed): treated as absent -> content still Ready -> Deleted.
		r := newLostChildrenController(t, lostChildContent("child-1", true), lostChildSnapshotCRTerminating(childCRName("child-1")))
		reason, _, err := r.detectLostDeclaredChildren(ctx, owner, content)
		if err != nil {
			t.Fatalf("detect: %v", err)
		}
		if reason != snapshot.ReasonChildSnapshotDeleted {
			t.Fatalf("reason = %q, want %q (Terminating owner CR = deleted, content survives)", reason, snapshot.ReasonChildSnapshotDeleted)
		}
	})

	t.Run("frozen edge: child content Terminating -> Lost (fail-fast)", func(t *testing.T) {
		owner := lostOwnerSnapshot(t, finished)
		content := lostFrozenContent(t, "child-1")
		// The frozen child content itself is Terminating: its durable data is going away -> terminal Lost,
		// even though its owning CR is still alive.
		r := newLostChildrenController(t, lostChildContentTerminating("child-1", true), lostChildSnapshotCR(childCRName("child-1")))
		reason, msg, err := r.detectLostDeclaredChildren(ctx, owner, content)
		if err != nil {
			t.Fatalf("detect: %v", err)
		}
		if reason != snapshot.ReasonChildSnapshotLost {
			t.Fatalf("reason = %q, want %q (Terminating child content)", reason, snapshot.ReasonChildSnapshotLost)
		}
		if msg == "" {
			t.Fatalf("expected a non-empty message")
		}
	})

	t.Run("declared-ref mode: child CR Terminating -> Lost (fail-fast)", func(t *testing.T) {
		owner := lostOwnerSnapshot(t, planned, "child-snap")
		content := barrier2OwnedContent(t) // no frozen edges
		r := newLostChildrenController(t, lostChildSnapshotCRTerminating("child-snap"))
		reason, _, err := r.detectLostDeclaredChildren(ctx, owner, content)
		if err != nil {
			t.Fatalf("detect: %v", err)
		}
		if reason != snapshot.ReasonChildSnapshotLost {
			t.Fatalf("reason = %q, want %q (Terminating declared child CR)", reason, snapshot.ReasonChildSnapshotLost)
		}
	})

	t.Run("declared-ref mode: child CR missing -> Lost", func(t *testing.T) {
		owner := lostOwnerSnapshot(t, planned, "child-snap")
		content := barrier2OwnedContent(t) // no frozen edges
		r := newLostChildrenController(t)  // child-snap not seeded
		reason, _, err := r.detectLostDeclaredChildren(ctx, owner, content)
		if err != nil {
			t.Fatalf("detect: %v", err)
		}
		if reason != snapshot.ReasonChildSnapshotLost {
			t.Fatalf("reason = %q, want %q", reason, snapshot.ReasonChildSnapshotLost)
		}
	})

	t.Run("declared-ref mode: child CR present -> none", func(t *testing.T) {
		owner := lostOwnerSnapshot(t, planned, "child-snap")
		content := barrier2OwnedContent(t)
		r := newLostChildrenController(t, lostChildSnapshotCR("child-snap"))
		reason, _, err := r.detectLostDeclaredChildren(ctx, owner, content)
		if err != nil {
			t.Fatalf("detect: %v", err)
		}
		if reason != "" {
			t.Fatalf("reason = %q, want empty (child present)", reason)
		}
	})
}

// TestMirrorReadyToOwnerSnapshot_LostChildrenFold asserts the fold precedence on the owner Ready mirror:
// a terminal Lost overrides even a phase=Finished Ready=True; a non-terminal Deleted downgrades a
// would-be Ready=True; and a restored child (its owning CR present again) self-heals the mirror to Ready.
func TestMirrorReadyToOwnerSnapshot_LostChildrenFold(t *testing.T) {
	ctx := context.Background()
	finished := string(storagev1alpha1.SnapshotCapturePhaseFinished)

	readOwnerReady := func(t *testing.T, r *SnapshotContentController) *metav1.Condition {
		t.Helper()
		fresh := &unstructured.Unstructured{}
		fresh.SetGroupVersionKind(storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot"))
		if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: "ns1", Name: "owner"}, fresh); err != nil {
			t.Fatalf("get owner: %v", err)
		}
		like, err := snapshot.ExtractSnapshotLike(fresh)
		if err != nil {
			t.Fatalf("extract: %v", err)
		}
		got := snapshot.GetCondition(like, snapshot.ConditionReady)
		if got == nil {
			t.Fatalf("owner has no Ready condition")
		}
		return got
	}

	t.Run("Lost overrides a phase=Finished Ready=True", func(t *testing.T) {
		owner := lostOwnerSnapshot(t, finished)
		r := newLostChildrenController(t, owner) // child-1 missing
		content := lostFrozenContent(t, "child-1")
		if err := r.mirrorReadyToOwnerSnapshot(ctx, content); err != nil {
			t.Fatalf("mirror: %v", err)
		}
		got := readOwnerReady(t, r)
		if got.Status != metav1.ConditionFalse || got.Reason != snapshot.ReasonChildSnapshotLost {
			t.Fatalf("Ready = %s/%s, want False/%s", got.Status, got.Reason, snapshot.ReasonChildSnapshotLost)
		}
	})

	t.Run("Deleted downgrades a phase=Finished Ready=True", func(t *testing.T) {
		owner := lostOwnerSnapshot(t, finished)
		r := newLostChildrenController(t, owner, lostChildContent("child-1", true)) // child CR deleted
		content := lostFrozenContent(t, "child-1")
		if err := r.mirrorReadyToOwnerSnapshot(ctx, content); err != nil {
			t.Fatalf("mirror: %v", err)
		}
		got := readOwnerReady(t, r)
		if got.Status != metav1.ConditionFalse || got.Reason != snapshot.ReasonChildSnapshotDeleted {
			t.Fatalf("Ready = %s/%s, want False/%s", got.Status, got.Reason, snapshot.ReasonChildSnapshotDeleted)
		}
	})

	t.Run("restored child self-heals the mirror back to Ready", func(t *testing.T) {
		owner := lostOwnerSnapshot(t, finished)
		// Content Ready + its owning child CR present again (manually restored) -> healthy.
		r := newLostChildrenController(t, owner, lostChildContent("child-1", true), lostChildSnapshotCR(childCRName("child-1")))
		content := lostFrozenContent(t, "child-1")
		if err := r.mirrorReadyToOwnerSnapshot(ctx, content); err != nil {
			t.Fatalf("mirror: %v", err)
		}
		got := readOwnerReady(t, r)
		if got.Status != metav1.ConditionTrue {
			t.Fatalf("Ready = %s/%s, want True", got.Status, got.Reason)
		}
	})
}
