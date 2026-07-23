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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
)

// ownerWithDomainPhase builds an unstructured owning Snapshot carrying
// status.captureState.domainSpecificController.phase (+ optional reason/message).
func ownerWithDomainPhase(phase, reason, message string) *unstructured.Unstructured {
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot"))
	o.SetNamespace("ns1")
	o.SetName("owner")
	if phase != "" {
		_ = unstructured.SetNestedField(o.Object, phase, "status", "captureState", "domainSpecificController", "phase")
	}
	if reason != "" {
		_ = unstructured.SetNestedField(o.Object, reason, "status", "captureState", "domainSpecificController", "reason")
	}
	if message != "" {
		_ = unstructured.SetNestedField(o.Object, message, "status", "captureState", "domainSpecificController", "message")
	}
	return o
}

// This test pins CURRENT (51eb6c2) applyDomainPhaseFold behavior. For the SnapshotContent's own Ready
// (forContent=true), phase=Failed becomes canonical ReasonDomainCaptureFailed; for the namespaced owner
// (forContent=false), the raw reason is still kept. The active target updates these expectations so both
// surfaces use DomainCaptureFailed with the original reason/message in Condition.Message; childrenSettled
// can then read Ready only.
func TestApplyDomainPhaseFold(t *testing.T) {
	const (
		baseReason  = snapshot.ReasonCompleted
		baseMessage = "manifests, data, and child content are ready"
	)
	tests := []struct {
		name        string
		owner       *unstructured.Unstructured
		forContent  bool
		inStatus    metav1.ConditionStatus
		wantStatus  metav1.ConditionStatus
		wantReason  string
		wantMsgPart string
	}{
		{
			name:       "nil owner is a verbatim no-op",
			owner:      nil,
			forContent: true,
			inStatus:   metav1.ConditionTrue,
			wantStatus: metav1.ConditionTrue,
			wantReason: baseReason,
		},
		{
			name:       "non-domain owner (no phase) mirrors verbatim",
			owner:      ownerWithDomainPhase("", "", ""),
			forContent: true,
			inStatus:   metav1.ConditionTrue,
			wantStatus: metav1.ConditionTrue,
			wantReason: baseReason,
		},
		{
			name:       "phase Finished finalizes Ready",
			owner:      ownerWithDomainPhase(string(storagev1alpha1.SnapshotCapturePhaseFinished), "", ""),
			forContent: true,
			inStatus:   metav1.ConditionTrue,
			wantStatus: metav1.ConditionTrue,
			wantReason: baseReason,
		},
		{
			name:       "phase Planned holds content Ready as ChildrenPending",
			owner:      ownerWithDomainPhase(string(storagev1alpha1.SnapshotCapturePhasePlanned), "", ""),
			forContent: true,
			inStatus:   metav1.ConditionTrue,
			wantStatus: metav1.ConditionFalse,
			wantReason: snapshot.ReasonChildrenPending,
		},
		{
			name:        "phase Failed on content -> canonical terminal DomainCaptureFailed (raw reason in message)",
			owner:       ownerWithDomainPhase(string(storagev1alpha1.SnapshotCapturePhaseFailed), "ConsistencyDeadlineExceeded", "fsfreeze deadline elapsed"),
			forContent:  true,
			inStatus:    metav1.ConditionTrue,
			wantStatus:  metav1.ConditionFalse,
			wantReason:  snapshot.ReasonDomainCaptureFailed,
			wantMsgPart: "ConsistencyDeadlineExceeded",
		},
		{
			name:        "phase Failed on owner snapshot mirror -> keeps raw domain reason",
			owner:       ownerWithDomainPhase(string(storagev1alpha1.SnapshotCapturePhaseFailed), "ConsistencyDeadlineExceeded", "fsfreeze deadline elapsed"),
			forContent:  false,
			inStatus:    metav1.ConditionTrue,
			wantStatus:  metav1.ConditionFalse,
			wantReason:  "ConsistencyDeadlineExceeded",
			wantMsgPart: "fsfreeze deadline elapsed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotStatus, gotReason, gotMessage := applyDomainPhaseFold(tt.owner, tt.forContent, tt.inStatus, baseReason, baseMessage)
			if gotStatus != tt.wantStatus || gotReason != tt.wantReason {
				t.Fatalf("fold = %s/%s, want %s/%s", gotStatus, gotReason, tt.wantStatus, tt.wantReason)
			}
			if tt.wantMsgPart != "" && !strings.Contains(gotMessage, tt.wantMsgPart) {
				t.Fatalf("fold message %q must contain %q", gotMessage, tt.wantMsgPart)
			}
		})
	}
}

// A domain child that terminally failed (phase=Failed) must make its OWN SnapshotContent Ready=False with
// the canonical terminal ReasonDomainCaptureFailed — not stay Ready=True over a failed capture. Legs are all
// green (MCP ready, no data, no children), so only the domain-phase fold can flip Ready. This is the leaf
// half of the reported bug (a DemoVirtualMachineSnapshot that hit ConsistencyDeadlineExceeded).
func TestReconcileContentReady_DomainPhaseFailedTerminalOnOwnContent(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	content := commonContentWithStatus("vm-content", "mcp-ok")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp, content).WithStatusSubresource(content).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	owner := ownerWithDomainPhase(string(storagev1alpha1.SnapshotCapturePhaseFailed), "ConsistencyDeadlineExceeded", "fsfreeze deadline elapsed")
	ready, err := r.reconcileCommonSnapshotContentStatus(ctx, content, false, "", "", owner)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if ready {
		t.Fatalf("content Ready must be False when the owning domain snapshot reported phase=Failed")
	}

	fresh := &unstructured.Unstructured{}
	fresh.SetGroupVersionKind(unifiedbootstrap.CommonSnapshotContentGVK())
	if err := cl.Get(ctx, client.ObjectKey{Name: "vm-content"}, fresh); err != nil {
		t.Fatalf("get content: %v", err)
	}
	contentLike, err := snapshot.ExtractSnapshotContentLike(fresh)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	rd := snapshot.GetCondition(contentLike, snapshot.ConditionReady)
	if rd == nil || rd.Status != metav1.ConditionFalse || rd.Reason != snapshot.ReasonDomainCaptureFailed {
		t.Fatalf("content Ready = %#v, want False/%s", rd, snapshot.ReasonDomainCaptureFailed)
	}
	if !isTerminalChildContentFailure(rd.Reason) {
		t.Fatalf("reason %q must be classified terminal so it propagates as ChildrenFailed", rd.Reason)
	}
}

// While the owning domain snapshot has not reported phase=Finished (still Planned), the content must NOT
// finalize Ready=True even though all legs are green — it holds Ready=False/ChildrenPending. This is the
// pre-Finished (barrier-2) half of the reported bug: it prevents the parent from going Ready before the
// domain finishes its consistency actions (fs freeze/unfreeze, verify).
func TestReconcileContentReady_DomainPhaseNotFinishedHoldsChildrenPending(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	content := commonContentWithStatus("vm-content", "mcp-ok")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp, content).WithStatusSubresource(content).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	owner := ownerWithDomainPhase(string(storagev1alpha1.SnapshotCapturePhasePlanned), "", "")
	ready, err := r.reconcileCommonSnapshotContentStatus(ctx, content, false, "", "", owner)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if ready {
		t.Fatalf("content Ready must be False while the domain phase is not Finished")
	}

	fresh := &unstructured.Unstructured{}
	fresh.SetGroupVersionKind(unifiedbootstrap.CommonSnapshotContentGVK())
	if err := cl.Get(ctx, client.ObjectKey{Name: "vm-content"}, fresh); err != nil {
		t.Fatalf("get content: %v", err)
	}
	contentLike, err := snapshot.ExtractSnapshotContentLike(fresh)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	rd := snapshot.GetCondition(contentLike, snapshot.ConditionReady)
	if rd == nil || rd.Status != metav1.ConditionFalse || rd.Reason != snapshot.ReasonChildrenPending {
		t.Fatalf("content Ready = %#v, want False/%s", rd, snapshot.ReasonChildrenPending)
	}
}

// The parent (root) content must NOT read Ready=True when a child content is terminally failed via
// ReasonDomainCaptureFailed: it aggregates as ChildrenFailed, carrying the child name/message up. This is
// the aggregation half of the reported bug (root demo-tree Ready=True over a failed DemoVirtualMachineSnapshot
// child) and guards that a domain-phase terminal now propagates through the content tree like a data-leg one.
func TestContentPlanChildrenFailed_DomainCaptureFailedChild(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	failedChild := contentWithReadyCond("vm-child", metav1.ConditionFalse, snapshot.ReasonDomainCaptureFailed, "domain capture failed: ConsistencyDeadlineExceeded: fsfreeze deadline elapsed")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp, failedChild).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, commonContentWithStatus("root", "mcp-ok", "vm-child"))
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.childrenReady != metav1.ConditionFalse || !plan.childrenFailed {
		t.Fatalf("childrenReady=%s failed=%v, want False/terminal", plan.childrenReady, plan.childrenFailed)
	}
	if plan.readyStatus != metav1.ConditionFalse || plan.readyReason != snapshot.ReasonChildrenFailed {
		t.Fatalf("ready=%s/%s, want False/%s", plan.readyStatus, plan.readyReason, snapshot.ReasonChildrenFailed)
	}
	if !strings.Contains(plan.readyMessage, "vm-child") {
		t.Fatalf("ready message %q must name the failed child", plan.readyMessage)
	}
}
