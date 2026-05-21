package restore

import (
	"fmt"
	"strings"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
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
		binding, ok := findDataBindingForPVC(obj, node.DataBindings)
		if !ok {
			return TransformResult{}, fmt.Errorf(
				"dataRefs binding is required for PVC %s/%s (uid=%s)",
				obj.GetNamespace(), obj.GetName(), obj.GetUID(),
			)
		}
		if binding.Artifact.Kind == "" || binding.Artifact.Name == "" {
			return TransformResult{}, fmt.Errorf(
				"dataRefs artifact is required for PVC %s/%s (uid=%s)",
				obj.GetNamespace(), obj.GetName(), obj.GetUID(),
			)
		}
		vrr, err := buildVRR(obj, binding.Artifact, node, targetNamespace)
		if err != nil {
			return TransformResult{}, err
		}
		output = append(output, *vrr)
	}
	return TransformResult{Objects: output}, nil
}

func buildVRR(pvc unstructured.Unstructured, artifact snapshot.ObjectRef, node *SnapshotContentNode, targetNamespace string) (*unstructured.Unstructured, error) {
	pvcName := pvc.GetName()
	if pvcName == "" {
		return nil, fmt.Errorf("PVC name is empty")
	}

	uidSuffix := shortUID(node.Content.GetUID())
	vrrName := fmt.Sprintf("restore-%s-%s", pvcName, uidSuffix)

	metadata := map[string]interface{}{
		"name": pvcName,
	}
	if labels := pvc.GetLabels(); len(labels) > 0 {
		metadata["labels"] = labels
	}
	if annotations := pvc.GetAnnotations(); len(annotations) > 0 {
		metadata["annotations"] = annotations
	}

	spec := map[string]interface{}{
		"source": map[string]interface{}{
			"kind": artifact.Kind,
			"name": artifact.Name,
		},
		"pvcTemplate": map[string]interface{}{
			"metadata": metadata,
			"spec":     extractPVCSpec(pvc),
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
	for key, val := range spec {
		result[key] = val
	}
	delete(result, "volumeName")
	delete(result, "dataSource")
	delete(result, "dataSourceRef")
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
