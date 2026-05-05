/*
Copyright 2025 Flant JSC

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

package controllers

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
)

func TestDemoVirtualDiskSnapshot_InvalidParentRefDoesNotCreateContentOrMCR(t *testing.T) {
	tests := []struct {
		name      string
		parentRef demov1alpha1.SnapshotParentRef
	}{
		{
			name: "missing parentRef",
		},
		{
			name: "wrong kind",
			parentRef: demov1alpha1.SnapshotParentRef{
				APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
				Kind:       "UnsupportedSnapshot",
				Name:       "root",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cl := newDemoSourceRefFakeClient(t, &demov1alpha1.DemoVirtualDiskSnapshot{
				ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "ns1"},
				Spec: demov1alpha1.DemoVirtualDiskSnapshotSpec{
					ParentSnapshotRef: tt.parentRef,
					SourceRef: demov1alpha1.SnapshotSourceRef{
						APIVersion: demov1alpha1.SchemeGroupVersion.String(),
						Kind:       KindDemoVirtualDisk,
						Name:       "disk-a",
					},
				},
			})
			reconciler := &DemoVirtualDiskSnapshotReconciler{Client: cl}
			if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "snap"}}); err != nil {
				t.Fatalf("reconcile failed: %v", err)
			}

			snap := getDemoDiskSnapshot(t, cl)
			ready := meta.FindStatusCondition(snap.Status.Conditions, snapshot.ConditionReady)
			if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "InvalidParentRef" {
				t.Fatalf("expected Ready=False InvalidParentRef, got %#v", ready)
			}
			assertNoDemoDiskContents(t, cl)
			assertNoDemoMCRs(t, cl)
		})
	}
}

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
				Kind:       KindDemoVirtualMachine,
				Name:       "disk-a",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cl := newDemoSourceRefFakeClient(t, &demov1alpha1.DemoVirtualDiskSnapshot{
				ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "ns1"},
				Spec: demov1alpha1.DemoVirtualDiskSnapshotSpec{
					ParentSnapshotRef: demov1alpha1.SnapshotParentRef{
						APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
						Kind:       KindNamespaceSnapshot,
						Name:       "root",
					},
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
			ParentSnapshotRef: demov1alpha1.SnapshotParentRef{
				APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
				Kind:       KindNamespaceSnapshot,
				Name:       "root",
			},
			SourceRef: demov1alpha1.SnapshotSourceRef{
				APIVersion: demov1alpha1.SchemeGroupVersion.String(),
				Kind:       KindDemoVirtualDisk,
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
				ParentSnapshotRef: demov1alpha1.SnapshotParentRef{
					APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
					Kind:       KindNamespaceSnapshot,
					Name:       "root",
				},
				SourceRef: demov1alpha1.SnapshotSourceRef{
					APIVersion: demov1alpha1.SchemeGroupVersion.String(),
					Kind:       KindDemoVirtualDisk,
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
	mcrName := demoSnapshotManifestCaptureRequestName(KindDemoVirtualDiskSnapshot, "ns1", "snap")
	mcr := &ssv1alpha1.ManifestCaptureRequest{}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "ns1", Name: mcrName}, mcr); err != nil {
		t.Fatalf("expected MCR %q: %v", mcrName, err)
	}
	expectedTargets := []ssv1alpha1.ManifestTarget{{
		APIVersion: demov1alpha1.SchemeGroupVersion.String(),
		Kind:       KindDemoVirtualDisk,
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
	ready := meta.FindStatusCondition(snap.Status.Conditions, snapshot.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionTrue || ready.Reason != snapshot.ReasonCompleted {
		t.Fatalf("expected Ready=True Completed, got %#v", ready)
	}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: contentName}, content); err != nil {
		t.Fatalf("get content after ready: %v", err)
	}
	if content.Status.ManifestCheckpointName != "mcp-disk" {
		t.Fatalf("expected content MCP link %q, got %q", "mcp-disk", content.Status.ManifestCheckpointName)
	}
	contentReady := meta.FindStatusCondition(content.Status.Conditions, snapshot.ConditionReady)
	if contentReady == nil || contentReady.Status != metav1.ConditionTrue || contentReady.Reason != snapshot.ReasonCompleted {
		t.Fatalf("expected content Ready=True Completed, got %#v", contentReady)
	}
}

func TestDemoVirtualMachineSnapshot_InvalidParentRefDoesNotCreateContentMCROrChildren(t *testing.T) {
	tests := []struct {
		name      string
		parentRef demov1alpha1.SnapshotParentRef
	}{
		{
			name: "missing parentRef",
		},
		{
			name: "wrong kind",
			parentRef: demov1alpha1.SnapshotParentRef{
				APIVersion: demov1alpha1.SchemeGroupVersion.String(),
				Kind:       KindDemoVirtualMachineSnapshot,
				Name:       "parent-vm",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cl := newDemoSourceRefFakeClient(t, &demov1alpha1.DemoVirtualMachineSnapshot{
				ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "ns1"},
				Spec: demov1alpha1.DemoVirtualMachineSnapshotSpec{
					ParentSnapshotRef: tt.parentRef,
					SourceRef: demov1alpha1.SnapshotSourceRef{
						APIVersion: demov1alpha1.SchemeGroupVersion.String(),
						Kind:       KindDemoVirtualMachine,
						Name:       "vm-a",
					},
				},
			})
			reconciler := &DemoVirtualMachineSnapshotReconciler{Client: cl}
			if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "snap"}}); err != nil {
				t.Fatalf("reconcile failed: %v", err)
			}

			snap := getDemoVMSnapshot(t, cl)
			ready := meta.FindStatusCondition(snap.Status.Conditions, snapshot.ConditionReady)
			if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "InvalidParentRef" {
				t.Fatalf("expected Ready=False InvalidParentRef, got %#v", ready)
			}
			assertNoDemoVMContents(t, cl)
			assertNoDemoMCRs(t, cl)
			assertNoDemoDiskSnapshots(t, cl)
		})
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
				Kind:       KindDemoVirtualDisk,
				Name:       "vm-a",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cl := newDemoSourceRefFakeClient(t, &demov1alpha1.DemoVirtualMachineSnapshot{
				ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "ns1"},
				Spec: demov1alpha1.DemoVirtualMachineSnapshotSpec{
					ParentSnapshotRef: demov1alpha1.SnapshotParentRef{
						APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
						Kind:       KindNamespaceSnapshot,
						Name:       "root",
					},
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
			ParentSnapshotRef: demov1alpha1.SnapshotParentRef{
				APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
				Kind:       KindNamespaceSnapshot,
				Name:       "root",
			},
			SourceRef: demov1alpha1.SnapshotSourceRef{
				APIVersion: demov1alpha1.SchemeGroupVersion.String(),
				Kind:       KindDemoVirtualMachine,
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
		},
		&demov1alpha1.DemoVirtualDisk{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "disk-owned",
				Namespace: "ns1",
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: demov1alpha1.SchemeGroupVersion.String(),
					Kind:       KindDemoVirtualMachine,
					Name:       "vm-a",
					UID:        vmUID,
				}},
			},
		},
		&demov1alpha1.DemoVirtualDisk{
			ObjectMeta: metav1.ObjectMeta{Name: "disk-unowned", Namespace: "ns1"},
		},
		&demov1alpha1.DemoVirtualMachineSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "ns1", UID: "snap-uid"},
			Spec: demov1alpha1.DemoVirtualMachineSnapshotSpec{
				ParentSnapshotRef: demov1alpha1.SnapshotParentRef{
					APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
					Kind:       KindNamespaceSnapshot,
					Name:       "root",
				},
				SourceRef: demov1alpha1.SnapshotSourceRef{
					APIVersion: demov1alpha1.SchemeGroupVersion.String(),
					Kind:       KindDemoVirtualMachine,
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
	mcrName := demoSnapshotManifestCaptureRequestName(KindDemoVirtualMachineSnapshot, "ns1", "snap")
	mcr := &ssv1alpha1.ManifestCaptureRequest{}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "ns1", Name: mcrName}, mcr); err != nil {
		t.Fatalf("expected VM MCR %q: %v", mcrName, err)
	}
	expectedTargets := []ssv1alpha1.ManifestTarget{{
		APIVersion: demov1alpha1.SchemeGroupVersion.String(),
		Kind:       KindDemoVirtualMachine,
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
		Kind:       KindDemoVirtualDisk,
		Name:       "disk-owned",
	}) {
		t.Fatalf("unexpected child sourceRef: %#v", child.Spec.SourceRef)
	}
	diskSnapshots := &demov1alpha1.DemoVirtualDiskSnapshotList{}
	if err := cl.List(context.Background(), diskSnapshots, client.InNamespace("ns1")); err != nil {
		t.Fatalf("list disk snapshots: %v", err)
	}
	if len(diskSnapshots.Items) != 1 {
		t.Fatalf("expected only owned disk child snapshot, got %d", len(diskSnapshots.Items))
	}
	vmSnap := getDemoVMSnapshot(t, cl)
	if !namespaceSnapshotChildRefsEqualIgnoreOrder(vmSnap.Status.ChildrenSnapshotRefs, []storagev1alpha1.NamespaceSnapshotChildRef{{
		APIVersion: demov1alpha1.SchemeGroupVersion.String(),
		Kind:       KindDemoVirtualDiskSnapshot,
		Name:       childName,
	}}) {
		t.Fatalf("unexpected VM child refs: %#v", vmSnap.Status.ChildrenSnapshotRefs)
	}
	ready := meta.FindStatusCondition(vmSnap.Status.Conditions, snapshot.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != snapshot.ReasonManifestCapturePending {
		t.Fatalf("expected Ready=False mirrored content pending before child content ready, got %#v", ready)
	}

	diskContentName := "disk-content"
	if err := cl.Create(context.Background(), &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: diskContentName},
		Spec: storagev1alpha1.SnapshotContentSpec{
			SnapshotRef: storagev1alpha1.SnapshotSubjectRef{
				APIVersion: demov1alpha1.SchemeGroupVersion.String(),
				Kind:       KindDemoVirtualDiskSnapshot,
				Name:       childName,
				Namespace:  "ns1",
				UID:        child.UID,
			},
		},
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
	if !snapshotContentChildRefsEqualIgnoreOrder(vmContent.Status.ChildrenSnapshotContentRefs, []storagev1alpha1.SnapshotContentChildRef{{Name: diskContentName}}) {
		t.Fatalf("unexpected VM content child refs: %#v", vmContent.Status.ChildrenSnapshotContentRefs)
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
	reconciler := &SnapshotContentController{Client: cl, APIReader: cl}
	if _, err := reconciler.reconcileCommonSnapshotContentStatus(context.Background(), obj); err != nil {
		t.Fatalf("reconcile common SnapshotContent status %q: %v", contentName, err)
	}
}

func newDemoSourceRefFakeClient(t *testing.T, initObjs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}
	if err := ssv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add state-snapshotter scheme: %v", err)
	}
	if err := demov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add demo scheme: %v", err)
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
