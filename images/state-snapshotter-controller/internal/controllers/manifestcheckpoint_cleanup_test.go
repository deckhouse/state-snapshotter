package controllers

import (
	"context"
	"testing"

	"github.com/deckhouse/state-snapshotter/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestCleanupArtifactsForMCR_DeletesOrphans(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(scheme)

	chunk := &v1alpha1.ManifestCheckpointContentChunk{
		ObjectMeta: metav1.ObjectMeta{Name: "chunk-1"},
		Spec:       v1alpha1.ManifestCheckpointContentChunkSpec{CheckpointName: "mcp-1"},
	}
	checkpoint := &v1alpha1.ManifestCheckpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "mcp-1"},
		Status: v1alpha1.ManifestCheckpointStatus{
			Chunks: []v1alpha1.ChunkInfo{{Name: "chunk-1"}},
		},
	}
	mcr := &v1alpha1.ManifestCaptureRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "mcr-1", Namespace: "default"},
		Status:     v1alpha1.ManifestCaptureRequestStatus{CheckpointName: "mcp-1"},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcr, checkpoint, chunk).Build()
	controller := &ManifestCheckpointController{
		Client:    client,
		APIReader: client,
		Scheme:    scheme,
	}

	if err := controller.cleanupArtifactsForMCR(context.Background(), mcr); err != nil {
		t.Fatalf("cleanupArtifactsForMCR failed: %v", err)
	}

	err := client.Get(context.Background(), ctrlclient.ObjectKey{Name: "mcp-1"}, &v1alpha1.ManifestCheckpoint{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected ManifestCheckpoint to be deleted, got: %v", err)
	}
	err = client.Get(context.Background(), ctrlclient.ObjectKey{Name: "chunk-1"}, &v1alpha1.ManifestCheckpointContentChunk{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected chunk to be deleted, got: %v", err)
	}
}

func TestCleanupArtifactsForMCR_SkipsManaged(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(scheme)

	ownerRefs := []metav1.OwnerReference{{
		APIVersion: "test.deckhouse.io/v1alpha1",
		Kind:       "TestSnapshotContent",
		Name:       "content-1",
		UID:        "uid-1",
	}}

	chunk := &v1alpha1.ManifestCheckpointContentChunk{
		ObjectMeta: metav1.ObjectMeta{Name: "chunk-2", OwnerReferences: ownerRefs},
		Spec:       v1alpha1.ManifestCheckpointContentChunkSpec{CheckpointName: "mcp-2"},
	}
	checkpoint := &v1alpha1.ManifestCheckpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "mcp-2", OwnerReferences: ownerRefs},
		Status: v1alpha1.ManifestCheckpointStatus{
			Chunks: []v1alpha1.ChunkInfo{{Name: "chunk-2"}},
		},
	}
	mcr := &v1alpha1.ManifestCaptureRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "mcr-2", Namespace: "default"},
		Status:     v1alpha1.ManifestCaptureRequestStatus{CheckpointName: "mcp-2"},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcr, checkpoint, chunk).Build()
	controller := &ManifestCheckpointController{
		Client:    client,
		APIReader: client,
		Scheme:    scheme,
	}

	if err := controller.cleanupArtifactsForMCR(context.Background(), mcr); err != nil {
		t.Fatalf("cleanupArtifactsForMCR failed: %v", err)
	}

	if err := client.Get(context.Background(), ctrlclient.ObjectKey{Name: "mcp-2"}, &v1alpha1.ManifestCheckpoint{}); err != nil {
		t.Fatalf("expected ManifestCheckpoint to remain: %v", err)
	}
	if err := client.Get(context.Background(), ctrlclient.ObjectKey{Name: "chunk-2"}, &v1alpha1.ManifestCheckpointContentChunk{}); err != nil {
		t.Fatalf("expected chunk to remain: %v", err)
	}
}
