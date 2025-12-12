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

package iretainer

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:subresource:status
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=iret
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="FollowObject",type=string,JSONPath=`.spec.followObjectRef.name`
type IRetainer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   IRetainerSpec   `json:"spec"`
	Status IRetainerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type IRetainerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IRetainer `json:"items"`
}

// +k8s:deepcopy-gen=true
type IRetainerSpec struct {
	// Mode controls retention behavior
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=FollowObject;TTL;FollowObjectWithTTL
	Mode string `json:"mode"`

	// FollowObjectRef references the namespaced object that controls retention
	// Required when mode = FollowObject or FollowObjectWithTTL
	// The Retainer will be garbage collected when the referenced object is deleted
	// (or after TTL expires if mode = FollowObjectWithTTL)
	FollowObjectRef *FollowObjectRef `json:"followObjectRef,omitempty"`

	// TTL specifies how long the Retainer must live
	// Required when mode = TTL or FollowObjectWithTTL
	// The Retainer will expire after this duration
	// For FollowObjectWithTTL: TTL starts counting from object deletion time
	TTL *metav1.Duration `json:"ttl,omitempty"`
}

// +k8s:deepcopy-gen=true
type FollowObjectRef struct {
	// APIVersion of the object to follow
	APIVersion string `json:"apiVersion"`

	// Kind of the object to follow
	Kind string `json:"kind"`

	// Namespace of the object to follow
	Namespace string `json:"namespace"`

	// Name of the object to follow
	Name string `json:"name"`

	// UID of the object to follow (required for verification)
	// Used by RetainerController to detect object deletion or recreation
	// +kubebuilder:validation:Required
	UID string `json:"uid"`
}

// +k8s:deepcopy-gen=true
type IRetainerStatus struct {
	// Conditions represent the latest available observations of the retainer state
	// Expected condition types: Active
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
