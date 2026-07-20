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

// ownerWithManifestCaptured builds an unstructured owning Snapshot carrying the monotonic
// status.captureState.commonController.manifestCaptured latch. Once true, this node's manifest artifact was
// produced and the content's manifestCheckpointName published, so a subsequently-missing MCP is a
// post-capture loss (not the publish-before-create window).
func ownerWithManifestCaptured(captured bool) *unstructured.Unstructured {
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot"))
	o.SetNamespace("ns1")
	o.SetName("owner")
	_ = unstructured.SetNestedField(o.Object, captured, "status", "captureState", "commonController", "manifestCaptured")
	return o
}

func contentReadyCondition(ctx context.Context, t *testing.T, cl client.Client, name string) *metav1.Condition {
	t.Helper()
	fresh := &unstructured.Unstructured{}
	fresh.SetGroupVersionKind(unifiedbootstrap.CommonSnapshotContentGVK())
	if err := cl.Get(ctx, client.ObjectKey{Name: name}, fresh); err != nil {
		t.Fatalf("get content %s: %v", name, err)
	}
	contentLike, err := snapshot.ExtractSnapshotContentLike(fresh)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	return snapshot.GetCondition(contentLike, snapshot.ConditionReady)
}

// After the owner's manifest-capture latch is set, a published ManifestCheckpoint that is genuinely absent
// (deleted after capture) must flip the content to the TERMINAL Ready=False/ManifestCheckpointFailed, not
// the non-terminal ManifestCapturePending — symmetric with a missing chunk. Terminal so it propagates up
// the content tree as ChildrenFailed. This is the "delete the whole MCP after capture => failed" contract.
func TestReconcileContentReady_PublishedMCPDeletedAfterCaptureTerminal(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	// The published MCP is intentionally NOT created in the client: it was deleted after capture.
	content := commonContentWithStatus("vm-content", "mcp-gone")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(content).WithStatusSubresource(content).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	owner := ownerWithManifestCaptured(true)
	ready, err := r.reconcileCommonSnapshotContentStatus(ctx, content, false, "", "", owner)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if ready {
		t.Fatalf("content Ready must be False when the published MCP was deleted after capture")
	}

	rd := contentReadyCondition(ctx, t, cl, "vm-content")
	if rd == nil || rd.Status != metav1.ConditionFalse || rd.Reason != snapshot.ReasonManifestCheckpointFailed {
		t.Fatalf("content Ready = %#v, want False/%s", rd, snapshot.ReasonManifestCheckpointFailed)
	}
	if !isTerminalChildContentFailure(rd.Reason) {
		t.Fatalf("reason %q must be terminal so a deleted MCP propagates up as ChildrenFailed", rd.Reason)
	}
	if !strings.Contains(rd.Message, "mcp-gone") {
		t.Fatalf("Ready message %q must name the deleted ManifestCheckpoint", rd.Message)
	}
}

// BEFORE capture (latch not set), a missing published MCP is the legitimate publish-before-create window and
// MUST stay the NON-terminal ManifestCapturePending — the escalation to terminal only kicks in post-latch.
// A nil owner (ownerless/bucket content, exported test entry) behaves the same as an un-latched owner.
func TestReconcileContentReady_MCPAbsentBeforeCaptureStaysPending(t *testing.T) {
	cases := []struct {
		name  string
		owner *unstructured.Unstructured
	}{
		{name: "latch not set", owner: ownerWithManifestCaptured(false)},
		{name: "nil owner", owner: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			scheme := aggScheme(t)
			content := commonContentWithStatus("vm-content", "mcp-missing")
			cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(content).WithStatusSubresource(content).Build()
			r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

			ready, err := r.reconcileCommonSnapshotContentStatus(ctx, content, false, "", "", tc.owner)
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if ready {
				t.Fatalf("content Ready must be False while the MCP is not yet published/created")
			}
			rd := contentReadyCondition(ctx, t, cl, "vm-content")
			if rd == nil || rd.Status != metav1.ConditionFalse || rd.Reason != snapshot.ReasonManifestCapturePending {
				t.Fatalf("content Ready = %#v, want False/%s (non-terminal)", rd, snapshot.ReasonManifestCapturePending)
			}
			if isTerminalChildContentFailure(rd.Reason) {
				t.Fatalf("reason %q must be NON-terminal before capture (publish-before-create window)", rd.Reason)
			}
		})
	}
}

// With the latch set AND the published MCP still present-and-Ready, there must be NO false escalation: the
// content finalizes Ready=True/Completed. Guards that the post-capture integrity check only fires on a
// genuinely-absent MCP, never on the happy path.
func TestReconcileContentReady_MCPPresentAfterCaptureNoFalseEscalation(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	content := commonContentWithStatus("vm-content", "mcp-ok")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp, content).WithStatusSubresource(content).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	owner := ownerWithManifestCaptured(true)
	ready, err := r.reconcileCommonSnapshotContentStatus(ctx, content, false, "", "", owner)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !ready {
		t.Fatalf("content Ready must be True when the published MCP is present and Ready")
	}
	rd := contentReadyCondition(ctx, t, cl, "vm-content")
	if rd == nil || rd.Status != metav1.ConditionTrue || rd.Reason != snapshot.ReasonCompleted {
		t.Fatalf("content Ready = %#v, want True/%s", rd, snapshot.ReasonCompleted)
	}
}
