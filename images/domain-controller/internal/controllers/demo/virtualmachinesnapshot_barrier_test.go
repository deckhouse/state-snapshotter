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

package demo

import (
	"testing"

	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk"
)

// These pin the VM aggregator's barrier-2 wait/stop predicates (Variant A): the domain confirms
// consistency only once every child disk's data leg is latched, and it stops waiting once a child has gone
// terminal (the core owns and bubbles that terminal; the domain never re-drives it).

func TestChildrenHaveTerminal(t *testing.T) {
	cases := []struct {
		name     string
		children []snapshotsdk.ChildCaptureState
		want     bool
	}{
		{"empty", nil, false},
		{
			"all-pending",
			[]snapshotsdk.ChildCaptureState{{ReadyReason: "DataCapturePending"}, {ReadyReason: ""}},
			false,
		},
		{
			"one-terminal-volume-capture-failed",
			[]snapshotsdk.ChildCaptureState{{ReadyReason: "DataCapturePending"}, {ReadyReason: "VolumeCaptureFailed"}},
			true,
		},
		{
			"one-terminal-children-failed",
			[]snapshotsdk.ChildCaptureState{{ReadyReason: "ChildrenFailed"}},
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := childrenHaveTerminal(tc.children); got != tc.want {
				t.Fatalf("childrenHaveTerminal = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAllChildrenLegsCaptured(t *testing.T) {
	cases := []struct {
		name     string
		children []snapshotsdk.ChildCaptureState
		want     bool
	}{
		{"empty-vacuously-true", nil, true},
		{
			"all-captured",
			[]snapshotsdk.ChildCaptureState{{AllLegsCaptured: true}, {AllLegsCaptured: true}},
			true,
		},
		{
			"one-not-captured",
			[]snapshotsdk.ChildCaptureState{{AllLegsCaptured: true}, {AllLegsCaptured: false}},
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := allChildrenLegsCaptured(tc.children); got != tc.want {
				t.Fatalf("allChildrenLegsCaptured = %v, want %v", got, tc.want)
			}
		})
	}
}
