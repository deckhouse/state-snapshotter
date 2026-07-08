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

package snapshotsdk

import (
	"context"
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk/internal/children"
)

// These tests pin the planned-freeze guard on EnsureChildren (sdk-children-planned-freeze): once a node
// declares barrier 1 (phase>=Planned, or the terminal Failed) its declared child set is frozen, so a later
// EnsureChildren may re-publish the SAME set (idempotent no-op / ownerRef repair) but may NOT grow it or
// change the excluded set. The guard is fail-closed and side-effect-free — it rejects BEFORE children.Reconcile
// creates/adopts anything — because the immutable childrenSnapshotContentRefs CEL on the main side would
// otherwise wedge a freshly created child edge forever.

const (
	freezeNS   = "ns1"
	freezeRoot = "root"
	freezeUID  = types.UID("root-uid")
)

// freezeChildSpec builds a Snapshot child object (a scheme-registered client.Object) the SDK can derive a
// SnapshotChildRef from and create/adopt under the root.
func freezeChildSpec(name string) ChildSpec {
	c := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Namespace: freezeNS, Name: name}}
	c.SetGroupVersionKind(storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot"))
	return ChildSpec{Object: c}
}

// freezeChildRef is the SnapshotChildRef freezeChildSpec(name) publishes — the same (apiVersion, kind,
// name) derivation the guard and children.Reconcile perform.
func freezeChildRef(name string) SnapshotChildRef {
	return SnapshotChildRef{APIVersion: storagev1alpha1.SchemeGroupVersion.String(), Kind: "Snapshot", Name: name}
}

// newFreezeFixture wires a fake client (informer cache) holding the root Snapshot plus any already-adopted
// children, a refreshTestAdapter carrying the given published child refs + phase, and the SDK under test.
// Pre-created children carry the root's controller owner reference so an idempotent re-reconcile is a true
// no-op (children.Reconcile neither creates nor patches them).
func newFreezeFixture(t *testing.T, phase Phase, published []SnapshotChildRef, adopted ...string) (context.Context, client.Client, *refreshTestAdapter, CaptureSDK) {
	t.Helper()
	scheme := newRefreshTestScheme(t)
	root := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Namespace: freezeNS, Name: freezeRoot, UID: freezeUID}}
	root.SetGroupVersionKind(storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot"))

	controller := true
	ownerRef := metav1.OwnerReference{
		APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
		Kind:       "Snapshot",
		Name:       freezeRoot,
		UID:        freezeUID,
		Controller: &controller,
	}
	builder := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&storagev1alpha1.Snapshot{}).
		WithObjects(root)
	for _, name := range adopted {
		child := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Namespace: freezeNS, Name: name, OwnerReferences: []metav1.OwnerReference{ownerRef}}}
		child.SetGroupVersionKind(storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot"))
		builder = builder.WithObjects(child)
	}
	cl := builder.Build()
	adapter := &refreshTestAdapter{obj: root, domain: DomainCaptureState{Phase: phase, ChildrenSnapshotRefs: published}}
	return context.Background(), cl, adapter, New(cl, &countingReader{}, &fakeVolumeProvider{name: "vcr"})
}

func freezeChildExists(t *testing.T, ctx context.Context, cl client.Client, name string) bool {
	t.Helper()
	err := cl.Get(ctx, client.ObjectKey{Namespace: freezeNS, Name: name}, &storagev1alpha1.Snapshot{})
	switch {
	case err == nil:
		return true
	case apierrors.IsNotFound(err):
		return false
	default:
		t.Fatalf("get child %q: %v", name, err)
		return false
	}
}

// Pre-Planned: growth is the normal planning path — the child is created and the ref is unioned in.
func TestEnsureChildren_GrowthAllowedBeforePlanned(t *testing.T) {
	ctx, cl, adapter, sdk := newFreezeFixture(t, PhasePlanning, nil)

	if err := sdk.EnsureChildren(ctx, adapter, []ChildSpec{freezeChildSpec("child-a")}, nil); err != nil {
		t.Fatalf("growth before Planned must be allowed: %v", err)
	}
	if !freezeChildExists(t, ctx, cl, "child-a") {
		t.Fatal("child-a was not created")
	}
	if got := adapter.domain.ChildrenSnapshotRefs; !children.RefsEqualIgnoreOrder(got, []SnapshotChildRef{freezeChildRef("child-a")}) {
		t.Fatalf("published refs = %v, want [child-a]", got)
	}
}

// Post-Planned, same declared set: no error, no status write, no new create — idempotent across reconciles.
func TestEnsureChildren_SameSetAfterPlannedIsNoop(t *testing.T) {
	published := []SnapshotChildRef{freezeChildRef("child-a")}
	ctx, cl, adapter, sdk := newFreezeFixture(t, PhasePlanned, published, "child-a")

	for i := 0; i < 5; i++ {
		if err := sdk.EnsureChildren(ctx, adapter, []ChildSpec{freezeChildSpec("child-a")}, nil); err != nil {
			t.Fatalf("re-reconcile[%d] of the same set must be a no-op, got: %v", i, err)
		}
	}
	if !children.RefsEqualIgnoreOrder(adapter.domain.ChildrenSnapshotRefs, published) {
		t.Fatalf("published refs drifted to %v, want unchanged %v", adapter.domain.ChildrenSnapshotRefs, published)
	}
	if freezeChildExists(t, ctx, cl, "child-b") {
		t.Fatal("no-op re-reconcile must not create any new child")
	}
}

// Post-Planned growth is rejected fail-closed: typed error, published set untouched, and — crucially — the
// new child CR is NOT created (the reject happens before children.Reconcile).
func TestEnsureChildren_GrowthAfterPlannedRejectedNoSideEffects(t *testing.T) {
	published := []SnapshotChildRef{freezeChildRef("child-a")}
	ctx, cl, adapter, sdk := newFreezeFixture(t, PhasePlanned, published, "child-a")

	err := sdk.EnsureChildren(ctx, adapter, []ChildSpec{freezeChildSpec("child-a"), freezeChildSpec("child-b")}, nil)
	if !errors.Is(err, ErrChildrenSetFrozen) {
		t.Fatalf("growth after Planned must return ErrChildrenSetFrozen, got: %v", err)
	}
	if freezeChildExists(t, ctx, cl, "child-b") {
		t.Fatal("a rejected growth must NOT create the new child CR (fail-closed before Reconcile)")
	}
	if !children.RefsEqualIgnoreOrder(adapter.domain.ChildrenSnapshotRefs, published) {
		t.Fatalf("published refs must be untouched, got %v want %v", adapter.domain.ChildrenSnapshotRefs, published)
	}
}

// The freeze covers every frozen phase, not just Planned: Finished and the terminal Failed reject growth too.
func TestEnsureChildren_GrowthAtFinishedAndFailedRejected(t *testing.T) {
	for _, phase := range []Phase{PhaseFinished, PhaseFailed} {
		t.Run(string(phase), func(t *testing.T) {
			published := []SnapshotChildRef{freezeChildRef("child-a")}
			ctx, cl, adapter, sdk := newFreezeFixture(t, phase, published, "child-a")

			err := sdk.EnsureChildren(ctx, adapter, []ChildSpec{freezeChildSpec("child-a"), freezeChildSpec("child-b")}, nil)
			if !errors.Is(err, ErrChildrenSetFrozen) {
				t.Fatalf("growth at %s must return ErrChildrenSetFrozen, got: %v", phase, err)
			}
			if freezeChildExists(t, ctx, cl, "child-b") {
				t.Fatalf("growth at %s must not create the new child CR", phase)
			}
		})
	}
}

// A CHANGED excluded set is frozen too; an equal set is a no-op.
func TestEnsureChildren_ExcludedChangeAfterPlanned(t *testing.T) {
	published := []SnapshotChildRef{freezeChildRef("child-a")}
	excluded := []ExcludedObjectRef{{APIVersion: "v1", Kind: "ConfigMap", Name: "cm-x"}}
	ctx, _, adapter, sdk := newFreezeFixture(t, PhasePlanned, published, "child-a")
	adapter.domain.ExcludedRefs = excluded

	// Changing the excluded set after Planned is rejected.
	err := sdk.EnsureChildren(ctx, adapter, []ChildSpec{freezeChildSpec("child-a")}, []ExcludedObjectRef{{APIVersion: "v1", Kind: "ConfigMap", Name: "cm-y"}})
	if !errors.Is(err, ErrChildrenSetFrozen) {
		t.Fatalf("changing the excluded set after Planned must return ErrChildrenSetFrozen, got: %v", err)
	}
	if !excludedRefsEqualIgnoreOrder(adapter.domain.ExcludedRefs, excluded) {
		t.Fatalf("excluded refs must be untouched, got %v want %v", adapter.domain.ExcludedRefs, excluded)
	}

	// Re-publishing the SAME excluded set is a harmless no-op.
	if err := sdk.EnsureChildren(ctx, adapter, []ChildSpec{freezeChildSpec("child-a")}, excluded); err != nil {
		t.Fatalf("re-publishing the same excluded set must be a no-op, got: %v", err)
	}
}

// childrenSetFrozen is the pure freeze predicate shared by the pre-check and the in-closure TOCTOU belt;
// this table test pins both (the belt's frozen-and-grown branch is otherwise not deterministically reachable).
func TestChildrenSetFrozen(t *testing.T) {
	cases := []struct {
		phase Phase
		want  bool
	}{
		{"", false},
		{PhasePlanning, false},
		{PhasePlanned, true},
		{PhaseFinished, true},
		{PhaseFailed, true},
	}
	for _, c := range cases {
		if got := childrenSetFrozen(c.phase); got != c.want {
			t.Errorf("childrenSetFrozen(%q) = %v, want %v", c.phase, got, c.want)
		}
	}
}

// freezeRaceAdapter reports a different phase on successive GetDomainCaptureState calls, so the phase can
// advance into a frozen phase in the window BETWEEN the pre-check's authoritative read and patch.Status's
// retry re-read — the only way to exercise the in-closure belt deterministically.
type freezeRaceAdapter struct {
	*refreshTestAdapter
	phases []Phase
	calls  int
}

func (a *freezeRaceAdapter) GetDomainCaptureState() DomainCaptureState {
	st := a.refreshTestAdapter.GetDomainCaptureState()
	if a.calls < len(a.phases) {
		st.Phase = a.phases[a.calls]
	}
	a.calls++
	return st
}

// TOCTOU belt: pre-check observes Planning (growth allowed, so Reconcile creates the new child), but the
// node advances to Planned before the status patch. The closure cannot return an error, so it fail-closed
// DROPS the frozen growth — the published set stays frozen even though the racing child CR now exists.
func TestEnsureChildren_FrozenBeltDropsRacyGrowth(t *testing.T) {
	published := []SnapshotChildRef{freezeChildRef("child-a")}
	ctx, cl, base, sdk := newFreezeFixture(t, PhasePlanning, published, "child-a")
	adapter := &freezeRaceAdapter{refreshTestAdapter: base, phases: []Phase{PhasePlanning, PhasePlanned}}

	if err := sdk.EnsureChildren(ctx, adapter, []ChildSpec{freezeChildSpec("child-a"), freezeChildSpec("child-b")}, nil); err != nil {
		t.Fatalf("belt must silently drop the racy write, not error: %v", err)
	}
	if !children.RefsEqualIgnoreOrder(base.domain.ChildrenSnapshotRefs, published) {
		t.Fatalf("belt must not publish the racy growth, got %v want frozen %v", base.domain.ChildrenSnapshotRefs, published)
	}
	if !freezeChildExists(t, ctx, cl, "child-b") {
		t.Fatal("Reconcile ran before the freeze took effect, so the racy child is expected to exist (belt guards the publish, not the create)")
	}
}
