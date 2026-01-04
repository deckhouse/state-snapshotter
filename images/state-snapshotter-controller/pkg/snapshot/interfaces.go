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

// SnapshotLike is a typed interface for any XxxxSnapshot resource
// This allows the common controller to work with any snapshot type
// without using dynamic client or JSONPath
type SnapshotLike interface {
	runtime.Object
	metav1.Object

	// GetSpecSnapshotRef returns the reference to the source object (if any)
	// Returns nil if not applicable
	GetSpecSnapshotRef() *ObjectRef

	// GetStatusContentName returns the name of the associated SnapshotContent
	GetStatusContentName() string

	// GetStatusManifestCaptureRequestName returns the name of MCR
	GetStatusManifestCaptureRequestName() string

	// GetStatusVolumeCaptureRequestName returns the name of VCR (if any)
	// Returns empty string if not applicable
	GetStatusVolumeCaptureRequestName() string

	// GetStatusChildrenSnapshotRefs returns the list of child snapshot references
	GetStatusChildrenSnapshotRefs() []ObjectRef

	// GetStatusConditions returns the current conditions
	GetStatusConditions() []metav1.Condition

	// SetStatusConditions sets the conditions
	SetStatusConditions([]metav1.Condition)

	// GetStatusDataConsistency returns data consistency level (if any)
	GetStatusDataConsistency() string

	// GetStatusDataSnapshotMethod returns snapshot method (if any)
	GetStatusDataSnapshotMethod() string

	// IsNamespaced returns true if this is a namespaced resource
	IsNamespaced() bool
}

// SnapshotContentLike is a typed interface for any XxxxSnapshotContent resource
type SnapshotContentLike interface {
	runtime.Object
	metav1.Object

	// GetSpecSnapshotRef returns the reference to the Snapshot
	GetSpecSnapshotRef() *ObjectRef

	// GetStatusManifestCheckpointName returns the name of ManifestCheckpoint
	GetStatusManifestCheckpointName() string

	// GetStatusDataRef returns the data reference (VSC or PV)
	// Returns nil if not applicable
	GetStatusDataRef() *ObjectRef

	// GetStatusChildrenSnapshotContentRefs returns the list of child SnapshotContent references
	GetStatusChildrenSnapshotContentRefs() []ObjectRef

	// GetStatusConditions returns the current conditions
	GetStatusConditions() []metav1.Condition

	// SetStatusConditions sets the conditions
	SetStatusConditions([]metav1.Condition)

	// GetStatusDataConsistency returns data consistency level (copied from Snapshot)
	GetStatusDataConsistency() string

	// GetStatusDataSnapshotMethod returns snapshot method (copied from Snapshot)
	GetStatusDataSnapshotMethod() string
}

