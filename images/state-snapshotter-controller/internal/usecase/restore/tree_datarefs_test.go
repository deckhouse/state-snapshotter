package restore

import (
	"context"
	"strings"
	"testing"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestResolveSnapshotTree_ChildPreservesOwnDataBindings(t *testing.T) {
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(snapshotGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(contentGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: contentGVK.Group, Version: contentGVK.Version, Kind: contentGVK.Kind + "List"}, &unstructured.UnstructuredList{})

	snap := snapshotWithBoundContent("snap-1", "parent-content")
	parent := contentWithDataRefs("parent-content", "mcp-parent", []interface{}{
		dataRefsEntry("uid-parent", "pvc-parent", "vsc-parent"),
	}, []interface{}{
		map[string]interface{}{"name": "child-content", "kind": "SnapshotContent"},
	})
	child := contentWithDataRefs("child-content", "mcp-child", []interface{}{
		dataRefsEntry("uid-child", "pvc-child", "vsc-child"),
	}, nil)

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(snap, parent, child).Build()
	node, err := NewResolver(cl).ResolveSnapshotTree(context.Background(), "default", "snap-1")
	if err != nil {
		t.Fatalf("ResolveSnapshotTree: %v", err)
	}
	if len(node.DataBindings) != 1 || node.DataBindings[0].Artifact.Name != "vsc-parent" {
		t.Fatalf("parent bindings: %#v", node.DataBindings)
	}
	if len(node.Children) != 1 {
		t.Fatalf("expected one child, got %d", len(node.Children))
	}
	childNode := node.Children[0]
	if len(childNode.DataBindings) != 1 || childNode.DataBindings[0].Artifact.Name != "vsc-child" {
		t.Fatalf("child bindings: %#v", childNode.DataBindings)
	}
	if &node.DataBindings[0] == &childNode.DataBindings[0] {
		t.Fatal("parent and child must not share DataBindings slice")
	}
}

func TestTransform_TreeNodeUsesOwnDataBindingsOnly(t *testing.T) {
	parentPVC := pvcManifest("pvc-parent", "default", "uid-parent")
	parentPVC.SetUID("uid-parent")
	childPVC := pvcManifest("pvc-child", "default", "uid-child")
	childPVC.SetUID("uid-child")

	parentNode := &SnapshotContentNode{
		Content: contentObject("parent-content", "parent-uid"),
		DataBindings: []snapshot.DataBindingRef{
			dataBindingRef("uid-parent", "pvc-parent", "vsc-parent"),
		},
	}
	childNode := &SnapshotContentNode{
		Content: contentObject("child-content", "child-uid"),
		DataBindings: []snapshot.DataBindingRef{
			dataBindingRef("uid-child", "pvc-child", "vsc-child"),
		},
	}
	opts := Options{SnapshotNamespace: "default", RestoreStrategy: "vrr"}
	tr := NewTransformer()

	if _, err := tr.Transform([]unstructured.Unstructured{childPVC}, opts, parentNode); err == nil {
		t.Fatal("parent DataBindings must not match child MCP PVC")
	}
	if _, err := tr.Transform([]unstructured.Unstructured{parentPVC}, opts, childNode); err == nil {
		t.Fatal("child DataBindings must not match parent MCP PVC")
	}

	parentResult, err := tr.Transform([]unstructured.Unstructured{parentPVC}, opts, parentNode)
	if err != nil {
		t.Fatalf("parent transform: %v", err)
	}
	childResult, err := tr.Transform([]unstructured.Unstructured{childPVC}, opts, childNode)
	if err != nil {
		t.Fatalf("child transform: %v", err)
	}
	if sourceName(parentResult.Objects[0]) != "vsc-parent" {
		t.Fatalf("parent VRR source: %#v", parentResult.Objects[0].Object)
	}
	if sourceName(childResult.Objects[0]) != "vsc-child" {
		t.Fatalf("child VRR source: %#v", childResult.Objects[0].Object)
	}
}

func TestTransform_TwoPVCsTwoDataRefsDistinctVSCSources(t *testing.T) {
	pvcA := pvcManifest("data-a", "default", "uid-a")
	pvcA.SetUID("uid-a")
	pvcB := pvcManifest("data-b", "default", "uid-b")
	pvcB.SetUID("uid-b")

	node := &SnapshotContentNode{
		Content: contentObject("content-1", "content-uid-12345678"),
		DataBindings: []snapshot.DataBindingRef{
			dataBindingRef("uid-a", "data-a", "vsc-a"),
			dataBindingRef("uid-b", "data-b", "vsc-b"),
		},
	}
	opts := Options{SnapshotNamespace: "default", TargetNamespace: "restore-ns", RestoreStrategy: "vrr"}
	result, err := NewTransformer().Transform([]unstructured.Unstructured{pvcA, pvcB}, opts, node)
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}
	if len(result.Objects) != 2 {
		t.Fatalf("expected 2 VRR objects, got %d", len(result.Objects))
	}
	sources := map[string]struct{}{}
	for _, obj := range result.Objects {
		if obj.GetKind() != "VolumeRestoreRequest" {
			t.Fatalf("expected VRR, got %s", obj.GetKind())
		}
		name := sourceName(obj)
		if name == "" {
			t.Fatal("expected VRR spec.source.name")
		}
		if _, dup := sources[name]; dup {
			t.Fatalf("duplicate VSC source %q", name)
		}
		sources[name] = struct{}{}
	}
	if _, ok := sources["vsc-a"]; !ok {
		t.Fatalf("expected vsc-a source, got %v", sources)
	}
	if _, ok := sources["vsc-b"]; !ok {
		t.Fatalf("expected vsc-b source, got %v", sources)
	}
}

func TestTransform_MissingBindingFailClosedMessage(t *testing.T) {
	pvc := pvcManifest("data-a", "default", "uid-a")
	pvc.SetUID("uid-a")
	node := &SnapshotContentNode{
		Content: contentObject("content-1", "content-uid"),
		DataBindings: []snapshot.DataBindingRef{
			dataBindingRef("uid-b", "data-b", "vsc-b"),
		},
	}
	_, err := NewTransformer().Transform([]unstructured.Unstructured{pvc}, Options{RestoreStrategy: "vrr"}, node)
	if err == nil {
		t.Fatal("expected error for missing binding")
	}
	msg := err.Error()
	for _, want := range []string{"default", "data-a", "uid-a"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("expected error to contain %q, got %q", want, msg)
		}
	}
}

func snapshotWithBoundContent(name, contentName string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "state-snapshotter.deckhouse.io/v1alpha1",
		"kind":       "Snapshot",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": "default",
		},
		"status": map[string]interface{}{
			"boundSnapshotContentName": contentName,
			"conditions": []interface{}{
				map[string]interface{}{"type": "Ready", "status": "True"},
			},
		},
	}}
}

func contentWithDataRefs(name, mcp string, dataRefs, children []interface{}) *unstructured.Unstructured {
	status := map[string]interface{}{
		"manifestCheckpointName": mcp,
		"conditions": []interface{}{
			map[string]interface{}{"type": "Ready", "status": "True"},
		},
	}
	if len(dataRefs) > 0 {
		status["dataRefs"] = dataRefs
	}
	if len(children) > 0 {
		status["childrenSnapshotContentRefs"] = children
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "state-snapshotter.deckhouse.io/v1alpha1",
		"kind":       "SnapshotContent",
		"metadata":   map[string]interface{}{"name": name},
		"status":     status,
	}}
}

func dataRefsEntry(targetUID, pvcName, vscName string) map[string]interface{} {
	return map[string]interface{}{
		"targetUID": targetUID,
		"target": map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "PersistentVolumeClaim",
			"namespace":  "default",
			"name":       pvcName,
			"uid":        targetUID,
		},
		"artifact": map[string]interface{}{
			"apiVersion": "snapshot.storage.k8s.io/v1",
			"kind":       "VolumeSnapshotContent",
			"name":       vscName,
		},
	}
}

func contentObject(name, uid string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{
			"name": name,
			"uid":  uid,
		},
	}}
}

func sourceName(obj unstructured.Unstructured) string {
	spec, _ := obj.Object["spec"].(map[string]interface{})
	source, _ := spec["source"].(map[string]interface{})
	if source == nil {
		return ""
	}
	name, _ := source["name"].(string)
	return name
}
