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
// +kubebuilder:resource:scope=Cluster,shortName=demovdsnapc
// DemoVirtualDiskSnapshotContent is cluster-scoped content for a DemoVirtualDiskSnapshot (DSC / unified mapping).
type DemoVirtualDiskSnapshotContent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DemoVirtualDiskSnapshotContentSpec   `json:"spec,omitempty"`
	Status DemoVirtualDiskSnapshotContentStatus `json:"status,omitempty"`
}

// DemoVirtualDiskSnapshotContentSpec defines the desired state of DemoVirtualDiskSnapshotContent.
type DemoVirtualDiskSnapshotContentSpec struct {
	// SnapshotRef points to the namespaced DemoVirtualDiskSnapshot.
	// +kubebuilder:validation:Required
	SnapshotRef storagev1alpha1.SnapshotSubjectRef `json:"snapshotRef"`
}

// DemoVirtualDiskSnapshotContentStatus defines the observed state of DemoVirtualDiskSnapshotContent.
type DemoVirtualDiskSnapshotContentStatus struct {
	// ManifestCheckpointName points to the materialized manifest checkpoint for this content node.
	// +optional
	ManifestCheckpointName string `json:"manifestCheckpointName,omitempty"`
}

// +kubebuilder:object:root=true
type DemoVirtualDiskSnapshotContentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DemoVirtualDiskSnapshotContent `json:"items"`
}
