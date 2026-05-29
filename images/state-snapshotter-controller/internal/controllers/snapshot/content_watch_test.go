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

package snapshot

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/common"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

func TestMapSnapshotContentToBoundSnapshots(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	bound := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: "ns1"},
		Status: storagev1alpha1.SnapshotStatus{
			BoundSnapshotContentName: "ns-abc123",
		},
	}
	other := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "ns2"},
		Status: storagev1alpha1.SnapshotStatus{
			BoundSnapshotContentName: "ns-other",
		},
	}
	content := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "ns-abc123"},
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(bound, other).
		WithIndex(&storagev1alpha1.Snapshot{}, SnapshotBoundContentFieldIndex, func(rawObj client.Object) []string {
			snap, ok := rawObj.(*storagev1alpha1.Snapshot)
			if !ok || snap.Status.BoundSnapshotContentName == "" {
				return nil
			}
			return []string{snap.Status.BoundSnapshotContentName}
		}).
		Build()

	reqs := MapSnapshotContentToBoundSnapshots(ctx, cl, content)
	if len(reqs) != 1 {
		t.Fatalf("expected 1 enqueue request, got %d", len(reqs))
	}
	want := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "root"}}
	if reqs[0] != want {
		t.Fatalf("unexpected request: got %#v want %#v", reqs[0], want)
	}
}

func TestMapSnapshotContentToBoundSnapshots_noMatch(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithIndex(&storagev1alpha1.Snapshot{}, SnapshotBoundContentFieldIndex, func(rawObj client.Object) []string {
			snap, ok := rawObj.(*storagev1alpha1.Snapshot)
			if !ok || snap.Status.BoundSnapshotContentName == "" {
				return nil
			}
			return []string{snap.Status.BoundSnapshotContentName}
		}).
		Build()

	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "orphan-content"}}
	if reqs := MapSnapshotContentToBoundSnapshots(ctx, cl, content); len(reqs) != 0 {
		t.Fatalf("expected no requests, got %d", len(reqs))
	}
}

func TestMapSnapshotContentChildSubtreeUpdateEnqueuesRootSnapshot(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	controller := true
	rootContent := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "ns-root"},
	}
	childContent := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ns-child-disk",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
					Kind:       controllercommon.KindSnapshotContent,
					Name:       "ns-root",
					Controller: &controller,
				},
			},
		},
	}
	rootSnap := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-full", Namespace: "snapshot-demo"},
		Status: storagev1alpha1.SnapshotStatus{
			BoundSnapshotContentName: "ns-root",
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(rootSnap, rootContent, childContent).
		WithIndex(&storagev1alpha1.Snapshot{}, SnapshotBoundContentFieldIndex, func(rawObj client.Object) []string {
			snap, ok := rawObj.(*storagev1alpha1.Snapshot)
			if !ok || snap.Status.BoundSnapshotContentName == "" {
				return nil
			}
			return []string{snap.Status.BoundSnapshotContentName}
		}).
		Build()

	reqs := MapSnapshotContentUpdateToSnapshots(ctx, cl, childContent)
	if len(reqs) != 1 {
		t.Fatalf("expected 1 enqueue request for root snapshot, got %d: %#v", len(reqs), reqs)
	}
	want := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "snapshot-demo", Name: "demo-full"}}
	if reqs[0] != want {
		t.Fatalf("unexpected request: got %#v want %#v", reqs[0], want)
	}
}

func TestSnapshotContentSubtreeWakeupStatusChanged_manifestCheckpoint(t *testing.T) {
	oldSC := &storagev1alpha1.SnapshotContent{Status: storagev1alpha1.SnapshotContentStatus{}}
	newSC := &storagev1alpha1.SnapshotContent{
		Status: storagev1alpha1.SnapshotContentStatus{ManifestCheckpointName: "mcp-1"},
	}
	if !snapshotContentSubtreeWakeupStatusChanged(oldSC, newSC) {
		t.Fatal("expected manifestCheckpointName change to wake subtree")
	}
}

func TestSnapshotContentSubtreeWakeupStatusChanged_readyCondition(t *testing.T) {
	oldSC := &storagev1alpha1.SnapshotContent{
		Status: storagev1alpha1.SnapshotContentStatus{
			Conditions: []metav1.Condition{
				{Type: snapshot.ConditionReady, Status: metav1.ConditionFalse, Reason: "Pending"},
			},
		},
	}
	newSC := &storagev1alpha1.SnapshotContent{
		Status: storagev1alpha1.SnapshotContentStatus{
			Conditions: []metav1.Condition{
				{Type: snapshot.ConditionReady, Status: metav1.ConditionTrue, Reason: "Ready"},
			},
		},
	}
	if !snapshotContentSubtreeWakeupStatusChanged(oldSC, newSC) {
		t.Fatal("expected Ready condition change to wake subtree")
	}
}
