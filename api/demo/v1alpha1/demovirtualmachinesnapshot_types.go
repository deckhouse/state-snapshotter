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

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=demovmsnap
// DemoVirtualMachineSnapshot is an intermediate demo snapshot node (PR5b) under root Snapshot; disk snapshots may attach here.
type DemoVirtualMachineSnapshot struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DemoVirtualMachineSnapshotSpec   `json:"spec,omitempty"`
	Status DemoVirtualMachineSnapshotStatus `json:"status,omitempty"`
}

// DemoVirtualMachineSnapshotSpec defines the desired state of DemoVirtualMachineSnapshot.
// +k8s:deepcopy-gen=true
type DemoVirtualMachineSnapshotSpec struct {
	// SourceRef identifies the DemoVirtualMachine captured by this snapshot.
	// +kubebuilder:validation:Required
	SourceRef SnapshotSourceRef `json:"sourceRef"`
}

// DemoVirtualMachineSnapshotStatus defines the observed state of DemoVirtualMachineSnapshot.
// +k8s:deepcopy-gen=true
type DemoVirtualMachineSnapshotStatus struct {
	// BoundSnapshotContentName is the cluster-scoped SnapshotContent name, once created.
	BoundSnapshotContentName string `json:"boundSnapshotContentName,omitempty"`

	// ManifestCaptureRequestName is the temporary MCR owned by this snapshot while own-scope capture runs.
	ManifestCaptureRequestName string `json:"manifestCaptureRequestName,omitempty"`

	// Conditions report readiness (e.g. Ready=True for generic parent E6 aggregation).
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ChildrenSnapshotRefs lists child snapshot objects (e.g. DemoVirtualDiskSnapshot) under this VM snapshot.
	// +optional
	// +listType=map
	// +listMapKey=apiVersion
	// +listMapKey=kind
	// +listMapKey=name
	ChildrenSnapshotRefs []storagev1alpha1.SnapshotChildRef `json:"childrenSnapshotRefs,omitempty"`
}

// +kubebuilder:object:root=true
type DemoVirtualMachineSnapshotList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DemoVirtualMachineSnapshot `json:"items"`
}
