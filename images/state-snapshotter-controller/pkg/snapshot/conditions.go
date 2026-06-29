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

package snapshot

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// Contract conditions/reasons are defined canonically in api/storage and aliased here so core code
// keeps using snapshot.ConditionReady etc. unchanged, while the domain controller shares the same
// definitions via api/. Core-internal leg conditions and the rest of the reason taxonomy stay below.
const (
	ConditionReady         = storagev1alpha1.ConditionReady
	ConditionPlanningReady = storagev1alpha1.ConditionPlanningReady
	// ConditionManifestsArchived is the subtree-latch contract condition (see api/storage). It is NOT
	// part of the Ready formula; it signals that this node and all descendants have archived their
	// manifests at least once and never re-opens once True.
	ConditionManifestsArchived = storagev1alpha1.ConditionManifestsArchived

	ReasonArtifactMissing     = storagev1alpha1.ReasonArtifactMissing
	ReasonCompleted           = storagev1alpha1.ReasonCompleted
	ReasonCreateChildFailed   = storagev1alpha1.ReasonCreateChildFailed
	ReasonGraphPlanningFailed = storagev1alpha1.ReasonGraphPlanningFailed

	// ManifestsArchived reasons (subtree latch): Archived (True, lifelong), Capturing (False,
	// transient), Failed (False, terminal). Defined canonically in api/storage.
	ReasonManifestsArchived      = storagev1alpha1.ReasonManifestsArchived
	ReasonManifestsCapturing     = storagev1alpha1.ReasonManifestsCapturing
	ReasonManifestsArchiveFailed = storagev1alpha1.ReasonManifestsArchiveFailed
)

// Condition types: the public condition model
// (PlanningReady, ManifestsReady, VolumesReady, ChildrenReady, Ready).
const (
	// ConditionManifestsReady reports this node's own manifest leg readiness: the manifest
	// capture checkpoint (status.manifestCheckpointName) is published and Ready (empty archive counts).
	// It does not consider the volume leg or children.
	ConditionManifestsReady = "ManifestsReady"

	// ConditionVolumesReady reports this node's own volume/data leg readiness: all required data
	// artifacts (status.dataRefs[]) are Ready (an empty dataRefs[] is VolumesReady=True vacuously).
	// It does not consider the manifest leg or children.
	ConditionVolumesReady = "VolumesReady"

	// ConditionChildrenReady reports that all child SnapshotContents are Ready=True
	// (a leaf with no children is ChildrenReady=True vacuously).
	ConditionChildrenReady = "ChildrenReady"
)

// Reasons for Ready=False
const (
	ReasonContentMissing       = "ContentMissing"
	ReasonChildSnapshotMissing = "ChildSnapshotMissing"
	// ReasonArtifactNotReady is an internal/compat reason for "artifact exists but not ready yet".
	// The data leg surfaces this state on VolumesReady/Ready as ReasonDataCapturePending (progress-aware);
	// ReasonArtifactNotReady is retained for internal classification and backward compatibility.
	ReasonArtifactNotReady = "ArtifactNotReady"
	// ReasonDataCapturePending is the surfaced pending reason for the data leg: published data artifacts
	// (e.g. VolumeSnapshotContent) exist but are not yet ready. Non-terminal. Message carries a
	// "<ready>/<total> ready" progress count and a capped pending list.
	ReasonDataCapturePending       = "DataCapturePending"
	ReasonDataArtifactInvalid      = "DataArtifactInvalid"
	ReasonDataArtifactNotSupported = "DataArtifactNotSupported"
	// ReasonDataImportAmbiguous is the terminal reason when more than one DataImport reverse-matches an
	// import-mode leaf's identity (spec.targetRef group/kind/name). Exactly one DataImport must target
	// a leaf, so an ambiguous match is fail-closed rather than picking one arbitrarily.
	ReasonDataImportAmbiguous = "DataImportAmbiguous"
	// ReasonVolumeCaptureFailed is the terminal data-leg reason when volume capture failed: a failed
	// VolumeCaptureRequest (domain path) or a terminal CSI VolumeSnapshot/VolumeSnapshotContent error
	// (namespace-root orphan-PVC path, ADR 2026-06-09 / spec §3.9.11).
	ReasonVolumeCaptureFailed = "VolumeCaptureFailed"
	// ReasonManifestCheckpointFailed is the terminal requests-leg reason when the bound ManifestCheckpoint
	// is terminally failed. Used by SnapshotContent aggregation (own requests leg) and the terminal
	// child-content classification.
	ReasonManifestCheckpointFailed = "ManifestCheckpointFailed"
	// ReasonArtifactFailed is the terminal reason for a previously-ready artifact that degraded
	// (Phase 2, damaged-artifact revalidation). Declared here as part of the shared taxonomy; it is not
	// wired into runtime until the Phase 2 wake-up/revalidation watches land.
	ReasonArtifactFailed = "ArtifactFailed"
	// ReasonContentBindingPending is the transitional pre-bind reason on a Snapshot: there is no bound
	// SnapshotContent yet (or the bound content has no Ready condition yet). After bind, Snapshot.Ready
	// is a verbatim mirror of SnapshotContent.Ready and this reason is not used.
	ReasonContentBindingPending = "ContentBindingPending"
	ReasonDeleting              = "Deleting"
	// ReasonImportPending is the non-terminal reason on an import-mode Snapshot (spec.source.import)
	// whose content has not been materialized yet. The controller does NOT capture the live namespace
	// for these; d8 uploads per-node manifests+children and (for data leaves) creates a DataImport, then
	// the import orchestrator reconstructs SnapshotContent and binds it. Requeued, never terminal.
	ReasonImportPending = "ImportPending"
	// ReasonSourceContentNotFound is the non-terminal static-bind reason on a Snapshot whose
	// spec.source.snapshotContentName references a SnapshotContent that does not exist yet (the import
	// controller may not have pre-provisioned it). The controller requeues without failing terminally.
	ReasonSourceContentNotFound = "SourceContentNotFound"
	// ReasonSnapshotContentMisbound is the terminal static-bind reason when the referenced
	// SnapshotContent.spec.snapshotRef does not point back at this Snapshot (cross-binding). Because
	// SnapshotContent.spec is immutable, the only fix is editing spec.source, so this is not requeued.
	ReasonSnapshotContentMisbound = "SnapshotContentMisbound"

	// ReasonChildrenPending is set while a required child SnapshotContent/Snapshot is not yet bound,
	// has no Ready condition, is Ready=False with a non-terminal reason, or root capture is not complete
	// yet (no higher-priority reason applies).
	ReasonChildrenPending = "ChildrenPending"
	// ReasonSubtreeManifestCapturePending is set on root Snapshot (and root SnapshotContent)
	// while status.childrenSnapshotRefs is non-empty but the subtree exclude set cannot be computed yet
	// (no root ManifestCaptureRequest until exclude is complete — distinct from ChildrenPending / ListFailed).
	ReasonSubtreeManifestCapturePending = "SubtreeManifestCapturePending"
	// ReasonNamespaceCaptureIncomplete is the non-terminal, fail-closed reason on a root Snapshot when
	// discovery-based namespace capture planning could not read every namespaced type: a Forbidden list
	// (RBAC for the transient per-namespace RoleBinding has not propagated yet) or a partial discovery
	// failure (broken aggregated APIService). The controller does NOT create the root MCR with an
	// incomplete plan; it degrades Ready and requeues until the missing types become readable. The
	// message lists the unreadable GVRs.
	ReasonNamespaceCaptureIncomplete = "NamespaceCaptureIncomplete"
	// ReasonManifestCapturePending is set while a snapshot controller waits for its own MCR/MCP materialization.
	ReasonManifestCapturePending = "ManifestCapturePending"
	// ReasonChildrenFailed is set when any required child has a terminal Ready=False
	// (see usecase.ChildSnapshotTerminalReadyReasons).
	ReasonChildrenFailed = "ChildrenFailed"
	// ReasonResidualVolumeCapturePending is the non-terminal, fail-closed reason on a namespace-root
	// SnapshotContent (mirrored onto Snapshot) while the final residual/orphan-PVC capture wave has not
	// completed (status.residualVolumeCapture.phase != Complete). It gates only the FIRST Ready=True so
	// a consumer never restores before the orphan data is captured. Defined canonically in api/storage.
	ReasonResidualVolumeCapturePending = storagev1alpha1.ReasonResidualVolumeCapturePending
)

// Reasons for PlanningReady=False.
const (
	ReasonChildGraphPending = "ChildGraphPending"
	ReasonListFailed        = "ListFailed"
	// ReasonPriorityLayerPending is set on a parent Snapshot while a higher-priority child snapshot
	// layer has not yet published a current PlanningReady=True. This is NOT a failure and has no deadline:
	// a child snapshot (e.g. a large-storage capture) may legitimately stay pending for hours. The
	// parent holds PlanningReady=False/PriorityLayerPending (with the pending children listed in the
	// message) and never starts capture until the layer is ready. Waiting is woken primarily by child
	// watches; a RequeueAfter polling fallback covers a missed watch event.
	ReasonPriorityLayerPending = "PriorityLayerPending"
	// ReasonSourceListForbidden is set when listing a mapped source kind is rejected with Forbidden.
	// RBAC for domain/custom resources is granted externally (Deckhouse RBAC controller, signalled via
	// CSD SourceAccessGranted); the planner must not treat a Forbidden source list as "no objects" (that would
	// silently drop coverage). Instead it degrades the graph (PlanningReady=False) and requeues so coverage
	// resumes once RBAC is granted, without spamming hard reconcile errors.
	ReasonSourceListForbidden = "SourceListForbidden"
)

// Reasons for Ready=True
const (
	ReasonReady = "Ready"
)

// SetCondition sets a condition on a SnapshotLike or SnapshotContentLike object.
//
// This is the ONLY way to write conditions - all controllers must use this function.
// This ensures consistency and prevents races.
//
// IMPORTANT: Function Contract
//
// This function is a formal contract defined in unified-snapshots-test-plan.md.
// The behavior MUST NOT be changed without updating:
//   - unified-snapshots-test-plan.md (FUNCTIONS: pkg/snapshot Conditions)
//
// Contract Rules:
//   - MUST be idempotent (setting same condition twice has no effect on LastTransitionTime)
//   - MUST update LastTransitionTime ONLY when status changes
//   - MUST work with any object implementing SnapshotLike or SnapshotContentLike
//   - observedGeneration is NOT set automatically (remains 0) - it's optional per ADR
//     Controllers should set it explicitly if needed (e.g., obj.GetGeneration())
//
// See: unified-snapshots-test-plan.md (TEST CASE: SetCondition - Idempotency)
func SetCondition(obj interface{}, conditionType string, status metav1.ConditionStatus, reason, message string) {
	var conditions []metav1.Condition
	var setter func([]metav1.Condition)

	switch v := obj.(type) {
	case SnapshotLike:
		conditions = v.GetStatusConditions()
		setter = v.SetStatusConditions
	case SnapshotContentLike:
		conditions = v.GetStatusConditions()
		setter = v.SetStatusConditions
	default:
		return
	}

	// Check if condition already exists with same status
	existingCond := meta.FindStatusCondition(conditions, conditionType)
	var lastTransitionTime metav1.Time
	if existingCond != nil && existingCond.Status == status {
		// Status hasn't changed - preserve LastTransitionTime
		lastTransitionTime = existingCond.LastTransitionTime
	} else {
		// Status changed or condition doesn't exist - use current time
		lastTransitionTime = metav1.Now()
	}

	cond := metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: lastTransitionTime,
	}

	// Remove existing condition of the same type
	meta.RemoveStatusCondition(&conditions, conditionType)
	// Add new condition
	meta.SetStatusCondition(&conditions, cond)

	setter(conditions)
}

// HasCondition checks if an object has a condition with the given type and status
func HasCondition(obj interface{}, conditionType string, status metav1.ConditionStatus) bool {
	var conditions []metav1.Condition

	switch v := obj.(type) {
	case SnapshotLike:
		conditions = v.GetStatusConditions()
	case SnapshotContentLike:
		conditions = v.GetStatusConditions()
	default:
		return false
	}

	cond := meta.FindStatusCondition(conditions, conditionType)
	return cond != nil && cond.Status == status
}

// GetCondition returns the condition with the given type, or nil if not found
func GetCondition(obj interface{}, conditionType string) *metav1.Condition {
	var conditions []metav1.Condition

	switch v := obj.(type) {
	case SnapshotLike:
		conditions = v.GetStatusConditions()
	case SnapshotContentLike:
		conditions = v.GetStatusConditions()
	default:
		return nil
	}

	return meta.FindStatusCondition(conditions, conditionType)
}

// IsReady returns true if the object has Ready=True condition.
//
// Contract: Pure function, idempotent, no side effects.
// Works with any object implementing SnapshotLike or SnapshotContentLike.
//
// See: unified-snapshots-test-plan.md (TEST CASE: IsReady - Returns True Only When Ready=True)
func IsReady(obj interface{}) bool {
	return HasCondition(obj, ConditionReady, metav1.ConditionTrue)
}

// IsTerminal returns true if the object is in a terminal state (Ready=True or Ready=False)
func IsTerminal(obj interface{}) bool {
	readyCond := GetCondition(obj, ConditionReady)
	return readyCond != nil && (readyCond.Status == metav1.ConditionTrue || readyCond.Status == metav1.ConditionFalse)
}
