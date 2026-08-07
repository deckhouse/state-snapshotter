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

package volumecapture

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
)

// vcrWithConditions builds a VCR carrying exactly the given conditions, each as
// {type, status, reason, message}.
func vcrWithConditions(conditions ...[4]string) *unstructured.Unstructured {
	items := make([]interface{}, 0, len(conditions))
	for _, c := range conditions {
		items = append(items, map[string]interface{}{
			"type":    c[0],
			"status":  c[1],
			"reason":  c[2],
			"message": c[3],
		})
	}
	vcr := &unstructured.Unstructured{Object: map[string]interface{}{}}
	vcr.SetGroupVersionKind(vcpkg.VolumeCaptureRequestGVK)
	if len(items) > 0 {
		_ = unstructured.SetNestedSlice(vcr.Object, items, "status", "conditions")
	}
	return vcr
}

func TestVolumeCaptureRequestStalled(t *testing.T) {
	t.Parallel()

	const (
		reason  = "SnapshotExecutionUnobservable"
		message = `no observable snapshot execution for VolumeSnapshotContent "vsc-1" for 4m12s`
	)
	pending := [4]string{vcpkg.ConditionTypeReady, string(metav1.ConditionFalse), vcpkg.ConditionReasonTargetsPending, "waiting"}

	tests := []struct {
		name       string
		vcr        *unstructured.Unstructured
		wantStall  bool
		wantReason string
	}{
		{
			name:       "a reported stall carries its reason and message",
			vcr:        vcrWithConditions(pending, [4]string{vcpkg.ConditionTypeStalled, string(metav1.ConditionTrue), reason, message}),
			wantStall:  true,
			wantReason: reason,
		},
		{
			name: "a cleared stall is not a stall",
			vcr: vcrWithConditions(pending, [4]string{vcpkg.ConditionTypeStalled, string(metav1.ConditionFalse),
				"SnapshotExecutionResumed", "observable snapshot activity resumed"}),
		},
		{
			name: "an unknown stall state is not evidence of a stall",
			vcr:  vcrWithConditions(pending, [4]string{vcpkg.ConditionTypeStalled, string(metav1.ConditionUnknown), "", ""}),
		},
		{
			name: "a request without the condition is not stalled",
			vcr:  vcrWithConditions(pending),
		},
		{
			name: "a request without any conditions is not stalled",
			vcr:  vcrWithConditions(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stalled, gotReason, gotMessage := VolumeCaptureRequestStalled(tt.vcr)
			if stalled != tt.wantStall {
				t.Fatalf("stalled = %v, want %v", stalled, tt.wantStall)
			}
			if gotReason != tt.wantReason {
				t.Fatalf("reason = %q, want %q", gotReason, tt.wantReason)
			}
			if tt.wantStall && gotMessage != message {
				t.Fatalf("message = %q, want the diagnosis verbatim", gotMessage)
			}
		})
	}
}

// TestStalledRequestIsNotFailed is the safety invariant of the whole stall track: a diagnosis must
// never be mistaken for a terminal failure, or a capture that is merely slow would be torn down.
func TestStalledRequestIsNotFailed(t *testing.T) {
	t.Parallel()

	vcr := vcrWithConditions(
		[4]string{vcpkg.ConditionTypeReady, string(metav1.ConditionFalse), vcpkg.ConditionReasonTargetsPending, "waiting"},
		[4]string{vcpkg.ConditionTypeStalled, string(metav1.ConditionTrue), "SnapshotExecutionUnobservable", "no observable snapshot execution"},
	)

	if failed, reason, _ := VolumeCaptureRequestFailed(vcr); failed {
		t.Fatalf("a stalled request must not read as failed, got reason %q", reason)
	}
	if VolumeCaptureRequestReady(vcr) {
		t.Fatalf("a stalled request is not ready either")
	}
}
