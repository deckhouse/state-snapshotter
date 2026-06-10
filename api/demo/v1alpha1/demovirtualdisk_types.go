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
// +kubebuilder:resource:shortName=demovd
// DemoVirtualDisk is a minimal placeholder "resource" side for CSD mapping (PR5a); not used by reconcile logic yet.
type DemoVirtualDisk struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              DemoVirtualDiskSpec   `json:"spec,omitempty"`
	Status            DemoVirtualDiskStatus `json:"status,omitempty"`
}

// +k8s:deepcopy-gen=true
type DemoVirtualDiskSpec struct {
	// PersistentVolumeClaimName is the in-namespace PVC backing this disk. When set, the disk snapshot
	// owns the PVC data leg: it creates a VolumeCaptureRequest for the PVC and publishes the bound
	// VolumeSnapshotContent into its SnapshotContent.status.dataRefs[]. The PVC then becomes a
	// subtree-covered volume and the namespace root MUST NOT treat it as an orphan PVC (no root VS).
	// When empty the disk is manifest-only (no data leg).
	// +optional
	PersistentVolumeClaimName string `json:"persistentVolumeClaimName,omitempty"`

	// DataSource, when set, declares restore intent: this disk is to be provisioned from an existing
	// snapshot, mirroring how PVC.spec.dataSource references a VolumeSnapshot. The reference points to a
	// DemoVirtualDiskSnapshot. This records intent only; restore reconcile is not implemented yet.
	// +optional
	DataSource *DemoVirtualDiskDataSource `json:"dataSource,omitempty"`
}

// DemoVirtualDiskDataSource references the restore source for a DemoVirtualDisk, mirroring the typed
// PVC.spec.dataSource (which references a VolumeSnapshot). Today only DemoVirtualDiskSnapshot is supported.
// +k8s:deepcopy-gen=true
type DemoVirtualDiskDataSource struct {
	// APIGroup of the source object's kind (for example demo.state-snapshotter.deckhouse.io). An empty
	// value refers to the core API group.
	// +optional
	APIGroup string `json:"apiGroup,omitempty"`

	// Kind of the source object (for example DemoVirtualDiskSnapshot).
	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`

	// Name of the source object in the same namespace as the disk.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

type DemoVirtualDiskStatus struct{}

// +kubebuilder:object:root=true
type DemoVirtualDiskList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DemoVirtualDisk `json:"items"`
}
