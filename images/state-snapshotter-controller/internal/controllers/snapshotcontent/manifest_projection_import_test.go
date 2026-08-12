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

package snapshotcontent

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

func importManifestProjScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}
	if err := ssv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add snapshotter scheme: %v", err)
	}
	return scheme
}

// importOwnerUnstructured builds an import-mode owner snapshot (spec.mode: Import) with the given UID, whose
// SnapshotContent's manifest leg the aggregator projects from the reconstructed ManifestCheckpoint.
func importOwnerUnstructured(uid string) *unstructured.Unstructured {
	owner := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": storagev1alpha1.SchemeGroupVersion.String(),
		"kind":       "Snapshot",
		"metadata":   map[string]interface{}{"namespace": projTestNS, "name": "imp-snap", "uid": uid},
		"spec":       map[string]interface{}{"mode": string(storagev1alpha1.SnapshotModeImport)},
	}}
	return owner
}

// TestReconcileManifestCheckpointNameProjection_ImportPublishesReconstructed pins content-single-writer §10:
// the aggregator (not the import controllers) is the single writer of status.manifestCheckpointName on the
// import path, projecting the deterministic reconstructed checkpoint name once the upload endpoint has
// created it.
func TestReconcileManifestCheckpointNameProjection_ImportPublishesReconstructed(t *testing.T) {
	ctx := context.Background()
	scheme := importManifestProjScheme(t)

	const ownerUID = "imp-uid"
	mcpName := usecase.ReconstructedManifestCheckpointName(types.UID(ownerUID), "")
	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: projTestContent}}
	mcp := &ssv1alpha1.ManifestCheckpoint{ObjectMeta: metav1.ObjectMeta{Name: mcpName}}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&storagev1alpha1.SnapshotContent{}).
		WithObjects(content, mcp).
		Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	requeue, err := r.reconcileManifestCheckpointNameProjection(ctx, projContentObj(), importOwnerUnstructured(ownerUID), projTestNS, true)
	if err != nil {
		t.Fatalf("projection: %v", err)
	}
	if requeue {
		t.Fatalf("a published import manifest leg must not requeue")
	}

	got := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: projTestContent}, got); err != nil {
		t.Fatalf("get content: %v", err)
	}
	if got.Status.ManifestCheckpointName != mcpName {
		t.Fatalf("aggregator must publish the reconstructed MCP name, got %q want %q", got.Status.ManifestCheckpointName, mcpName)
	}
}

// TestReconcileManifestCheckpointNameProjection_ImportPendingWhenNoMCP: before the upload endpoint creates
// the reconstructed checkpoint there is nothing to publish — the projection requeues and writes nothing.
func TestReconcileManifestCheckpointNameProjection_ImportPendingWhenNoMCP(t *testing.T) {
	ctx := context.Background()
	scheme := importManifestProjScheme(t)

	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: projTestContent}}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&storagev1alpha1.SnapshotContent{}).
		WithObjects(content).
		Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	requeue, err := r.reconcileManifestCheckpointNameProjection(ctx, projContentObj(), importOwnerUnstructured("imp-uid"), projTestNS, true)
	if err != nil {
		t.Fatalf("projection: %v", err)
	}
	if !requeue {
		t.Fatalf("a missing reconstructed checkpoint must requeue (pending)")
	}
	got := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: projTestContent}, got); err != nil {
		t.Fatalf("get content: %v", err)
	}
	if got.Status.ManifestCheckpointName != "" {
		t.Fatalf("nothing must be published before the checkpoint exists, got %q", got.Status.ManifestCheckpointName)
	}
}
