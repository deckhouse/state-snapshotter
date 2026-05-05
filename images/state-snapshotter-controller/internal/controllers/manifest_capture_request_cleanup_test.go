package controllers

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
)

func TestIsMCRSafeToDeleteRequiresContentLinkedReadyOwnedMCP(t *testing.T) {
	contentUID := types.UID("content-uid")
	baseMCR := &ssv1alpha1.ManifestCaptureRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "mcr", Namespace: "ns"},
		Status:     ssv1alpha1.ManifestCaptureRequestStatus{CheckpointName: "mcp"},
	}
	baseContent := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "content", UID: contentUID},
		Status:     storagev1alpha1.SnapshotContentStatus{ManifestCheckpointName: "mcp"},
	}
	readyOwnedMCP := &ssv1alpha1.ManifestCheckpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name: "mcp",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
				Kind:       "SnapshotContent",
				Name:       "content",
				UID:        contentUID,
			}},
		},
		Status: ssv1alpha1.ManifestCheckpointStatus{Conditions: []metav1.Condition{{
			Type:   ssv1alpha1.ManifestCheckpointConditionTypeReady,
			Status: metav1.ConditionTrue,
			Reason: ssv1alpha1.ManifestCheckpointConditionReasonCompleted,
		}}},
	}

	tests := []struct {
		name   string
		mutate func(*ssv1alpha1.ManifestCaptureRequest, *storagev1alpha1.SnapshotContent, *ssv1alpha1.ManifestCheckpoint)
		want   bool
	}{
		{
			name: "not safe when MCP exists but is not Ready",
			mutate: func(_ *ssv1alpha1.ManifestCaptureRequest, _ *storagev1alpha1.SnapshotContent, mcp *ssv1alpha1.ManifestCheckpoint) {
				mcp.Status.Conditions[0].Status = metav1.ConditionFalse
			},
		},
		{
			name: "not safe when MCP Ready but ownerRef is not SnapshotContent",
			mutate: func(_ *ssv1alpha1.ManifestCaptureRequest, _ *storagev1alpha1.SnapshotContent, mcp *ssv1alpha1.ManifestCheckpoint) {
				mcp.OwnerReferences = nil
			},
		},
		{
			name: "not safe when MCP Ready but ownerRef is another SnapshotContent",
			mutate: func(_ *ssv1alpha1.ManifestCaptureRequest, _ *storagev1alpha1.SnapshotContent, mcp *ssv1alpha1.ManifestCheckpoint) {
				mcp.OwnerReferences[0].Name = "other-content"
			},
		},
		{
			name: "not safe when SnapshotContent has not published manifestCheckpointName",
			mutate: func(_ *ssv1alpha1.ManifestCaptureRequest, content *storagev1alpha1.SnapshotContent, _ *ssv1alpha1.ManifestCheckpoint) {
				content.Status.ManifestCheckpointName = ""
			},
		},
		{
			name: "safe when content links Ready MCP owned by content",
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mcr := baseMCR.DeepCopy()
			content := baseContent.DeepCopy()
			mcp := readyOwnedMCP.DeepCopy()
			if tt.mutate != nil {
				tt.mutate(mcr, content, mcp)
			}
			if got := isMCRSafeToDelete(mcr, content, mcp); got != tt.want {
				t.Fatalf("isMCRSafeToDelete() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestManifestCaptureRequestSafeToDeleteReadsExplicitArtifactChain(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}
	if err := ssv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add snapshotter scheme: %v", err)
	}
	content := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "content", UID: "content-uid"},
		Status:     storagev1alpha1.SnapshotContentStatus{ManifestCheckpointName: "mcp"},
	}
	mcr := &ssv1alpha1.ManifestCaptureRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "mcr", Namespace: "ns"},
		Status:     ssv1alpha1.ManifestCaptureRequestStatus{CheckpointName: "mcp"},
	}
	mcp := &ssv1alpha1.ManifestCheckpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name: "mcp",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
				Kind:       "SnapshotContent",
				Name:       "content",
				UID:        "content-uid",
			}},
		},
		Status: ssv1alpha1.ManifestCheckpointStatus{Conditions: []metav1.Condition{{
			Type:   ssv1alpha1.ManifestCheckpointConditionTypeReady,
			Status: metav1.ConditionTrue,
		}}},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(content, mcr, mcp).Build()

	got, err := manifestCaptureRequestSafeToDelete(ctx, cl, client.ObjectKey{Namespace: "ns", Name: "mcr"}, "content")
	if err != nil {
		t.Fatalf("manifestCaptureRequestSafeToDelete() error: %v", err)
	}
	if !got {
		t.Fatal("expected MCR to be safe to delete after explicit MCP handoff")
	}
}
