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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/api/storage/v1alpha1/dataleg"
	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/snaphelpers"
	vcctrl "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/volumecapture"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
)

// reconcileDataLegProjection is the single writer of SnapshotContent.status.data for domain owners.
// It replaces the binder's data-leg publish
// (genericbinder/domain_content.go): the aggregator projects the owning snapshot's captured volume
// artifact onto status.data, performs the VolumeSnapshotContent Retain + ownerRef handoff, and enriches
// volume metadata.
//
// Core is the single writer of the terminal Ready (vcr-watch-core-terminal, decision D2): on a failed
// data-leg VCR (or the Variant-A >1-artifact fault) it returns a non-empty (termReason, termMessage) so
// reconcileCommonSnapshotContentStatus makes the CONTENT itself terminal (DataReady=VolumeCaptureFailed).
// The content-level terminal is what propagates up the content-aggregation tree as ChildrenFailed (the
// former hack folded the leg terminal only into the owning snapshot's Ready, so it never reached the
// parent contents). Otherwise it returns an empty termReason and only publishes, or requeues while the leg
// is pending.
//
// Two data sources by owner kind:
//   - VCR domains (demo disk, etc.): captureState.domainSpecificController.volumeCaptureRequestName ->
//     VolumeCaptureRequest -> VolumeSnapshotContent;
//   - native-CSI kind VolumeSnapshot: the fork binds the VS to a VSC directly, so the aggregator
//     reads owner.status.boundVolumeSnapshotContentName. Active once the CSD registers the kind.
//
// The route is taken through dataleg.Classify, on the two structural discriminators {owner is a CSI
// VolumeSnapshot, owner declares spec.mode: Import}. Those four combinations ARE the four cells of the
// status.data completeness matrix (dataleg.Scenarios), and routing through it is what keeps the two from
// drifting: the matrix cannot describe three paths while the router takes four, and the import cells cannot
// be merged in the table without merging them here. Note the asymmetry it makes explicit — a native-CSI
// IMPORT does not take the DataImport branch; it shares the bound-VSC projection with capture.
//
// It is latch-idempotent: once status.data covers the source, it is kept even after the VCR is reaped.
func (r *SnapshotContentController) reconcileDataLegProjection(ctx context.Context, contentObj, owner *unstructured.Unstructured, ownerNamespace string, ownerFound bool) (requeue bool, termReason string, termMessage string, err error) {
	if !ownerFound {
		// spec.snapshotRef absent (synthetic/legacy) or owner not observable yet: nothing to project.
		return false, "", "", nil
	}

	scenario := dataleg.Classify(
		owner.GetObjectKind().GroupVersionKind().Kind == snapshot.KindVolumeSnapshot,
		usecase.IsUnstructuredImportMode(owner),
	)
	switch scenario {
	case dataleg.NativeCapture, dataleg.NativeImport:
		// Native-CSI data leg: the VolumeSnapshot IS the volume capture; project from its bound VSC.
		// This covers BOTH capture VS (fork binds it) and import VS (the import binder publishes
		// snapshotSource + boundVolumeSnapshotContentName), so import VS does not take the DataImport branch.
		return r.projectContentDataLegFromBoundVSC(ctx, contentObj, owner, ownerNamespace)

	case dataleg.DomainImport:
		// Generic import leaf: no live VCR — the volume artifact comes from the reverse-looked-up
		// DataImport's produced VolumeSnapshotContent. Structural import nodes (root/VM) are not data-bearing
		// and short-circuit inside.
		return r.projectContentDataLegFromDataImport(ctx, contentObj, owner)

	case dataleg.DomainCapture:
		vcrName, nestedErr := domainVolumeCaptureRequestName(owner)
		if nestedErr != nil {
			return false, "", "", nestedErr
		}
		if vcrName == "" {
			// Manifest-only leaf (no data leg) or pre-Planned: nothing to project this pass.
			return false, "", "", nil
		}
		return r.projectContentDataLegFromVCR(ctx, contentObj, ownerNamespace, vcrName)

	default:
		// Unreachable while Classify is total over its two booleans (dataleg.Validate proves the four
		// combinations cover the axis). Kept as an error rather than a panic or a silent no-op so a future
		// scenario added to the axis without a route here is loud instead of dropping the data leg.
		return false, "", "", fmt.Errorf("SnapshotContent %s: no data-leg route for scenario %q (owner %s %s/%s)",
			contentObj.GetName(), scenario, owner.GetObjectKind().GroupVersionKind().Kind, owner.GetNamespace(), owner.GetName())
	}
}

// domainVolumeCaptureRequestName reads the domain-created VolumeCaptureRequest name off a domain capture
// owner. Empty (without error) means the domain has not declared a data leg on this pass — a manifest-only
// leaf, or a node that has not reached Planned yet.
func domainVolumeCaptureRequestName(owner *unstructured.Unstructured) (string, error) {
	name, _, err := unstructured.NestedString(owner.Object, "status", "captureState", "domainSpecificController", "volumeCaptureRequestName")
	return name, err
}

// projectContentDataLegFromVCR reads the domain-created VolumeCaptureRequest, and once it is Ready and its
// dataRefs are consistent, enriches volume metadata, transfers VolumeSnapshotContent ownership to the
// SnapshotContent (Retain + ownerRef), and publishes status.data. It requeues while the leg is pending and
// keeps the published binding once the VCR is reaped (latch-idempotent).
//
// Core-owned terminal (vcr-watch-core-terminal, decision D2): a failed VCR and the >1-artifact Variant-A
// fault are surfaced here as a non-empty (termReason, termMessage). The caller makes the CONTENT terminal
// (DataReady=VolumeCaptureFailed), which propagates up as ChildrenFailed — the former hack only folded
// the leg terminal into the owning snapshot's Ready, so it never reached the parent contents.
func (r *SnapshotContentController) projectContentDataLegFromVCR(ctx context.Context, contentObj *unstructured.Unstructured, namespace, vcrName string) (requeue bool, termReason string, termMessage string, err error) {
	contentName := contentObj.GetName()

	vcr := &unstructured.Unstructured{}
	vcr.SetGroupVersionKind(vcpkg.VolumeCaptureRequestGVK)
	// Cached read: the content controller now event-driven-watches VCR (AddVolumeCaptureRequestWatch,
	// added once a data-artifact kind is registered — the VCR CRD is RESTMappable by then). A VCR status
	// flip enqueues this content directly, so the informer cache is authoritative-enough here and we no
	// longer pay an uncached read per pass. (If a VCR read ever happens before the watch was added, the
	// cached Get lazily starts the informer — correct, just first-Get-blocks-on-sync.)
	if getErr := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: vcrName}, vcr); getErr != nil {
		if errors.IsNotFound(getErr) {
			// Pre-publish: the domain has not (re)created the VCR yet -> requeue until it appears.
			// Post-publish: the binder reaped the VCR after a durable handoff -> keep the latched
			// status.data, no requeue. Distinguish by whether the content already carries data.
			return !r.contentHasData(ctx, contentName), "", "", nil
		}
		return false, "", "", getErr
	}

	expectedTargets, parseErr := vcctrl.ParseVolumeCaptureTargets(vcr)
	if parseErr != nil {
		return false, "", "", parseErr
	}

	content := &storagev1alpha1.SnapshotContent{}
	if cErr := r.Get(ctx, client.ObjectKey{Name: contentName}, content); cErr != nil {
		return false, "", "", cErr
	}
	if vcctrl.ContentDataRefsCoverExpectedTargets(content.DataList(), expectedTargets) {
		// Already published and covering the targets: latched, nothing to do.
		return false, "", "", nil
	}
	if failed, reason, msg := vcctrl.VolumeCaptureRequestFailed(vcr); failed {
		// Core-owned terminal: make the content terminal on the failed VCR so it propagates upward.
		detail := msg
		if reason != "" {
			detail = fmt.Sprintf("%s: %s", reason, msg)
		}
		return false, snapshot.ReasonVolumeCaptureFailed, fmt.Sprintf("data-leg volume capture failed: %s", detail), nil
	}
	if !vcctrl.VolumeCaptureRequestReady(vcr) {
		return true, "", "", nil
	}

	vcrRefs, refErr := vcctrl.ParseVolumeCaptureDataRefs(vcr)
	if refErr != nil {
		return false, "", "", refErr
	}
	if validateErr := vcctrl.ValidateDataRefsForPublish(expectedTargets, vcrRefs); validateErr != nil {
		// Ready VCR whose dataRefs are not yet consistent: retry without publishing.
		return true, "", "", nil
	}

	bindings := vcctrl.SnapshotDataBindingsFromVCRStatus(vcrRefs)
	// Variant A (cardinality ≤1): a domain volume leaf owns exactly one PVC. A ready VCR returning >1 data
	// artifact for one logical content is a domain decomposition fault — make the content terminal instead
	// of looping forever while the projection declines to publish.
	if len(bindings) > 1 {
		return false, snapshot.ReasonVolumeCaptureFailed,
			fmt.Sprintf("data-leg volume capture returned %d data artifacts for a single SnapshotContent %q; Variant A allows at most one PVC per domain volume node (decompose multiple volumes into child volume nodes)", len(bindings), contentName), nil
	}
	if len(bindings) != 1 {
		// Zero bindings on a ready+valid VCR: not representable yet, hold pending.
		return true, "", "", nil
	}
	// A domain capture leg attests no import metadata (zero value): everything it publishes beyond the refs is
	// read off the live source PVC by the enricher.
	requeue, err = r.publishDataBindings(ctx, contentName, bindings, importVolumeMetadata{})
	return requeue, "", "", err
}

// projectContentDataLegFromBoundVSC projects the native-CSI data leg: a VolumeSnapshot owner is
// bound to a VolumeSnapshotContent by the fork's CSI machinery (status.boundVolumeSnapshotContentName), so
// the aggregator builds the {source PVC, VSC artifact} binding from the owner status and performs the same
// enrich + Retain/ownerRef handoff + publish as the VCR branch. The source PVC is published by the domain
// reconciler at adoption (owner.status.sourceRef). Active once the CSD registers the kind.
//
// This branch is SHARED by capture and import VolumeSnapshots (the import binder publishes both
// status.sourceRef and status.boundVolumeSnapshotContentName, so imports project uniformly with capture).
// The import-only extras — the DataImport reverse-lookup and the authoritative import StorageClass — are
// therefore gated STRUCTURALLY on spec.mode: Import, never on the emptiness of a value. For a capture
// owner not a single line of import logic runs and the latch stays exactly what it was.
func (r *SnapshotContentController) projectContentDataLegFromBoundVSC(ctx context.Context, contentObj, owner *unstructured.Unstructured, _ string) (requeue bool, termReason string, termMessage string, err error) {
	contentName := contentObj.GetName()
	isImport := usecase.IsUnstructuredImportMode(owner)

	vscName, _, err := unstructured.NestedString(owner.Object, "status", "boundVolumeSnapshotContentName")
	if err != nil {
		return false, "", "", err
	}
	if vscName == "" {
		// CSI has not bound the VolumeSnapshot to a VolumeSnapshotContent yet: nothing to project.
		return true, "", "", nil
	}

	binding := storagev1alpha1.SnapshotDataBinding{
		SourceRef: volumeSnapshotOwnerSource(owner),
		ArtifactRef: storagev1alpha1.SnapshotDataArtifactRef{
			APIVersion: volumeSnapshotContentAPIVersion,
			Kind:       kindVolumeSnapshotContent,
			Name:       vscName,
		},
	}
	if binding.SourceRef.Name == "" {
		// The domain reconciler has not published status.sourceRef yet: wait.
		return true, "", "", nil
	}

	// Import-only: the volume metadata of an imported volume lives on the DataImport that staged the bytes,
	// not on any live object the enricher can read — the recovered source PVC is a checkpoint manifest, not a
	// cluster object. importMeta says per field whether we know the value; it is deliberately not derived
	// from the emptiness of the published field, so neither the publish nor the latch below ever infers a
	// decision from the value it is about to write.
	var importMeta importVolumeMetadata
	importDataImportResolved := false
	if isImport {
		// A cardinality fault (>=2 DataImports, terminalReason) is deliberately ignored here: the import
		// binder surfaces it terminally on VolumeSnapshot.status.error, and gating a publish on it in a
		// branch shared with capture would introduce a new wedge. It simply leaves the metadata unresolved.
		di, _, _, lErr := controllercommon.FindDataImportForLeaf(ctx, r.Client, owner)
		if lErr != nil {
			return false, "", "", lErr
		}
		importDataImportResolved = di != nil
		importMeta = importVolumeMetadataFromDataImport(di)
		if !importDataImportResolved {
			// Not created yet, or already reaped by its idle TTL after a completed import. Publish anyway —
			// withholding the data leg until a DataImport shows up would wedge every imported leaf whose
			// DataImport is already gone. Once one appears the latch stops matching and the metadata lands
			// on the next pass.
			logf.FromContext(ctx).V(1).Info("import VolumeSnapshot has no resolved DataImport; publishing the data leg with whatever it already carries",
				"content", contentName, "volumeSnapshot", owner.GetNamespace()+"/"+owner.GetName())
		}
	}

	content := &storagev1alpha1.SnapshotContent{}
	if cErr := r.Get(ctx, client.ObjectKey{Name: contentName}, content); cErr != nil {
		return false, "", "", cErr
	}
	if isImport && !importDataImportResolved {
		// Nothing attests these fields any more, so what the content already carries becomes the value to
		// KEEP: it is the only surviving record (the scratch volume was destroyed right after capture) and a
		// re-publish rebuilds the binding from {sourceRef, artifactRef} alone — so without this, a pass that
		// re-publishes for an unrelated reason (typically a still-missing size) would blank the fields for
		// good, and an empty volumeMode fail-closes export.
		//
		// It is deliberately NOT applied when the DataImport IS resolved: then its values are the authority,
		// including the ones it leaves empty (a Block import has no filesystem), so a content carrying a value
		// the authority does not confirm — a stale one, or one an earlier revision derived from a live PVC that
		// merely shared the source name — has to give way rather than be preserved.
		//
		// Kept out of the capture path on purpose: a capture leg re-derives its metadata from the live source
		// PVC and must run no import logic at all.
		importMeta = importVolumeMetadataFromPublished(content.Status.Data)
	}
	if content.Status.Data != nil && content.Status.Data.ArtifactRef.Name == vscName && content.Status.Data.Size != "" &&
		importMeta.matchesPublished(content.Status.Data) {
		// Already published, bound to the same VSC, AND the durable restore size captured: latched.
		// Size MUST gate the latch: unlike the VCR path (whose VCR turns Ready only after the CSI snapshot
		// completes, so restoreSize is already present at first publish), this native-CSI leg publishes as
		// soon as the VolumeSnapshot binds its VSC (status.boundVolumeSnapshotContentName) — which can
		// precede the fork's status.restoreSize. Latching on the VSC name alone would freeze status.data
		// without size forever; re-enriching until size is captured backfills it once the driver publishes
		// restoreSize (PublishSnapshotContentDataRef is a no-op once equal, so no churn after it lands).
		//
		// The import-metadata terms exist so that contents published before a field was projected self-heal;
		// each field contributes a term only while its own value is known (see matchesPublished). For a
		// capture owner importMeta is the zero value and adds no term at all, leaving this latch exactly what
		// it was: a value-based comparison there could never match, so every native-CSI capture content would
		// re-publish forever and, once its source PVC is gone, lose the durable metadata restore needs.
		return false, "", "", nil
	}
	requeue, err = r.publishDataBindings(ctx, contentName, []storagev1alpha1.SnapshotDataBinding{binding}, importMeta)
	return requeue, "", "", err
}

// importVolumeMetadata is the volume metadata that exists only on the import side, and only for as long as
// the DataImport does: the StorageClass the bytes were staged into, the volumeMode of the volume they were
// staged onto, and the filesystem they were actually written onto. None of the three can be re-derived once
// the DataImport is reaped by its idle TTL — the scratch volume is destroyed right after capture and the
// durable VolumeSnapshotContent records none of them — so publishing them onto the content is what makes them
// durable at all. The paths are owned by snaphelpers (Import* readers), which also gate them on
// DataImport.spec.mode.
//
// Per field, an empty value means "not known here", never "clear the field": see applyTo and matchesPublished.
// A capture leg attests none of it and uses the zero value — the type is only ever built for an import owner,
// and only from the structural spec.mode: Import discriminator.
type importVolumeMetadata struct {
	StorageClassName string
	VolumeMode       string
	FsType           string
}

// importVolumeMetadataFromDataImport reads all three fields off the DataImport that staged the bytes. A nil
// DataImport (not created yet, already reaped, or ambiguous) yields the zero value.
func importVolumeMetadataFromDataImport(di *unstructured.Unstructured) importVolumeMetadata {
	return importVolumeMetadata{
		StorageClassName: controllercommon.ImportStorageClassName(di),
		VolumeMode:       controllercommon.ImportVolumeMode(di),
		FsType:           controllercommon.ImportFsType(di),
	}
}

// importVolumeMetadataFromPublished takes the content's own already-published binding as the metadata to
// keep. It is for the case where no DataImport attests these fields any more (never created, reaped by its
// idle TTL, or ambiguous): the published copy is then the only surviving record, and treating it as the value
// to re-publish is what keeps a re-publish from erasing it. A content with nothing published yields the zero
// value — there is nothing to preserve and nothing to compare.
func importVolumeMetadataFromPublished(published *storagev1alpha1.SnapshotDataBinding) importVolumeMetadata {
	if published == nil {
		return importVolumeMetadata{}
	}
	return importVolumeMetadata{
		StorageClassName: published.StorageClassName,
		VolumeMode:       published.VolumeMode,
		FsType:           published.FsType,
	}
}

// applyTo stamps every known field onto each binding. Callers apply it AFTER enrichment: on import these
// values are authoritative, while the enricher can only see a live PVC that happens to share the source name.
// An unknown (empty) field is left alone rather than cleared — the caller has nothing to apply there, which is
// not a request to erase what the enricher or a previous publish produced.
func (m importVolumeMetadata) applyTo(bindings []storagev1alpha1.SnapshotDataBinding) {
	for i := range bindings {
		if m.StorageClassName != "" {
			bindings[i].StorageClassName = m.StorageClassName
		}
		if m.VolumeMode != "" {
			bindings[i].VolumeMode = m.VolumeMode
		}
		if m.FsType != "" {
			bindings[i].FsType = m.FsType
		}
	}
}

// matchesPublished reports whether every field this metadata knows is already published as such — the
// import half of a projection's fast-path latch.
//
// Each field contributes a term only while its OWN value is known; an unknown field contributes none. That
// asymmetry is the whole point: demanding equality on a value we cannot compute would never be satisfied, the
// leg would re-publish on every pass, and the field still would not appear (a wedge). Conversely, a known
// value that is not published yet keeps the latch open until it is, which is how a content published before
// the field existed heals. Per-field, because a term borrowed from another field would make one field's latch
// hinge on whether a different one happens to be resolved.
func (m importVolumeMetadata) matchesPublished(published *storagev1alpha1.SnapshotDataBinding) bool {
	if published == nil {
		return false
	}
	if m.StorageClassName != "" && published.StorageClassName != m.StorageClassName {
		return false
	}
	if m.VolumeMode != "" && published.VolumeMode != m.VolumeMode {
		return false
	}
	if m.FsType != "" && published.FsType != m.FsType {
		return false
	}
	return true
}

// publishDataBindings enriches the bindings with live volume metadata, transfers VolumeSnapshotContent
// ownership to the content (Retain + ownerRef), and publishes status.data. Handoff is retryable (requeue),
// enrich/publish errors propagate.
//
// importMeta carries the import-authoritative volume metadata (storageClassName / volumeMode / fsType) of an
// import leg, stamped onto every binding AFTER enrichment so it overrides whatever the enricher derived from a
// live PVC that happens to share the source name. A capture leg passes the zero value: it computes none of
// this and keeps exactly what the enricher read off its real source PVC. An unknown field is never a request
// to clear anything — see importVolumeMetadata.applyTo.
//
// On a successful publish it returns requeue=true so the aggregator re-runs and re-reads the content WITH
// the freshly written dataRefs (status.data is a separate patch, invisible to the same pass). Correctness
// against a premature Ready does NOT rely on this requeue alone: reconcileDataLegProjection surfaces this
// same "leg not durably published+ready" state as dataLegPending, and reconcileCommonSnapshotContentStatus
// downgrades the (stale-empty) volume leg to DataCapturePending for the pass, so Ready cannot escalate before
// the bound VolumeSnapshotContent's readyToUse is validated on the next pass.
func (r *SnapshotContentController) publishDataBindings(ctx context.Context, contentName string, bindings []storagev1alpha1.SnapshotDataBinding, importMeta importVolumeMetadata) (requeue bool, err error) {
	bindings, err = EnrichDataBindingsWithVolumeMetadata(ctx, r.Client, r.APIReader, bindings)
	if err != nil {
		return false, err
	}
	importMeta.applyTo(bindings)
	content := &storagev1alpha1.SnapshotContent{}
	if cErr := r.Get(ctx, client.ObjectKey{Name: contentName}, content); cErr != nil {
		return false, cErr
	}
	if handoffErr := EnsureVolumeSnapshotContentsOwnedByContent(ctx, r.Client, content, bindings); handoffErr != nil {
		// Retryable handoff; coverage still holds via the pending source until dataRefs are published.
		return true, nil
	}
	if pubErr := PublishSnapshotContentDataRefs(ctx, r.Client, contentName, bindings); pubErr != nil {
		return false, pubErr
	}
	return true, nil
}

// contentHasData reports whether the SnapshotContent already carries a published status.data binding.
func (r *SnapshotContentController) contentHasData(ctx context.Context, contentName string) bool {
	content := &storagev1alpha1.SnapshotContent{}
	if err := r.Get(ctx, client.ObjectKey{Name: contentName}, content); err != nil {
		return false
	}
	return content.Status.Data != nil
}

// volumeSnapshotOwnerSource builds the captured PVC source ref from a VolumeSnapshot owner's published
// status.sourceRef (written by the foundation domain reconciler at adoption). Absent
// fields yield an empty ref, which the caller treats as "source not published yet".
func volumeSnapshotOwnerSource(owner *unstructured.Unstructured) storagev1alpha1.SnapshotSubjectRef {
	apiVersion, _, _ := unstructured.NestedString(owner.Object, "status", "sourceRef", "apiVersion")
	kind, _, _ := unstructured.NestedString(owner.Object, "status", "sourceRef", "kind")
	name, _, _ := unstructured.NestedString(owner.Object, "status", "sourceRef", "name")
	namespace, _, _ := unstructured.NestedString(owner.Object, "status", "sourceRef", "namespace")
	uid, _, _ := unstructured.NestedString(owner.Object, "status", "sourceRef", "uid")
	return storagev1alpha1.SnapshotSubjectRef{
		APIVersion: apiVersion,
		Kind:       kind,
		Name:       name,
		Namespace:  namespace,
		UID:        types.UID(uid),
	}
}
