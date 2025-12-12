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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=mcpchunk
// +kubebuilder:printcolumn:name="Checkpoint",type=string,JSONPath=`.spec.checkpointName`
// +kubebuilder:printcolumn:name="Index",type=integer,JSONPath=`.spec.index`
// +kubebuilder:printcolumn:name="Objects",type=integer,JSONPath=`.spec.objectsCount`
type ManifestCheckpointContentChunk struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ManifestCheckpointContentChunkSpec `json:"spec"`
}

// +kubebuilder:object:root=true
type ManifestCheckpointContentChunkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ManifestCheckpointContentChunk `json:"items"`
}

// +k8s:deepcopy-gen=true
type ManifestCheckpointContentChunkSpec struct {
	// CheckpointName is the name of the ManifestCheckpoint this chunk belongs to
	CheckpointName string `json:"checkpointName"`

	// Index is the 0-based index of this chunk
	Index int `json:"index"`

	// Data contains the gzipped JSON array of objects, base64 encoded
	// Format: base64(gzip(json[]))
	// +kubebuilder:validation:MaxLength=1048576
	Data string `json:"data"`

	// ObjectsCount is the number of objects in this chunk
	ObjectsCount int `json:"objectsCount"`

	// Checksum is the SHA256 hash of the compressed chunk data (base64 encoded)
	// Used for integrity validation and debugging
	Checksum string `json:"checksum,omitempty"`
}

