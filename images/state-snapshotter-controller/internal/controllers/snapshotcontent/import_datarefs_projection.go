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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/snaphelpers"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// projectContentDataLegFromDataImport is the import twin of the capture data-leg projection (VCR / bound
// VSC): it makes the aggregator the single writer of SnapshotContent.status.data for GENERIC import
// leaves. A generic import leaf carries no live VCR — its volume artifact is
// produced by a DataImport found by reverse-lookup (DataImport.spec.targetRef -> this leaf). Once the
// DataImport has produced its VolumeSnapshotContent the aggregator enriches, hands the VSC off to the
// content (Retain + ownerRef), and publishes status.data. The binder retains ONLY the leaf-facing work
// (terminal-reason surfacing on an unsupported artifact, and the status.data export mirror onto the leaf
// snapshot) — it no longer writes the content.
//
// Native-CSI import VolumeSnapshots do NOT come here: they are kind VolumeSnapshot, so
// reconcileDataLegProjection routes them to projectContentDataLegFromBoundVSC (the import binder publishes
// the recovered-PVC source onto owner.status.sourceRef + the imported VSC onto
// boundVolumeSnapshotContentName, and that native-CSI branch projects the content data uniformly with
// capture VS).
//
// Like the VCR branch it is latch-idempotent. UNLIKE the VCR capture branch (which surfaces a failed VCR /
// Variant-A fault as a terminal termReason the aggregation folds into content.Ready, decision D2), this
// import branch never surfaces a terminal reason yet: it publishes, or requeues while pending, and the
// import binder still owns the terminal Ready=False for import faults (cardinality, unsupported artifact).
// The termReason/termMessage returns exist only for signature parity with the capture path.
func (r *SnapshotContentController) projectContentDataLegFromDataImport(ctx context.Context, contentObj, owner *unstructured.Unstructured) (requeue bool, termReason string, termMessage string, err error) {
	if !r.GVKRegistry.RequiresDataArtifact(owner.GetObjectKind().GroupVersionKind().Kind) {
		// Structural import node (root Snapshot, VM snapshot, ...): manifests + children only, no data leg.
		return false, "", "", nil
	}
	contentName := contentObj.GetName()

	// Reverse-lookup the DataImport that materializes this leaf's data leg (the import marker carries no
	// name). >=2 is a fail-closed fault the binder surfaces terminally; none means d8 has not created it yet.
	di, treason, _, lErr := controllercommon.FindDataImportForLeaf(ctx, r.Client, owner)
	if lErr != nil {
		return false, "", "", lErr
	}
	if treason != "" {
		// Fail-closed cardinality fault: the binder surfaces the terminal Ready=False; the aggregator never
		// trusts any of the ambiguous DataImports as authority — it requeues pre-publish, keeps a complete
		// published binding, and finishes an incomplete one from the published copy alone.
		return r.completeOrKeepPublishedImportLeg(ctx, contentName)
	}
	if di == nil {
		// Pre-publish: DataImport not visible yet -> requeue. Post-publish: keep a COMPLETE latched
		// status.data; an INCOMPLETE one keeps publishing from the published copy so the reaped DataImport
		// does not freeze the size gap (see completeOrKeepPublishedImportLeg).
		return r.completeOrKeepPublishedImportLeg(ctx, contentName)
	}

	binding, ready, dtreason, _ := BuildImportDataBinding(di, owner)
	if dtreason != "" {
		// Non-retryable import fault (e.g. a non-VolumeSnapshotContent artifact): the binder surfaces it
		// terminally on the leaf; the aggregator declines to publish (holds pending).
		return !r.contentHasData(ctx, contentName), "", "", nil
	}
	if !ready {
		// DataImport has not produced its artifact yet -> pending.
		return !r.contentHasData(ctx, contentName), "", "", nil
	}

	content := &storagev1alpha1.SnapshotContent{}
	if cErr := r.Get(ctx, client.ObjectKey{Name: contentName}, content); cErr != nil {
		return false, "", "", cErr
	}
	// The volume metadata of an imported volume is carried only by the DataImport (its StorageClass in the
	// spec, its volumeMode and the filesystem it was written onto in the status); the aggregator is the single
	// writer that projects it into the content, and the leaf mirrors then copy the content verbatim. Unlike the
	// shared bound-VSC branch, this one needs no fallback to the published copy HERE: the unresolved-DataImport
	// case has already been handled above (completeOrKeepPublishedImportLeg keeps a complete binding and
	// finishes an incomplete one from the published copy).
	importMeta := importVolumeMetadataFromDataImport(di)

	// Fast-path latch: skip re-enriching/re-publishing when the published dataRef already matches the artifact,
	// the durable restore size has been captured, AND every import-metadata field we know is published as such
	// (a content bound before one of them was projected therefore self-heals instead of keeping the gap
	// forever).
	//
	// Size MUST gate the latch, symmetrically with the bound-VSC branch, and for the same reason: this
	// projection publishes as soon as the DataImport reports its artifact, which can PRECEDE the driver
	// publishing VolumeSnapshotContent.status.restoreSize — the only place the durable size comes from (the
	// imported leaf has no live PVC, and the DataImport records the REQUESTED scratch size in
	// spec.storageParams, not the size the artifact can be restored to). Latching on the artifact alone freezes
	// status.data with an empty size for good: every later pass re-evaluates this same condition and closes it
	// again, so nothing ever backfills the field, and restore/export size the target PVC from it. Re-enriching
	// until the size lands backfills it on the pass after the driver reports it; PublishSnapshotContentDataRef
	// is a no-op once the binding is equal, so the waiting passes re-read but do not write.
	if content.Status.Data != nil &&
		content.Status.Data.ArtifactRef == binding.ArtifactRef &&
		content.Status.Data.Size != "" &&
		importMeta.matchesPublished(content.Status.Data) {
		return false, "", "", nil
	}
	requeue, err = r.publishDataBindings(ctx, contentName, []storagev1alpha1.SnapshotDataBinding{*binding}, importMeta)
	return requeue, "", "", err
}

// completeOrKeepPublishedImportLeg handles the generic-import data leg when no single live DataImport
// attests it any more (not created yet, reaped by its idle TTL after a completed import, or ambiguous):
// pre-publish it requeues until one shows up; a COMPLETE published binding is kept latched untouched;
// an INCOMPLETE one — empty size, the DataImport vanished between the artifact publish and the driver
// reporting restoreSize — keeps publishing, rebuilt from the published copy itself, so the enricher
// backfills the durable size from the live VolumeSnapshotContent named by the published artifactRef.
//
// Without the incomplete branch a bare "content has data -> keep" exit re-latched the size gap forever:
// every later pass — including the VSC watch wake-up that delivers restoreSize — took the same exit, the
// content went Ready with an empty size (readiness gates on readyToUse, not size), and an empty
// volumeMode would fail-close export for good. The published copy is the only surviving record of the
// import volume metadata (the same contract importVolumeMetadataFromPublished states), and rebuilding
// the binding from {sourceRef, artifactRef} of the published copy preserves the artifact identity —
// including the uid an earlier publish already enriched — instead of re-deriving it from anything live.
func (r *SnapshotContentController) completeOrKeepPublishedImportLeg(ctx context.Context, contentName string) (requeue bool, termReason string, termMessage string, err error) {
	content := &storagev1alpha1.SnapshotContent{}
	if cErr := r.Get(ctx, client.ObjectKey{Name: contentName}, content); cErr != nil {
		return false, "", "", cErr
	}
	published := content.Status.Data
	if published == nil {
		// Pre-publish: nothing recorded yet -> requeue until a DataImport becomes visible.
		return true, "", "", nil
	}
	if published.Size != "" {
		// Post-publish and complete: keep the latched status.data (a reaped DataImport with published
		// data is the normal steady state of a finished import, not a fault).
		return false, "", "", nil
	}
	binding := storagev1alpha1.SnapshotDataBinding{
		SourceRef:   published.SourceRef,
		ArtifactRef: published.ArtifactRef,
	}
	requeue, err = r.publishDataBindings(ctx, contentName,
		[]storagev1alpha1.SnapshotDataBinding{binding}, importVolumeMetadataFromPublished(published))
	return requeue, "", "", err
}

// BuildImportDataBinding maps a DataImport's produced artifact (status.data.artifactRef) into the single
// SnapshotDataBinding for a generic imported leaf's content. ready=false (binding nil, no terminal reason)
// means the DataImport has not produced its artifact yet. A non-empty terminalReason is a non-retryable
// fault. Pure function (no client) so it is unit-tested directly and shared by the aggregator (publish) and
// the import binder (terminal-reason precondition + export mirror).
//
// Moved from genericbinder to the aggregator's package in the import creator/main unification: the
// aggregator is the sole writer of content.status.data.
func BuildImportDataBinding(di *unstructured.Unstructured, leaf *unstructured.Unstructured) (binding *storagev1alpha1.SnapshotDataBinding, ready bool, terminalReason string, terminalMessage string) {
	apiVersion, _, _ := unstructured.NestedString(di.Object, "status", "data", "artifactRef", "apiVersion")
	kind, _, _ := unstructured.NestedString(di.Object, "status", "data", "artifactRef", "kind")
	name, _, _ := unstructured.NestedString(di.Object, "status", "data", "artifactRef", "name")
	// uid is best-effort (DataImport fills it from the VCR artifact uid). When empty, the dataRef
	// enricher backfills it from the live VolumeSnapshotContent; when present, it is preserved.
	uid, _, _ := unstructured.NestedString(di.Object, "status", "data", "artifactRef", "uid")
	if apiVersion == "" || kind == "" || name == "" {
		return nil, false, "", ""
	}
	if kind != snapshot.KindVolumeSnapshotContent {
		// PV-backed (Detach) artifacts need the PersistentVolume data-readiness path (follow-up). Fail loud
		// rather than publishing a dataRef the SnapshotContent readiness cannot validate as Ready.
		return nil, false, snapshot.ReasonDataArtifactInvalid,
			fmt.Sprintf("DataImport %s produced a %q data artifact; import dataRef currently supports %s only",
				di.GetName(), kind, snapshot.KindVolumeSnapshotContent)
	}
	leafGVK := leaf.GetObjectKind().GroupVersionKind()
	// volumeMode and fsType describe the volume the bytes were staged onto and the filesystem they were
	// actually written onto. EnrichDataBindingsWithVolumeMetadata cannot recover either here: the binding
	// targets the leaf snapshot, not a live PVC, so the PVC-based enricher only fills Size. The DataImport is
	// the authority for both (see controllercommon.ImportVolumeMode / ImportFsType for the paths and why the
	// values exist nowhere else once it is reaped), and downstream restore fails CLOSED on an empty
	// volumeMode rather than defaulting to Filesystem.
	//
	// storageClassName is NOT set here: it comes from the DataImport SPEC
	// (spec.storageParams.storageClassName), not from its status, and this function is also called by the
	// import binder purely to test for a terminal artifact fault. The caller
	// (projectContentDataLegFromDataImport) assembles all three into importVolumeMetadata and passes them to
	// publishDataBindings, which stamps them after enrichment; the two set here are the same values and let
	// the caller's latch compare against the binding it is about to publish.
	volumeMode := controllercommon.ImportVolumeMode(di)
	fsType := controllercommon.ImportFsType(di)
	return &storagev1alpha1.SnapshotDataBinding{
		// The imported leaf has no live source PVC; use the leaf identity as the binding source so the
		// data binding is stable/idempotent (size etc. are enriched from VolumeSnapshotContent.status.restoreSize).
		SourceRef: storagev1alpha1.SnapshotSubjectRef{
			APIVersion: leafGVK.GroupVersion().String(),
			Kind:       leafGVK.Kind,
			Namespace:  leaf.GetNamespace(),
			Name:       leaf.GetName(),
			UID:        leaf.GetUID(),
		},
		ArtifactRef: storagev1alpha1.SnapshotDataArtifactRef{
			APIVersion: apiVersion,
			Kind:       kind,
			Name:       name,
			UID:        types.UID(uid),
		},
		VolumeMode: volumeMode,
		FsType:     fsType,
	}, true, "", ""
}
