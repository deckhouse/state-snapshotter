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

// Types below are a verbatim mirror of the upstream ObjectKeeper API. Keep struct
// fields and json tags identical to the source (see doc.go for path + version).

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
type ObjectKeeper struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ObjectKeeperSpec   `json:"spec"`
	Status ObjectKeeperStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ObjectKeeperList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ObjectKeeper `json:"items"`
}

type ObjectKeeperSpec struct {
	// Mode controls retention behavior
	Mode string `json:"mode"`

	// FollowObjectRef references the namespaced object that controls retention
	// Required when mode = FollowObject or FollowObjectWithTTL
	// The ObjectKeeper will be garbage collected when the referenced object is deleted
	// (or after TTL expires if mode = FollowObjectWithTTL)
	FollowObjectRef *FollowObjectRef `json:"followObjectRef,omitempty"`

	// TTL specifies how long the ObjectKeeper must live
	// Required when mode = TTL or FollowObjectWithTTL
	// The ObjectKeeper will expire after this duration
	// For FollowObjectWithTTL: TTL starts counting from object deletion time
	TTL *metav1.Duration `json:"ttl,omitempty"`
}

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
	// Used by ObjectKeeperController to detect object deletion or recreation
	UID string `json:"uid"`
}

type ObjectKeeperPhase string

const (
	// PhasePending indicates that the ObjectKeeper cannot be processed yet,
	// most likely due to missing or invalid configuration (e.g., TTL or FollowObjectRef is not set).
	PhasePending ObjectKeeperPhase = "Pending"

	// PhaseTracking means the ObjectKeeper is actively tracking the referenced object,
	// and the object exists with a matching UID.
	PhaseTracking ObjectKeeperPhase = "Tracking"

	// PhaseExpiring indicates that the ObjectKeeper is waiting for the TTL to expire,
	// typically after the referenced object was deleted or its UID no longer matches.
	PhaseExpiring ObjectKeeperPhase = "Expiring"
)

type ObjectKeeperStatus struct {
	// Phase of the objectkeeper
	Phase ObjectKeeperPhase `json:"phase,omitempty"`

	// Conditions represent the latest available observations of the objectkeeper state
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Message provides additional information about the status
	Message string `json:"message,omitempty"`

	// Timestamp when the referenced object was no longer found (deleted or UID mismatch).
	LostAt *metav1.Time `json:"lostAt,omitempty"`
}
