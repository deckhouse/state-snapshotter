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
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=csd
// +kubebuilder:printcolumn:name="Owner",type=string,JSONPath=`.spec.ownerModule`
// +kubebuilder:printcolumn:name="Accepted",type=string,JSONPath=`.status.conditions[?(@.type=="Accepted")].status`
// CustomSnapshotDefinition registers custom snapshot types for platform modules.
// See ADR: snapshot-rework/2026-01-23-unified-snapshots-registry.md
type CustomSnapshotDefinition struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CustomSnapshotDefinitionSpec   `json:"spec,omitempty"`
	Status CustomSnapshotDefinitionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type CustomSnapshotDefinitionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CustomSnapshotDefinition `json:"items"`
}

// +k8s:deepcopy-gen=true
type CustomSnapshotDefinitionSpec struct {
	// OwnerModule identifies the owning Deckhouse module (audit/RBAC); immutable in v1alpha1.
	// +kubebuilder:validation:Required
	OwnerModule string `json:"ownerModule"`

	// SnapshotResourceMapping declares source resource CRD -> snapshot CRD mappings.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	SnapshotResourceMapping []SnapshotResourceMappingEntry `json:"snapshotResourceMapping"`

	// ManifestTransformation optional webhook for manifest transforms for this CSD.
	ManifestTransformation *ManifestTransformation `json:"manifestTransformation,omitempty"`
}

// +k8s:deepcopy-gen=true
// SnapshotResourceMappingEntry maps resource CRDs to snapshot CRDs (plural.resource.group form).
type SnapshotResourceMappingEntry struct {
	// +kubebuilder:validation:Required
	ResourceCRDName string `json:"resourceCRDName"`
	// +kubebuilder:validation:Required
	SnapshotCRDName string `json:"snapshotCRDName"`
	// Priority is used when ordering orchestration across mappings (lower runs first).
	// +kubebuilder:validation:Minimum=0
	Priority int32 `json:"priority,omitempty"`
}

// +k8s:deepcopy-gen=true
type ManifestTransformation struct {
	// +kubebuilder:validation:Required
	ServiceRef ManifestTransformationServiceRef `json:"serviceRef"`
}

// +k8s:deepcopy-gen=true
type ManifestTransformationServiceRef struct {
	// +kubebuilder:validation:Required
	Name string `json:"name"`
	// +kubebuilder:validation:Required
	Namespace string `json:"namespace"`
}

// +k8s:deepcopy-gen=true
type CustomSnapshotDefinitionStatus struct {
	// Conditions include Accepted, RBACReady, Ready (see ADR).
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
