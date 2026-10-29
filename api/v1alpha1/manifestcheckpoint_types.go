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

// ManifestCheckpoint is the immutable checkpoint of manifest state created for a
// ManifestCaptureRequest.

// Stores an immutable checkpoint of captured object manifests.
// +kubebuilder:subresource:status
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=mcp
// +kubebuilder:metadata:labels=module=state-snapshotter
// +kubebuilder:printcolumn:name="Objects",type=integer,JSONPath=`.status.totalObjects`
// +kubebuilder:printcolumn:name="Size",type=string,JSONPath=`.status.totalSizeBytes`
// +crd-enricher:raw:properties.apiVersion.x-doc-skip=true
// +crd-enricher:raw:properties.kind.x-doc-skip=true
// +crd-enricher:raw:properties.metadata.x-doc-skip=true
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

// ManifestCheckpointSpec is intentionally empty.
//
// A ManifestCheckpoint is a name-addressed, controller-created artifact: it is looked up by name
// (from SnapshotContent and the aggregated API), never by a spec field. Source provenance — the
// name of the originating (short-lived) ManifestCaptureRequest — is recorded on the
// state-snapshotter.deckhouse.io/source-request label. There is deliberately no source-namespace
// field or label so the type stays correct for future cluster-wide capture sources.

// No user-configurable fields.
// +k8s:deepcopy-gen=true
type ManifestCheckpointSpec struct {
}

// Current storage and readiness state of the checkpoint.
// +k8s:deepcopy-gen=true
type ManifestCheckpointStatus struct {
	// Chunks contains information about all chunks

	// Stored chunks that make up the checkpoint.
	Chunks []ChunkInfo `json:"chunks,omitempty"`

	// TotalObjects is the total number of objects captured

	// Total number of captured objects.
	TotalObjects int `json:"totalObjects,omitempty"`

	// TotalSizeBytes is the total size of all chunks in bytes (compressed)

	// Total compressed size of all chunks in bytes.
	TotalSizeBytes int64 `json:"totalSizeBytes,omitempty"`

	// Conditions represent the latest available observations of the checkpoint state
	// Expected condition types: Ready

	// Current checkpoint conditions.
	// +crd-enricher:raw:items.description="Current condition."
	// +crd-enricher:raw:items.properties.lastTransitionTime.description="Time of the last condition status change."
	// +crd-enricher:raw:items.properties.message.description="Human-readable details about the condition."
	// +crd-enricher:raw:items.properties.observedGeneration.description="Object generation observed when this condition was set."
	// +crd-enricher:raw:items.properties.reason.description="Programmatic reason for the last condition transition."
	// +crd-enricher:raw:items.properties.status.description="Condition status."
	// +crd-enricher:raw:items.properties.type.description="Condition type."
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Information about one stored checkpoint chunk.
// +k8s:deepcopy-gen=true
type ChunkInfo struct {
	// Name of the chunk

	// Name of the ManifestCheckpointContentChunk object.
	Name string `json:"name"`

	// Index of the chunk (0-based)

	// Zero-based chunk index.
	Index int `json:"index"`

	// ObjectsCount is the number of objects in this chunk

	// Number of objects in this chunk.
	ObjectsCount int `json:"objectsCount"`

	// SizeBytes is the size of this chunk in bytes (compressed)

	// Compressed size of this chunk in bytes.
	SizeBytes int64 `json:"sizeBytes"`

	// Checksum is the SHA256 hash of the compressed chunk data (base64 encoded)
	// Used for integrity validation and debugging

	// SHA-256 checksum of the compressed chunk data, encoded in Base64.
	Checksum string `json:"checksum,omitempty"`
}
