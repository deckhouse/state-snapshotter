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

package snapshot

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// snapWithChildrenReady builds a root Snapshot carrying a ChildrenSnapshotReady condition, used to
// exercise the childGraphReplanSkippable fast-path guard in isolation.
func snapWithChildrenReady(generation int64, status metav1.ConditionStatus, reason string, observedGen int64, refs []storagev1alpha1.SnapshotChildRef) *storagev1alpha1.Snapshot {
	s := &storagev1alpha1.Snapshot{}
	s.Namespace = "ns"
	s.Name = "root"
	s.Generation = generation
	s.Status.ChildrenSnapshotRefs = refs
	if status != "" {
		s.Status.Conditions = []metav1.Condition{{
			Type:               snapshotpkg.ConditionChildrenSnapshotReady,
			Status:             status,
			Reason:             reason,
			ObservedGeneration: observedGen,
		}}
	}
	return s
}

// Test 1: ChildrenSnapshotReady=True/Completed and current generation -> re-plan is skipped, and the
// downstream capture leg relies on the already-published status.childrenSnapshotRefs.
func TestChildGraphReplanSkippable_ReadyCurrentGenerationSkips(t *testing.T) {
	s := snapWithChildrenReady(3, metav1.ConditionTrue, snapshotpkg.ReasonCompleted, 3, []storagev1alpha1.SnapshotChildRef{childRef("nss-child-a")})
	if !childGraphReplanSkippable(s) {
		t.Fatalf("expected skip when ChildrenSnapshotReady=True/Completed and observedGeneration==generation")
	}
	if len(s.Status.ChildrenSnapshotRefs) != 1 || s.Status.ChildrenSnapshotRefs[0].Name != "nss-child-a" {
		t.Fatalf("skip must preserve published status.childrenSnapshotRefs; got %+v", s.Status.ChildrenSnapshotRefs)
	}
}

// Test 2: stale observedGeneration (spec changed) -> full re-plan.
func TestChildGraphReplanSkippable_StaleObservedGenerationReplans(t *testing.T) {
	s := snapWithChildrenReady(4, metav1.ConditionTrue, snapshotpkg.ReasonCompleted, 3, []storagev1alpha1.SnapshotChildRef{childRef("nss-child-a")})
	if childGraphReplanSkippable(s) {
		t.Fatalf("stale observedGeneration (3) vs generation (4) must force a full re-plan")
	}
}

// Test 3: ChildrenSnapshotReady not True/Completed (False, Unknown, True-but-not-Completed, missing) -> full re-plan.
func TestChildGraphReplanSkippable_NonSuccessfulPlanReplans(t *testing.T) {
	cases := []struct {
		name   string
		status metav1.ConditionStatus
		reason string
	}{
		{"False/GraphPlanningFailed", metav1.ConditionFalse, snapshotpkg.ReasonGraphPlanningFailed},
		{"Unknown", metav1.ConditionUnknown, "Pending"},
		{"TrueButNotCompleted", metav1.ConditionTrue, "Pending"},
		{"MissingCondition", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := snapWithChildrenReady(2, tc.status, tc.reason, 2, nil)
			if childGraphReplanSkippable(s) {
				t.Fatalf("%s must force a full re-plan (skip requires True+Completed)", tc.name)
			}
		})
	}
}

// Test 5: an empty child graph is a valid success state (manifests-only namespace) and must be skippable.
func TestChildGraphReplanSkippable_EmptyGraphIsValidAndSkips(t *testing.T) {
	s := snapWithChildrenReady(1, metav1.ConditionTrue, snapshotpkg.ReasonCompleted, 1, nil)
	if !childGraphReplanSkippable(s) {
		t.Fatalf("empty child graph with ChildrenSnapshotReady=True/Completed/current must be skippable (manifests-only namespace)")
	}
	if len(s.Status.ChildrenSnapshotRefs) != 0 {
		t.Fatalf("empty graph must carry no child refs; got %+v", s.Status.ChildrenSnapshotRefs)
	}
}

// Test 4: a child that terminally fails AFTER readiness must still degrade the parent. Skipping the
// re-plan does not remove failure propagation: the capture pending-path bridge summarizes terminal
// child failures directly from the published status.childrenSnapshotRefs, independent of the re-plan.
func TestChildTerminalFailureDetectedWhileReplanSkipped(t *testing.T) {
	ctx := context.Background()
	const ns = "ns"

	failed := childRef("nss-child-a")
	parent := snapWithChildrenReady(1, metav1.ConditionTrue, snapshotpkg.ReasonCompleted, 1, []storagev1alpha1.SnapshotChildRef{failed})
	if !childGraphReplanSkippable(parent) {
		t.Fatalf("precondition: parent must be in the re-plan skip state")
	}

	child := &unstructured.Unstructured{}
	child.SetGroupVersionKind(schema.GroupVersionKind{Group: "demo.test", Version: "v1", Kind: "DemoSnapshot"})
	child.SetNamespace(ns)
	child.SetName(failed.Name)
	_ = unstructured.SetNestedField(child.Object, "child-content", "status", "boundSnapshotContentName")
	_ = unstructured.SetNestedSlice(child.Object, []interface{}{
		map[string]interface{}{
			"type":    snapshotpkg.ConditionReady,
			"status":  string(metav1.ConditionFalse),
			"reason":  "CapturePlanDrift",
			"message": "child terminally failed after readiness",
		},
	}, "status", "conditions")
	cl := fake.NewClientBuilder().WithRuntimeObjects(child).Build()

	sum, err := usecase.SummarizeChildSnapshotTerminalFailures(ctx, cl, parent.Status.ChildrenSnapshotRefs, ns)
	if err != nil {
		t.Fatal(err)
	}
	if !sum.HasFailed || len(sum.Messages) == 0 {
		t.Fatalf("terminal child failure must still be detected while the re-plan is skipped; got %+v", sum)
	}
}
