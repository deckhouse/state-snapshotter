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
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
)

func newReconstructClient(t *testing.T) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := ssv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&ssv1alpha1.ManifestCheckpoint{}).
		Build()
}

func sampleManifests(t *testing.T) []byte {
	t.Helper()
	objs := []map[string]interface{}{
		{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]interface{}{"name": "a", "namespace": "ns1"}},
		{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]interface{}{"name": "b", "namespace": "ns1"}},
	}
	raw, err := json.Marshal(objs)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func TestReconstructManifestCheckpoint_BuildsReadyCheckpoint(t *testing.T) {
	ctx := context.Background()
	cl := newReconstructClient(t)
	name := ReconstructedManifestCheckpointName(types.UID("import-uid"), "Snapshot--ns1--snap")
	ownerRefs := []metav1.OwnerReference{{APIVersion: "state-snapshotter.deckhouse.io/v1alpha1", Kind: "Snapshot", Name: "imp", UID: "import-uid"}}

	if err := ReconstructManifestCheckpoint(ctx, cl, name, ownerRefs, sampleManifests(t)); err != nil {
		t.Fatalf("reconstruct: %v", err)
	}

	cp := &ssv1alpha1.ManifestCheckpoint{}
	if err := cl.Get(ctx, types.NamespacedName{Name: name}, cp); err != nil {
		t.Fatalf("get checkpoint: %v", err)
	}
	if !meta.IsStatusConditionTrue(cp.Status.Conditions, ssv1alpha1.ManifestCheckpointConditionTypeReady) {
		t.Fatalf("checkpoint must be Ready, conditions=%v", cp.Status.Conditions)
	}
	if cp.Status.TotalObjects != 2 {
		t.Fatalf("expected 2 objects, got %d", cp.Status.TotalObjects)
	}
	if len(cp.Status.Chunks) == 0 {
		t.Fatal("expected at least one chunk in status")
	}
	if cp.OwnerReferences[0].Kind != "Snapshot" {
		t.Fatalf("expected Snapshot owner, got %v", cp.OwnerReferences)
	}
	if !strings.HasPrefix(name, namespacemanifest.CheckpointNamePrefix) {
		t.Fatalf("checkpoint name %q must use the capture prefix %q", name, namespacemanifest.CheckpointNamePrefix)
	}

	// Chunk names are recorded in status and read back from there (unified wave4C scheme keys them by the
	// checkpoint UID, not the name), so resolve chunk 0 via the recorded ChunkInfo rather than derivation.
	chunk := &ssv1alpha1.ManifestCheckpointContentChunk{}
	if err := cl.Get(ctx, types.NamespacedName{Name: cp.Status.Chunks[0].Name}, chunk); err != nil {
		t.Fatalf("get chunk 0: %v", err)
	}
	if chunk.Spec.CheckpointName != name {
		t.Fatalf("chunk must reference checkpoint %q, got %q", name, chunk.Spec.CheckpointName)
	}
	if chunk.OwnerReferences[0].Kind != "ManifestCheckpoint" {
		t.Fatalf("chunk must be owned by the checkpoint, got %v", chunk.OwnerReferences)
	}
}

func TestReconstructManifestCheckpoint_Idempotent(t *testing.T) {
	ctx := context.Background()
	cl := newReconstructClient(t)
	name := ReconstructedManifestCheckpointName(types.UID("import-uid"), "Snapshot--ns1--snap")
	raw := sampleManifests(t)

	if err := ReconstructManifestCheckpoint(ctx, cl, name, nil, raw); err != nil {
		t.Fatalf("first reconstruct: %v", err)
	}
	// A second call on an already-Ready checkpoint is a no-op and must not error.
	if err := ReconstructManifestCheckpoint(ctx, cl, name, nil, raw); err != nil {
		t.Fatalf("second reconstruct: %v", err)
	}
	cp := &ssv1alpha1.ManifestCheckpoint{}
	if err := cl.Get(ctx, types.NamespacedName{Name: name}, cp); err != nil {
		t.Fatalf("get checkpoint: %v", err)
	}
	if cp.Status.TotalObjects != 2 {
		t.Fatalf("expected 2 objects after idempotent re-run, got %d", cp.Status.TotalObjects)
	}
}

// TestReconstructManifestCheckpoint_AdoptsReadyUnanchoredCheckpoint pins the §10.1 upgrade-safety path: a
// checkpoint that is already Ready but UNANCHORED (pre-§10.1 ownerless scheme, or a repeat upload before the
// aggregator handoff) is adopted onto the passed import ObjectKeeper and its reconstructed label backfilled,
// so it becomes GC-safe instead of leaving a stray keeper that owns nothing.
func TestReconstructManifestCheckpoint_AdoptsReadyUnanchoredCheckpoint(t *testing.T) {
	ctx := context.Background()
	cl := newReconstructClient(t)
	name := ReconstructedManifestCheckpointName(types.UID("import-uid"), "unanchored")

	// Seed a Ready checkpoint, then strip its reconstructed label to emulate the pre-§10.1 ownerless scheme.
	if err := ReconstructManifestCheckpoint(ctx, cl, name, nil, sampleManifests(t)); err != nil {
		t.Fatalf("seed reconstruct: %v", err)
	}
	cp := &ssv1alpha1.ManifestCheckpoint{}
	if err := cl.Get(ctx, types.NamespacedName{Name: name}, cp); err != nil {
		t.Fatalf("get: %v", err)
	}
	base := cp.DeepCopy()
	delete(cp.Labels, ReconstructedManifestCheckpointLabelKey)
	if err := cl.Patch(ctx, cp, client.MergeFrom(base)); err != nil {
		t.Fatalf("strip label: %v", err)
	}

	controllerTrue := true
	okRef := metav1.OwnerReference{APIVersion: "deckhouse.io/v1alpha1", Kind: "ObjectKeeper", Name: "nss-import-ok-x", UID: "ok-uid", Controller: &controllerTrue}
	if err := ReconstructManifestCheckpoint(ctx, cl, name, []metav1.OwnerReference{okRef}, sampleManifests(t)); err != nil {
		t.Fatalf("adopt reconstruct: %v", err)
	}

	if err := cl.Get(ctx, types.NamespacedName{Name: name}, cp); err != nil {
		t.Fatalf("get after adopt: %v", err)
	}
	if ctrlRef := metav1.GetControllerOf(cp); ctrlRef == nil || ctrlRef.Name != "nss-import-ok-x" {
		t.Fatalf("a Ready unanchored checkpoint must be adopted onto the import ObjectKeeper, got %#v", cp.OwnerReferences)
	}
	if cp.Labels[ReconstructedManifestCheckpointLabelKey] != reconstructedManifestCheckpointLabelValue {
		t.Fatalf("adoption must backfill the reconstructed label, got %v", cp.Labels)
	}
}

// TestReconstructManifestCheckpoint_DoesNotReanchorOwnedCheckpoint is the negative counterpart: a Ready
// checkpoint that already has a controller owner (here the SnapshotContent, i.e. the aggregator handoff
// already ran) must NOT be re-anchored onto the import ObjectKeeper — that would either add a second
// controller ref or resurrect a keeper the handoff was meant to retire.
func TestReconstructManifestCheckpoint_DoesNotReanchorOwnedCheckpoint(t *testing.T) {
	ctx := context.Background()
	cl := newReconstructClient(t)
	name := ReconstructedManifestCheckpointName(types.UID("import-uid"), "owned")
	controllerTrue := true
	contentRef := metav1.OwnerReference{APIVersion: "state-snapshotter.deckhouse.io/v1alpha1", Kind: "SnapshotContent", Name: "content", UID: "content-uid", Controller: &controllerTrue}
	if err := ReconstructManifestCheckpoint(ctx, cl, name, []metav1.OwnerReference{contentRef}, sampleManifests(t)); err != nil {
		t.Fatalf("seed reconstruct: %v", err)
	}

	okRef := metav1.OwnerReference{APIVersion: "deckhouse.io/v1alpha1", Kind: "ObjectKeeper", Name: "nss-import-ok-y", UID: "ok-uid", Controller: &controllerTrue}
	if err := ReconstructManifestCheckpoint(ctx, cl, name, []metav1.OwnerReference{okRef}, sampleManifests(t)); err != nil {
		t.Fatalf("re-run: %v", err)
	}

	cp := &ssv1alpha1.ManifestCheckpoint{}
	if err := cl.Get(ctx, types.NamespacedName{Name: name}, cp); err != nil {
		t.Fatalf("get: %v", err)
	}
	if ctrlRef := metav1.GetControllerOf(cp); ctrlRef == nil || ctrlRef.Kind != "SnapshotContent" {
		t.Fatalf("a handed-off checkpoint must keep its SnapshotContent controller owner, got %#v", cp.OwnerReferences)
	}
	for _, ref := range cp.OwnerReferences {
		if ref.Kind == "ObjectKeeper" {
			t.Fatalf("must not re-anchor a content-owned checkpoint onto the import ObjectKeeper, got %#v", cp.OwnerReferences)
		}
	}
}

// TestReconstructManifestCheckpoint_AnchorsNotReadyResumedCheckpoint pins the resume-path §10.1 anchoring:
// a pre-existing but NOT-yet-Ready ownerless checkpoint (pre-§10.1 partial create) is anchored onto the
// passed import ObjectKeeper as it is finished (chunks + Ready status), so it never reaches Ready without a
// GC backstop.
func TestReconstructManifestCheckpoint_AnchorsNotReadyResumedCheckpoint(t *testing.T) {
	ctx := context.Background()
	cl := newReconstructClient(t)
	name := ReconstructedManifestCheckpointName(types.UID("import-uid"), "resume")

	pre := &ssv1alpha1.ManifestCheckpoint{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := cl.Create(ctx, pre); err != nil {
		t.Fatalf("seed not-ready mcp: %v", err)
	}

	controllerTrue := true
	okRef := metav1.OwnerReference{APIVersion: "deckhouse.io/v1alpha1", Kind: "ObjectKeeper", Name: "nss-import-ok-z", UID: "ok-uid", Controller: &controllerTrue}
	if err := ReconstructManifestCheckpoint(ctx, cl, name, []metav1.OwnerReference{okRef}, sampleManifests(t)); err != nil {
		t.Fatalf("resume reconstruct: %v", err)
	}

	cp := &ssv1alpha1.ManifestCheckpoint{}
	if err := cl.Get(ctx, types.NamespacedName{Name: name}, cp); err != nil {
		t.Fatalf("get: %v", err)
	}
	if ctrlRef := metav1.GetControllerOf(cp); ctrlRef == nil || ctrlRef.Name != "nss-import-ok-z" {
		t.Fatalf("a resumed not-Ready checkpoint must be anchored onto the import ObjectKeeper, got %#v", cp.OwnerReferences)
	}
	if !meta.IsStatusConditionTrue(cp.Status.Conditions, ssv1alpha1.ManifestCheckpointConditionTypeReady) {
		t.Fatalf("resumed checkpoint must finish Ready, conditions=%v", cp.Status.Conditions)
	}
	if cp.Labels[ReconstructedManifestCheckpointLabelKey] != reconstructedManifestCheckpointLabelValue {
		t.Fatalf("resumed checkpoint must carry the reconstructed label, got %v", cp.Labels)
	}
}

func TestReconstructManifestCheckpoint_EmptyObjectsSingleChunk(t *testing.T) {
	ctx := context.Background()
	cl := newReconstructClient(t)
	name := ReconstructedManifestCheckpointName(types.UID("import-uid"), "Snapshot--ns1--empty")

	if err := ReconstructManifestCheckpoint(ctx, cl, name, nil, []byte("[]")); err != nil {
		t.Fatalf("reconstruct empty: %v", err)
	}
	cp := &ssv1alpha1.ManifestCheckpoint{}
	if err := cl.Get(ctx, types.NamespacedName{Name: name}, cp); err != nil {
		t.Fatalf("get checkpoint: %v", err)
	}
	if len(cp.Status.Chunks) != 1 {
		t.Fatalf("empty manifests must still yield a single chunk, got %d", len(cp.Status.Chunks))
	}
	if cp.Status.TotalObjects != 0 {
		t.Fatalf("expected 0 objects, got %d", cp.Status.TotalObjects)
	}
}

func TestReconstructManifestCheckpoint_RejectsNonArray(t *testing.T) {
	ctx := context.Background()
	cl := newReconstructClient(t)
	name := ReconstructedManifestCheckpointName(types.UID("import-uid"), "Snapshot--ns1--bad")
	if err := ReconstructManifestCheckpoint(ctx, cl, name, nil, []byte(`{"not":"an array"}`)); err == nil {
		t.Fatal("expected error for non-array manifests")
	}
}

func TestCollectReconstructedManifestObjects_RoundTrip(t *testing.T) {
	ctx := context.Background()
	cl := newReconstructClient(t)
	name := ReconstructedManifestCheckpointName(types.UID("import-uid"), "Snapshot--ns1--rt")

	// A reconstructed orphan-PVC leaf checkpoint carries the PVC manifest (uid preserved) the import
	// binder reads back to target the dataRef at the PVC.
	objs := []map[string]interface{}{
		{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": map[string]interface{}{"name": "bk-pvc", "namespace": "ns1", "uid": "pvc-uid-123"}},
	}
	raw, err := json.Marshal(objs)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := ReconstructManifestCheckpoint(ctx, cl, name, nil, raw); err != nil {
		t.Fatalf("reconstruct: %v", err)
	}

	cp := &ssv1alpha1.ManifestCheckpoint{}
	if err := cl.Get(ctx, types.NamespacedName{Name: name}, cp); err != nil {
		t.Fatalf("get checkpoint: %v", err)
	}

	got, err := CollectReconstructedManifestObjects(ctx, cl, cp)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 object, got %d", len(got))
	}
	if got[0].GetKind() != "PersistentVolumeClaim" || got[0].GetName() != "bk-pvc" {
		t.Fatalf("unexpected object: kind=%s name=%s", got[0].GetKind(), got[0].GetName())
	}
	if got[0].GetUID() != "pvc-uid-123" {
		t.Fatalf("PVC uid must be preserved for dataRef matching, got %q", got[0].GetUID())
	}
	if got[0].GetNamespace() != "ns1" {
		t.Fatalf("PVC namespace must be preserved, got %q", got[0].GetNamespace())
	}
}

func TestReconstructedNameStableAndUnique(t *testing.T) {
	a := ReconstructedManifestCheckpointName(types.UID("uid-1"), "node-a")
	if a != ReconstructedManifestCheckpointName(types.UID("uid-1"), "node-a") {
		t.Fatal("name must be stable across calls")
	}
	if a == ReconstructedManifestCheckpointName(types.UID("uid-1"), "node-b") {
		t.Fatal("different nodes must produce different names")
	}
	if a == ReconstructedManifestCheckpointName(types.UID("uid-2"), "node-a") {
		t.Fatal("different imports must produce different names")
	}
}
