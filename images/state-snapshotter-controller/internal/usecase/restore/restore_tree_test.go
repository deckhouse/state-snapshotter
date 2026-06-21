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

package restore

import (
	"context"
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

func restoreTreeScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = storagev1alpha1.AddToScheme(scheme)
	demoDiskSnapshotGVK := schema.GroupVersionKind{Group: "demo.state-snapshotter.deckhouse.io", Version: "v1alpha1", Kind: "DemoVirtualDiskSnapshot"}
	scheme.AddKnownTypeWithName(demoDiskSnapshotGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: demoDiskSnapshotGVK.Group, Version: demoDiskSnapshotGVK.Version, Kind: "DemoVirtualDiskSnapshotList"}, &unstructured.UnstructuredList{})
	vsGVK := schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshot"}
	scheme.AddKnownTypeWithName(vsGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: vsGVK.Group, Version: vsGVK.Version, Kind: "VolumeSnapshotList"}, &unstructured.UnstructuredList{})
	return scheme
}

func readySnapshotContent(name, mcp string, dataRefs []storagev1alpha1.SnapshotDataBinding) *storagev1alpha1.SnapshotContent {
	c := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     storagev1alpha1.SnapshotContentStatus{ManifestCheckpointName: mcp, DataRefs: dataRefs},
	}
	meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{Type: snapshot.ConditionReady, Status: metav1.ConditionTrue, Reason: "Completed"})
	return c
}

func demoDiskSnapshotObj(name, contentName string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "demo.state-snapshotter.deckhouse.io/v1alpha1",
		"kind":       "DemoVirtualDiskSnapshot",
		"metadata":   map[string]interface{}{"name": name, "namespace": "source-ns"},
		"status": map[string]interface{}{
			"boundSnapshotContentName": contentName,
			"conditions":               []interface{}{map[string]interface{}{"type": "Ready", "status": "True"}},
		},
	}}
}

func volumeSnapshotObj(name, boundVSC string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "snapshot.storage.k8s.io/v1",
		"kind":       "VolumeSnapshot",
		"metadata":   map[string]interface{}{"name": name, "namespace": "source-ns"},
		"status":     map[string]interface{}{"boundVolumeSnapshotContentName": boundVSC, "readyToUse": true},
	}}
}

func rootSnapshotObj(name, contentName string, children []storagev1alpha1.SnapshotChildRef) *storagev1alpha1.Snapshot {
	s := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "source-ns"},
		Status:     storagev1alpha1.SnapshotStatus{BoundSnapshotContentName: contentName, ChildrenSnapshotRefs: children},
	}
	meta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{Type: snapshot.ConditionReady, Status: metav1.ConditionTrue, Reason: "Completed"})
	return s
}

// domainSnapshotObj builds a domain snapshot CR (e.g. DemoVirtualDiskSnapshot) with a bound content,
// optional child refs and a Ready condition. The generic resolver (NewResolver without a domain-kind
// predicate) treats such kinds as ordinary child snapshot nodes, so these helpers exercise the
// resolver's readiness/cycle/childrenSnapshotRefs handling at arbitrary depth.
func domainSnapshotObj(kind, name, contentName string, childRefs []storagev1alpha1.SnapshotChildRef, ready bool) *unstructured.Unstructured {
	refs := make([]interface{}, 0, len(childRefs))
	for _, r := range childRefs {
		refs = append(refs, map[string]interface{}{"apiVersion": r.APIVersion, "kind": r.Kind, "name": r.Name})
	}
	readyStatus := "True"
	if !ready {
		readyStatus = "False"
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "demo.state-snapshotter.deckhouse.io/v1alpha1",
		"kind":       kind,
		"metadata":   map[string]interface{}{"name": name, "namespace": "source-ns"},
		"status": map[string]interface{}{
			"boundSnapshotContentName": contentName,
			"conditions":               []interface{}{map[string]interface{}{"type": "Ready", "status": readyStatus}},
			"childrenSnapshotRefs":     refs,
		},
	}}
}

func TestResolveRestoreTree_ResolvesVSLeavesAndChildSnapshots(t *testing.T) {
	scheme := restoreTreeScheme()

	root := rootSnapshotObj("snap", "root-content", []storagev1alpha1.SnapshotChildRef{
		{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshot", Name: "vs-orphan"},
		{APIVersion: "demo.state-snapshotter.deckhouse.io/v1alpha1", Kind: "DemoVirtualDiskSnapshot", Name: "disk-snap"},
	})
	rootContent := readySnapshotContent("root-content", "mcp-root", []storagev1alpha1.SnapshotDataBinding{{
		TargetUID: "uid-orphan",
		Target:    storagev1alpha1.SnapshotSubjectRef{APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "orphan-pvc", Namespace: "source-ns", UID: "uid-orphan"},
		Artifact:  storagev1alpha1.SnapshotDataArtifactRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: "vsc-orphan"},
	}})
	diskSnap := demoDiskSnapshotObj("disk-snap", "disk-content")
	diskContent := readySnapshotContent("disk-content", "mcp-disk", nil)
	vs := volumeSnapshotObj("vs-orphan", "vsc-orphan")

	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(root, rootContent, diskSnap, diskContent, vs).Build()

	node, err := NewResolver(cl).ResolveRestoreTree(context.Background(), "source-ns", "snap")
	if err != nil {
		t.Fatalf("ResolveRestoreTree: %v", err)
	}
	if node.ManifestCheckpointName != "mcp-root" {
		t.Fatalf("root MCP = %q", node.ManifestCheckpointName)
	}
	if got := node.VSCToVS["vsc-orphan"]; got != "vs-orphan" {
		t.Fatalf("VSCToVS[vsc-orphan] = %q, want vs-orphan", got)
	}
	if len(node.Children) != 1 {
		t.Fatalf("expected 1 child snapshot, got %d", len(node.Children))
	}
	child := node.Children[0]
	if child.SnapshotRef.Kind != "DemoVirtualDiskSnapshot" || child.SnapshotRef.Name != "disk-snap" {
		t.Fatalf("child snapshotRef = %+v", child.SnapshotRef)
	}
	if child.ManifestCheckpointName != "mcp-disk" {
		t.Fatalf("child MCP = %q", child.ManifestCheckpointName)
	}
}

func TestResolveRestoreTree_MissingVSLeafFailsClosed(t *testing.T) {
	scheme := restoreTreeScheme()
	root := rootSnapshotObj("snap", "root-content", []storagev1alpha1.SnapshotChildRef{
		{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshot", Name: "vs-missing"},
	})
	rootContent := readySnapshotContent("root-content", "mcp-root", nil)

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(root, rootContent).Build()

	_, err := NewResolver(cl).ResolveRestoreTree(context.Background(), "source-ns", "snap")
	if err == nil {
		t.Fatal("expected error when VolumeSnapshot leaf is missing")
	}
	if !errors.Is(err, ErrContractViolation) {
		t.Fatalf("expected ErrContractViolation, got %v", err)
	}
}

func TestResolveRestoreTree_VSLeafEmptyBoundFailsClosed(t *testing.T) {
	scheme := restoreTreeScheme()
	root := rootSnapshotObj("snap", "root-content", []storagev1alpha1.SnapshotChildRef{
		{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshot", Name: "vs-unbound"},
	})
	rootContent := readySnapshotContent("root-content", "mcp-root", nil)
	vs := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "snapshot.storage.k8s.io/v1",
		"kind":       "VolumeSnapshot",
		"metadata":   map[string]interface{}{"name": "vs-unbound", "namespace": "source-ns"},
		"status":     map[string]interface{}{"readyToUse": true, "boundVolumeSnapshotContentName": ""},
	}}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(root, rootContent, vs).Build()

	_, err := NewResolver(cl).ResolveRestoreTree(context.Background(), "source-ns", "snap")
	if err == nil {
		t.Fatal("expected error when VolumeSnapshot leaf has empty boundVolumeSnapshotContentName")
	}
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("expected ErrNotReady, got %v", err)
	}
}

func TestResolveRestoreTree_VSLeafNotReadyFailsClosed(t *testing.T) {
	scheme := restoreTreeScheme()
	root := rootSnapshotObj("snap", "root-content", []storagev1alpha1.SnapshotChildRef{
		{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshot", Name: "vs-pending"},
	})
	rootContent := readySnapshotContent("root-content", "mcp-root", nil)
	vs := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "snapshot.storage.k8s.io/v1",
		"kind":       "VolumeSnapshot",
		"metadata":   map[string]interface{}{"name": "vs-pending", "namespace": "source-ns"},
		"status":     map[string]interface{}{"readyToUse": false, "boundVolumeSnapshotContentName": "vsc-x"},
	}}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(root, rootContent, vs).Build()

	_, err := NewResolver(cl).ResolveRestoreTree(context.Background(), "source-ns", "snap")
	if err == nil {
		t.Fatal("expected error when VolumeSnapshot leaf is not readyToUse")
	}
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("expected ErrNotReady, got %v", err)
	}
}

func TestResolveRestoreTree_VSLeafDeletingFailsClosed(t *testing.T) {
	scheme := restoreTreeScheme()
	root := rootSnapshotObj("snap", "root-content", []storagev1alpha1.SnapshotChildRef{
		{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshot", Name: "vs-deleting"},
	})
	rootContent := readySnapshotContent("root-content", "mcp-root", nil)
	now := metav1.Now()
	vs := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "snapshot.storage.k8s.io/v1",
		"kind":       "VolumeSnapshot",
		"metadata": map[string]interface{}{
			"name":              "vs-deleting",
			"namespace":         "source-ns",
			"deletionTimestamp": now.UTC().Format("2006-01-02T15:04:05Z"),
			"finalizers":        []interface{}{"snapshot.storage.kubernetes.io/volumesnapshot-as-source-protection"},
		},
		"status": map[string]interface{}{"readyToUse": true, "boundVolumeSnapshotContentName": "vsc-x"},
	}}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(root, rootContent, vs).Build()

	_, err := NewResolver(cl).ResolveRestoreTree(context.Background(), "source-ns", "snap")
	if err == nil {
		t.Fatal("expected error when VolumeSnapshot leaf is being deleted")
	}
	if !errors.Is(err, ErrContractViolation) {
		t.Fatalf("expected ErrContractViolation, got %v", err)
	}
}

func TestResolveRestoreTree_DuplicateVSForSameVSCFailsClosed(t *testing.T) {
	scheme := restoreTreeScheme()
	root := rootSnapshotObj("snap", "root-content", []storagev1alpha1.SnapshotChildRef{
		{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshot", Name: "vs-a"},
		{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshot", Name: "vs-b"},
	})
	rootContent := readySnapshotContent("root-content", "mcp-root", nil)
	// Both leaves bound to the same VSC -> contract violation.
	vsA := volumeSnapshotObj("vs-a", "vsc-shared")
	vsB := volumeSnapshotObj("vs-b", "vsc-shared")

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(root, rootContent, vsA, vsB).Build()

	_, err := NewResolver(cl).ResolveRestoreTree(context.Background(), "source-ns", "snap")
	if err == nil {
		t.Fatal("expected error when two VolumeSnapshot leaves bind the same VSC")
	}
	if !errors.Is(err, ErrContractViolation) {
		t.Fatalf("expected ErrContractViolation, got %v", err)
	}
}

func TestResolveRestoreTree_RootMissingReadyConditionFailsClosed(t *testing.T) {
	scheme := restoreTreeScheme()
	// Root snapshot has bound content + ready content, but NO Ready condition of its own.
	root := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "source-ns"},
		Status:     storagev1alpha1.SnapshotStatus{BoundSnapshotContentName: "root-content"},
	}
	rootContent := readySnapshotContent("root-content", "mcp-root", nil)

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(root, rootContent).Build()

	_, err := NewResolver(cl).ResolveRestoreTree(context.Background(), "source-ns", "snap")
	if err == nil {
		t.Fatal("expected error when root Snapshot has no Ready=True condition")
	}
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("expected ErrNotReady, got %v", err)
	}
}

func TestResolveRestoreTree_ChildSnapshotNotReadyFailsClosed(t *testing.T) {
	scheme := restoreTreeScheme()
	root := rootSnapshotObj("snap", "root-content", []storagev1alpha1.SnapshotChildRef{
		{APIVersion: demoGroupV, Kind: "DemoVirtualDiskSnapshot", Name: "disk-snap"},
	})
	rootContent := readySnapshotContent("root-content", "mcp-root", nil)
	// Child snapshot CR itself is not Ready (its content is Ready).
	diskSnap := domainSnapshotObj("DemoVirtualDiskSnapshot", "disk-snap", "disk-content", nil, false)
	diskContent := readySnapshotContent("disk-content", "mcp-disk", nil)

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(root, rootContent, diskSnap, diskContent).Build()

	_, err := NewResolver(cl).ResolveRestoreTree(context.Background(), "source-ns", "snap")
	if err == nil {
		t.Fatal("expected error when a child snapshot CR is not Ready")
	}
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("expected ErrNotReady, got %v", err)
	}
}

func TestResolveRestoreTree_CycleFailsClosed(t *testing.T) {
	scheme := restoreTreeScheme()
	root := rootSnapshotObj("snap", "root-content", []storagev1alpha1.SnapshotChildRef{
		{APIVersion: demoGroupV, Kind: "DemoVirtualDiskSnapshot", Name: "a"},
	})
	rootContent := readySnapshotContent("root-content", "mcp-root", nil)
	// a -> b -> a forms a cycle in the snapshot run tree.
	a := domainSnapshotObj("DemoVirtualDiskSnapshot", "a", "a-content", []storagev1alpha1.SnapshotChildRef{
		{APIVersion: demoGroupV, Kind: "DemoVirtualDiskSnapshot", Name: "b"},
	}, true)
	aContent := readySnapshotContent("a-content", "mcp-a", nil)
	b := domainSnapshotObj("DemoVirtualDiskSnapshot", "b", "b-content", []storagev1alpha1.SnapshotChildRef{
		{APIVersion: demoGroupV, Kind: "DemoVirtualDiskSnapshot", Name: "a"},
	}, true)
	bContent := readySnapshotContent("b-content", "mcp-b", nil)

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(root, rootContent, a, aContent, b, bContent).Build()

	_, err := NewResolver(cl).ResolveRestoreTree(context.Background(), "source-ns", "snap")
	if err == nil {
		t.Fatal("expected error on a cycle in the snapshot run tree")
	}
	if !errors.Is(err, ErrContractViolation) {
		t.Fatalf("expected ErrContractViolation, got %v", err)
	}
}

func TestResolveRestoreTree_ChildNotReadyFailsClosed(t *testing.T) {
	scheme := restoreTreeScheme()
	root := rootSnapshotObj("snap", "root-content", []storagev1alpha1.SnapshotChildRef{
		{APIVersion: "demo.state-snapshotter.deckhouse.io/v1alpha1", Kind: "DemoVirtualDiskSnapshot", Name: "disk-snap"},
	})
	rootContent := readySnapshotContent("root-content", "mcp-root", nil)
	diskSnap := demoDiskSnapshotObj("disk-snap", "disk-content")
	// disk content present but NOT Ready
	diskContent := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "disk-content"},
		Status:     storagev1alpha1.SnapshotContentStatus{ManifestCheckpointName: "mcp-disk"},
	}
	meta.SetStatusCondition(&diskContent.Status.Conditions, metav1.Condition{Type: snapshot.ConditionReady, Status: metav1.ConditionFalse, Reason: "Pending"})

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(root, rootContent, diskSnap, diskContent).Build()

	_, err := NewResolver(cl).ResolveRestoreTree(context.Background(), "source-ns", "snap")
	if err == nil {
		t.Fatal("expected error when a child SnapshotContent is not Ready")
	}
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("expected ErrNotReady, got %v", err)
	}
}
