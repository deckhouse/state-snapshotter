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

package common

import (
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func SnapshotOwnerReference(apiVersion, kind, name string, uid types.UID) metav1.OwnerReference {
	controller := true
	return metav1.OwnerReference{
		APIVersion: apiVersion,
		Kind:       kind,
		Name:       name,
		UID:        uid,
		Controller: &controller,
	}
}

func SnapshotOwnerRefMatches(ref, desired metav1.OwnerReference) bool {
	if ref.APIVersion != desired.APIVersion || ref.Kind != desired.Kind || ref.Name != desired.Name {
		return false
	}
	return desired.UID == "" || ref.UID == "" || ref.UID == desired.UID
}

func EnsureSnapshotOwnerRef(obj client.Object, desired metav1.OwnerReference) error {
	refs := make([]metav1.OwnerReference, 0, len(obj.GetOwnerReferences())+1)
	desiredSet := false
	for _, ref := range obj.GetOwnerReferences() {
		if SnapshotOwnerRefMatches(ref, desired) {
			if !desiredSet {
				refs = append(refs, desired)
				desiredSet = true
			}
			continue
		}
		if IsSnapshotParentOwnerRef(ref) {
			return fmt.Errorf("child snapshot %s/%s is already owned by %s/%s", obj.GetNamespace(), obj.GetName(), ref.Kind, ref.Name)
		}
		if ref.Controller != nil && *ref.Controller {
			return fmt.Errorf("child snapshot %s/%s already has controller ownerRef %s/%s", obj.GetNamespace(), obj.GetName(), ref.Kind, ref.Name)
		}
		refs = append(refs, ref)
	}
	if !desiredSet {
		refs = append(refs, desired)
	}
	if !OwnerReferencesEqual(obj.GetOwnerReferences(), refs) {
		obj.SetOwnerReferences(refs)
	}
	return nil
}

func IsSnapshotParentOwnerRef(ref metav1.OwnerReference) bool {
	return strings.HasSuffix(ref.Kind, "Snapshot")
}
