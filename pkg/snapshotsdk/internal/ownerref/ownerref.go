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

// Package ownerref writes and reads the controller owner reference that ties a child snapshot object to its
// parent: Ensure stamps it on an object being created, ControlledBy asks whether a parent already holds one.
package ownerref

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Ensure makes desired the controller owner reference on obj, preserving unrelated references. It fails
// closed if obj is already controlled by a different parent: another reference of the same kind (a
// different parent snapshot) or any other controller=true reference. This prevents one snapshot from
// stealing a child that another snapshot already owns, without hardcoding any domain kind.
func Ensure(obj client.Object, desired metav1.OwnerReference) error {
	refs := obj.GetOwnerReferences()
	out := make([]metav1.OwnerReference, 0, len(refs)+1)
	found := false
	for _, ref := range refs {
		switch {
		case matches(ref, desired):
			found = true
			out = append(out, desired)
		case ref.APIVersion == desired.APIVersion && ref.Kind == desired.Kind:
			return fmt.Errorf("object %s/%s already owned by a different %s (%s)", obj.GetNamespace(), obj.GetName(), desired.Kind, ref.Name)
		case ref.Controller != nil && *ref.Controller:
			return fmt.Errorf("object %s/%s already controlled by %s/%s (%s)", obj.GetNamespace(), obj.GetName(), ref.APIVersion, ref.Kind, ref.Name)
		default:
			out = append(out, ref)
		}
	}
	if !found {
		out = append(out, desired)
	}
	obj.SetOwnerReferences(out)
	return nil
}

// Matches reports whether ref identifies the same owner as desired (apiVersion/kind/name).
func Matches(ref, desired metav1.OwnerReference) bool { return matches(ref, desired) }

// ControlledBy reports whether obj is ALREADY controlled by exactly desired: same apiVersion, kind, name
// and UID, with Controller=true. It is the eligibility question — "did this parent already take this object
// in?" — and unlike Ensure it never writes.
//
// The UID is part of the comparison on purpose. A reference that names the same parent by name but a
// different UID belongs to a previous incarnation of it, and its dependent is already condemned: garbage
// collection removes an object whose owner UID no longer exists. Treating that as "ours" would race the
// collector for an object about to disappear, so the caller fails closed and lets the collection finish.
func ControlledBy(obj client.Object, desired metav1.OwnerReference) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Controller == nil || !*ref.Controller {
			continue
		}
		return matches(ref, desired) && ref.UID == desired.UID
	}
	return false
}

func matches(ref, desired metav1.OwnerReference) bool {
	return ref.APIVersion == desired.APIVersion && ref.Kind == desired.Kind && ref.Name == desired.Name
}
