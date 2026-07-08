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
