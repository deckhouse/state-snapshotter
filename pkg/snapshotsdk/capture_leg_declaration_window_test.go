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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// A domain's terminal phase is a point-in-time outcome: nothing recomputes it. So the aggregate it is
// derived from must never say "complete" about a capture that is not — and what makes that easy to get
// wrong is that a half-written commonController looks exactly like a finished one of a different shape.
//
// The core now declares every applicable leg in a single patch, so it does not produce half-written states
// on purpose. It cannot promise they are unobservable either: a structural schema that declares one leg and
// not the other, or a foreign writer replacing the status, leaves precisely half of that write. Both halves
// are covered here, because both were once read as success:
//
//   - a captured data leg beside an absent manifest leg. Absent is not "this node has no manifest leg" —
//     every capture node has one. Reading it as success sent the domain terminal before the core declared
//     the leg, so it never created the capture request the core then asked for, and the content stayed at
//     ManifestCapturePending with no writer left that would move it;
//   - a captured manifest leg beside an absent data leg, on a kind that has a data line. Same absence,
//     same wrong conclusion, and the capture ends one leg short of done.

func windowCoreState(manifest, data *bool) CoreCaptureState {
	return CoreCaptureState{ManifestCaptured: manifest, DataCaptured: data}
}

// dataNodeCoreState is the same state on a kind that HAS a data line — the one thing the latches cannot say
// about themselves, and the difference between "no data leg here" and "not declared yet".
func dataNodeCoreState(manifest, data *bool) CoreCaptureState {
	c := windowCoreState(manifest, data)
	c.DataLegExpected = true
	return c
}

func windowAdapter(core CoreCaptureState) *refreshTestAdapter {
	return &refreshTestAdapter{
		obj:  &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "snap-w", UID: types.UID("snap-w-uid")}},
		core: core,
	}
}

func TestAllLegsCapturedRequiresTheManifestLegToBeDeclared(t *testing.T) {
	for _, tc := range []struct {
		name string
		core CoreCaptureState
		want bool
	}{
		{
			name: "the deadlock: data captured, manifest leg not declared yet",
			core: windowCoreState(nil, refreshBoolPtr(true)),
			want: false,
		},
		{
			name: "nothing declared at all",
			core: windowCoreState(nil, nil),
			want: false,
		},
		{
			name: "manifest leg declared open while the data leg is captured",
			core: windowCoreState(refreshBoolPtr(false), refreshBoolPtr(true)),
			want: false,
		},
		{
			name: "manifest-only node, captured",
			core: windowCoreState(refreshBoolPtr(true), nil),
			want: true,
		},
		{
			name: "data node whose data leg is not declared yet: half of one write is not a finished capture",
			core: dataNodeCoreState(refreshBoolPtr(true), nil),
			want: false,
		},
		{
			name: "data node, both legs captured",
			core: dataNodeCoreState(refreshBoolPtr(true), refreshBoolPtr(true)),
			want: true,
		},
		{
			name: "data node, data leg declared and still open",
			core: dataNodeCoreState(refreshBoolPtr(true), refreshBoolPtr(false)),
			want: false,
		},
		{
			name: "both legs captured",
			core: windowCoreState(refreshBoolPtr(true), refreshBoolPtr(true)),
			want: true,
		},
		{
			name: "manifest captured, data leg declared and still open",
			core: windowCoreState(refreshBoolPtr(true), refreshBoolPtr(false)),
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.core.AllLegsCaptured(); got != tc.want {
				t.Fatalf("AllLegsCaptured = %v, want %v", got, tc.want)
			}
		})
	}
}

// The same window seen through what the domain actually calls. CoreCaptureOutcome is the only thing
// standing between this state and a published terminal phase, so it is asserted in its own right rather
// than trusted to follow from the aggregate.
func TestCoreCaptureOutcomeStaysCapturingUntilTheManifestLegIsDeclared(t *testing.T) {
	adapter := windowAdapter(windowCoreState(nil, refreshBoolPtr(true)))

	outcome := CoreCaptureOutcome(adapter)
	if outcome.Outcome != CaptureOutcomeCapturing {
		t.Fatalf("outcome = %v, want %v: the manifest leg is undeclared, so this capture is not finished",
			outcome.Outcome, CaptureOutcomeCapturing)
	}

	// One core write later the leg is declared, and the answer is still "capturing" — now for the ordinary
	// reason. This is the state in which the domain creates the request, so it must be reachable: a domain
	// that had gone terminal in the window above never gets here.
	adapter.core = windowCoreState(refreshBoolPtr(false), refreshBoolPtr(true))
	if outcome := CoreCaptureOutcome(adapter); outcome.Outcome != CaptureOutcomeCapturing {
		t.Fatalf("outcome = %v, want %v with the manifest leg declared and open", outcome.Outcome, CaptureOutcomeCapturing)
	}
	if !ManifestCaptureNeeded(adapter) {
		t.Fatal("a declared, open manifest leg with no request name must be planned")
	}

	// And once the manifest leg is captured too, the capture is finished.
	adapter.core = windowCoreState(refreshBoolPtr(true), refreshBoolPtr(true))
	if outcome := CoreCaptureOutcome(adapter); outcome.Outcome != CaptureOutcomeCaptured {
		t.Fatalf("outcome = %v, want %v once every declared leg is captured", outcome.Outcome, CaptureOutcomeCaptured)
	}
}
