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

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:subresource:status
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=mcr
// +kubebuilder:printcolumn:name="Checkpoint",type=string,JSONPath=`.status.checkpointName`
type ManifestCaptureRequest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ManifestCaptureRequestSpec   `json:"spec"`
	Status ManifestCaptureRequestStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ManifestCaptureRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ManifestCaptureRequest `json:"items"`
}

// +k8s:deepcopy-gen=true
type ManifestCaptureRequestSpec struct {
	// Targets specifies the objects to capture.
	// All targets must be namespaced objects in the same namespace as the ManifestCaptureRequest, with a
	// single exception: the capture's own Namespace object (core v1 Namespace whose name equals the
	// ManifestCaptureRequest namespace) is the only allowed cluster-scoped target.
	//
	// Optional (may be empty): an MCR with no targets is a valid EMPTY capture. It is produced by the
	// namespace-root aggregator when a namespace has no allowlisted objects to capture (a single-object
	// domain snapshot always passes its own source identity, so it never hits this case). The executor
	// (ManifestCheckpointController) already handles an empty target set end-to-end — it writes a single
	// empty content chunk and marks the ManifestCheckpoint Ready=Completed — so an empty MCR converges to
	// an empty, Ready MCP and the owning SnapshotContent becomes ManifestsReady=True. This field is
	// therefore NOT required: the SDK sends a null/omitted targets for the empty aggregator MCR, and a
	// required constraint would reject that create with "spec.targets: Required value".
	// +optional
	Targets []ManifestTarget `json:"targets,omitempty"`
}

// +k8s:deepcopy-gen=true
type ManifestTarget struct {
	// APIVersion of the target object
	APIVersion string `json:"apiVersion"`
	// Kind of the target object
	// Cluster-scoped resources (Node, PersistentVolume, ClusterRole, etc.) are NOT allowed, with a single
	// exception: the core v1 Namespace whose name equals the ManifestCaptureRequest namespace (the capture's
	// own Namespace object).
	Kind string `json:"kind"`
	// Name of the target object
	Name string `json:"name"`
	// Namespace is not specified, it's implied to be the same as ManifestCaptureRequest namespace
}

// +k8s:deepcopy-gen=true
type ManifestCaptureRequestStatus struct {
	// CheckpointName is the name of the cluster-scoped ManifestCheckpoint
	CheckpointName string `json:"checkpointName,omitempty"`

	// CompletionTimestamp is when the request was completed
	CompletionTimestamp *metav1.Time `json:"completionTimestamp,omitempty"`

	// Conditions represent the latest available observations of the request state
	// Only Ready condition is used - it is set to True on success or False on final failure
	// Ready condition is set only in final state (terminal success or terminal failure)
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
