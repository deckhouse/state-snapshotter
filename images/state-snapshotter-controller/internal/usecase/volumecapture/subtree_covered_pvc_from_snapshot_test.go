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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	vcctrl "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/volumecapture"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
)

func snapChildRef(name string) storagev1alpha1.SnapshotChildRef {
	return storagev1alpha1.SnapshotChildRef{
		APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
		Kind:       "Snapshot",
		Name:       name,
	}
}

// aggChildRef references an aggregator (non-data-bearing) child kind, distinct from the data-leaf "Snapshot"
// kind, so a test can drive the coverage data-bearing predicate by kind.
func aggChildRef(name string) storagev1alpha1.SnapshotChildRef {
	return storagev1alpha1.SnapshotChildRef{
		APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
		Kind:       "AggregatorSnapshot",
		Name:       name,
	}
}

// leafKindDataBearing treats only the "Snapshot" data-leaf kind (and native CSI VolumeSnapshot) as
// data-bearing, so aggregator kinds are correctly skipped by coverage.
func leafKindDataBearing(kind string) bool {
	return kind == "Snapshot" || kind == snapshotpkg.KindVolumeSnapshot
}

// snapNodeUnstructured builds a snapshot-graph node of an arbitrary apiVersion/kind (so tests can model a
// data-leaf vs an aggregator that the data-bearing predicate keys on), bound to boundContent (empty =
// unbound) and linking children. Stored as unstructured; the coverage walk fetches nodes as unstructured.
func snapNodeUnstructured(ns, name, apiVersion, kind, boundContent string, children ...storagev1alpha1.SnapshotChildRef) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion(apiVersion)
	obj.SetKind(kind)
	obj.SetNamespace(ns)
	obj.SetName(name)
	if boundContent != "" {
		_ = unstructured.SetNestedField(obj.Object, boundContent, "status", "boundSnapshotContentName")
	}
	if len(children) > 0 {
		items := make([]interface{}, 0, len(children))
		for _, ch := range children {
			items = append(items, map[string]interface{}{"apiVersion": ch.APIVersion, "kind": ch.Kind, "name": ch.Name})
		}
		_ = unstructured.SetNestedSlice(obj.Object, items, "status", "childrenSnapshotRefs")
	}
	return obj
}

func withCaptureVCRName(obj *unstructured.Unstructured, vcrName string) *unstructured.Unstructured {
	_ = unstructured.SetNestedField(obj.Object, vcrName, "status", "captureState", "domainSpecificController", "volumeCaptureRequestName")
	return obj
}

func withSnapshotSourceUID(obj *unstructured.Unstructured, uid string) *unstructured.Unstructured {
	_ = unstructured.SetNestedField(obj.Object, uid, "status", "snapshotSource", "uid")
	return obj
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
	if _, err := CollectSubtreeCoveredPVCUIDsFromSnapshot(context.Background(), buildSnapshotSubtreeClient(t), nil, allKindsDataBearing); err == nil {
		t.Fatal("expected error for nil root Snapshot")
	}
}

func TestCollectSubtreeCoveredPVCUIDsFromSnapshot_emptyChildren(t *testing.T) {
	t.Parallel()
	root := snapNode("ns", "root", "")
	covered, err := CollectSubtreeCoveredPVCUIDsFromSnapshot(context.Background(), buildSnapshotSubtreeClient(t, root), root, allKindsDataBearing)
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
	covered, err := CollectSubtreeCoveredPVCUIDsFromSnapshot(context.Background(), cl, root, allKindsDataBearing)
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
	// Aggregator child (a non-data-bearing kind, e.g. a VM snapshot) owns no data itself but links a
	// grandchild disk node whose OWN bound content covers the PVC — coverage must skip the aggregator (per the
	// CSD data-bearing predicate, NOT the shape of the tree) yet still recurse into it to reach the disk leaf.
	grandchild := snapNode(ns, "disk", "content-disk")
	aggregator := snapNodeUnstructured(ns, "vm", storagev1alpha1.SchemeGroupVersion.String(), "AggregatorSnapshot", "content-vm", snapChildRef("disk"))
	root := snapNode(ns, "root", "", aggChildRef("vm"))
	cl := buildSnapshotSubtreeClient(t, root, grandchild, aggregator,
		&storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "content-vm"}},
		contentWithPVCUID("content-disk", "uid-disk"))
	covered, err := CollectSubtreeCoveredPVCUIDsFromSnapshot(context.Background(), cl, root, leafKindDataBearing)
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
	covered, err := CollectSubtreeCoveredPVCUIDsFromSnapshot(context.Background(), buildSnapshotSubtreeClient(t, root), root, allKindsDataBearing)
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
	_, err := CollectSubtreeCoveredPVCUIDsFromSnapshot(context.Background(), cl, root, allKindsDataBearing)
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
	_, err := CollectSubtreeCoveredPVCUIDsFromSnapshot(context.Background(), buildSnapshotSubtreeClient(t, root), root, allKindsDataBearing)
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
	_, err := CollectSubtreeCoveredPVCUIDsFromSnapshot(context.Background(), buildSnapshotSubtreeClient(t, root, child), root, allKindsDataBearing)
	if err == nil {
		t.Fatal("expected error for missing bound SnapshotContent")
	}
}

func TestCollectSubtreeCoveredPVCUIDsFromSnapshot_unboundNonDataBearingChildContributesNothing(t *testing.T) {
	t.Parallel()
	ns := "ns"
	// A non-data-bearing (aggregator-kind) child that is not yet bound adds no coverage and no error: coverage
	// only ever expects a covered PVC UID from a data-bearing kind.
	child := snapNodeUnstructured(ns, "child", storagev1alpha1.SchemeGroupVersion.String(), "AggregatorSnapshot", "")
	root := snapNode(ns, "root", "", aggChildRef("child"))
	covered, err := CollectSubtreeCoveredPVCUIDsFromSnapshot(context.Background(), buildSnapshotSubtreeClient(t, root, child), root, leafKindDataBearing)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(covered) != 0 {
		t.Fatalf("expected no coverage from unbound aggregator child, got %v", covered)
	}
}

func TestCollectSubtreeCoveredPVCUIDsFromSnapshot_dataBearingChildWithoutObservableCoverageIsPending(t *testing.T) {
	t.Parallel()
	ns := "ns"
	// A data-bearing child that is not yet observable (unbound, no in-flight VCR, no snapshotSource) must
	// fail closed with ErrSubtreeDataRefsPending so the residual wave requeues rather than under-cover — the
	// relaxed phase>=Planned gate only guarantees DIRECT children are Planned, so a still-Planning descendant
	// (or a Planned-but-not-yet-bound child before its capture refs land) must not be silently dropped.
	child := snapNode(ns, "child", "")
	root := snapNode(ns, "root", "", snapChildRef("child"))
	_, err := CollectSubtreeCoveredPVCUIDsFromSnapshot(context.Background(), buildSnapshotSubtreeClient(t, root, child), root, leafKindDataBearing)
	if !errors.Is(err, ErrSubtreeDataRefsPending) {
		t.Fatalf("expected ErrSubtreeDataRefsPending for an unobservable data-bearing child, got %v", err)
	}
}

func TestCollectSubtreeCoveredPVCUIDsFromSnapshot_boundManifestOnlyChildContributesNothing(t *testing.T) {
	t.Parallel()
	ns := "ns"
	// A bound non-data-bearing (manifest-only) leaf contributes no covered PVC UID and no error.
	child := snapNodeUnstructured(ns, "child", storagev1alpha1.SchemeGroupVersion.String(), "AggregatorSnapshot", "content-empty")
	root := snapNode(ns, "root", "", aggChildRef("child"))
	cl := buildSnapshotSubtreeClient(t, root, child, &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "content-empty"}})
	covered, err := CollectSubtreeCoveredPVCUIDsFromSnapshot(context.Background(), cl, root, leafKindDataBearing)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(covered) != 0 {
		t.Fatalf("expected no coverage from a manifest-only leaf, got %v", covered)
	}
}

func TestCollectSubtreeCoveredPVCUIDsFromSnapshot_plannedUnboundChildCoveredViaOwnerVCR(t *testing.T) {
	t.Parallel()
	ns := "ns"
	// A Planned but not-yet-bound data-bearing child is covered via the owner fallback read DIRECTLY off the
	// child's captureState.volumeCaptureRequestName (published by Planned) — no bound content required. This
	// is the case the previous "boundSnapshotContentName empty -> contribute nothing" short-circuit missed.
	child := withCaptureVCRName(snapNodeUnstructured(ns, "child", storagev1alpha1.SchemeGroupVersion.String(), "Snapshot", ""), "vcr-child")
	root := snapNode(ns, "root", "", snapChildRef("child"))
	vcr := vcctrl.NewVolumeCaptureRequestObject(ns, "vcr-child", metav1.OwnerReference{}, []vcpkg.Target{{
		UID: "uid-vcr", APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc-a", Namespace: ns,
	}})
	cl := buildSnapshotSubtreeClient(t, root, child, vcr)
	covered, err := CollectSubtreeCoveredPVCUIDsFromSnapshot(context.Background(), cl, root, leafKindDataBearing)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := covered[types.UID("uid-vcr")]; !ok {
		t.Fatalf("expected the in-flight VCR target UID in covered set, got %v", covered)
	}
}

func TestCollectSubtreeCoveredPVCUIDsFromSnapshot_volumeSnapshotCoveredViaSnapshotSource(t *testing.T) {
	t.Parallel()
	ns := "ns"
	// A native-CSI VolumeSnapshot child (no VCR) is covered via the owner's status.snapshotSource.uid,
	// published at adoption before Planned (§11.7).
	vs := withSnapshotSourceUID(snapNodeUnstructured(ns, "orphan-vs", snapshotpkg.CSISnapshotAPIVersion, snapshotpkg.KindVolumeSnapshot, ""), "uid-src")
	root := snapNode(ns, "root", "", storagev1alpha1.SnapshotChildRef{
		APIVersion: snapshotpkg.CSISnapshotAPIVersion, Kind: snapshotpkg.KindVolumeSnapshot, Name: "orphan-vs",
	})
	cl := buildSnapshotSubtreeClient(t, root, vs)
	covered, err := CollectSubtreeCoveredPVCUIDsFromSnapshot(context.Background(), cl, root, leafKindDataBearing)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := covered[types.UID("uid-src")]; !ok {
		t.Fatalf("expected the VolumeSnapshot source UID in covered set, got %v", covered)
	}
}
