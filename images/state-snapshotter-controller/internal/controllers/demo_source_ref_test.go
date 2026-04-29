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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
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
				Kind:       "DemoVirtualMachine",
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
						Kind:       "NamespaceSnapshot",
						Name:       "root",
					},
					SourceRef: tt.sourceRef,
				},
			})
			reconciler := &DemoVirtualDiskSnapshotReconciler{Client: cl}
			if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "snap"}}); err != nil {
				t.Fatalf("reconcile failed: %v", err)
			}

			snap := &demov1alpha1.DemoVirtualDiskSnapshot{}
			if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "ns1", Name: "snap"}, snap); err != nil {
				t.Fatalf("get snapshot: %v", err)
			}
			ready := meta.FindStatusCondition(snap.Status.Conditions, snapshot.ConditionReady)
			if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "InvalidSourceRef" {
				t.Fatalf("expected Ready=False InvalidSourceRef, got %#v", ready)
			}
			assertNoDemoMCRs(t, cl)
		})
	}
}

func TestDemoVirtualMachineSnapshot_SourceNotFoundDoesNotCreateMCR(t *testing.T) {
	cl := newDemoSourceRefFakeClient(t, &demov1alpha1.DemoVirtualMachineSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "ns1"},
		Spec: demov1alpha1.DemoVirtualMachineSnapshotSpec{
			ParentSnapshotRef: demov1alpha1.SnapshotParentRef{
				APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
				Kind:       "NamespaceSnapshot",
				Name:       "root",
			},
			SourceRef: demov1alpha1.SnapshotSourceRef{
				APIVersion: demov1alpha1.SchemeGroupVersion.String(),
				Kind:       "DemoVirtualMachine",
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
	assertNoDemoMCRs(t, cl)
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
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(initObjs...).
		WithObjects(initObjs...).
		Build()
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
