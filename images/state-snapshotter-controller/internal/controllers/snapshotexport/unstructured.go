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

package snapshotexport

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Cross-repo CRD coordinates. These are built/read as unstructured so the state-snapshotter
// controller keeps no Go dependency on storage-foundation (VRR) or storage-volume-data-manager
// (DataExport), mirroring the existing VolumeCaptureRequest handling.
var (
	volumeRestoreRequestGVK = schema.GroupVersionKind{Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "VolumeRestoreRequest"}
	dataExportGVK           = schema.GroupVersionKind{Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "DataExport"}
)

const (
	// conditionTypeReady is the Ready condition shared by VRR and DataExport.
	conditionTypeReady = "Ready"
	// reasonCompleted is the VRR success reason (ConditionReasonCompleted upstream).
	reasonCompleted = "Completed"
	// reasonExpired is the DataExport Ready=False reason emitted by the SVDM pod once the export's
	// idle TTL elapses (storage-volume-data-manager common.ReasonExpired). It is the signal that lets
	// the export controller free the leaf's heavy children instead of re-ensuring them.
	reasonExpired = "Expired"
	// kindVolumeSnapshotContent / kindPersistentVolumeClaim are the source/target kinds.
	kindVolumeSnapshotContent = "VolumeSnapshotContent"
	kindPersistentVolumeClaim = "PersistentVolumeClaim"
)

// newVolumeRestoreRequest builds a VRR that restores a VolumeSnapshotContent into a PVC in the
// export namespace. volumeMode is required upstream; the caller guarantees a non-empty value.
func newVolumeRestoreRequest(namespace, name string, owner metav1.OwnerReference, vscName, targetNamespace, targetPVC, storageClassName, volumeMode, fsType string, accessModes []string) *unstructured.Unstructured {
	spec := map[string]interface{}{
		"sourceRef": map[string]interface{}{
			"kind": kindVolumeSnapshotContent,
			"name": vscName,
		},
		"targetNamespace": targetNamespace,
		"targetPVCName":   targetPVC,
		"volumeMode":      volumeMode,
	}
	if storageClassName != "" {
		spec["storageClassName"] = storageClassName
	}
	if fsType != "" {
		spec["fsType"] = fsType
	}
	if len(accessModes) > 0 {
		modes := make([]interface{}, 0, len(accessModes))
		for _, m := range accessModes {
			modes = append(modes, m)
		}
		spec["accessModes"] = modes
	}
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": volumeRestoreRequestGVK.GroupVersion().String(),
		"kind":       volumeRestoreRequestGVK.Kind,
		"metadata": map[string]interface{}{
			"name":            name,
			"namespace":       namespace,
			"ownerReferences": []interface{}{ownerRefToMap(owner)},
		},
		"spec": spec,
	}}
	obj.SetGroupVersionKind(volumeRestoreRequestGVK)
	return obj
}

// newDataExport builds a DataExport serving a PVC's data.
func newDataExport(namespace, name string, owner metav1.OwnerReference, pvcName, ttl string, publish bool) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": dataExportGVK.GroupVersion().String(),
		"kind":       dataExportGVK.Kind,
		"metadata": map[string]interface{}{
			"name":            name,
			"namespace":       namespace,
			"ownerReferences": []interface{}{ownerRefToMap(owner)},
		},
		"spec": map[string]interface{}{
			"ttl":     ttl,
			"publish": publish,
			"targetRef": map[string]interface{}{
				"kind": kindPersistentVolumeClaim,
				"name": pvcName,
			},
		},
	}}
	obj.SetGroupVersionKind(dataExportGVK)
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

// readReadyCondition returns (status==True, reason) for the Ready condition of an unstructured object.
func readReadyCondition(obj *unstructured.Unstructured) (ready bool, reason string) {
	conds, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return false, ""
	}
	for _, c := range conds {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if s, _ := m["type"].(string); s != conditionTypeReady {
			continue
		}
		st, _ := m["status"].(string)
		rs, _ := m["reason"].(string)
		return st == "True", rs
	}
	return false, ""
}

// nestedStr is a convenience wrapper over unstructured.NestedString that ignores errors.
func nestedStr(obj *unstructured.Unstructured, fields ...string) string {
	s, _, _ := unstructured.NestedString(obj.Object, fields...)
	return s
}

// terminalChildReasons are VRR/DataExport Ready=False reasons that indicate a non-recoverable failure
// (as opposed to "still converging"). They mirror storage-foundation's condition reasons; the export
// controller treats them as terminal so it surfaces the failure and backs off instead of hot-looping.
var terminalChildReasons = map[string]struct{}{
	"InvalidMode":            {},
	"Incompatible":           {},
	"InvalidTTL":             {},
	"InternalError":          {},
	"NotFound":               {},
	"RBACDenied":             {},
	"InvalidSource":          {},
	"PVBound":                {},
	"SnapshotCreationFailed": {},
	"RestoreFailed":          {},
	"ValidationFailed":       {},
	"Expired":                {},
}

func isTerminalReason(reason string) bool {
	_, ok := terminalChildReasons[reason]
	return ok
}

func reasonOrUnknown(reason string) string {
	if reason == "" {
		return "pending"
	}
	return reason
}
