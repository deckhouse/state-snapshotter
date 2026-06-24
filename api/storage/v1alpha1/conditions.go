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
// (ManifestsReady/VolumesReady/ChildrenReady) and the broader reason taxonomy stay in core pkg/snapshot.
const (
	// ConditionReady indicates the object is ready for use. On SnapshotContent it is the single
	// aggregate; on Snapshot it mirrors the bound SnapshotContent.Ready.
	ConditionReady = "Ready"

	// ConditionChildrenSnapshotReady is the domain planning gate/barrier on a snapshot: the domain
	// controller finished planning child snapshots. It is NOT part of the Ready formula; readers must
	// require observedGeneration == generation.
	ConditionChildrenSnapshotReady = "ChildrenSnapshotReady"
)

const (
	// ReasonArtifactMissing: a required data artifact is missing.
	ReasonArtifactMissing = "ArtifactMissing"

	// ReasonCompleted: terminal success reason (Ready=True / ChildrenSnapshotReady=True).
	ReasonCompleted = "Completed"

	// ReasonCreateChildFailed: ChildrenSnapshotReady=False — creating a child snapshot failed.
	ReasonCreateChildFailed = "CreateChildFailed"

	// ReasonGraphPlanningFailed: ChildrenSnapshotReady=False — graph planning failed.
	ReasonGraphPlanningFailed = "GraphPlanningFailed"
)
