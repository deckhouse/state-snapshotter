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

package snapshotcontent

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

func residualTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return s
}

// TestMarkResidualVolumeCaptureCompleteSetsCompleteWhenAbsent: an absent residualVolumeCapture is
// latched to Complete, recording the captured orphan UIDs and a CompletedAt stamp.
func TestMarkResidualVolumeCaptureCompleteSetsCompleteWhenAbsent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "root"}}
	cl := fake.NewClientBuilder().WithScheme(residualTestScheme(t)).WithObjects(content).
		WithStatusSubresource(&storagev1alpha1.SnapshotContent{}).Build()

	if err := MarkResidualVolumeCaptureComplete(ctx, cl, "root", []string{"uid-a", "uid-b"}); err != nil {
		t.Fatalf("mark complete: %v", err)
	}

	got := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: "root"}, got); err != nil {
		t.Fatalf("get content: %v", err)
	}
	rvc := got.Status.ResidualVolumeCapture
	if rvc == nil {
		t.Fatal("residualVolumeCapture must be set")
	}
	if rvc.Phase != storagev1alpha1.ResidualVolumeCapturePhaseComplete {
		t.Fatalf("want phase=%q, got %q", storagev1alpha1.ResidualVolumeCapturePhaseComplete, rvc.Phase)
	}
	if rvc.CompletedAt == nil || rvc.CompletedAt.IsZero() {
		t.Fatalf("want CompletedAt stamped, got %#v", rvc.CompletedAt)
	}
	if len(rvc.TargetUIDs) != 2 || rvc.TargetUIDs[0] != "uid-a" || rvc.TargetUIDs[1] != "uid-b" {
		t.Fatalf("want TargetUIDs [uid-a uid-b], got %#v", rvc.TargetUIDs)
	}
}

// TestMarkResidualVolumeCaptureCompleteZeroTargets: a zero-targets capture root latches Complete with an
// empty TargetUIDs (no orphans), still satisfying the gate.
func TestMarkResidualVolumeCaptureCompleteZeroTargets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "root"}}
	cl := fake.NewClientBuilder().WithScheme(residualTestScheme(t)).WithObjects(content).
		WithStatusSubresource(&storagev1alpha1.SnapshotContent{}).Build()

	if err := MarkResidualVolumeCaptureComplete(ctx, cl, "root", nil); err != nil {
		t.Fatalf("mark complete: %v", err)
	}
	got := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: "root"}, got); err != nil {
		t.Fatalf("get content: %v", err)
	}
	if got.Status.ResidualVolumeCapture == nil ||
		got.Status.ResidualVolumeCapture.Phase != storagev1alpha1.ResidualVolumeCapturePhaseComplete {
		t.Fatalf("zero-targets must still latch Complete, got %#v", got.Status.ResidualVolumeCapture)
	}
	if len(got.Status.ResidualVolumeCapture.TargetUIDs) != 0 {
		t.Fatalf("zero-targets must have empty TargetUIDs, got %#v", got.Status.ResidualVolumeCapture.TargetUIDs)
	}
}

// TestMarkResidualVolumeCaptureCompleteIdempotentMonotonic: re-stamping an already-Complete latch is a
// no-op (no write, stable resourceVersion) and must NOT overwrite the recorded CompletedAt/TargetUIDs,
// proving the latch is monotonic.
func TestMarkResidualVolumeCaptureCompleteIdempotentMonotonic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "root"}}
	cl := fake.NewClientBuilder().WithScheme(residualTestScheme(t)).WithObjects(content).
		WithStatusSubresource(&storagev1alpha1.SnapshotContent{}).Build()

	if err := MarkResidualVolumeCaptureComplete(ctx, cl, "root", []string{"uid-a"}); err != nil {
		t.Fatalf("first mark: %v", err)
	}
	first := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: "root"}, first); err != nil {
		t.Fatalf("get after first: %v", err)
	}
	firstRV := first.ResourceVersion
	firstCompletedAt := first.Status.ResidualVolumeCapture.CompletedAt.DeepCopy()

	// Second call with DIFFERENT targets must not rewrite the latch.
	if err := MarkResidualVolumeCaptureComplete(ctx, cl, "root", []string{"uid-b", "uid-c"}); err != nil {
		t.Fatalf("second mark: %v", err)
	}
	second := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: "root"}, second); err != nil {
		t.Fatalf("get after second: %v", err)
	}
	if second.ResourceVersion != firstRV {
		t.Fatalf("idempotent latch must not re-write status (rv %s -> %s)", firstRV, second.ResourceVersion)
	}
	rvc := second.Status.ResidualVolumeCapture
	if !rvc.CompletedAt.Equal(firstCompletedAt) {
		t.Fatalf("CompletedAt must be preserved (monotonic), got %#v want %#v", rvc.CompletedAt, firstCompletedAt)
	}
	if len(rvc.TargetUIDs) != 1 || rvc.TargetUIDs[0] != "uid-a" {
		t.Fatalf("TargetUIDs must be preserved (monotonic), got %#v", rvc.TargetUIDs)
	}
}

// TestMarkResidualVolumeCaptureCompleteEmptyNameNoop: an empty contentName is a safe no-op.
func TestMarkResidualVolumeCaptureCompleteEmptyNameNoop(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cl := fake.NewClientBuilder().WithScheme(residualTestScheme(t)).
		WithStatusSubresource(&storagev1alpha1.SnapshotContent{}).Build()
	if err := MarkResidualVolumeCaptureComplete(ctx, cl, "", []string{"x"}); err != nil {
		t.Fatalf("empty name must be a no-op, got %v", err)
	}
}

// TestMarkResidualVolumeCaptureCompletePreservesConditions: the latch is a MergeFrom status patch, so it
// must NOT clobber the conditions the aggregator owns (reconciler -> aggregator write direction).
func TestMarkResidualVolumeCaptureCompletePreservesConditions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	content := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "root"},
		Status: storagev1alpha1.SnapshotContentStatus{
			Conditions: []metav1.Condition{{
				Type:               storagev1alpha1.ConditionReady,
				Status:             metav1.ConditionFalse,
				Reason:             storagev1alpha1.ReasonManifestsCapturing,
				Message:            "capturing",
				LastTransitionTime: metav1.Now(),
			}},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(residualTestScheme(t)).WithObjects(content).
		WithStatusSubresource(&storagev1alpha1.SnapshotContent{}).Build()

	if err := MarkResidualVolumeCaptureComplete(ctx, cl, "root", nil); err != nil {
		t.Fatalf("mark complete: %v", err)
	}
	got := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: "root"}, got); err != nil {
		t.Fatalf("get content: %v", err)
	}
	if got.Status.ResidualVolumeCapture == nil ||
		got.Status.ResidualVolumeCapture.Phase != storagev1alpha1.ResidualVolumeCapturePhaseComplete {
		t.Fatalf("latch must be set, got %#v", got.Status.ResidualVolumeCapture)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, storagev1alpha1.ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != storagev1alpha1.ReasonManifestsCapturing {
		t.Fatalf("aggregator-owned conditions must be preserved by the latch patch, got %#v", cond)
	}
}

// TestMarkResidualVolumeCaptureCompleteSurvivesAggregatorStatusUpdate: a subsequent full Status().Update
// by the aggregator (which reads fresh, sets conditions, then updates) must preserve the latch field
// (aggregator -> reconciler coexistence; no writer loses the other's subtree).
func TestMarkResidualVolumeCaptureCompleteSurvivesAggregatorStatusUpdate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "root"}}
	cl := fake.NewClientBuilder().WithScheme(residualTestScheme(t)).WithObjects(content).
		WithStatusSubresource(&storagev1alpha1.SnapshotContent{}).Build()

	// Reconciler latches Complete first.
	if err := MarkResidualVolumeCaptureComplete(ctx, cl, "root", []string{"uid-a"}); err != nil {
		t.Fatalf("mark complete: %v", err)
	}

	// Aggregator does a fresh read, sets a condition, and writes the whole status back.
	fresh := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: "root"}, fresh); err != nil {
		t.Fatalf("aggregator get: %v", err)
	}
	meta.SetStatusCondition(&fresh.Status.Conditions, metav1.Condition{
		Type: storagev1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: storagev1alpha1.ReasonCompleted, Message: "ready",
	})
	if err := cl.Status().Update(ctx, fresh); err != nil {
		t.Fatalf("aggregator status update: %v", err)
	}

	got := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: "root"}, got); err != nil {
		t.Fatalf("get content: %v", err)
	}
	if got.Status.ResidualVolumeCapture == nil ||
		got.Status.ResidualVolumeCapture.Phase != storagev1alpha1.ResidualVolumeCapturePhaseComplete {
		t.Fatalf("latch must survive the aggregator's full Status().Update, got %#v", got.Status.ResidualVolumeCapture)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, storagev1alpha1.ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("aggregator Ready=True must coexist with the latch, got %#v", cond)
	}
}
