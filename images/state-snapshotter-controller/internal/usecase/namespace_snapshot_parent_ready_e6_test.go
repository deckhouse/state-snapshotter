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

func TestPickParentReadyReasonE6_PriorityChildFailedOverSubtree(t *testing.T) {
	t.Parallel()
	out := PickParentReadyReasonE6(E6ParentReadyPickInput{
		HasChildFailed:                true,
		ChildFailedMessage:            "child failed",
		SubtreeManifestCapturePending: true,
		SubtreeMessage:                "subtree pending",
		HasChildPending:               true,
		ChildPendingMessage:           "child pending",
		SelfCaptureComplete:           false,
	})
	if out.Ready || out.Reason != snapshot.ReasonChildSnapshotFailed || out.Message != "child failed" {
		t.Fatalf("got %+v", out)
	}
}

func TestPickParentReadyReasonE6_SubtreeOverChildPending(t *testing.T) {
	t.Parallel()
	out := PickParentReadyReasonE6(E6ParentReadyPickInput{
		HasChildFailed:                false,
		SubtreeManifestCapturePending: true,
		SubtreeMessage:                "exclude not ready",
		HasChildPending:               true,
		ChildPendingMessage:           "waiting child",
		SelfCaptureComplete:           false,
	})
	if out.Ready || out.Reason != snapshot.ReasonSubtreeManifestCapturePending {
		t.Fatalf("got %+v", out)
	}
}

func TestPickParentReadyReasonE6_ChildPendingOverCompleted(t *testing.T) {
	t.Parallel()
	out := PickParentReadyReasonE6(E6ParentReadyPickInput{
		HasChildFailed:      false,
		HasChildPending:     true,
		ChildPendingMessage: "bound",
		SelfCaptureComplete: true,
	})
	if out.Ready || out.Reason != snapshot.ReasonChildSnapshotPending {
		t.Fatalf("got %+v", out)
	}
}

func TestPickParentReadyReasonE6_Completed(t *testing.T) {
	t.Parallel()
	out := PickParentReadyReasonE6(E6ParentReadyPickInput{
		SelfCaptureComplete: true,
	})
	if !out.Ready || out.Reason != snapshot.ReasonCompleted {
		t.Fatalf("got %+v", out)
	}
}

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

func refDemoDisk(ns, name string) storagev1alpha1.NamespaceSnapshotChildRef {
	return storagev1alpha1.NamespaceSnapshotChildRef{
		APIVersion: "demo.example.com/v1",
		Kind:       "DemoDiskSnapshot",
		Namespace:  ns,
		Name:       name,
	}
}

func TestSummarizeChildrenSnapshotRefsForParentReadyE6_InvalidRefFieldsFailed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cl := fake.NewClientBuilder().Build()
	sum, err := SummarizeChildrenSnapshotRefsForParentReadyE6(ctx, cl, []storagev1alpha1.NamespaceSnapshotChildRef{
		{APIVersion: "", Kind: "X", Name: "n", Namespace: "ns"},
	}, "ns")
	if err != nil {
		t.Fatal(err)
	}
	if !sum.HasFailed || sum.HasPending || sum.AllCompleted {
		t.Fatalf("got %+v", sum)
	}
}

func TestSummarizeChildrenSnapshotRefsForParentReadyE6_RefNamespaceEmptyMeansParentNS(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := "ns1"
	gvk := schema.GroupVersionKind{Group: "demo.example.com", Version: "v1", Kind: "DemoDiskSnapshot"}
	u := demoSnapshotUnstructuredReady(ns, "disk-a", gvk, "c1", metav1.ConditionTrue, snapshot.ReasonCompleted)
	cl := fake.NewClientBuilder().WithRuntimeObjects(u).Build()
	sum, err := SummarizeChildrenSnapshotRefsForParentReadyE6(ctx, cl, []storagev1alpha1.NamespaceSnapshotChildRef{
		{APIVersion: "demo.example.com/v1", Kind: "DemoDiskSnapshot", Namespace: "", Name: "disk-a"},
	}, ns)
	if err != nil {
		t.Fatal(err)
	}
	if sum.HasFailed || sum.HasPending || !sum.AllCompleted {
		t.Fatalf("got %+v", sum)
	}
}

func TestSummarizeChildrenSnapshotRefsForParentReadyE6_RefNamespaceEqualsParentNS(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := "ns1"
	gvk := schema.GroupVersionKind{Group: "demo.example.com", Version: "v1", Kind: "DemoDiskSnapshot"}
	u := demoSnapshotUnstructuredReady(ns, "disk-a", gvk, "c1", metav1.ConditionTrue, snapshot.ReasonCompleted)
	cl := fake.NewClientBuilder().WithRuntimeObjects(u).Build()
	sum, err := SummarizeChildrenSnapshotRefsForParentReadyE6(ctx, cl, []storagev1alpha1.NamespaceSnapshotChildRef{
		{APIVersion: "demo.example.com/v1", Kind: "DemoDiskSnapshot", Namespace: ns, Name: "disk-a"},
	}, ns)
	if err != nil {
		t.Fatal(err)
	}
	if sum.HasFailed || sum.HasPending || !sum.AllCompleted {
		t.Fatalf("got %+v", sum)
	}
}

func TestSummarizeChildrenSnapshotRefsForParentReadyE6_RefNamespaceCrossNSFailClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	nsParent := "ns-parent"
	nsOther := "ns-other"
	ref := storagev1alpha1.NamespaceSnapshotChildRef{
		APIVersion: "demo.example.com/v1", Kind: "DemoDiskSnapshot", Namespace: nsOther, Name: "disk-a",
	}
	cl := fake.NewClientBuilder().Build()
	_, _, gerr := GetChildSnapshot(ctx, cl, ref, nsParent)
	if !errors.Is(gerr, ErrInvalidChildSnapshotRefNamespace) {
		t.Fatalf("GetChildSnapshot: want ErrInvalidChildSnapshotRefNamespace, got %v", gerr)
	}
	sum, err := SummarizeChildrenSnapshotRefsForParentReadyE6(ctx, cl, []storagev1alpha1.NamespaceSnapshotChildRef{ref}, nsParent)
	if err != nil {
		t.Fatal(err)
	}
	if !sum.HasFailed || sum.HasPending || sum.AllCompleted || len(sum.FailedMessages) != 1 {
		t.Fatalf("expected HasFailed with one message for cross-namespace ref, got %+v", sum)
	}
}

func TestSummarizeChildrenSnapshotRefsForParentReadyE6_GenericChildCompleted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := "ns1"
	gvk := schema.GroupVersionKind{Group: "demo.example.com", Version: "v1", Kind: "DemoDiskSnapshot"}
	u := demoSnapshotUnstructuredReady(ns, "disk-a", gvk, "content-1", metav1.ConditionTrue, snapshot.ReasonCompleted)
	cl := fake.NewClientBuilder().WithRuntimeObjects(u).Build()
	sum, err := SummarizeChildrenSnapshotRefsForParentReadyE6(ctx, cl, []storagev1alpha1.NamespaceSnapshotChildRef{
		refDemoDisk(ns, "disk-a"),
	}, ns)
	if err != nil {
		t.Fatal(err)
	}
	if sum.HasFailed || sum.HasPending || !sum.AllCompleted {
		t.Fatalf("got %+v", sum)
	}
}

func TestSummarizeChildrenSnapshotRefsForParentReadyE6_SameNameDifferentKindsBothCompleted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := "ns1"
	name := "same-name"
	gvkA := schema.GroupVersionKind{Group: "generic.state-snapshotter.test", Version: "v1", Kind: "SnapA"}
	gvkB := schema.GroupVersionKind{Group: "generic.state-snapshotter.test", Version: "v1", Kind: "SnapB"}
	a := demoSnapshotUnstructuredReady(ns, name, gvkA, "c1", metav1.ConditionTrue, snapshot.ReasonCompleted)
	b := demoSnapshotUnstructuredReady(ns, name, gvkB, "c2", metav1.ConditionTrue, snapshot.ReasonCompleted)
	cl := fake.NewClientBuilder().WithRuntimeObjects(a, b).Build()
	sum, err := SummarizeChildrenSnapshotRefsForParentReadyE6(ctx, cl, []storagev1alpha1.NamespaceSnapshotChildRef{
		{APIVersion: "generic.state-snapshotter.test/v1", Kind: "SnapA", Namespace: ns, Name: name},
		{APIVersion: "generic.state-snapshotter.test/v1", Kind: "SnapB", Namespace: ns, Name: name},
	}, ns)
	if err != nil {
		t.Fatal(err)
	}
	if sum.HasFailed || sum.HasPending || !sum.AllCompleted {
		t.Fatalf("got %+v", sum)
	}
}

func TestSummarizeChildrenSnapshotRefsForParentReadyE6_NotFoundPending(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cl := fake.NewClientBuilder().Build()
	sum, err := SummarizeChildrenSnapshotRefsForParentReadyE6(ctx, cl, []storagev1alpha1.NamespaceSnapshotChildRef{
		refDemoDisk("ns", "missing"),
	}, "ns")
	if err != nil {
		t.Fatal(err)
	}
	if sum.HasFailed || !sum.HasPending || sum.AllCompleted {
		t.Fatalf("got %+v", sum)
	}
}

func TestSummarizeChildrenSnapshotRefsForParentReadyE6_MultiPending(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := "ns1"
	gvk := schema.GroupVersionKind{Group: "demo.example.com", Version: "v1", Kind: "DemoDiskSnapshot"}
	a := demoSnapshotUnstructuredReady(ns, "a", gvk, "", "", "")
	b := demoSnapshotUnstructuredReady(ns, "b", gvk, "z", metav1.ConditionTrue, snapshot.ReasonCompleted)
	cl := fake.NewClientBuilder().WithRuntimeObjects(a, b).Build()
	sum, err := SummarizeChildrenSnapshotRefsForParentReadyE6(ctx, cl, []storagev1alpha1.NamespaceSnapshotChildRef{
		refDemoDisk(ns, "a"),
		refDemoDisk(ns, "b"),
	}, ns)
	if err != nil {
		t.Fatal(err)
	}
	if sum.HasFailed || !sum.HasPending {
		t.Fatalf("got %+v", sum)
	}
}

func TestSummarizeChildrenSnapshotRefsForParentReadyE6_GetErrorPropagates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	bad := errGetClient{err: errors.New("get failed")}
	_, err := SummarizeChildrenSnapshotRefsForParentReadyE6(ctx, bad, []storagev1alpha1.NamespaceSnapshotChildRef{
		refDemoDisk("ns", "x"),
	}, "ns")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestClassifyGenericChildSnapshotReady_TerminalVsPending(t *testing.T) {
	t.Parallel()
	gvk := schema.GroupVersionKind{Group: "demo.example.com", Version: "v1", Kind: "DemoDiskSnapshot"}
	u := demoSnapshotUnstructuredReady("ns", "x", gvk, "nsc", metav1.ConditionFalse, "CapturePlanDrift")
	c, msg := ClassifyGenericChildSnapshotReady(u, gvk, "ns", "x")
	if c != NamespaceSnapshotChildReadyClassFailed || msg == "" {
		t.Fatalf("got %v %q", c, msg)
	}
	u2 := demoSnapshotUnstructuredReady("ns", "y", gvk, "nsc2", metav1.ConditionFalse, "ManifestCheckpointPending")
	c2, _ := ClassifyGenericChildSnapshotReady(u2, gvk, "ns", "y")
	if c2 != NamespaceSnapshotChildReadyClassPending {
		t.Fatalf("got %v", c2)
	}
}

func TestClassifyNamespaceSnapshotChildReady_DelegatesToGeneric(t *testing.T) {
	t.Parallel()
	ch := &storagev1alpha1.NamespaceSnapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "leaf"},
		Status: storagev1alpha1.NamespaceSnapshotStatus{
			BoundSnapshotContentName: "nsc",
			Conditions: []metav1.Condition{{
				Type: snapshot.ConditionReady, Status: metav1.ConditionTrue, Reason: snapshot.ReasonCompleted,
			}},
		},
	}
	c, msg := ClassifyNamespaceSnapshotChildReady(ch)
	if c != NamespaceSnapshotChildReadyClassCompleted || msg != "" {
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
