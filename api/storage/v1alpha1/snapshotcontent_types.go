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
	"k8s.io/apimachinery/pkg/types"
)

// DeletionPolicy values for SnapshotContent.spec.deletionPolicy.
const (
	SnapshotContentDeletionPolicyRetain = "Retain"
	SnapshotContentDeletionPolicyDelete = "Delete"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=stsnapct
// SnapshotContent holds the result of a snapshot (shared carrier for multiple snapshot root kinds).
type SnapshotContent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SnapshotContentSpec   `json:"spec,omitempty"`
	Status SnapshotContentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type SnapshotContentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SnapshotContent `json:"items"`
}

// +k8s:deepcopy-gen=true
type SnapshotContentSpec struct {
	// SnapshotRef points to the snapshot root (e.g. NamespaceSnapshot). Kind must match the root CRD.
	// Current runtime uses it to finish content aggregation; a later refactor should remove this dependency.
	SnapshotRef SnapshotSubjectRef `json:"snapshotRef"`

	// BackupRepositoryName optional; used when snapshot class resolves to a backup repository.
	BackupRepositoryName string `json:"backupRepositoryName,omitempty"`

	// DeletionPolicy controls whether the controller may delete this SnapshotContent when the root snapshot is removed.
	// +kubebuilder:validation:Enum=Retain;Delete
	DeletionPolicy string `json:"deletionPolicy,omitempty"`
}

// +k8s:deepcopy-gen=true
type SnapshotSubjectRef struct {
	APIVersion string    `json:"apiVersion"`
	Kind       string    `json:"kind"`
	Name       string    `json:"name"`
	Namespace  string    `json:"namespace,omitempty"`
	UID        types.UID `json:"uid,omitempty"`
}

// SnapshotDataRef points to a durable data artifact produced by the data path.
// It MUST reference a final artifact such as VolumeSnapshotContent or equivalent.
// It MUST NOT reference execution requests such as VolumeCaptureRequest.
// v0 intended target: VolumeSnapshotContent or equivalent final artifact.
// +k8s:deepcopy-gen=true
type SnapshotDataRef struct {
	// +kubebuilder:validation:MinLength=1
	APIVersion string `json:"apiVersion"`
	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// +k8s:deepcopy-gen=true
type SnapshotContentStatus struct {
	// ManifestCheckpointName is the cluster-scoped ManifestCheckpoint name once manifest capture has persisted.
	// +optional
	ManifestCheckpointName string `json:"manifestCheckpointName,omitempty"`

	// ChildrenSnapshotContentRefs lists direct child SnapshotContent objects in the snapshot tree.
	// +optional
	ChildrenSnapshotContentRefs []SnapshotContentChildRef `json:"childrenSnapshotContentRefs,omitempty"`

	// DataRef optionally references a durable data artifact produced by the data path.
	// It MUST NOT reference an execution request such as VolumeCaptureRequest/DataExport.
	// +optional
	DataRef *SnapshotDataRef `json:"dataRef,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
