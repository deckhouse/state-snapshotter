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
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// reclaimDataArtifactsFromContentObj physically reclaims the durable data artifacts (VolumeSnapshotContent)
// of a SnapshotContent that is being torn down. For the whole lifetime of a SnapshotContent its VSC is
// pinned to deletionPolicy=Retain (so deleting the per-run VolumeSnapshot/VolumeCaptureRequest never
// reclaims the snapshot); the ONLY place that reclaim is allowed is here, when the SnapshotContent itself
// goes away. This is the unified teardown reclaim for BOTH import (content deletionPolicy=Delete, deleted
// with the import root) and capture trees (content deletionPolicy=Retain, GC'd when the root ObjectKeeper
// TTL fires): in both cases the content reaches its deletion handler and its VSC must be reclaimed rather
// than orphaned. It flips each VSC Retain->Delete and deletes it, so the CSI external-snapshotter reclaims
// the physical snapshot; removing our artifact-protect finalizer (caller / removeArtifactFinalizer) then
// lets the deletion complete. Idempotent and best-effort: an already-deleting or gone VSC is skipped.
func (r *SnapshotContentController) reclaimDataArtifactsFromContentObj(ctx context.Context, contentObj *unstructured.Unstructured) error {
	contentLike, err := snapshot.ExtractSnapshotContentLike(contentObj)
	if err != nil {
		return fmt.Errorf("extract SnapshotContentLike for artifact reclaim: %w", err)
	}
	for _, binding := range contentLike.GetStatusDataRefs() {
		art := binding.Artifact
		if art.Kind != kindVolumeSnapshotContent || art.Name == "" {
			continue
		}
		if err := r.reclaimVolumeSnapshotContent(ctx, art.Name); err != nil {
			return err
		}
	}
	return nil
}

// reclaimVolumeSnapshotContent flips a VolumeSnapshotContent's spec.deletionPolicy Retain->Delete and
// issues a delete so the CSI external-snapshotter reclaims the underlying physical snapshot. The VSC also
// carries our artifact-protect finalizer (removed by the caller) and, on the import path, a second
// non-controller ownerRef to the DataImport keeper (C4) — neither blocks this explicit, deterministic
// reclaim. NotFound / already-deleting is success.
func (r *SnapshotContentController) reclaimVolumeSnapshotContent(ctx context.Context, vscName string) error {
	logger := log.FromContext(ctx)
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		vsc := &unstructured.Unstructured{}
		vsc.SetGroupVersionKind(volumeSnapshotContentGVK())
		if err := r.Get(ctx, client.ObjectKey{Name: vscName}, vsc); err != nil {
			if errors.IsNotFound(err) {
				return nil
			}
			return err
		}
		policy, _, perr := unstructured.NestedString(vsc.Object, "spec", "deletionPolicy")
		if perr != nil {
			return fmt.Errorf("read VolumeSnapshotContent %s deletionPolicy: %w", vscName, perr)
		}
		if policy == volumeSnapshotContentDeletePolicy {
			return nil
		}
		base := vsc.DeepCopy()
		if serr := unstructured.SetNestedField(vsc.Object, volumeSnapshotContentDeletePolicy, "spec", "deletionPolicy"); serr != nil {
			return fmt.Errorf("set VolumeSnapshotContent %s deletionPolicy=Delete: %w", vscName, serr)
		}
		return r.Patch(ctx, vsc, client.MergeFrom(base))
	}); err != nil {
		return fmt.Errorf("reclaim VolumeSnapshotContent %s (flip to Delete): %w", vscName, err)
	}

	vsc := &unstructured.Unstructured{}
	vsc.SetGroupVersionKind(volumeSnapshotContentGVK())
	vsc.SetName(vscName)
	if err := r.Delete(ctx, vsc); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("reclaim VolumeSnapshotContent %s (delete): %w", vscName, err)
	}
	logger.Info("Reclaiming data artifact on SnapshotContent teardown (flipped Retain->Delete + delete)", "vsc", vscName)
	return nil
}

// volumeSnapshotContentDeletePolicy is the CSI deletionPolicy that makes deleting the VSC reclaim the
// physical snapshot (the inverse of volumeSnapshotContentRetainPolicy used to pin durability).
const volumeSnapshotContentDeletePolicy = "Delete"
