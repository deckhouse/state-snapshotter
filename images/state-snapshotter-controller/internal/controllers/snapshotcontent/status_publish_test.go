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
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// Under the content-single-writer model an orphan CSI VolumeSnapshot is an ordinary domain child (no longer
// a skipped "visibility leaf"): a brand-new edge to a VolumeSnapshot whose bound SnapshotContent is not
// visible yet must REQUEUE (ok=false) rather than publish a dangling edge — exactly like any other child.
func TestPublishSnapshotContentChildrenFromSnapshotRefsRequeuesUnboundVolumeSnapshotChild(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	parent := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "root-content"}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(parent).
		WithStatusSubresource(&storagev1alpha1.SnapshotContent{}).Build()

	ok, err := PublishSnapshotContentChildrenFromSnapshotRefs(ctx, cl, cl, "ns1", parent.Name, []storagev1alpha1.SnapshotChildRef{{
		APIVersion: "snapshot.storage.k8s.io/v1",
		Kind:       "VolumeSnapshot",
		Name:       "nss-vs-orphan",
	}})
	if err != nil {
		t.Fatalf("publish children: %v", err)
	}
	if ok {
		t.Fatal("an unbound VolumeSnapshot child must requeue (ok=false), not publish a dangling edge")
	}
	got := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: parent.Name}, got); err != nil {
		t.Fatalf("get parent content: %v", err)
	}
	if len(got.Status.ChildrenSnapshotContentRefs) != 0 {
		t.Fatalf("no child edge must be published while the VolumeSnapshot child is unbound, got %#v", got.Status.ChildrenSnapshotContentRefs)
	}
}

// TestPublishSnapshotContentChildrenRefsHoldsFrozenSet asserts the Block 4 frozen-set writer guard
// (INV-CONTENT-CHILDREN-2, Option A CEL): once status.childrenSnapshotContentRefs is non-empty the writer
// MUST NOT try to grow/replace it. The empty -> complete first write lands; a later attempt to add a child
// to an already-populated set is held as-is (no patch), so the apiserver CEL never has to reject anything
// and the reconcile never wedges. (The fake client does not enforce CEL, so this pins the WRITER guard; the
// apiserver-level rejection is covered by the snapshotcontent_children_frozen integration test.)
func TestPublishSnapshotContentChildrenRefsHoldsFrozenSet(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "frozen-content"}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(content).
		WithStatusSubresource(&storagev1alpha1.SnapshotContent{}).Build()
	refs := func(names ...string) []storagev1alpha1.SnapshotContentChildRef {
		out := make([]storagev1alpha1.SnapshotContentChildRef, 0, len(names))
		for _, n := range names {
			out = append(out, storagev1alpha1.SnapshotContentChildRef{Name: n})
		}
		return out
	}
	read := func() []storagev1alpha1.SnapshotContentChildRef {
		got := &storagev1alpha1.SnapshotContent{}
		if err := cl.Get(ctx, client.ObjectKey{Name: content.Name}, got); err != nil {
			t.Fatalf("get content: %v", err)
		}
		return got.Status.ChildrenSnapshotContentRefs
	}

	// Empty -> complete first write lands.
	if err := PublishSnapshotContentChildrenRefs(ctx, cl, cl, content.Name, refs("child-a", "child-b")); err != nil {
		t.Fatalf("empty -> complete publish: %v", err)
	}
	if got := read(); len(got) != 2 {
		t.Fatalf("expected the complete 2-child set after the first write, got %#v", got)
	}

	// Attempt to grow the now-frozen set: the writer must hold it as-is (no error, no growth).
	if err := PublishSnapshotContentChildrenRefs(ctx, cl, cl, content.Name, refs("child-a", "child-b", "child-c")); err != nil {
		t.Fatalf("frozen-set hold must not error: %v", err)
	}
	if got := read(); len(got) != 2 {
		t.Fatalf("the frozen set must NOT grow to 3 children, got %#v", got)
	}

	// Idempotent re-publish of the identical set is a clean no-op.
	if err := PublishSnapshotContentChildrenRefs(ctx, cl, cl, content.Name, refs("child-b", "child-a")); err != nil {
		t.Fatalf("idempotent re-publish must not error: %v", err)
	}
	if got := read(); len(got) != 2 {
		t.Fatalf("idempotent re-publish must keep the 2-child set, got %#v", got)
	}
}

// childSnapshotRef builds a strict child Snapshot ref (Snapshot is in the storage scheme so the resolver
// can read status.boundSnapshotContentName via an unstructured Get from the fake client).
func childSnapshotRef(name string) storagev1alpha1.SnapshotChildRef {
	return storagev1alpha1.SnapshotChildRef{
		APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
		Kind:       "Snapshot",
		Name:       name,
	}
}

// TestPublishSnapshotContentChildrenFromSnapshotRefsKeepsDegradedChildWhenContentMissing asserts that an
// ALREADY-PUBLISHED child edge whose bound SnapshotContent has since been deleted (E3 degradation) does NOT
// hard-error the parent publish: the publish must succeed (ok=true, no err) and PRESERVE the degraded child
// ref so the parent content keeps aggregating it as pending (ChildrenReady=False). Previously this returned
// a hard error, wedging the root Snapshot reconcile before its Ready mirror so the root stayed Ready=True.
func TestPublishSnapshotContentChildrenFromSnapshotRefsKeepsDegradedChildWhenContentMissing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	// The edge was already published while the content existed; the content is now gone (degradation).
	parent := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "root-content"},
		Status: storagev1alpha1.SnapshotContentStatus{
			ChildrenSnapshotContentRefs: []storagev1alpha1.SnapshotContentChildRef{{Name: "child-content-missing"}},
		},
	}
	childSnap := boundChildSnapshot("ns1", "child-snap", "child-content-missing")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(parent, childSnap).
		WithStatusSubresource(&storagev1alpha1.SnapshotContent{}, &storagev1alpha1.Snapshot{}).Build()

	ok, err := PublishSnapshotContentChildrenFromSnapshotRefs(ctx, cl, cl, "ns1", parent.Name,
		[]storagev1alpha1.SnapshotChildRef{childSnapshotRef("child-snap")})
	if err != nil {
		t.Fatalf("publish must tolerate a missing (degraded) child content: %v", err)
	}
	if !ok {
		t.Fatal("a degraded child content must not block content child publication")
	}
	got := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: parent.Name}, got); err != nil {
		t.Fatalf("get parent content: %v", err)
	}
	if len(got.Status.ChildrenSnapshotContentRefs) != 1 ||
		got.Status.ChildrenSnapshotContentRefs[0].Name != "child-content-missing" {
		t.Fatalf("degraded child ref must remain published for pending aggregation, got %#v", got.Status.ChildrenSnapshotContentRefs)
	}
}

// TestPublishSnapshotContentChildrenFromSnapshotRefsDoesNotPublishNewDanglingChildEdge asserts the
// initial-bind / cache-lag case: a child is bound but its SnapshotContent is not visible yet and the edge
// has NOT been published before. The publish must NOT introduce a dangling edge (which the later root
// capture-planning subtree walk would have to resolve); it requeues instead (ok=false, no err) and leaves
// the parent content's child refs untouched.
func TestPublishSnapshotContentChildrenFromSnapshotRefsDoesNotPublishNewDanglingChildEdge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	parent := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "root-content"}}
	childSnap := boundChildSnapshot("ns1", "child-snap", "child-content-pending")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(parent, childSnap).
		WithStatusSubresource(&storagev1alpha1.SnapshotContent{}, &storagev1alpha1.Snapshot{}).Build()

	ok, err := PublishSnapshotContentChildrenFromSnapshotRefs(ctx, cl, cl, "ns1", parent.Name,
		[]storagev1alpha1.SnapshotChildRef{childSnapshotRef("child-snap")})
	if err != nil {
		t.Fatalf("publish must not error on a not-yet-visible child content: %v", err)
	}
	if ok {
		t.Fatal("a brand-new edge to a missing child content must requeue (ok=false), not publish a dangling edge")
	}
	got := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: parent.Name}, got); err != nil {
		t.Fatalf("get parent content: %v", err)
	}
	if len(got.Status.ChildrenSnapshotContentRefs) != 0 {
		t.Fatalf("no child edge must be published while the child content is absent, got %#v", got.Status.ChildrenSnapshotContentRefs)
	}
}
