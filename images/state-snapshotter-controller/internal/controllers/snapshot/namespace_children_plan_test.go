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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/csdregistry"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// planTestFixture wires the two GVKs, a fake dynamic client seeding the source objects, and the mapping the
// planner consumes. The snapshot GVK is demo.test/v1 DemoSnapshot to match the demoSnapshotChild* helpers
// so weight-layer readiness can be seeded via the shared fixtures.
type planTestFixture struct {
	sourceGVK   schema.GroupVersionKind
	sourceGVR   schema.GroupVersionResource
	snapshotGVK schema.GroupVersionKind
	mapping     csdregistry.EligibleResourceSnapshotMapping
}

func newPlanTestFixture() planTestFixture {
	sourceGVK := schema.GroupVersionKind{Group: "demo.test", Version: "v1", Kind: "DemoSource"}
	sourceGVR := schema.GroupVersionResource{Group: "demo.test", Version: "v1", Resource: "demosources"}
	snapshotGVK := schema.GroupVersionKind{Group: "demo.test", Version: "v1", Kind: "DemoSnapshot"}
	snapshotGVR := schema.GroupVersionResource{Group: "demo.test", Version: "v1", Resource: "demosnapshots"}
	return planTestFixture{
		sourceGVK:   sourceGVK,
		sourceGVR:   sourceGVR,
		snapshotGVK: snapshotGVK,
		mapping: csdregistry.EligibleResourceSnapshotMapping{
			SourceGVR:   sourceGVR,
			SourceGVK:   sourceGVK,
			SnapshotGVR: snapshotGVR,
			SnapshotGVK: snapshotGVK,
		},
	}
}

func (f planTestFixture) source(name, uid string) *unstructured.Unstructured {
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(f.sourceGVK)
	o.SetNamespace("ns1")
	o.SetName(name)
	o.SetUID(types.UID(uid))
	return o
}

func (f planTestFixture) dynamic(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{f.sourceGVR: f.sourceGVK.Kind + "List"},
		objs...,
	)
}

// TestPlanParentOwnedChildGraphLayerBuildsSpecs verifies the build-spec layer helper: a kept source object
// becomes one ChildSpec (named by parent+source UID, carrying spec.sourceRef, no owner ref) plus its ref,
// while a veto-labeled sibling is dropped from expansion and recorded as a top-level exclude.
func TestPlanParentOwnedChildGraphLayerBuildsSpecs(t *testing.T) {
	f := newPlanTestFixture()
	keep := f.source("keep", "uid-keep")
	vetoed := f.source("vetoed", "uid-vetoed")
	vetoed.SetLabels(map[string]string{storagev1alpha1.ExcludeLabelKey: ""})

	r := &SnapshotReconciler{
		Dynamic: f.dynamic(keep, vetoed),
		Client:  fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build(),
	}
	nsSnap := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: "ns1", UID: "root-uid"}}
	selector, err := nsSnap.ResolveResourceSelector()
	if err != nil {
		t.Fatalf("ResolveResourceSelector: %v", err)
	}

	coverage := newSnapshotCoverageChecker(r.Client, nsSnap.Namespace, nil)
	specs, refs, excluded, err := r.planParentOwnedChildGraphLayer(context.Background(), nsSnap, f.mapping, coverage, selector)
	if err != nil {
		t.Fatalf("planParentOwnedChildGraphLayer: %v", err)
	}
	if len(specs) != 1 || len(refs) != 1 {
		t.Fatalf("want exactly one kept child (specs=%d refs=%d)", len(specs), len(refs))
	}
	wantName := snapshotChildSnapshotName(nsSnap.UID, keep.GetUID())
	if refs[0].Name != wantName || refs[0].Kind != f.snapshotGVK.Kind || refs[0].APIVersion != f.snapshotGVK.GroupVersion().String() {
		t.Fatalf("ref mismatch: %+v (want name %q)", refs[0], wantName)
	}
	obj := specs[0].Object
	if obj.GetName() != wantName || obj.GetNamespace() != "ns1" {
		t.Fatalf("spec object identity mismatch: %s/%s", obj.GetNamespace(), obj.GetName())
	}
	if len(obj.GetOwnerReferences()) != 0 {
		t.Fatalf("spec must carry NO owner ref (the SDK stamps it): %+v", obj.GetOwnerReferences())
	}
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		t.Fatalf("spec object must be unstructured, got %T", obj)
	}
	srcRef, found, err := unstructured.NestedStringMap(u.Object, "spec", "sourceRef")
	if err != nil || !found {
		t.Fatalf("spec.sourceRef missing (found=%v err=%v)", found, err)
	}
	if srcRef["kind"] != f.sourceGVK.Kind || srcRef["name"] != "keep" {
		t.Fatalf("spec.sourceRef mismatch: %+v", srcRef)
	}
	if len(excluded) != 1 || excluded[0].Name != "vetoed" || excluded[0].Kind != f.sourceGVK.Kind {
		t.Fatalf("excluded mismatch: %+v", excluded)
	}
}

func TestPlanNamespaceChildrenEmptyMappings(t *testing.T) {
	r := &SnapshotReconciler{}
	nsSnap := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: "ns1", UID: "root-uid"}}
	plan, err := r.planNamespaceChildren(context.Background(), nsSnap, nil)
	if err != nil {
		t.Fatalf("planNamespaceChildren: %v", err)
	}
	if plan.outcome != namespaceChildrenAllPlanned || len(plan.desired) != 0 {
		t.Fatalf("empty mappings must be AllPlanned with no children, got outcome=%d desired=%d", plan.outcome, len(plan.desired))
	}
}

// TestPlanNamespaceChildrenPendingLayer: a single mapping with one source whose child snapshot has not yet
// reached phase>=Planned (not present in the cluster) yields Pending with the spec built (so EnsureChildren
// can create it) and reason ChildrenPending — the same convergence the bespoke path had.
func TestPlanNamespaceChildrenPendingLayer(t *testing.T) {
	f := newPlanTestFixture()
	src := f.source("vm-1", "uid-vm-1")
	r := &SnapshotReconciler{
		Dynamic: f.dynamic(src),
		Client:  fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build(),
	}
	nsSnap := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: "ns1", UID: "root-uid"}}

	plan, err := r.planNamespaceChildren(context.Background(), nsSnap, []csdregistry.EligibleResourceSnapshotMapping{f.mapping})
	if err != nil {
		t.Fatalf("planNamespaceChildren: %v", err)
	}
	if plan.outcome != namespaceChildrenPending {
		t.Fatalf("want Pending outcome, got %d", plan.outcome)
	}
	if plan.reason != snapshotpkg.ReasonChildrenPending {
		t.Fatalf("want reason %q, got %q", snapshotpkg.ReasonChildrenPending, plan.reason)
	}
	if len(plan.desired) != 1 {
		t.Fatalf("pending layer must still build its child spec, got %d", len(plan.desired))
	}
}

// planLostFixtureReconciler wires an AllPlanned single-source fixture (source vm-1 with its child present
// at phase=Planned) and returns the reconciler plus the desired child name, so the lost-child tests can
// stack extra PUBLISHED refs on nsSnap.Status.ChildrenSnapshotRefs. APIReader is the same fake as Client
// (the pre-Planned lost gate reads uncached).
func planLostFixtureReconciler(t *testing.T, extraObjs ...runtime.Object) (*SnapshotReconciler, planTestFixture, string) {
	t.Helper()
	f := newPlanTestFixture()
	src := f.source("vm-1", "uid-vm-1")
	childName := snapshotChildSnapshotName(types.UID("root-uid"), src.GetUID())
	plannedChild := demoSnapshotChildWithPhase(childName, storagev1alpha1.SnapshotCapturePhasePlanned)
	objs := append([]client.Object{}, plannedChild)
	for _, o := range extraObjs {
		objs = append(objs, o.(client.Object))
	}
	cl := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(objs...).Build()
	r := &SnapshotReconciler{Dynamic: f.dynamic(src), Client: cl, APIReader: cl}
	return r, f, childName
}

func lostPublishedRef(name string) storagev1alpha1.SnapshotChildRef {
	return storagev1alpha1.SnapshotChildRef{APIVersion: "demo.test/v1", Kind: "DemoSnapshot", Name: name}
}

// TestPlanNamespaceChildren_LostDomainChild_Terminal: a published domain child whose source vanished (not
// in the freshly-planned desired set) AND whose CR was deleted (absent) is surfaced as terminal
// ChildSnapshotLost pre-Planned (Block E, case 4).
func TestPlanNamespaceChildren_LostDomainChild_Terminal(t *testing.T) {
	r, f, childName := planLostFixtureReconciler(t)
	nsSnap := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: "ns1", UID: "root-uid"}}
	nsSnap.Status.ChildrenSnapshotRefs = []storagev1alpha1.SnapshotChildRef{
		lostPublishedRef(childName),    // still desired + present
		lostPublishedRef("gone-child"), // source gone, CR absent -> lost
	}

	plan, err := r.planNamespaceChildren(context.Background(), nsSnap, []csdregistry.EligibleResourceSnapshotMapping{f.mapping})
	if err != nil {
		t.Fatalf("planNamespaceChildren: %v", err)
	}
	if plan.outcome != namespaceChildrenTerminal {
		t.Fatalf("want Terminal outcome, got %d", plan.outcome)
	}
	if plan.reason != snapshotpkg.ReasonChildSnapshotLost {
		t.Fatalf("want reason %q, got %q", snapshotpkg.ReasonChildSnapshotLost, plan.reason)
	}
}

// TestPlanNamespaceChildren_StaleRefCRPresent_NotLost: a published ref whose source vanished but whose CR
// still exists is NOT lost (losing the source does not un-capture an already-created child).
func TestPlanNamespaceChildren_StaleRefCRPresent_NotLost(t *testing.T) {
	stale := demoSnapshotChild("stale-child", nil)
	r, f, childName := planLostFixtureReconciler(t, stale)
	nsSnap := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: "ns1", UID: "root-uid"}}
	nsSnap.Status.ChildrenSnapshotRefs = []storagev1alpha1.SnapshotChildRef{
		lostPublishedRef(childName),
		lostPublishedRef("stale-child"), // source gone but CR present -> not lost
	}

	plan, err := r.planNamespaceChildren(context.Background(), nsSnap, []csdregistry.EligibleResourceSnapshotMapping{f.mapping})
	if err != nil {
		t.Fatalf("planNamespaceChildren: %v", err)
	}
	if plan.outcome != namespaceChildrenAllPlanned {
		t.Fatalf("want AllPlanned, got %d (reason=%q)", plan.outcome, plan.reason)
	}
}

// TestPlanNamespaceChildren_LostChildPostPlanned_GateOff: past barrier 1 (phase=Planned) the pre-Planned
// gate is off — a lost child is NOT surfaced here (the binder owns Ready post-Planned; the main-side
// owner-mirror folds report it instead).
func TestPlanNamespaceChildren_LostChildPostPlanned_GateOff(t *testing.T) {
	r, f, childName := planLostFixtureReconciler(t)
	nsSnap := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: "ns1", UID: "root-uid"}}
	nsSnap.Status.CaptureState = &storagev1alpha1.CaptureStateStatus{
		DomainSpecificController: &storagev1alpha1.DomainSpecificControllerCaptureState{
			Phase: storagev1alpha1.SnapshotCapturePhasePlanned,
		},
	}
	nsSnap.Status.ChildrenSnapshotRefs = []storagev1alpha1.SnapshotChildRef{
		lostPublishedRef(childName),
		lostPublishedRef("gone-child"),
	}

	plan, err := r.planNamespaceChildren(context.Background(), nsSnap, []csdregistry.EligibleResourceSnapshotMapping{f.mapping})
	if err != nil {
		t.Fatalf("planNamespaceChildren: %v", err)
	}
	if plan.outcome != namespaceChildrenAllPlanned {
		t.Fatalf("want AllPlanned (gate off post-Planned), got %d (reason=%q)", plan.outcome, plan.reason)
	}
}

// TestPlanNamespaceChildrenAllPlanned: with the layer's child snapshot present at phase=Planned, the
// planner advances past the layer and returns AllPlanned.
func TestPlanNamespaceChildrenAllPlanned(t *testing.T) {
	f := newPlanTestFixture()
	src := f.source("vm-1", "uid-vm-1")
	childName := snapshotChildSnapshotName(types.UID("root-uid"), src.GetUID())
	plannedChild := demoSnapshotChildWithPhase(childName, storagev1alpha1.SnapshotCapturePhasePlanned)

	r := &SnapshotReconciler{
		Dynamic: f.dynamic(src),
		Client:  fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(plannedChild).Build(),
	}
	nsSnap := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: "ns1", UID: "root-uid"}}

	plan, err := r.planNamespaceChildren(context.Background(), nsSnap, []csdregistry.EligibleResourceSnapshotMapping{f.mapping})
	if err != nil {
		t.Fatalf("planNamespaceChildren: %v", err)
	}
	if plan.outcome != namespaceChildrenAllPlanned {
		t.Fatalf("want AllPlanned outcome, got %d (reason=%q msg=%q)", plan.outcome, plan.reason, plan.message)
	}
	if len(plan.desired) != 1 {
		t.Fatalf("want one planned child spec, got %d", len(plan.desired))
	}
	if len(plan.excluded) != 0 {
		t.Fatalf("no veto in this fixture; excluded must be empty, got %+v", plan.excluded)
	}
}
