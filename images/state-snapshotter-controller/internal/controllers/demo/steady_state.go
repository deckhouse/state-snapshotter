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

package demo

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// demoSnapshotContentManifestHandoffComplete reports whether manifest capture has been PUBLISHED and
// HANDED OFF for this SnapshotContent: status.manifestCheckpointName is set and the referenced
// ManifestCheckpoint exists and is owned (controller ownerRef) by the SnapshotContent.
//
// Intentionally NOT gated on current MCP Ready / content Ready (the previous behavior). Once the MCP
// is published and its ownership is handed off to the SnapshotContent, manifest capture is DONE for
// this content. A later MCP Ready=False/Failed (durable post-publish artifact degradation) must be
// reflected by status (SnapshotContentController -> SnapshotContent.RequestsReady=False/
// ManifestCheckpointFailed -> tree), NOT silently repaired by re-running capture. Gating handoff on
// MCP readiness made a failed published MCP look like "capture incomplete", which caused the demo
// reconciler to create a fresh MCR/MCP and mask the failure (re-capture). Ownership is the durable
// signal: SnapshotContentController hands the MCP to the SnapshotContent only after it first became
// Ready, so ownership==content means the artifact was published, regardless of its current Ready.
// Root E5 exclude reads SnapshotContent/MCP only (not live MCR).
func demoSnapshotContentManifestHandoffComplete(ctx context.Context, c client.Reader, contentName string) (bool, error) {
	content := &storagev1alpha1.SnapshotContent{}
	if err := c.Get(ctx, client.ObjectKey{Name: contentName}, content); err != nil {
		return false, err
	}
	mcpName := content.Status.ManifestCheckpointName
	if mcpName == "" {
		return false, nil
	}
	mcp := &ssv1alpha1.ManifestCheckpoint{}
	if err := c.Get(ctx, client.ObjectKey{Name: mcpName}, mcp); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return manifestCheckpointHandedOffToContent(mcp, contentName), nil
}

// manifestCheckpointHandedOffToContent is true when the ManifestCheckpoint carries the CONTROLLER
// ownerRef set by SnapshotContentController during handoff (kind SnapshotContent, matching name,
// Controller=true). A non-controller/decorative SnapshotContent ownerRef does NOT count as a durable
// handoff: only the controller ownerRef represents the transfer of artifact ownership to the content.
func manifestCheckpointHandedOffToContent(mcp *ssv1alpha1.ManifestCheckpoint, contentName string) bool {
	for _, ref := range mcp.OwnerReferences {
		if ref.APIVersion == storagev1alpha1.SchemeGroupVersion.String() &&
			ref.Kind == "SnapshotContent" && ref.Name == contentName &&
			ref.Controller != nil && *ref.Controller {
			return true
		}
	}
	return false
}

// demoMirrorOnlyIfHandoffComplete handles the post-publish state of a demo snapshot. Once manifest
// capture is published and MCP ownership is handed off to the SnapshotContent, the demo snapshot is
// mirror-only: it MUST NOT re-run manifest capture. It cleans any stray (completed) MCR, clears
// status.manifestCaptureRequestName, and patches the demo snapshot Ready as a mirror of the bound
// SnapshotContent.Ready. A later MCP Ready=False/Failed degrades the content and is mirrored here
// without re-capture. Wake-up is event-driven via the bound-content watch (content_watch.go), so no
// polling/requeue is used. Returns handled=true when the handoff is complete and the caller must stop.
func demoMirrorOnlyIfHandoffComplete(
	ctx context.Context,
	c client.Client,
	reader client.Reader,
	namespace string,
	snapshotKind string,
	snapshotName string,
	contentName string,
	currentMCRName string,
	patchReady func(status metav1.ConditionStatus, reason, message string) error,
	clearMCRName func() error,
) (bool, error) {
	handoffComplete, err := demoSnapshotContentManifestHandoffComplete(ctx, reader, contentName)
	if err != nil || !handoffComplete {
		return false, err
	}
	if err := demoCleanupStrayManifestCaptureRequest(ctx, c, reader, namespace, snapshotKind, snapshotName, contentName); err != nil {
		return false, err
	}
	if currentMCRName != "" {
		if err := clearMCRName(); err != nil {
			return false, err
		}
	}
	contentReady, contentReason, contentMessage, err := commonSnapshotContentReadyForSnapshot(ctx, reader, contentName)
	if err != nil {
		return false, err
	}
	if contentReady {
		return true, patchReady(metav1.ConditionTrue, snapshot.ReasonCompleted, contentMessage)
	}
	return true, patchReady(metav1.ConditionFalse, contentReason, contentMessage)
}

// demoCleanupStrayManifestCaptureRequest removes a completed MCR left over after handoff
// so later reconciles do not observe an empty checkpointName on a live request.
func demoCleanupStrayManifestCaptureRequest(
	ctx context.Context,
	c client.Client,
	reader client.Reader,
	namespace string,
	snapshotKind string,
	snapshotName string,
	contentName string,
) error {
	mcrName := demoSnapshotManifestCaptureRequestName(snapshotKind, namespace, snapshotName)
	key := types.NamespacedName{Namespace: namespace, Name: mcrName}
	mcr := &ssv1alpha1.ManifestCaptureRequest{}
	if err := c.Get(ctx, key, mcr); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	safe, err := demoSnapshotManifestCaptureRequestReadyForCleanup(ctx, reader, key, contentName)
	if err != nil {
		return err
	}
	if !safe {
		return nil
	}
	return cleanupDemoSnapshotManifestCaptureRequest(ctx, c, mcr)
}

func demoReconcilerReader(apiReader, fallback client.Reader) client.Reader {
	if apiReader != nil {
		return apiReader
	}
	return fallback
}
