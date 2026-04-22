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
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=demovd
// DemoVirtualDisk is a minimal placeholder "resource" side for DSC mapping (PR5a); not used by reconcile logic yet.
type DemoVirtualDisk struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              DemoVirtualDiskSpec   `json:"spec,omitempty"`
	Status            DemoVirtualDiskStatus `json:"status,omitempty"`
}

type DemoVirtualDiskSpec struct{}

type DemoVirtualDiskStatus struct{}

// +kubebuilder:object:root=true
type DemoVirtualDiskList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DemoVirtualDisk `json:"items"`
}
