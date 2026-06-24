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

// Package ownerref implements safe, idempotent controller-owner adoption for child snapshot objects.
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

func matches(ref, desired metav1.OwnerReference) bool {
	return ref.APIVersion == desired.APIVersion && ref.Kind == desired.Kind && ref.Name == desired.Name
}
