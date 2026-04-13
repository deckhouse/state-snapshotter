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

// SnapshotRef identifies a namespaced NamespaceSnapshot child (N2b graph on root status).
// PR1: shape only; controller wiring and semantics come in later PRs (see implementation-plan §2.4.2).
//
// +k8s:deepcopy-gen=true
type SnapshotRef struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// SnapshotContentRef identifies a cluster-scoped NamespaceSnapshotContent child (N2b graph on content status).
// PR1: shape only.
//
// +k8s:deepcopy-gen=true
type SnapshotContentRef struct {
	Name string `json:"name"`
}
