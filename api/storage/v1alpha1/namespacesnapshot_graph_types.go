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

// NamespaceSnapshotChildRef identifies one child snapshot object in the run tree (element of
// status.childrenSnapshotRefs). apiVersion and kind are required (Kubernetes-style reference);
// generic code resolves the object with a single client Get — no registry scan and no ambiguity.
//
// Snapshot-run tree is namespace-local to the root NamespaceSnapshot: the child object MUST live
// in the same namespace as that parent. Namespace is implicit and always taken from parent
// NamespaceSnapshot namespace. Cross-namespace refs are not part of this model.
//
// +k8s:deepcopy-gen=true
type NamespaceSnapshotChildRef struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
}

// NamespaceSnapshotContentChildRef identifies one child NamespaceSnapshotContent in the N2b graph
// (element of status.childrenSnapshotContentRefs). Cluster-scoped name only; kind is implied
// (NamespaceSnapshotContent). Not a universal SnapshotContent reference.
//
// +k8s:deepcopy-gen=true
type NamespaceSnapshotContentChildRef struct {
	Name string `json:"name"`
}

// SnapshotContentChildRef identifies one child common SnapshotContent in the target content graph.
// SnapshotContent is cluster-scoped, so the ref is name-only and MUST NOT carry namespace.
//
// +k8s:deepcopy-gen=true
type SnapshotContentChildRef struct {
	Name string `json:"name"`
}
