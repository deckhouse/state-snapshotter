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

// Package conditions merges a single status condition while preserving co-owned conditions.
package conditions

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Upsert sets or updates the condition of cond.Type in conds, leaving all other conditions untouched, and
// returns the resulting slice. observedGeneration must be stamped by the caller on cond.
func Upsert(conds []metav1.Condition, cond metav1.Condition) []metav1.Condition {
	meta.SetStatusCondition(&conds, cond)
	return conds
}

// IsTrue reports whether the condition of condType is present with Status=True. It is used as the durable
// commit marker for the child topology: once the planning barrier is True, the published child set is
// frozen (see capture.EnsureChildren).
func IsTrue(conds []metav1.Condition, condType string) bool {
	return meta.IsStatusConditionTrue(conds, condType)
}

// Equal reports whether the existing condition of condType already matches the desired status/reason/
// message at the given observedGeneration, so a no-op patch can be skipped.
func Equal(conds []metav1.Condition, condType string, status metav1.ConditionStatus, reason, message string, observedGeneration int64) bool {
	existing := meta.FindStatusCondition(conds, condType)
	return existing != nil &&
		existing.Status == status &&
		existing.Reason == reason &&
		existing.Message == message &&
		existing.ObservedGeneration == observedGeneration
}
