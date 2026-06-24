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
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=demovm
// DemoVirtualMachine is the demo domain compute resource. The domain controller materializes a Pod that
// mounts the backing PVC of the linked DemoVirtualDisk.
type DemoVirtualMachine struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DemoVirtualMachineSpec   `json:"spec,omitempty"`
	Status DemoVirtualMachineStatus `json:"status,omitempty"`
}

// +k8s:deepcopy-gen=true
type DemoVirtualMachineSpec struct {
	// VirtualDiskName links this VM to a DemoVirtualDisk for snapshot-tree planning (parent -> child).
	// The reference points DOWN the hierarchy (VM -> Disk -> PVC), mirroring
	// DemoVirtualDisk.spec.persistentVolumeClaimName. Kubernetes ownerReferences are lifecycle
	// metadata only and are not used as topology source-of-truth.
	// +optional
	VirtualDiskName string `json:"virtualDiskName,omitempty"`
}

// +k8s:deepcopy-gen=true
type DemoVirtualMachineStatus struct {
	// Phase summarizes VM materialization (Pending, Ready, Failed).
	// +optional
	Phase string `json:"phase,omitempty"`

	// Conditions report readiness and materialization progress.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// PodRef references the materialized demo Pod in the same namespace.
	// +optional
	PodRef *DemoObjectRef `json:"podRef,omitempty"`
}

// +kubebuilder:object:root=true
type DemoVirtualMachineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DemoVirtualMachine `json:"items"`
}
