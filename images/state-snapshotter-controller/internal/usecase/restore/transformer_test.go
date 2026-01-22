package restore

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestTransform_ConvertsPVCToVRR(t *testing.T) {
	pvc := unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]interface{}{
			"name": "data-pvc",
		},
		"spec": map[string]interface{}{
			"accessModes": []interface{}{"ReadWriteOnce"},
			"resources": map[string]interface{}{
				"requests": map[string]interface{}{
					"storage": "10Gi",
				},
			},
			"storageClassName": "standard",
		},
	}}

	node := &SnapshotContentNode{
		Content: &unstructured.Unstructured{
			Object: map[string]interface{}{"metadata": map[string]interface{}{"uid": "uid-12345678"}},
		},
		DataRefKind: "VolumeSnapshotContent",
		DataRefName: "vsc-1",
	}

	opts := Options{
		SnapshotName:      "snap-1",
		SnapshotNamespace: "default",
		TargetNamespace:   "restore-ns",
		RestoreStrategy:   "vrr",
	}

	result, err := NewTransformer().Transform([]unstructured.Unstructured{pvc}, opts, node)
	if err != nil {
		t.Fatalf("Transform failed: %v", err)
	}
	if len(result.Objects) != 1 {
		t.Fatalf("expected 1 object, got %d", len(result.Objects))
	}
	obj := result.Objects[0]
	if obj.GetKind() != "VolumeRestoreRequest" {
		t.Fatalf("expected VRR, got %s", obj.GetKind())
	}
	if obj.GetNamespace() != "restore-ns" {
		t.Fatalf("expected namespace restore-ns, got %s", obj.GetNamespace())
	}
	if obj.GetName() == "" {
		t.Fatal("expected VRR name to be set")
	}
	spec, _ := obj.Object["spec"].(map[string]interface{})
	if spec == nil {
		t.Fatal("expected spec in VRR")
	}
	source, _ := spec["source"].(map[string]interface{})
	if source == nil || source["name"] != "vsc-1" {
		t.Fatalf("expected source name vsc-1, got %v", source)
	}
}
