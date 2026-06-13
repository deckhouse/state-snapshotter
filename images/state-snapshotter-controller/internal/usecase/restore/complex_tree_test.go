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
	"encoding/json"
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

// Complex-tree harness: build arbitrarily large/deep synthetic snapshot run-trees (Snapshot ->
// childrenSnapshotRefs -> domain snapshot CRs -> ... + orphan-PVC VolumeSnapshot leaves), back every
// node with a real ManifestCheckpoint, and run the actual restore compiler against a fake client.
//
// This validates the GENERIC algorithm (traversal, post-order output, covered-PVC suppression, orphan
// PVC resolution at any depth, dedup, bottom-up child propagation, fail-closed) without a cluster.
// Domain-specific field rewrites (e.g. demo disk dataSource) are exercised through a stub transformer
// that mirrors the demo logic, since the demo package cannot be imported here (it imports restore).

const demoGroupV = "demo.state-snapshotter.deckhouse.io/v1alpha1"

// orphanLeaf is a namespace-residual PVC captured at a node via a CSI VolumeSnapshot visibility leaf.
type orphanLeaf struct {
	pvcName string
	pvcUID  string
	vsName  string
	vscName string
}

// treeNode declares one snapshot run-tree node. kind=="" marks the root (storage Snapshot); otherwise
// it is a domain snapshot CR kind (e.g. DemoVirtualDiskSnapshot). manifests are the objects captured
// at this node (namespace "source-ns").
type treeNode struct {
	kind      string
	name      string
	manifests []map[string]interface{}
	orphans   []orphanLeaf
	notReady  bool
	children  []treeNode
}

func complexTreeScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = ssv1alpha1.AddToScheme(scheme)
	_ = storagev1alpha1.AddToScheme(scheme)
	for _, k := range []string{"DemoVirtualMachineSnapshot", "DemoVirtualDiskSnapshot"} {
		gvk := schema.GroupVersionKind{Group: "demo.state-snapshotter.deckhouse.io", Version: "v1alpha1", Kind: k}
		scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: gvk.Group, Version: gvk.Version, Kind: k + "List"}, &unstructured.UnstructuredList{})
	}
	vsGVK := schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshot"}
	scheme.AddKnownTypeWithName(vsGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: vsGVK.Group, Version: vsGVK.Version, Kind: "VolumeSnapshotList"}, &unstructured.UnstructuredList{})
	return scheme
}

func cmManifest(name string) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]interface{}{"name": name, "namespace": "source-ns"},
		"data":     map[string]interface{}{"k": "v"},
	}
}

func pvcManifestRaw(name, uid string) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "v1", "kind": "PersistentVolumeClaim",
		"metadata": map[string]interface{}{"name": name, "namespace": "source-ns", "uid": uid},
		"spec":     map[string]interface{}{"accessModes": []interface{}{"ReadWriteOnce"}},
	}
}

func diskManifest(name, coveredPVC string) map[string]interface{} {
	spec := map[string]interface{}{}
	if coveredPVC != "" {
		spec["persistentVolumeClaimName"] = coveredPVC
	}
	return map[string]interface{}{
		"apiVersion": demoGroupV, "kind": "DemoVirtualDisk",
		"metadata": map[string]interface{}{"name": name, "namespace": "source-ns"},
		"spec":     spec,
	}
}

func vmManifest(name, diskName string) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": demoGroupV, "kind": "DemoVirtualMachine",
		"metadata": map[string]interface{}{"name": name, "namespace": "source-ns"},
		"spec":     map[string]interface{}{"virtualDiskName": diskName},
	}
}

func domainSnapshotObj(kind, name, content string, childRefs []storagev1alpha1.SnapshotChildRef, ready bool) *unstructured.Unstructured {
	refs := make([]interface{}, 0, len(childRefs))
	for _, r := range childRefs {
		refs = append(refs, map[string]interface{}{"apiVersion": r.APIVersion, "kind": r.Kind, "name": r.Name})
	}
	readyStatus := "True"
	if !ready {
		readyStatus = "False"
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": demoGroupV,
		"kind":       kind,
		"metadata":   map[string]interface{}{"name": name, "namespace": "source-ns"},
		"status": map[string]interface{}{
			"boundSnapshotContentName": content,
			"conditions":               []interface{}{map[string]interface{}{"type": "Ready", "status": readyStatus}},
			"childrenSnapshotRefs":     refs,
		},
	}}
}

// collectTree materializes one node (MCP + chunk + SnapshotContent + snapshot CR + VS leaves) and
// recurses into children, appending every object to objs.
func collectTree(t *testing.T, n treeNode, isRoot bool, objs *[]client.Object) {
	t.Helper()
	content := n.name + "-content"
	mcp := "mcp-" + n.name

	data, checksum := encodeChunk(n.manifests)
	*objs = append(*objs, &ssv1alpha1.ManifestCheckpointContentChunk{
		ObjectMeta: metav1.ObjectMeta{Name: mcp + "-chunk-0"},
		Spec: ssv1alpha1.ManifestCheckpointContentChunkSpec{
			CheckpointName: mcp, Index: 0, Data: data, Checksum: checksum, ObjectsCount: len(n.manifests),
		},
	})
	mcpObj := &ssv1alpha1.ManifestCheckpoint{
		ObjectMeta: metav1.ObjectMeta{Name: mcp, UID: types.UID("uid-" + mcp)},
		Spec:       ssv1alpha1.ManifestCheckpointSpec{SourceNamespace: "source-ns"},
		Status: ssv1alpha1.ManifestCheckpointStatus{
			Chunks:       []ssv1alpha1.ChunkInfo{{Name: mcp + "-chunk-0", Index: 0, Checksum: checksum}},
			TotalObjects: len(n.manifests),
		},
	}
	meta.SetStatusCondition(&mcpObj.Status.Conditions, metav1.Condition{
		Type: ssv1alpha1.ManifestCheckpointConditionTypeReady, Status: metav1.ConditionTrue,
		Reason: ssv1alpha1.ManifestCheckpointConditionReasonCompleted,
	})
	*objs = append(*objs, mcpObj)

	var dataRefs []storagev1alpha1.SnapshotDataBinding
	for _, o := range n.orphans {
		dataRefs = append(dataRefs, storagev1alpha1.SnapshotDataBinding{
			TargetUID: o.pvcUID,
			Target:    storagev1alpha1.SnapshotSubjectRef{APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: o.pvcName, Namespace: "source-ns", UID: types.UID(o.pvcUID)},
			Artifact:  storagev1alpha1.SnapshotDataArtifactRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: o.vscName},
		})
	}
	contentObj := readySnapshotContent(content, mcp, dataRefs)
	if n.notReady {
		meta.SetStatusCondition(&contentObj.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "Pending"})
	}
	*objs = append(*objs, contentObj)

	for _, o := range n.orphans {
		*objs = append(*objs, volumeSnapshotObj(o.vsName, o.vscName))
	}

	var childRefs []storagev1alpha1.SnapshotChildRef
	for _, o := range n.orphans {
		childRefs = append(childRefs, storagev1alpha1.SnapshotChildRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshot", Name: o.vsName})
	}
	for _, c := range n.children {
		childRefs = append(childRefs, storagev1alpha1.SnapshotChildRef{APIVersion: demoGroupV, Kind: c.kind, Name: c.name})
	}

	if isRoot {
		*objs = append(*objs, rootSnapshotObj(n.name, content, childRefs))
	} else {
		*objs = append(*objs, domainSnapshotObj(n.kind, n.name, content, childRefs, !n.notReady))
	}

	for _, c := range n.children {
		collectTree(t, c, false, objs)
	}
}

func buildComplexService(t *testing.T, root treeNode) *Service {
	t.Helper()
	var objs []client.Object
	collectTree(t, root, true, &objs)
	cl := fake.NewClientBuilder().WithScheme(complexTreeScheme()).WithObjects(objs...).Build()
	log, _ := logger.NewLogger("error")
	arch := usecase.NewArchiveService(cl, cl, log)
	return NewService(cl, arch, &diskRestoreStub{}, &vmRestoreStub{})
}

// diskRestoreStub mirrors the demo DemoVirtualDisk transform without importing the demo package.
type diskRestoreStub struct{}

func (diskRestoreStub) CoveredPVCNames(_ *RestoreNode, objects []unstructured.Unstructured) map[string]struct{} {
	covered := map[string]struct{}{}
	for i := range objects {
		if objects[i].GetKind() != "DemoVirtualDisk" {
			continue
		}
		if pvc, _, _ := unstructured.NestedString(objects[i].Object, "spec", "persistentVolumeClaimName"); pvc != "" {
			covered[pvc] = struct{}{}
		}
	}
	return covered
}

func (diskRestoreStub) TransformObject(node *RestoreNode, obj *unstructured.Unstructured, _ []NodeResult) (bool, error) {
	if obj.GetKind() != "DemoVirtualDisk" || node == nil || node.SnapshotRef.Kind != "DemoVirtualDiskSnapshot" {
		return false, nil
	}
	_ = unstructured.SetNestedMap(obj.Object, map[string]interface{}{
		"apiGroup": "demo.state-snapshotter.deckhouse.io", "kind": "DemoVirtualDiskSnapshot", "name": node.SnapshotRef.Name,
	}, "spec", "dataSource")
	return true, nil
}

// vmRestoreStub records whether a parent VM transform sees its already-restored disk children, proving
// the compiler threads bottom-up child results into the parent transform.
type vmRestoreStub struct {
	sawRestoredDiskChild bool
}

func (vmRestoreStub) CoveredPVCNames(_ *RestoreNode, _ []unstructured.Unstructured) map[string]struct{} {
	return nil
}

func (s *vmRestoreStub) TransformObject(_ *RestoreNode, obj *unstructured.Unstructured, children []NodeResult) (bool, error) {
	if obj.GetKind() != "DemoVirtualMachine" {
		return false, nil
	}
	for _, c := range children {
		for i := range c.Objects {
			if c.Objects[i].GetKind() == "DemoVirtualDisk" {
				s.sawRestoredDiskChild = true
			}
		}
	}
	return true, nil
}

func decodeObjects(t *testing.T, raw []byte) []map[string]interface{} {
	t.Helper()
	var objects []map[string]interface{}
	if err := json.Unmarshal(raw, &objects); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	return objects
}

func idxOf(objs []map[string]interface{}, kind, name string) int {
	for i, o := range objs {
		u := unstructured.Unstructured{Object: o}
		if u.GetKind() == kind && u.GetName() == name {
			return i
		}
	}
	return -1
}

func hasObj(objs []map[string]interface{}, kind, name string) bool {
	return idxOf(objs, kind, name) >= 0
}

func dataSourceName(objs []map[string]interface{}, kind, name string) string {
	i := idxOf(objs, kind, name)
	if i < 0 {
		return ""
	}
	v, _, _ := unstructured.NestedString(objs[i], "spec", "dataSource", "name")
	return v
}

// TestBuildManifestsWithDataRestoration_ComplexTree compiles a deep, multi-branch tree:
//
//	app (root)                       cm "config" + orphan PVC "shared-pvc"
//	├── vm-a-snap (VMSnapshot)       vm "vm-a"
//	│   ├── disk-a1-snap (DiskSnap)  disk "disk-a1" (covers pvc-a1) + pvc-a1
//	│   │   └── disk-a1-nested-snap  disk "disk-a1-nested" + orphan PVC "pvc-nested"
//	│   └── disk-a2-snap (DiskSnap)  disk "disk-a2" (covers pvc-a2) + pvc-a2
//	└── disk-standalone-snap         disk "disk-standalone"
func TestBuildManifestsWithDataRestoration_ComplexTree(t *testing.T) {
	vmStub := &vmRestoreStub{}
	root := treeNode{
		name:      "app",
		manifests: []map[string]interface{}{cmManifest("config"), pvcManifestRaw("shared-pvc", "uid-shared")},
		orphans:   []orphanLeaf{{pvcName: "shared-pvc", pvcUID: "uid-shared", vsName: "vs-shared", vscName: "vsc-shared"}},
		children: []treeNode{
			{
				kind: "DemoVirtualMachineSnapshot", name: "vm-a-snap",
				manifests: []map[string]interface{}{vmManifest("vm-a", "disk-a1")},
				children: []treeNode{
					{
						kind: "DemoVirtualDiskSnapshot", name: "disk-a1-snap",
						manifests: []map[string]interface{}{diskManifest("disk-a1", "pvc-a1"), pvcManifestRaw("pvc-a1", "uid-a1")},
						children: []treeNode{
							{
								kind: "DemoVirtualDiskSnapshot", name: "disk-a1-nested-snap",
								manifests: []map[string]interface{}{diskManifest("disk-a1-nested", ""), pvcManifestRaw("pvc-nested", "uid-nested")},
								orphans:   []orphanLeaf{{pvcName: "pvc-nested", pvcUID: "uid-nested", vsName: "vs-nested", vscName: "vsc-nested"}},
							},
						},
					},
					{
						kind: "DemoVirtualDiskSnapshot", name: "disk-a2-snap",
						manifests: []map[string]interface{}{diskManifest("disk-a2", "pvc-a2"), pvcManifestRaw("pvc-a2", "uid-a2")},
					},
				},
			},
			{
				kind: "DemoVirtualDiskSnapshot", name: "disk-standalone-snap",
				manifests: []map[string]interface{}{diskManifest("disk-standalone", "")},
			},
		},
	}

	var objs []client.Object
	collectTree(t, root, true, &objs)
	cl := fake.NewClientBuilder().WithScheme(complexTreeScheme()).WithObjects(objs...).Build()
	log, _ := logger.NewLogger("error")
	svc := NewService(cl, usecase.NewArchiveService(cl, cl, log), &diskRestoreStub{}, vmStub)

	out, err := svc.BuildManifestsWithDataRestoration(context.Background(), Options{
		SnapshotName: "app", SnapshotNamespace: "source-ns", TargetNamespace: "restore-ns",
	})
	if err != nil {
		t.Fatalf("BuildManifestsWithDataRestoration: %v", err)
	}
	objects := decodeObjects(t, out)

	// All emitted objects (and only application objects) in the target namespace.
	for _, o := range objects {
		u := unstructured.Unstructured{Object: o}
		switch u.GetKind() {
		case "VolumeSnapshot", "VolumeSnapshotContent", "VolumeRestoreRequest", "Snapshot", "SnapshotContent":
			t.Fatalf("control-plane kind %s must not be emitted", u.GetKind())
		}
		if u.GetNamespace() != "restore-ns" {
			t.Fatalf("%s/%s namespace = %q, want restore-ns", u.GetKind(), u.GetName(), u.GetNamespace())
		}
	}

	// Covered PVCs are suppressed; orphan PVCs are emitted with a dataSourceRef to their VS.
	for _, covered := range []string{"pvc-a1", "pvc-a2"} {
		if hasObj(objects, "PersistentVolumeClaim", covered) {
			t.Fatalf("covered PVC %s must not be emitted standalone", covered)
		}
	}
	for _, o := range []struct{ pvc, vs string }{{"shared-pvc", "vs-shared"}, {"pvc-nested", "vs-nested"}} {
		i := idxOf(objects, "PersistentVolumeClaim", o.pvc)
		if i < 0 {
			t.Fatalf("orphan PVC %s missing from output", o.pvc)
		}
		name, _, _ := unstructured.NestedString(objects[i], "spec", "dataSourceRef", "name")
		kind, _, _ := unstructured.NestedString(objects[i], "spec", "dataSourceRef", "kind")
		if kind != "VolumeSnapshot" || name != o.vs {
			t.Fatalf("orphan PVC %s dataSourceRef = %s/%s, want VolumeSnapshot/%s", o.pvc, kind, name, o.vs)
		}
	}

	// Every disk points at its OWN snapshot node identity (not the disk object name).
	for _, d := range []struct{ disk, snap string }{
		{"disk-a1", "disk-a1-snap"},
		{"disk-a1-nested", "disk-a1-nested-snap"},
		{"disk-a2", "disk-a2-snap"},
		{"disk-standalone", "disk-standalone-snap"},
	} {
		if got := dataSourceName(objects, "DemoVirtualDisk", d.disk); got != d.snap {
			t.Fatalf("disk %s dataSource.name = %q, want %q", d.disk, got, d.snap)
		}
	}

	// Post-order output: descendants strictly precede their parent node's objects.
	mustBefore := func(childKind, childName, parentKind, parentName string) {
		ci, pi := idxOf(objects, childKind, childName), idxOf(objects, parentKind, parentName)
		if ci < 0 || pi < 0 || ci >= pi {
			t.Fatalf("post-order violation: %s/%s (idx %d) must precede %s/%s (idx %d)", childKind, childName, ci, parentKind, parentName, pi)
		}
	}
	mustBefore("DemoVirtualDisk", "disk-a1-nested", "DemoVirtualDisk", "disk-a1") // nested disk before its parent disk
	mustBefore("DemoVirtualDisk", "disk-a1", "DemoVirtualMachine", "vm-a")        // disk before its VM
	mustBefore("DemoVirtualDisk", "disk-a2", "DemoVirtualMachine", "vm-a")
	mustBefore("DemoVirtualMachine", "vm-a", "ConfigMap", "config")         // VM subtree before root objects
	mustBefore("DemoVirtualDisk", "disk-standalone", "ConfigMap", "config") // sibling subtree before root objects

	// Bottom-up: the VM parent transform saw its already-restored disk children.
	if !vmStub.sawRestoredDiskChild {
		t.Fatal("VM transform did not receive restored disk children (bottom-up propagation broken)")
	}

	// Exactly the expected application objects, no duplicates, no leaks.
	if len(objects) != 8 {
		names := make([]string, 0, len(objects))
		for _, o := range objects {
			u := unstructured.Unstructured{Object: o}
			names = append(names, u.GetKind()+"/"+u.GetName())
		}
		t.Fatalf("expected 8 objects, got %d: %v", len(objects), names)
	}
}

// TestBuildManifestsWithDataRestorationForNode_VMSubtree proves the per-node restore endpoint
// compiles only the subtree rooted at the given snapshot node (here the VM), not the whole namespace.
func TestBuildManifestsWithDataRestorationForNode_VMSubtree(t *testing.T) {
	root := treeNode{
		name:      "app",
		manifests: []map[string]interface{}{cmManifest("config")},
		children: []treeNode{
			{
				kind: "DemoVirtualMachineSnapshot", name: "vm-a-snap",
				manifests: []map[string]interface{}{vmManifest("vm-a", "disk-a1")},
				children: []treeNode{
					{
						kind: "DemoVirtualDiskSnapshot", name: "disk-a1-snap",
						manifests: []map[string]interface{}{diskManifest("disk-a1", "pvc-a1"), pvcManifestRaw("pvc-a1", "uid-a1")},
					},
				},
			},
			{
				kind: "DemoVirtualDiskSnapshot", name: "disk-standalone-snap",
				manifests: []map[string]interface{}{diskManifest("disk-standalone", "")},
			},
		},
	}
	svc := buildComplexService(t, root)

	gvk := schema.GroupVersionKind{Group: "demo.state-snapshotter.deckhouse.io", Version: "v1alpha1", Kind: "DemoVirtualMachineSnapshot"}
	out, err := svc.BuildManifestsWithDataRestorationForNode(context.Background(), gvk, Options{
		SnapshotName: "vm-a-snap", SnapshotNamespace: "source-ns", TargetNamespace: "restore-ns",
	})
	if err != nil {
		t.Fatalf("BuildManifestsWithDataRestorationForNode: %v", err)
	}
	objects := decodeObjects(t, out)

	if !hasObj(objects, "DemoVirtualMachine", "vm-a") {
		t.Fatal("VM subtree must contain vm-a")
	}
	if got := dataSourceName(objects, "DemoVirtualDisk", "disk-a1"); got != "disk-a1-snap" {
		t.Fatalf("disk-a1 dataSource.name = %q, want disk-a1-snap", got)
	}
	// Nodes outside the VM subtree must NOT appear.
	if hasObj(objects, "DemoVirtualDisk", "disk-standalone") {
		t.Fatal("disk-standalone is a sibling subtree and must not be compiled for the VM node")
	}
	if hasObj(objects, "ConfigMap", "config") {
		t.Fatal("root namespace objects must not be compiled for the VM node")
	}
	if hasObj(objects, "PersistentVolumeClaim", "pvc-a1") {
		t.Fatal("covered PVC must be suppressed")
	}
}

// TestBuildManifestsWithDataRestoration_DuplicateAcrossBranchesFailsClosed proves the final dedup
// rejects two sibling subtrees that compile the same object identity (apiVersion|kind|ns|name).
func TestBuildManifestsWithDataRestoration_DuplicateAcrossBranchesFailsClosed(t *testing.T) {
	root := treeNode{
		name:      "app",
		manifests: []map[string]interface{}{cmManifest("root-cm")},
		children: []treeNode{
			{kind: "DemoVirtualDiskSnapshot", name: "disk-x-snap", manifests: []map[string]interface{}{cmManifest("dup")}},
			{kind: "DemoVirtualDiskSnapshot", name: "disk-y-snap", manifests: []map[string]interface{}{cmManifest("dup")}},
		},
	}
	svc := buildComplexService(t, root)
	_, err := svc.BuildManifestsWithDataRestoration(context.Background(), Options{
		SnapshotName: "app", SnapshotNamespace: "source-ns", TargetNamespace: "restore-ns",
	})
	if err == nil {
		t.Fatal("expected duplicate object across branches to fail closed")
	}
	if !errors.Is(err, ErrContractViolation) {
		t.Fatalf("expected ErrContractViolation, got %v", err)
	}
}

// TestBuildManifestsWithDataRestoration_DeepNotReadyFailsClosed proves a single not-Ready content deep
// in the tree aborts the whole compilation (no partial apply-ready output).
func TestBuildManifestsWithDataRestoration_DeepNotReadyFailsClosed(t *testing.T) {
	root := treeNode{
		name:      "app",
		manifests: []map[string]interface{}{cmManifest("config")},
		children: []treeNode{{
			kind: "DemoVirtualMachineSnapshot", name: "vm-a-snap", manifests: []map[string]interface{}{vmManifest("vm-a", "disk-a1")},
			children: []treeNode{{
				kind: "DemoVirtualDiskSnapshot", name: "disk-a1-snap", manifests: []map[string]interface{}{diskManifest("disk-a1", "")},
				notReady: true, // deep node not Ready
			}},
		}},
	}
	svc := buildComplexService(t, root)
	_, err := svc.BuildManifestsWithDataRestoration(context.Background(), Options{
		SnapshotName: "app", SnapshotNamespace: "source-ns", TargetNamespace: "restore-ns",
	})
	if err == nil {
		t.Fatal("expected deep not-Ready content to fail closed")
	}
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("expected ErrNotReady, got %v", err)
	}
}
