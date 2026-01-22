package restore

import "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

type SnapshotContentNode struct {
	Content *unstructured.Unstructured
	// ManifestCheckpointName is required for /manifests.
	ManifestCheckpointName string
	// DataRefKind is optional. When present, it is used as VRR source kind.
	DataRefKind string
	// DataRefName is optional. When present, it is used as VRR source name.
	DataRefName string
	Children    []*SnapshotContentNode
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
