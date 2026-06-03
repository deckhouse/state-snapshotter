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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// D: the bound Snapshot Ready is a verbatim mirror of the root SnapshotContent Ready (status, reason,
// message). The binder must not recompute an alternative reason — SnapshotContent is the single source
// of truth for tree readiness (INV-COND2/INV-COND4, snapshot-rework/2026-06-03-snapshot-conditions-model.md).
func TestCheckConsistencyAndSetReadyMirrorsContentReadyVerbatim(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}

	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "root-content"}}
	meta.SetStatusCondition(&content.Status.Conditions, metav1.Condition{
		Type:    snapshot.ConditionReady,
		Status:  metav1.ConditionFalse,
		Reason:  snapshot.ReasonChildSnapshotFailed,
		Message: "child SnapshotContent child-a failed: reason=ChildSnapshotFailed message=child SnapshotContent leaf-broken failed: reason=ManifestCheckpointFailed message=ManifestCheckpoint mcp-leaf not found",
	})

	snapGVK := storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot")
	snapObj := &unstructured.Unstructured{}
	snapObj.SetGroupVersionKind(snapGVK)
	snapObj.SetName("root-snap")
	snapObj.SetNamespace("default")
	if err := unstructured.SetNestedField(snapObj.Object, "root-content", "status", "boundSnapshotContentName"); err != nil {
		t.Fatalf("set boundSnapshotContentName: %v", err)
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(content, snapObj).
		WithStatusSubresource(snapObj).
		Build()

	reg := snapshot.NewGVKRegistry()
	if err := reg.RegisterSnapshotContentMapping(
		"Snapshot", storagev1alpha1.SchemeGroupVersion.String(),
		"SnapshotContent", storagev1alpha1.SchemeGroupVersion.String(),
	); err != nil {
		t.Fatalf("register snapshot/content mapping: %v", err)
	}
	r := &GenericSnapshotBinderController{Client: cl, APIReader: cl, Scheme: scheme, GVKRegistry: reg}

	snapLike, err := snapshot.ExtractSnapshotLike(snapObj)
	if err != nil {
		t.Fatalf("extract snapshot like: %v", err)
	}
	if err := r.checkConsistencyAndSetReady(ctx, snapLike, snapObj); err != nil {
		t.Fatalf("checkConsistencyAndSetReady: %v", err)
	}

	fresh := &unstructured.Unstructured{}
	fresh.SetGroupVersionKind(snapGVK)
	if err := cl.Get(ctx, client.ObjectKey{Namespace: "default", Name: "root-snap"}, fresh); err != nil {
		t.Fatalf("get snapshot: %v", err)
	}
	freshLike, err := snapshot.ExtractSnapshotLike(fresh)
	if err != nil {
		t.Fatalf("extract fresh snapshot like: %v", err)
	}

	got := snapshot.GetCondition(freshLike, snapshot.ConditionReady)
	want := meta.FindStatusCondition(content.Status.Conditions, snapshot.ConditionReady)
	if got == nil {
		t.Fatalf("snapshot has no Ready condition after mirror")
	}
	if got.Status != want.Status || got.Reason != want.Reason || got.Message != want.Message {
		t.Fatalf("snapshot Ready is not a verbatim mirror:\n got  (%s/%s/%q)\n want (%s/%s/%q)",
			got.Status, got.Reason, got.Message, want.Status, want.Reason, want.Message)
	}
}
