package controllers

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
)

func TestSnapshotContentControllerCascadeRemoveFinalizersFromCommonChildren(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}

	child := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "child-content",
			Finalizers: []string{snapshot.FinalizerParentProtect},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(child).Build()
	r := &SnapshotContentController{
		Client:      cl,
		APIReader:   cl,
		GVKRegistry: snapshot.NewGVKRegistry(),
	}

	rootObj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": storagev1alpha1.SchemeGroupVersion.String(),
			"kind":       "SnapshotContent",
			"metadata": map[string]interface{}{
				"name": "root-content",
			},
			"status": map[string]interface{}{
				"childrenSnapshotContentRefs": []interface{}{
					map[string]interface{}{"name": "child-content"},
				},
			},
		},
	}
	rootObj.SetGroupVersionKind(unifiedbootstrap.CommonSnapshotContentGVK())
	contentLike, err := snapshot.ExtractSnapshotContentLike(rootObj)
	if err != nil {
		t.Fatalf("extract content like: %v", err)
	}

	if err := r.cascadeRemoveFinalizersFromChildren(ctx, contentLike, rootObj); err != nil {
		t.Fatalf("cascade finalizers: %v", err)
	}

	freshChild := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: "child-content"}, freshChild); err != nil {
		t.Fatalf("get child: %v", err)
	}
	for _, finalizer := range freshChild.Finalizers {
		if finalizer == snapshot.FinalizerParentProtect {
			t.Fatalf("child parent-protect finalizer was not removed: %v", freshChild.Finalizers)
		}
	}
}

func TestBuildCommonSnapshotContentStatusPlanWaitsForGraphReady(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}

	snap := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": storagev1alpha1.SchemeGroupVersion.String(),
		"kind":       "NamespaceSnapshot",
		"metadata": map[string]interface{}{
			"name":      "snap",
			"namespace": "ns",
		},
		"status": map[string]interface{}{
			"conditions": []interface{}{map[string]interface{}{
				"type":               snapshot.ConditionGraphReady,
				"status":             string(metav1.ConditionFalse),
				"reason":             snapshot.ReasonGraphPlanningFailed,
				"message":            "planning failed",
				"lastTransitionTime": metav1.Now().Format(time.RFC3339),
			}},
		},
	}}
	snap.SetGroupVersionKind(storagev1alpha1.SchemeGroupVersion.WithKind("NamespaceSnapshot"))
	content := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": storagev1alpha1.SchemeGroupVersion.String(),
		"kind":       "SnapshotContent",
		"metadata": map[string]interface{}{
			"name": "content",
		},
		"spec": map[string]interface{}{
			"snapshotRef": map[string]interface{}{
				"apiVersion": storagev1alpha1.SchemeGroupVersion.String(),
				"kind":       "NamespaceSnapshot",
				"name":       "snap",
				"namespace":  "ns",
			},
		},
	}}
	content.SetGroupVersionKind(unifiedbootstrap.CommonSnapshotContentGVK())

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(snap).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, content)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.readyStatus != metav1.ConditionFalse || plan.readyReason != snapshot.ReasonChildGraphPending {
		t.Fatalf("expected content pending on GraphReady=False, got status=%s reason=%s", plan.readyStatus, plan.readyReason)
	}
	if plan.manifestCheckpointName != "" {
		t.Fatalf("content must not publish manifest checkpoint while graph is stale, got %q", plan.manifestCheckpointName)
	}
}

func TestEnsureChildSnapshotContentOwnedByParentDoesNotStealConflictingOwner(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}
	child := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Name: "child-content",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
				Kind:       "SnapshotContent",
				Name:       "other-parent",
				UID:        "other-uid",
			}},
		},
	}
	parent := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": storagev1alpha1.SchemeGroupVersion.String(),
		"kind":       "SnapshotContent",
		"metadata": map[string]interface{}{
			"name": "parent-content",
			"uid":  "parent-uid",
		},
	}}
	parent.SetGroupVersionKind(unifiedbootstrap.CommonSnapshotContentGVK())
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(child).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	if err := r.ensureChildSnapshotContentOwnedByParent(ctx, "child-content", parent); err == nil {
		t.Fatal("expected conflicting child content ownerRef to fail closed")
	}
	fresh := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: "child-content"}, fresh); err != nil {
		t.Fatalf("get child content: %v", err)
	}
	if got := fresh.OwnerReferences[0].Name; got != "other-parent" {
		t.Fatalf("child content ownerRef was stolen: got owner %q", got)
	}
}
