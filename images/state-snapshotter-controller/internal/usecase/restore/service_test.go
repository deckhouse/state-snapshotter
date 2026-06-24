package restore

import (
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestMarshalObjects_Duplicate(t *testing.T) {
	obj := unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]interface{}{
			"name":      "cm-1",
			"namespace": "default",
		},
	}}

	_, err := marshalObjects([]unstructured.Unstructured{obj, obj})
	if err == nil {
		t.Fatal("expected duplicate error")
	}
	if !errors.Is(err, ErrContractViolation) {
		t.Fatalf("expected ErrContractViolation, got %v", err)
	}
}
