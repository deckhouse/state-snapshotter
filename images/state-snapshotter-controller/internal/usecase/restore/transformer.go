package restore

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	restoreStrategyVRR = "vrr"
	pvcKind            = "PersistentVolumeClaim"
)

type Transformer struct{}

func NewTransformer() *Transformer {
	return &Transformer{}
}

func (t *Transformer) Transform(objects []unstructured.Unstructured, opts Options, node *SnapshotContentNode) (TransformResult, error) {
	if opts.RestoreStrategy == "" {
		opts.RestoreStrategy = restoreStrategyVRR
	}
	if opts.RestoreStrategy != restoreStrategyVRR {
		return TransformResult{}, fmt.Errorf("restoreStrategy %q is not supported in MVP", opts.RestoreStrategy)
	}

	targetNamespace := opts.TargetNamespace
	if targetNamespace == "" {
		targetNamespace = opts.SnapshotNamespace
	}

	var output []unstructured.Unstructured
	for _, obj := range objects {
		if obj.GetKind() != pvcKind || obj.GetAPIVersion() != "v1" {
			output = append(output, obj)
			continue
		}
		if node.DataRefKind == "" || node.DataRefName == "" {
			return TransformResult{}, fmt.Errorf("dataRef is required for PVC %s", obj.GetName())
		}
		vrr, err := buildVRR(obj, node, opts, targetNamespace)
		if err != nil {
			return TransformResult{}, err
		}
		output = append(output, *vrr)
	}
	return TransformResult{Objects: output}, nil
}

func buildVRR(pvc unstructured.Unstructured, node *SnapshotContentNode, opts Options, targetNamespace string) (*unstructured.Unstructured, error) {
	pvcName := pvc.GetName()
	if pvcName == "" {
		return nil, fmt.Errorf("PVC name is empty")
	}

	uidSuffix := shortUID(node.Content.GetUID())
	vrrName := fmt.Sprintf("restore-%s-%s", pvcName, uidSuffix)

	spec := map[string]interface{}{
		"source": map[string]interface{}{
			"kind": node.DataRefKind,
			"name": node.DataRefName,
		},
		"pvcTemplate": map[string]interface{}{
			"metadata": map[string]interface{}{
				"name": pvcName,
			},
			"spec": extractPVCSpec(pvc),
		},
	}

	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "storage.deckhouse.io/v1alpha1",
			"kind":       "VolumeRestoreRequest",
			"metadata": map[string]interface{}{
				"name":      vrrName,
				"namespace": targetNamespace,
			},
			"spec": spec,
		},
	}
	return obj, nil
}

func extractPVCSpec(pvc unstructured.Unstructured) map[string]interface{} {
	spec, ok := pvc.Object["spec"].(map[string]interface{})
	if !ok {
		return map[string]interface{}{}
	}
	result := map[string]interface{}{}
	if val, ok := spec["accessModes"]; ok {
		result["accessModes"] = val
	}
	if val, ok := spec["resources"]; ok {
		result["resources"] = val
	}
	if val, ok := spec["storageClassName"]; ok {
		result["storageClassName"] = val
	}
	if val, ok := spec["volumeMode"]; ok {
		result["volumeMode"] = val
	}
	return result
}

func shortUID(uid interface{}) string {
	str := fmt.Sprintf("%v", uid)
	str = strings.TrimSpace(str)
	if str == "" {
		return "unknown"
	}
	if len(str) <= 8 {
		return str
	}
	return str[:8]
}
