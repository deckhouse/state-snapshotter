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
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// recordingQueue captures Add calls so the enqueue helpers can be asserted without a real workqueue. Only
// Add is exercised by enqueueContentDrivenSnapshots; the embedded interface is nil (other methods unused).
type recordingQueue struct {
	workqueue.TypedRateLimitingInterface[reconcile.Request]
	added []reconcile.Request
}

func (q *recordingQueue) Add(item reconcile.Request) { q.added = append(q.added, item) }

func gatedParentTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	return scheme
}

func boundContentIndexClient(scheme *runtime.Scheme, objs ...client.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithIndex(&storagev1alpha1.Snapshot{}, SnapshotBoundContentFieldIndex, func(rawObj client.Object) []string {
			snap, ok := rawObj.(*storagev1alpha1.Snapshot)
			if !ok || snap.Status.BoundSnapshotContentName == "" {
				return nil
			}
			return []string{snap.Status.BoundSnapshotContentName}
		}).
		WithIndex(&storagev1alpha1.Snapshot{}, SnapshotChildrenRefFieldIndex, snapshotChildrenRefIndexValues).
		Build()
}

// storageSnapshotRef builds the immutable content->owning-snapshot back-reference for a storage Snapshot S.
func storageSnapshotRef(name, namespace string) *storagev1alpha1.SnapshotSubjectRef {
	return &storagev1alpha1.SnapshotSubjectRef{
		APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
		Kind:       "Snapshot",
		Name:       name,
		Namespace:  namespace,
	}
}

func storageSnapshotChildRef(name string) storagev1alpha1.SnapshotChildRef {
	return storagev1alpha1.SnapshotChildRef{
		APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
		Kind:       "Snapshot",
		Name:       name,
	}
}

// A direct-child content (spec.snapshotRef -> owning child Snapshot S) maps to the parent Snapshot R that
// lists S in status.childrenSnapshotRefs — the Snapshot whose root-MCR gate reads this content's archive.
func TestGatedParentRequestsFromContent_DirectChildMapsToParent(t *testing.T) {
	ctx := context.Background()
	scheme := gatedParentTestScheme(t)

	root := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: "ns1"},
		Status:     storagev1alpha1.SnapshotStatus{ChildrenSnapshotRefs: []storagev1alpha1.SnapshotChildRef{storageSnapshotChildRef("vm-snap")}},
	}
	content := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "cc-vm"},
		Spec:       storagev1alpha1.SnapshotContentSpec{SnapshotRef: storageSnapshotRef("vm-snap", "ns1")},
	}
	cl := boundContentIndexClient(scheme, root)

	reqs := gatedParentRequestsFromContent(ctx, cl, content)
	want := []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "root"}}}
	if len(reqs) != 1 || reqs[0] != want[0] {
		t.Fatalf("gatedParent reqs = %#v, want %#v", reqs, want)
	}
}

// A grandchild content (spec.snapshotRef -> grandchild Snapshot G) maps to G's IMMEDIATE parent (the mid
// Snapshot that lists G in childrenSnapshotRefs), not to the root — status.childrenSnapshotRefs holds only
// direct children, so each level wakes exactly its own parent.
func TestGatedParentRequestsFromContent_GrandchildMapsToImmediateParent(t *testing.T) {
	ctx := context.Background()
	scheme := gatedParentTestScheme(t)

	root := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: "ns1"},
		Status:     storagev1alpha1.SnapshotStatus{ChildrenSnapshotRefs: []storagev1alpha1.SnapshotChildRef{storageSnapshotChildRef("vm-snap")}},
	}
	mid := &storagev1alpha1.Snapshot{ // immediate parent of the grandchild vd-snap
		ObjectMeta: metav1.ObjectMeta{Name: "vm-snap", Namespace: "ns1"},
		Status:     storagev1alpha1.SnapshotStatus{ChildrenSnapshotRefs: []storagev1alpha1.SnapshotChildRef{storageSnapshotChildRef("vd-snap")}},
	}
	grandchildContent := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "cc-vd"},
		Spec:       storagev1alpha1.SnapshotContentSpec{SnapshotRef: storageSnapshotRef("vd-snap", "ns1")},
	}
	cl := boundContentIndexClient(scheme, root, mid)

	reqs := gatedParentRequestsFromContent(ctx, cl, grandchildContent)
	if len(reqs) != 1 || !hasReq(reqs, "ns1", "vm-snap") {
		t.Fatalf("grandchild gatedParent reqs = %#v, want exactly the immediate parent ns1/vm-snap", reqs)
	}
	if hasReq(reqs, "ns1", "root") {
		t.Fatalf("grandchild must NOT wake the root directly: %#v", reqs)
	}
}

// A root content (its owning snapshot is the root, referenced by no parent) maps to no gated parent, so a
// root's own archive transition never enqueues a spurious parent (and cannot form a self-wake cycle).
func TestGatedParentRequestsFromContent_RootContentMapsToNothing(t *testing.T) {
	ctx := context.Background()
	scheme := gatedParentTestScheme(t)

	root := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: "ns1"},
		Status:     storagev1alpha1.SnapshotStatus{ChildrenSnapshotRefs: []storagev1alpha1.SnapshotChildRef{storageSnapshotChildRef("vm-snap")}},
	}
	// Root content's owning snapshot is the root itself; no Snapshot lists "root" as a child.
	rootContent := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "ns-root"},
		Spec:       storagev1alpha1.SnapshotContentSpec{SnapshotRef: storageSnapshotRef("root", "ns1")},
	}
	cl := boundContentIndexClient(scheme, root)

	if reqs := gatedParentRequestsFromContent(ctx, cl, rootContent); len(reqs) != 0 {
		t.Fatalf("root content gatedParent reqs = %#v, want none", reqs)
	}
}

func TestGatedParentRequestsFromContent_NoSnapshotRef(t *testing.T) {
	ctx := context.Background()
	scheme := gatedParentTestScheme(t)
	cl := boundContentIndexClient(scheme)
	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "cc"}}
	if reqs := gatedParentRequestsFromContent(ctx, cl, content); len(reqs) != 0 {
		t.Fatalf("content without snapshotRef gatedParent reqs = %#v, want none", reqs)
	}
}

func contentWithArchivedCondition(name string, status metav1.ConditionStatus) *storagev1alpha1.SnapshotContent {
	c := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if status != "" {
		meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{
			Type: snapshotpkg.ConditionManifestsArchived, Status: status, Reason: "x",
		})
	}
	return c
}

func TestSnapshotContentManifestsArchivedTrue(t *testing.T) {
	cases := []struct {
		name   string
		status metav1.ConditionStatus
		want   bool
	}{
		{"true", metav1.ConditionTrue, true},
		{"false", metav1.ConditionFalse, false},
		{"absent", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := snapshotContentManifestsArchivedTrue(contentWithArchivedCondition("c", tc.status))
			if got != tc.want {
				t.Fatalf("archivedTrue(%s) = %v, want %v", tc.status, got, tc.want)
			}
		})
	}
	// The UpdateFunc gated-parent wake fires only on the False->True transition.
	oldC := contentWithArchivedCondition("c", metav1.ConditionFalse)
	newC := contentWithArchivedCondition("c", metav1.ConditionTrue)
	if transition := !snapshotContentManifestsArchivedTrue(oldC) && snapshotContentManifestsArchivedTrue(newC); !transition {
		t.Fatal("False->True must be a transition")
	}
	if transition := !snapshotContentManifestsArchivedTrue(newC) && snapshotContentManifestsArchivedTrue(newC); transition {
		t.Fatal("True->True must NOT be a transition (no per-write parent churn)")
	}
	if transition := !snapshotContentManifestsArchivedTrue(oldC) && snapshotContentManifestsArchivedTrue(oldC); transition {
		t.Fatal("False->False must NOT be a transition")
	}
}

// With includeGatedParents=true the helper enqueues BOTH the bound owner Snapshot and the gated parent
// Snapshot; with it false only the bound owner is enqueued.
func TestEnqueueContentDrivenSnapshots_GatedParentToggle(t *testing.T) {
	ctx := context.Background()
	scheme := gatedParentTestScheme(t)

	owner := &storagev1alpha1.Snapshot{ // C is bound to this owning child Snapshot S (vm-snap)
		ObjectMeta: metav1.ObjectMeta{Name: "vm-snap", Namespace: "ns1"},
		Status:     storagev1alpha1.SnapshotStatus{BoundSnapshotContentName: "cc-vm"},
	}
	root := &storagev1alpha1.Snapshot{ // R lists S as a direct child -> gated on cc-vm's archive
		ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: "ns1"},
		Status:     storagev1alpha1.SnapshotStatus{ChildrenSnapshotRefs: []storagev1alpha1.SnapshotChildRef{storageSnapshotChildRef("vm-snap")}},
	}
	content := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "cc-vm"},
		Spec:       storagev1alpha1.SnapshotContentSpec{SnapshotRef: storageSnapshotRef("vm-snap", "ns1")},
	}
	cl := boundContentIndexClient(scheme, owner, root)

	withParents := &recordingQueue{}
	enqueueContentDrivenSnapshots(ctx, cl, content, "update", true, withParents)
	if !hasReq(withParents.added, "ns1", "vm-snap") || !hasReq(withParents.added, "ns1", "root") {
		t.Fatalf("includeGatedParents=true added = %#v, want both vm-snap (owner) and root (parent)", withParents.added)
	}
	if len(withParents.added) != 2 {
		t.Fatalf("includeGatedParents=true added %d reqs, want 2", len(withParents.added))
	}

	boundOnly := &recordingQueue{}
	enqueueContentDrivenSnapshots(ctx, cl, content, "update", false, boundOnly)
	if len(boundOnly.added) != 1 || !hasReq(boundOnly.added, "ns1", "vm-snap") {
		t.Fatalf("includeGatedParents=false added = %#v, want only vm-snap (owner)", boundOnly.added)
	}
}

// When the bound owner and the gated parent resolve to the SAME Snapshot key, the request is enqueued once
// (resync/overlapping channels must not produce duplicate reconcile requests).
func TestEnqueueContentDrivenSnapshots_Dedup(t *testing.T) {
	ctx := context.Background()
	scheme := gatedParentTestScheme(t)

	// X is both the bound owner of cc (boundSnapshotContentName) AND a parent that lists cc's owning
	// snapshot "sref" as a child — so both channels resolve to X.
	x := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "ns1"},
		Status: storagev1alpha1.SnapshotStatus{
			BoundSnapshotContentName: "cc",
			ChildrenSnapshotRefs:     []storagev1alpha1.SnapshotChildRef{storageSnapshotChildRef("sref")},
		},
	}
	content := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "cc"},
		Spec:       storagev1alpha1.SnapshotContentSpec{SnapshotRef: storageSnapshotRef("sref", "ns1")},
	}
	cl := boundContentIndexClient(scheme, x)

	q := &recordingQueue{}
	enqueueContentDrivenSnapshots(ctx, cl, content, "update", true, q)
	if len(q.added) != 1 || !hasReq(q.added, "ns1", "x") {
		t.Fatalf("dedup: added = %#v, want exactly one request for ns1/x", q.added)
	}
}

func hasReq(reqs []reconcile.Request, ns, name string) bool {
	for _, r := range reqs {
		if r.Namespace == ns && r.Name == name {
			return true
		}
	}
	return false
}
