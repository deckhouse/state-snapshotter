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
	"errors"
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

const (
	testSnapName    = "snap"
	testSnapNS      = "ns1"
	testSnapUID     = "snap-uid"
	testContentName = "content-snap"
	testContentUID  = "content-uid"
)

func coreSnapshotGVK() schema.GroupVersionKind {
	return storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot")
}

func uploadTestClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := aggManifestTestScheme(t)
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&ssv1alpha1.ManifestCheckpoint{}, &storagev1alpha1.Snapshot{}).
		WithObjects(objs...).
		Build()
}

// importModeSnapshot builds an import-mode core Snapshot. bound sets status.boundSnapshotContentName so
// the bind-first gate passes.
func importModeSnapshot(bound bool) *storagev1alpha1.Snapshot {
	snap := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: testSnapName, Namespace: testSnapNS, UID: types.UID(testSnapUID)},
		Spec:       storagev1alpha1.SnapshotSpec{Mode: storagev1alpha1.SnapshotModeImport},
	}
	if bound {
		snap.Status.BoundSnapshotContentName = testContentName
	}
	return snap
}

// importContent builds the SnapshotContent the binder would create for the import Snapshot, with the
// back-reference (spec.snapshotRef, uid included) the anti-spoofing check requires and the content-slug
// UID the reconstructed-MCP name is keyed to.
func importContent() *storagev1alpha1.SnapshotContent {
	return &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: testContentName, UID: types.UID(testContentUID)},
		Spec: storagev1alpha1.SnapshotContentSpec{
			SnapshotRef: &storagev1alpha1.SnapshotSubjectRef{
				APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
				Kind:       "Snapshot",
				Namespace:  testSnapNS,
				Name:       testSnapName,
				UID:        types.UID(testSnapUID),
			},
		},
	}
}

func uploadPayload(t *testing.T, childRefs ...UploadChildRef) []byte {
	t.Helper()
	manifests, _ := json.Marshal([]map[string]interface{}{
		{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": map[string]interface{}{"name": "data", "namespace": testSnapNS}},
	})
	body, err := json.Marshal(ManifestsAndChildrenUpload{Manifests: manifests, ChildRefs: childRefs})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return body
}

func assertAggReason(t *testing.T, err error, wantStatus int, wantReason string) {
	t.Helper()
	var st *AggregatedStatusError
	if !errors.As(err, &st) {
		t.Fatalf("expected AggregatedStatusError, got %T: %v", err, err)
	}
	if st.HTTPStatus != wantStatus || st.Reason != wantReason {
		t.Fatalf("expected HTTP %d reason %q, got %d %q (%s)", wantStatus, wantReason, st.HTTPStatus, st.Reason, st.Message)
	}
}

// TestImportUpload_HappyPath_BoundSnapshot pins the two-layer happy path for a bound core Snapshot: the
// namespaced layer records childRefs, and once the CR is bound it forwards the manifests to the
// content-addressed layer, which reconstructs the MCP owned by the SnapshotContent FROM BIRTH (no
// ObjectKeeper backstop). The checkpoint name stays deterministic from the snapshot UID.
func TestImportUpload_HappyPath_BoundSnapshot(t *testing.T) {
	ctx := context.Background()
	cl := uploadTestClient(t, importModeSnapshot(true), importContent())
	svc := NewImportUploadService(cl)

	child := UploadChildRef{APIVersion: storagev1alpha1.SchemeGroupVersion.String(), Kind: "Snapshot", Name: "child"}
	cpName, err := svc.Upload(ctx, coreSnapshotGVK(), testSnapNS, testSnapName, uploadPayload(t, child), false)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if want := ReconstructedManifestCheckpointName(types.UID(testSnapUID), ""); cpName != want {
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
	if cp.Labels[ReconstructedManifestCheckpointLabelKey] != reconstructedManifestCheckpointLabelValue {
		t.Fatalf("reconstructed MCP must carry the %s label, got %v", ReconstructedManifestCheckpointLabelKey, cp.Labels)
	}

	// The MCP is owned by the SnapshotContent from birth — no import ObjectKeeper, no aggregator handoff.
	ctrlRef := metav1.GetControllerOf(cp)
	if ctrlRef == nil || ctrlRef.Kind != "SnapshotContent" || ctrlRef.Name != testContentName || ctrlRef.UID != types.UID(testContentUID) {
		t.Fatalf("reconstructed MCP must be controller-owned by SnapshotContent %s, got %#v", testContentName, cp.OwnerReferences)
	}
	for _, ref := range cp.OwnerReferences {
		if ref.Kind == "ObjectKeeper" {
			t.Fatalf("reconstructed MCP must NOT carry an ObjectKeeper owner anymore, got %#v", cp.OwnerReferences)
		}
	}

	// childRefs are recorded on the NAMESPACED layer (the CR's own status), not on the content layer.
	got := &storagev1alpha1.Snapshot{}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: testSnapNS, Name: testSnapName}, got); err != nil {
		t.Fatalf("get snapshot: %v", err)
	}
	if len(got.Status.ChildrenSnapshotRefs) != 1 ||
		got.Status.ChildrenSnapshotRefs[0].Name != "child" ||
		got.Status.ChildrenSnapshotRefs[0].Kind != "Snapshot" {
		t.Fatalf("status.childrenSnapshotRefs not persisted: %#v", got.Status.ChildrenSnapshotRefs)
	}
}

// TestImportUpload_409UntilBound pins bind-first on the namespaced layer: an import CR without
// status.boundSnapshotContentName is refused with 409 ImportContentNotBound (the canonical string). No MCP
// is created pre-bind, but childRefs (a snapshot-layer attribute) are still recorded so a later bound
// retry needs no re-write.
func TestImportUpload_409UntilBound(t *testing.T) {
	ctx := context.Background()
	cl := uploadTestClient(t, importModeSnapshot(false))
	svc := NewImportUploadService(cl)

	child := UploadChildRef{APIVersion: storagev1alpha1.SchemeGroupVersion.String(), Kind: "Snapshot", Name: "child"}
	_, err := svc.Upload(ctx, coreSnapshotGVK(), testSnapNS, testSnapName, uploadPayload(t, child), false)
	assertAggReason(t, err, http.StatusConflict, ReasonImportContentNotBound)

	// No reconstructed MCP is created pre-bind (nothing is written to the content layer).
	cp := &ssv1alpha1.ManifestCheckpoint{}
	name := ReconstructedManifestCheckpointName(types.UID(testSnapUID), "")
	if err := cl.Get(ctx, types.NamespacedName{Name: name}, cp); !apierrors.IsNotFound(err) {
		t.Fatalf("no MCP must be created pre-bind, got err=%v", err)
	}
	// childRefs are recorded regardless of bind (idempotent, ordering matches the domain facade).
	got := &storagev1alpha1.Snapshot{}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: testSnapNS, Name: testSnapName}, got); err != nil {
		t.Fatalf("get snapshot: %v", err)
	}
	if len(got.Status.ChildrenSnapshotRefs) != 1 || got.Status.ChildrenSnapshotRefs[0].Name != "child" {
		t.Fatalf("childRefs must be written even before bind: %#v", got.Status.ChildrenSnapshotRefs)
	}
}

// TestImportUpload_BackRefMismatch pins the anti-spoofing back-ref on the namespaced upload path: a CR
// bound to a content whose spec.snapshotRef points at a DIFFERENT snapshot is refused with 403, before any
// manifests are forwarded to the content layer.
func TestImportUpload_BackRefMismatch(t *testing.T) {
	ctx := context.Background()
	content := importContent()
	content.Spec.SnapshotRef.Name = "other-snap" // back-ref aims at a different Snapshot
	cl := uploadTestClient(t, importModeSnapshot(true), content)
	svc := NewImportUploadService(cl)

	_, err := svc.Upload(ctx, coreSnapshotGVK(), testSnapNS, testSnapName, uploadPayload(t), false)
	assertAggStatus(t, err, http.StatusForbidden)

	cp := &ssv1alpha1.ManifestCheckpoint{}
	name := ReconstructedManifestCheckpointName(types.UID(testSnapUID), "")
	if err := cl.Get(ctx, types.NamespacedName{Name: name}, cp); !apierrors.IsNotFound(err) {
		t.Fatalf("no MCP must be created on a back-ref mismatch, got err=%v", err)
	}
}

// TestImportUpload_BoundContentMissing pins that a CR bound to a non-existent content is a 404 (the bind
// name is stale), never ImportContentNotBound (the CR IS bound).
func TestImportUpload_BoundContentMissing(t *testing.T) {
	ctx := context.Background()
	cl := uploadTestClient(t, importModeSnapshot(true)) // bound name set, but no content object exists
	svc := NewImportUploadService(cl)
	_, err := svc.Upload(ctx, coreSnapshotGVK(), testSnapNS, testSnapName, uploadPayload(t), false)
	assertAggStatus(t, err, http.StatusNotFound)
}

// TestImportUpload_Idempotent pins that a repeat bound upload returns the same checkpoint name and does NOT
// overwrite an already-Ready MCP (idempotency at the content layer).
func TestImportUpload_Idempotent(t *testing.T) {
	ctx := context.Background()
	cl := uploadTestClient(t, importModeSnapshot(true), importContent())
	svc := NewImportUploadService(cl)

	body := uploadPayload(t)
	first, err := svc.Upload(ctx, coreSnapshotGVK(), testSnapNS, testSnapName, body, false)
	if err != nil {
		t.Fatalf("first upload: %v", err)
	}
	// Mutate the live MCP's TotalObjects so a non-idempotent second upload would be detectable.
	cp := &ssv1alpha1.ManifestCheckpoint{}
	if err := cl.Get(ctx, types.NamespacedName{Name: first}, cp); err != nil {
		t.Fatalf("get MCP: %v", err)
	}
	sentinel := int64(4242)
	cp.Status.TotalObjects = int(sentinel)
	if err := cl.Status().Update(ctx, cp); err != nil {
		t.Fatalf("mutate MCP: %v", err)
	}

	second, err := svc.Upload(ctx, coreSnapshotGVK(), testSnapNS, testSnapName, body, false)
	if err != nil {
		t.Fatalf("second upload: %v", err)
	}
	if first != second {
		t.Fatalf("upload not idempotent: %q != %q", first, second)
	}
	if err := cl.Get(ctx, types.NamespacedName{Name: first}, cp); err != nil {
		t.Fatalf("get MCP after re-upload: %v", err)
	}
	if cp.Status.TotalObjects != int(sentinel) {
		t.Fatalf("a Ready MCP must not be overwritten on re-upload, TotalObjects = %d, want %d", cp.Status.TotalObjects, sentinel)
	}
}

func TestImportUpload_RejectsNonImportMode(t *testing.T) {
	ctx := context.Background()
	// A Snapshot with no spec.mode is a live-capture snapshot — upload must be refused before the bind gate.
	snap := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Name: testSnapName, Namespace: testSnapNS, UID: "u"}}
	cl := uploadTestClient(t, snap)
	svc := NewImportUploadService(cl)

	_, err := svc.Upload(ctx, coreSnapshotGVK(), testSnapNS, testSnapName, uploadPayload(t), false)
	assertAggStatus(t, err, http.StatusConflict)
}

func TestImportUpload_RejectsBadPayload(t *testing.T) {
	ctx := context.Background()
	cl := uploadTestClient(t, importModeSnapshot(true), importContent())
	svc := NewImportUploadService(cl)
	gvk := coreSnapshotGVK()

	// manifests missing.
	bad, _ := json.Marshal(ManifestsAndChildrenUpload{})
	_, err := svc.Upload(ctx, gvk, testSnapNS, testSnapName, bad, false)
	assertAggStatus(t, err, http.StatusBadRequest)

	// manifests not an array.
	notArray, _ := json.Marshal(map[string]interface{}{"manifests": map[string]string{"not": "array"}})
	_, err = svc.Upload(ctx, gvk, testSnapNS, testSnapName, notArray, false)
	assertAggStatus(t, err, http.StatusBadRequest)

	// childRef missing name.
	manifests, _ := json.Marshal([]map[string]interface{}{{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]interface{}{"name": "x", "namespace": testSnapNS}}})
	badChild, _ := json.Marshal(ManifestsAndChildrenUpload{Manifests: manifests, ChildRefs: []UploadChildRef{{APIVersion: "v1", Kind: "ConfigMap"}}})
	_, err = svc.Upload(ctx, gvk, testSnapNS, testSnapName, badChild, false)
	assertAggStatus(t, err, http.StatusBadRequest)
}

// TestImportUpload_LeafRejectsChildRefs pins the leaf validation: a leaf connector (the CSI VolumeSnapshot
// connector, leaf=true) must reject a non-empty childRefs payload (400).
func TestImportUpload_LeafRejectsChildRefs(t *testing.T) {
	ctx := context.Background()
	cl := uploadTestClient(t, importModeSnapshot(true), importContent())
	svc := NewImportUploadService(cl)

	child := UploadChildRef{APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "c"}
	_, err := svc.Upload(ctx, coreSnapshotGVK(), testSnapNS, testSnapName, uploadPayload(t, child), true)
	assertAggStatus(t, err, http.StatusBadRequest)
}

func TestImportUpload_NotFound(t *testing.T) {
	ctx := context.Background()
	cl := uploadTestClient(t)
	svc := NewImportUploadService(cl)
	_, err := svc.Upload(ctx, coreSnapshotGVK(), testSnapNS, "missing", uploadPayload(t), false)
	assertAggStatus(t, err, http.StatusNotFound)
}

// TestUploadToContent_HappyPath pins the content-addressed layer in isolation: given a SnapshotContent it
// reconstructs the MCP owned by that content, keyed by spec.snapshotRef.uid (so the name matches the
// aggregator's ReconstructedManifestCheckpointName(owner.UID) projection).
func TestUploadToContent_HappyPath(t *testing.T) {
	ctx := context.Background()
	content := importContent()
	cl := uploadTestClient(t, content)
	svc := NewImportUploadService(cl)

	manifests, _ := json.Marshal([]map[string]interface{}{{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]interface{}{"name": "cm", "namespace": testSnapNS}}})
	cpName, err := svc.UploadToContent(ctx, content, manifests)
	if err != nil {
		t.Fatalf("UploadToContent: %v", err)
	}
	if want := ReconstructedManifestCheckpointName(types.UID(testSnapUID), ""); cpName != want {
		t.Fatalf("checkpoint name = %q, want %q", cpName, want)
	}
	cp := &ssv1alpha1.ManifestCheckpoint{}
	if err := cl.Get(ctx, types.NamespacedName{Name: cpName}, cp); err != nil {
		t.Fatalf("get MCP: %v", err)
	}
	if ctrlRef := metav1.GetControllerOf(cp); ctrlRef == nil || ctrlRef.Kind != "SnapshotContent" || ctrlRef.Name != testContentName {
		t.Fatalf("MCP must be owned by the SnapshotContent, got %#v", cp.OwnerReferences)
	}
}

// TestUploadToContent_MissingSnapshotRefUID pins that the content layer refuses a content that has no
// spec.snapshotRef.uid to key the reconstructed checkpoint name.
func TestUploadToContent_MissingSnapshotRefUID(t *testing.T) {
	ctx := context.Background()
	content := importContent()
	content.Spec.SnapshotRef.UID = ""
	cl := uploadTestClient(t, content)
	svc := NewImportUploadService(cl)

	manifests, _ := json.Marshal([]map[string]interface{}{{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]interface{}{"name": "cm", "namespace": testSnapNS}}})
	_, err := svc.UploadToContent(ctx, content, manifests)
	assertAggStatus(t, err, http.StatusConflict)
}

// TestImportUpload_LeafSkipsChildrenStatusWriteUnderConflict pins the phase-5 regression: a leaf upload
// (no child refs) must not write status.childrenSnapshotRefs, so it never races whoever owns the CR's
// status. The concrete failure was the import VolumeSnapshot leaf — its forked status schema has no
// childrenSnapshotRefs field and the state-snapshotter binder reconciles its status continuously, so a
// blind Status().Update lost the conflict race for the whole retry budget and the upload returned 409.
// A non-leaf upload still needs the write, so the conflict must surface (no silent loss of child edges).
func TestImportUpload_LeafSkipsChildrenStatusWriteUnderConflict(t *testing.T) {
	ctx := context.Background()
	scheme := aggManifestTestScheme(t)
	// Fail every Snapshot status update (the competing status owner always wins). The reconstructed
	// ManifestCheckpoint status update is typed (*ManifestCheckpoint) and passes through untouched.
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&ssv1alpha1.ManifestCheckpoint{}, &storagev1alpha1.Snapshot{}).
		WithObjects(importModeSnapshot(true), importContent()).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if u, ok := obj.(*unstructured.Unstructured); ok && u.GetKind() == "Snapshot" {
					return apierrors.NewConflict(
						schema.GroupResource{Group: "state-snapshotter.deckhouse.io", Resource: "snapshots"},
						u.GetName(), fmt.Errorf("status writer race"))
				}
				return c.SubResource(subResourceName).Update(ctx, obj, opts...)
			},
		}).
		Build()
	svc := NewImportUploadService(cl)
	gvk := coreSnapshotGVK()

	// Leaf (empty childRefs): no status write, so the status-owner conflict never fires and the bound
	// upload succeeds through both layers.
	if _, err := svc.Upload(ctx, gvk, testSnapNS, testSnapName, uploadPayload(t), false); err != nil {
		t.Fatalf("leaf upload must succeed despite a status-write conflict: %v", err)
	}

	// Non-leaf (childRefs present): the status write is required, so the conflict surfaces as 409.
	child := UploadChildRef{APIVersion: storagev1alpha1.SchemeGroupVersion.String(), Kind: "Snapshot", Name: "child"}
	_, err := svc.Upload(ctx, gvk, testSnapNS, testSnapName, uploadPayload(t, child), false)
	assertAggStatus(t, err, http.StatusConflict)
}
