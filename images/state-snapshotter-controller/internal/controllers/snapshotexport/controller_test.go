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

package snapshotexport

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

func exportScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}
	return s
}

func export(name string, uid types.UID) *storagev1alpha1.SnapshotExport {
	return &storagev1alpha1.SnapshotExport{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", UID: uid},
	}
}

// TestResourceBaseName_ExportUnique covers review M1: two exports whose names share the first 40 chars
// and target the same leaf must NOT derive the same intermediate resource name.
func TestResourceBaseName_ExportUnique(t *testing.T) {
	longA := strings.Repeat("x", 40) + "-alpha"
	longB := strings.Repeat("x", 40) + "-beta" // identical first 40 chars as longA
	key := "Snapshot--ns--root/vsc-1"

	a := resourceBaseName(export(longA, types.UID("uid-a")), key)
	b := resourceBaseName(export(longB, types.UID("uid-b")), key)
	if a == b {
		t.Fatalf("names must differ for distinct exports sharing a 40-char prefix: both %q", a)
	}

	// Same export + same key is stable.
	again := resourceBaseName(export(longA, types.UID("uid-a")), key)
	if a != again {
		t.Fatalf("resourceBaseName must be stable: %q != %q", a, again)
	}
	// Same name but different UID still differs (UID is folded into the hash).
	diffUID := resourceBaseName(export(longA, types.UID("uid-z")), key)
	if a == diffUID {
		t.Fatalf("names must differ when only the UID differs: both %q", a)
	}
	if !strings.HasPrefix(a, "se-") {
		t.Fatalf("name must start with se-, got %q", a)
	}
}

func TestSnapshotID(t *testing.T) {
	got := snapshotID(snapshotpkg.ObjectRef{Kind: "Snapshot", Namespace: "ns", Name: "root"})
	if want := "Snapshot--ns--root"; got != want {
		t.Fatalf("snapshotID: got %q want %q", got, want)
	}
}

func TestIsTerminalReason(t *testing.T) {
	for _, r := range []string{"RestoreFailed", "InvalidMode", "Expired", "NotFound"} {
		if !isTerminalReason(r) {
			t.Errorf("%q should be terminal", r)
		}
	}
	for _, r := range []string{"", "Completed", "TargetsPending", "Working"} {
		if isTerminalReason(r) {
			t.Errorf("%q should NOT be terminal", r)
		}
	}
	if reasonOrUnknown("") != "pending" {
		t.Errorf("empty reason should render as pending")
	}
	if reasonOrUnknown("X") != "X" {
		t.Errorf("non-empty reason should pass through")
	}
}

func TestCapJoin(t *testing.T) {
	if got := capJoin([]string{"a", "b"}); got != "a; b" {
		t.Fatalf("capJoin: got %q", got)
	}
	long := []string{strings.Repeat("z", 2000)}
	if got := capJoin(long); len(got) > 1024+3 {
		t.Fatalf("capJoin must cap length, got len=%d", len(got))
	}
}

// TestOwnerRefs covers review H1: the controller-owned children get Controller=true, while the held
// PVC ref must be a NON-controller owner (the VRR/ObjectKeeper already controls the PVC).
func TestOwnerRefs(t *testing.T) {
	r := &SnapshotExportReconciler{}
	exp := export("e", types.UID("u1"))

	child := r.ownerRef(exp)
	if child.Controller == nil || !*child.Controller {
		t.Fatalf("child ownerRef must be Controller=true")
	}
	if child.BlockOwnerDeletion == nil || *child.BlockOwnerDeletion {
		t.Fatalf("child ownerRef must have BlockOwnerDeletion=false (no finalizers RBAC dependency)")
	}

	hold := r.holdOwnerRef(exp)
	if hold.Controller == nil || *hold.Controller {
		t.Fatalf("hold ownerRef must be Controller=false")
	}
	if hold.BlockOwnerDeletion == nil || *hold.BlockOwnerDeletion {
		t.Fatalf("hold ownerRef must have BlockOwnerDeletion=false")
	}
}

func TestNewVolumeRestoreRequest(t *testing.T) {
	r := &SnapshotExportReconciler{}
	owner := r.ownerRef(export("e", types.UID("u1")))
	vrr := newVolumeRestoreRequest("ns", "se-x", owner, "vsc-1", "ns", "se-x", "fast", "Block", "ext4", []string{"ReadWriteOnce"})

	if got := nestedStr(vrr, "spec", "volumeMode"); got != "Block" {
		t.Fatalf("volumeMode: got %q", got)
	}
	if got := nestedStr(vrr, "spec", "sourceRef", "name"); got != "vsc-1" {
		t.Fatalf("sourceRef.name: got %q", got)
	}
	if got := nestedStr(vrr, "spec", "sourceRef", "kind"); got != kindVolumeSnapshotContent {
		t.Fatalf("sourceRef.kind: got %q", got)
	}
	if got := nestedStr(vrr, "spec", "targetPVCName"); got != "se-x" {
		t.Fatalf("targetPVCName: got %q", got)
	}
	if got := nestedStr(vrr, "spec", "storageClassName"); got != "fast" {
		t.Fatalf("storageClassName: got %q", got)
	}
	if got := nestedStr(vrr, "spec", "fsType"); got != "ext4" {
		t.Fatalf("fsType: got %q", got)
	}
}

func TestNewDataExport(t *testing.T) {
	r := &SnapshotExportReconciler{}
	owner := r.ownerRef(export("e", types.UID("u1")))
	de := newDataExport("ns", "se-x", owner, "se-x", "24h", true)

	if got := nestedStr(de, "spec", "ttl"); got != "24h" {
		t.Fatalf("ttl: got %q", got)
	}
	if got := nestedStr(de, "spec", "targetRef", "kind"); got != kindPersistentVolumeClaim {
		t.Fatalf("targetRef.kind: got %q", got)
	}
	if got := nestedStr(de, "spec", "targetRef", "name"); got != "se-x" {
		t.Fatalf("targetRef.name: got %q", got)
	}
}

// TestIsExpiredLatched verifies the terminal-Expired latch predicate: only Ready=False/reason=Expired
// counts; an absent condition, a True Ready, or a different reason must not latch.
func TestIsExpiredLatched(t *testing.T) {
	cases := []struct {
		name  string
		conds []metav1.Condition
		latch bool
	}{
		{name: "no conditions", conds: nil, latch: false},
		{
			name:  "ready true",
			conds: []metav1.Condition{{Type: storagev1alpha1.SnapshotExportConditionReady, Status: metav1.ConditionTrue, Reason: storagev1alpha1.SnapshotExportReasonPublished}},
			latch: false,
		},
		{
			name:  "false but other reason",
			conds: []metav1.Condition{{Type: storagev1alpha1.SnapshotExportConditionReady, Status: metav1.ConditionFalse, Reason: storagev1alpha1.SnapshotExportReasonDataPending}},
			latch: false,
		},
		{
			name:  "false expired",
			conds: []metav1.Condition{{Type: storagev1alpha1.SnapshotExportConditionReady, Status: metav1.ConditionFalse, Reason: storagev1alpha1.SnapshotExportReasonExpired}},
			latch: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exp := export("e", types.UID("u1"))
			exp.Status.Conditions = tc.conds
			if got := isExpiredLatched(exp); got != tc.latch {
				t.Fatalf("isExpiredLatched = %v, want %v", got, tc.latch)
			}
		})
	}
}

// TestSetExpired verifies the terminal-Expired writer latches both Ready and DataReady to
// False/reason=Expired and records the (non-serving) entries.
func TestSetExpired(t *testing.T) {
	ctx := context.Background()
	scheme := exportScheme(t)
	exp := export("e", types.UID("u1"))
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(exp).
		WithStatusSubresource(&storagev1alpha1.SnapshotExport{}).Build()
	r := &SnapshotExportReconciler{Client: cl, Scheme: scheme}

	entries := []storagev1alpha1.SnapshotExportDataEntry{{SnapshotID: "a"}}
	if _, err := r.setExpired(ctx, exp, entries); err != nil {
		t.Fatalf("setExpired: %v", err)
	}
	got := &storagev1alpha1.SnapshotExport{}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "e"}, got); err != nil {
		t.Fatal(err)
	}
	if !isExpiredLatched(got) {
		t.Fatalf("export must be latched Expired after setExpired, conditions=%#v", got.Status.Conditions)
	}
	dr := meta.FindStatusCondition(got.Status.Conditions, storagev1alpha1.SnapshotExportConditionDataReady)
	if dr == nil || dr.Status != metav1.ConditionFalse || dr.Reason != storagev1alpha1.SnapshotExportReasonExpired {
		t.Fatalf("DataReady must be False/Expired, got %#v", dr)
	}
	if len(got.Status.DataSnapshots) != 1 || got.Status.DataSnapshots[0].SnapshotID != "a" {
		t.Fatalf("entries not recorded: %#v", got.Status.DataSnapshots)
	}
}

// TestPublishStatus covers review H2: a failing leaf must surface DataReady/Ready=False with the
// DataExportFailed reason and the detail message, not silently stay generic-pending.
func TestPublishStatus(t *testing.T) {
	ctx := context.Background()
	scheme := exportScheme(t)

	cases := []struct {
		name       string
		allReady   bool
		anyFailed  bool
		details    []string
		entries    []storagev1alpha1.SnapshotExportDataEntry
		wantReady  metav1.ConditionStatus
		wantReason string
	}{
		{
			name:       "published",
			allReady:   true,
			entries:    []storagev1alpha1.SnapshotExportDataEntry{{SnapshotID: "a", DataURL: "u", Ready: true}},
			wantReady:  metav1.ConditionTrue,
			wantReason: storagev1alpha1.SnapshotExportReasonPublished,
		},
		{
			name:       "failed",
			anyFailed:  true,
			details:    []string{"a: VolumeRestoreRequest not ready (RestoreFailed)"},
			entries:    []storagev1alpha1.SnapshotExportDataEntry{{SnapshotID: "a"}},
			wantReady:  metav1.ConditionFalse,
			wantReason: storagev1alpha1.SnapshotExportReasonDataExportFailed,
		},
		{
			name:       "pending",
			details:    []string{"a: restored PVC not present yet"},
			entries:    []storagev1alpha1.SnapshotExportDataEntry{{SnapshotID: "a"}},
			wantReady:  metav1.ConditionFalse,
			wantReason: storagev1alpha1.SnapshotExportReasonDataPending,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exp := export("e", types.UID("u1"))
			cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(exp).
				WithStatusSubresource(&storagev1alpha1.SnapshotExport{}).Build()
			r := &SnapshotExportReconciler{Client: cl, Scheme: scheme}

			if err := r.publishStatus(ctx, exp, "/index", "/manifests", tc.entries, tc.allReady, tc.anyFailed, tc.details); err != nil {
				t.Fatalf("publishStatus: %v", err)
			}
			got := &storagev1alpha1.SnapshotExport{}
			if err := cl.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "e"}, got); err != nil {
				t.Fatal(err)
			}
			cond := meta.FindStatusCondition(got.Status.Conditions, storagev1alpha1.SnapshotExportConditionReady)
			if cond == nil || cond.Status != tc.wantReady || cond.Reason != tc.wantReason {
				t.Fatalf("Ready cond: got %#v, want status=%s reason=%s", cond, tc.wantReady, tc.wantReason)
			}
			if got.Status.IndexURL != "/index" || got.Status.ManifestsURL != "/manifests" {
				t.Fatalf("URLs not published: %+v", got.Status)
			}
		})
	}
}
