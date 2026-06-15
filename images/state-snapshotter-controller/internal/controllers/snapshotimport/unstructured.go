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

package snapshotimport

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Cross-repo CRD coordinates, consumed as unstructured so the controller keeps no Go dependency on
// storage-volume-data-manager (DataImport) or storage-foundation (VolumeCaptureRequest is consumed
// through the shared volumecapture package).
var (
	dataImportGVK = schema.GroupVersionKind{Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "DataImport"}
)

const (
	conditionTypeReady          = "Ready"
	conditionTypeUploadFinished = "UploadFinished"
	kindPersistentVolumeClaim   = "PersistentVolumeClaim"
)

// dataImportSpec carries the resolved per-node template for a populating DataImport.
type dataImportSpec struct {
	pvcName          string
	storageClassName string
	volumeMode       string
	accessModes      []string
	sizeQuantity     string
	ttl              string
	publish          bool
}

// newDataImport builds a DataImport that populates a PVC (targetRef.kind=PersistentVolumeClaim) from
// the resolved per-node template. The importer pod created by SVDM exposes the resumable upload
// endpoint reported in status.url.
func newDataImport(namespace, name string, owner metav1.OwnerReference, di dataImportSpec) *unstructured.Unstructured {
	pvcSpec := map[string]interface{}{
		"storageClassName": di.storageClassName,
		"volumeMode":       di.volumeMode,
	}
	if len(di.accessModes) > 0 {
		modes := make([]interface{}, 0, len(di.accessModes))
		for _, m := range di.accessModes {
			modes = append(modes, m)
		}
		pvcSpec["accessModes"] = modes
	}
	if di.sizeQuantity != "" {
		pvcSpec["resources"] = map[string]interface{}{
			"requests": map[string]interface{}{
				"storage": di.sizeQuantity,
			},
		}
	}
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": dataImportGVK.GroupVersion().String(),
		"kind":       dataImportGVK.Kind,
		"metadata": map[string]interface{}{
			"name":            name,
			"namespace":       namespace,
			"ownerReferences": []interface{}{ownerRefToMap(owner)},
		},
		"spec": map[string]interface{}{
			"ttl":     di.ttl,
			"publish": di.publish,
			"targetRef": map[string]interface{}{
				"kind": kindPersistentVolumeClaim,
				"pvcTemplate": map[string]interface{}{
					"metadata": map[string]interface{}{
						"name": di.pvcName,
					},
					"spec": pvcSpec,
				},
			},
		},
	}}
	obj.SetGroupVersionKind(dataImportGVK)
	return obj
}

func ownerRefToMap(ref metav1.OwnerReference) map[string]interface{} {
	m := map[string]interface{}{
		"apiVersion": ref.APIVersion,
		"kind":       ref.Kind,
		"name":       ref.Name,
		"uid":        string(ref.UID),
	}
	if ref.Controller != nil {
		m["controller"] = *ref.Controller
	}
	if ref.BlockOwnerDeletion != nil {
		m["blockOwnerDeletion"] = *ref.BlockOwnerDeletion
	}
	return m
}

// readConditionTrue reports whether the named condition has status "True" on an unstructured object.
func readConditionTrue(obj *unstructured.Unstructured, condType string) bool {
	conds, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return false
	}
	for _, c := range conds {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); t != condType {
			continue
		}
		st, _ := m["status"].(string)
		return st == "True"
	}
	return false
}

// nestedStr is a convenience wrapper over unstructured.NestedString that ignores errors.
func nestedStr(obj *unstructured.Unstructured, fields ...string) string {
	s, _, _ := unstructured.NestedString(obj.Object, fields...)
	return s
}
