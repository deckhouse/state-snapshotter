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
// +kubebuilder:resource:scope=Cluster,shortName=demovmsnapc
// DemoVirtualMachineSnapshotContent is cluster-scoped content for a DemoVirtualMachineSnapshot (PR5b).
type DemoVirtualMachineSnapshotContent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DemoVirtualMachineSnapshotContentSpec   `json:"spec,omitempty"`
	Status DemoVirtualMachineSnapshotContentStatus `json:"status,omitempty"`
}

// DemoVirtualMachineSnapshotContentSpec defines the desired state of DemoVirtualMachineSnapshotContent.
type DemoVirtualMachineSnapshotContentSpec struct {
	// SnapshotRef points to the namespaced DemoVirtualMachineSnapshot.
	// +kubebuilder:validation:Required
	SnapshotRef storagev1alpha1.SnapshotSubjectRef `json:"snapshotRef"`
}

// DemoVirtualMachineSnapshotContentStatus defines the observed state of DemoVirtualMachineSnapshotContent.
// +k8s:deepcopy-gen=true
type DemoVirtualMachineSnapshotContentStatus struct {
	// ChildrenSnapshotContentRefs lists child snapshot content objects (e.g. DemoVirtualDiskSnapshotContent).
	// +optional
	ChildrenSnapshotContentRefs []storagev1alpha1.NamespaceSnapshotContentChildRef `json:"childrenSnapshotContentRefs,omitempty"`
}

// +kubebuilder:object:root=true
type DemoVirtualMachineSnapshotContentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DemoVirtualMachineSnapshotContent `json:"items"`
}
