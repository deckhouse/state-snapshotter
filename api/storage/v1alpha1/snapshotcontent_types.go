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
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Manifests",type=string,JSONPath=`.status.conditions[?(@.type=="ManifestsReady")].status`
// +kubebuilder:printcolumn:name="Volumes",type=string,JSONPath=`.status.conditions[?(@.type=="VolumesReady")].status`
// +kubebuilder:printcolumn:name="Children",type=string,JSONPath=`.status.conditions[?(@.type=="ChildrenReady")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// SnapshotContent holds the result of a snapshot (shared carrier for multiple snapshot root kinds).
type SnapshotContent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="SnapshotContent spec is immutable"
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
	// DeletionPolicy controls whether the controller may delete this SnapshotContent when the root snapshot is removed.
	// +kubebuilder:validation:Enum=Retain;Delete
	DeletionPolicy string `json:"deletionPolicy,omitempty"`

	// SnapshotRef is an immutable back-reference to the owning Snapshot, mirroring
	// VolumeSnapshotContent.spec.volumeSnapshotRef. It is set at creation time (in particular
	// by the import path for pre-provisioned content) and enables the static-binding handshake:
	// a Snapshot with spec.source.snapshotContentName binds only when this ref points back at it.
	// The whole spec is immutable, so this ref cannot change after creation.
	// +optional
	SnapshotRef *SnapshotSubjectRef `json:"snapshotRef,omitempty"`
}

// +k8s:deepcopy-gen=true
type SnapshotSubjectRef struct {
	APIVersion string    `json:"apiVersion"`
	Kind       string    `json:"kind"`
	Name       string    `json:"name"`
	Namespace  string    `json:"namespace,omitempty"`
	UID        types.UID `json:"uid,omitempty"`
}

// SnapshotDataArtifactRef points to a durable data artifact produced by the data path.
// It MUST reference a final artifact such as VolumeSnapshotContent or equivalent.
// It MUST NOT reference execution requests such as VolumeCaptureRequest.
// +k8s:deepcopy-gen=true
type SnapshotDataArtifactRef struct {
	// +kubebuilder:validation:MinLength=1
	APIVersion string `json:"apiVersion"`
	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// SnapshotDataBinding associates a PVC target with its captured data artifact on one SnapshotContent.
// +k8s:deepcopy-gen=true
type SnapshotDataBinding struct {
	// TargetUID is the map key for status.dataRefs (PersistentVolumeClaim UID).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	TargetUID string `json:"targetUID"`

	// Target identifies the PVC (and related metadata) captured in MCP for this binding.
	Target SnapshotSubjectRef `json:"target"`

	// Artifact references the cluster-scoped durable data artifact (for example VolumeSnapshotContent).
	Artifact SnapshotDataArtifactRef `json:"artifact"`

	// VolumeMode records the source volume mode (Block or Filesystem). CSI snapshots are
	// mode-agnostic, so this is persisted here to drive the unified export (VolumeRestoreRequest)
	// and to recreate the PVC on import. Typed as a string to keep the api module dependency-free;
	// controllers convert it to corev1.PersistentVolumeMode.
	// +kubebuilder:validation:Enum=Block;Filesystem
	// +optional
	VolumeMode string `json:"volumeMode,omitempty"`

	// FsType records the source filesystem type (Filesystem volumes only).
	// +optional
	FsType string `json:"fsType,omitempty"`

	// AccessModes records the source PVC access modes (e.g. ReadWriteOnce, ReadWriteMany).
	// +optional
	AccessModes []string `json:"accessModes,omitempty"`

	// StorageClassName records the source StorageClass of the captured volume. Used by the
	// aggregated /index and by import StorageClass mapping.
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`

	// Size records the real allocated size of the captured volume, taken from the data artifact
	// (VolumeSnapshotContent.status.restoreSize). The snapshot outlives the source PVC, so the size MUST
	// be persisted here to recreate the volume on restore/export (the export VolumeRestoreRequest sizes
	// the target PVC from it). Stored as a resource.Quantity string (e.g. "10Gi") to keep the api module
	// dependency-free; controllers parse it via resource.ParseQuantity.
	// +optional
	Size string `json:"size,omitempty"`
}

// +k8s:deepcopy-gen=true
type SnapshotContentStatus struct {
	// ManifestCheckpointName is the cluster-scoped ManifestCheckpoint name once manifest capture has persisted.
	// +optional
	ManifestCheckpointName string `json:"manifestCheckpointName,omitempty"`

	// ChildrenSnapshotContentRefs lists direct child SnapshotContent objects in the snapshot tree.
	// +optional
	ChildrenSnapshotContentRefs []SnapshotContentChildRef `json:"childrenSnapshotContentRefs,omitempty"`

	// DataRefs lists PVC target to data artifact bindings for this logical snapshot node.
	// +listType=map
	// +listMapKey=targetUID
	// +optional
	DataRefs []SnapshotDataBinding `json:"dataRefs,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
