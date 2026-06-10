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
)

// Condition types: the final 4-condition model (DomainReady, RequestsReady, ChildrenReady, Ready).
const (
	// ConditionReady indicates the object is ready for use.
	// On SnapshotContent it is the single aggregate: Ready = RequestsReady && ChildrenReady.
	// On Snapshot it mirrors the bound SnapshotContent.Ready, except for terminal child-Snapshot capture failures.
	ConditionReady = "Ready"

	// ConditionRequestsReady reports that this node's own requests completed and durable refs are
	// published on SnapshotContent (manifestCheckpointName, dataRefs[]). It does not consider children.
	ConditionRequestsReady = "RequestsReady"

	// ConditionChildrenReady reports that all child SnapshotContents are Ready=True
	// (a leaf with no children is ChildrenReady=True vacuously).
	ConditionChildrenReady = "ChildrenReady"

	// ConditionDomainReady indicates that the domain controller finished planning.
	// It is a gate/barrier (not part of the Ready formula); readers must require
	// observedGeneration == generation.
	ConditionDomainReady = "DomainReady"
)

// Reasons for Ready=False
const (
	ReasonContentMissing       = "ContentMissing"
	ReasonChildSnapshotMissing = "ChildSnapshotMissing"
	ReasonArtifactMissing      = "ArtifactMissing"
	// ReasonArtifactNotReady is an internal/compat reason for "artifact exists but not ready yet".
	// The data leg surfaces this state on RequestsReady/Ready as ReasonDataCapturePending (progress-aware);
	// ReasonArtifactNotReady is retained for internal classification and backward compatibility.
	ReasonArtifactNotReady = "ArtifactNotReady"
	// ReasonDataCapturePending is the surfaced pending reason for the data leg: published data artifacts
	// (e.g. VolumeSnapshotContent) exist but are not yet ready. Non-terminal. Message carries a
	// "<ready>/<total> ready" progress count and a capped pending list.
	ReasonDataCapturePending       = "DataCapturePending"
	ReasonDataArtifactInvalid      = "DataArtifactInvalid"
	ReasonDataArtifactNotSupported = "DataArtifactNotSupported"
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
)

// Reasons for DomainReady=False.
const (
	ReasonChildGraphPending = "ChildGraphPending"
	ReasonListFailed        = "ListFailed"
	ReasonCreateChildFailed = "CreateChildFailed"
	// ReasonPriorityLayerPending is set on a parent Snapshot while a higher-priority child snapshot
	// layer has not yet published a current DomainReady=True. This is NOT a failure and has no deadline:
	// a child snapshot (e.g. a large-storage capture) may legitimately stay pending for hours. The
	// parent holds DomainReady=False/PriorityLayerPending (with the pending children listed in the
	// message) and never starts capture until the layer is ready. Waiting is woken primarily by child
	// watches; a RequeueAfter polling fallback covers a missed watch event.
	ReasonPriorityLayerPending = "PriorityLayerPending"
	ReasonGraphPlanningFailed  = "GraphPlanningFailed"
	// ReasonSourceListForbidden is set when listing a mapped source kind is rejected with Forbidden.
	// RBAC for domain/custom resources is granted externally (Deckhouse RBAC controller, signalled via
	// DSC RBACReady); the planner must not treat a Forbidden source list as "no objects" (that would
	// silently drop coverage). Instead it degrades the graph (DomainReady=False) and requeues so coverage
	// resumes once RBAC is granted, without spamming hard reconcile errors.
	ReasonSourceListForbidden = "SourceListForbidden"
	// ReasonSourceIdentityAnnotationMismatch is set when an existing child snapshot's generic source
	// identity annotation drifted from the value the planner manages. The planner fails closed (no
	// silent rewrite) so external corruption/races surface instead of being masked.
	ReasonSourceIdentityAnnotationMismatch = "SourceIdentityAnnotationMismatch"
)

// Reasons for Ready=True
const (
	ReasonReady     = "Ready"
	ReasonCompleted = "Completed"
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
