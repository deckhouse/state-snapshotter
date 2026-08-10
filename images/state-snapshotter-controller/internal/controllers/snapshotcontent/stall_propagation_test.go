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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
)

// The diagnosis the foundation publishes: the storage-specific reason plus a message that already
// names the CSI driver and the VolumeSnapshotContent.
const (
	stallLowerReason  = "SnapshotExecutionUnobservable"
	stallLowerMessage = `no observable snapshot execution for VolumeSnapshotContent "vsc-1" (CSI driver "rook-ceph.rbd.csi.ceph.com") for 4m12s`
)

func stallOwnerWithVCR(vcrName string) *unstructured.Unstructured {
	owner := &unstructured.Unstructured{}
	owner.SetGroupVersionKind(schema.GroupVersionKind{Group: "sds-unified-snapshots-poc.deckhouse.io", Version: "v1alpha1", Kind: "DemoVirtualDiskSnapshot"})
	owner.SetNamespace(projTestNS)
	owner.SetName("disk-snap")
	if vcrName != "" {
		_ = unstructured.SetNestedField(owner.Object, vcrName, "status", "captureState", "domainSpecificController", "volumeCaptureRequestName")
	}
	return owner
}

// stallVCR builds a VolumeCaptureRequest as the foundation writes it during a stall: still
// non-terminal on Ready, with the diagnosis on the separate Stalled condition.
func stallVCR(stalledStatus metav1.ConditionStatus, reason, message string) *unstructured.Unstructured {
	vcr := projReadyVCR()
	_ = unstructured.SetNestedSlice(vcr.Object, []interface{}{
		map[string]interface{}{
			"type":   vcpkg.ConditionTypeReady,
			"status": string(metav1.ConditionFalse),
			"reason": vcpkg.ConditionReasonTargetsPending,
		},
		map[string]interface{}{
			"type":    vcpkg.ConditionTypeStalled,
			"status":  string(stalledStatus),
			"reason":  reason,
			"message": message,
		},
	}, "status", "conditions")
	return vcr
}

func TestObserveDataLegStall(t *testing.T) {
	ctx := context.Background()

	t.Run("reads the diagnosis published by the capture request", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(projScheme(t)).
			WithObjects(stallVCR(metav1.ConditionTrue, stallLowerReason, stallLowerMessage)).Build()
		r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

		reason, message := r.observeDataLegStall(ctx, stallOwnerWithVCR(projTestVCRName))

		if reason != stallLowerReason {
			t.Fatalf("reason = %q, want %q", reason, stallLowerReason)
		}
		if message != stallLowerMessage {
			t.Fatalf("message = %q, want the foundation's message verbatim", message)
		}
	})

	t.Run("a cleared stall is not a diagnosis", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(projScheme(t)).
			WithObjects(stallVCR(metav1.ConditionFalse, "SnapshotExecutionResumed", "observable snapshot activity resumed")).Build()
		r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

		if reason, _ := r.observeDataLegStall(ctx, stallOwnerWithVCR(projTestVCRName)); reason != "" {
			t.Fatalf("reason = %q, want none", reason)
		}
	})

	t.Run("owners without a capture request yield nothing", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(projScheme(t)).Build()
		r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

		if reason, _ := r.observeDataLegStall(ctx, stallOwnerWithVCR("")); reason != "" {
			t.Fatalf("an owner with no data leg must not be diagnosed, got %q", reason)
		}
		if reason, _ := r.observeDataLegStall(ctx, nil); reason != "" {
			t.Fatalf("a missing owner must not be diagnosed, got %q", reason)
		}
	})

	t.Run("an unreadable capture request leaves the pending state as it was", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(projScheme(t)).Build()
		r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

		if reason, _ := r.observeDataLegStall(ctx, stallOwnerWithVCR(projTestVCRName)); reason != "" {
			t.Fatalf("a missing request is not evidence of a stall, got %q", reason)
		}
	})
}

// TestBuildDataCaptureStalledMessage pins what the upper layers are allowed to see: one snapshot-level
// reason, with the storage vocabulary confined to the text.
func TestBuildDataCaptureStalledMessage(t *testing.T) {
	got := buildDataCaptureStalledMessage(stallLowerReason, stallLowerMessage)

	for _, want := range []string{stallLowerReason, "vsc-1", "rook-ceph.rbd.csi.ceph.com", "no observable progress"} {
		if !strings.Contains(got, want) {
			t.Fatalf("message %q must mention %q", got, want)
		}
	}
	if strings.Contains(got, "snapshotter is absent") {
		t.Fatalf("message must not claim what observation cannot prove: %q", got)
	}
}

// TestContentPlanChildStalled: a stalled child is still pending, but the parent reports the diagnosis
// instead of a bare progress count, so the reason survives all the way up the tree.
func TestContentPlanChildStalled(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	stalledChild := contentWithReadyCond("child-stalled", metav1.ConditionFalse,
		snapshot.ReasonDataCaptureStalled, buildDataCaptureStalledMessage(stallLowerReason, stallLowerMessage))
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp, stalledChild).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, commonContentWithStatus("c", "mcp-ok", "child-stalled"))
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}

	if plan.childrenFailed {
		t.Fatalf("a stalled child must never fail the tree: the capture can still succeed")
	}
	if plan.readyStatus != metav1.ConditionFalse || plan.readyReason != snapshot.ReasonDataCaptureStalled {
		t.Fatalf("ready = %s/%s, want False/%s", plan.readyStatus, plan.readyReason, snapshot.ReasonDataCaptureStalled)
	}
	if !strings.Contains(plan.readyMessage, "vsc-1") || !strings.Contains(plan.readyMessage, "child-stalled") {
		t.Fatalf("parent message must name both the child and the original diagnosis, got %q", plan.readyMessage)
	}
	if storagev1alpha1.IsReasonTerminal(plan.readyReason) {
		t.Fatalf("%q must stay non-terminal at the snapshot level", plan.readyReason)
	}
}

// TestContentPlanChildStalledLosesToTerminal: priority is terminal first, stall second, plain pending
// last. A tree that already has a real failure must report the failure.
func TestContentPlanChildStalledLosesToTerminal(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	stalledChild := contentWithReadyCond("child-a-stalled", metav1.ConditionFalse,
		snapshot.ReasonDataCaptureStalled, buildDataCaptureStalledMessage(stallLowerReason, stallLowerMessage))
	failedChild := contentWithReadyCond("child-b-failed", metav1.ConditionFalse,
		snapshot.ReasonVolumeCaptureFailed, "capture failed")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp, stalledChild, failedChild).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx,
		commonContentWithStatus("c", "mcp-ok", "child-a-stalled", "child-b-failed"))
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}

	if plan.readyReason != snapshot.ReasonChildrenFailed || !plan.childrenFailed {
		t.Fatalf("ready = %s/%s failed=%v, want a terminal %s", plan.readyStatus, plan.readyReason, plan.childrenFailed, snapshot.ReasonChildrenFailed)
	}
}

// TestContentPlanChildStalledBeatsPlainPending: with one stalled and one merely pending child, the
// summary reports the stall. A generic ChildrenPending would hide the only actionable fact, and an
// early warning costs less than a hidden one.
func TestContentPlanChildStalledBeatsPlainPending(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	stalledChild := contentWithReadyCond("child-z-stalled", metav1.ConditionFalse,
		snapshot.ReasonDataCaptureStalled, buildDataCaptureStalledMessage(stallLowerReason, stallLowerMessage))
	pendingChild := contentWithReadyCond("child-a-pending", metav1.ConditionFalse,
		snapshot.ReasonArtifactNotReady, "vsc not readyToUse")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp, stalledChild, pendingChild).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx,
		commonContentWithStatus("c", "mcp-ok", "child-a-pending", "child-z-stalled"))
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}

	if plan.readyReason != snapshot.ReasonDataCaptureStalled {
		t.Fatalf("ready reason = %q, want %q", plan.readyReason, snapshot.ReasonDataCaptureStalled)
	}
	if !strings.Contains(plan.readyMessage, "ready=0/2") {
		t.Fatalf("message must keep the progress of the other legs, got %q", plan.readyMessage)
	}
}

// TestChildrenStalledMessageStaysFlat: the diagnosis is pinned to the original leaf instead of being
// nested at every level, so the message does not grow with tree depth, and multiple stalled children
// render deterministically.
func TestChildrenStalledMessageStaysFlat(t *testing.T) {
	leafMessage := buildDataCaptureStalledMessage(stallLowerReason, stallLowerMessage)
	direct := buildChildrenStalledMessage("disk-content", "disk-content", leafMessage, 1, 0, 1)

	cond := &metav1.Condition{Reason: snapshot.ReasonDataCaptureStalled, Message: direct}
	leaf, message := childStalledLeafInfo("vm-content", cond)
	if leaf != "disk-content" {
		t.Fatalf("leaf = %q, want the original stalled leaf", leaf)
	}
	if message != leafMessage {
		t.Fatalf("message = %q, want the leaf diagnosis unnested", message)
	}

	grandparent := buildChildrenStalledMessage("vm-content", leaf, message, 3, 1, 4)
	if strings.Count(grandparent, "no observable progress") != 1 {
		t.Fatalf("the diagnosis must appear exactly once, got %q", grandparent)
	}
	for _, want := range []string{"vm-content", "disk-content", "stalled=3", "ready=1/4"} {
		if !strings.Contains(grandparent, want) {
			t.Fatalf("message %q must mention %q", grandparent, want)
		}
	}
}

// TestStalledChildIsNotTerminalChildFailure keeps the two child classifications apart: a stall is
// pending, a terminal reason fails the tree.
func TestStalledChildIsNotTerminalChildFailure(t *testing.T) {
	if isTerminalChildContentFailure(snapshot.ReasonDataCaptureStalled) {
		t.Fatalf("%q must not be a terminal child failure", snapshot.ReasonDataCaptureStalled)
	}
	if !isStalledChildContent(snapshot.ReasonDataCaptureStalled) {
		t.Fatalf("%q must be recognised as a stalled child", snapshot.ReasonDataCaptureStalled)
	}
	if isStalledChildContent(snapshot.ReasonDataCapturePending) {
		t.Fatalf("plain pending is not a stall")
	}
}
