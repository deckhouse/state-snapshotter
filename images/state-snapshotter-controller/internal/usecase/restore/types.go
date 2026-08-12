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

package restore

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// RestoreNode is one node of the snapshot run tree used by the restore compiler
// (manifests-with-data-restoration, ADR 2026-06-10). It walks the Snapshot run tree
// (Snapshot -> status.childrenSnapshotRefs) so it carries the owning snapshot CR identity (needed by
// domain restore transforms to point a restored object at its own snapshot) and the orphan-PVC
// VolumeSnapshot visibility leaves.
type RestoreNode struct {
	// SnapshotRef is the snapshot CR that owns this node (generic Snapshot at the root, domain
	// snapshot CRs below). Namespace is the run-tree namespace.
	SnapshotRef snapshot.ObjectRef
	// ManifestCheckpointName is the MCP of the SnapshotContent bound to this snapshot.
	ManifestCheckpointName string
	// DataBindings are this node's SnapshotContent.status.dataRefs[] (PVC -> VSC artifact).
	DataBindings []snapshot.DataBindingRef
	// VSCToVS maps a durable VolumeSnapshotContent name to the namespaced VolumeSnapshot name,
	// resolved from this node's Snapshot.status.childrenSnapshotRefs[] VolumeSnapshot visibility
	// leaves (INV-ORPHAN4). It is the only supported way to turn a dataRefs VSC artifact into a
	// PVC.spec.dataSourceRef VolumeSnapshot; the compiler never reads the VS from MCP manifests.
	VSCToVS  map[string]string
	Children []*RestoreNode

	// Domain marks a node whose kind is owned by an out-of-process domain controller. Core does not
	// compile it in-process (it reads no SnapshotContent/MCP for it and does not descend): the whole
	// subtree rooted here is restored by the domain's aggregated apiserver (manifests-with-data-
	// restoration), and the compiler splices the returned manifests. SnapshotRef carries the GVK/
	// namespace/name needed to address that call. Only the boundary node is marked; its descendants
	// are resolved by the domain apiserver, not by this resolver.
	Domain bool
}

// NodeResult carries the apply-ready objects compiled for one RestoreNode, passed up to the parent
// transform during post-order traversal so domain parents can reference restored children.
type NodeResult struct {
	Node    *RestoreNode
	Objects []unstructured.Unstructured
}

// Scope selects the compilation depth of manifests-with-data-restoration.
type Scope string

const (
	// ScopeSubtree compiles the addressed node and its whole run-tree recursively (post-order). It is
	// the default when no scope is requested — the historical behavior, fully backward compatible — and
	// fails closed: any not-ready descendant aborts the whole request (409).
	ScopeSubtree Scope = "subtree"
	// ScopeNode compiles ONLY the addressed node. The node's own validations still run (Ready gate,
	// bound content Ready, anti-spoofing back-ref 403 for the root, non-empty MCP), but the run-tree
	// children are not read at all — a not-ready child does NOT fail the request.
	ScopeNode Scope = "node"
)

type Options struct {
	SnapshotName      string
	SnapshotNamespace string
	TargetNamespace   string

	// Scope selects the compilation depth (see Scope). An empty value is treated as ScopeSubtree, so a
	// zero Options keeps the historical recursive behavior. Set by the handler from the ?scope= query.
	Scope Scope

	// FilterKind, FilterName and FilterAPIVersion select a single object from the compiled node output
	// (?kind=&name=&apiVersion=). The object filter is valid ONLY together with ScopeNode; the handler
	// enforces that contract (an object filter with scope != node is rejected with ErrBadRequest) and
	// this struct only carries the already-validated values. FilterAPIVersion is optional and may be a
	// full "group/version" or a bare "group" (used to disambiguate a kind+name present in two groups).
	FilterKind       string
	FilterName       string
	FilterAPIVersion string
}

type TransformResult struct {
	Objects []unstructured.Unstructured
}
