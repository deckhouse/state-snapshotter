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
// +kubebuilder:resource:scope=Namespaced,shortName=nssnap
// +kubebuilder:printcolumn:name="Content",type=string,JSONPath=`.status.boundSnapshotContentName`
// NamespaceSnapshot requests a namespace state/configuration snapshot (MVP: design namespace-snapshot-controller.md).
type NamespaceSnapshot struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NamespaceSnapshotSpec   `json:"spec,omitempty"`
	Status NamespaceSnapshotStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type NamespaceSnapshotList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NamespaceSnapshot `json:"items"`
}

// +k8s:deepcopy-gen=true
type NamespaceSnapshotSpec struct {
	// SnapshotClassName optionally selects class/policy (aligned with unified snapshot model; resolution is N2+).
	SnapshotClassName string `json:"snapshotClassName,omitempty"`
}

// +k8s:deepcopy-gen=true
type NamespaceSnapshotStatus struct {
	// ObservedGeneration is the metadata.generation the controller last reconciled into this status.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// BoundSnapshotContentName is the cluster-scoped name of the bound snapshot content object for this root.
	// The content kind is defined by the snapshot line (e.g. NamespaceSnapshotContent), not by this field name.
	BoundSnapshotContentName string `json:"boundSnapshotContentName,omitempty"`

	// ChildrenSnapshotRefs lists child NamespaceSnapshot roots in the manifests-only tree (N2b).
	// Populated by the controller in PR2+; PR1 adds the field only (implementation-plan §2.4.2).
	// +optional
	ChildrenSnapshotRefs []NamespaceSnapshotChildRef `json:"childrenSnapshotRefs,omitempty"`

	// Conditions represent the latest observations (Ready, Bound, Failed, etc.).
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
