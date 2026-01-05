/*
Copyright 2025 Flant JSC

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
)

// ObjectRef represents a reference to a Kubernetes object
type ObjectRef struct {
	Kind      string
	Name      string
	Namespace string // Only for namespaced resources
}

// SnapshotLike is a typed interface for any XxxxSnapshot resource.
//
// This interface is a formal contract defined in unified-snapshots-test-plan.md.
// It allows the common controller to work with any snapshot type without using
// dynamic client or JSONPath.
//
// IMPORTANT: Interface Stability Contract
//
// This interface MUST NOT be changed without updating the architectural documents:
//   - unified-snapshots-test-plan.md (PACKAGE INTERFACES section)
//   - unified-snapshots-architecture-diagrams.md (GLOBAL INVARIANTS)
//
// Changes to this interface require:
//   1. Architectural justification
//   2. Test plan update
//   3. Backward compatibility consideration
//
// Contract Rules:
//   - Getter methods MUST be pure functions (no side effects, no mutations)
//   - Getter methods MUST be idempotent
//   - Setter methods (SetStatusConditions) MAY have side effects
//   - Interface MUST remain stable across implementation refactoring
//
// See: unified-snapshots-test-plan.md (TESTING PHILOSOPHY, PACKAGE INTERFACES)
type SnapshotLike interface {
	runtime.Object
	metav1.Object

	// GetSpecSnapshotRef returns the reference to the parent Snapshot (if any).
	// Returns nil if this is a root snapshot.
	// Contract: Pure function, idempotent, no side effects.
	GetSpecSnapshotRef() *ObjectRef

	// GetStatusContentName returns the name of the associated SnapshotContent.
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
// This interface is a formal contract defined in unified-snapshots-test-plan.md.
//
// IMPORTANT: Interface Stability Contract
//
// This interface MUST NOT be changed without updating the architectural documents:
//   - unified-snapshots-test-plan.md (PACKAGE INTERFACES section)
//   - unified-snapshots-architecture-diagrams.md (GLOBAL INVARIANTS)
//
// Changes to this interface require:
//   1. Architectural justification
//   2. Test plan update
//   3. Backward compatibility consideration
//
// Contract Rules:
//   - Getter methods MUST be pure functions (no side effects, no mutations)
//   - Getter methods MUST be idempotent
//   - Setter methods (SetStatusConditions) MAY have side effects
//   - Interface MUST remain stable across implementation refactoring
//
// See: unified-snapshots-test-plan.md (TESTING PHILOSOPHY, PACKAGE INTERFACES)
type SnapshotContentLike interface {
	runtime.Object
	metav1.Object

	// GetSpecSnapshotRef returns the reference to the Snapshot.
	// Contract: Pure function, idempotent, no side effects.
	GetSpecSnapshotRef() *ObjectRef

	// GetStatusManifestCheckpointName returns the name of ManifestCheckpoint.
	// Contract: Pure function, idempotent, no side effects.
	GetStatusManifestCheckpointName() string

	// GetStatusDataRef returns the data reference (VSC or PV).
	// Returns nil if not applicable.
	// Contract: Pure function, idempotent, no side effects.
	GetStatusDataRef() *ObjectRef

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

