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
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
)

func uploadTestClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := aggManifestTestScheme(t)
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&ssv1alpha1.ManifestCheckpoint{}, &storagev1alpha1.Snapshot{}).
		WithObjects(objs...).
		Build()
}

func importModeSnapshot(name, ns string, uid types.UID) *storagev1alpha1.Snapshot {
	return &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: uid},
		Spec:       storagev1alpha1.SnapshotSpec{Source: &storagev1alpha1.SnapshotSource{Import: &storagev1alpha1.SnapshotImportSource{}}},
	}
}

func uploadPayload(t *testing.T, childRefs ...UploadChildRef) []byte {
	t.Helper()
	manifests, _ := json.Marshal([]map[string]interface{}{
		{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": map[string]interface{}{"name": "data", "namespace": "ns1"}},
	})
	body, err := json.Marshal(ManifestsAndChildrenUpload{Manifests: manifests, ChildRefs: childRefs})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return body
}

func TestImportUpload_ReconstructsMCPAndWritesChildRefs(t *testing.T) {
	ctx := context.Background()
	snap := importModeSnapshot("snap", "ns1", types.UID("snap-uid"))
	cl := uploadTestClient(t, snap)
	svc := NewImportUploadService(cl)

	child := UploadChildRef{APIVersion: "storage.deckhouse.io/v1alpha1", Kind: "Snapshot", Name: "child"}
	cpName, err := svc.Upload(ctx, storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot"), "ns1", "snap", uploadPayload(t, child))
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if want := ReconstructedManifestCheckpointName(types.UID("snap-uid"), ""); cpName != want {
		t.Fatalf("checkpoint name = %q, want deterministic %q", cpName, want)
	}

	cp := &ssv1alpha1.ManifestCheckpoint{}
	if err := cl.Get(ctx, types.NamespacedName{Name: cpName}, cp); err != nil {
		t.Fatalf("get reconstructed MCP: %v", err)
	}
	if !meta.IsStatusConditionTrue(cp.Status.Conditions, ssv1alpha1.ManifestCheckpointConditionTypeReady) {
		t.Fatalf("reconstructed MCP must be Ready, got %v", cp.Status.Conditions)
	}
	if cp.Status.TotalObjects != 1 {
		t.Fatalf("want 1 object in MCP, got %d", cp.Status.TotalObjects)
	}

	got := &storagev1alpha1.Snapshot{}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "ns1", Name: "snap"}, got); err != nil {
		t.Fatalf("get snapshot: %v", err)
	}
	if len(got.Status.ChildrenSnapshotRefs) != 1 ||
		got.Status.ChildrenSnapshotRefs[0].Name != "child" ||
		got.Status.ChildrenSnapshotRefs[0].Kind != "Snapshot" {
		t.Fatalf("status.childrenSnapshotRefs not persisted: %#v", got.Status.ChildrenSnapshotRefs)
	}
}

func TestImportUpload_Idempotent(t *testing.T) {
	ctx := context.Background()
	snap := importModeSnapshot("snap", "ns1", types.UID("snap-uid"))
	cl := uploadTestClient(t, snap)
	svc := NewImportUploadService(cl)

	body := uploadPayload(t)
	first, err := svc.Upload(ctx, storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot"), "ns1", "snap", body)
	if err != nil {
		t.Fatalf("first upload: %v", err)
	}
	second, err := svc.Upload(ctx, storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot"), "ns1", "snap", body)
	if err != nil {
		t.Fatalf("second upload: %v", err)
	}
	if first != second {
		t.Fatalf("upload not idempotent: %q != %q", first, second)
	}
}

func TestImportUpload_RejectsNonImportMode(t *testing.T) {
	ctx := context.Background()
	// A Snapshot with no spec.source is a live-capture snapshot — upload must be refused.
	snap := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "ns1", UID: "u"}}
	cl := uploadTestClient(t, snap)
	svc := NewImportUploadService(cl)

	_, err := svc.Upload(ctx, storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot"), "ns1", "snap", uploadPayload(t))
	assertAggStatus(t, err, http.StatusConflict)
}

func TestImportUpload_RejectsBadPayload(t *testing.T) {
	ctx := context.Background()
	snap := importModeSnapshot("snap", "ns1", types.UID("snap-uid"))
	cl := uploadTestClient(t, snap)
	svc := NewImportUploadService(cl)

	gvk := storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot")

	// manifests missing.
	bad, _ := json.Marshal(ManifestsAndChildrenUpload{})
	_, err := svc.Upload(ctx, gvk, "ns1", "snap", bad)
	assertAggStatus(t, err, http.StatusBadRequest)

	// manifests not an array.
	notArray, _ := json.Marshal(map[string]interface{}{"manifests": map[string]string{"not": "array"}})
	_, err = svc.Upload(ctx, gvk, "ns1", "snap", notArray)
	assertAggStatus(t, err, http.StatusBadRequest)

	// childRef missing name.
	manifests, _ := json.Marshal([]map[string]interface{}{{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]interface{}{"name": "x", "namespace": "ns1"}}})
	badChild, _ := json.Marshal(ManifestsAndChildrenUpload{Manifests: manifests, ChildRefs: []UploadChildRef{{APIVersion: "v1", Kind: "ConfigMap"}}})
	_, err = svc.Upload(ctx, gvk, "ns1", "snap", badChild)
	assertAggStatus(t, err, http.StatusBadRequest)
}

func TestImportUpload_NotFound(t *testing.T) {
	ctx := context.Background()
	cl := uploadTestClient(t)
	svc := NewImportUploadService(cl)
	_, err := svc.Upload(ctx, storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot"), "ns1", "missing", uploadPayload(t))
	assertAggStatus(t, err, http.StatusNotFound)
}

// TestImportUpload_LeafSkipsChildrenStatusWriteUnderConflict pins the phase-5 regression: a leaf upload
// (no child refs) must not write status.childrenSnapshotRefs, so it never races whoever owns the CR's
// status. The concrete failure was the import VolumeSnapshot leaf — its forked status schema has no
// childrenSnapshotRefs field and the state-snapshotter binder reconciles its status continuously, so a
// blind Status().Update lost the conflict race for the whole retry budget and the upload returned 409.
// A non-leaf upload still needs the write, so the conflict must surface (no silent loss of child edges).
func TestImportUpload_LeafSkipsChildrenStatusWriteUnderConflict(t *testing.T) {
	ctx := context.Background()
	snap := importModeSnapshot("snap", "ns1", types.UID("snap-uid"))
	scheme := aggManifestTestScheme(t)
	// Fail every Snapshot status update (the competing status owner always wins). The reconstructed
	// ManifestCheckpoint status update is typed (*ManifestCheckpoint) and passes through untouched.
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&ssv1alpha1.ManifestCheckpoint{}, &storagev1alpha1.Snapshot{}).
		WithObjects(snap).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if u, ok := obj.(*unstructured.Unstructured); ok && u.GetKind() == "Snapshot" {
					return apierrors.NewConflict(
						schema.GroupResource{Group: "storage.deckhouse.io", Resource: "snapshots"},
						u.GetName(), fmt.Errorf("status writer race"))
				}
				return c.SubResource(subResourceName).Update(ctx, obj, opts...)
			},
		}).
		Build()
	svc := NewImportUploadService(cl)

	gvk := storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot")
	if _, err := svc.Upload(ctx, gvk, "ns1", "snap", uploadPayload(t)); err != nil {
		t.Fatalf("leaf upload must succeed despite a status-write conflict: %v", err)
	}

	child := UploadChildRef{APIVersion: "storage.deckhouse.io/v1alpha1", Kind: "Snapshot", Name: "child"}
	_, err := svc.Upload(ctx, gvk, "ns1", "snap", uploadPayload(t, child))
	assertAggStatus(t, err, http.StatusConflict)
}
