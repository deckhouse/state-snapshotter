package restore

import (
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type SnapshotContentNode struct {
	Content *unstructured.Unstructured
	// ManifestCheckpointName is required for /manifests.
	ManifestCheckpointName string
	// DataBindings are this node's status.dataRefs[] only (not inherited from parent/child).
	DataBindings []snapshot.DataBindingRef
	Children     []*SnapshotContentNode
}

// RestoreNode is one node of the snapshot run tree used by the restore compiler
// (manifests-with-data-restoration, ADR 2026-06-10). Unlike SnapshotContentNode it walks the
// Snapshot run tree (Snapshot -> status.childrenSnapshotRefs) so it carries the owning snapshot CR
// identity (needed by domain restore transforms to point a restored object at its own snapshot) and
// the orphan-PVC VolumeSnapshot visibility leaves.
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

type Options struct {
	SnapshotName      string
	SnapshotNamespace string
	TargetNamespace   string
	RestoreStrategy   string
}

type TransformResult struct {
	Objects []unstructured.Unstructured
}
