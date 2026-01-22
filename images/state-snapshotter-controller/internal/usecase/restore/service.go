package restore

import (
	"context"
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
)

type Service struct {
	resolver   *Resolver
	loader     *Loader
	transform  *Transformer
	kubeClient client.Client
}

func NewService(kubeClient client.Client, archiveService *usecase.ArchiveService) *Service {
	return &Service{
		resolver:   NewResolver(kubeClient),
		loader:     NewLoader(kubeClient, archiveService),
		transform:  NewTransformer(),
		kubeClient: kubeClient,
	}
}

func (s *Service) BuildManifestsWithDataRestoration(ctx context.Context, opts Options) ([]byte, error) {
	root, err := s.resolver.ResolveSnapshotTree(ctx, opts.SnapshotNamespace, opts.SnapshotName)
	if err != nil {
		return nil, err
	}

	objects, err := s.collectManifests(ctx, root, opts)
	if err != nil {
		return nil, err
	}

	return marshalObjects(objects)
}

func (s *Service) BuildManifests(ctx context.Context, opts Options) ([]byte, error) {
	root, err := s.resolver.ResolveSnapshotTree(ctx, opts.SnapshotNamespace, opts.SnapshotName)
	if err != nil {
		return nil, err
	}

	objects, err := s.collectRawManifests(ctx, root)
	if err != nil {
		return nil, err
	}
	return marshalObjects(objects)
}

func (s *Service) collectManifests(ctx context.Context, node *SnapshotContentNode, opts Options) ([]unstructured.Unstructured, error) {
	raw, err := s.loader.LoadManifests(ctx, node.ManifestCheckpointName)
	if err != nil {
		return nil, err
	}
	result, err := s.transform.Transform(raw, opts, node)
	if err != nil {
		return nil, err
	}

	objects := result.Objects
	for _, child := range node.Children {
		childObjects, err := s.collectManifests(ctx, child, opts)
		if err != nil {
			return nil, err
		}
		objects = append(objects, childObjects...)
	}
	return objects, nil
}

func (s *Service) collectRawManifests(ctx context.Context, node *SnapshotContentNode) ([]unstructured.Unstructured, error) {
	raw, err := s.loader.LoadManifests(ctx, node.ManifestCheckpointName)
	if err != nil {
		return nil, err
	}
	objects := raw
	for _, child := range node.Children {
		childObjects, err := s.collectRawManifests(ctx, child)
		if err != nil {
			return nil, err
		}
		objects = append(objects, childObjects...)
	}
	return objects, nil
}

func marshalObjects(objects []unstructured.Unstructured) ([]byte, error) {
	raw := make([]map[string]interface{}, 0, len(objects))
	for i := range objects {
		raw = append(raw, objects[i].Object)
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize manifests: %w", err)
	}
	return data, nil
}
