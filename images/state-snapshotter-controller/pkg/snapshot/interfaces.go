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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// ObjectRef aliases the canonical contract type in api/storage so core and the domain controller
// share one definition via api/.
type ObjectRef = storagev1alpha1.ObjectRef

// DataBindingRef is one PVC target to durable data artifact binding on SnapshotContent.
type DataBindingRef struct {
	TargetUID string
	Target    ObjectRef
	Artifact  ObjectRef
	// VolumeMode/FsType/StorageClassName mirror SnapshotContent.status.dataRefs[]
	// volume metadata. They are persisted on the binding because CSI snapshots are mode-agnostic;
	// the export/index path needs them to recreate the volume faithfully. All optional.
	VolumeMode       string
	FsType           string
	StorageClassName string
}

// SnapshotLike is a typed interface for any XxxxSnapshot resource.
//
// It allows the common controller to work with any snapshot type without using
// dynamic client or JSONPath.
//
// IMPORTANT: Interface Stability Contract
//
// Every snapshot kind implements this interface, including domain kinds whose controllers run
// out-of-process, so a change here breaks implementors outside this repository. Changes require
// architectural justification and explicit backward-compatibility consideration.
//
// Contract Rules:
//   - Getter methods MUST be pure functions (no side effects, no mutations)
//   - Getter methods MUST be idempotent
//   - Setter methods (SetStatusConditions) MAY have side effects
//   - Interface MUST remain stable across implementation refactoring
type SnapshotLike interface {
	runtime.Object
	metav1.Object

	// GetSpecSnapshotRef returns the reference to the parent Snapshot (if any).
	// Returns nil if this is a root snapshot.
	// Contract: Pure function, idempotent, no side effects.
	GetSpecSnapshotRef() *ObjectRef

	// GetStatusContentName returns status.boundSnapshotContentName (unified bind field for snapshot roots; actual content kind is pairing-specific).
	// Contract: Pure function, idempotent, no side effects.
	GetStatusContentName() string

	// GetStatusManifestCaptureRequestName returns the name of MCR.
	// Contract: Pure function, idempotent, no side effects.
	GetStatusManifestCaptureRequestName() string

	// GetStatusVolumeCaptureRequestName returns the name of VCR (if any).
	// Returns empty string if not applicable.
	// Contract: Pure function, idempotent, no side effects.
	GetStatusVolumeCaptureRequestName() string

	// GetStatusChildrenSnapshotRefs returns the list of child snapshot references.
	// This is the authoritative source for child snapshots.
	// Contract: Pure function, idempotent, no side effects.
	GetStatusChildrenSnapshotRefs() []ObjectRef

	// GetStatusConditions returns the current conditions.
	// Contract: Pure function, idempotent, no side effects.
	GetStatusConditions() []metav1.Condition

	// SetStatusConditions sets the conditions.
	// This is the ONLY setter method and MAY have side effects.
	// Contract: Idempotent, may modify object state.
	SetStatusConditions([]metav1.Condition)

	// GetStatusDataConsistency returns data consistency level (if any).
	// Contract: Pure function, idempotent, no side effects.
	GetStatusDataConsistency() string

	// GetStatusDataSnapshotMethod returns snapshot method (if any).
	// Contract: Pure function, idempotent, no side effects.
	GetStatusDataSnapshotMethod() string

	// IsNamespaced returns true if this is a namespaced resource.
	// Contract: Pure function, idempotent, no side effects.
	IsNamespaced() bool
}

// SnapshotContentLike is a typed interface for any XxxxSnapshotContent resource.
//
// IMPORTANT: Interface Stability Contract
//
// Every snapshot-content kind implements this interface, including domain kinds whose controllers run
// out-of-process, so a change here breaks implementors outside this repository. Changes require
// architectural justification and explicit backward-compatibility consideration.
//
// Contract Rules:
//   - Getter methods MUST be pure functions (no side effects, no mutations)
//   - Getter methods MUST be idempotent
//   - Setter methods (SetStatusConditions) MAY have side effects
//   - Interface MUST remain stable across implementation refactoring
type SnapshotContentLike interface {
	runtime.Object
	metav1.Object

	// GetSpecSnapshotRef returns the reference to the Snapshot.
	// Contract: Pure function, idempotent, no side effects.
	GetSpecSnapshotRef() *ObjectRef

	// GetStatusManifestCheckpointName returns the name of ManifestCheckpoint.
	// Contract: Pure function, idempotent, no side effects.
	GetStatusManifestCheckpointName() string

	// GetStatusDataRefs returns durable PVC to data artifact bindings (for example PVC -> VSC).
	// Artifacts must not point at execution requests such as VCR/DataExport.
	// Contract: Pure function, idempotent, no side effects.
	GetStatusDataRefs() []DataBindingRef

	// GetStatusChildrenSnapshotContentRefs returns the list of child SnapshotContent references.
	// This is the authoritative source for child SnapshotContent objects.
	// Contract: Pure function, idempotent, no side effects.
	GetStatusChildrenSnapshotContentRefs() []ObjectRef

	// GetStatusConditions returns the current conditions.
	// Contract: Pure function, idempotent, no side effects.
	GetStatusConditions() []metav1.Condition

	// SetStatusConditions sets the conditions.
	// This is the ONLY setter method and MAY have side effects.
	// Contract: Idempotent, may modify object state.
	SetStatusConditions([]metav1.Condition)

	// GetStatusDataConsistency returns data consistency level (copied from Snapshot).
	// Contract: Pure function, idempotent, no side effects.
	GetStatusDataConsistency() string

	// GetStatusDataSnapshotMethod returns snapshot method (copied from Snapshot).
	// Contract: Pure function, idempotent, no side effects.
	GetStatusDataSnapshotMethod() string
}
