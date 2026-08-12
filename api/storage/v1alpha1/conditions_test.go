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

package v1alpha1

import "testing"

func TestIsReasonDegraded(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		want   bool
	}{
		{name: "recoverable degradation", reason: ReasonChildSnapshotDeleted, want: true},

		// Terminal reasons are NOT degraded (disjoint from DegradedReadyReasons).
		{name: "terminal ChildSnapshotLost", reason: ReasonChildSnapshotLost, want: false},
		{name: "terminal ListFailed", reason: "ListFailed", want: false},
		{name: "terminal VolumeCaptureFailed", reason: "VolumeCaptureFailed", want: false},
		{name: "terminal GraphPlanningFailed", reason: ReasonGraphPlanningFailed, want: false},

		// In-progress (non-terminal, non-degraded) reasons are NOT degraded.
		{name: "in-progress DataCapturePending", reason: "DataCapturePending", want: false},
		{name: "in-progress ChildrenPending", reason: "ChildrenPending", want: false},
		{name: "in-progress ImportPending", reason: "ImportPending", want: false},

		// Success and empty are NOT degraded.
		{name: "success Completed", reason: ReasonCompleted, want: false},
		{name: "empty reason", reason: "", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsReasonDegraded(tc.reason); got != tc.want {
				t.Fatalf("IsReasonDegraded(%q) = %v, want %v", tc.reason, got, tc.want)
			}
		})
	}
}

// TestDegradedReadyReasons_ExactMembership guards the catalog against verbatim drift from the ADR:
// it must contain exactly {ChildSnapshotDeleted}.
func TestDegradedReadyReasons_ExactMembership(t *testing.T) {
	if len(DegradedReadyReasons) != 1 {
		t.Fatalf("DegradedReadyReasons must have exactly 1 member, got %d: %v", len(DegradedReadyReasons), DegradedReadyReasons)
	}
	if _, ok := DegradedReadyReasons[ReasonChildSnapshotDeleted]; !ok {
		t.Fatalf("DegradedReadyReasons must contain %q", ReasonChildSnapshotDeleted)
	}
}

// TestDegradedAndTerminalReasonsDisjoint proves no reason is classified as both degraded and terminal.
func TestDegradedAndTerminalReasonsDisjoint(t *testing.T) {
	for reason := range DegradedReadyReasons {
		if _, ok := TerminalReadyReasons[reason]; ok {
			t.Errorf("reason %q is in both DegradedReadyReasons and TerminalReadyReasons; the sets must be disjoint", reason)
		}
	}
	for reason := range TerminalReadyReasons {
		if _, ok := DegradedReadyReasons[reason]; ok {
			t.Errorf("reason %q is in both TerminalReadyReasons and DegradedReadyReasons; the sets must be disjoint", reason)
		}
	}
}
