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

type Options struct {
	SnapshotName      string
	SnapshotNamespace string
	TargetNamespace   string
	RestoreStrategy   string
}

type TransformResult struct {
	Objects []unstructured.Unstructured
}
