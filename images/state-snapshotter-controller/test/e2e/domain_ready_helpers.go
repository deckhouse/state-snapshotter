//go:build e2e
// +build e2e

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

package e2e

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// injectPlanningReadyCurrent sets PlanningReady=True with observedGeneration == generation on a
// SnapshotLike (in memory). Use it inside blocks that already drive their own
// SyncConditionsToUnstructured + Status().Update; SyncConditionsToUnstructured persists
// observedGeneration so the condition stays current for the generic binder barrier.
func injectPlanningReadyCurrent(like snapshot.SnapshotLike, generation int64) {
	conds := like.GetStatusConditions()
	kept := make([]metav1.Condition, 0, len(conds)+1)
	for _, c := range conds {
		if c.Type == snapshot.ConditionPlanningReady {
			continue
		}
		kept = append(kept, c)
	}
	kept = append(kept, metav1.Condition{
		Type:               snapshot.ConditionPlanningReady,
		Status:             metav1.ConditionTrue,
		Reason:             snapshot.ReasonCompleted,
		Message:            "domain planning complete",
		ObservedGeneration: generation,
		LastTransitionTime: metav1.Now(),
	})
	like.SetStatusConditions(kept)
}
