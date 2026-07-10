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

// SnapshotChildRef identifies one child snapshot object in the run tree (element of
// status.childrenSnapshotRefs). apiVersion and kind are required (Kubernetes-style reference);
// generic code resolves the object with a single client Get — no registry scan and no ambiguity.
//
// Snapshot-run tree is namespace-local to the root Snapshot: the child object MUST live
// in the same namespace as that parent. Namespace is implicit and always taken from parent
// Snapshot namespace. Cross-namespace refs are not part of this model.
//
// +k8s:deepcopy-gen=true
type SnapshotChildRef struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
}

// SnapshotContentChildRef identifies one child common SnapshotContent in the target content graph.
// SnapshotContent is cluster-scoped, so the ref is name-only and MUST NOT carry namespace.
//
// +k8s:deepcopy-gen=true
type SnapshotContentChildRef struct {
	// Name is the cluster-scoped child SnapshotContent name. MaxLength=253 is the Kubernetes object-name
	// ceiling (a DNS subdomain), so it constrains nothing real; it is REQUIRED here to bound the CEL
	// estimated cost of the childrenSnapshotContentRefs frozen-set immutability rule (self == oldSelf), which
	// otherwise assumes an unbounded per-element string and blows the apiserver cost budget (CRD rejected).
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// ExcludedObjectRef identifies one source object excluded from a snapshot (element of
// status.excludedRefs on Snapshot/domain CRs and SnapshotContent). It is the shadow of SnapshotChildRef:
// the same {apiVersion,kind,name} shape, but pointing at the SOURCE object that was vetoed out (via the
// state-snapshotter.deckhouse.io/exclude label, or an explicit top-level drop) rather than at a child
// snapshot. Namespace is implicit (the snapshot's namespace).
//
// Unlike childrenSnapshotRefs (direct edges), the durable aggregate on SnapshotContent collects the
// excluded refs of the WHOLE subtree, because an excluded object is non-navigable: no snapshot CR is
// created for it, so it cannot be descended into to recover a per-node view later.
//
// +k8s:deepcopy-gen=true
type ExcludedObjectRef struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
}

// SnapshotSourceRef is the single source-of-truth identifying the namespace-local source object that a
// snapshot captures. It lives on the snapshot spec (spec.sourceRef) and is the generic contract the
// core planner reads to deduplicate coverage across the run tree. Namespace is implicit: the source
// object MUST live in the same namespace as the snapshot. It carries no uid; the captured source
// object ref (including uid) is published separately on the snapshot status (status.sourceRef).
//
// This is the canonical definition shared across API groups (the demo API group aliases it).
//
// +k8s:deepcopy-gen=true
type SnapshotSourceRef struct {
	// +kubebuilder:validation:MinLength=1
	APIVersion string `json:"apiVersion"`
	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// ObjectRef is a minimal, non-CRD reference to a Kubernetes object
// ({apiVersion,kind,name,namespace,uid}). It is a plain helper type (no JSON tags, not a registered
// Kubernetes object) used across the snapshot graph contract — e.g. restore-graph nodes and PVC data
// targets. It is the canonical definition shared across modules: core pkg/snapshot aliases it, so the
// core controller and the domain controller reference one type via api/.
type ObjectRef struct {
	APIVersion string
	Kind       string
	Name       string
	Namespace  string // Only for namespaced resources
	UID        string // Optional; set for PVC targets in dataRefs
}
