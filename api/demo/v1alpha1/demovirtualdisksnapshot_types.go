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

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=demovdsnap
// DemoVirtualDiskSnapshot is a minimal demo snapshot node (PR5a). Wires into root NamespaceSnapshot via children*Refs.
type DemoVirtualDiskSnapshot struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DemoVirtualDiskSnapshotSpec   `json:"spec,omitempty"`
	Status DemoVirtualDiskSnapshotStatus `json:"status,omitempty"`
}

// DemoVirtualDiskSnapshotSpec defines the desired state of DemoVirtualDiskSnapshot.
type DemoVirtualDiskSnapshotSpec struct {
	// RootNamespaceSnapshotRef identifies the NamespaceSnapshot run root (same namespace allowed).
	// +kubebuilder:validation:Required
	RootNamespaceSnapshotRef storagev1alpha1.SnapshotSubjectRef `json:"rootNamespaceSnapshotRef"`

	// PersistentVolumeClaimName is the PVC name in the same namespace as this snapshot (PR5a: identity only; no VolumeSnapshot/CSI wiring yet).
	// +optional
	PersistentVolumeClaimName string `json:"persistentVolumeClaimName,omitempty"`
}

// DemoVirtualDiskSnapshotStatus defines the observed state of DemoVirtualDiskSnapshot.
type DemoVirtualDiskSnapshotStatus struct {
	// BoundSnapshotContentName is the cluster-scoped DemoVirtualDiskSnapshotContent name, once created.
	BoundSnapshotContentName string `json:"boundSnapshotContentName,omitempty"`
}

// +kubebuilder:object:root=true
type DemoVirtualDiskSnapshotList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DemoVirtualDiskSnapshot `json:"items"`
}
