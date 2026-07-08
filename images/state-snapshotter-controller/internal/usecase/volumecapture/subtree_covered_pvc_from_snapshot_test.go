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

package volumecapture

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

func snapChildRef(name string) storagev1alpha1.SnapshotChildRef {
	return storagev1alpha1.SnapshotChildRef{
		APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
		Kind:       "Snapshot",
		Name:       name,
	}
}

// snapNode builds a namespaced Snapshot child node bound to boundContent, linking the given child refs.
func snapNode(ns, name, boundContent string, children ...storagev1alpha1.SnapshotChildRef) *storagev1alpha1.Snapshot {
	return &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Status: storagev1alpha1.SnapshotStatus{
			BoundSnapshotContentName: boundContent,
			ChildrenSnapshotRefs:     children,
		},
	}
}

// contentWithPVCUID builds a cluster-scoped SnapshotContent data-leaf whose single data binding covers uid.
func contentWithPVCUID(name, uid string) *storagev1alpha1.SnapshotContent {
	return &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: storagev1alpha1.SnapshotContentStatus{
			Data: &storagev1alpha1.SnapshotDataBinding{Source: storagev1alpha1.SnapshotSubjectRef{UID: types.UID(uid)}},
		},
	}
}

func buildSnapshotSubtreeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(testSubtreeScheme(t)).WithObjects(objs...).Build()
}

func TestCollectSubtreeCoveredPVCUIDsFromSnapshot_nilRootErrors(t *testing.T) {
	t.Parallel()
	if _, err := CollectSubtreeCoveredPVCUIDsFromSnapshot(context.Background(), buildSnapshotSubtreeClient(t), nil); err == nil {
		t.Fatal("expected error for nil root Snapshot")
	}
}

func TestCollectSubtreeCoveredPVCUIDsFromSnapshot_emptyChildren(t *testing.T) {
	t.Parallel()
	root := snapNode("ns", "root", "")
	covered, err := CollectSubtreeCoveredPVCUIDsFromSnapshot(context.Background(), buildSnapshotSubtreeClient(t, root), root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(covered) != 0 {
		t.Fatalf("expected empty covered set, got %v", covered)
	}
}

func TestCollectSubtreeCoveredPVCUIDsFromSnapshot_directChildren(t *testing.T) {
	t.Parallel()
	ns := "ns"
	childA := snapNode(ns, "child-a", "content-a")
	childB := snapNode(ns, "child-b", "content-b")
	root := snapNode(ns, "root", "", snapChildRef("child-a"), snapChildRef("child-b"))
	cl := buildSnapshotSubtreeClient(t, root, childA, childB, contentWithPVCUID("content-a", "uid-a"), contentWithPVCUID("content-b", "uid-b"))
	covered, err := CollectSubtreeCoveredPVCUIDsFromSnapshot(context.Background(), cl, root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(covered) != 2 {
		t.Fatalf("expected 2 covered uids, got %v", covered)
	}
	for _, want := range []types.UID{"uid-a", "uid-b"} {
		if _, ok := covered[want]; !ok {
			t.Fatalf("expected %q in covered set, got %v", want, covered)
		}
	}
}

func TestCollectSubtreeCoveredPVCUIDsFromSnapshot_nestedAggregator(t *testing.T) {
	t.Parallel()
	ns := "ns"
	// Aggregator child (e.g. a VM snapshot) owns no data itself but links a grandchild disk node whose OWN
	// bound content covers the PVC — coverage must be reached by recursing the Snapshot child graph.
	grandchild := snapNode(ns, "disk", "content-disk")
	aggregator := snapNode(ns, "vm", "content-vm", snapChildRef("disk"))
	root := snapNode(ns, "root", "", snapChildRef("vm"))
	cl := buildSnapshotSubtreeClient(t, root, aggregator, grandchild,
		&storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "content-vm"}},
		contentWithPVCUID("content-disk", "uid-disk"))
	covered, err := CollectSubtreeCoveredPVCUIDsFromSnapshot(context.Background(), cl, root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(covered) != 1 {
		t.Fatalf("expected 1 covered uid, got %v", covered)
	}
	if _, ok := covered["uid-disk"]; !ok {
		t.Fatalf("expected grandchild uid-disk in covered set, got %v", covered)
	}
}

func TestCollectSubtreeCoveredPVCUIDsFromSnapshot_absentVolumeSnapshotChildSelfHeals(t *testing.T) {
	t.Parallel()
	ns := "ns"
	// A referenced-but-absent CSI VolumeSnapshot child is the residual wave's OWN deterministically-named
	// (rootUID, pvcUID) output: the walk SKIPS it (contributes no coverage, no error) so its PVC
	// re-classifies as residual and the wave recreates it at the same name (idempotent). Failing closed here
	// would wedge the wave before the recreate path runs. A missing NON-VolumeSnapshot child still fails
	// closed — see _missingChildFailsClosed.
	vsLeaf := storagev1alpha1.SnapshotChildRef{
		APIVersion: snapshotpkg.CSISnapshotAPIVersion,
		Kind:       snapshotpkg.KindVolumeSnapshot,
		Name:       "orphan-vs",
	}
	root := snapNode(ns, "root", "", vsLeaf)
	covered, err := CollectSubtreeCoveredPVCUIDsFromSnapshot(context.Background(), buildSnapshotSubtreeClient(t, root), root)
	if err != nil {
		t.Fatalf("absent orphan VolumeSnapshot child must self-heal (skip), got error: %v", err)
	}
	if len(covered) != 0 {
		t.Fatalf("absent VolumeSnapshot child must contribute no coverage, got %v", covered)
	}
}

func TestCollectSubtreeCoveredPVCUIDsFromSnapshot_duplicateUIDFailsClosed(t *testing.T) {
	t.Parallel()
	ns := "ns"
	childA := snapNode(ns, "child-a", "content-a")
	childB := snapNode(ns, "child-b", "content-b")
	root := snapNode(ns, "root", "", snapChildRef("child-a"), snapChildRef("child-b"))
	cl := buildSnapshotSubtreeClient(t, root, childA, childB, contentWithPVCUID("content-a", "dup-uid"), contentWithPVCUID("content-b", "dup-uid"))
	_, err := CollectSubtreeCoveredPVCUIDsFromSnapshot(context.Background(), cl, root)
	if err == nil {
		t.Fatal("expected duplicate covered PVC UID error")
	}
	if !errors.Is(err, ErrDuplicateCoveredPVCUID) {
		t.Fatalf("expected ErrDuplicateCoveredPVCUID, got %v", err)
	}
}

func TestCollectSubtreeCoveredPVCUIDsFromSnapshot_missingChildFailsClosed(t *testing.T) {
	t.Parallel()
	ns := "ns"
	// Root references a child that does not exist: coverage must fail closed rather than silently under-cover
	// (which would let an already-captured PVC be re-captured by the residual wave).
	root := snapNode(ns, "root", "", snapChildRef("ghost"))
	_, err := CollectSubtreeCoveredPVCUIDsFromSnapshot(context.Background(), buildSnapshotSubtreeClient(t, root), root)
	if err == nil {
		t.Fatal("expected error for missing child Snapshot")
	}
}

func TestCollectSubtreeCoveredPVCUIDsFromSnapshot_missingBoundContentFailsClosed(t *testing.T) {
	t.Parallel()
	ns := "ns"
	// The child exists and names a bound content, but that content is absent: fail closed (same reason).
	child := snapNode(ns, "child", "gone-content")
	root := snapNode(ns, "root", "", snapChildRef("child"))
	_, err := CollectSubtreeCoveredPVCUIDsFromSnapshot(context.Background(), buildSnapshotSubtreeClient(t, root, child), root)
	if err == nil {
		t.Fatal("expected error for missing bound SnapshotContent")
	}
}

func TestCollectSubtreeCoveredPVCUIDsFromSnapshot_unboundChildContributesNothing(t *testing.T) {
	t.Parallel()
	ns := "ns"
	// A child not yet bound (empty status.boundSnapshotContentName) simply adds no coverage, no error.
	child := snapNode(ns, "child", "")
	root := snapNode(ns, "root", "", snapChildRef("child"))
	covered, err := CollectSubtreeCoveredPVCUIDsFromSnapshot(context.Background(), buildSnapshotSubtreeClient(t, root, child), root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(covered) != 0 {
		t.Fatalf("expected no coverage from unbound child, got %v", covered)
	}
}

func TestCollectSubtreeCoveredPVCUIDsFromSnapshot_boundContentWithoutDataContributesNothing(t *testing.T) {
	t.Parallel()
	ns := "ns"
	// A bound but manifest-only leaf (content without a data binding) contributes no covered PVC UID.
	child := snapNode(ns, "child", "content-empty")
	root := snapNode(ns, "root", "", snapChildRef("child"))
	cl := buildSnapshotSubtreeClient(t, root, child, &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "content-empty"}})
	covered, err := CollectSubtreeCoveredPVCUIDsFromSnapshot(context.Background(), cl, root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(covered) != 0 {
		t.Fatalf("expected no coverage from data-less content, got %v", covered)
	}
}
