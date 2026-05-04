package controllers

import (
	"context"
	"testing"

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
