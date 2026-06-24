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
	captureRef := &ssv1alpha1.ObjectReference{Name: "imp", Namespace: "ns1", UID: "import-uid"}
	ownerRefs := []metav1.OwnerReference{{APIVersion: "storage.deckhouse.io/v1alpha1", Kind: "Snapshot", Name: "imp", UID: "import-uid"}}

	if err := ReconstructManifestCheckpoint(ctx, cl, name, "ns1", captureRef, ownerRefs, sampleManifests(t)); err != nil {
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

	// Chunk naming must follow <prefix><id>-<n> so the ArchiveService (prefix-stripped id) can find it.
	id := strings.TrimPrefix(name, namespacemanifest.CheckpointNamePrefix)
	chunk := &ssv1alpha1.ManifestCheckpointContentChunk{}
	if err := cl.Get(ctx, types.NamespacedName{Name: namespacemanifest.CheckpointNamePrefix + id + "-0"}, chunk); err != nil {
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

	if err := ReconstructManifestCheckpoint(ctx, cl, name, "ns1", nil, nil, raw); err != nil {
		t.Fatalf("first reconstruct: %v", err)
	}
	// A second call on an already-Ready checkpoint is a no-op and must not error.
	if err := ReconstructManifestCheckpoint(ctx, cl, name, "ns1", nil, nil, raw); err != nil {
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

func TestReconstructManifestCheckpoint_EmptyObjectsSingleChunk(t *testing.T) {
	ctx := context.Background()
	cl := newReconstructClient(t)
	name := ReconstructedManifestCheckpointName(types.UID("import-uid"), "Snapshot--ns1--empty")

	if err := ReconstructManifestCheckpoint(ctx, cl, name, "ns1", nil, nil, []byte("[]")); err != nil {
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
	if err := ReconstructManifestCheckpoint(ctx, cl, name, "ns1", nil, nil, []byte(`{"not":"an array"}`)); err == nil {
		t.Fatal("expected error for non-array manifests")
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
