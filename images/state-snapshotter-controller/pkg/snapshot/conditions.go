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
//
// Ready is the ONLY user-facing condition. The former PlanningReady/Consistent conditions were
// replaced by status.captureState.domainSpecificController.phase, and the ManifestsArchived latch
// condition by SnapshotContent.status.subtreeManifestsPersisted (a core-internal bool).
const (
	ConditionReady = storagev1alpha1.ConditionReady

	ReasonArtifactMissing     = storagev1alpha1.ReasonArtifactMissing
	ReasonCompleted           = storagev1alpha1.ReasonCompleted
	ReasonCreateChildFailed   = storagev1alpha1.ReasonCreateChildFailed
	ReasonGraphPlanningFailed = storagev1alpha1.ReasonGraphPlanningFailed

	// ReasonChildSnapshotDeleted (NON-terminal) and ReasonChildSnapshotLost (terminal) are folded onto the
	// owner Ready mirror by the SnapshotContentController when a declared child snapshot vanishes. Defined
	// canonically in api/storage; see there for the full recoverable-vs-lost semantics.
	ReasonChildSnapshotDeleted = storagev1alpha1.ReasonChildSnapshotDeleted
	ReasonChildSnapshotLost    = storagev1alpha1.ReasonChildSnapshotLost
)

// IsReasonTerminal reports whether a Ready=False reason is terminal. Re-exported from api/storage so
// core code and the wave-barrier reference the single canonical classifier.
var IsReasonTerminal = storagev1alpha1.IsReasonTerminal

// Condition types: the public condition model
// (PlanningReady, ManifestsReady, VolumeReady, ChildrenReady, Ready).
const (
	// ConditionManifestsReady reports this node's own manifest leg readiness: the manifest
	// capture checkpoint (status.manifestCheckpointName) is published and Ready (empty archive counts).
	// It does not consider the volume leg or children.
	ConditionManifestsReady = "ManifestsReady"

	// ConditionVolumeReady reports this node's own volume/data leg readiness: the node's data
	// artifact (status.dataRef) is Ready (a node with no dataRef is VolumeReady=True vacuously).
	// It does not consider the manifest leg or children.
	ConditionVolumeReady = "VolumeReady"

	// ConditionChildrenReady reports that all child SnapshotContents are Ready=True
	// (a leaf with no children is ChildrenReady=True vacuously).
	ConditionChildrenReady = "ChildrenReady"
)

// Reasons for Ready=False
const (
	ReasonContentMissing = "ContentMissing"
	// ReasonArtifactNotReady is an internal/compat reason for "artifact exists but not ready yet".
	// The data leg surfaces this state on VolumeReady/Ready as ReasonDataCapturePending (progress-aware);
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
	// ReasonManifestCapturePending is set while a snapshot controller waits for its own MCR/MCP materialization.
	ReasonManifestCapturePending = "ManifestCapturePending"
	// ReasonChildrenFailed is set when any required child has a terminal Ready=False
	// (see usecase.ChildSnapshotTerminalReadyReasons).
	ReasonChildrenFailed = "ChildrenFailed"
	// ReasonChildrenLinkPending is the non-terminal, fail-closed reason on a namespace-root SnapshotContent
	// (mirrored onto Snapshot) while declared child snapshots — in particular the orphan/residual-PVC
	// volume leaves — are not yet linked into status.childrenSnapshotContentRefs. ChildrenReady is held at
	// this reason until every declared child content edge is present, so a consumer never observes the
	// first Ready=True before the orphan data is captured and linked. It subsumes the former residual/
	// orphan-PVC capture latch gate. Defined canonically in api/storage.
	ReasonChildrenLinkPending = storagev1alpha1.ReasonChildrenLinkPending
)

// Graph/capture-planning reasons surfaced on the root Snapshot Ready condition. The parent no longer
// carries a separate PlanningReady condition: waiting on an earlier weight layer folds into
// Ready=False/ChildrenPending (with the pending children listed in the message).
const (
	// ReasonListFailed is a terminal reason when listing a mapped source kind fails (in TerminalReadyReasons).
	ReasonListFailed = "ListFailed"
	// ReasonSourceListForbidden is set when listing a mapped source kind is rejected with Forbidden.
	// RBAC for domain/custom resources is granted externally (Deckhouse RBAC controller, signalled via
	// CSD AccessGranted); the planner must not treat a Forbidden source list as "no objects" (that would
	// silently drop coverage). Instead it degrades Ready (non-terminal) and requeues so coverage
	// resumes once RBAC is granted, without spamming hard reconcile errors.
	ReasonSourceListForbidden = "SourceListForbidden"
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
