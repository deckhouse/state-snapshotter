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
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// Domains re-assert phase=Planned on every reconcile before switching on the capture outcome.
// Without a guard this causes two phase write storms, both of which flap the mirrored Ready and starve
// the core binder's optimistic-lock Ready mirror:
//
//   - Finished<->Planned: once a leaf reaches Finished, an unguarded Planned re-assert regresses it to
//     Planned, then Finished re-Finishes it — killed by the forward-chain no-regress rule.
//   - Failed<->Planned: once a leaf Fails, an unguarded Planned re-assert resurrects it to Planned, then
//     the terminal outcome re-Fails it — killed by making Failed a TERMINAL SINK (it never resurrects).
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

func TestSetPhase_PlannedDoesNotRegressFinished(t *testing.T) {
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

	if err := s.DomainCaptureStatus(adapter).Phase(PhasePlanned).Apply(ctx); err != nil {
		t.Fatalf("Planned: %v", err)
	}
	if got := adapter.domain.Phase; got != storagev1alpha1.SnapshotCapturePhasePlanned {
		t.Fatalf("after Planned: phase = %q, want Planned", got)
	}

	if err := s.DomainCaptureStatus(adapter).Phase(PhaseFinished).Apply(ctx); err != nil {
		t.Fatalf("Finished: %v", err)
	}
	if got := adapter.domain.Phase; got != storagev1alpha1.SnapshotCapturePhaseFinished {
		t.Fatalf("after Finished: phase = %q, want Finished", got)
	}

	// The domain controller unconditionally re-asserts Planned on the next reconcile — it must be a no-op
	// once Finished, otherwise the phase flaps Planned<->Finished forever.
	for i := 0; i < 5; i++ {
		if err := s.DomainCaptureStatus(adapter).Phase(PhasePlanned).Apply(ctx); err != nil {
			t.Fatalf("re-Planned[%d]: %v", i, err)
		}
		if got := adapter.domain.Phase; got != storagev1alpha1.SnapshotCapturePhaseFinished {
			t.Fatalf("re-Planned[%d]: phase regressed to %q, want Finished", i, got)
		}
	}
}
