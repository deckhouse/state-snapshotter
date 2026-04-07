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
			"name":        "data-pvc",
			"labels":      map[string]interface{}{"app": "demo"},
			"annotations": map[string]interface{}{"team": "storage"},
		},
		"spec": map[string]interface{}{
			"accessModes": []interface{}{"ReadWriteOnce"},
			"resources": map[string]interface{}{
				"requests": map[string]interface{}{
					"storage": "10Gi",
				},
			},
			"storageClassName": "standard",
			"volumeName":       "legacy-pv",
			"dataSource": map[string]interface{}{
				"kind": "VolumeSnapshot",
				"name": "snap-1",
			},
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
	pvcTemplate, _ := spec["pvcTemplate"].(map[string]interface{})
	metadata, _ := pvcTemplate["metadata"].(map[string]interface{})
	if metadata == nil || metadata["labels"] == nil || metadata["annotations"] == nil {
		t.Fatal("expected labels/annotations to be preserved in pvcTemplate metadata")
	}
	pvcSpec, _ := pvcTemplate["spec"].(map[string]interface{})
	if pvcSpec == nil {
		t.Fatal("expected pvcTemplate spec to be present")
	}
	if _, ok := pvcSpec["volumeName"]; ok {
		t.Fatal("expected volumeName to be removed from pvcTemplate spec")
	}
	if _, ok := pvcSpec["dataSource"]; ok {
		t.Fatal("expected dataSource to be removed from pvcTemplate spec")
	}
}

func TestTransform_PVCRequiresDataRef(t *testing.T) {
	pvc := unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]interface{}{
			"name": "data-pvc",
		},
	}}

	node := &SnapshotContentNode{
		Content: &unstructured.Unstructured{
			Object: map[string]interface{}{"metadata": map[string]interface{}{"uid": "uid-12345678"}},
		},
	}

	opts := Options{
		SnapshotName:      "snap-1",
		SnapshotNamespace: "default",
		RestoreStrategy:   "vrr",
	}

	_, err := NewTransformer().Transform([]unstructured.Unstructured{pvc}, opts, node)
	if err == nil {
		t.Fatal("expected error when dataRef is missing for PVC")
	}
}
