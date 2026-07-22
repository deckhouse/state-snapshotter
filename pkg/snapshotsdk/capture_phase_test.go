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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// The domain controllers call MarkPlanned on every reconcile before switching on the capture outcome
// (see demo virtualmachine/virtualdisk controllers). Without a guard this causes two phase write storms,
// both of which flap the mirrored Ready and starve the core binder's optimistic-lock Ready mirror:
//
//   - Finished<->Planned: once a leaf reaches Finished, an unguarded MarkPlanned regresses it to Planned,
//     then ConfirmConsistent re-Finishes it — killed by the forward-chain no-regress rule.
//   - Failed<->Planned: once a leaf Fails, an unguarded MarkPlanned resurrects it to Planned, then the
//     terminal outcome re-Fails it — killed by making Failed a TERMINAL SINK (it never resurrects).
//
// Because the domain watches its own object, either pair re-triggers the reconcile forever. These tests
// pin both rules of phaseCanAdvance.

func TestPhaseCanAdvance(t *testing.T) {
	const (
		none     = storagev1alpha1.SnapshotCapturePhase("")
		planning = storagev1alpha1.SnapshotCapturePhasePlanning
		planned  = storagev1alpha1.SnapshotCapturePhasePlanned
		finished = storagev1alpha1.SnapshotCapturePhaseFinished
		failed   = storagev1alpha1.SnapshotCapturePhaseFailed
	)
	cases := []struct {
		from, to storagev1alpha1.SnapshotCapturePhase
		want     bool
	}{
		{none, planning, true},
		{none, planned, true},
		{planning, planned, true},
		{planned, finished, true},
		{planned, planned, true},   // same phase (reason/message refresh) allowed
		{finished, finished, true}, // idempotent
		{finished, planned, false}, // the regression that causes the flap
		{finished, planning, false},
		{planned, planning, false},
		{planned, failed, true},   // late error may always surface
		{finished, failed, true},  // even after Finished
		{failed, failed, true},    // idempotent re-assert / terminal reason-message refresh
		{failed, planned, false},  // Failed is a terminal sink: never resurrects (kills the flap)
		{failed, finished, false}, // Failed is a terminal sink: never resurrects
		{failed, planning, false}, // Failed is a terminal sink: never resurrects
	}
	for _, c := range cases {
		if got := phaseCanAdvance(c.from, c.to); got != c.want {
			t.Errorf("phaseCanAdvance(%q, %q) = %v, want %v", c.from, c.to, got, c.want)
		}
	}
}

func TestSetPhase_MarkPlannedDoesNotRegressFinished(t *testing.T) {
	ctx := context.Background()
	scheme := newRefreshTestScheme(t)

	snap := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "snap-p", UID: types.UID("snap-p-uid")},
	}
	snap.SetGroupVersionKind(storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot"))

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&storagev1alpha1.Snapshot{}).
		WithObjects(snap).
		Build()

	s := New(cl, &countingReader{}, &fakeVolumeProvider{name: "vcr-p"})
	adapter := &refreshTestAdapter{obj: snap}

	if err := s.MarkPlanned(ctx, adapter); err != nil {
		t.Fatalf("MarkPlanned: %v", err)
	}
	if got := adapter.domain.Phase; got != storagev1alpha1.SnapshotCapturePhasePlanned {
		t.Fatalf("after MarkPlanned: phase = %q, want Planned", got)
	}

	if err := s.ConfirmConsistent(ctx, adapter); err != nil {
		t.Fatalf("ConfirmConsistent: %v", err)
	}
	if got := adapter.domain.Phase; got != storagev1alpha1.SnapshotCapturePhaseFinished {
		t.Fatalf("after ConfirmConsistent: phase = %q, want Finished", got)
	}

	// The domain controller unconditionally re-runs MarkPlanned on the next reconcile — it must be a no-op
	// once Finished, otherwise the phase flaps Planned<->Finished forever.
	for i := 0; i < 5; i++ {
		if err := s.MarkPlanned(ctx, adapter); err != nil {
			t.Fatalf("re-MarkPlanned[%d]: %v", i, err)
		}
		if got := adapter.domain.Phase; got != storagev1alpha1.SnapshotCapturePhaseFinished {
			t.Fatalf("re-MarkPlanned[%d]: phase regressed to %q, want Finished", i, got)
		}
	}
}

type statusBackedPhaseAdapter struct {
	obj *storagev1alpha1.Snapshot
}

func (a *statusBackedPhaseAdapter) Object() client.Object { return a.obj }
func (a *statusBackedPhaseAdapter) SourceRef() SourceRef  { return SourceRef{} }
func (a *statusBackedPhaseAdapter) GetDomainCaptureState() DomainCaptureState {
	st := DomainCaptureState{ChildrenSnapshotRefs: a.obj.Status.ChildrenSnapshotRefs}
	if a.obj.Status.CaptureState == nil || a.obj.Status.CaptureState.DomainSpecificController == nil {
		return st
	}
	domain := a.obj.Status.CaptureState.DomainSpecificController
	st.ManifestCaptureRequestName = domain.ManifestCaptureRequestName
	st.VolumeCaptureRequestName = domain.VolumeCaptureRequestName
	st.ExcludedRefs = domain.ExcludedRefs
	st.Phase = domain.Phase
	st.Reason = domain.Reason
	st.Message = domain.Message
	return st
}
func (a *statusBackedPhaseAdapter) SetDomainCaptureState(st DomainCaptureState) {
	a.obj.Status.ChildrenSnapshotRefs = st.ChildrenSnapshotRefs
	if a.obj.Status.CaptureState == nil {
		a.obj.Status.CaptureState = &storagev1alpha1.CaptureStateStatus{}
	}
	a.obj.Status.CaptureState.DomainSpecificController = &storagev1alpha1.DomainSpecificControllerCaptureState{
		ManifestCaptureRequestName: st.ManifestCaptureRequestName,
		VolumeCaptureRequestName:   st.VolumeCaptureRequestName,
		ExcludedRefs:               append([]ExcludedObjectRef{}, st.ExcludedRefs...),
		Phase:                      st.Phase,
		Reason:                     st.Reason,
		Message:                    st.Message,
	}
}
func (a *statusBackedPhaseAdapter) GetSnapshotSource() *SnapshotSource { return a.obj.Status.SourceRef }
func (a *statusBackedPhaseAdapter) SetSnapshotSource(src *SnapshotSource) {
	a.obj.Status.SourceRef = src
}
func (a *statusBackedPhaseAdapter) CoreCaptureState() CoreCaptureState  { return CoreCaptureState{} }
func (a *statusBackedPhaseAdapter) ReadyStatus() metav1.ConditionStatus { return "" }
func (a *statusBackedPhaseAdapter) ReadyReason() string                 { return "" }
func (a *statusBackedPhaseAdapter) ReadyMessage() string                { return "" }

type staleSnapshotReadClient struct {
	client.Client
	stale *storagev1alpha1.Snapshot
}

func (c *staleSnapshotReadClient) Get(
	ctx context.Context,
	key client.ObjectKey,
	obj client.Object,
	opts ...client.GetOption,
) error {
	snapshot, ok := obj.(*storagev1alpha1.Snapshot)
	if ok && key == client.ObjectKeyFromObject(c.stale) {
		c.stale.DeepCopyInto(snapshot)
		return nil
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

type phaseCountingReader struct {
	client.Reader
	gets int
}

func (r *phaseCountingReader) Get(
	ctx context.Context,
	key client.ObjectKey,
	obj client.Object,
	opts ...client.GetOption,
) error {
	r.gets++
	return r.Reader.Get(ctx, key, obj, opts...)
}

// EnsureChildren and MarkPlanned are consecutive status commits. If MarkPlanned bases its patch on an
// informer copy that predates EnsureChildren, the non-omitempty excludedRefs field is serialized as [] and
// silently replaces the veto set just published by EnsureChildren. The next reconcile then sees Planned
// with an empty frozen set and fails with ErrChildrenSetFrozen.
func TestMarkPlannedPreservesExcludedRefsAcrossCacheLag(t *testing.T) {
	ctx := context.Background()
	scheme := newRefreshTestScheme(t)

	root := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns1",
			Name:      "snap-cache-lag",
			UID:       types.UID("snap-cache-lag-uid"),
		},
	}
	root.SetGroupVersionKind(storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot"))

	apiServer := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&storagev1alpha1.Snapshot{}).
		WithObjects(root).
		Build()
	cachedClient := &staleSnapshotReadClient{Client: apiServer, stale: root.DeepCopy()}
	apiReader := &phaseCountingReader{Reader: apiServer}
	adapter := &statusBackedPhaseAdapter{obj: root.DeepCopy()}
	sdk := New(cachedClient, apiReader, &fakeVolumeProvider{name: "vcr"})
	excluded := []ExcludedObjectRef{{
		APIVersion: "demo.example.io/v1",
		Kind:       "DemoVirtualDisk",
		Name:       "disk-veto",
	}}

	if err := sdk.EnsureChildren(ctx, adapter, nil, excluded); err != nil {
		t.Fatalf("EnsureChildren: %v", err)
	}
	if err := sdk.MarkPlanned(ctx, adapter); err != nil {
		t.Fatalf("MarkPlanned: %v", err)
	}

	got := &storagev1alpha1.Snapshot{}
	if err := apiServer.Get(ctx, client.ObjectKeyFromObject(root), got); err != nil {
		t.Fatalf("get persisted snapshot: %v", err)
	}
	if got.Status.CaptureState == nil || got.Status.CaptureState.DomainSpecificController == nil {
		t.Fatal("persisted snapshot has no domain capture state")
	}
	domain := got.Status.CaptureState.DomainSpecificController
	if domain.Phase != storagev1alpha1.SnapshotCapturePhasePlanned {
		t.Fatalf("phase = %q, want Planned", domain.Phase)
	}
	if len(domain.ExcludedRefs) != 1 || domain.ExcludedRefs[0] != excluded[0] {
		t.Fatalf("excludedRefs = %#v, want %#v", domain.ExcludedRefs, excluded)
	}

	getsAfterTransition := apiReader.gets
	for i := 0; i < 3; i++ {
		if err := sdk.MarkPlanned(ctx, adapter); err != nil {
			t.Fatalf("idempotent MarkPlanned[%d]: %v", i, err)
		}
	}
	if apiReader.gets != getsAfterTransition {
		t.Fatalf("idempotent MarkPlanned used %d additional APIReader GETs, want zero", apiReader.gets-getsAfterTransition)
	}
}
