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

package transform

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// PVCDataBinding is one captured PVC bound to the durable data artifact that holds its volume content —
// the restore-relevant projection of a snapshot node's data leg. Volume metadata carried alongside the
// binding by the capture side (volume mode, filesystem type, storage class) is not part of it: the
// restore compiler only needs to know WHICH artifact backs WHICH PVC.
type PVCDataBinding struct {
	// TargetUID is the captured PVC's UID. It is the primary match key and is recorded separately from
	// Target because a binding may predate the full target identity.
	TargetUID string
	// Target is the captured PVC identity (apiVersion/kind/namespace/name, optionally uid).
	Target storagev1alpha1.ObjectRef
	// Artifact is the durable object holding the volume content — a VolumeSnapshotContent for every
	// binding the restore compiler accepts.
	Artifact storagev1alpha1.ObjectRef
}

// FindDataBindingForPVC matches a captured PVC manifest to one of the node's own data bindings.
// When the PVC has a UID, only targetUID/target.uid are considered (no name fallback): a name match on a
// different UID would bind a restored PVC to another volume's data. When the PVC UID is empty (older
// captures), it matches by apiVersion/kind/namespace/name.
//
// It is matched against ONE node's bindings, never a tree-wide set: each node's data leg belongs to that
// node alone.
func FindDataBindingForPVC(pvc unstructured.Unstructured, bindings []PVCDataBinding) (PVCDataBinding, bool) {
	pvcUID := string(pvc.GetUID())
	if pvcUID != "" {
		for _, b := range bindings {
			if b.TargetUID == pvcUID || b.Target.UID == pvcUID {
				return b, true
			}
		}
		return PVCDataBinding{}, false
	}
	for _, b := range bindings {
		if bindingMatchesPVCIdentity(pvc, b) {
			return b, true
		}
	}
	return PVCDataBinding{}, false
}

func bindingMatchesPVCIdentity(pvc unstructured.Unstructured, b PVCDataBinding) bool {
	return b.Target.APIVersion == pvc.GetAPIVersion() &&
		b.Target.Kind == pvc.GetKind() &&
		b.Target.Namespace == pvc.GetNamespace() &&
		b.Target.Name == pvc.GetName()
}
