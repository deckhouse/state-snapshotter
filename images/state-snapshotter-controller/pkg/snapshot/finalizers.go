/*
Copyright 2025 Flant JSC

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

package snapshot

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// HasFinalizer checks if an object has the specified finalizer.
//
// Contract: Pure function, idempotent, no side effects.
//
// See: unified-snapshots-test-plan.md (FUNCTIONS: pkg/snapshot Finalizers)
func HasFinalizer(obj metav1.Object, finalizer string) bool {
	finalizers := obj.GetFinalizers()
	for _, f := range finalizers {
		if f == finalizer {
			return true
		}
	}
	return false
}

// AddFinalizer adds a finalizer to an object if it doesn't already exist.
//
// Returns true if finalizer was added, false if it already existed.
//
// Contract: Idempotent - adding the same finalizer twice has no effect.
// Modifies object state (sets finalizers).
//
// See: unified-snapshots-test-plan.md (TEST CASE: AddFinalizer - Idempotency)
func AddFinalizer(obj metav1.Object, finalizer string) bool {
	if HasFinalizer(obj, finalizer) {
		return false
	}
	obj.SetFinalizers(append(obj.GetFinalizers(), finalizer))
	return true
}

// RemoveFinalizer removes a finalizer from an object if it exists.
//
// Returns true if finalizer was removed, false if it didn't exist.
//
// Contract: Idempotent - removing non-existent finalizer has no effect.
// Modifies object state (removes finalizer).
//
// See: unified-snapshots-test-plan.md (TEST CASE: RemoveFinalizer - Idempotency)
func RemoveFinalizer(obj metav1.Object, finalizer string) bool {
	finalizers := obj.GetFinalizers()
	for i, f := range finalizers {
		if f == finalizer {
			obj.SetFinalizers(append(finalizers[:i], finalizers[i+1:]...))
			return true
		}
	}
	return false
}

