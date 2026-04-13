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
// +kubebuilder:resource:scope=Cluster,shortName=nssnapct
// NamespaceSnapshotContent holds the materialized result for a NamespaceSnapshot root (not generic SnapshotContent).
type NamespaceSnapshotContent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NamespaceSnapshotContentSpec   `json:"spec,omitempty"`
	Status NamespaceSnapshotContentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type NamespaceSnapshotContentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NamespaceSnapshotContent `json:"items"`
}

// +k8s:deepcopy-gen=true
type NamespaceSnapshotContentSpec struct {
	// NamespaceSnapshotRef points to the namespaced NamespaceSnapshot root.
	NamespaceSnapshotRef SnapshotSubjectRef `json:"namespaceSnapshotRef"`

	// BackupRepositoryName optional; used when snapshot class resolves to a backup repository.
	BackupRepositoryName string `json:"backupRepositoryName,omitempty"`

	// DeletionPolicy controls whether the controller may delete this NamespaceSnapshotContent when the root is removed.
	// +kubebuilder:validation:Enum=Retain;Delete
	DeletionPolicy string `json:"deletionPolicy,omitempty"`
}

// +k8s:deepcopy-gen=true
type NamespaceSnapshotContentStatus struct {
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
