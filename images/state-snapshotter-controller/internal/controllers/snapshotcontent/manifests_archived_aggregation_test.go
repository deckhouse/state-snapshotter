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

package snapshotcontent

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// withCurrentSubtreePersisted stamps the persisted latch (status.subtreeManifestsPersisted=true) onto the
// (unstructured) content object so lifelong-latch behaviour can be exercised: the compute reads the
// persisted bool first and holds it true regardless of the live legs.
func withCurrentSubtreePersisted(obj *unstructured.Unstructured) *unstructured.Unstructured {
	statusMap, _ := obj.Object["status"].(map[string]interface{})
	if statusMap == nil {
		statusMap = map[string]interface{}{}
	}
	statusMap["subtreeManifestsPersisted"] = true
	obj.Object["status"] = statusMap
	return obj
}

// Leaf node, own MCP ready, no children -> subtreeManifestsPersisted=true.
func TestSubtreeManifestsPersistedLeafTrueWhenManifestReady(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, commonContentWithStatus("c", "mcp-ok"))
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if !plan.subtreeManifestsPersisted {
		t.Fatalf("subtreeManifestsPersisted=%v, want true", plan.subtreeManifestsPersisted)
	}
}

// Own manifest leg pending (no MCP name) -> subtreeManifestsPersisted=false (transient, non-terminal).
func TestSubtreeManifestsPersistedFalseWhenManifestPending(t *testing.T) {
	ctx := context.Background()
	cl := fake.NewClientBuilder().WithScheme(aggScheme(t)).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, commonContentWithStatus("c", ""))
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.subtreeManifestsPersisted {
		t.Fatalf("subtreeManifestsPersisted=%v, want false (manifest pending)", plan.subtreeManifestsPersisted)
	}
}

// Own manifest leg terminally failed before persist -> subtreeManifestsPersisted stays false and the
// failure surfaces on the manifest leg (manifestsFailed), NOT as a Failed latch value (the latch is a
// success-only bool now, no tri-state).
func TestSubtreeManifestsPersistedFalseWhenOwnManifestFailed(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-bad", metav1.ConditionFalse, ssv1alpha1.ManifestCheckpointConditionReasonFailed, "capture failed")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, commonContentWithStatus("c", "mcp-bad"))
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.subtreeManifestsPersisted {
		t.Fatalf("subtreeManifestsPersisted=%v, want false (own manifest failed)", plan.subtreeManifestsPersisted)
	}
	if !plan.manifestsFailed {
		t.Fatalf("manifestsFailed=%v, want true (failure surfaces on the manifest leg, not the latch)", plan.manifestsFailed)
	}
}

// Recursion: own MCP ready but a child is not yet persisted -> parent subtreeManifestsPersisted=false.
func TestSubtreeManifestsPersistedRecursionParentFalseUntilChildPersisted(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	child := contentWithSubtreeManifestsPersisted("child", false)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp, child).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, commonContentWithStatus("c", "mcp-ok", "child"))
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.subtreeManifestsPersisted {
		t.Fatalf("subtreeManifestsPersisted=%v, want false (child not persisted)", plan.subtreeManifestsPersisted)
	}
}

// Recursion: own MCP ready and child persisted -> parent subtreeManifestsPersisted=true.
func TestSubtreeManifestsPersistedRecursionParentTrueWhenChildPersisted(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	child := contentWithSubtreeManifestsPersisted("child", true)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp, child).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, commonContentWithStatus("c", "mcp-ok", "child"))
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if !plan.subtreeManifestsPersisted {
		t.Fatalf("subtreeManifestsPersisted=%v, want true", plan.subtreeManifestsPersisted)
	}
}

// Latch: once persisted, a later own-manifest degradation (MCP gone) must NOT re-open the latch.
func TestSubtreeManifestsPersistedLatchHoldsTrueOnDegradation(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	// No MCP object exists -> own manifest leg would be pending, but the persisted latch is already true.
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	obj := withCurrentSubtreePersisted(commonContentWithStatus("c", ""))
	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, obj)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if !plan.subtreeManifestsPersisted {
		t.Fatalf("subtreeManifestsPersisted=%v, want true (lifelong latch must not re-open)", plan.subtreeManifestsPersisted)
	}
}
