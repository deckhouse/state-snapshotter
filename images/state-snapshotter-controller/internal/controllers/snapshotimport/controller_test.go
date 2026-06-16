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

package snapshotimport

import (
	"context"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase/restore"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

func importScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}
	return s
}

const (
	storageGV = "storage.deckhouse.io/v1alpha1"
	domainGV  = "demo.state-snapshotter.deckhouse.io/v1alpha1"
)

func twoNodeIndex() *restore.Index {
	root := restore.IndexSnapshot{ID: "Snapshot--ns--root", APIVersion: storageGV, Kind: "Snapshot", Namespace: "ns", Name: "root"}
	child := restore.IndexSnapshot{ID: "DemoVirtualDiskSnapshot--ns--disk1", APIVersion: domainGV, Kind: "DemoVirtualDiskSnapshot", Namespace: "ns", Name: "disk1", ParentID: root.ID}
	return &restore.Index{
		Version:      restore.IndexVersion,
		RootSnapshot: restore.IndexSnapshotID{ID: root.ID, APIVersion: storageGV, Kind: "Snapshot", Namespace: "ns", Name: "root"},
		Snapshots:    []restore.IndexSnapshot{root, child},
	}
}

// TestRecreatedName covers review H1: the root node is recreated under spec.snapshotName; non-root
// nodes keep their original index name.
func TestRecreatedName(t *testing.T) {
	idx := twoNodeIndex()
	imp := &storagev1alpha1.SnapshotImport{Spec: storagev1alpha1.SnapshotImportSpec{TargetName: "restored-root"}}

	if got := recreatedName(imp, idx, &idx.Snapshots[0]); got != "restored-root" {
		t.Fatalf("root name: got %q want restored-root", got)
	}
	if got := recreatedName(imp, idx, &idx.Snapshots[1]); got != "disk1" {
		t.Fatalf("non-root name: got %q want disk1", got)
	}
}

func TestIsRootNode(t *testing.T) {
	idx := twoNodeIndex()
	if !isRootNode(idx, &idx.Snapshots[0]) {
		t.Fatalf("node 0 should be root")
	}
	if isRootNode(idx, &idx.Snapshots[1]) {
		t.Fatalf("node 1 should not be root")
	}
}

// TestNodesMissingSize covers review M2: data nodes with unknown size must be reported (fail closed).
func TestNodesMissingSize(t *testing.T) {
	nodes := []dataNode{
		{id: "a", size: 1024},
		{id: "b", size: 0},
		{id: "c", size: -1},
	}
	got := nodesMissingSize(nodes)
	want := []string{"b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("nodesMissingSize: got %v want %v", got, want)
	}
	if nodesMissingSize([]dataNode{{id: "ok", size: 5}}) != nil {
		t.Fatalf("no missing sizes should return nil")
	}
}

func TestSizeToQuantity(t *testing.T) {
	if got := sizeToQuantity(0); got != "" {
		t.Fatalf("zero size must be empty, got %q", got)
	}
	if got := sizeToQuantity(2048); got != "2048" {
		t.Fatalf("size: got %q", got)
	}
}

func TestUploadsReason(t *testing.T) {
	if uploadsReason(true) != storagev1alpha1.SnapshotImportReasonAllUploadsReady {
		t.Fatalf("ready reason mismatch")
	}
	if uploadsReason(false) != storagev1alpha1.SnapshotImportReasonUploadsPending {
		t.Fatalf("pending reason mismatch")
	}
}

// TestReadReadyReason verifies the Ready (status, reason) reader used to detect an idled-out DataImport.
func TestReadReadyReason(t *testing.T) {
	mk := func(conds ...map[string]interface{}) *unstructured.Unstructured {
		list := make([]interface{}, 0, len(conds))
		for _, c := range conds {
			list = append(list, c)
		}
		return &unstructured.Unstructured{Object: map[string]interface{}{
			"status": map[string]interface{}{"conditions": list},
		}}
	}
	cases := []struct {
		name       string
		obj        *unstructured.Unstructured
		wantReady  bool
		wantReason string
	}{
		{"no status", &unstructured.Unstructured{Object: map[string]interface{}{}}, false, ""},
		{"ready true", mk(map[string]interface{}{"type": "Ready", "status": "True", "reason": "Ready"}), true, "Ready"},
		{"expired", mk(map[string]interface{}{"type": "Ready", "status": "False", "reason": "Expired"}), false, "Expired"},
		{"other cond only", mk(map[string]interface{}{"type": "UploadFinished", "status": "True", "reason": "Finished"}), false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ready, reason := readReadyReason(tc.obj)
			if ready != tc.wantReady || reason != tc.wantReason {
				t.Fatalf("readReadyReason = (%v, %q), want (%v, %q)", ready, reason, tc.wantReady, tc.wantReason)
			}
		})
	}
}

// TestIsImportExpiredLatched verifies the terminal-Expired latch predicate: only Ready=False/Expired
// counts as latched.
func TestIsImportExpiredLatched(t *testing.T) {
	cases := []struct {
		name  string
		conds []metav1.Condition
		latch bool
	}{
		{name: "no conditions", conds: nil, latch: false},
		{
			name:  "ready true",
			conds: []metav1.Condition{{Type: storagev1alpha1.SnapshotImportConditionReady, Status: metav1.ConditionTrue, Reason: storagev1alpha1.SnapshotImportReasonImported}},
			latch: false,
		},
		{
			name:  "false other reason",
			conds: []metav1.Condition{{Type: storagev1alpha1.SnapshotImportConditionReady, Status: metav1.ConditionFalse, Reason: storagev1alpha1.SnapshotImportReasonUploadsPending}},
			latch: false,
		},
		{
			name:  "false expired",
			conds: []metav1.Condition{{Type: storagev1alpha1.SnapshotImportConditionReady, Status: metav1.ConditionFalse, Reason: storagev1alpha1.SnapshotImportReasonExpired}},
			latch: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			imp := &storagev1alpha1.SnapshotImport{}
			imp.Status.Conditions = tc.conds
			if got := isImportExpiredLatched(imp); got != tc.latch {
				t.Fatalf("isImportExpiredLatched = %v, want %v", got, tc.latch)
			}
		})
	}
}

// TestSetExpired verifies the terminal-Expired writer latches both UploadsPrepared and Ready to
// False/reason=Expired and records the entries.
func TestSetExpired(t *testing.T) {
	ctx := context.Background()
	scheme := importScheme(t)
	imp := &storagev1alpha1.SnapshotImport{
		ObjectMeta: metav1.ObjectMeta{Name: "imp", Namespace: "ns", UID: types.UID("u1")},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(imp).
		WithStatusSubresource(&storagev1alpha1.SnapshotImport{}).Build()
	r := &SnapshotImportReconciler{Client: cl, Direct: cl, Scheme: scheme}

	entries := []storagev1alpha1.SnapshotImportSnapshotEntry{{SnapshotID: "a"}}
	if err := r.setExpired(ctx, imp, entries); err != nil {
		t.Fatalf("setExpired: %v", err)
	}
	got := &storagev1alpha1.SnapshotImport{}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "imp"}, got); err != nil {
		t.Fatal(err)
	}
	if !isImportExpiredLatched(got) {
		t.Fatalf("import must be latched Expired after setExpired, conditions=%#v", got.Status.Conditions)
	}
	up := meta.FindStatusCondition(got.Status.Conditions, storagev1alpha1.SnapshotImportConditionUploadsPrepared)
	if up == nil || up.Status != metav1.ConditionFalse || up.Reason != storagev1alpha1.SnapshotImportReasonExpired {
		t.Fatalf("UploadsPrepared must be False/Expired, got %#v", up)
	}
	if len(got.Status.Snapshots) != 1 || got.Status.Snapshots[0].SnapshotID != "a" {
		t.Fatalf("entries not recorded: %#v", got.Status.Snapshots)
	}
}

// TestEnsureSnapshotContent_BackRef covers review H2: SnapshotContent.spec.snapshotRef.apiVersion/kind
// must come from the index node (so domain nodes get their domain group, not the storage group), and
// the name must match the recreated snapshot (root -> spec.snapshotName).
func TestEnsureSnapshotContent_BackRef(t *testing.T) {
	ctx := context.Background()
	scheme := importScheme(t)
	idx := twoNodeIndex()
	imp := &storagev1alpha1.SnapshotImport{
		ObjectMeta: metav1.ObjectMeta{Name: "imp", Namespace: "ns"},
		Spec:       storagev1alpha1.SnapshotImportSpec{TargetName: "restored-root"},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&storagev1alpha1.SnapshotContent{}).Build()
	r := &SnapshotImportReconciler{Client: cl, Direct: cl, Scheme: scheme}

	cases := []struct {
		name          string
		node          *restore.IndexSnapshot
		contentName   string
		wantRefName   string
		wantRefAPIVer string
		wantRefKind   string
	}{
		{"root", &idx.Snapshots[0], "content-root", "restored-root", storageGV, "Snapshot"},
		{"domain", &idx.Snapshots[1], "content-disk", "disk1", domainGV, "DemoVirtualDiskSnapshot"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := r.ensureSnapshotContent(ctx, tc.contentName, tc.node, imp, idx, "mcp-x", nil, nil); err != nil {
				t.Fatalf("ensureSnapshotContent: %v", err)
			}
			got := &storagev1alpha1.SnapshotContent{}
			if err := cl.Get(ctx, client.ObjectKey{Name: tc.contentName}, got); err != nil {
				t.Fatal(err)
			}
			ref := got.Spec.SnapshotRef
			if ref == nil {
				t.Fatalf("snapshotRef must be set")
			}
			if ref.Name != tc.wantRefName || ref.APIVersion != tc.wantRefAPIVer || ref.Kind != tc.wantRefKind {
				t.Fatalf("snapshotRef: got {name=%s apiVersion=%s kind=%s} want {name=%s apiVersion=%s kind=%s}",
					ref.Name, ref.APIVersion, ref.Kind, tc.wantRefName, tc.wantRefAPIVer, tc.wantRefKind)
			}
			if got.Status.ManifestCheckpointName != "mcp-x" {
				t.Fatalf("manifestCheckpointName not published: %q", got.Status.ManifestCheckpointName)
			}
			cond := meta.FindStatusCondition(got.Status.Conditions, snapshotpkg.ConditionReady)
			if cond == nil || cond.Status != metav1.ConditionTrue {
				t.Fatalf("content must be Ready=True after import, got %#v", cond)
			}
		})
	}
}
