package restore

import (
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type SnapshotContentNode struct {
	Content *unstructured.Unstructured
	// ManifestCheckpointName is required for /manifests.
	ManifestCheckpointName string
	// DataBindings maps PVC targets to durable data artifacts on this content node.
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
