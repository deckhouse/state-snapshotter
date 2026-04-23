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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

func e6TestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme storage: %v", err)
	}
	return s
}

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

func TestSummarizeNamespaceSnapshotChildrenRefsWithDefaultNamespace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := "ns1"
	c1 := &storagev1alpha1.NamespaceSnapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "c1"},
		Status: storagev1alpha1.NamespaceSnapshotStatus{
			BoundSnapshotContentName: "x",
			Conditions: []metav1.Condition{{
				Type: snapshot.ConditionReady, Status: metav1.ConditionTrue, Reason: snapshot.ReasonCompleted,
			}},
		},
	}
	c2 := &storagev1alpha1.NamespaceSnapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "c2"},
		Status: storagev1alpha1.NamespaceSnapshotStatus{
			BoundSnapshotContentName: "y",
			Conditions: []metav1.Condition{{
				Type: snapshot.ConditionReady, Status: metav1.ConditionFalse, Reason: "CapturePlanDrift",
			}},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(e6TestScheme(t)).WithObjects(c1, c2).WithStatusSubresource(c1, c2).Build()
	refs := []storagev1alpha1.NamespaceSnapshotChildRef{
		{Name: "c1", Namespace: ""},
		{Name: "c2", Namespace: ns},
	}
	sum, err := SummarizeNamespaceSnapshotChildrenRefsWithDefaultNamespace(ctx, cl, refs, ns)
	if err != nil {
		t.Fatal(err)
	}
	if !sum.HasFailed || sum.HasPending || sum.AllCompleted {
		t.Fatalf("got %+v", sum)
	}
}

func TestSummarizeNamespaceSnapshotChildrenRefs_MultiPending(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := "ns1"
	a := &storagev1alpha1.NamespaceSnapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "a"},
		Status:     storagev1alpha1.NamespaceSnapshotStatus{BoundSnapshotContentName: ""},
	}
	b := &storagev1alpha1.NamespaceSnapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "b"},
		Status: storagev1alpha1.NamespaceSnapshotStatus{
			BoundSnapshotContentName: "z",
			Conditions: []metav1.Condition{{
				Type: snapshot.ConditionReady, Status: metav1.ConditionTrue, Reason: snapshot.ReasonCompleted,
			}},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(e6TestScheme(t)).WithObjects(a, b).WithStatusSubresource(a, b).Build()
	sum, err := SummarizeNamespaceSnapshotChildrenRefsWithDefaultNamespace(ctx, cl, []storagev1alpha1.NamespaceSnapshotChildRef{
		{Namespace: ns, Name: "a"},
		{Namespace: ns, Name: "b"},
	}, ns)
	if err != nil {
		t.Fatal(err)
	}
	if sum.HasFailed || !sum.HasPending {
		t.Fatalf("got %+v", sum)
	}
}

func TestSummarize_NotFoundIsPending(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cl := fake.NewClientBuilder().WithScheme(e6TestScheme(t)).Build()
	sum, err := SummarizeNamespaceSnapshotChildrenRefsWithDefaultNamespace(ctx, cl, []storagev1alpha1.NamespaceSnapshotChildRef{
		{Namespace: "ns", Name: "missing"},
	}, "ns")
	if err != nil {
		t.Fatal(err)
	}
	if sum.HasFailed || !sum.HasPending {
		t.Fatalf("got %+v", sum)
	}
}

func fakeChild(ns, name string, ready metav1.ConditionStatus, reason string) *storagev1alpha1.NamespaceSnapshot {
	ch := &storagev1alpha1.NamespaceSnapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Status: storagev1alpha1.NamespaceSnapshotStatus{
			BoundSnapshotContentName: "nsc-" + name,
		},
	}
	if ready != "" {
		meta.SetStatusCondition(&ch.Status.Conditions, metav1.Condition{
			Type: snapshot.ConditionReady, Status: ready, Reason: reason,
		})
	}
	return ch
}

func TestClassifyNamespaceSnapshotChildReady_TerminalVsPending(t *testing.T) {
	t.Parallel()
	ch := fakeChild("ns", "x", metav1.ConditionFalse, "CapturePlanDrift")
	c, msg := ClassifyNamespaceSnapshotChildReady(ch)
	if c != NamespaceSnapshotChildReadyClassFailed || msg == "" {
		t.Fatalf("got %v %q", c, msg)
	}
	ch2 := fakeChild("ns", "y", metav1.ConditionFalse, "ManifestCheckpointPending")
	c2, _ := ClassifyNamespaceSnapshotChildReady(ch2)
	if c2 != NamespaceSnapshotChildReadyClassPending {
		t.Fatalf("got %v", c2)
	}
}

func TestSummarize_ClientGetErrorPropagates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	bad := errGetClient{err: errors.New("get failed")}
	_, err := SummarizeNamespaceSnapshotChildrenRefsWithDefaultNamespace(ctx, bad, []storagev1alpha1.NamespaceSnapshotChildRef{
		{Namespace: "ns", Name: "x"},
	}, "ns")
	if err == nil {
		t.Fatal("expected error")
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
