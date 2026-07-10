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

// reclaimVolumeSnapshotContent makes a VolumeSnapshotContent's physical reclaim self-sufficient at teardown:
// it flips spec.deletionPolicy Retain->Delete AND stamps the CSI deletion annotation
// snapshot.storage.kubernetes.io/volumesnapshot-being-deleted=yes, then issues a delete so the CSI
// external-snapshotter reclaims the underlying physical snapshot. For a dynamically-provisioned VSC still
// bi-directionally bound to its VolumeSnapshot, the legacy external-snapshotter delete rule fires ONLY when
// that annotation is present; it is normally stamped by the common controller during the bound-VS deletion
// lifecycle, but once the bound VolumeSnapshot is already gone that stamp can be lost (content lookup miss,
// binding mismatch, force-stripped VS finalizers, or a non-deletion-candidate VS), permanently wedging the
// VSC + physical snapshot. Stamping it ourselves — state-snapshotter is the authority for these VSCs, having
// pinned them to Retain and now intentionally reclaiming them — removes the dependency on that
// lifecycle-window stamp entirely. The VSC also carries our artifact-protect finalizer (removed by the
// caller) and, on the import path, a second non-controller ownerRef to the DataImport keeper (C4) — neither
// blocks this explicit, deterministic reclaim. NotFound / already-deleting is success; the annotation has no
// effect until a deletionTimestamp exists and deletionPolicy still gates the physical delete, so it is inert
// (and, for VCR-leg VSC-only contents, redundant-but-harmless) outside this teardown path.
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
		// Two independent repairs. needsAnnotation also recovers a VSC previously flipped to Delete but left
		// unannotated (the old code returned early on Delete-policy and never stamped it). Return early ONLY
		// when both are already satisfied so the no-op path issues zero Patch calls.
		annotations := vsc.GetAnnotations()
		needsPolicy := policy != volumeSnapshotContentDeletePolicy
		needsAnnotation := annotations[volumeSnapshotBeingDeletedAnnotation] != volumeSnapshotBeingDeletedValue
		if !needsPolicy && !needsAnnotation {
			return nil
		}
		base := vsc.DeepCopy()
		if needsPolicy {
			if serr := unstructured.SetNestedField(vsc.Object, volumeSnapshotContentDeletePolicy, "spec", "deletionPolicy"); serr != nil {
				return fmt.Errorf("set VolumeSnapshotContent %s deletionPolicy=Delete: %w", vscName, serr)
			}
		}
		if needsAnnotation {
			// Preserve unrelated annotations: GetAnnotations returned every existing key; canonicalize only ours.
			if annotations == nil {
				annotations = map[string]string{}
			}
			annotations[volumeSnapshotBeingDeletedAnnotation] = volumeSnapshotBeingDeletedValue
			vsc.SetAnnotations(annotations)
		}
		return r.Patch(ctx, vsc, client.MergeFrom(base))
	}); err != nil {
		return fmt.Errorf("reclaim VolumeSnapshotContent %s (flip to Delete + stamp being-deleted): %w", vscName, err)
	}

	vsc := &unstructured.Unstructured{}
	vsc.SetGroupVersionKind(volumeSnapshotContentGVK())
	vsc.SetName(vscName)
	if err := r.Delete(ctx, vsc); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("reclaim VolumeSnapshotContent %s (delete): %w", vscName, err)
	}
	logger.Info("Reclaiming data artifact on SnapshotContent teardown (flipped Retain->Delete + stamped being-deleted + delete)", "vsc", vscName)
	return nil
}

// volumeSnapshotContentDeletePolicy is the CSI deletionPolicy that makes deleting the VSC reclaim the
// physical snapshot (the inverse of volumeSnapshotContentRetainPolicy used to pin durability).
const volumeSnapshotContentDeletePolicy = "Delete"

// volumeSnapshotBeingDeletedAnnotation / volumeSnapshotBeingDeletedValue are the CSI external-snapshotter
// deletion gate for a bound, Delete-policy VolumeSnapshotContent. The sidecar's legacy shouldDelete rule
// deletes such a VSC only when this annotation is present with the canonical value; the common controller
// normally stamps it during the bound VolumeSnapshot deletion lifecycle. reclaimVolumeSnapshotContent stamps
// it directly so the reclaim completes even when the bound VolumeSnapshot is already gone (lost-stamp wedge).
const (
	volumeSnapshotBeingDeletedAnnotation = "snapshot.storage.kubernetes.io/volumesnapshot-being-deleted"
	volumeSnapshotBeingDeletedValue      = "yes"
)
