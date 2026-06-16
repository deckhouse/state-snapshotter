package restore

import (
	"context"
	"encoding/json"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// SnapshotViewVersion is the current snapshot view format version. It is intentionally independent of
// IndexVersion: the view is the stable, presentation-oriented projection consumed by `d8 snapshot
// list`, so the internal index.json can evolve without forcing a view (and thus a d8) change.
const SnapshotViewVersion = "v1"

// SnapshotView is the human/CLI-facing projection of a snapshot hierarchy: a nested tree carrying
// only the fields needed to render `d8 snapshot list`. It deliberately does NOT expose the internal
// index wiring (per-node ids, parent links, artifact names, manifest checkpoints): the CLI renders the
// tree from this stable shape and never parses index.json.
type SnapshotView struct {
	Version string           `json:"version"`
	Root    SnapshotViewNode `json:"root"`
}

// SnapshotViewNode is one node of the view tree.
type SnapshotViewNode struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	// HasData reports whether this node carries a restorable volume.
	HasData bool `json:"hasData"`
	// VolumeMode is the data volume mode (Block or Filesystem); empty for dataless nodes.
	VolumeMode string `json:"volumeMode,omitempty"`
	// SizeBytes is the volume size (from the source VolumeSnapshotContent restoreSize); 0 if unknown.
	SizeBytes int64              `json:"sizeBytes,omitempty"`
	Children  []SnapshotViewNode `json:"children,omitempty"`
}

// BuildView resolves the snapshot run tree rooted at the namespaced Snapshot and serialises it as a
// JSON SnapshotView. It reuses the restore resolver, so it fails closed on any not-ready/missing node.
func (s *Service) BuildView(ctx context.Context, opts Options) ([]byte, error) {
	return s.buildView(ctx, func() (*RestoreNode, error) {
		return s.resolver.ResolveRestoreTree(ctx, opts.SnapshotNamespace, opts.SnapshotName)
	})
}

// BuildViewForNode builds the view for the subtree rooted at a domain snapshot CR identified by gvk.
func (s *Service) BuildViewForNode(ctx context.Context, gvk schema.GroupVersionKind, opts Options) ([]byte, error) {
	return s.buildView(ctx, func() (*RestoreNode, error) {
		return s.resolver.ResolveRestoreSubtree(ctx, gvk, opts.SnapshotNamespace, opts.SnapshotName)
	})
}

func (s *Service) buildView(ctx context.Context, resolveRoot func() (*RestoreNode, error)) ([]byte, error) {
	root, err := resolveRoot()
	if err != nil {
		return nil, err
	}
	node, err := s.viewNode(ctx, root)
	if err != nil {
		return nil, err
	}
	return json.Marshal(&SnapshotView{Version: SnapshotViewVersion, Root: *node})
}

func (s *Service) viewNode(ctx context.Context, n *RestoreNode) (*SnapshotViewNode, error) {
	out := &SnapshotViewNode{
		APIVersion: n.SnapshotRef.APIVersion,
		Kind:       n.SnapshotRef.Kind,
		Namespace:  n.SnapshotRef.Namespace,
		Name:       n.SnapshotRef.Name,
	}
	if len(n.DataBindings) > 0 {
		// Unified model: at most one data binding per snapshot; take the first deterministically.
		b := n.DataBindings[0]
		size, err := s.resolveArtifactSize(ctx, b.Artifact)
		if err != nil {
			return nil, err
		}
		out.HasData = true
		out.VolumeMode = b.VolumeMode
		out.SizeBytes = size
	}
	for _, c := range n.Children {
		cn, err := s.viewNode(ctx, c)
		if err != nil {
			return nil, err
		}
		out.Children = append(out.Children, *cn)
	}
	return out, nil
}
