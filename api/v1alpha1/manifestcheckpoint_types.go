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
// +kubebuilder:resource:scope=Cluster,shortName=mcp
// +kubebuilder:metadata:labels=module=state-snapshotter
// +kubebuilder:printcolumn:name="Objects",type=integer,JSONPath=`.status.totalObjects`
// +kubebuilder:printcolumn:name="Size",type=string,JSONPath=`.status.totalSizeBytes`
type ManifestCheckpoint struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ManifestCheckpointSpec   `json:"spec"`
	Status ManifestCheckpointStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ManifestCheckpointList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ManifestCheckpoint `json:"items"`
}

// +k8s:deepcopy-gen=true
type ManifestCheckpointSpec struct {
	// SourceNamespace is the namespace of the original ManifestCaptureRequest. It is the only durable
	// source-provenance field: the originating ManifestCaptureRequest is short-lived (deleted right after
	// the checkpoint is created) and never resolved by ref, so the checkpoint carries no back-reference to
	// it. The originating request name is recorded on the state-snapshotter.deckhouse.io/source-request
	// label instead.
	// +kubebuilder:validation:Required
	SourceNamespace string `json:"sourceNamespace"`
}

// +k8s:deepcopy-gen=true
type ManifestCheckpointStatus struct {
	// Chunks contains information about all chunks
	Chunks []ChunkInfo `json:"chunks,omitempty"`

	// TotalObjects is the total number of objects captured
	TotalObjects int `json:"totalObjects,omitempty"`

	// TotalSizeBytes is the total size of all chunks in bytes (compressed)
	TotalSizeBytes int64 `json:"totalSizeBytes,omitempty"`

	// Conditions represent the latest available observations of the checkpoint state
	// Expected condition types: Ready
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +k8s:deepcopy-gen=true
type ChunkInfo struct {
	// Name of the chunk
	Name string `json:"name"`

	// Index of the chunk (0-based)
	Index int `json:"index"`

	// ObjectsCount is the number of objects in this chunk
	ObjectsCount int `json:"objectsCount"`

	// SizeBytes is the size of this chunk in bytes (compressed)
	SizeBytes int64 `json:"sizeBytes"`

	// Checksum is the SHA256 hash of the compressed chunk data (base64 encoded)
	// Used for integrity validation and debugging
	Checksum string `json:"checksum,omitempty"`
}
