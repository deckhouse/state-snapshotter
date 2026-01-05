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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// SyncConditionsToUnstructured synchronizes conditions from SnapshotLike/SnapshotContentLike
// to unstructured object status.conditions
// This is a single source of truth for condition serialization
func SyncConditionsToUnstructured(obj *unstructured.Unstructured, conditions []metav1.Condition) {
	status := obj.Object["status"]
	if status == nil {
		status = make(map[string]interface{})
		obj.Object["status"] = status
	}
	statusMap := status.(map[string]interface{})

	conditionsRaw := make([]interface{}, 0, len(conditions))
	for _, cond := range conditions {
		condMap := map[string]interface{}{
			"type":               cond.Type,
			"status":             string(cond.Status),
			"reason":             cond.Reason,
			"message":            cond.Message,
			"observedGeneration": cond.ObservedGeneration,
		}
		// Only include lastTransitionTime if it's not zero
		if !cond.LastTransitionTime.IsZero() {
			condMap["lastTransitionTime"] = cond.LastTransitionTime.Format(time.RFC3339)
		}
		conditionsRaw = append(conditionsRaw, condMap)
	}
	statusMap["conditions"] = conditionsRaw
}

