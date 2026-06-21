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

package restore

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

func cloneDataBindings(refs []snapshot.DataBindingRef) []snapshot.DataBindingRef {
	if len(refs) == 0 {
		return nil
	}
	out := make([]snapshot.DataBindingRef, len(refs))
	copy(out, refs)
	return out
}

// findDataBindingForPVC matches a PVC manifest to this content node's dataRefs[] only.
// When the PVC has a UID, only targetUID/target.uid are considered (no name fallback).
// When the PVC UID is empty, match by apiVersion/kind/namespace/name.
func findDataBindingForPVC(pvc unstructured.Unstructured, bindings []snapshot.DataBindingRef) (snapshot.DataBindingRef, bool) {
	pvcUID := string(pvc.GetUID())
	if pvcUID != "" {
		for _, b := range bindings {
			if b.TargetUID == pvcUID || b.Target.UID == pvcUID {
				return b, true
			}
		}
		return snapshot.DataBindingRef{}, false
	}
	for _, b := range bindings {
		if bindingMatchesPVCIdentity(pvc, b) {
			return b, true
		}
	}
	return snapshot.DataBindingRef{}, false
}

func bindingMatchesPVCIdentity(pvc unstructured.Unstructured, b snapshot.DataBindingRef) bool {
	return b.Target.APIVersion == pvc.GetAPIVersion() &&
		b.Target.Kind == pvc.GetKind() &&
		b.Target.Namespace == pvc.GetNamespace() &&
		b.Target.Name == pvc.GetName()
}
