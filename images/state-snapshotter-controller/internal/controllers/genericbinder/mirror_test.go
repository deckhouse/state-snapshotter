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

package genericbinder

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
)

// wave7 final-wave-1 moved the STEADY-STATE Ready mirror (content.Ready verbatim + phase=Failed bubble +
// barrier-2 gate) to the SnapshotContentController's single post-bind writer (ready_mirror.go). The binder's
// checkConsistencyAndSetReady must therefore:
//   - NOT overwrite Ready when the bound content exists (steady state is owned by the content controller), and
//   - still co-write the E3 degradation Ready=False/ContentMissing when the bound content is gone (a deleted
//     content produces no reconcile for the content controller to mirror from).
func newMirrorTestController(t *testing.T, objs ...client.Object) *GenericSnapshotBinderController {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(objs...).
		Build()
	reg := snapshot.NewGVKRegistry()
	if err := reg.RegisterSnapshotContentMapping(
		"Snapshot", storagev1alpha1.SchemeGroupVersion.String(),
		"SnapshotContent", storagev1alpha1.SchemeGroupVersion.String(),
	); err != nil {
		t.Fatalf("register snapshot/content mapping: %v", err)
	}
	return &GenericSnapshotBinderController{Client: cl, APIReader: cl, Scheme: scheme, GVKRegistry: reg}
}

func newBoundSnapshotUnstructured(name, contentName string) *unstructured.Unstructured {
	snapGVK := storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot")
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(snapGVK)
	obj.SetName(name)
	obj.SetNamespace("default")
	_ = unstructured.SetNestedField(obj.Object, contentName, "status", "boundSnapshotContentName")
	return obj
}

// E3 degradation: the bound content was deleted out from under the snapshot. The binder (not the content
// controller) must co-write Ready=False/ContentMissing.
func TestCheckConsistencyAndSetReady_ContentMissingCoWrite(t *testing.T) {
	ctx := context.Background()
	snapObj := newBoundSnapshotUnstructured("root-snap", "gone-content")
	r := newMirrorTestController(t, snapObj)

	snapLike, err := snapshot.ExtractSnapshotLike(snapObj)
	if err != nil {
		t.Fatalf("extract snapshot like: %v", err)
	}
	if err := r.checkConsistencyAndSetReady(ctx, snapLike, snapObj); err != nil {
		t.Fatalf("checkConsistencyAndSetReady: %v", err)
	}

	got := freshSnapshotReady(t, r.Client, "root-snap")
	if got == nil {
		t.Fatalf("snapshot has no Ready condition after content-missing co-write")
	}
	if got.Status != metav1.ConditionFalse || got.Reason != snapshot.ReasonContentMissing {
		t.Fatalf("want Ready=False/ContentMissing, got %s/%s", got.Status, got.Reason)
	}
}

// Steady state: the bound content exists and is Ready. The binder must NOT write/overwrite the snapshot's
// Ready condition — that is owned by the SnapshotContentController's single post-bind writer.
func TestCheckConsistencyAndSetReady_DoesNotOverwriteReadyWhenContentPresent(t *testing.T) {
	ctx := context.Background()
	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "root-content"}}
	content.Status.Conditions = []metav1.Condition{{
		Type:    snapshot.ConditionReady,
		Status:  metav1.ConditionTrue,
		Reason:  snapshot.ReasonCompleted,
		Message: "ready",
	}}
	snapObj := newBoundSnapshotUnstructured("root-snap", "root-content")
	r := newMirrorTestController(t, content, snapObj)

	snapLike, err := snapshot.ExtractSnapshotLike(snapObj)
	if err != nil {
		t.Fatalf("extract snapshot like: %v", err)
	}
	if err := r.checkConsistencyAndSetReady(ctx, snapLike, snapObj); err != nil {
		t.Fatalf("checkConsistencyAndSetReady: %v", err)
	}

	if got := freshSnapshotReady(t, r.Client, "root-snap"); got != nil {
		t.Fatalf("binder must not write Ready when content is present (owned by content controller), got %s/%s", got.Status, got.Reason)
	}
}

func freshSnapshotReady(t *testing.T, cl client.Client, name string) *metav1.Condition {
	t.Helper()
	snapGVK := storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot")
	fresh := &unstructured.Unstructured{}
	fresh.SetGroupVersionKind(snapGVK)
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, fresh); err != nil {
		t.Fatalf("get snapshot: %v", err)
	}
	freshLike, err := snapshot.ExtractSnapshotLike(fresh)
	if err != nil {
		t.Fatalf("extract fresh snapshot like: %v", err)
	}
	return snapshot.GetCondition(freshLike, snapshot.ConditionReady)
}
