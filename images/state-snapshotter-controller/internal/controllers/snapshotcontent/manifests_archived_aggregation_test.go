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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// contentWithArchivedCond builds a child SnapshotContent carrying a ManifestsArchived condition, used to
// drive the parent's recursive latch aggregation.
func contentWithArchivedCond(name string, status metav1.ConditionStatus, reason, message string) *storagev1alpha1.SnapshotContent {
	c := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: name}}
	meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{
		Type:    snapshot.ConditionManifestsArchived,
		Status:  status,
		Reason:  reason,
		Message: message,
	})
	return c
}

// withCurrentArchivedCondition stamps an existing ManifestsArchived condition onto the (unstructured)
// content object so latch behaviour can be exercised (the latch reads the current condition first).
func withCurrentArchivedCondition(obj *unstructured.Unstructured, status metav1.ConditionStatus, reason, message string) *unstructured.Unstructured {
	statusMap, _ := obj.Object["status"].(map[string]interface{})
	if statusMap == nil {
		statusMap = map[string]interface{}{}
	}
	conds, _ := statusMap["conditions"].([]interface{})
	conds = append(conds, map[string]interface{}{
		"type":               snapshot.ConditionManifestsArchived,
		"status":             string(status),
		"reason":             reason,
		"message":            message,
		"lastTransitionTime": metav1.Now().Format("2006-01-02T15:04:05Z07:00"),
	})
	statusMap["conditions"] = conds
	obj.Object["status"] = statusMap
	return obj
}

// Leaf node, own MCP ready, no children -> ManifestsArchived=True/Archived.
func TestManifestsArchivedLeafTrueWhenManifestReady(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, commonContentWithStatus("c", "mcp-ok"))
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.manifestsArchivedStatus != metav1.ConditionTrue || plan.manifestsArchivedReason != snapshot.ReasonManifestsArchived {
		t.Fatalf("manifestsArchived=%s/%s, want True/%s", plan.manifestsArchivedStatus, plan.manifestsArchivedReason, snapshot.ReasonManifestsArchived)
	}
}

// Own manifest leg pending (no MCP name) -> ManifestsArchived=False/Capturing (transient).
func TestManifestsArchivedCapturingWhenManifestPending(t *testing.T) {
	ctx := context.Background()
	cl := fake.NewClientBuilder().WithScheme(aggScheme(t)).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, commonContentWithStatus("c", ""))
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.manifestsArchivedStatus != metav1.ConditionFalse || plan.manifestsArchivedReason != snapshot.ReasonManifestsCapturing {
		t.Fatalf("manifestsArchived=%s/%s, want False/%s", plan.manifestsArchivedStatus, plan.manifestsArchivedReason, snapshot.ReasonManifestsCapturing)
	}
}

// Own manifest leg terminally failed before archive -> ManifestsArchived=False/Failed.
func TestManifestsArchivedFailedWhenOwnManifestFailed(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-bad", metav1.ConditionFalse, ssv1alpha1.ManifestCheckpointConditionReasonFailed, "capture failed")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, commonContentWithStatus("c", "mcp-bad"))
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.manifestsArchivedStatus != metav1.ConditionFalse || plan.manifestsArchivedReason != snapshot.ReasonManifestsArchiveFailed {
		t.Fatalf("manifestsArchived=%s/%s, want False/%s", plan.manifestsArchivedStatus, plan.manifestsArchivedReason, snapshot.ReasonManifestsArchiveFailed)
	}
}

// Recursion: own MCP ready but a child is not yet archived -> parent ManifestsArchived=False/Capturing.
func TestManifestsArchivedRecursionParentCapturingUntilChildArchived(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	child := contentWithArchivedCond("child", metav1.ConditionFalse, snapshot.ReasonManifestsCapturing, "still capturing")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp, child).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, commonContentWithStatus("c", "mcp-ok", "child"))
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.manifestsArchivedStatus != metav1.ConditionFalse || plan.manifestsArchivedReason != snapshot.ReasonManifestsCapturing {
		t.Fatalf("manifestsArchived=%s/%s, want False/%s (child not archived)", plan.manifestsArchivedStatus, plan.manifestsArchivedReason, snapshot.ReasonManifestsCapturing)
	}
}

// Recursion: own MCP ready and child archived -> parent ManifestsArchived=True/Archived.
func TestManifestsArchivedRecursionParentArchivedWhenChildArchived(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	child := contentWithArchivedCond("child", metav1.ConditionTrue, snapshot.ReasonManifestsArchived, "archived")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp, child).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, commonContentWithStatus("c", "mcp-ok", "child"))
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.manifestsArchivedStatus != metav1.ConditionTrue || plan.manifestsArchivedReason != snapshot.ReasonManifestsArchived {
		t.Fatalf("manifestsArchived=%s/%s, want True/%s", plan.manifestsArchivedStatus, plan.manifestsArchivedReason, snapshot.ReasonManifestsArchived)
	}
}

// Recursion: a child whose manifests can never be archived -> parent ManifestsArchived=False/Failed.
func TestManifestsArchivedFailedWhenChildFailed(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	child := contentWithArchivedCond("child", metav1.ConditionFalse, snapshot.ReasonManifestsArchiveFailed, "subtree cannot be archived")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp, child).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, commonContentWithStatus("c", "mcp-ok", "child"))
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.manifestsArchivedStatus != metav1.ConditionFalse || plan.manifestsArchivedReason != snapshot.ReasonManifestsArchiveFailed {
		t.Fatalf("manifestsArchived=%s/%s, want False/%s", plan.manifestsArchivedStatus, plan.manifestsArchivedReason, snapshot.ReasonManifestsArchiveFailed)
	}
}

// Latch: once Archived, a later own-manifest degradation (MCP gone) must NOT re-open the latch.
func TestManifestsArchivedLatchHoldsTrueOnDegradation(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	// No MCP object exists -> own manifest leg would be pending/capturing, but the current latch is True.
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	obj := withCurrentArchivedCondition(commonContentWithStatus("c", ""), metav1.ConditionTrue, snapshot.ReasonManifestsArchived, "archived earlier")
	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, obj)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.manifestsArchivedStatus != metav1.ConditionTrue || plan.manifestsArchivedReason != snapshot.ReasonManifestsArchived {
		t.Fatalf("manifestsArchived=%s/%s, want True/%s (lifelong latch must not re-open)", plan.manifestsArchivedStatus, plan.manifestsArchivedReason, snapshot.ReasonManifestsArchived)
	}
}

// Latch: once Failed, a later own-manifest recovery must NOT flip the latch back.
func TestManifestsArchivedLatchHoldsFailed(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	obj := withCurrentArchivedCondition(commonContentWithStatus("c", "mcp-ok"), metav1.ConditionFalse, snapshot.ReasonManifestsArchiveFailed, "failed earlier")
	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, obj)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.manifestsArchivedStatus != metav1.ConditionFalse || plan.manifestsArchivedReason != snapshot.ReasonManifestsArchiveFailed {
		t.Fatalf("manifestsArchived=%s/%s, want False/%s (terminal latch must stick)", plan.manifestsArchivedStatus, plan.manifestsArchivedReason, snapshot.ReasonManifestsArchiveFailed)
	}
}
