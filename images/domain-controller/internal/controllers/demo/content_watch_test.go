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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

func demoWatchScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := demov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("demo AddToScheme: %v", err)
	}
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("storage AddToScheme: %v", err)
	}
	return scheme
}

func TestMapContentToBoundDemoDiskSnapshots(t *testing.T) {
	ctx := context.Background()
	scheme := demoWatchScheme(t)

	bound := &demov1alpha1.DemoVirtualDiskSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "disk-snap", Namespace: "ns1"},
		Status:     demov1alpha1.DemoVirtualDiskSnapshotStatus{BoundSnapshotContentName: "demodiskc-abc"},
	}
	// Sibling bound to a different content must NOT be enqueued (sibling isolation).
	sibling := &demov1alpha1.DemoVirtualDiskSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "disk-snap-2", Namespace: "ns1"},
		Status:     demov1alpha1.DemoVirtualDiskSnapshotStatus{BoundSnapshotContentName: "demodiskc-other"},
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(bound, sibling).
		WithIndex(&demov1alpha1.DemoVirtualDiskSnapshot{}, DemoSnapshotBoundContentFieldIndex, func(rawObj client.Object) []string {
			s, ok := rawObj.(*demov1alpha1.DemoVirtualDiskSnapshot)
			if !ok || s.Status.BoundSnapshotContentName == "" {
				return nil
			}
			return []string{s.Status.BoundSnapshotContentName}
		}).
		Build()

	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "demodiskc-abc"}}
	reqs := mapContentToBoundDemoDiskSnapshots(cl)(ctx, content)
	if len(reqs) != 1 {
		t.Fatalf("expected 1 enqueue request, got %d (%#v)", len(reqs), reqs)
	}
	want := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "disk-snap"}}
	if reqs[0] != want {
		t.Fatalf("unexpected request: got %#v want %#v", reqs[0], want)
	}

	// Unrelated content enqueues nothing.
	if got := mapContentToBoundDemoDiskSnapshots(cl)(ctx, &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "orphan"}}); len(got) != 0 {
		t.Fatalf("expected no requests for unrelated content, got %d", len(got))
	}
}

func TestMapContentToBoundDemoVMSnapshots(t *testing.T) {
	ctx := context.Background()
	scheme := demoWatchScheme(t)

	bound := &demov1alpha1.DemoVirtualMachineSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "vm-snap", Namespace: "ns1"},
		Status:     demov1alpha1.DemoVirtualMachineSnapshotStatus{BoundSnapshotContentName: "demovmc-abc"},
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(bound).
		WithIndex(&demov1alpha1.DemoVirtualMachineSnapshot{}, DemoSnapshotBoundContentFieldIndex, func(rawObj client.Object) []string {
			s, ok := rawObj.(*demov1alpha1.DemoVirtualMachineSnapshot)
			if !ok || s.Status.BoundSnapshotContentName == "" {
				return nil
			}
			return []string{s.Status.BoundSnapshotContentName}
		}).
		Build()

	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "demovmc-abc"}}
	reqs := mapContentToBoundDemoVMSnapshots(cl)(ctx, content)
	if len(reqs) != 1 {
		t.Fatalf("expected 1 enqueue request, got %d (%#v)", len(reqs), reqs)
	}
	want := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "vm-snap"}}
	if reqs[0] != want {
		t.Fatalf("unexpected request: got %#v want %#v", reqs[0], want)
	}
}
