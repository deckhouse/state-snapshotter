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

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	vcctrl "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/volumecapture"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
)

// reconcileDataLegProjection is the single writer of SnapshotContent.status.data for domain owners
// (content-single-writer design §4 Slice 3 / §11.4). It replaces the binder's data-leg publish
// (genericbinder/domain_content.go): the aggregator projects the owning snapshot's captured volume
// artifact onto status.data, performs the VolumeSnapshotContent Retain + ownerRef handoff, and enriches
// volume metadata. The binder keeps ownership of the VCR lifecycle (terminal-failure surfacing, the
// dataCaptured latch, and VCR deletion after a durable handoff), so the aggregator here NEVER surfaces a
// terminal reason — it only publishes, or requeues while the leg is pending.
//
// Two data sources by owner kind:
//   - VCR domains (demo disk, etc.): captureState.domainSpecificController.volumeCaptureRequestName ->
//     VolumeCaptureRequest -> VolumeSnapshotContent (§4 Slice 3);
//   - native-CSI kind VolumeSnapshot (§11.4): the fork binds the VS to a VSC directly, so the aggregator
//     reads owner.status.boundVolumeSnapshotContentName. Dormant until the CSD registers the kind (Block 3c).
//
// It is latch-idempotent: once status.data covers the source, it is kept even after the VCR is reaped.
func (r *SnapshotContentController) reconcileDataLegProjection(ctx context.Context, contentObj, owner *unstructured.Unstructured, ownerNamespace string, ownerFound bool) (requeue bool, err error) {
	// Child-volume-node (orphan leaf) contents keep their data leg on the snapshot orphan-PVC path until
	// the orphan machinery is dismantled (Block 3d, design §11.6); skip them here — same guard as the
	// child-edge and manifest projections.
	if contentObj.GetLabels()[snapshot.LabelChildVolumeNode] == "true" {
		return false, nil
	}
	if !ownerFound {
		// spec.snapshotRef absent (synthetic/legacy) or owner not observable yet: nothing to project.
		return false, nil
	}

	if owner.GetObjectKind().GroupVersionKind().Kind == snapshot.KindVolumeSnapshot {
		// Native-CSI data leg (§11.4): the VolumeSnapshot IS the volume capture; project from its bound VSC.
		return r.projectContentDataLegFromBoundVSC(ctx, contentObj, owner, ownerNamespace)
	}

	vcrName, _, err := unstructured.NestedString(owner.Object, "status", "captureState", "domainSpecificController", "volumeCaptureRequestName")
	if err != nil {
		return false, err
	}
	if vcrName == "" {
		// Manifest-only leaf (no data leg) or pre-Planned: nothing to project this pass.
		return false, nil
	}
	return r.projectContentDataLegFromVCR(ctx, contentObj, ownerNamespace, vcrName)
}

// projectContentDataLegFromVCR reads the domain-created VolumeCaptureRequest, and once it is Ready and its
// dataRefs are consistent, enriches volume metadata, transfers VolumeSnapshotContent ownership to the
// SnapshotContent (Retain + ownerRef), and publishes status.data. It requeues while the leg is pending and
// keeps the published binding once the VCR is reaped (latch-idempotent). VCR failures and the >1-artifact
// Variant-A fault are surfaced terminally by the binder (patchSnapshotReadyFromContent); here they only
// hold the projection pending (no publish).
func (r *SnapshotContentController) projectContentDataLegFromVCR(ctx context.Context, contentObj *unstructured.Unstructured, namespace, vcrName string) (requeue bool, err error) {
	contentName := contentObj.GetName()

	vcr := &unstructured.Unstructured{}
	vcr.SetGroupVersionKind(vcpkg.VolumeCaptureRequestGVK)
	// Uncached direct read: the aggregator does not watch VCR (adding it to the per-GVK status-watch
	// builder risks breaking setup when the foundation VCR CRD is not RESTMappable yet). The 500 ms
	// self-requeue while !ready drives convergence (design §3.2).
	if getErr := r.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: vcrName}, vcr); getErr != nil {
		if errors.IsNotFound(getErr) {
			// Pre-publish: the domain has not (re)created the VCR yet -> requeue until it appears.
			// Post-publish: the binder reaped the VCR after a durable handoff -> keep the latched
			// status.data, no requeue. Distinguish by whether the content already carries data.
			return !r.contentHasData(ctx, contentName), nil
		}
		return false, getErr
	}

	expectedTargets, parseErr := vcctrl.ParseVolumeCaptureTargets(vcr)
	if parseErr != nil {
		return false, parseErr
	}

	content := &storagev1alpha1.SnapshotContent{}
	if cErr := r.Client.Get(ctx, client.ObjectKey{Name: contentName}, content); cErr != nil {
		return false, cErr
	}
	if vcctrl.ContentDataRefsCoverExpectedTargets(content.DataList(), expectedTargets) {
		// Already published and covering the targets: latched, nothing to do.
		return false, nil
	}
	if failed, _, _ := vcctrl.VolumeCaptureRequestFailed(vcr); failed {
		// The binder surfaces the terminal Ready=False; the aggregator only waits.
		return true, nil
	}
	if !vcctrl.VolumeCaptureRequestReady(vcr) {
		return true, nil
	}

	vcrRefs, refErr := vcctrl.ParseVolumeCaptureDataRefs(vcr)
	if refErr != nil {
		return false, refErr
	}
	if validateErr := vcctrl.ValidateDataRefsForPublish(expectedTargets, vcrRefs); validateErr != nil {
		// Ready VCR whose dataRefs are not yet consistent: retry without publishing.
		return true, nil
	}

	bindings := vcctrl.SnapshotDataBindingsFromVCRStatus(vcrRefs)
	// Variant A (cardinality ≤1): a domain volume leaf owns exactly one PVC. A ready VCR returning >1 data
	// artifact for one logical content is a domain decomposition fault; the binder fails it terminally.
	// The aggregator declines to publish (holds pending) rather than dropping the extra bindings.
	if len(bindings) != 1 {
		return true, nil
	}
	return r.publishDataBindings(ctx, contentName, bindings)
}

// projectContentDataLegFromBoundVSC projects the native-CSI data leg (§11.4): a VolumeSnapshot owner is
// bound to a VolumeSnapshotContent by the fork's CSI machinery (status.boundVolumeSnapshotContentName), so
// the aggregator builds the {source PVC, VSC artifact} binding from the owner status and performs the same
// enrich + Retain/ownerRef handoff + publish as the VCR branch. The source PVC is published by the domain
// reconciler at adoption (owner.status.snapshotSource). Dormant until the CSD registers the kind (Block 3c).
func (r *SnapshotContentController) projectContentDataLegFromBoundVSC(ctx context.Context, contentObj, owner *unstructured.Unstructured, _ string) (requeue bool, err error) {
	contentName := contentObj.GetName()

	vscName, _, err := unstructured.NestedString(owner.Object, "status", "boundVolumeSnapshotContentName")
	if err != nil {
		return false, err
	}
	if vscName == "" {
		// CSI has not bound the VolumeSnapshot to a VolumeSnapshotContent yet: nothing to project.
		return true, nil
	}

	binding := storagev1alpha1.SnapshotDataBinding{
		Source: volumeSnapshotOwnerSource(owner),
		Artifact: storagev1alpha1.SnapshotDataArtifactRef{
			APIVersion: volumeSnapshotContentAPIVersion,
			Kind:       kindVolumeSnapshotContent,
			Name:       vscName,
		},
	}
	if binding.Source.Name == "" {
		// The domain reconciler has not published status.snapshotSource yet: wait.
		return true, nil
	}

	content := &storagev1alpha1.SnapshotContent{}
	if cErr := r.Client.Get(ctx, client.ObjectKey{Name: contentName}, content); cErr != nil {
		return false, cErr
	}
	if content.Status.Data != nil && content.Status.Data.Artifact.Name == vscName {
		// Already published and bound to the same VSC: latched.
		return false, nil
	}
	return r.publishDataBindings(ctx, contentName, []storagev1alpha1.SnapshotDataBinding{binding})
}

// publishDataBindings enriches the bindings with live volume metadata, transfers VolumeSnapshotContent
// ownership to the content (Retain + ownerRef), and publishes status.data. Handoff is retryable (requeue),
// enrich/publish errors propagate.
//
// On a successful publish it returns requeue=true so the aggregator re-runs and re-reads the content WITH
// the freshly written dataRefs (status.data is a separate patch, invisible to the same pass). Correctness
// against a premature Ready does NOT rely on this requeue alone: reconcileDataLegProjection surfaces this
// same "leg not durably published+ready" state as dataLegPending, and reconcileCommonSnapshotContentStatus
// downgrades the (stale-empty) volume leg to DataCapturePending for the pass, so Ready cannot escalate before
// the bound VolumeSnapshotContent's readyToUse is validated on the next pass.
func (r *SnapshotContentController) publishDataBindings(ctx context.Context, contentName string, bindings []storagev1alpha1.SnapshotDataBinding) (requeue bool, err error) {
	bindings, err = EnrichDataBindingsWithVolumeMetadata(ctx, r.Client, r.APIReader, bindings)
	if err != nil {
		return false, err
	}
	content := &storagev1alpha1.SnapshotContent{}
	if cErr := r.Client.Get(ctx, client.ObjectKey{Name: contentName}, content); cErr != nil {
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
	if err := r.Client.Get(ctx, client.ObjectKey{Name: contentName}, content); err != nil {
		return false
	}
	return content.Status.Data != nil
}

// volumeSnapshotOwnerSource builds the captured PVC source ref from a VolumeSnapshot owner's published
// status.snapshotSource (design §11.4, written by the foundation domain reconciler at adoption). Absent
// fields yield an empty ref, which the caller treats as "source not published yet".
func volumeSnapshotOwnerSource(owner *unstructured.Unstructured) storagev1alpha1.SnapshotSubjectRef {
	apiVersion, _, _ := unstructured.NestedString(owner.Object, "status", "snapshotSource", "apiVersion")
	kind, _, _ := unstructured.NestedString(owner.Object, "status", "snapshotSource", "kind")
	name, _, _ := unstructured.NestedString(owner.Object, "status", "snapshotSource", "name")
	namespace, _, _ := unstructured.NestedString(owner.Object, "status", "snapshotSource", "namespace")
	uid, _, _ := unstructured.NestedString(owner.Object, "status", "snapshotSource", "uid")
	return storagev1alpha1.SnapshotSubjectRef{
		APIVersion: apiVersion,
		Kind:       kind,
		Name:       name,
		Namespace:  namespace,
		UID:        types.UID(uid),
	}
}
