package controllers

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
)

func manifestCaptureRequestSafeToDelete(
	ctx context.Context,
	reader client.Reader,
	key client.ObjectKey,
	contentName string,
) (bool, error) {
	mcr := &ssv1alpha1.ManifestCaptureRequest{}
	if err := reader.Get(ctx, key, mcr); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	if contentName == "" {
		return false, nil
	}
	content := &storagev1alpha1.SnapshotContent{}
	if err := reader.Get(ctx, client.ObjectKey{Name: contentName}, content); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if content.Status.ManifestCheckpointName == "" {
		return false, nil
	}
	mcp := &ssv1alpha1.ManifestCheckpoint{}
	if err := reader.Get(ctx, client.ObjectKey{Name: content.Status.ManifestCheckpointName}, mcp); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return isMCRSafeToDelete(mcr, content, mcp), nil
}

// isMCRSafeToDelete encodes the handoff invariant:
// SnapshotContent.status.manifestCheckpointName must point at an existing Ready MCP,
// and that MCP must already be owned by the same SnapshotContent.
func isMCRSafeToDelete(
	mcr *ssv1alpha1.ManifestCaptureRequest,
	content *storagev1alpha1.SnapshotContent,
	mcp *ssv1alpha1.ManifestCheckpoint,
) bool {
	if mcr == nil || content == nil || mcp == nil {
		return false
	}
	if content.Status.ManifestCheckpointName == "" || content.Status.ManifestCheckpointName != mcp.Name {
		return false
	}
	if mcr.Status.CheckpointName == "" || mcr.Status.CheckpointName != mcp.Name {
		return false
	}
	ready := meta.FindStatusCondition(mcp.Status.Conditions, ssv1alpha1.ManifestCheckpointConditionTypeReady)
	if ready == nil || ready.Status != metav1.ConditionTrue {
		return false
	}
	return manifestCheckpointOwnedBySnapshotContentName(mcp, content.Name, content.UID)
}

func manifestCheckpointOwnedBySnapshotContentName(mcp *ssv1alpha1.ManifestCheckpoint, contentName string, contentUID types.UID) bool {
	for _, ref := range mcp.OwnerReferences {
		if ref.APIVersion != storagev1alpha1.SchemeGroupVersion.String() || ref.Kind != "SnapshotContent" || ref.Name != contentName {
			continue
		}
		return contentUID == "" || ref.UID == "" || ref.UID == contentUID
	}
	return false
}
