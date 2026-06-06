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

package usecase

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

func demoSnapshotUnstructuredReady(ns, name string, gvk schema.GroupVersionKind, bound string, ready metav1.ConditionStatus, reason string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace(ns)
	u.SetName(name)
	_ = unstructured.SetNestedField(u.Object, bound, "status", "boundSnapshotContentName")
	if ready != "" {
		cond := map[string]interface{}{
			"type": snapshot.ConditionReady, "status": string(ready), "reason": reason, "message": "",
		}
		_ = unstructured.SetNestedSlice(u.Object, []interface{}{cond}, "status", "conditions")
	}
	return u
}

func refDemoDisk(name string) storagev1alpha1.SnapshotChildRef {
	return storagev1alpha1.SnapshotChildRef{
		APIVersion: "demo.example.com/v1",
		Kind:       "DemoDiskSnapshot",
		Name:       name,
	}
}

func TestSummarizeChildSnapshotTerminalFailures_InvalidRefFailed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cl := fake.NewClientBuilder().Build()
	sum, err := SummarizeChildSnapshotTerminalFailures(ctx, cl, []storagev1alpha1.SnapshotChildRef{
		{APIVersion: "", Kind: "X", Name: "n"},
	}, "ns")
	if err != nil {
		t.Fatal(err)
	}
	if !sum.HasFailed || len(sum.Messages) == 0 {
		t.Fatalf("got %+v", sum)
	}
}

func TestSummarizeChildSnapshotTerminalFailures_TerminalChildFailed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := "ns1"
	gvk := schema.GroupVersionKind{Group: "demo.example.com", Version: "v1", Kind: "DemoDiskSnapshot"}
	u := demoSnapshotUnstructuredReady(ns, "disk-a", gvk, "c1", metav1.ConditionFalse, "CapturePlanDrift")
	cl := fake.NewClientBuilder().WithRuntimeObjects(u).Build()
	sum, err := SummarizeChildSnapshotTerminalFailures(ctx, cl, []storagev1alpha1.SnapshotChildRef{
		refDemoDisk("disk-a"),
	}, ns)
	if err != nil {
		t.Fatal(err)
	}
	if !sum.HasFailed || len(sum.Messages) == 0 {
		t.Fatalf("got %+v", sum)
	}
}

func TestSummarizeChildSnapshotTerminalFailures_CompletedChildNotFailed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := "ns1"
	gvk := schema.GroupVersionKind{Group: "demo.example.com", Version: "v1", Kind: "DemoDiskSnapshot"}
	u := demoSnapshotUnstructuredReady(ns, "disk-a", gvk, "content-1", metav1.ConditionTrue, snapshot.ReasonCompleted)
	cl := fake.NewClientBuilder().WithRuntimeObjects(u).Build()
	sum, err := SummarizeChildSnapshotTerminalFailures(ctx, cl, []storagev1alpha1.SnapshotChildRef{
		refDemoDisk("disk-a"),
	}, ns)
	if err != nil {
		t.Fatal(err)
	}
	if sum.HasFailed {
		t.Fatalf("got %+v", sum)
	}
}

func TestSummarizeChildSnapshotTerminalFailures_NotFoundIsNotTerminal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cl := fake.NewClientBuilder().Build()
	sum, err := SummarizeChildSnapshotTerminalFailures(ctx, cl, []storagev1alpha1.SnapshotChildRef{
		refDemoDisk("missing"),
	}, "ns")
	if err != nil {
		t.Fatal(err)
	}
	if sum.HasFailed {
		t.Fatalf("not-found child must not be a terminal failure, got %+v", sum)
	}
}

func TestSummarizeChildSnapshotTerminalFailures_PendingChildIsNotTerminal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := "ns1"
	gvk := schema.GroupVersionKind{Group: "demo.example.com", Version: "v1", Kind: "DemoDiskSnapshot"}
	// Unbound child (no boundSnapshotContentName) classifies as pending, not terminal.
	u := demoSnapshotUnstructuredReady(ns, "disk-a", gvk, "", "", "")
	cl := fake.NewClientBuilder().WithRuntimeObjects(u).Build()
	sum, err := SummarizeChildSnapshotTerminalFailures(ctx, cl, []storagev1alpha1.SnapshotChildRef{
		refDemoDisk("disk-a"),
	}, ns)
	if err != nil {
		t.Fatal(err)
	}
	if sum.HasFailed {
		t.Fatalf("pending child must not be a terminal failure, got %+v", sum)
	}
}

func TestSummarizeChildSnapshotTerminalFailures_DoesNotReadOtherNamespace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	nsParent := "ns-parent"
	nsOther := "ns-other"
	gvk := schema.GroupVersionKind{Group: "demo.example.com", Version: "v1", Kind: "DemoDiskSnapshot"}
	// A terminally-failed child living only in another namespace must be invisible to the parent.
	other := demoSnapshotUnstructuredReady(nsOther, "disk-a", gvk, "c1", metav1.ConditionFalse, "CapturePlanDrift")
	cl := fake.NewClientBuilder().WithRuntimeObjects(other).Build()
	ref := storagev1alpha1.SnapshotChildRef{APIVersion: "demo.example.com/v1", Kind: "DemoDiskSnapshot", Name: "disk-a"}
	sum, err := SummarizeChildSnapshotTerminalFailures(ctx, cl, []storagev1alpha1.SnapshotChildRef{ref}, nsParent)
	if err != nil {
		t.Fatal(err)
	}
	if sum.HasFailed {
		t.Fatalf("child in another namespace must not be visible, got %+v", sum)
	}
}

func TestSummarizeChildSnapshotTerminalFailures_GetErrorPropagates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	bad := errGetClient{err: errors.New("get failed")}
	_, err := SummarizeChildSnapshotTerminalFailures(ctx, bad, []storagev1alpha1.SnapshotChildRef{
		refDemoDisk("x"),
	}, "ns")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestClassifyGenericChildSnapshotReady_TerminalVsPending(t *testing.T) {
	t.Parallel()
	gvk := schema.GroupVersionKind{Group: "demo.example.com", Version: "v1", Kind: "DemoDiskSnapshot"}
	u := demoSnapshotUnstructuredReady("ns", "x", gvk, "content", metav1.ConditionFalse, "CapturePlanDrift")
	c, msg := ClassifyGenericChildSnapshotReady(u, gvk, "ns", "x")
	if c != SnapshotChildReadyClassFailed || msg == "" {
		t.Fatalf("got %v %q", c, msg)
	}
	u2 := demoSnapshotUnstructuredReady("ns", "y", gvk, "content2", metav1.ConditionFalse, "ManifestCheckpointPending")
	c2, _ := ClassifyGenericChildSnapshotReady(u2, gvk, "ns", "y")
	if c2 != SnapshotChildReadyClassPending {
		t.Fatalf("got %v", c2)
	}
}

func TestClassifySnapshotChildReady_DelegatesToGeneric(t *testing.T) {
	t.Parallel()
	ch := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "leaf"},
		Status: storagev1alpha1.SnapshotStatus{
			BoundSnapshotContentName: "content",
			Conditions: []metav1.Condition{{
				Type: snapshot.ConditionReady, Status: metav1.ConditionTrue, Reason: snapshot.ReasonCompleted,
			}},
		},
	}
	c, msg := ClassifySnapshotChildReady(ch)
	if c != SnapshotChildReadyClassCompleted || msg != "" {
		t.Fatalf("got %v %q", c, msg)
	}
}

type errGetClient struct {
	err error
}

func (e errGetClient) Get(_ context.Context, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	return e.err
}

func (errGetClient) List(_ context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	return nil
}
