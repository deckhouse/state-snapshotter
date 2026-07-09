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

package v1alpha1

// Contract conditions and reasons shared across the snapshot graph boundary (core planner <-> domain
// snapshot controllers). These are the canonical definitions: core pkg/snapshot aliases them so both
// sides reference one definition via api/. Core-internal leg conditions
// (ManifestsReady/DataReady/ChildrenReady) and the broader reason taxonomy stay in core pkg/snapshot.
//
// Ready is the ONLY user-facing condition on every snapshot object (root Snapshot, SnapshotContent,
// domain CR); it is always derived by the core. The former planning/consistency conditions
// (PlanningReady, Consistent) and the ManifestsArchived latch condition were replaced by internal
// status fields: captureState.domainSpecificController.phase, captureState.commonController.*,
// and SnapshotContent.status.subtreeManifestsPersisted.
const (
	// ConditionReady indicates the object is ready for use. On SnapshotContent it is the single
	// aggregate; on Snapshot and domain CRs it mirrors the bound SnapshotContent.Ready plus a bubbled
	// domain phase=Failed reason/message.
	ConditionReady = "Ready"
)

const (
	// ReasonArtifactMissing: a required data artifact is missing.
	ReasonArtifactMissing = "ArtifactMissing"

	// ReasonCompleted: terminal success reason (Ready=True).
	ReasonCompleted = "Completed"

	// ReasonCreateChildFailed: terminal Ready=False — creating a child snapshot failed.
	ReasonCreateChildFailed = "CreateChildFailed"

	// ReasonGraphPlanningFailed: terminal Ready=False — graph planning failed.
	ReasonGraphPlanningFailed = "GraphPlanningFailed"

	// ReasonChildrenLinkPending: Ready=False (non-terminal) — a namespace-root content has declared child
	// snapshots (in particular the orphan/residual-PVC volume leaves) that are not yet linked into
	// status.childrenSnapshotContentRefs. ChildrenReady is held fail-closed at this reason until every
	// declared child content edge is present, so a consumer never observes the first Ready=True before the
	// orphan data is captured and linked. This subsumes the former residual/orphan-PVC capture latch gate.
	ReasonChildrenLinkPending = "ChildrenLinkPending"

	// ReasonChildSnapshotDeleted: Ready=False (NON-terminal, recoverable) — a declared child snapshot CR
	// of an already-captured snapshot was deleted while its child SnapshotContent survives in the recycle
	// bin (the child content is alive, its status.parentDeleted=true, and the parent snapshot is
	// still alive). The captured data is intact — only the namespaced user surface (d8 download, which
	// reads namespaced CRs) degrades, while the durable SnapshotContent tree stays Ready. The message names
	// the deleted child and notes its content survives; the automated restore path is not yet defined
	// (recovery is possible by manual intervention). Detected and folded onto the owner mirror by the
	// SnapshotContentController; because it is a namespaced-surface degradation it is deliberately NOT in
	// TerminalReadyReasons.
	ReasonChildSnapshotDeleted = "ChildSnapshotDeleted"

	// ReasonChildSnapshotLost: TERMINAL Ready=False — a declared child snapshot is unrecoverably gone. Its
	// child SnapshotContent is absent (content names are UID-derived, so a recreated child can never relink
	// into the immutable frozen childrenSnapshotContentRefs edge set), OR its CR vanished after the node
	// froze its plan (phase>=Planned) while the capture was still incomplete (a surviving but not-Ready
	// content cannot resume capture), OR — pre-Planned — a declared child's CR and its source object both
	// vanished so re-planning cannot recreate it. A new snapshot is required. In TerminalReadyReasons.
	ReasonChildSnapshotLost = "ChildSnapshotLost"
)

// TerminalReadyReasons is the canonical set of Ready=False reasons treated as terminal capture failure
// across all snapshot kinds. It is the single source of truth consumed by the core planner, the
// wave-barrier, the namespace-capture RBAC hook, the domain SDK (CoreCaptureOutcome), and tests.
//
// It includes the manifest-phase terminals (ManifestCheckpointFailed, ChildrenFailed,
// GraphPlanningFailed, CreateChildFailed) — a manifest-phase failure surfaces on the root Ready as one
// of these, NOT via a latch condition. The reason string values are defined canonically in core
// pkg/snapshot; they are listed here as literals to keep the api module dependency-free. Domain-supplied
// reasons (e.g. SourceNotFound) are free-form and NOT in this set.
var TerminalReadyReasons = map[string]struct{}{
	"ListFailed":               {},
	"ManifestCheckpointFailed": {},
	"NamespaceNotFound":        {},
	"VolumeCaptureFailed":      {},
	"DuplicateCoveredPVCUID":   {},
	"ChildrenFailed":           {},
	ReasonGraphPlanningFailed:  {},
	ReasonCreateChildFailed:    {},
	ReasonChildSnapshotLost:    {},
}

// IsReasonTerminal reports whether a Ready=False reason is terminal (unrecoverable for this snapshot;
// spec is immutable, so a new snapshot is required). It is the canonical terminal classifier.
func IsReasonTerminal(reason string) bool {
	_, ok := TerminalReadyReasons[reason]
	return ok
}
