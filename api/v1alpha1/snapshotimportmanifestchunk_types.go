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

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=simchunk
// +kubebuilder:printcolumn:name="Request",type=string,JSONPath=`.spec.importRequestName`
// +kubebuilder:printcolumn:name="NodeID",type=string,JSONPath=`.spec.nodeId`
// +kubebuilder:printcolumn:name="Index",type=integer,JSONPath=`.spec.index`
// +kubebuilder:printcolumn:name="Objects",type=integer,JSONPath=`.spec.objectsCount`
type SnapshotImportManifestChunk struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec SnapshotImportManifestChunkSpec `json:"spec"`
}

// +kubebuilder:object:root=true
type SnapshotImportManifestChunkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SnapshotImportManifestChunk `json:"items"`
}

// +k8s:deepcopy-gen=true
type SnapshotImportManifestChunkSpec struct {
	// ImportRequestName is the name of the owning SnapshotImportRequest.
	// +kubebuilder:validation:MinLength=1
	ImportRequestName string `json:"importRequestName"`

	// NodeID identifies which node in the import tree this chunk belongs to.
	// +kubebuilder:validation:MinLength=1
	NodeID string `json:"nodeId"`

	// Index is the 0-based index of this chunk within the node's manifest set.
	Index int `json:"index"`

	// Total is the total number of chunks for this node's manifest set.
	// +kubebuilder:validation:Minimum=1
	Total int `json:"total"`

	// Data contains the gzipped JSON array of objects, base64 encoded.
	// Format: base64(gzip(json[]))
	// The controller re-encodes this into the canonical ManifestCheckpointContentChunk format.
	// +kubebuilder:validation:MaxLength=1048576
	Data string `json:"data"`

	// ObjectsCount is the number of objects in this chunk's data.
	ObjectsCount int `json:"objectsCount"`
}
