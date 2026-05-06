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

// +k8s:deepcopy-gen=true
// ObjectReference contains enough information to let you inspect or modify the referred object.
type ObjectReference struct {
	// Name of the referred object
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Namespace of the referred object (empty for cluster-scoped objects)
	// +kubebuilder:validation:Required
	Namespace string `json:"namespace"`

	// UID of the referred object
	// +kubebuilder:validation:Required
	UID string `json:"uid"`
}
