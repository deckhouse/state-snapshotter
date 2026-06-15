package restore

import (
	"context"
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// IndexVersion is the current snapshot index format version.
const IndexVersion = "v1"

// Index is the machine-readable description of a snapshot hierarchy used by export/import
// (the d8 CLI and the SnapshotImport controller). It carries the snapshot tree plus, for every
// data node, the volume metadata needed to faithfully re-create the volume on import.
type Index struct {
	Version      string          `json:"version"`
	RootSnapshot IndexSnapshotID `json:"rootSnapshot"`
	// Snapshots is a flat, deterministic (pre-order) list of every snapshot node in the tree.
	Snapshots []IndexSnapshot `json:"snapshots"`
}

// IndexSnapshotID identifies the root snapshot of the hierarchy.
type IndexSnapshotID struct {
	ID         string `json:"id"`
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
}

// IndexSnapshot is one node of the hierarchy.
type IndexSnapshot struct {
	// ID is the stable archive identifier "<kind>--<namespace>--<name>".
	ID         string   `json:"id"`
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Namespace  string   `json:"namespace"`
	Name       string   `json:"name"`
	ParentID   string   `json:"parentId,omitempty"`
	Children   []string `json:"children,omitempty"`
	HasData    bool     `json:"hasData"`
	// Data holds the volume metadata for data nodes (one data per snapshot in the unified model).
	Data *IndexData `json:"data,omitempty"`
}

// IndexData is the per-data-node volume metadata.
type IndexData struct {
	StorageClassName string   `json:"storageClassName,omitempty"`
	VolumeMode       string   `json:"volumeMode,omitempty"`
	FsType           string   `json:"fsType,omitempty"`
	AccessModes      []string `json:"accessModes,omitempty"`
	// Size is the volume size in bytes (read from the source VolumeSnapshotContent restoreSize).
	Size int64 `json:"size,omitempty"`
	// ArtifactName is the source durable VolumeSnapshotContent name (informational).
	ArtifactName string `json:"artifactName,omitempty"`
}

// BuildIndex resolves the snapshot run tree and serialises it as a JSON Index. It reuses the same
// resolver as the restore compiler, so it fails closed on any not-ready / missing node.
func (s *Service) BuildIndex(ctx context.Context, opts Options) ([]byte, error) {
	return s.buildIndex(ctx, func() (*RestoreNode, error) {
		return s.resolver.ResolveRestoreTree(ctx, opts.SnapshotNamespace, opts.SnapshotName)
	})
}

// BuildIndexForNode builds the index for the subtree rooted at a domain snapshot CR identified by gvk.
func (s *Service) BuildIndexForNode(ctx context.Context, gvk schema.GroupVersionKind, opts Options) ([]byte, error) {
	return s.buildIndex(ctx, func() (*RestoreNode, error) {
		return s.resolver.ResolveRestoreSubtree(ctx, gvk, opts.SnapshotNamespace, opts.SnapshotName)
	})
}

func (s *Service) buildIndex(ctx context.Context, resolveRoot func() (*RestoreNode, error)) ([]byte, error) {
	root, err := resolveRoot()
	if err != nil {
		return nil, err
	}
	idx := &Index{
		Version: IndexVersion,
		RootSnapshot: IndexSnapshotID{
			ID:         indexNodeID(root),
			APIVersion: root.SnapshotRef.APIVersion,
			Kind:       root.SnapshotRef.Kind,
			Namespace:  root.SnapshotRef.Namespace,
			Name:       root.SnapshotRef.Name,
		},
	}
	if err := s.appendIndexNodes(ctx, root, "", &idx.Snapshots); err != nil {
		return nil, err
	}
	return json.Marshal(idx)
}

func indexNodeID(n *RestoreNode) string {
	return n.SnapshotRef.Kind + "--" + n.SnapshotRef.Namespace + "--" + n.SnapshotRef.Name
}

func (s *Service) appendIndexNodes(ctx context.Context, node *RestoreNode, parentID string, out *[]IndexSnapshot) error {
	id := indexNodeID(node)
	entry := IndexSnapshot{
		ID:         id,
		APIVersion: node.SnapshotRef.APIVersion,
		Kind:       node.SnapshotRef.Kind,
		Namespace:  node.SnapshotRef.Namespace,
		Name:       node.SnapshotRef.Name,
		ParentID:   parentID,
	}
	for _, c := range node.Children {
		entry.Children = append(entry.Children, indexNodeID(c))
	}

	if len(node.DataBindings) > 0 {
		// Unified model: a snapshot carries at most one data. If more than one binding is present we
		// take the first deterministically; richer multi-data layout is out of scope for index v1.
		b := node.DataBindings[0]
		size, err := s.resolveArtifactSize(ctx, b.Artifact)
		if err != nil {
			return err
		}
		entry.HasData = true
		entry.Data = &IndexData{
			StorageClassName: b.StorageClassName,
			VolumeMode:       b.VolumeMode,
			FsType:           b.FsType,
			AccessModes:      b.AccessModes,
			Size:             size,
			ArtifactName:     b.Artifact.Name,
		}
	}

	*out = append(*out, entry)
	// Children are already deterministically sorted by the resolver, which also guarantees an
	// acyclic tree (the /manifests path detects duplicates with a 409), so this recursion needs no
	// visited-set.
	for _, c := range node.Children {
		if err := s.appendIndexNodes(ctx, c, id, out); err != nil {
			return err
		}
	}
	return nil
}

// resolveArtifactSize reads restoreSize (bytes) from the durable VolumeSnapshotContent referenced by
// the data binding. Non-VSC artifacts and a missing/zero restoreSize yield 0 (size unknown).
func (s *Service) resolveArtifactSize(ctx context.Context, artifact snapshot.ObjectRef) (int64, error) {
	if artifact.Kind != snapshot.KindVolumeSnapshotContent {
		return 0, nil
	}
	vsc := &unstructured.Unstructured{}
	vsc.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   snapshot.CSISnapshotGroup,
		Version: snapshot.CSISnapshotVersion,
		Kind:    snapshot.KindVolumeSnapshotContent,
	})
	if err := s.kubeClient.Get(ctx, client.ObjectKey{Name: artifact.Name}, vsc); err != nil {
		if errors.IsNotFound(err) {
			return 0, fmt.Errorf("%w: artifact VolumeSnapshotContent %s not found", ErrContractViolation, artifact.Name)
		}
		return 0, fmt.Errorf("failed to get VolumeSnapshotContent %s: %w", artifact.Name, err)
	}
	size, _, err := unstructured.NestedInt64(vsc.Object, "status", "restoreSize")
	if err != nil {
		// restoreSize may be absent; treat as unknown rather than failing the whole index.
		return 0, nil
	}
	return size, nil
}
