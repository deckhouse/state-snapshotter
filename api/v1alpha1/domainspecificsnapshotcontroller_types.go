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
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=dssc
// +kubebuilder:printcolumn:name="Owner",type=string,JSONPath=`.spec.ownerModule`
// +kubebuilder:printcolumn:name="Accepted",type=string,JSONPath=`.status.conditions[?(@.type=="Accepted")].status`
// DomainSpecificSnapshotController registers unified snapshot types for a platform module (DSC).
// See ADR: snapshot-rework/2026-01-23-unified-snapshots-registry.md
type DomainSpecificSnapshotController struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DomainSpecificSnapshotControllerSpec   `json:"spec,omitempty"`
	Status DomainSpecificSnapshotControllerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type DomainSpecificSnapshotControllerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DomainSpecificSnapshotController `json:"items"`
}

// +k8s:deepcopy-gen=true
type DomainSpecificSnapshotControllerSpec struct {
	// OwnerModule identifies the owning Deckhouse module (audit/RBAC); immutable in v1alpha1.
	// +kubebuilder:validation:Required
	OwnerModule string `json:"ownerModule"`

	// SnapshotResourceMapping declares supported snapshot types using CRD names only (v1alpha1).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	SnapshotResourceMapping []SnapshotResourceMappingEntry `json:"snapshotResourceMapping"`

	// ManifestTransformation optional webhook for manifest transforms for this DSC.
	ManifestTransformation *ManifestTransformation `json:"manifestTransformation,omitempty"`
}

// +k8s:deepcopy-gen=true
// SnapshotResourceMappingEntry maps resource / snapshot / content CRDs (plural.resource.group form).
type SnapshotResourceMappingEntry struct {
	// +kubebuilder:validation:Required
	ResourceCRDName string `json:"resourceCRDName"`
	// +kubebuilder:validation:Required
	SnapshotCRDName string `json:"snapshotCRDName"`
	// +kubebuilder:validation:Required
	ContentCRDName string `json:"contentCRDName"`
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
type DomainSpecificSnapshotControllerStatus struct {
	// Conditions include Accepted, RBACReady, Ready (see ADR).
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
