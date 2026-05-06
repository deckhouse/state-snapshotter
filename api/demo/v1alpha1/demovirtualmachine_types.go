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
// +kubebuilder:resource:shortName=demovm
// DemoVirtualMachine is a minimal placeholder "resource" side for DSC mapping (PR5b); not reconciled beyond registration.
type DemoVirtualMachine struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DemoVirtualMachineSpec   `json:"spec,omitempty"`
	Status DemoVirtualMachineStatus `json:"status,omitempty"`
}

type DemoVirtualMachineSpec struct{}

type DemoVirtualMachineStatus struct{}

// +kubebuilder:object:root=true
type DemoVirtualMachineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DemoVirtualMachine `json:"items"`
}
