package manifestcapture

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

func ManifestCaptureRequestSafeToDelete(
	ctx context.Context,
	reader client.Reader,
	key client.ObjectKey,
	contentName string,
) (bool, error) {
	// Both the manifestCaptured marker AND the MCR delete are gated on the DURABLE manifest invariant (a
	// Ready ManifestCheckpoint published into and owned by the SnapshotContent) — NEVER on the MCR's mere
	// absence. An MCR that was deleted or TTL-reaped WITHOUT a successful handoff (e.g. a capture that
	// terminated Failed) must not be mistaken for a completed capture: stamping manifestCaptured=true then
	// makes the domain controller stop recreating the MCR while the content never receives a checkpoint,
	// wedging the snapshot in ManifestCapturePending forever. So a missing MCR is NOT automatically "safe":
	// it is only safe if the content already holds the durable checkpoint.
	var mcr *ssv1alpha1.ManifestCaptureRequest
	fetched := &ssv1alpha1.ManifestCaptureRequest{}
	switch err := reader.Get(ctx, key, fetched); {
	case err == nil:
		mcr = fetched
	case apierrors.IsNotFound(err):
		mcr = nil
	default:
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
	if mcr != nil {
		return isMCRSafeToDelete(mcr, content, mcp), nil
	}
	// MCR already gone: the mcr.Status.CheckpointName -> MCP link can no longer be re-verified, but a Ready
	// ManifestCheckpoint published into and owned by the content is sufficient proof the capture completed
	// durably before the MCR was cleaned up.
	return isManifestCheckpointDurable(content, mcp), nil
}

func ManifestCheckpointNameFromRequest(mcr *ssv1alpha1.ManifestCaptureRequest) string {
	if mcr == nil {
		return ""
	}
	if mcr.Status.CheckpointName != "" {
		return mcr.Status.CheckpointName
	}
	return ""
}

// isMCRSafeToDelete encodes the full handoff invariant: the SnapshotContent has durably taken over the
// manifest (see isManifestCheckpointDurable) AND the MCR references that same MCP.
func isMCRSafeToDelete(
	mcr *ssv1alpha1.ManifestCaptureRequest,
	content *storagev1alpha1.SnapshotContent,
	mcp *ssv1alpha1.ManifestCheckpoint,
) bool {
	if !isManifestCheckpointDurable(content, mcp) {
		return false
	}
	if mcr == nil {
		return false
	}
	return mcr.Status.CheckpointName != "" && mcr.Status.CheckpointName == mcp.Name
}

// isManifestCheckpointDurable reports whether the SnapshotContent has durably taken over the manifest:
// status.manifestCheckpointName must point at an existing Ready MCP that is owned by that same
// SnapshotContent. This is the MCR-independent half of the handoff invariant, so it also gates the
// manifestCaptured marker when the MCR is already gone.
func isManifestCheckpointDurable(
	content *storagev1alpha1.SnapshotContent,
	mcp *ssv1alpha1.ManifestCheckpoint,
) bool {
	if content == nil || mcp == nil {
		return false
	}
	if content.Status.ManifestCheckpointName == "" || content.Status.ManifestCheckpointName != mcp.Name {
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
