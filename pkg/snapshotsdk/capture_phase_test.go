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

// The domain controllers call MarkPlanned on every reconcile before switching on the capture outcome
// (see demo virtualmachine/virtualdisk controllers). Once a leaf reaches Finished, an unguarded
// MarkPlanned would regress it back to Planned, so every reconcile would emit two status writes
// (Planned then Finished). Because the domain watches its own object, that pair re-triggers the reconcile
// — a self-sustaining phase write storm that starves the core binder's optimistic-lock Ready mirror and
// wedges the tree at Ready=False/ContentMissing. These tests pin the monotonic guard that kills the storm.

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
		{planned, failed, true},  // late error may always surface
		{finished, failed, true}, // even after Finished
		{failed, planned, true},  // recovery preserved (pre-guard behavior)
		{failed, finished, true}, // recovery preserved
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
