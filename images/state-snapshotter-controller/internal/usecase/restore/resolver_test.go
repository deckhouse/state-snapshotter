package restore

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var (
	snapshotGVK = schema.GroupVersionKind{Group: "state-snapshotter.deckhouse.io", Version: "v1alpha1", Kind: "Snapshot"}
	contentGVK  = schema.GroupVersionKind{Group: "state-snapshotter.deckhouse.io", Version: "v1alpha1", Kind: "SnapshotContent"}
)

func TestResolveSnapshotTree_PrimaryPath(t *testing.T) {
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(snapshotGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(contentGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: contentGVK.Group, Version: contentGVK.Version, Kind: contentGVK.Kind + "List"}, &unstructured.UnstructuredList{})

	snapshot := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "state-snapshotter.deckhouse.io/v1alpha1",
		"kind":       "Snapshot",
		"metadata": map[string]interface{}{
			"name":      "snap-1",
			"namespace": "default",
		},
		"status": map[string]interface{}{
			"boundSnapshotContentName": "content-1",
			"conditions": []interface{}{
				map[string]interface{}{
					"type":   "Ready",
					"status": "True",
				},
			},
		},
	}}

	content := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "state-snapshotter.deckhouse.io/v1alpha1",
		"kind":       "SnapshotContent",
		"metadata": map[string]interface{}{
			"name": "content-1",
		},
		"status": map[string]interface{}{
			"manifestCheckpointName": "mcp-1",
			"conditions": []interface{}{
				map[string]interface{}{
					"type":   "Ready",
					"status": "True",
				},
			},
		},
	}}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(snapshot, content).Build()
	resolver := NewResolver(client)

	node, err := resolver.ResolveSnapshotTree(context.Background(), "default", "snap-1")
	if err != nil {
		t.Fatalf("ResolveSnapshotTree failed: %v", err)
	}
	if node.ManifestCheckpointName != "mcp-1" {
		t.Fatalf("expected manifestCheckpointName mcp-1, got %s", node.ManifestCheckpointName)
	}
}

func TestResolveSnapshotTree_ContentNotReady(t *testing.T) {
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(snapshotGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(contentGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: contentGVK.Group, Version: contentGVK.Version, Kind: contentGVK.Kind + "List"}, &unstructured.UnstructuredList{})

	snapshot := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "state-snapshotter.deckhouse.io/v1alpha1",
		"kind":       "Snapshot",
		"metadata": map[string]interface{}{
			"name":      "snap-1",
			"namespace": "default",
		},
		"status": map[string]interface{}{
			"boundSnapshotContentName": "content-1",
			"conditions": []interface{}{
				map[string]interface{}{
					"type":   "Ready",
					"status": "True",
				},
			},
		},
	}}

	content := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "state-snapshotter.deckhouse.io/v1alpha1",
		"kind":       "SnapshotContent",
		"metadata": map[string]interface{}{
			"name": "content-1",
		},
		"status": map[string]interface{}{
			"manifestCheckpointName": "mcp-1",
			"conditions": []interface{}{
				map[string]interface{}{
					"type":   "Ready",
					"status": "False",
				},
			},
		},
	}}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(snapshot, content).Build()
	resolver := NewResolver(client)

	_, err := resolver.ResolveSnapshotTree(context.Background(), "default", "snap-1")
	if err == nil {
		t.Fatal("expected error for not ready SnapshotContent")
	}
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("expected ErrNotReady, got %v", err)
	}
}

func TestResolveSnapshotTree_FallbackBySnapshotRef(t *testing.T) {
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(snapshotGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(contentGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: contentGVK.Group, Version: contentGVK.Version, Kind: contentGVK.Kind + "List"}, &unstructured.UnstructuredList{})

	snapshot := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "state-snapshotter.deckhouse.io/v1alpha1",
		"kind":       "Snapshot",
		"metadata": map[string]interface{}{
			"name":      "snap-1",
			"namespace": "default",
		},
		"status": map[string]interface{}{
			"conditions": []interface{}{
				map[string]interface{}{
					"type":   "Ready",
					"status": "True",
				},
			},
		},
	}}

	content := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "state-snapshotter.deckhouse.io/v1alpha1",
		"kind":       "SnapshotContent",
		"metadata": map[string]interface{}{
			"name": "content-1",
		},
		"spec": map[string]interface{}{
			"snapshotRef": map[string]interface{}{
				"name":      "snap-1",
				"namespace": "default",
			},
		},
		"status": map[string]interface{}{
			"manifestCheckpointName": "mcp-1",
			"conditions": []interface{}{
				map[string]interface{}{
					"type":   "Ready",
					"status": "True",
				},
			},
		},
	}}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(snapshot, content).Build()
	resolver := NewResolver(client)

	node, err := resolver.ResolveSnapshotTree(context.Background(), "default", "snap-1")
	if err != nil {
		t.Fatalf("ResolveSnapshotTree failed: %v", err)
	}
	if node.Content.GetName() != "content-1" {
		t.Fatalf("expected content-1, got %s", node.Content.GetName())
	}
}

func TestResolveSnapshotTree_FallbackNotFoundIsNotReady(t *testing.T) {
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(snapshotGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(contentGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: contentGVK.Group, Version: contentGVK.Version, Kind: contentGVK.Kind + "List"}, &unstructured.UnstructuredList{})

	snapshot := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "state-snapshotter.deckhouse.io/v1alpha1",
		"kind":       "Snapshot",
		"metadata": map[string]interface{}{
			"name":      "snap-1",
			"namespace": "default",
		},
		"status": map[string]interface{}{
			"conditions": []interface{}{
				map[string]interface{}{
					"type":   "Ready",
					"status": "True",
				},
			},
		},
	}}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(snapshot).Build()
	resolver := NewResolver(client)

	_, err := resolver.ResolveSnapshotTree(context.Background(), "default", "snap-1")
	if err == nil {
		t.Fatal("expected not ready error when content is missing")
	}
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("expected ErrNotReady, got %v", err)
	}
}

func TestResolveSnapshotTree_ChildMissing(t *testing.T) {
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(snapshotGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(contentGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: contentGVK.Group, Version: contentGVK.Version, Kind: contentGVK.Kind + "List"}, &unstructured.UnstructuredList{})

	snapshot := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "state-snapshotter.deckhouse.io/v1alpha1",
		"kind":       "Snapshot",
		"metadata": map[string]interface{}{
			"name":      "snap-1",
			"namespace": "default",
		},
		"status": map[string]interface{}{
			"boundSnapshotContentName": "content-1",
			"conditions": []interface{}{
				map[string]interface{}{
					"type":   "Ready",
					"status": "True",
				},
			},
		},
	}}

	content := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "state-snapshotter.deckhouse.io/v1alpha1",
		"kind":       "SnapshotContent",
		"metadata": map[string]interface{}{
			"name": "content-1",
		},
		"status": map[string]interface{}{
			"manifestCheckpointName": "mcp-1",
			"childrenSnapshotContentRefs": []interface{}{
				map[string]interface{}{
					"name": "child-1",
					"kind": "SnapshotContent",
				},
			},
			"conditions": []interface{}{
				map[string]interface{}{
					"type":   "Ready",
					"status": "True",
				},
			},
		},
	}}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(snapshot, content).Build()
	resolver := NewResolver(client)

	_, err := resolver.ResolveSnapshotTree(context.Background(), "default", "snap-1")
	if err == nil {
		t.Fatal("expected error for missing child SnapshotContent")
	}
	if !errors.Is(err, ErrContractViolation) {
		t.Fatalf("expected ErrContractViolation, got %v", err)
	}
}

func TestResolveSnapshotTree_MultipleSnapshotRefMatches(t *testing.T) {
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(snapshotGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(contentGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: contentGVK.Group, Version: contentGVK.Version, Kind: contentGVK.Kind + "List"}, &unstructured.UnstructuredList{})

	snapshot := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "state-snapshotter.deckhouse.io/v1alpha1",
		"kind":       "Snapshot",
		"metadata": map[string]interface{}{
			"name":      "snap-1",
			"namespace": "default",
		},
		"status": map[string]interface{}{
			"conditions": []interface{}{
				map[string]interface{}{
					"type":   "Ready",
					"status": "True",
				},
			},
		},
	}}

	content1 := newContentWithRef("content-1")
	content2 := newContentWithRef("content-2")

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(snapshot, content1, content2).Build()
	resolver := NewResolver(client)

	_, err := resolver.ResolveSnapshotTree(context.Background(), "default", "snap-1")
	if err == nil {
		t.Fatal("expected error for multiple SnapshotContent matches")
	}
	if !errors.Is(err, ErrContractViolation) {
		t.Fatalf("expected ErrContractViolation, got %v", err)
	}
}

func newContentWithRef(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "state-snapshotter.deckhouse.io/v1alpha1",
		"kind":       "SnapshotContent",
		"metadata": map[string]interface{}{
			"name": name,
		},
		"spec": map[string]interface{}{
			"snapshotRef": map[string]interface{}{
				"name":      "snap-1",
				"namespace": "default",
			},
		},
		"status": map[string]interface{}{
			"manifestCheckpointName": "mcp-1",
			"conditions": []interface{}{
				map[string]interface{}{
					"type":   "Ready",
					"status": string(metav1.ConditionTrue),
				},
			},
		},
	}}
}
