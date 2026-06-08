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

package demo

import (
	"context"
	"testing"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestDemoSnapshotContentManifestHandoffComplete pins the post-publish contract: handoff completeness
// is decided by PUBLICATION (manifestCheckpointName) + CONTROLLER OWNERSHIP (MCP carries the
// SnapshotContent ownerRef with Controller=true), NOT by current MCP readiness. A published+handed-off
// MCP that is Ready=False/Failed must still count as complete so the demo reconciler does not re-capture
// (which previously masked the failure). A non-controller/decorative ownerRef does NOT count.
func TestDemoSnapshotContentManifestHandoffComplete(t *testing.T) {
	ctx := context.Background()
	cl := newDemoSourceRefFakeClient(t)
	contentName := "demodiskc-abc"
	ctrlTrue := true

	content := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: contentName},
		Status: storagev1alpha1.SnapshotContentStatus{
			ManifestCheckpointName: "mcp-1",
		},
	}
	// MCP owned (Controller=true) by the SnapshotContent (handoff done) and intentionally Ready=False/Failed.
	mcp := &ssv1alpha1.ManifestCheckpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name: "mcp-1",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
				Kind:       "SnapshotContent",
				Name:       contentName,
				Controller: &ctrlTrue,
			}},
		},
		Status: ssv1alpha1.ManifestCheckpointStatus{
			Conditions: []metav1.Condition{{
				Type:   ssv1alpha1.ManifestCheckpointConditionTypeReady,
				Status: metav1.ConditionFalse,
				Reason: ssv1alpha1.ManifestCheckpointConditionReasonFailed,
			}},
		},
	}
	if err := cl.Create(ctx, content); err != nil {
		t.Fatalf("create content: %v", err)
	}
	if err := cl.Create(ctx, mcp); err != nil {
		t.Fatalf("create MCP: %v", err)
	}
	if err := cl.Status().Update(ctx, content); err != nil {
		t.Fatalf("update content status: %v", err)
	}

	ok, err := demoSnapshotContentManifestHandoffComplete(ctx, cl, contentName)
	if err != nil {
		t.Fatalf("handoff complete: %v", err)
	}
	if !ok {
		t.Fatal("expected handoff complete for an owned (controller handed-off) MCP even though it is Ready=False/Failed")
	}

	// Non-controller (decorative) SnapshotContent ownerRef: NOT a durable handoff.
	mcp.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
		Kind:       "SnapshotContent",
		Name:       contentName,
	}}
	if err := cl.Update(ctx, mcp); err != nil {
		t.Fatalf("set non-controller MCP ownerRef: %v", err)
	}
	ok, err = demoSnapshotContentManifestHandoffComplete(ctx, cl, contentName)
	if err != nil {
		t.Fatalf("handoff (non-controller ownerRef): %v", err)
	}
	if ok {
		t.Fatal("expected handoff incomplete for a non-controller SnapshotContent ownerRef")
	}

	// MCP no longer owned by the SnapshotContent (pre-handoff window): not complete.
	mcp.OwnerReferences = nil
	if err := cl.Update(ctx, mcp); err != nil {
		t.Fatalf("clear MCP ownerRefs: %v", err)
	}
	ok, err = demoSnapshotContentManifestHandoffComplete(ctx, cl, contentName)
	if err != nil {
		t.Fatalf("handoff (unowned MCP): %v", err)
	}
	if ok {
		t.Fatal("expected handoff incomplete when MCP is not owned by the SnapshotContent")
	}

	// No manifestCheckpointName published: not complete.
	content.Status.ManifestCheckpointName = ""
	if err := cl.Status().Update(ctx, content); err != nil {
		t.Fatalf("update content: %v", err)
	}
	ok, err = demoSnapshotContentManifestHandoffComplete(ctx, cl, contentName)
	if err != nil {
		t.Fatalf("handoff incomplete: %v", err)
	}
	if ok {
		t.Fatal("expected handoff incomplete without manifestCheckpointName")
	}
}
