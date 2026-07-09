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
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// inspectChildSnapshot builds a child snapshot (typed core Snapshot, standing in for any domain child kind
// — see subtreeChildGV) with an optional Ready condition and optional core-owned leg latches. status "" =
// no Ready condition; nil latch = leg not declared.
func inspectChildSnapshot(name string, status metav1.ConditionStatus, reason string, manifest, data *bool) *storagev1alpha1.Snapshot {
	s := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: name},
	}
	s.SetGroupVersionKind(storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot"))
	if status != "" {
		s.Status.Conditions = []metav1.Condition{{
			Type:               storagev1alpha1.ConditionReady,
			Status:             status,
			Reason:             reason,
			Message:            reason + " detail",
			LastTransitionTime: metav1.Now(),
		}}
	}
	if manifest != nil || data != nil {
		s.Status.CaptureState = &storagev1alpha1.CaptureStateStatus{
			CommonController: &storagev1alpha1.CommonControllerCaptureState{
				ManifestCaptured: manifest,
				DataCaptured:     data,
			},
		}
	}
	return s
}

// TestChildrenCaptureStates_ReadsReadyAndLatches verifies the SDK resolves every declared child ref and
// reports its Ready condition (status/reason/message) plus whether all its declared legs are latched.
func TestChildrenCaptureStates_ReadsReadyAndLatches(t *testing.T) {
	scheme := newRefreshTestScheme(t)
	captured := refreshBoolPtr(true)
	notYet := refreshBoolPtr(false)
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(
			inspectChildSnapshot("child-a", metav1.ConditionTrue, storagev1alpha1.ReasonCompleted, captured, captured),
			inspectChildSnapshot("child-b", metav1.ConditionFalse, "DataCapturePending", captured, notYet),
		).Build()
	s := &sdk{client: cl}
	a := &refreshTestAdapter{
		obj: &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "root"}},
		domain: DomainCaptureState{ChildrenSnapshotRefs: []SnapshotChildRef{
			childRef("child-a"), childRef("child-b"),
		}},
	}

	states, err := s.ChildrenCaptureStates(context.Background(), a)
	if err != nil {
		t.Fatalf("ChildrenCaptureStates: %v", err)
	}
	if len(states) != 2 {
		t.Fatalf("want 2 states, got %d", len(states))
	}

	if states[0].Ref.Name != "child-a" {
		t.Fatalf("state[0] ref = %q, want child-a", states[0].Ref.Name)
	}
	if states[0].ReadyStatus != metav1.ConditionTrue || states[0].ReadyReason != storagev1alpha1.ReasonCompleted {
		t.Fatalf("child-a ready = %q/%q, want True/Completed", states[0].ReadyStatus, states[0].ReadyReason)
	}
	if !states[0].AllLegsCaptured {
		t.Fatalf("child-a AllLegsCaptured = false, want true")
	}

	if states[1].ReadyStatus != metav1.ConditionFalse || states[1].ReadyReason != "DataCapturePending" {
		t.Fatalf("child-b ready = %q/%q, want False/DataCapturePending", states[1].ReadyStatus, states[1].ReadyReason)
	}
	if states[1].AllLegsCaptured {
		t.Fatalf("child-b AllLegsCaptured = true, want false (data leg not latched)")
	}
}

// TestChildrenCaptureStates_MissingChildIsPending verifies a not-yet-materialized child ref is reported as
// still-pending (empty Ready, AllLegsCaptured=false) instead of erroring — the caller keeps waiting.
func TestChildrenCaptureStates_MissingChildIsPending(t *testing.T) {
	scheme := newRefreshTestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	s := &sdk{client: cl}
	a := &refreshTestAdapter{
		obj:    &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "root"}},
		domain: DomainCaptureState{ChildrenSnapshotRefs: []SnapshotChildRef{childRef("ghost")}},
	}

	states, err := s.ChildrenCaptureStates(context.Background(), a)
	if err != nil {
		t.Fatalf("ChildrenCaptureStates: %v", err)
	}
	if len(states) != 1 {
		t.Fatalf("want 1 state, got %d", len(states))
	}
	if states[0].ReadyStatus != "" || states[0].AllLegsCaptured {
		t.Fatalf("missing child = %q/%v, want empty/false", states[0].ReadyStatus, states[0].AllLegsCaptured)
	}
}

// TestChildrenCaptureStates_TerminalChildReason verifies a terminally failed child surfaces its terminal
// Ready reason so an aggregator can stop waiting (the core owns and bubbles the terminal).
func TestChildrenCaptureStates_TerminalChildReason(t *testing.T) {
	scheme := newRefreshTestScheme(t)
	captured := refreshBoolPtr(true)
	notYet := refreshBoolPtr(false)
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(inspectChildSnapshot("child-fail", metav1.ConditionFalse, "VolumeCaptureFailed", captured, notYet)).
		Build()
	s := &sdk{client: cl}
	a := &refreshTestAdapter{
		obj:    &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "root"}},
		domain: DomainCaptureState{ChildrenSnapshotRefs: []SnapshotChildRef{childRef("child-fail")}},
	}

	states, err := s.ChildrenCaptureStates(context.Background(), a)
	if err != nil {
		t.Fatalf("ChildrenCaptureStates: %v", err)
	}
	if len(states) != 1 {
		t.Fatalf("want 1 state, got %d", len(states))
	}
	if !storagev1alpha1.IsReasonTerminal(states[0].ReadyReason) {
		t.Fatalf("child-fail reason %q not terminal, want terminal (VolumeCaptureFailed)", states[0].ReadyReason)
	}
	if states[0].AllLegsCaptured {
		t.Fatalf("child-fail AllLegsCaptured = true, want false")
	}
}
