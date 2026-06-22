package restore

import (
	"context"
	"testing"

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
		// Variant A: a SnapshotContent carries at most one dataRef (singular object, not a list).
		status["dataRef"] = dataRefs[0]
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
