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

// readySnapshotContent builds a Ready SnapshotContent carrying at most one dataRef (Variant A: the
// data leg is a singular object, not a list). Pass nil for an aggregator/manifests-only node.
func readySnapshotContent(name, mcp string, dataRef *storagev1alpha1.SnapshotDataBinding) *storagev1alpha1.SnapshotContent {
	c := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     storagev1alpha1.SnapshotContentStatus{ManifestCheckpointName: mcp, Data: dataRef},
	}
	meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{Type: snapshot.ConditionReady, Status: metav1.ConditionTrue, Reason: "Completed"})
	return c
}

// orphanChildContent builds a Ready child volume-node SnapshotContent that carries the anti-spoofing
// back-reference (spec.snapshotRef) to the orphan VolumeSnapshot vsName (in source-ns) that binds it via
// status.boundSnapshotContentName. The restore resolver requires this reverse reference to fail closed
// against a content bound to a foreign snapshot subject.
func orphanChildContent(name, mcp, vsName string, dataRef *storagev1alpha1.SnapshotDataBinding) *storagev1alpha1.SnapshotContent {
	c := readySnapshotContent(name, mcp, dataRef)
	c.Spec.SnapshotRef = &storagev1alpha1.SnapshotSubjectRef{
		APIVersion: "snapshot.storage.k8s.io/v1",
		Kind:       "VolumeSnapshot",
		Namespace:  "source-ns",
		Name:       vsName,
	}
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

// volumeSnapshotObj builds an orphan-PVC CSI VolumeSnapshot handle (Variant A INV-ORPHAN4):
// boundVSC is the durable data artifact, and boundContent is status.boundSnapshotContentName, the
// namespaced handle to the standalone child volume SnapshotContent that owns the PVC manifest + dataRef.
func volumeSnapshotObj(name, boundVSC, boundContent string) *unstructured.Unstructured {
	status := map[string]interface{}{"boundVolumeSnapshotContentName": boundVSC, "readyToUse": true}
	if boundContent != "" {
		status["boundSnapshotContentName"] = boundContent
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "snapshot.storage.k8s.io/v1",
		"kind":       "VolumeSnapshot",
		"metadata":   map[string]interface{}{"name": name, "namespace": "source-ns"},
		"status":     status,
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

	// Variant A: the orphan-PVC VolumeSnapshot is a namespaced handle to a standalone child volume
	// node. Its PVC manifest + single dataRef live on that child SnapshotContent (its own MCP), not on
	// the root, so the resolver must materialize it as a child RestoreNode (not a root VSCToVS entry).
	root := rootSnapshotObj("snap", "root-content", []storagev1alpha1.SnapshotChildRef{
		{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshot", Name: "vs-orphan"},
		{APIVersion: "demo.state-snapshotter.deckhouse.io/v1alpha1", Kind: "DemoVirtualDiskSnapshot", Name: "disk-snap"},
	})
	rootContent := readySnapshotContent("root-content", "mcp-root", nil)
	orphanContent := orphanChildContent("root-content-vol-orphan", "mcp-orphan", "vs-orphan", &storagev1alpha1.SnapshotDataBinding{
		Source:   storagev1alpha1.SnapshotSubjectRef{APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "orphan-pvc", Namespace: "source-ns", UID: "uid-orphan"},
		Artifact: storagev1alpha1.SnapshotDataArtifactRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: "vsc-orphan"},
	})
	diskSnap := demoDiskSnapshotObj("disk-snap", "disk-content")
	diskContent := readySnapshotContent("disk-content", "mcp-disk", nil)
	vs := volumeSnapshotObj("vs-orphan", "vsc-orphan", "root-content-vol-orphan")

	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(root, rootContent, orphanContent, diskSnap, diskContent, vs).Build()

	node, err := NewResolver(cl).ResolveRestoreTree(context.Background(), "source-ns", "snap")
	if err != nil {
		t.Fatalf("ResolveRestoreTree: %v", err)
	}
	if node.ManifestCheckpointName != "mcp-root" {
		t.Fatalf("root MCP = %q", node.ManifestCheckpointName)
	}
	// Root no longer carries the orphan's dataRef nor a VSC->VS mapping.
	if len(node.DataBindings) != 0 {
		t.Fatalf("root DataBindings = %#v, want none", node.DataBindings)
	}
	if len(node.VSCToVS) != 0 {
		t.Fatalf("root VSCToVS = %#v, want none", node.VSCToVS)
	}
	if len(node.Children) != 2 {
		t.Fatalf("expected 2 children (orphan volume node + disk snapshot), got %d", len(node.Children))
	}

	var orphanNode, diskNode *RestoreNode
	for _, c := range node.Children {
		switch c.SnapshotRef.Kind {
		case "VolumeSnapshot":
			orphanNode = c
		case "DemoVirtualDiskSnapshot":
			diskNode = c
		}
	}
	if orphanNode == nil {
		t.Fatal("expected an orphan VolumeSnapshot child volume node")
	}
	if orphanNode.SnapshotRef.Name != "vs-orphan" {
		t.Fatalf("orphan node snapshotRef = %+v", orphanNode.SnapshotRef)
	}
	if orphanNode.ManifestCheckpointName != "mcp-orphan" {
		t.Fatalf("orphan node MCP = %q, want mcp-orphan", orphanNode.ManifestCheckpointName)
	}
	if len(orphanNode.DataBindings) != 1 || orphanNode.DataBindings[0].Artifact.Name != "vsc-orphan" {
		t.Fatalf("orphan node DataBindings = %#v", orphanNode.DataBindings)
	}
	if got := orphanNode.VSCToVS["vsc-orphan"]; got != "vs-orphan" {
		t.Fatalf("orphan node VSCToVS[vsc-orphan] = %q, want vs-orphan", got)
	}

	if diskNode == nil {
		t.Fatal("expected a disk child snapshot")
	}
	if diskNode.SnapshotRef.Name != "disk-snap" || diskNode.ManifestCheckpointName != "mcp-disk" {
		t.Fatalf("disk node = %+v mcp=%q", diskNode.SnapshotRef, diskNode.ManifestCheckpointName)
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

// TestResolveRestoreTree_OrphanVSMissingChildContentFailsClosed proves the Variant A orphan handle
// fails closed when its status.boundSnapshotContentName references a child volume SnapshotContent that
// does not exist: emitting the PVC without its manifest+dataRef node would silently lose data.
func TestResolveRestoreTree_OrphanVSMissingChildContentFailsClosed(t *testing.T) {
	scheme := restoreTreeScheme()
	root := rootSnapshotObj("snap", "root-content", []storagev1alpha1.SnapshotChildRef{
		{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshot", Name: "vs-orphan"},
	})
	rootContent := readySnapshotContent("root-content", "mcp-root", nil)
	// Handle points at a child content that was never materialized.
	vs := volumeSnapshotObj("vs-orphan", "vsc-orphan", "root-content-vol-missing")

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(root, rootContent, vs).Build()

	_, err := NewResolver(cl).ResolveRestoreTree(context.Background(), "source-ns", "snap")
	if err == nil {
		t.Fatal("expected error when the orphan child SnapshotContent is missing")
	}
	if !errors.Is(err, ErrContractViolation) {
		t.Fatalf("expected ErrContractViolation, got %v", err)
	}
}

// TestResolveRestoreTree_OrphanVSEmptyBoundContentFailsClosed proves an orphan VS handle that has not
// yet had status.boundSnapshotContentName published (child volume node not yet linked) is treated as
// not-ready rather than emitting a data-less PVC.
func TestResolveRestoreTree_OrphanVSEmptyBoundContentFailsClosed(t *testing.T) {
	scheme := restoreTreeScheme()
	root := rootSnapshotObj("snap", "root-content", []storagev1alpha1.SnapshotChildRef{
		{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshot", Name: "vs-orphan"},
	})
	rootContent := readySnapshotContent("root-content", "mcp-root", nil)
	// readyToUse + boundVSC set, but boundSnapshotContentName empty.
	vs := volumeSnapshotObj("vs-orphan", "vsc-orphan", "")

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(root, rootContent, vs).Build()

	_, err := NewResolver(cl).ResolveRestoreTree(context.Background(), "source-ns", "snap")
	if err == nil {
		t.Fatal("expected error when the orphan handle has empty boundSnapshotContentName")
	}
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("expected ErrNotReady, got %v", err)
	}
}

// TestResolveRestoreTree_OrphanContentSnapshotRefMismatchFailsClosed proves the anti-spoofing handshake:
// an orphan child SnapshotContent whose spec.snapshotRef does not point back at the VolumeSnapshot handle
// that bound it (status.boundSnapshotContentName) is rejected as a contract violation, even though every
// readiness/dataRef field is otherwise valid. This prevents binding a foreign content via a forged handle.
func TestResolveRestoreTree_OrphanContentSnapshotRefMismatchFailsClosed(t *testing.T) {
	scheme := restoreTreeScheme()
	root := rootSnapshotObj("snap", "root-content", []storagev1alpha1.SnapshotChildRef{
		{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshot", Name: "vs-orphan"},
	})
	rootContent := readySnapshotContent("root-content", "mcp-root", nil)
	// The child content's spec.snapshotRef points at a DIFFERENT VolumeSnapshot than the one binding it.
	orphanContent := orphanChildContent("root-content-vol-orphan", "mcp-orphan", "vs-other", &storagev1alpha1.SnapshotDataBinding{
		Source:   storagev1alpha1.SnapshotSubjectRef{APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "orphan-pvc", Namespace: "source-ns", UID: "uid-orphan"},
		Artifact: storagev1alpha1.SnapshotDataArtifactRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: "vsc-orphan"},
	})
	vs := volumeSnapshotObj("vs-orphan", "vsc-orphan", "root-content-vol-orphan")

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(root, rootContent, orphanContent, vs).Build()

	_, err := NewResolver(cl).ResolveRestoreTree(context.Background(), "source-ns", "snap")
	if err == nil {
		t.Fatal("expected error when the orphan child content snapshotRef does not point back at the VolumeSnapshot")
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
