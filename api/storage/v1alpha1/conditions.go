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
// (ManifestsReady/VolumeReady/ChildrenReady) and the broader reason taxonomy stay in core pkg/snapshot.
const (
	// ConditionReady indicates the object is ready for use. On SnapshotContent it is the single
	// aggregate; on Snapshot it mirrors the bound SnapshotContent.Ready.
	ConditionReady = "Ready"

	// ConditionPlanningReady is the planning gate/barrier on a snapshot: the node has been planned
	// (its child snapshot refs and own MCR/VCR have been published). It is NOT a literal "children are
	// ready" signal and is NOT part of the Ready formula; readers must require
	// observedGeneration == generation.
	ConditionPlanningReady = "PlanningReady"

	// ConditionManifestsArchived is a subtree latch on SnapshotContent (mirrored onto Snapshot and
	// domain XxxxSnapshot): the manifests for this node AND all descendant content nodes have been
	// captured into their ManifestCheckpoints at least once. It is a precursor/contract signal, NOT a
	// leg of the Ready formula (Ready = ManifestsReady && VolumeReady && ChildrenReady).
	//
	// Latch semantics (lifelong, never re-opens):
	//   - True  / ReasonManifestsArchived  — own manifest leg reached readiness once AND every child
	//     content is ManifestsArchived=True. Once True it stays True forever, immune to later
	//     ManifestsReady/Ready degradation or to a child disappearing (the fact "namespace was read
	//     and stored in a checkpoint" is irreversible). Snapshot.spec is immutable, so there is no
	//     recapture: observedGeneration is still stamped on the condition (like every other condition,
	//     for gen-gated readers) but the latch state never depends on a generation change.
	//   - False / ReasonManifestsCapturing — transient: not archived yet and not failed (includes the
	//     fail-closed NamespaceCaptureIncomplete wait for RBAC).
	//   - False / ReasonManifestsArchiveFailed — terminal: own manifest leg failed terminally BEFORE
	//     archiving, or a child is ManifestsArchived=Failed (the subtree can never be archived). With
	//     an immutable spec this state is unrecoverable for that Snapshot (a new one is required).
	//
	// Primary consumer: the namespace-capture RBAC hook, which grants the transient per-namespace
	// RoleBinding while a Snapshot still needs to read the live namespace (not yet Archived/Failed).
	ConditionManifestsArchived = "ManifestsArchived"
)

const (
	// ReasonArtifactMissing: a required data artifact is missing.
	ReasonArtifactMissing = "ArtifactMissing"

	// ReasonCompleted: terminal success reason (Ready=True / PlanningReady=True).
	ReasonCompleted = "Completed"

	// ReasonCreateChildFailed: PlanningReady=False — creating a child snapshot failed.
	ReasonCreateChildFailed = "CreateChildFailed"

	// ReasonGraphPlanningFailed: PlanningReady=False — graph planning failed.
	ReasonGraphPlanningFailed = "GraphPlanningFailed"

	// ReasonManifestsArchived: ManifestsArchived=True — this node and its whole subtree have had
	// their manifests captured into checkpoints at least once (lifelong latch).
	ReasonManifestsArchived = "Archived"

	// ReasonManifestsCapturing: ManifestsArchived=False (transient) — the subtree is not archived
	// yet and has not failed; capture is still in progress (or waiting for capture RBAC).
	ReasonManifestsCapturing = "Capturing"

	// ReasonManifestsArchiveFailed: ManifestsArchived=False (terminal) — the own manifest leg failed
	// terminally before archiving, or a descendant's manifests can never be archived.
	ReasonManifestsArchiveFailed = "Failed"

	// ReasonResidualVolumeCapturePending: Ready=False (non-terminal) — the namespace-root content has
	// finished its domain children but the final residual/orphan-PVC capture wave has not completed yet
	// (status.residualVolumeCapture.phase != Complete). The aggregate Ready is held at this reason so a
	// consumer never observes the first Ready=True before the orphan data is captured (fail-closed gate).
	ReasonResidualVolumeCapturePending = "ResidualVolumeCapturePending"
)
