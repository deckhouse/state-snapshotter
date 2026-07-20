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

func newDomainStatusFixture(t *testing.T) (context.Context, CaptureSDK, *refreshTestAdapter) {
	t.Helper()
	ctx := context.Background()
	scheme := newRefreshTestScheme(t)
	snap := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "snap-dcs", UID: types.UID("snap-dcs-uid")},
	}
	snap.SetGroupVersionKind(storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot"))
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&storagev1alpha1.Snapshot{}).
		WithObjects(snap).
		Build()
	s := New(cl, &countingReader{}, &fakeVolumeProvider{name: "vcr-dcs"})
	adapter := &refreshTestAdapter{obj: snap}
	return ctx, s, adapter
}

func TestDomainCaptureStatus_PlanningReasonMessage(t *testing.T) {
	ctx, s, adapter := newDomainStatusFixture(t)

	if err := s.DomainCaptureStatus(adapter).
		Phase(PhasePlanning).
		Reason("Snapshotting").
		Message("Waiting for PVC").
		Apply(ctx); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got := adapter.GetDomainCaptureState()
	if got.Phase != PhasePlanning || got.Reason != "Snapshotting" || got.Message != "Waiting for PVC" {
		t.Fatalf("got %+v, want Planning/Snapshotting/Waiting for PVC", got)
	}

	// Other domain fields must survive a triple-only write.
	adapter.domain.ManifestCaptureRequestName = "mcr-keep"
	adapter.domain.VolumeCaptureRequestName = "vcr-keep"
	if err := s.DomainCaptureStatus(adapter).
		Phase(PhasePlanning).
		Reason("Snapshotting").
		Message("Waiting for PVC").
		Apply(ctx); err != nil {
		t.Fatalf("re-Apply: %v", err)
	}
	got = adapter.GetDomainCaptureState()
	if got.ManifestCaptureRequestName != "mcr-keep" || got.VolumeCaptureRequestName != "vcr-keep" {
		t.Fatalf("Apply cleared unrelated fields: %+v", got)
	}
}

func TestDomainCaptureStatus_WaitingMessageOnly(t *testing.T) {
	ctx, s, adapter := newDomainStatusFixture(t)

	if err := s.DomainCaptureStatus(adapter).
		Phase(PhasePlanning).
		Message("waiting for source").
		Apply(ctx); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := adapter.GetDomainCaptureState()
	if got.Phase != PhasePlanning || got.Message != "waiting for source" || got.Reason != "" {
		t.Fatalf("got %+v, want Planning / waiting for source / empty reason", got)
	}
}

func TestDomainCaptureStatus_PlannedClearsReasonMessage(t *testing.T) {
	ctx, s, adapter := newDomainStatusFixture(t)
	adapter.domain = DomainCaptureState{
		Phase:   PhasePlanning,
		Reason:  "Snapshotting",
		Message: "Waiting",
	}

	if err := s.DomainCaptureStatus(adapter).
		Phase(PhasePlanned).
		Reason("should-be-cleared").
		Message("should-be-cleared").
		Apply(ctx); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := adapter.GetDomainCaptureState()
	if got.Phase != PhasePlanned || got.Reason != "" || got.Message != "" {
		t.Fatalf("got %+v, want Planned with empty reason/message", got)
	}
}

func TestDomainCaptureStatus_NoPhaseRegression(t *testing.T) {
	ctx, s, adapter := newDomainStatusFixture(t)
	if err := s.DomainCaptureStatus(adapter).Phase(PhasePlanned).Apply(ctx); err != nil {
		t.Fatalf("Planned: %v", err)
	}
	if err := s.DomainCaptureStatus(adapter).Phase(PhaseFinished).Apply(ctx); err != nil {
		t.Fatalf("Finished: %v", err)
	}

	if err := s.DomainCaptureStatus(adapter).
		Phase(PhasePlanning).
		Reason("Snapshotting").
		Message("late progress").
		Apply(ctx); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := adapter.GetDomainCaptureState().Phase; got != PhaseFinished {
		t.Fatalf("phase regressed to %q, want Finished (setPhase no-op)", got)
	}
}

func TestDomainCaptureStatus_FailedSink(t *testing.T) {
	ctx, s, adapter := newDomainStatusFixture(t)
	if err := s.DomainCaptureStatus(adapter).
		Phase(PhaseFailed).
		Reason("PotentiallyInconsistent").
		Message("cannot freeze").
		Apply(ctx); err != nil {
		t.Fatalf("Apply Failed: %v", err)
	}
	if err := s.DomainCaptureStatus(adapter).
		Phase(PhasePlanned).
		Apply(ctx); err != nil {
		t.Fatalf("Apply Planned after Failed: %v", err)
	}
	got := adapter.GetDomainCaptureState()
	if got.Phase != PhaseFailed || got.Reason != "PotentiallyInconsistent" {
		t.Fatalf("Failed sink broken: %+v", got)
	}
}

func TestDomainCaptureStatus_Idempotent(t *testing.T) {
	ctx, s, adapter := newDomainStatusFixture(t)
	write := func() {
		t.Helper()
		if err := s.DomainCaptureStatus(adapter).
			Phase(PhasePlanning).
			Reason("Snapshotting").
			Message("Waiting").
			Apply(ctx); err != nil {
			t.Fatalf("Apply: %v", err)
		}
	}
	write()
	write()
	got := adapter.GetDomainCaptureState()
	if got.Phase != PhasePlanning || got.Reason != "Snapshotting" || got.Message != "Waiting" {
		t.Fatalf("idempotent Apply drifted: %+v", got)
	}
}

func TestDomainCaptureStatus_PlannedSamePhaseMessageDiagnostic(t *testing.T) {
	ctx, s, adapter := newDomainStatusFixture(t)
	if err := s.DomainCaptureStatus(adapter).Phase(PhasePlanned).Apply(ctx); err != nil {
		t.Fatalf("Planned: %v", err)
	}
	if err := s.DomainCaptureStatus(adapter).
		Phase(PhasePlanned).
		Message("namespace plan incomplete").
		Apply(ctx); err != nil {
		t.Fatalf("diagnostic: %v", err)
	}
	got := adapter.GetDomainCaptureState()
	if got.Phase != PhasePlanned || got.Message != "namespace plan incomplete" || got.Reason != "" {
		t.Fatalf("got %+v, want Planned + diagnostic message", got)
	}
	if err := s.DomainCaptureStatus(adapter).
		Phase(PhasePlanned).
		Message("").
		Apply(ctx); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got = adapter.GetDomainCaptureState()
	if got.Phase != PhasePlanned || got.Message != "" {
		t.Fatalf("got %+v, want Planned with cleared message", got)
	}
}

func TestDomainCaptureStatus_PlanningAfterFailedIsNoop(t *testing.T) {
	ctx, s, adapter := newDomainStatusFixture(t)
	if err := s.DomainCaptureStatus(adapter).
		Phase(PhaseFailed).
		Reason("Boom").
		Apply(ctx); err != nil {
		t.Fatalf("Failed: %v", err)
	}
	if err := s.DomainCaptureStatus(adapter).
		Phase(PhasePlanning).
		Message("should not stick").
		Apply(ctx); err != nil {
		t.Fatalf("Planning after Failed: %v", err)
	}
	got := adapter.GetDomainCaptureState()
	if got.Phase != PhaseFailed || got.Message == "should not stick" {
		t.Fatalf("Planning touched Failed: %+v", got)
	}
}
