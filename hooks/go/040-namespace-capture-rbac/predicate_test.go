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

package namespace_capture_rbac

import (
	"testing"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// snapshotWithManifestCaptured builds a Snapshot whose root manifest-leg latch
// (captureState.commonController.manifestCaptured) is set to captured. This is the monotonic signal the
// hook reads to release the transient capture RoleBinding.
func snapshotWithManifestCaptured(ns string, captured bool) *storagev1alpha1.Snapshot {
	c := captured
	return &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "snap"},
		Status: storagev1alpha1.SnapshotStatus{
			CaptureState: &storagev1alpha1.CaptureStateStatus{
				CommonController: &storagev1alpha1.CommonControllerCaptureState{ManifestCaptured: &c},
			},
		},
	}
}

// snapshotWithReady builds a Snapshot carrying a Ready condition (used to model terminal capture failure).
func snapshotWithReady(ns string, status metav1.ConditionStatus, reason string) *storagev1alpha1.Snapshot {
	s := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "snap"}}
	apimeta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{
		Type:   storagev1alpha1.ConditionReady,
		Status: status,
		Reason: reason,
	})
	return s
}

func TestNeedsCaptureRBAC(t *testing.T) {
	cases := []struct {
		name string
		snap *storagev1alpha1.Snapshot
		want bool
	}{
		{
			name: "nil snapshot",
			snap: nil,
			want: false,
		},
		{
			name: "import mode never captures live namespace",
			snap: &storagev1alpha1.Snapshot{Spec: storagev1alpha1.SnapshotSpec{Mode: storagev1alpha1.SnapshotModeImport}},
			want: false,
		},
		{
			name: "static bind never captures live namespace",
			snap: &storagev1alpha1.Snapshot{Spec: storagev1alpha1.SnapshotSpec{Mode: storagev1alpha1.SnapshotModeStaticBind}},
			want: false,
		},
		{
			name: "no captureState yet -> grant (capture about to start)",
			snap: &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Namespace: "ns"}},
			want: true,
		},
		{
			name: "manifest leg declared but not captured -> grant",
			snap: snapshotWithManifestCaptured("ns", false),
			want: true,
		},
		{
			name: "manifestCaptured=true -> release",
			snap: snapshotWithManifestCaptured("ns", true),
			want: false,
		},
		{
			name: "Ready=False terminal reason -> release",
			snap: snapshotWithReady("ns", metav1.ConditionFalse, storagev1alpha1.ReasonGraphPlanningFailed),
			want: false,
		},
		{
			name: "Ready=False non-terminal reason -> grant (capture still in progress)",
			snap: snapshotWithReady("ns", metav1.ConditionFalse, storagev1alpha1.ReasonResidualVolumeCapturePending),
			want: true,
		},
		{
			name: "Ready=Unknown -> grant (fail-open, only manifestCaptured/terminal release)",
			snap: snapshotWithReady("ns", metav1.ConditionUnknown, ""),
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := needsCaptureRBAC(tc.snap); got != tc.want {
				t.Fatalf("needsCaptureRBAC = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNamespacesNeedingCaptureRBAC(t *testing.T) {
	snaps := []storagev1alpha1.Snapshot{
		*snapshotWithManifestCaptured("ns-capturing", false),
		*snapshotWithManifestCaptured("ns-captured", true),
		*snapshotWithReady("ns-failed", metav1.ConditionFalse, storagev1alpha1.ReasonGraphPlanningFailed),
		// Second snapshot in ns-captured still capturing -> namespace must stay in the desired set.
		*snapshotWithManifestCaptured("ns-captured", false),
	}
	desired := namespacesNeedingCaptureRBAC(snaps)

	if _, ok := desired["ns-capturing"]; !ok {
		t.Fatalf("ns-capturing must be in desired set")
	}
	if _, ok := desired["ns-captured"]; !ok {
		t.Fatalf("ns-captured must be in desired set (one snapshot still capturing)")
	}
	if _, ok := desired["ns-failed"]; ok {
		t.Fatalf("ns-failed must NOT be in desired set")
	}
	if len(desired) != 2 {
		t.Fatalf("desired set size = %d, want 2 (%v)", len(desired), desired)
	}
}
