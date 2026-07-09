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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	deckhousev1alpha1 "github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1"
	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/domain-controller/internal/controllers/snaphelpers"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk"
)

// Demo reconcilers are content-free (commit 2 content-ownership, D1/D3): they validate the source, plan
// the manifest-capture request (MCR), the data-leg volume-capture request (VCR) and the owned-disk child
// graph, and publish results into demo.status.captureState.domainSpecificController
// (manifestCaptureRequestName, volumeCaptureRequestName, phase) plus status.childrenSnapshotRefs and the
// top-level status.snapshotSource. They never create/own/bind/mirror SnapshotContent;
// GenericSnapshotBinderController owns all SnapshotContent work for demo kinds. These unit tests therefore
// assert only the domain planning side; content creation/projection/Ready mirror is covered by the binder.

func TestDemoVirtualDiskSnapshot_InvalidSourceRefDoesNotCreateMCR(t *testing.T) {
	tests := []struct {
		name      string
		sourceRef *demov1alpha1.SnapshotSourceRef
	}{
		{
			name: "missing sourceRef",
		},
		{
			name: "wrong kind",
			sourceRef: &demov1alpha1.SnapshotSourceRef{
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
			reconciler := &DemoVirtualDiskSnapshotReconciler{Client: cl, APIReader: cl}
			if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "snap"}}); err != nil {
				t.Fatalf("reconcile failed: %v", err)
			}

			snap := getDemoDiskSnapshot(t, cl)
			// New model: the domain publishes phase=Failed+reason (Reject); the core mirrors it into Ready.
			// This isolated reconciler test runs no core, so assert the domain-owned phase/reason directly.
			if phase := domainPhase(snap.Status.CaptureState); phase != storagev1alpha1.SnapshotCapturePhaseFailed {
				t.Fatalf("expected domainSpecificController.phase=Failed, got %q", phase)
			}
			if reason := domainReason(snap.Status.CaptureState); reason != demoReasonInvalidSourceRef {
				t.Fatalf("expected domain reason %q, got %q", demoReasonInvalidSourceRef, reason)
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
			SourceRef: &demov1alpha1.SnapshotSourceRef{
				APIVersion: demov1alpha1.SchemeGroupVersion.String(),
				Kind:       controllercommon.KindDemoVirtualDisk,
				Name:       "missing-disk",
			},
		},
	})
	reconciler := &DemoVirtualDiskSnapshotReconciler{Client: cl, APIReader: cl}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "snap"}}); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	snap := getDemoDiskSnapshot(t, cl)
	if phase := domainPhase(snap.Status.CaptureState); phase != storagev1alpha1.SnapshotCapturePhaseFailed {
		t.Fatalf("expected domainSpecificController.phase=Failed, got %q", phase)
	}
	if reason := domainReason(snap.Status.CaptureState); reason != demoReasonSourceNotFound {
		t.Fatalf("expected domain reason %q, got %q", demoReasonSourceNotFound, reason)
	}
	assertNoDemoDiskContents(t, cl)
	assertNoDemoMCRs(t, cl)
}

// A manifest-only disk snapshot plans its MCR (manifest target = the source disk), publishes the MCR name,
// reaches phase=Planned (leaf planning barrier), and never creates SnapshotContent.
func TestDemoVirtualDiskSnapshot_PlansMCRAndChildrenReady(t *testing.T) {
	cl := newDemoSourceRefFakeClient(t,
		&demov1alpha1.DemoVirtualDisk{
			ObjectMeta: metav1.ObjectMeta{Name: "disk-a", Namespace: "ns1", UID: "disk-a-uid"},
		},
		&demov1alpha1.DemoVirtualDiskSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "ns1", UID: "snap-uid"},
			Spec: demov1alpha1.DemoVirtualDiskSnapshotSpec{
				SourceRef: &demov1alpha1.SnapshotSourceRef{
					APIVersion: demov1alpha1.SchemeGroupVersion.String(),
					Kind:       controllercommon.KindDemoVirtualDisk,
					Name:       "disk-a",
				},
			},
		},
	)
	reconciler := &DemoVirtualDiskSnapshotReconciler{Client: cl, APIReader: cl}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "snap"}}

	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	snap := getDemoDiskSnapshot(t, cl)
	// The captured live source uid is published (informational) for import-mode recreation.
	if snap.Status.SnapshotSource == nil || snap.Status.SnapshotSource.UID != "disk-a-uid" {
		t.Fatalf("expected status.snapshotSource.uid from the live DemoVirtualDisk uid, got %#v", snap.Status.SnapshotSource)
	}
	mcrName := domainMCRName(snap.Status.CaptureState)
	if mcrName == "" {
		t.Fatalf("expected published manifestCaptureRequestName, got empty")
	}
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
	if phase := domainPhase(snap.Status.CaptureState); phase != storagev1alpha1.SnapshotCapturePhasePlanned {
		t.Fatalf("expected domainSpecificController.phase=Planned for leaf disk snapshot, got %q", phase)
	}
	// Content ownership is the binder's job; the demo reconciler never creates SnapshotContent.
	assertNoDemoDiskContents(t, cl)
	if snap.Status.BoundSnapshotContentName != "" {
		t.Fatalf("demo reconciler must not bind content, got %q", snap.Status.BoundSnapshotContentName)
	}
}

// Once the common controller stamps status.manifestCaptured (durable handoff) and deletes the MCR, the
// demo reconciler must NOT re-create it. This is the domain-only suppression contract (no SnapshotContent
// read on the demo side).
func TestDemoVirtualDiskSnapshot_ManifestCapturedSuppressesMCRRecreation(t *testing.T) {
	cl := newDemoSourceRefFakeClient(t,
		&demov1alpha1.DemoVirtualDisk{
			ObjectMeta: metav1.ObjectMeta{Name: "disk-a", Namespace: "ns1"},
		},
		&demov1alpha1.DemoVirtualDiskSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "ns1", UID: "snap-uid"},
			Spec: demov1alpha1.DemoVirtualDiskSnapshotSpec{
				SourceRef: &demov1alpha1.SnapshotSourceRef{
					APIVersion: demov1alpha1.SchemeGroupVersion.String(),
					Kind:       controllercommon.KindDemoVirtualDisk,
					Name:       "disk-a",
				},
			},
		},
	)
	reconciler := &DemoVirtualDiskSnapshotReconciler{Client: cl, APIReader: cl}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "snap"}}

	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("first reconcile failed: %v", err)
	}
	snap := getDemoDiskSnapshot(t, cl)
	mcrName := domainMCRName(snap.Status.CaptureState)
	if mcrName == "" {
		t.Fatalf("expected published manifestCaptureRequestName after first reconcile")
	}
	mcr := &ssv1alpha1.ManifestCaptureRequest{}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "ns1", Name: mcrName}, mcr); err != nil {
		t.Fatalf("expected MCR %q after first reconcile: %v", mcrName, err)
	}

	// Common controller marks the leg captured (captureState.commonController.manifestCaptured) and
	// deletes the request.
	base := snap.DeepCopy()
	setCommonManifestCaptured(&snap.Status.CaptureState, true)
	if err := cl.Status().Patch(context.Background(), snap, client.MergeFrom(base)); err != nil {
		t.Fatalf("patch manifestCaptured: %v", err)
	}
	if err := cl.Delete(context.Background(), mcr); err != nil {
		t.Fatalf("delete MCR: %v", err)
	}

	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("second reconcile failed: %v", err)
	}
	assertNoDemoMCRs(t, cl)
}

func TestDemoVirtualMachineSnapshot_InvalidSourceRefDoesNotCreateContentMCROrChildren(t *testing.T) {
	tests := []struct {
		name      string
		sourceRef *demov1alpha1.SnapshotSourceRef
	}{
		{
			name: "missing sourceRef",
		},
		{
			name: "wrong kind",
			sourceRef: &demov1alpha1.SnapshotSourceRef{
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
			reconciler := &DemoVirtualMachineSnapshotReconciler{Client: cl, APIReader: cl}
			if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "snap"}}); err != nil {
				t.Fatalf("reconcile failed: %v", err)
			}

			snap := getDemoVMSnapshot(t, cl)
			if phase := domainPhase(snap.Status.CaptureState); phase != storagev1alpha1.SnapshotCapturePhaseFailed {
				t.Fatalf("expected domainSpecificController.phase=Failed, got %q", phase)
			}
			if reason := domainReason(snap.Status.CaptureState); reason != demoReasonInvalidSourceRef {
				t.Fatalf("expected domain reason %q, got %q", demoReasonInvalidSourceRef, reason)
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
			SourceRef: &demov1alpha1.SnapshotSourceRef{
				APIVersion: demov1alpha1.SchemeGroupVersion.String(),
				Kind:       controllercommon.KindDemoVirtualMachine,
				Name:       "missing-vm",
			},
		},
	})
	reconciler := &DemoVirtualMachineSnapshotReconciler{Client: cl, APIReader: cl}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "snap"}}); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	snap := getDemoVMSnapshot(t, cl)
	if phase := domainPhase(snap.Status.CaptureState); phase != storagev1alpha1.SnapshotCapturePhaseFailed {
		t.Fatalf("expected domainSpecificController.phase=Failed, got %q", phase)
	}
	if reason := domainReason(snap.Status.CaptureState); reason != demoReasonSourceNotFound {
		t.Fatalf("expected domain reason %q, got %q", demoReasonSourceNotFound, reason)
	}
	assertNoDemoVMContents(t, cl)
	assertNoDemoMCRs(t, cl)
	assertNoDemoDiskSnapshots(t, cl)
}

// A VM snapshot plans its owned-disk child graph (only disks owned by the VM), publishes
// childrenSnapshotRefs, plans the VM MCR, reaches phase=Planned, and never creates content.
func TestDemoVirtualMachineSnapshot_PlansOwnedDiskChildrenAndMCR(t *testing.T) {
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
				SourceRef: &demov1alpha1.SnapshotSourceRef{
					APIVersion: demov1alpha1.SchemeGroupVersion.String(),
					Kind:       controllercommon.KindDemoVirtualMachine,
					Name:       "vm-a",
				},
			},
		},
	)
	reconciler := &DemoVirtualMachineSnapshotReconciler{Client: cl, APIReader: cl}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "snap"}}

	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	vmSnap := getDemoVMSnapshot(t, cl)
	// The captured live source uid is published (informational) for import-mode recreation.
	if vmSnap.Status.SnapshotSource == nil || vmSnap.Status.SnapshotSource.UID != vmUID {
		t.Fatalf("expected status.snapshotSource.uid from the live DemoVirtualMachine uid %q, got %#v", vmUID, vmSnap.Status.SnapshotSource)
	}
	mcrName := domainMCRName(vmSnap.Status.CaptureState)
	if mcrName == "" {
		t.Fatalf("expected published VM manifestCaptureRequestName, got empty")
	}
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

	childName := snapshotsdk.ChildSnapshotName(types.UID("snap-uid"), types.UID("disk-owned-uid"))
	child := &demov1alpha1.DemoVirtualDiskSnapshot{}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "ns1", Name: childName}, child); err != nil {
		t.Fatalf("expected owned disk child snapshot %q: %v", childName, err)
	}
	if child.Spec.SourceRef == nil || *child.Spec.SourceRef != (demov1alpha1.SnapshotSourceRef{
		APIVersion: demov1alpha1.SchemeGroupVersion.String(),
		Kind:       controllercommon.KindDemoVirtualDisk,
		Name:       "disk-owned",
	}) {
		t.Fatalf("unexpected child sourceRef: %#v", child.Spec.SourceRef)
	}
	assertDemoSnapshotOwnedBy(t, child, demov1alpha1.SchemeGroupVersion.String(), controllercommon.KindDemoVirtualMachineSnapshot, "snap")
	assertDemoDiskSnapshotsCount(t, cl, 1)

	vmSnap = getDemoVMSnapshot(t, cl)
	if !controllercommon.SnapshotChildRefsEqualIgnoreOrder(vmSnap.Status.ChildrenSnapshotRefs, []storagev1alpha1.SnapshotChildRef{{
		APIVersion: demov1alpha1.SchemeGroupVersion.String(),
		Kind:       controllercommon.KindDemoVirtualDiskSnapshot,
		Name:       childName,
	}}) {
		t.Fatalf("unexpected VM child refs: %#v", vmSnap.Status.ChildrenSnapshotRefs)
	}
	if phase := domainPhase(vmSnap.Status.CaptureState); phase != storagev1alpha1.SnapshotCapturePhasePlanned {
		t.Fatalf("expected VM domainSpecificController.phase=Planned after writing child refs, got %q", phase)
	}
	assertNoDemoVMContents(t, cl)
	if vmSnap.Status.BoundSnapshotContentName != "" {
		t.Fatalf("demo reconciler must not bind content, got %q", vmSnap.Status.BoundSnapshotContentName)
	}
}

func TestDemoVirtualMachineSnapshot_DoesNotStealConflictingDiskChildOwner(t *testing.T) {
	vmUID := types.UID("vm-uid")
	childName := snapshotsdk.ChildSnapshotName(types.UID("snap-uid"), types.UID("disk-owned-uid"))
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
				SourceRef: &demov1alpha1.SnapshotSourceRef{
					APIVersion: demov1alpha1.SchemeGroupVersion.String(),
					Kind:       controllercommon.KindDemoVirtualDisk,
					Name:       "disk-owned",
				},
			},
		},
		&demov1alpha1.DemoVirtualMachineSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "ns1", UID: "snap-uid"},
			Spec: demov1alpha1.DemoVirtualMachineSnapshotSpec{
				SourceRef: &demov1alpha1.SnapshotSourceRef{
					APIVersion: demov1alpha1.SchemeGroupVersion.String(),
					Kind:       controllercommon.KindDemoVirtualMachine,
					Name:       "vm-a",
				},
			},
		},
	)
	reconciler := &DemoVirtualMachineSnapshotReconciler{Client: cl, APIReader: cl}
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
	if phase := domainPhase(vmSnap.Status.CaptureState); phase != storagev1alpha1.SnapshotCapturePhaseFailed {
		t.Fatalf("expected domainSpecificController.phase=Failed on child-create failure, got %q", phase)
	}
	if reason := domainReason(vmSnap.Status.CaptureState); reason != storagev1alpha1.ReasonCreateChildFailed {
		t.Fatalf("expected domainSpecificController.reason=CreateChildFailed, got %q", reason)
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

// domainMCRName reads the domain-owned MCR name from captureState.domainSpecificController (empty if the
// domain half is absent).
func domainMCRName(cs *storagev1alpha1.CaptureStateStatus) string {
	if cs == nil || cs.DomainSpecificController == nil {
		return ""
	}
	return cs.DomainSpecificController.ManifestCaptureRequestName
}

// domainPhase reads the domain lifecycle phase from captureState.domainSpecificController (empty if absent).
func domainPhase(cs *storagev1alpha1.CaptureStateStatus) storagev1alpha1.SnapshotCapturePhase {
	if cs == nil || cs.DomainSpecificController == nil {
		return ""
	}
	return cs.DomainSpecificController.Phase
}

// domainReason reads the domain phase=Failed reason from captureState.domainSpecificController.
func domainReason(cs *storagev1alpha1.CaptureStateStatus) string {
	if cs == nil || cs.DomainSpecificController == nil {
		return ""
	}
	return cs.DomainSpecificController.Reason
}

// setCommonManifestCaptured simulates the common controller stamping the manifest-leg latch on
// captureState.commonController (the core-written half the demo reconciler only reads).
func setCommonManifestCaptured(cs **storagev1alpha1.CaptureStateStatus, v bool) {
	if *cs == nil {
		*cs = &storagev1alpha1.CaptureStateStatus{}
	}
	if (*cs).CommonController == nil {
		(*cs).CommonController = &storagev1alpha1.CommonControllerCaptureState{}
	}
	captured := v
	(*cs).CommonController.ManifestCaptured = &captured
}
