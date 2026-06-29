//go:build integration
// +build integration

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

package integration

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

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

// setSnapshotPlanningReadyCurrent publishes PlanningReady=True with observedGeneration ==
// metadata.generation — the planning-done signal the generic binder barrier requires. obj must
// already carry a server-assigned generation (created or fetched).
func setSnapshotPlanningReadyCurrent(ctx context.Context, obj *unstructured.Unstructured) {
	GinkgoHelper()
	setSnapshotPlanningReady(ctx, obj, metav1.ConditionTrue, obj.GetGeneration())
}

// setSnapshotPlanningReady publishes PlanningReady with an explicit status and observedGeneration so a
// test can exercise current vs stale-generation barrier behaviour, then updates the status
// subresource via k8sClient.
func setSnapshotPlanningReady(ctx context.Context, obj *unstructured.Unstructured, status metav1.ConditionStatus, observedGeneration int64) {
	GinkgoHelper()
	setPlanningReadyCondition(obj, status, observedGeneration)
	Expect(k8sClient.Status().Update(ctx, obj)).To(Succeed())
}

// setPlanningReadyCondition replaces any existing PlanningReady condition on obj (in memory only).
func setPlanningReadyCondition(obj *unstructured.Unstructured, status metav1.ConditionStatus, observedGeneration int64) {
	conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	kept := make([]interface{}, 0, len(conds)+1)
	for _, raw := range conds {
		if m, ok := raw.(map[string]interface{}); ok && m["type"] == snapshot.ConditionPlanningReady {
			continue
		}
		kept = append(kept, raw)
	}
	kept = append(kept, map[string]interface{}{
		"type":               snapshot.ConditionPlanningReady,
		"status":             string(status),
		"reason":             snapshot.ReasonCompleted,
		"message":            "domain planning complete",
		"observedGeneration": observedGeneration,
		"lastTransitionTime": metav1.Now().Format(time.RFC3339),
	})
	Expect(unstructured.SetNestedSlice(obj.Object, kept, "status", "conditions")).To(Succeed())
}
