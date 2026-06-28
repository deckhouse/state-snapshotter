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

func snapshotWithArchived(ns string, status metav1.ConditionStatus, reason string) *storagev1alpha1.Snapshot {
	s := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "snap"}}
	apimeta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{
		Type:   storagev1alpha1.ConditionManifestsArchived,
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
			snap: &storagev1alpha1.Snapshot{Spec: storagev1alpha1.SnapshotSpec{Source: &storagev1alpha1.SnapshotSource{Import: &storagev1alpha1.SnapshotImportSource{}}}},
			want: false,
		},
		{
			name: "static bind never captures live namespace",
			snap: &storagev1alpha1.Snapshot{Spec: storagev1alpha1.SnapshotSpec{Source: &storagev1alpha1.SnapshotSource{SnapshotContentName: "preprovisioned"}}},
			want: false,
		},
		{
			name: "no ManifestsArchived condition yet -> grant (capture about to start)",
			snap: &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Namespace: "ns"}},
			want: true,
		},
		{
			name: "Capturing -> grant",
			snap: snapshotWithArchived("ns", metav1.ConditionFalse, storagev1alpha1.ReasonManifestsCapturing),
			want: true,
		},
		{
			name: "Archived -> release",
			snap: snapshotWithArchived("ns", metav1.ConditionTrue, storagev1alpha1.ReasonManifestsArchived),
			want: false,
		},
		{
			name: "Failed -> release",
			snap: snapshotWithArchived("ns", metav1.ConditionFalse, storagev1alpha1.ReasonManifestsArchiveFailed),
			want: false,
		},
		{
			name: "Unknown status -> grant (fail-open, only True/Failed release)",
			snap: snapshotWithArchived("ns", metav1.ConditionUnknown, ""),
			want: true,
		},
		{
			name: "False with unexpected non-Failed reason -> grant (fail-open)",
			snap: snapshotWithArchived("ns", metav1.ConditionFalse, "SomethingUnexpected"),
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
		*snapshotWithArchived("ns-capturing", metav1.ConditionFalse, storagev1alpha1.ReasonManifestsCapturing),
		*snapshotWithArchived("ns-archived", metav1.ConditionTrue, storagev1alpha1.ReasonManifestsArchived),
		*snapshotWithArchived("ns-failed", metav1.ConditionFalse, storagev1alpha1.ReasonManifestsArchiveFailed),
		// Second snapshot in ns-archived still capturing -> namespace must stay in the desired set.
		*snapshotWithArchived("ns-archived", metav1.ConditionFalse, storagev1alpha1.ReasonManifestsCapturing),
	}
	desired := namespacesNeedingCaptureRBAC(snaps)

	if _, ok := desired["ns-capturing"]; !ok {
		t.Fatalf("ns-capturing must be in desired set")
	}
	if _, ok := desired["ns-archived"]; !ok {
		t.Fatalf("ns-archived must be in desired set (one snapshot still capturing)")
	}
	if _, ok := desired["ns-failed"]; ok {
		t.Fatalf("ns-failed must NOT be in desired set")
	}
	if len(desired) != 2 {
		t.Fatalf("desired set size = %d, want 2 (%v)", len(desired), desired)
	}
}
