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

// +kubebuilder:subresource:status
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=sir
// +kubebuilder:printcolumn:name="Snapshot",type=string,JSONPath=`.spec.rootSnapshotName`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
type SnapshotImportRequest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SnapshotImportRequestSpec   `json:"spec"`
	Status SnapshotImportRequestStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type SnapshotImportRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SnapshotImportRequest `json:"items"`
}

// +k8s:deepcopy-gen=true
type SnapshotImportRequestSpec struct {
	// RootSnapshotName is the name of the root Snapshot to be created in the request namespace.
	// +kubebuilder:validation:MinLength=1
	RootSnapshotName string `json:"rootSnapshotName"`

	// Nodes describes the snapshot tree to be recreated, ordered leaf-to-root.
	// Each entry mirrors a NodeRecord from the archive index.
	Nodes []ImportNode `json:"nodes"`

	// Volumes lists per-volume staging PVC references populated by the CLI
	// after DataImport materialises each volume into a PVC.
	Volumes []ImportVolume `json:"volumes,omitempty"`

	// StorageClassMapping optionally remaps storage class names from the archive
	// to target cluster storage classes (source -> target).
	StorageClassMapping map[string]string `json:"storageClassMapping,omitempty"`

	// TTL is the retention duration for the root ObjectKeeper (e.g. "168h").
	// Empty means no TTL (keep until explicit deletion).
	TTL string `json:"ttl,omitempty"`
}

// +k8s:deepcopy-gen=true
type ImportNode struct {
	// ID is the stable node identifier from the archive (matches NodeRecord.id).
	// +kubebuilder:validation:MinLength=1
	ID string `json:"id"`

	// APIVersion of the snapshot object to create (e.g. "state-snapshotter.deckhouse.io/v1alpha1").
	// +kubebuilder:validation:MinLength=1
	APIVersion string `json:"apiVersion"`

	// Kind of the snapshot object to create (e.g. "Snapshot", "DemoVirtualMachineSnapshot").
	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`

	// Name is the desired name for the snapshot object.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// ParentID is the ID of the parent node, or empty for the root node.
	ParentID string `json:"parentId,omitempty"`

	// Children lists the IDs of immediate child nodes.
	Children []string `json:"children,omitempty"`

	// HasData is true when this node has volume data that needs to be imported via VCR.
	HasData bool `json:"hasData"`
}

// +k8s:deepcopy-gen=true
type ImportVolume struct {
	// NodeID references the ImportNode.ID this volume belongs to.
	// +kubebuilder:validation:MinLength=1
	NodeID string `json:"nodeId"`

	// PVCName is the name of the original PersistentVolumeClaim (from the archive).
	// +kubebuilder:validation:MinLength=1
	PVCName string `json:"pvcName"`

	// VolumeMode is the volume mode: "Block" or "Filesystem".
	// +kubebuilder:validation:Enum=Block;Filesystem
	VolumeMode string `json:"volumeMode"`

	// StagingPVCName is the name of the PVC populated by DataImport on the target cluster.
	// The CLI sets this field after DataImport completes; the controller waits for it before
	// creating the VolumeCaptureRequest.
	StagingPVCName string `json:"stagingPvcName,omitempty"`
}

// +k8s:deepcopy-gen=true
type SnapshotImportRequestStatus struct {
	// Phase is a high-level summary of progress.
	// +kubebuilder:validation:Enum=Pending;Importing;Ready;Failed
	Phase SnapshotImportPhase `json:"phase,omitempty"`

	// CreatedSnapshotName is the name of the root Snapshot object that was created.
	CreatedSnapshotName string `json:"createdSnapshotName,omitempty"`

	// NodeProgress contains per-node status summary.
	NodeProgress []ImportNodeProgress `json:"nodeProgress,omitempty"`

	// Conditions represent the latest available observations of the request state.
	// Expected condition types: Ready.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// SnapshotImportPhase is the high-level lifecycle phase of a SnapshotImportRequest.
type SnapshotImportPhase string

const (
	SnapshotImportPhasePending   SnapshotImportPhase = "Pending"
	SnapshotImportPhaseImporting SnapshotImportPhase = "Importing"
	SnapshotImportPhaseReady     SnapshotImportPhase = "Ready"
	SnapshotImportPhaseFailed    SnapshotImportPhase = "Failed"
)

// +k8s:deepcopy-gen=true
type ImportNodeProgress struct {
	// ID is the node identifier.
	ID string `json:"id"`

	// Phase is the per-node phase.
	Phase string `json:"phase,omitempty"`

	// Message provides a human-readable status message.
	Message string `json:"message,omitempty"`
}
