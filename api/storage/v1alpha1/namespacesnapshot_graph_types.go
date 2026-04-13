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

// NamespaceSnapshotChildRef identifies one child NamespaceSnapshot in the N2b manifests-only tree
// (element of status.childrenSnapshotRefs). It is not a generic snapshot reference: it does not
// carry apiVersion/kind; N2b currently assumes children are NamespaceSnapshot roots in the named
// namespace. A future multi-kind child model would require extending or replacing this shape.
//
// +k8s:deepcopy-gen=true
type NamespaceSnapshotChildRef struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// NamespaceSnapshotContentChildRef identifies one child NamespaceSnapshotContent in the N2b graph
// (element of status.childrenSnapshotContentRefs). Cluster-scoped name only; kind is implied
// (NamespaceSnapshotContent). Not a universal SnapshotContent reference.
//
// +k8s:deepcopy-gen=true
type NamespaceSnapshotContentChildRef struct {
	Name string `json:"name"`
}
