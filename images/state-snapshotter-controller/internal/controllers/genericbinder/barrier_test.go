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

package genericbinder

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// snapshotLikeWithPlanningReady builds a SnapshotLike at the given generation. When set is true it adds a
// PlanningReady condition with the given status and observedGeneration.
func snapshotLikeWithPlanningReady(generation int64, set bool, status metav1.ConditionStatus, observedGeneration int64) snapshot.SnapshotLike {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{}}
	obj.SetName("snap")
	obj.SetGeneration(generation)
	if set {
		_ = unstructured.SetNestedSlice(obj.Object, []interface{}{
			map[string]interface{}{
				"type":               snapshot.ConditionPlanningReady,
				"status":             string(status),
				"reason":             snapshot.ReasonCompleted,
				"observedGeneration": observedGeneration,
			},
		}, "status", "conditions")
	}
	like, _ := snapshot.ExtractSnapshotLike(obj)
	return like
}

func TestIsDomainPlanningComplete(t *testing.T) {
	tests := []struct {
		name string
		like snapshot.SnapshotLike
		want bool
	}{
		{
			name: "PlanningReady True for current generation passes",
			like: snapshotLikeWithPlanningReady(3, true, metav1.ConditionTrue, 3),
			want: true,
		},
		{
			name: "PlanningReady True with stale observedGeneration does not pass",
			like: snapshotLikeWithPlanningReady(3, true, metav1.ConditionTrue, 2),
			want: false,
		},
		{
			name: "PlanningReady False does not pass",
			like: snapshotLikeWithPlanningReady(3, true, metav1.ConditionFalse, 3),
			want: false,
		},
		{
			name: "no PlanningReady condition does not pass",
			like: snapshotLikeWithPlanningReady(3, false, metav1.ConditionTrue, 0),
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDomainPlanningComplete(tc.like); got != tc.want {
				t.Fatalf("isDomainPlanningComplete = %v, want %v", got, tc.want)
			}
		})
	}
}
