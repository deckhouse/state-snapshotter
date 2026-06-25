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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=demovd
// DemoVirtualDisk is the demo domain data resource. The domain controller materializes its backing PVC
// (a blank, freshly provisioned disk, or one restored from a DemoVirtualDiskSnapshot) and publishes real
// allocated capacity in status.
type DemoVirtualDisk struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              DemoVirtualDiskSpec   `json:"spec,omitempty"`
	Status            DemoVirtualDiskStatus `json:"status,omitempty"`
}

// +k8s:deepcopy-gen=true
// +kubebuilder:validation:XValidation:rule="!has(self.dataSource) || self.dataSource.kind == 'DemoVirtualDiskSnapshot'",message="dataSource must reference DemoVirtualDiskSnapshot (cloning from another DemoVirtualDisk is not supported)"
// +kubebuilder:validation:XValidation:rule="!has(self.dataSource) || !has(self.dataSource.apiGroup) || size(self.dataSource.apiGroup) == 0 || self.dataSource.apiGroup == 'demo.state-snapshotter.deckhouse.io'",message="dataSource apiGroup must be demo.state-snapshotter.deckhouse.io or empty"
// +kubebuilder:validation:XValidation:rule="has(self.dataSource) || has(self.size)",message="size is required when dataSource is not set (blank disk provisioning)"
type DemoVirtualDiskSpec struct {
	// PersistentVolumeClaimName is the in-namespace PVC the disk controller creates and owns. When set,
	// the disk snapshot owns the PVC data leg: it creates a VolumeCaptureRequest for the PVC and publishes
	// the bound VolumeSnapshotContent into its SnapshotContent.status.dataRef. The PVC then becomes a
	// subtree-covered volume and the namespace root MUST NOT treat it as an orphan PVC (no root VS).
	// When empty the disk is manifest-only (no data leg).
	// +optional
	PersistentVolumeClaimName string `json:"persistentVolumeClaimName,omitempty"`

	// Size is the requested storage capacity for a blank (empty) disk. Required when dataSource is unset.
	// Ignored for restore disks (size comes from the snapshot data artifact / VSC.restoreSize).
	// +optional
	Size *resource.Quantity `json:"size,omitempty"`

	// StorageClassName is the StorageClass for the blank PVC, or the fallback when restoring if the
	// snapshot dataRef does not carry a StorageClassName.
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`

	// VolumeMode selects Block or Filesystem for the blank PVC, or the fallback when restoring if the
	// snapshot dataRef does not carry volumeMode (restore still requires a mode before VRR creation).
	// +kubebuilder:validation:Enum=Block;Filesystem
	// +optional
	VolumeMode string `json:"volumeMode,omitempty"`

	// DataSource, when set, declares restore intent: this disk is provisioned from an existing
	// DemoVirtualDiskSnapshot in the same namespace (mirroring PVC.spec.dataSource -> VolumeSnapshot).
	// +optional
	DataSource *DemoVirtualDiskDataSource `json:"dataSource,omitempty"`
}

// DemoVirtualDiskDataSource references the restore source for a DemoVirtualDisk, mirroring the typed
// PVC.spec.dataSource (which references a VolumeSnapshot). Only DemoVirtualDiskSnapshot is supported.
// +k8s:deepcopy-gen=true
type DemoVirtualDiskDataSource struct {
	// APIGroup of the source object's kind (for example demo.state-snapshotter.deckhouse.io). An empty
	// value refers to the core API group.
	// +optional
	APIGroup string `json:"apiGroup,omitempty"`

	// Kind of the source object (must be DemoVirtualDiskSnapshot).
	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`

	// Name of the source object in the same namespace as the disk.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// +k8s:deepcopy-gen=true
type DemoVirtualDiskStatus struct {
	// Phase summarizes disk materialization (Pending, Ready, Failed).
	// +optional
	Phase string `json:"phase,omitempty"`

	// Conditions report readiness and materialization progress.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// PersistentVolumeClaimRef references the materialized backing PVC in the same namespace.
	// +optional
	PersistentVolumeClaimRef *DemoObjectRef `json:"persistentVolumeClaimRef,omitempty"`

	// Capacity reports the REAL allocated size of the disk's backing volume, mirroring
	// PersistentVolumeClaim.status.capacity (a ResourceList keyed by resource name such as "storage").
	// The snapshot import path reads this from the captured raw manifest in MCP to size the blank PVC /
	// restored volume, so every domain data resource MUST publish its real allocated size here (not the
	// requested size). The map is typed with apimachinery's resource.Quantity (string key) to keep the
	// api module dependency-free while staying wire-compatible with corev1.ResourceList. See unified
	// snapshot plan §2 (volume metadata / status.capacity contract).
	// +optional
	Capacity map[string]resource.Quantity `json:"capacity,omitempty"`
}

// DemoObjectRef is a namespaced object reference within the same namespace as the parent resource.
// +k8s:deepcopy-gen=true
type DemoObjectRef struct {
	// Name of the referenced object.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// +kubebuilder:object:root=true
type DemoVirtualDiskList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DemoVirtualDisk `json:"items"`
}
