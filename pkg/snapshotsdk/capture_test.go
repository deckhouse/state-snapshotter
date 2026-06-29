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

package snapshotsdk_test

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk"
)

const captureNS = "ns"

// fakeProvider is a test double for VolumeCaptureProvider. EnsureChildren never calls it; for manifest
// tests OwnedPVCTarget returns a fixed (optional) owned-PVC target so the SDK's owned-PVC augmentation can
// be exercised without seeding a storage-foundation VolumeCaptureRequest.
type fakeProvider struct {
	ownedPVC *snapshotsdk.Target
}

func (f fakeProvider) VCRName(uid types.UID) string { return "vcr-" + string(uid) }
func (f fakeProvider) EnsureVCR(_ context.Context, _, _ string, _ metav1.OwnerReference, _ snapshotsdk.Target) error {
	return nil
}

func (f fakeProvider) OwnedPVCTarget(_ context.Context, _, _ string) (*snapshotsdk.Target, error) {
	return f.ownedPVC, nil
}

// captureAdapter is a minimal SnapshotAdapter over a corev1.ConfigMap "snapshot" object, with the
// domain-state and conditions held in-memory. EnsureChildren only needs Object(), the domain-state
// accessors, the conditions (commit marker), and (for the parent owner reference) the object's GVK — so a
// ConfigMap is sufficient and keeps the SDK test free of any domain type (boundary: the SDK never imports
// a domain package).
type captureAdapter struct {
	obj   *corev1.ConfigMap
	state snapshotsdk.DomainCaptureState
	conds []metav1.Condition
}

func (a *captureAdapter) Object() client.Object                                  { return a.obj }
func (a *captureAdapter) SourceRef() snapshotsdk.SourceRef                       { return snapshotsdk.SourceRef{} }
func (a *captureAdapter) GetConditions() []metav1.Condition                      { return a.conds }
func (a *captureAdapter) SetConditions(c []metav1.Condition)                     { a.conds = c }
func (a *captureAdapter) GetDomainCaptureState() snapshotsdk.DomainCaptureState  { return a.state }
func (a *captureAdapter) SetDomainCaptureState(s snapshotsdk.DomainCaptureState) { a.state = s }

// commitBarrier stamps ChildrenSnapshotReady=True, the durable marker that freezes the child topology.
func (a *captureAdapter) commitBarrier() *captureAdapter {
	a.conds = append(a.conds, metav1.Condition{
		Type:   storagev1alpha1.ConditionChildrenSnapshotReady,
		Status: metav1.ConditionTrue,
		Reason: storagev1alpha1.ReasonCompleted,
	})
	return a
}

func captureScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1: %v", err)
	}
	if err := ssv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add ssv1alpha1: %v", err)
	}
	return scheme
}

func childRef(name string) snapshotsdk.SnapshotChildRef {
	return snapshotsdk.SnapshotChildRef{APIVersion: "v1", Kind: "ConfigMap", Name: name}
}

func childSpec(name string) snapshotsdk.ChildSpec {
	return snapshotsdk.ChildSpec{Object: &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: captureNS}}}
}

func newCaptureSDK(t *testing.T, scheme *runtime.Scheme, seed ...client.Object) (snapshotsdk.CaptureSDK, client.Client) {
	t.Helper()
	return newCaptureSDKWithProvider(t, scheme, fakeProvider{}, seed...)
}

func newCaptureSDKWithProvider(t *testing.T, scheme *runtime.Scheme, provider snapshotsdk.VolumeCaptureProvider, seed ...client.Object) (snapshotsdk.CaptureSDK, client.Client) {
	t.Helper()
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1.ConfigMap{}).WithObjects(seed...).Build()
	return snapshotsdk.New(cl, provider), cl
}

func parentObj() *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: captureNS, UID: types.UID("parent-uid")}}
}

func parentAdapter(published ...snapshotsdk.SnapshotChildRef) *captureAdapter {
	return &captureAdapter{
		obj:   parentObj(),
		state: snapshotsdk.DomainCaptureState{ChildrenSnapshotRefs: published},
	}
}

func childExists(t *testing.T, cl client.Client, name string) bool {
	t.Helper()
	err := cl.Get(context.Background(), client.ObjectKey{Namespace: captureNS, Name: name}, &corev1.ConfigMap{})
	return err == nil
}

// TestEnsureChildrenFirstPlanningPublishes is the baseline: with the barrier not yet committed, the
// desired set is created and published.
func TestEnsureChildrenFirstPlanningPublishes(t *testing.T) {
	scheme := captureScheme(t)
	sdk, cl := newCaptureSDK(t, scheme, parentObj())
	adapter := parentAdapter()

	if err := sdk.EnsureChildren(context.Background(), adapter, []snapshotsdk.ChildSpec{childSpec("a"), childSpec("b")}); err != nil {
		t.Fatalf("first planning must succeed: %v", err)
	}
	if !childExists(t, cl, "a") || !childExists(t, cl, "b") {
		t.Fatal("expected children a and b created")
	}
	if !sameRefSet(adapter.state.ChildrenSnapshotRefs, childRef("a"), childRef("b")) {
		t.Fatalf("expected published [a,b], got %#v", adapter.state.ChildrenSnapshotRefs)
	}
}

// TestEnsureChildrenState2SameSetReEnsures: State 2 (published non-empty, barrier NOT committed). The same
// published set re-derived after a restart is not drift; a child missing from the cluster (e.g. crash
// before it was created) is (re)created, and the published refs stay put.
func TestEnsureChildrenState2SameSetReEnsures(t *testing.T) {
	scheme := captureScheme(t)
	// Only child "a" exists; "b" is missing.
	sdk, cl := newCaptureSDK(t, scheme, parentObj(),
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: captureNS}},
	)
	adapter := parentAdapter(childRef("a"), childRef("b")) // published [a,b], NOT committed

	if err := sdk.EnsureChildren(context.Background(), adapter, []snapshotsdk.ChildSpec{childSpec("b"), childSpec("a")}); err != nil {
		t.Fatalf("equal published set before the barrier must succeed: %v", err)
	}
	if !childExists(t, cl, "b") {
		t.Fatal("missing child b must be (re)created before the barrier")
	}
	if !sameRefSet(adapter.state.ChildrenSnapshotRefs, childRef("a"), childRef("b")) {
		t.Fatalf("published refs must stay [a,b], got %#v", adapter.state.ChildrenSnapshotRefs)
	}
}

// TestEnsureChildrenState2DriftCountChanged: State 2 (published [a,b], barrier NOT committed). A published
// artifact is immutable even before the barrier, so desired [a] is terminal drift — refs unchanged, nothing
// deleted. This closes the pre-commit silent-rewrite hole (publish-gated, not barrier-gated).
func TestEnsureChildrenState2DriftCountChanged(t *testing.T) {
	scheme := captureScheme(t)
	sdk, cl := newCaptureSDK(t, scheme, parentObj(),
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: captureNS}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: captureNS}},
	)
	adapter := parentAdapter(childRef("a"), childRef("b")) // published, NOT committed

	err := sdk.EnsureChildren(context.Background(), adapter, []snapshotsdk.ChildSpec{childSpec("a")})
	if !errors.Is(err, snapshotsdk.ErrTopologyDrift) {
		t.Fatalf("expected ErrTopologyDrift, got %v", err)
	}
	if !sameRefSet(adapter.state.ChildrenSnapshotRefs, childRef("a"), childRef("b")) {
		t.Fatalf("published refs must be left untouched [a,b], got %#v", adapter.state.ChildrenSnapshotRefs)
	}
	if !childExists(t, cl, "b") {
		t.Fatal("delete-free: detached child b must NOT be deleted")
	}
}

// TestEnsureChildrenState2DriftSameCountDifferentChild is the critical case a len()-only comparison would
// wrongly accept: published [a,b] (barrier NOT committed), desired [a,c] (same count) → drift; no c created.
func TestEnsureChildrenState2DriftSameCountDifferentChild(t *testing.T) {
	scheme := captureScheme(t)
	sdk, cl := newCaptureSDK(t, scheme, parentObj(),
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: captureNS}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: captureNS}},
	)
	adapter := parentAdapter(childRef("a"), childRef("b")) // published, NOT committed

	err := sdk.EnsureChildren(context.Background(), adapter, []snapshotsdk.ChildSpec{childSpec("a"), childSpec("c")})
	if !errors.Is(err, snapshotsdk.ErrTopologyDrift) {
		t.Fatalf("expected ErrTopologyDrift for same-count different-member set, got %v", err)
	}
	if !sameRefSet(adapter.state.ChildrenSnapshotRefs, childRef("a"), childRef("b")) {
		t.Fatalf("published refs must be left untouched [a,b], got %#v", adapter.state.ChildrenSnapshotRefs)
	}
	if childExists(t, cl, "c") {
		t.Fatal("fail-closed: drifted child c must NOT be created")
	}
}

// TestEnsureChildrenState1EmptyPublishedConverges: State 1 (nothing published yet, barrier NOT committed).
// An empty published set is not frozen — the desired set may still converge.
func TestEnsureChildrenState1EmptyPublishedConverges(t *testing.T) {
	scheme := captureScheme(t)
	sdk, cl := newCaptureSDK(t, scheme, parentObj())
	adapter := parentAdapter() // published empty, NOT committed

	if err := sdk.EnsureChildren(context.Background(), adapter, []snapshotsdk.ChildSpec{childSpec("a")}); err != nil {
		t.Fatalf("State 1 convergence must succeed, got %v", err)
	}
	if !childExists(t, cl, "a") || !sameRefSet(adapter.state.ChildrenSnapshotRefs, childRef("a")) {
		t.Fatalf("State 1 set must converge to [a], got %#v", adapter.state.ChildrenSnapshotRefs)
	}
}

// TestEnsureChildrenState3InertAfterBarrier: State 3 (barrier committed). The SDK is inert — a divergent
// desired set is neither created nor reported as drift; the published refs are untouched.
func TestEnsureChildrenState3InertAfterBarrier(t *testing.T) {
	scheme := captureScheme(t)
	sdk, cl := newCaptureSDK(t, scheme, parentObj(),
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: captureNS}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: captureNS}},
	)
	adapter := parentAdapter(childRef("a"), childRef("b")).commitBarrier()

	if err := sdk.EnsureChildren(context.Background(), adapter, []snapshotsdk.ChildSpec{childSpec("a"), childSpec("c")}); err != nil {
		t.Fatalf("committed snapshot must be inert (nil), got %v", err)
	}
	if childExists(t, cl, "c") {
		t.Fatal("inert: drifted child c must NOT be created after the barrier")
	}
	if !sameRefSet(adapter.state.ChildrenSnapshotRefs, childRef("a"), childRef("b")) {
		t.Fatalf("published refs must stay [a,b], got %#v", adapter.state.ChildrenSnapshotRefs)
	}
}

// TestEnsureChildrenState3InertEmptyLeafStaysEmpty: a committed leaf (published []) stays a leaf — a later
// desired [a] is ignored (inert), never grown.
func TestEnsureChildrenState3InertEmptyLeafStaysEmpty(t *testing.T) {
	scheme := captureScheme(t)
	sdk, cl := newCaptureSDK(t, scheme, parentObj())
	adapter := parentAdapter().commitBarrier() // committed with an empty child set

	if err := sdk.EnsureChildren(context.Background(), adapter, []snapshotsdk.ChildSpec{childSpec("a")}); err != nil {
		t.Fatalf("committed empty leaf must be inert (nil), got %v", err)
	}
	if len(adapter.state.ChildrenSnapshotRefs) != 0 {
		t.Fatalf("published refs must stay empty, got %#v", adapter.state.ChildrenSnapshotRefs)
	}
	if childExists(t, cl, "a") {
		t.Fatal("inert: child a must NOT be created on a committed empty leaf")
	}
}

// TestEnsureChildrenRejectsDuplicateDesired: a duplicate desired child is a planning bug, not topology
// drift — EnsureChildren fails with a non-drift error.
func TestEnsureChildrenRejectsDuplicateDesired(t *testing.T) {
	scheme := captureScheme(t)
	sdk, _ := newCaptureSDK(t, scheme, parentObj())
	adapter := parentAdapter()

	err := sdk.EnsureChildren(context.Background(), adapter, []snapshotsdk.ChildSpec{childSpec("a"), childSpec("a")})
	if err == nil {
		t.Fatal("expected error for duplicate desired child")
	}
	if errors.Is(err, snapshotsdk.ErrTopologyDrift) {
		t.Fatalf("duplicate desired child must NOT be reported as topology drift, got %v", err)
	}
}

// --- Manifest capture drift -----------------------------------------------------------------------------
//
// Manifest capture is fail-closed exactly like child snapshots and data capture: once a ManifestCaptureRequest is
// published, a later reconcile that derives a different target set is terminal ErrManifestDrift, not a
// silent re-create or self-heal. Comparison is by canonical (apiVersion, kind, name) identity, order- and
// duplicate-insensitive. These tests use a two-step create-then-re-call shape so the deterministic MCR name
// stays internal to the SDK (the SDK publishes it on the adapter's domain state).

func mt(apiVersion, kind, name string) snapshotsdk.ManifestTarget {
	return snapshotsdk.ManifestTarget{APIVersion: apiVersion, Kind: kind, Name: name}
}

func manifestAdapter() *captureAdapter { return &captureAdapter{obj: parentObj()} }

func mcrTargets(t *testing.T, cl client.Client, name string) []snapshotsdk.ManifestTarget {
	t.Helper()
	mcr := &ssv1alpha1.ManifestCaptureRequest{}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: captureNS, Name: name}, mcr); err != nil {
		t.Fatalf("get MCR %q: %v", name, err)
	}
	return mcr.Spec.Targets
}

func sameTargetSet(got []snapshotsdk.ManifestTarget, want ...snapshotsdk.ManifestTarget) bool {
	key := func(t snapshotsdk.ManifestTarget) string { return t.APIVersion + "|" + t.Kind + "|" + t.Name }
	seen := map[string]int{}
	for _, t := range got {
		seen[key(t)]++
	}
	for _, t := range want {
		if seen[key(t)] == 0 {
			return false
		}
		seen[key(t)]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

// TestEnsureManifestCaptureCreatePublishesTargets is the baseline create path: the MCR is created with the
// desired targets and its name is published on the domain state.
func TestEnsureManifestCaptureCreatePublishesTargets(t *testing.T) {
	scheme := captureScheme(t)
	sdk, cl := newCaptureSDK(t, scheme, parentObj())
	adapter := manifestAdapter()

	a, b := mt("v1", "DemoThing", "a"), mt("v1", "DemoThing", "b")
	if err := sdk.EnsureManifestCapture(context.Background(), adapter, snapshotsdk.ManifestCaptureSpec{Targets: []snapshotsdk.ManifestTarget{a, b}}); err != nil {
		t.Fatalf("create path must succeed: %v", err)
	}
	name := adapter.state.ManifestCaptureRequestName
	if name == "" {
		t.Fatal("expected published ManifestCaptureRequestName")
	}
	if !sameTargetSet(mcrTargets(t, cl, name), a, b) {
		t.Fatalf("MCR targets must be {a,b}, got %#v", mcrTargets(t, cl, name))
	}
}

// TestEnsureManifestCaptureRestartSameTargets: an existing MCR whose targets equal the desired set (only the
// order differs) is a no-op success — no drift, name still published.
func TestEnsureManifestCaptureRestartSameTargets(t *testing.T) {
	scheme := captureScheme(t)
	sdk, cl := newCaptureSDK(t, scheme, parentObj())
	adapter := manifestAdapter()

	a, b := mt("v1", "DemoThing", "a"), mt("v1", "DemoThing", "b")
	if err := sdk.EnsureManifestCapture(context.Background(), adapter, snapshotsdk.ManifestCaptureSpec{Targets: []snapshotsdk.ManifestTarget{a, b}}); err != nil {
		t.Fatalf("create path must succeed: %v", err)
	}
	name := adapter.state.ManifestCaptureRequestName

	if err := sdk.EnsureManifestCapture(context.Background(), adapter, snapshotsdk.ManifestCaptureSpec{Targets: []snapshotsdk.ManifestTarget{b, a}}); err != nil {
		t.Fatalf("same target set (reordered) after restart must be no-op success: %v", err)
	}
	if adapter.state.ManifestCaptureRequestName != name {
		t.Fatalf("published MCR name must be stable, got %q want %q", adapter.state.ManifestCaptureRequestName, name)
	}
	if !sameTargetSet(mcrTargets(t, cl, name), a, b) {
		t.Fatalf("MCR targets must stay {a,b}, got %#v", mcrTargets(t, cl, name))
	}
}

// TestEnsureManifestCaptureDriftCountChanged: existing {a,b}, desired {a} → ErrManifestDrift; existing MCR
// is left untouched.
func TestEnsureManifestCaptureDriftCountChanged(t *testing.T) {
	scheme := captureScheme(t)
	sdk, cl := newCaptureSDK(t, scheme, parentObj())
	adapter := manifestAdapter()

	a, b := mt("v1", "DemoThing", "a"), mt("v1", "DemoThing", "b")
	if err := sdk.EnsureManifestCapture(context.Background(), adapter, snapshotsdk.ManifestCaptureSpec{Targets: []snapshotsdk.ManifestTarget{a, b}}); err != nil {
		t.Fatalf("create path must succeed: %v", err)
	}
	name := adapter.state.ManifestCaptureRequestName

	err := sdk.EnsureManifestCapture(context.Background(), adapter, snapshotsdk.ManifestCaptureSpec{Targets: []snapshotsdk.ManifestTarget{a}})
	if !errors.Is(err, snapshotsdk.ErrManifestDrift) {
		t.Fatalf("expected ErrManifestDrift, got %v", err)
	}
	if !sameTargetSet(mcrTargets(t, cl, name), a, b) {
		t.Fatalf("existing MCR must be left untouched {a,b}, got %#v", mcrTargets(t, cl, name))
	}
}

// TestEnsureManifestCaptureDriftSameCountDifferentTarget is the critical case a len()-only check would
// wrongly accept: existing {a,b}, desired {a,c} (same count) → ErrManifestDrift; c is never added.
func TestEnsureManifestCaptureDriftSameCountDifferentTarget(t *testing.T) {
	scheme := captureScheme(t)
	sdk, cl := newCaptureSDK(t, scheme, parentObj())
	adapter := manifestAdapter()

	a, b, c := mt("v1", "DemoThing", "a"), mt("v1", "DemoThing", "b"), mt("v1", "DemoThing", "c")
	if err := sdk.EnsureManifestCapture(context.Background(), adapter, snapshotsdk.ManifestCaptureSpec{Targets: []snapshotsdk.ManifestTarget{a, b}}); err != nil {
		t.Fatalf("create path must succeed: %v", err)
	}
	name := adapter.state.ManifestCaptureRequestName

	err := sdk.EnsureManifestCapture(context.Background(), adapter, snapshotsdk.ManifestCaptureSpec{Targets: []snapshotsdk.ManifestTarget{a, c}})
	if !errors.Is(err, snapshotsdk.ErrManifestDrift) {
		t.Fatalf("expected ErrManifestDrift for same-count different-member set, got %v", err)
	}
	got := mcrTargets(t, cl, name)
	if !sameTargetSet(got, a, b) {
		t.Fatalf("existing MCR must stay {a,b}, got %#v", got)
	}
	for _, tg := range got {
		if tg.Name == "c" {
			t.Fatal("fail-closed: drifted target c must NOT be added to the MCR")
		}
	}
}

// TestEnsureManifestCaptureOwnedPVCAugmentationNoFalseDrift is the regression guard for point 6: the
// owned-PVC target derived from the data capture must be part of the DESIRED set before comparison. The MCR is
// created with {domain, owned-PVC}; a restart that re-derives only the domain target must re-augment the
// PVC and therefore NOT report false drift.
func TestEnsureManifestCaptureOwnedPVCAugmentationNoFalseDrift(t *testing.T) {
	scheme := captureScheme(t)
	pvc := &snapshotsdk.Target{APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "data-pvc", Namespace: captureNS}
	sdk, cl := newCaptureSDKWithProvider(t, scheme, fakeProvider{ownedPVC: pvc}, parentObj())
	adapter := manifestAdapter()
	adapter.state.VolumeCaptureRequestName = "vcr-x" // non-empty so the provider's owned-PVC target is consulted

	domain := mt("v1", "DemoThing", "a")
	if err := sdk.EnsureManifestCapture(context.Background(), adapter, snapshotsdk.ManifestCaptureSpec{Targets: []snapshotsdk.ManifestTarget{domain}}); err != nil {
		t.Fatalf("create path must succeed: %v", err)
	}
	name := adapter.state.ManifestCaptureRequestName
	if !sameTargetSet(mcrTargets(t, cl, name), domain, mt("v1", "PersistentVolumeClaim", "data-pvc")) {
		t.Fatalf("MCR must carry domain + owned-PVC targets, got %#v", mcrTargets(t, cl, name))
	}

	if err := sdk.EnsureManifestCapture(context.Background(), adapter, snapshotsdk.ManifestCaptureSpec{Targets: []snapshotsdk.ManifestTarget{domain}}); err != nil {
		t.Fatalf("owned-PVC augmentation must be part of desired (no false drift), got %v", err)
	}
}

func countMCRs(t *testing.T, cl client.Client) int {
	t.Helper()
	list := &ssv1alpha1.ManifestCaptureRequestList{}
	if err := cl.List(context.Background(), list, client.InNamespace(captureNS)); err != nil {
		t.Fatalf("list MCRs: %v", err)
	}
	return len(list.Items)
}

// TestEnsureManifestCaptureEmptyFailsClosed: empty domain targets and no owned-PVC → the final manifest set
// is empty, which is an SDK invariant violation. EnsureManifestCapture fails closed with ErrEmptyManifest
// before any cluster mutation: no MCR is created and no name is published.
func TestEnsureManifestCaptureEmptyFailsClosed(t *testing.T) {
	scheme := captureScheme(t)
	sdk, cl := newCaptureSDK(t, scheme, parentObj())
	adapter := manifestAdapter()

	err := sdk.EnsureManifestCapture(context.Background(), adapter, snapshotsdk.ManifestCaptureSpec{Targets: nil})
	if !errors.Is(err, snapshotsdk.ErrEmptyManifest) {
		t.Fatalf("empty manifest set must fail with ErrEmptyManifest, got %v", err)
	}
	if adapter.state.ManifestCaptureRequestName != "" {
		t.Fatalf("no MCR name must be published on empty manifest, got %q", adapter.state.ManifestCaptureRequestName)
	}
	if n := countMCRs(t, cl); n != 0 {
		t.Fatalf("no MCR must be created on empty manifest, found %d", n)
	}
}

// TestEnsureManifestCaptureEmptyInputRescuedByOwnedPVC: empty domain targets but a data-capture owned-PVC makes
// the FINAL set non-empty (augmentation happens before the cardinality check) → success with exactly that
// one target.
func TestEnsureManifestCaptureEmptyInputRescuedByOwnedPVC(t *testing.T) {
	scheme := captureScheme(t)
	pvc := &snapshotsdk.Target{APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "data-pvc", Namespace: captureNS}
	sdk, cl := newCaptureSDKWithProvider(t, scheme, fakeProvider{ownedPVC: pvc}, parentObj())
	adapter := manifestAdapter()
	adapter.state.VolumeCaptureRequestName = "vcr-x" // non-empty so the provider's owned-PVC target is consulted

	if err := sdk.EnsureManifestCapture(context.Background(), adapter, snapshotsdk.ManifestCaptureSpec{Targets: nil}); err != nil {
		t.Fatalf("owned-PVC augmentation must rescue an empty input set, got %v", err)
	}
	name := adapter.state.ManifestCaptureRequestName
	if name == "" {
		t.Fatal("expected published ManifestCaptureRequestName")
	}
	got := mcrTargets(t, cl, name)
	if len(got) != 1 || !sameTargetSet(got, mt("v1", "PersistentVolumeClaim", "data-pvc")) {
		t.Fatalf("MCR must carry exactly the owned-PVC target, got %#v", got)
	}
}

// TestEnsureManifestCaptureCommittedIsInert: State 3 (barrier committed) — EnsureManifestCapture is inert:
// it creates no MCR, publishes no name, and does not even reach the cardinality check.
func TestEnsureManifestCaptureCommittedIsInert(t *testing.T) {
	scheme := captureScheme(t)
	sdk, cl := newCaptureSDK(t, scheme, parentObj())
	adapter := manifestAdapter()
	adapter.commitBarrier()

	if err := sdk.EnsureManifestCapture(context.Background(), adapter, snapshotsdk.ManifestCaptureSpec{Targets: []snapshotsdk.ManifestTarget{mt("v1", "DemoThing", "a")}}); err != nil {
		t.Fatalf("committed snapshot must be inert (nil), got %v", err)
	}
	if n := countMCRs(t, cl); n != 0 {
		t.Fatalf("inert: no MCR must be created after the barrier, found %d", n)
	}
	if adapter.state.ManifestCaptureRequestName != "" {
		t.Fatalf("inert: no MCR name must be published after the barrier, got %q", adapter.state.ManifestCaptureRequestName)
	}
}

func sameRefSet(got []snapshotsdk.SnapshotChildRef, want ...snapshotsdk.SnapshotChildRef) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[snapshotsdk.SnapshotChildRef]int{}
	for _, r := range got {
		seen[r]++
	}
	for _, r := range want {
		if seen[r] == 0 {
			return false
		}
		seen[r]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}
