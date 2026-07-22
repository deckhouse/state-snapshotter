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

import "testing"

func settledBoolPtr(b bool) *bool { return &b }

// childrenSettled() reports the latch value, treating a nil latch (no children / not computed) as false.
func TestCoreCaptureState_ChildrenSettledHelper(t *testing.T) {
	for _, tc := range []struct {
		name  string
		latch *bool
		want  bool
	}{
		{"nil latch reads false", nil, false},
		{"false latch", settledBoolPtr(false), false},
		{"true latch", settledBoolPtr(true), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := CoreCaptureState{ChildrenSettled: tc.latch}
			if got := c.childrenSettled(); got != tc.want {
				t.Fatalf("childrenSettled() = %v, want %v", got, tc.want)
			}
		})
	}
}

// AllLegsCaptured must NOT react to ChildrenSettled: it is a per-node leg aggregate, while childrenSettled is
// a subtree-scoped completeness signal. Toggling ChildrenSettled (or leaving it nil) leaves AllLegsCaptured
// determined solely by the declared manifest/data legs.
func TestCoreCaptureState_AllLegsCapturedIgnoresChildrenSettled(t *testing.T) {
	for _, settled := range []*bool{nil, settledBoolPtr(false), settledBoolPtr(true)} {
		// Every declared leg captured => AllLegsCaptured true regardless of ChildrenSettled.
		captured := CoreCaptureState{
			ManifestCaptured: settledBoolPtr(true),
			DataCaptured:     settledBoolPtr(true),
			ChildrenSettled:  settled,
		}
		if !captured.AllLegsCaptured() {
			t.Fatalf("AllLegsCaptured must stay true with all legs captured (ChildrenSettled=%v)", settled)
		}

		// A not-yet-captured leg => AllLegsCaptured false, even when ChildrenSettled=true.
		pending := CoreCaptureState{
			ManifestCaptured: settledBoolPtr(true),
			DataCaptured:     settledBoolPtr(false),
			ChildrenSettled:  settled,
		}
		if pending.AllLegsCaptured() {
			t.Fatalf("AllLegsCaptured must stay false while the data leg is uncaptured (ChildrenSettled=%v)", settled)
		}
	}

	// A manifest-only node with children settled must still read AllLegsCaptured from its own manifest leg.
	manifestOnly := CoreCaptureState{ManifestCaptured: settledBoolPtr(true), ChildrenSettled: settledBoolPtr(true)}
	if !manifestOnly.AllLegsCaptured() {
		t.Fatalf("manifest-only node with captured manifest must be AllLegsCaptured=true")
	}
	noLegs := CoreCaptureState{ChildrenSettled: settledBoolPtr(true)}
	if noLegs.AllLegsCaptured() {
		t.Fatalf("with no declared leg AllLegsCaptured must be false even if ChildrenSettled=true")
	}
}
