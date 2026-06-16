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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase/restore"
)

// vmTreeIndex builds: root(Snapshot) -> [vm(VM), diskStandalone]; vm -> [diskA]. Children links are
// populated so effectiveIndex can walk the subtree.
func vmTreeIndex() *restore.Index {
	const vmGV = domainGV
	root := restore.IndexSnapshot{ID: "Snapshot--ns--root", APIVersion: storageGV, Kind: "Snapshot", Namespace: "ns", Name: "root",
		Children: []string{"DemoVirtualMachineSnapshot--ns--vm", "DemoVirtualDiskSnapshot--ns--standalone"}}
	vm := restore.IndexSnapshot{ID: "DemoVirtualMachineSnapshot--ns--vm", APIVersion: vmGV, Kind: "DemoVirtualMachineSnapshot", Namespace: "ns", Name: "vm",
		ParentID: root.ID, Children: []string{"DemoVirtualDiskSnapshot--ns--diskA"}}
	diskA := restore.IndexSnapshot{ID: "DemoVirtualDiskSnapshot--ns--diskA", APIVersion: domainGV, Kind: "DemoVirtualDiskSnapshot", Namespace: "ns", Name: "diskA", ParentID: vm.ID}
	standalone := restore.IndexSnapshot{ID: "DemoVirtualDiskSnapshot--ns--standalone", APIVersion: domainGV, Kind: "DemoVirtualDiskSnapshot", Namespace: "ns", Name: "standalone", ParentID: root.ID}
	return &restore.Index{
		Version:      restore.IndexVersion,
		RootSnapshot: restore.IndexSnapshotID{ID: root.ID, APIVersion: storageGV, Kind: "Snapshot", Namespace: "ns", Name: "root"},
		Snapshots:    []restore.IndexSnapshot{root, vm, diskA, standalone},
	}
}

// TestEffectiveIndex_Reroot proves selecting a child re-roots the index at that node, keeps only its
// subtree, and clears the new root's parent link without mutating the input index.
func TestEffectiveIndex_Reroot(t *testing.T) {
	idx := vmTreeIndex()
	child := &storagev1alpha1.SnapshotReference{APIVersion: domainGV, Kind: "DemoVirtualMachineSnapshot", Name: "vm"}

	eff, err := effectiveIndex(idx, child)
	if err != nil {
		t.Fatalf("effectiveIndex: %v", err)
	}
	if eff.RootSnapshot.ID != "DemoVirtualMachineSnapshot--ns--vm" || eff.RootSnapshot.Kind != "DemoVirtualMachineSnapshot" {
		t.Fatalf("re-rooted root = %+v, want vm", eff.RootSnapshot)
	}
	gotIDs := map[string]bool{}
	for i := range eff.Snapshots {
		gotIDs[eff.Snapshots[i].ID] = true
		if eff.Snapshots[i].ID == eff.RootSnapshot.ID && eff.Snapshots[i].ParentID != "" {
			t.Fatalf("new root must have empty ParentID, got %q", eff.Snapshots[i].ParentID)
		}
	}
	if len(gotIDs) != 2 || !gotIDs["DemoVirtualMachineSnapshot--ns--vm"] || !gotIDs["DemoVirtualDiskSnapshot--ns--diskA"] {
		t.Fatalf("subtree nodes = %v, want vm + diskA only", gotIDs)
	}
	// Input index must be untouched (standalone still present, vm still has a parent).
	if len(idx.Snapshots) != 4 || idx.Snapshots[1].ParentID == "" {
		t.Fatalf("input index was mutated: %+v", idx.Snapshots)
	}
}

func TestEffectiveIndex_NilChild(t *testing.T) {
	idx := vmTreeIndex()
	eff, err := effectiveIndex(idx, nil)
	if err != nil {
		t.Fatalf("effectiveIndex(nil): %v", err)
	}
	if eff != idx {
		t.Fatalf("nil child must return the index unchanged")
	}
}

func TestEffectiveIndex_ChildNotFound(t *testing.T) {
	idx := vmTreeIndex()
	child := &storagev1alpha1.SnapshotReference{APIVersion: domainGV, Kind: "DemoVirtualDiskSnapshot", Name: "ghost"}
	if _, err := effectiveIndex(idx, child); err == nil {
		t.Fatal("expected error for a child that is not in the bundle")
	}
}

// TestBuildSnapshotEntries proves every node gets a per-node manifests upload URL and data nodes also
// carry the merged volume upload fields.
func TestBuildSnapshotEntries(t *testing.T) {
	idx := twoNodeIndex()
	imp := &storagev1alpha1.SnapshotImport{ObjectMeta: metav1.ObjectMeta{Name: "imp", Namespace: "ns"}}
	r := &SnapshotImportReconciler{}

	dataEntries := []storagev1alpha1.SnapshotImportSnapshotEntry{
		{SnapshotID: idx.Snapshots[1].ID, VolumeMode: "Filesystem", UploadURL: "https://up", UploadReady: true},
	}
	entries := r.buildSnapshotEntries(imp, idx, dataEntries)
	if len(entries) != 2 {
		t.Fatalf("want one entry per node, got %d", len(entries))
	}
	for _, e := range entries {
		if !strings.Contains(e.ManifestsUploadURL, "/snapshotimports/imp/manifests?node=") {
			t.Fatalf("entry %s missing per-node manifests URL: %q", e.SnapshotID, e.ManifestsUploadURL)
		}
	}
	// Root node is dataless: no upload endpoint.
	if entries[0].UploadURL != "" || entries[0].VolumeMode != "" {
		t.Fatalf("root (dataless) must not carry upload fields: %+v", entries[0])
	}
	// Child node carries the merged data fields.
	if entries[1].VolumeMode != "Filesystem" || entries[1].UploadURL != "https://up" || !entries[1].UploadReady {
		t.Fatalf("data node fields not merged: %+v", entries[1])
	}
}

// TestDetectNameConflict proves the fail-closed preflight: a pre-existing target object that does not
// point at this import's content is a conflict, while this import's own objects (and absent objects)
// are not.
func TestDetectNameConflict(t *testing.T) {
	scheme := importScheme(t)
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "demo.state-snapshotter.deckhouse.io", Version: "v1alpha1", Kind: "DemoVirtualDiskSnapshot"}, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "demo.state-snapshotter.deckhouse.io", Version: "v1alpha1", Kind: "DemoVirtualDiskSnapshotList"}, &unstructured.UnstructuredList{})

	idx := twoNodeIndex()
	imp := &storagev1alpha1.SnapshotImport{
		ObjectMeta: metav1.ObjectMeta{Name: "imp", Namespace: "ns", UID: types.UID("u1")},
		Spec:       storagev1alpha1.SnapshotImportSpec{TargetName: "restored-root"},
	}

	t.Run("no objects -> no conflict", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).Build()
		r := &SnapshotImportReconciler{Client: cl, Direct: cl, Scheme: scheme}
		conflict, err := r.detectNameConflict(context.Background(), imp, idx)
		if err != nil || conflict != "" {
			t.Fatalf("want (\"\", nil), got (%q, %v)", conflict, err)
		}
	})

	t.Run("foreign root -> conflict", func(t *testing.T) {
		foreign := &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "restored-root", Namespace: "ns"},
			Spec:       storagev1alpha1.SnapshotSpec{Source: &storagev1alpha1.SnapshotSource{SnapshotContentName: "someone-elses-content"}},
		}
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(foreign).Build()
		r := &SnapshotImportReconciler{Client: cl, Direct: cl, Scheme: scheme}
		conflict, err := r.detectNameConflict(context.Background(), imp, idx)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if !strings.Contains(conflict, "restored-root") {
			t.Fatalf("want conflict on restored-root, got %q", conflict)
		}
	})

	t.Run("our own root -> no conflict", func(t *testing.T) {
		r0 := &SnapshotImportReconciler{}
		ours := &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "restored-root", Namespace: "ns"},
			Spec:       storagev1alpha1.SnapshotSpec{Source: &storagev1alpha1.SnapshotSource{SnapshotContentName: r0.contentName(imp, idx.Snapshots[0].ID)}},
		}
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ours).Build()
		r := &SnapshotImportReconciler{Client: cl, Direct: cl, Scheme: scheme}
		conflict, err := r.detectNameConflict(context.Background(), imp, idx)
		if err != nil || conflict != "" {
			t.Fatalf("our own object must not conflict, got (%q, %v)", conflict, err)
		}
	})
}
