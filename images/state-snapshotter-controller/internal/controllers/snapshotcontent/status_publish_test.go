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

func TestPublishSnapshotContentChildrenFromSnapshotRefsSkipsVolumeSnapshotVisibilityLeaf(t *testing.T) {
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
	if !ok {
		t.Fatal("VolumeSnapshot visibility leaf must not block content child publication")
	}
	got := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: parent.Name}, got); err != nil {
		t.Fatalf("get parent content: %v", err)
	}
	if len(got.Status.ChildrenSnapshotContentRefs) != 0 {
		t.Fatalf("VolumeSnapshot visibility leaf must not become content child, got %#v", got.Status.ChildrenSnapshotContentRefs)
	}
}
