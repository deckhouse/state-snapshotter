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
	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/common"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/snapshotcontent"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	deckhousev1alpha1 "github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1"
	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
)

func TestDemoVirtualDiskSnapshot_InvalidSourceRefDoesNotCreateMCR(t *testing.T) {
	tests := []struct {
		name      string
		sourceRef demov1alpha1.SnapshotSourceRef
	}{
		{
			name: "missing sourceRef",
		},
		{
			name: "wrong kind",
			sourceRef: demov1alpha1.SnapshotSourceRef{
				APIVersion: demov1alpha1.SchemeGroupVersion.String(),
				Kind:       controllercommon.KindDemoVirtualMachine,
				Name:       "disk-a",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cl := newDemoSourceRefFakeClient(t, &demov1alpha1.DemoVirtualDiskSnapshot{
				ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "ns1"},
				Spec: demov1alpha1.DemoVirtualDiskSnapshotSpec{
					SourceRef: tt.sourceRef,
				},
			})
			reconciler := &DemoVirtualDiskSnapshotReconciler{Client: cl}
			if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "snap"}}); err != nil {
				t.Fatalf("reconcile failed: %v", err)
			}

			snap := getDemoDiskSnapshot(t, cl)
			ready := meta.FindStatusCondition(snap.Status.Conditions, snapshot.ConditionReady)
			if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "InvalidSourceRef" {
				t.Fatalf("expected Ready=False InvalidSourceRef, got %#v", ready)
			}
			assertNoDemoDiskContents(t, cl)
			assertNoDemoMCRs(t, cl)
		})
	}
}

func TestDemoVirtualDiskSnapshot_SourceNotFoundDoesNotCreateContentOrMCR(t *testing.T) {
	cl := newDemoSourceRefFakeClient(t, &demov1alpha1.DemoVirtualDiskSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "ns1"},
		Spec: demov1alpha1.DemoVirtualDiskSnapshotSpec{
			SourceRef: demov1alpha1.SnapshotSourceRef{
				APIVersion: demov1alpha1.SchemeGroupVersion.String(),
				Kind:       controllercommon.KindDemoVirtualDisk,
				Name:       "missing-disk",
			},
		},
	})
	reconciler := &DemoVirtualDiskSnapshotReconciler{Client: cl}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "snap"}}); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	snap := getDemoDiskSnapshot(t, cl)
	ready := meta.FindStatusCondition(snap.Status.Conditions, snapshot.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "SourceNotFound" {
		t.Fatalf("expected Ready=False SourceNotFound, got %#v", ready)
	}
	assertNoDemoDiskContents(t, cl)
	assertNoDemoMCRs(t, cl)
}

func TestDemoVirtualDiskSnapshot_HappyPathCreatesContentMCRAndCompletes(t *testing.T) {
	cl := newDemoSourceRefFakeClient(t,
		&demov1alpha1.DemoVirtualDisk{
			ObjectMeta: metav1.ObjectMeta{Name: "disk-a", Namespace: "ns1"},
		},
		&demov1alpha1.DemoVirtualDiskSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "ns1", UID: "snap-uid"},
			Spec: demov1alpha1.DemoVirtualDiskSnapshotSpec{
				SourceRef: demov1alpha1.SnapshotSourceRef{
					APIVersion: demov1alpha1.SchemeGroupVersion.String(),
					Kind:       controllercommon.KindDemoVirtualDisk,
					Name:       "disk-a",
				},
			},
		},
	)
	reconciler := &DemoVirtualDiskSnapshotReconciler{Client: cl}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "snap"}}

	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("first reconcile failed: %v", err)
	}

	contentName := demoVirtualDiskSnapshotContentName("ns1", "snap")
	content := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: contentName}, content); err != nil {
		t.Fatalf("expected content %q: %v", contentName, err)
	}
	mcrName := demoSnapshotManifestCaptureRequestName(controllercommon.KindDemoVirtualDiskSnapshot, "ns1", "snap")
	mcr := &ssv1alpha1.ManifestCaptureRequest{}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "ns1", Name: mcrName}, mcr); err != nil {
		t.Fatalf("expected MCR %q: %v", mcrName, err)
	}
	expectedTargets := []ssv1alpha1.ManifestTarget{{
		APIVersion: demov1alpha1.SchemeGroupVersion.String(),
		Kind:       controllercommon.KindDemoVirtualDisk,
		Name:       "disk-a",
	}}
	if !equality.Semantic.DeepEqual(mcr.Spec.Targets, expectedTargets) {
		t.Fatalf("unexpected MCR targets: %#v", mcr.Spec.Targets)
	}

	baseMCR := mcr.DeepCopy()
	mcr.Status.CheckpointName = "mcp-disk"
	if err := cl.Status().Patch(context.Background(), mcr, client.MergeFrom(baseMCR)); err != nil {
		t.Fatalf("patch MCR status: %v", err)
	}
	mcp := &ssv1alpha1.ManifestCheckpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "mcp-disk"},
		Spec: ssv1alpha1.ManifestCheckpointSpec{
			SourceNamespace: "ns1",
			ManifestCaptureRequestRef: &ssv1alpha1.ObjectReference{
				Name:      mcrName,
				Namespace: "ns1",
				UID:       string(mcr.UID),
			},
		},
		Status: ssv1alpha1.ManifestCheckpointStatus{
			Conditions: []metav1.Condition{{
				Type:   ssv1alpha1.ManifestCheckpointConditionTypeReady,
				Status: metav1.ConditionTrue,
				Reason: ssv1alpha1.ManifestCheckpointConditionReasonCompleted,
			}},
		},
	}
	if err := cl.Create(context.Background(), mcp); err != nil {
		t.Fatalf("create MCP: %v", err)
	}

	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("second reconcile failed: %v", err)
	}
	reconcileCommonSnapshotContentStatusForTest(t, cl, contentName)
	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("third reconcile failed: %v", err)
	}

	snap := getDemoDiskSnapshot(t, cl)
	if snap.Status.BoundSnapshotContentName != contentName {
		t.Fatalf("expected bound content %q, got %q", contentName, snap.Status.BoundSnapshotContentName)
	}
	assertDemoSnapshotNotOwnedBy(t, snap, controllercommon.DeckhouseAPIVersion, controllercommon.KindObjectKeeper, controllercommon.RootObjectKeeperName("ns1", demov1alpha1.SchemeGroupVersion.String(), controllercommon.KindDemoVirtualDiskSnapshot, "snap"))
	ready := meta.FindStatusCondition(snap.Status.Conditions, snapshot.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionTrue || ready.Reason != snapshot.ReasonCompleted {
		t.Fatalf("expected Ready=True Completed, got %#v", ready)
	}
	domainReady := meta.FindStatusCondition(snap.Status.Conditions, snapshot.ConditionDomainReady)
	if domainReady == nil || domainReady.Status != metav1.ConditionTrue {
		t.Fatalf("expected DomainReady=True for leaf disk snapshot, got %#v", domainReady)
	}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: contentName}, content); err != nil {
		t.Fatalf("get content after ready: %v", err)
	}
	assertDemoOwnerRef(t, content.OwnerReferences, controllercommon.DeckhouseAPIVersion, controllercommon.KindObjectKeeper, controllercommon.RootObjectKeeperName("ns1", demov1alpha1.SchemeGroupVersion.String(), controllercommon.KindDemoVirtualDiskSnapshot, "snap"), true)
	assertNoSnapshotContentOwnerRefToSnapshot(t, content)
	if content.Status.ManifestCheckpointName != "mcp-disk" {
		t.Fatalf("expected content MCP link %q, got %q", "mcp-disk", content.Status.ManifestCheckpointName)
	}
	contentReady := meta.FindStatusCondition(content.Status.Conditions, snapshot.ConditionReady)
	if contentReady == nil || contentReady.Status != metav1.ConditionTrue || contentReady.Reason != snapshot.ReasonCompleted {
		t.Fatalf("expected content Ready=True Completed, got %#v", contentReady)
	}
	if len(content.Status.DataRefs) != 0 {
		t.Fatalf("state-only snapshot content must not require or set dataRefs, got %#v", content.Status.DataRefs)
	}

	mcpBefore := content.Status.ManifestCheckpointName
	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("fourth reconcile (steady state) failed: %v", err)
	}
	assertNoDemoMCRs(t, cl)
	if err := cl.Get(context.Background(), client.ObjectKey{Name: contentName}, content); err != nil {
		t.Fatalf("get content after steady reconcile: %v", err)
	}
	if content.Status.ManifestCheckpointName != mcpBefore {
		t.Fatalf("steady reconcile must not change manifestCheckpointName: was %q, got %q", mcpBefore, content.Status.ManifestCheckpointName)
	}
	snap = getDemoDiskSnapshot(t, cl)
	if snap.Status.ManifestCaptureRequestName != "" {
		t.Fatalf("expected empty manifestCaptureRequestName after steady reconcile, got %q", snap.Status.ManifestCaptureRequestName)
	}
	ready = meta.FindStatusCondition(snap.Status.Conditions, snapshot.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionTrue {
		t.Fatalf("expected snapshot to stay Ready=True after steady reconcile, got %#v", ready)
	}
}

// TestDemoVirtualDiskSnapshot_FailedHandedOffMCPDoesNotRecapture mirrors live tree-demo Stage 06.
// Once the demo disk snapshot has published its ManifestCheckpoint and the MCP is handed off (owned)
// by the SnapshotContent, patching that MCP Ready=False/Failed is a durable post-publish degradation:
// SnapshotContentController must degrade the content (RequestsReady=False/ManifestCheckpointFailed),
// the demo snapshot must mirror Ready=False, and the demo reconciler must NOT create a fresh MCR/MCP
// (no re-capture that would silently mask the failure).
func TestDemoVirtualDiskSnapshot_FailedHandedOffMCPDoesNotRecapture(t *testing.T) {
	cl := newDemoSourceRefFakeClient(t,
		&demov1alpha1.DemoVirtualDisk{
			ObjectMeta: metav1.ObjectMeta{Name: "disk-a", Namespace: "ns1"},
		},
		&demov1alpha1.DemoVirtualDiskSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "ns1", UID: "snap-uid"},
			Spec: demov1alpha1.DemoVirtualDiskSnapshotSpec{
				SourceRef: demov1alpha1.SnapshotSourceRef{
					APIVersion: demov1alpha1.SchemeGroupVersion.String(),
					Kind:       controllercommon.KindDemoVirtualDisk,
					Name:       "disk-a",
				},
			},
		},
	)
	reconciler := &DemoVirtualDiskSnapshotReconciler{Client: cl}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "snap"}}
	contentName := demoVirtualDiskSnapshotContentName("ns1", "snap")
	mcrName := demoSnapshotManifestCaptureRequestName(controllercommon.KindDemoVirtualDiskSnapshot, "ns1", "snap")

	// Drive capture to publication: create MCR, complete its MCP.
	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("first reconcile failed: %v", err)
	}
	mcr := &ssv1alpha1.ManifestCaptureRequest{}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "ns1", Name: mcrName}, mcr); err != nil {
		t.Fatalf("expected MCR %q: %v", mcrName, err)
	}
	baseMCR := mcr.DeepCopy()
	mcr.Status.CheckpointName = "mcp-disk"
	if err := cl.Status().Patch(context.Background(), mcr, client.MergeFrom(baseMCR)); err != nil {
		t.Fatalf("patch MCR status: %v", err)
	}
	mcp := &ssv1alpha1.ManifestCheckpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "mcp-disk"},
		Spec: ssv1alpha1.ManifestCheckpointSpec{
			SourceNamespace: "ns1",
			ManifestCaptureRequestRef: &ssv1alpha1.ObjectReference{
				Name:      mcrName,
				Namespace: "ns1",
				UID:       string(mcr.UID),
			},
		},
		Status: ssv1alpha1.ManifestCheckpointStatus{
			Conditions: []metav1.Condition{{
				Type:   ssv1alpha1.ManifestCheckpointConditionTypeReady,
				Status: metav1.ConditionTrue,
				Reason: ssv1alpha1.ManifestCheckpointConditionReasonCompleted,
			}},
		},
	}
	if err := cl.Create(context.Background(), mcp); err != nil {
		t.Fatalf("create MCP: %v", err)
	}

	// Reconcile so the content controller (run below) sees the published MCP and hands off ownership.
	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("second reconcile failed: %v", err)
	}
	reconcileCommonSnapshotContentStatusForTest(t, cl, contentName)
	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("third reconcile failed: %v", err)
	}

	// Precondition: MCP handed off to content, snapshot Ready=True, no live MCR.
	snap := getDemoDiskSnapshot(t, cl)
	if ready := meta.FindStatusCondition(snap.Status.Conditions, snapshot.ConditionReady); ready == nil || ready.Status != metav1.ConditionTrue {
		t.Fatalf("precondition: expected disk snapshot Ready=True, got %#v", ready)
	}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: "mcp-disk"}, mcp); err != nil {
		t.Fatalf("get MCP: %v", err)
	}
	if !manifestCheckpointHandedOffToContent(mcp, contentName) {
		t.Fatalf("precondition: expected MCP handed off (owned) by content %q, ownerRefs=%#v", contentName, mcp.OwnerReferences)
	}
	assertNoDemoMCRs(t, cl)

	// Inject durable post-publish failure on the handed-off MCP.
	baseMCP := mcp.DeepCopy()
	meta.SetStatusCondition(&mcp.Status.Conditions, metav1.Condition{
		Type:    ssv1alpha1.ManifestCheckpointConditionTypeReady,
		Status:  metav1.ConditionFalse,
		Reason:  ssv1alpha1.ManifestCheckpointConditionReasonFailed,
		Message: "tree-demo injected MCP failure",
	})
	if err := cl.Status().Patch(context.Background(), mcp, client.MergeFrom(baseMCP)); err != nil {
		t.Fatalf("patch MCP Ready=False/Failed: %v", err)
	}

	// SnapshotContentController observes the failed MCP and degrades the content.
	reconcileCommonSnapshotContentStatusForTest(t, cl, contentName)
	content := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: contentName}, content); err != nil {
		t.Fatalf("get content after MCP failure: %v", err)
	}
	if rr := meta.FindStatusCondition(content.Status.Conditions, snapshot.ConditionRequestsReady); rr == nil || rr.Status != metav1.ConditionFalse || rr.Reason != snapshot.ReasonManifestCheckpointFailed {
		t.Fatalf("expected content RequestsReady=False/ManifestCheckpointFailed, got %#v", rr)
	}
	if cr := meta.FindStatusCondition(content.Status.Conditions, snapshot.ConditionReady); cr == nil || cr.Status != metav1.ConditionFalse {
		t.Fatalf("expected content Ready=False after MCP failure, got %#v", cr)
	}

	// Demo reconcile after failure: mirror Ready=False, NO re-capture.
	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile after MCP failure: %v", err)
	}
	assertNoDemoMCRs(t, cl)
	mcps := &ssv1alpha1.ManifestCheckpointList{}
	if err := cl.List(context.Background(), mcps); err != nil {
		t.Fatalf("list MCPs: %v", err)
	}
	if len(mcps.Items) != 1 || mcps.Items[0].Name != "mcp-disk" {
		t.Fatalf("expected exactly the original MCP (no re-capture), got %d MCPs", len(mcps.Items))
	}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: contentName}, content); err != nil {
		t.Fatalf("get content after demo reconcile: %v", err)
	}
	if content.Status.ManifestCheckpointName != "mcp-disk" {
		t.Fatalf("content must keep its published MCP (no rebind), got %q", content.Status.ManifestCheckpointName)
	}
	snap = getDemoDiskSnapshot(t, cl)
	if ready := meta.FindStatusCondition(snap.Status.Conditions, snapshot.ConditionReady); ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != snapshot.ReasonManifestCheckpointFailed {
		t.Fatalf("expected demo disk snapshot Ready=False/ManifestCheckpointFailed mirror after MCP failure, got %#v", ready)
	}
}

func TestDemoVirtualMachineSnapshot_InvalidSourceRefDoesNotCreateContentMCROrChildren(t *testing.T) {
	tests := []struct {
		name      string
		sourceRef demov1alpha1.SnapshotSourceRef
	}{
		{
			name: "missing sourceRef",
		},
		{
			name: "wrong kind",
			sourceRef: demov1alpha1.SnapshotSourceRef{
				APIVersion: demov1alpha1.SchemeGroupVersion.String(),
				Kind:       controllercommon.KindDemoVirtualDisk,
				Name:       "vm-a",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cl := newDemoSourceRefFakeClient(t, &demov1alpha1.DemoVirtualMachineSnapshot{
				ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "ns1"},
				Spec: demov1alpha1.DemoVirtualMachineSnapshotSpec{
					SourceRef: tt.sourceRef,
				},
			})
			reconciler := &DemoVirtualMachineSnapshotReconciler{Client: cl}
			if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "snap"}}); err != nil {
				t.Fatalf("reconcile failed: %v", err)
			}

			snap := getDemoVMSnapshot(t, cl)
			ready := meta.FindStatusCondition(snap.Status.Conditions, snapshot.ConditionReady)
			if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "InvalidSourceRef" {
				t.Fatalf("expected Ready=False InvalidSourceRef, got %#v", ready)
			}
			assertNoDemoVMContents(t, cl)
			assertNoDemoMCRs(t, cl)
			assertNoDemoDiskSnapshots(t, cl)
		})
	}
}

func TestDemoVirtualMachineSnapshot_SourceNotFoundDoesNotCreateMCR(t *testing.T) {
	cl := newDemoSourceRefFakeClient(t, &demov1alpha1.DemoVirtualMachineSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "ns1"},
		Spec: demov1alpha1.DemoVirtualMachineSnapshotSpec{
			SourceRef: demov1alpha1.SnapshotSourceRef{
				APIVersion: demov1alpha1.SchemeGroupVersion.String(),
				Kind:       controllercommon.KindDemoVirtualMachine,
				Name:       "missing-vm",
			},
		},
	})
	reconciler := &DemoVirtualMachineSnapshotReconciler{Client: cl}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "snap"}}); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	snap := &demov1alpha1.DemoVirtualMachineSnapshot{}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "ns1", Name: "snap"}, snap); err != nil {
		t.Fatalf("get snapshot: %v", err)
	}
	ready := meta.FindStatusCondition(snap.Status.Conditions, snapshot.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "SourceNotFound" {
		t.Fatalf("expected Ready=False SourceNotFound, got %#v", ready)
	}
	assertNoDemoVMContents(t, cl)
	assertNoDemoMCRs(t, cl)
	assertNoDemoDiskSnapshots(t, cl)
}

func TestDemoVirtualMachineSnapshot_HappyPathCreatesOwnedDiskChildrenAndCompletes(t *testing.T) {
	vmUID := types.UID("vm-uid")
	cl := newDemoSourceRefFakeClient(t,
		&demov1alpha1.DemoVirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: "vm-a", Namespace: "ns1", UID: vmUID},
			Spec: demov1alpha1.DemoVirtualMachineSpec{
				VirtualDiskName: "disk-owned",
			},
		},
		&demov1alpha1.DemoVirtualDisk{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "disk-owned",
				Namespace: "ns1",
				UID:       "disk-owned-uid",
			},
		},
		&demov1alpha1.DemoVirtualDisk{
			ObjectMeta: metav1.ObjectMeta{Name: "disk-unowned", Namespace: "ns1"},
		},
		&demov1alpha1.DemoVirtualMachineSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "ns1", UID: "snap-uid"},
			Spec: demov1alpha1.DemoVirtualMachineSnapshotSpec{
				SourceRef: demov1alpha1.SnapshotSourceRef{
					APIVersion: demov1alpha1.SchemeGroupVersion.String(),
					Kind:       controllercommon.KindDemoVirtualMachine,
					Name:       "vm-a",
				},
			},
		},
	)
	reconciler := &DemoVirtualMachineSnapshotReconciler{Client: cl}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "snap"}}

	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("first reconcile failed: %v", err)
	}

	vmContentName := demoVirtualMachineSnapshotContentName("ns1", "snap")
	vmContent := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: vmContentName}, vmContent); err != nil {
		t.Fatalf("expected VM content %q: %v", vmContentName, err)
	}
	mcrName := demoSnapshotManifestCaptureRequestName(controllercommon.KindDemoVirtualMachineSnapshot, "ns1", "snap")
	mcr := &ssv1alpha1.ManifestCaptureRequest{}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "ns1", Name: mcrName}, mcr); err != nil {
		t.Fatalf("expected VM MCR %q: %v", mcrName, err)
	}
	expectedTargets := []ssv1alpha1.ManifestTarget{{
		APIVersion: demov1alpha1.SchemeGroupVersion.String(),
		Kind:       controllercommon.KindDemoVirtualMachine,
		Name:       "vm-a",
	}}
	if !equality.Semantic.DeepEqual(mcr.Spec.Targets, expectedTargets) {
		t.Fatalf("unexpected VM MCR targets: %#v", mcr.Spec.Targets)
	}
	assertDemoDiskSnapshotsCount(t, cl, 1)

	baseMCR := mcr.DeepCopy()
	mcr.Status.CheckpointName = "mcp-vm"
	if err := cl.Status().Patch(context.Background(), mcr, client.MergeFrom(baseMCR)); err != nil {
		t.Fatalf("patch VM MCR status: %v", err)
	}
	mcp := &ssv1alpha1.ManifestCheckpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "mcp-vm"},
		Spec: ssv1alpha1.ManifestCheckpointSpec{
			SourceNamespace: "ns1",
			ManifestCaptureRequestRef: &ssv1alpha1.ObjectReference{
				Name:      mcrName,
				Namespace: "ns1",
				UID:       string(mcr.UID),
			},
		},
		Status: ssv1alpha1.ManifestCheckpointStatus{
			Conditions: []metav1.Condition{{
				Type:   ssv1alpha1.ManifestCheckpointConditionTypeReady,
				Status: metav1.ConditionTrue,
				Reason: ssv1alpha1.ManifestCheckpointConditionReasonCompleted,
			}},
		},
	}
	if err := cl.Create(context.Background(), mcp); err != nil {
		t.Fatalf("create VM MCP: %v", err)
	}

	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("second reconcile failed: %v", err)
	}

	childName := demoVirtualMachineDiskSnapshotName("ns1", "snap", "disk-owned")
	child := &demov1alpha1.DemoVirtualDiskSnapshot{}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "ns1", Name: childName}, child); err != nil {
		t.Fatalf("expected owned disk child snapshot %q: %v", childName, err)
	}
	if child.Spec.SourceRef != (demov1alpha1.SnapshotSourceRef{
		APIVersion: demov1alpha1.SchemeGroupVersion.String(),
		Kind:       controllercommon.KindDemoVirtualDisk,
		Name:       "disk-owned",
	}) {
		t.Fatalf("unexpected child sourceRef: %#v", child.Spec.SourceRef)
	}
	assertDemoSnapshotOwnedBy(t, child, demov1alpha1.SchemeGroupVersion.String(), controllercommon.KindDemoVirtualMachineSnapshot, "snap")
	diskSnapshots := &demov1alpha1.DemoVirtualDiskSnapshotList{}
	if err := cl.List(context.Background(), diskSnapshots, client.InNamespace("ns1")); err != nil {
		t.Fatalf("list disk snapshots: %v", err)
	}
	if len(diskSnapshots.Items) != 1 {
		t.Fatalf("expected only owned disk child snapshot, got %d", len(diskSnapshots.Items))
	}
	vmSnap := getDemoVMSnapshot(t, cl)
	if !controllercommon.SnapshotChildRefsEqualIgnoreOrder(vmSnap.Status.ChildrenSnapshotRefs, []storagev1alpha1.SnapshotChildRef{{
		APIVersion: demov1alpha1.SchemeGroupVersion.String(),
		Kind:       controllercommon.KindDemoVirtualDiskSnapshot,
		Name:       childName,
	}}) {
		t.Fatalf("unexpected VM child refs: %#v", vmSnap.Status.ChildrenSnapshotRefs)
	}
	domainReady := meta.FindStatusCondition(vmSnap.Status.Conditions, snapshot.ConditionDomainReady)
	if domainReady == nil || domainReady.Status != metav1.ConditionTrue {
		t.Fatalf("expected VM DomainReady=True after writing child refs, got %#v", domainReady)
	}
	// A1 (Slice 3): after bind the VM mirrors the bound SnapshotContent.Ready reason instead of writing a
	// local ChildrenPending. No SnapshotContentController runs in this direct-reconcile unit test, so the
	// content has no Ready condition yet and the mirror falls back to ContentBindingPending.
	ready := meta.FindStatusCondition(vmSnap.Status.Conditions, snapshot.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != snapshot.ReasonContentBindingPending {
		t.Fatalf("expected Ready=False mirrored from bound content (ContentBindingPending) before child content ready, got %#v", ready)
	}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: vmContentName}, vmContent); err != nil {
		t.Fatalf("get VM content while child pending: %v", err)
	}
	if vmContent.Status.ManifestCheckpointName != "" {
		t.Fatalf("parent content must not publish manifest ref before child content graph is complete, got %q", vmContent.Status.ManifestCheckpointName)
	}
	if len(vmContent.Status.ChildrenSnapshotContentRefs) != 0 {
		t.Fatalf("parent content must not publish incomplete child content refs, got %#v", vmContent.Status.ChildrenSnapshotContentRefs)
	}

	diskContentName := "disk-content"
	if err := cl.Create(context.Background(), &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: diskContentName},
		Spec:       storagev1alpha1.SnapshotContentSpec{},
	}); err != nil {
		t.Fatalf("create disk content: %v", err)
	}
	diskContent := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: diskContentName}, diskContent); err != nil {
		t.Fatalf("get disk content: %v", err)
	}
	baseDiskContent := diskContent.DeepCopy()
	meta.SetStatusCondition(&diskContent.Status.Conditions, metav1.Condition{
		Type:   snapshot.ConditionReady,
		Status: metav1.ConditionTrue,
		Reason: snapshot.ReasonCompleted,
	})
	if err := cl.Status().Patch(context.Background(), diskContent, client.MergeFrom(baseDiskContent)); err != nil {
		t.Fatalf("patch disk content Ready: %v", err)
	}
	baseChild := child.DeepCopy()
	child.Status.BoundSnapshotContentName = diskContentName
	meta.SetStatusCondition(&child.Status.Conditions, metav1.Condition{
		Type:   snapshot.ConditionReady,
		Status: metav1.ConditionTrue,
		Reason: snapshot.ReasonCompleted,
	})
	if err := cl.Status().Patch(context.Background(), child, client.MergeFrom(baseChild)); err != nil {
		t.Fatalf("patch child status: %v", err)
	}

	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("third reconcile failed: %v", err)
	}
	reconcileCommonSnapshotContentStatusForTest(t, cl, vmContentName)
	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("fourth reconcile failed: %v", err)
	}

	vmSnap = getDemoVMSnapshot(t, cl)
	if vmSnap.Status.BoundSnapshotContentName != vmContentName {
		t.Fatalf("expected VM bound content %q, got %q", vmContentName, vmSnap.Status.BoundSnapshotContentName)
	}
	assertDemoSnapshotNotOwnedBy(t, vmSnap, controllercommon.DeckhouseAPIVersion, controllercommon.KindObjectKeeper, controllercommon.RootObjectKeeperName("ns1", demov1alpha1.SchemeGroupVersion.String(), controllercommon.KindDemoVirtualMachineSnapshot, "snap"))
	ready = meta.FindStatusCondition(vmSnap.Status.Conditions, snapshot.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionTrue || ready.Reason != snapshot.ReasonCompleted {
		t.Fatalf("expected VM Ready=True Completed, got %#v", ready)
	}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: vmContentName}, vmContent); err != nil {
		t.Fatalf("get VM content after ready: %v", err)
	}
	if vmContent.Status.ManifestCheckpointName != "mcp-vm" {
		t.Fatalf("expected VM content MCP link %q, got %q", "mcp-vm", vmContent.Status.ManifestCheckpointName)
	}
	assertDemoOwnerRef(t, vmContent.OwnerReferences, controllercommon.DeckhouseAPIVersion, controllercommon.KindObjectKeeper, controllercommon.RootObjectKeeperName("ns1", demov1alpha1.SchemeGroupVersion.String(), controllercommon.KindDemoVirtualMachineSnapshot, "snap"), true)
	assertNoSnapshotContentOwnerRefToSnapshot(t, vmContent)
	if !controllercommon.SnapshotContentChildRefsEqualIgnoreOrder(vmContent.Status.ChildrenSnapshotContentRefs, []storagev1alpha1.SnapshotContentChildRef{{Name: diskContentName}}) {
		t.Fatalf("unexpected VM content child refs: %#v", vmContent.Status.ChildrenSnapshotContentRefs)
	}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: diskContentName}, diskContent); err != nil {
		t.Fatalf("get disk content after parent publish: %v", err)
	}
	assertDemoOwnerRef(t, diskContent.OwnerReferences, storagev1alpha1.SchemeGroupVersion.String(), "SnapshotContent", vmContentName, true)
	assertNoSnapshotContentOwnerRefToSnapshot(t, diskContent)
	if len(vmContent.Status.DataRefs) != 0 {
		t.Fatalf("state-only VM content must not require or set dataRefs, got %#v", vmContent.Status.DataRefs)
	}
}

func TestDemoVirtualMachineSnapshot_DoesNotStealConflictingDiskChildOwner(t *testing.T) {
	vmUID := types.UID("vm-uid")
	childName := demoVirtualMachineDiskSnapshotName("ns1", "snap", "disk-owned")
	conflictingOwner := metav1.OwnerReference{
		APIVersion: demov1alpha1.SchemeGroupVersion.String(),
		Kind:       controllercommon.KindDemoVirtualMachineSnapshot,
		Name:       "other-vm-snapshot",
		UID:        types.UID("other-uid"),
	}
	cl := newDemoSourceRefFakeClient(t,
		&demov1alpha1.DemoVirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: "vm-a", Namespace: "ns1", UID: vmUID},
			Spec: demov1alpha1.DemoVirtualMachineSpec{
				VirtualDiskName: "disk-owned",
			},
		},
		&demov1alpha1.DemoVirtualDisk{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "disk-owned",
				Namespace: "ns1",
				UID:       "disk-owned-uid",
			},
		},
		&demov1alpha1.DemoVirtualDiskSnapshot{
			ObjectMeta: metav1.ObjectMeta{
				Name:            childName,
				Namespace:       "ns1",
				OwnerReferences: []metav1.OwnerReference{conflictingOwner},
			},
			Spec: demov1alpha1.DemoVirtualDiskSnapshotSpec{
				SourceRef: demov1alpha1.SnapshotSourceRef{
					APIVersion: demov1alpha1.SchemeGroupVersion.String(),
					Kind:       controllercommon.KindDemoVirtualDisk,
					Name:       "disk-owned",
				},
			},
		},
		&demov1alpha1.DemoVirtualMachineSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "ns1", UID: "snap-uid"},
			Spec: demov1alpha1.DemoVirtualMachineSnapshotSpec{
				SourceRef: demov1alpha1.SnapshotSourceRef{
					APIVersion: demov1alpha1.SchemeGroupVersion.String(),
					Kind:       controllercommon.KindDemoVirtualMachine,
					Name:       "vm-a",
				},
			},
		},
	)
	reconciler := &DemoVirtualMachineSnapshotReconciler{Client: cl}
	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "snap"}})
	if err == nil {
		t.Fatalf("expected conflicting child ownerRef to fail closed")
	}

	child := &demov1alpha1.DemoVirtualDiskSnapshot{}
	if getErr := cl.Get(context.Background(), client.ObjectKey{Namespace: "ns1", Name: childName}, child); getErr != nil {
		t.Fatalf("get child snapshot: %v", getErr)
	}
	assertDemoSnapshotOwnedBy(t, child, conflictingOwner.APIVersion, conflictingOwner.Kind, conflictingOwner.Name)
	vmSnap := getDemoVMSnapshot(t, cl)
	domainReady := meta.FindStatusCondition(vmSnap.Status.Conditions, snapshot.ConditionDomainReady)
	if domainReady == nil || domainReady.Status != metav1.ConditionFalse || domainReady.Reason != snapshot.ReasonCreateChildFailed {
		t.Fatalf("expected DomainReady=False CreateChildFailed, got %#v", domainReady)
	}
}

func TestEnsureDemoSnapshotOwnerRefPreservesUnrelatedRefs(t *testing.T) {
	unrelated := metav1.OwnerReference{APIVersion: "example.io/v1", Kind: "AuditAnchor", Name: "audit"}
	child := &demov1alpha1.DemoVirtualDiskSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "child",
			Namespace:       "ns1",
			OwnerReferences: []metav1.OwnerReference{unrelated},
		},
	}
	desired := demoSnapshotOwnerReference(demov1alpha1.SchemeGroupVersion.String(), controllercommon.KindDemoVirtualMachineSnapshot, "vm-snap", "vm-uid")

	if err := ensureDemoSnapshotOwnerRef(child, desired); err != nil {
		t.Fatalf("ensure demo snapshot ownerRef: %v", err)
	}
	assertDemoSnapshotOwnedBy(t, child, desired.APIVersion, desired.Kind, desired.Name)
	assertOwnerRefPresent(t, child.GetOwnerReferences(), unrelated.APIVersion, unrelated.Kind, unrelated.Name)
}

func TestEnsureDemoSnapshotOwnerRefIsIdempotent(t *testing.T) {
	desired := demoSnapshotOwnerReference(demov1alpha1.SchemeGroupVersion.String(), controllercommon.KindDemoVirtualMachineSnapshot, "vm-snap", "vm-uid")
	child := &demov1alpha1.DemoVirtualDiskSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "child",
			Namespace:       "ns1",
			OwnerReferences: []metav1.OwnerReference{desired},
		},
	}

	if err := ensureDemoSnapshotOwnerRef(child, desired); err != nil {
		t.Fatalf("ensure demo snapshot ownerRef: %v", err)
	}
	if len(child.OwnerReferences) != 1 || !demoSnapshotOwnerRefMatches(child.OwnerReferences[0], desired) {
		t.Fatalf("ownerRefs changed unexpectedly: %#v", child.OwnerReferences)
	}
}

func reconcileCommonSnapshotContentStatusForTest(t *testing.T, cl client.Client, contentName string) {
	t.Helper()
	content := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: contentName}, content); err != nil {
		t.Fatalf("get content %q: %v", contentName, err)
	}
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(content)
	if err != nil {
		t.Fatalf("convert content %q to unstructured: %v", contentName, err)
	}
	obj := &unstructured.Unstructured{Object: raw}
	obj.SetGroupVersionKind(unifiedbootstrap.CommonSnapshotContentGVK())
	reconciler := &snapshotcontent.SnapshotContentController{Client: cl, APIReader: cl}
	if _, err := reconciler.ReconcileCommonSnapshotContentStatus(context.Background(), obj); err != nil {
		t.Fatalf("reconcile common SnapshotContent status %q: %v", contentName, err)
	}
}

func newDemoSourceRefFakeClient(t *testing.T, initObjs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}
	if err := ssv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add state-snapshotter scheme: %v", err)
	}
	if err := demov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add demo scheme: %v", err)
	}
	if err := deckhousev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add deckhouse scheme: %v", err)
	}
	statusSubresources := []client.Object{
		&demov1alpha1.DemoVirtualDiskSnapshot{},
		&storagev1alpha1.SnapshotContent{},
		&demov1alpha1.DemoVirtualMachineSnapshot{},
		&storagev1alpha1.SnapshotContent{},
		&ssv1alpha1.ManifestCaptureRequest{},
		&ssv1alpha1.ManifestCheckpoint{},
	}
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(statusSubresources...).
		WithObjects(initObjs...).
		Build()
}

func getDemoDiskSnapshot(t *testing.T, cl client.Client) *demov1alpha1.DemoVirtualDiskSnapshot {
	t.Helper()
	snap := &demov1alpha1.DemoVirtualDiskSnapshot{}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "ns1", Name: "snap"}, snap); err != nil {
		t.Fatalf("get snapshot: %v", err)
	}
	return snap
}

func getDemoVMSnapshot(t *testing.T, cl client.Client) *demov1alpha1.DemoVirtualMachineSnapshot {
	t.Helper()
	snap := &demov1alpha1.DemoVirtualMachineSnapshot{}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "ns1", Name: "snap"}, snap); err != nil {
		t.Fatalf("get VM snapshot: %v", err)
	}
	return snap
}

func assertNoDemoMCRs(t *testing.T, cl client.Client) {
	t.Helper()
	mcrs := &ssv1alpha1.ManifestCaptureRequestList{}
	if err := cl.List(context.Background(), mcrs); err != nil {
		t.Fatalf("list MCRs: %v", err)
	}
	if len(mcrs.Items) != 0 {
		t.Fatalf("expected no MCRs, got %d", len(mcrs.Items))
	}
}

func assertNoDemoDiskContents(t *testing.T, cl client.Client) {
	t.Helper()
	contents := &storagev1alpha1.SnapshotContentList{}
	if err := cl.List(context.Background(), contents); err != nil {
		t.Fatalf("list disk contents: %v", err)
	}
	if len(contents.Items) != 0 {
		t.Fatalf("expected no disk contents, got %d", len(contents.Items))
	}
}

func assertNoDemoVMContents(t *testing.T, cl client.Client) {
	t.Helper()
	contents := &storagev1alpha1.SnapshotContentList{}
	if err := cl.List(context.Background(), contents); err != nil {
		t.Fatalf("list VM contents: %v", err)
	}
	if len(contents.Items) != 0 {
		t.Fatalf("expected no VM contents, got %d", len(contents.Items))
	}
}

func assertNoDemoDiskSnapshots(t *testing.T, cl client.Client) {
	t.Helper()
	assertDemoDiskSnapshotsCount(t, cl, 0)
}

func assertDemoDiskSnapshotsCount(t *testing.T, cl client.Client, want int) {
	t.Helper()
	snaps := &demov1alpha1.DemoVirtualDiskSnapshotList{}
	if err := cl.List(context.Background(), snaps); err != nil {
		t.Fatalf("list disk snapshots: %v", err)
	}
	if len(snaps.Items) != want {
		t.Fatalf("expected %d disk snapshots, got %d", want, len(snaps.Items))
	}
}

func assertDemoSnapshotOwnedBy(t *testing.T, obj client.Object, apiVersion, kind, name string) {
	t.Helper()
	for _, ref := range obj.GetOwnerReferences() {
		if ref.APIVersion == apiVersion && ref.Kind == kind && ref.Name == name {
			return
		}
	}
	t.Fatalf("expected %s/%s to be owned by %s %s/%s, got %#v", obj.GetNamespace(), obj.GetName(), apiVersion, kind, name, obj.GetOwnerReferences())
}

func assertDemoSnapshotNotOwnedBy(t *testing.T, obj client.Object, apiVersion, kind, name string) {
	t.Helper()
	for _, ref := range obj.GetOwnerReferences() {
		if ref.APIVersion == apiVersion && ref.Kind == kind && ref.Name == name {
			t.Fatalf("expected %s/%s not to be owned by %s %s/%s, got %#v", obj.GetNamespace(), obj.GetName(), apiVersion, kind, name, obj.GetOwnerReferences())
		}
	}
}

func assertOwnerRefPresent(t *testing.T, refs []metav1.OwnerReference, apiVersion, kind, name string) {
	t.Helper()
	for _, ref := range refs {
		if ref.APIVersion == apiVersion && ref.Kind == kind && ref.Name == name {
			return
		}
	}
	t.Fatalf("expected ownerRef %s/%s/%s in %#v", apiVersion, kind, name, refs)
}

func assertDemoOwnerRef(t *testing.T, refs []metav1.OwnerReference, apiVersion, kind, name string, controller bool) {
	t.Helper()
	for _, ref := range refs {
		if ref.APIVersion != apiVersion || ref.Kind != kind || ref.Name != name {
			continue
		}
		gotController := ref.Controller != nil && *ref.Controller
		if gotController != controller {
			t.Fatalf("ownerRef %s/%s/%s controller=%v, want %v", apiVersion, kind, name, gotController, controller)
		}
		return
	}
	t.Fatalf("expected ownerRef %s/%s/%s in %#v", apiVersion, kind, name, refs)
}

func assertNoSnapshotContentOwnerRefToSnapshot(t *testing.T, content *storagev1alpha1.SnapshotContent) {
	t.Helper()
	for _, ref := range content.OwnerReferences {
		if ref.Kind == controllercommon.KindSnapshot || ref.Kind == controllercommon.KindDemoVirtualDiskSnapshot || ref.Kind == controllercommon.KindDemoVirtualMachineSnapshot {
			t.Fatalf("SnapshotContent %s must not be owned by Snapshot %s/%s", content.Name, ref.Kind, ref.Name)
		}
	}
}
