package restore

import (
	"context"
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
)

type Service struct {
	resolver     *Resolver
	loader       *Loader
	kubeClient   client.Client
	transformers []DomainRestoreTransformer
}

// NewService builds the restore compiler. Domain restore transformers (e.g. the demo controllers'
// transformer) are registered here so the generic pipeline stays domain-free.
func NewService(kubeClient client.Client, archiveService *usecase.ArchiveService, transformers ...DomainRestoreTransformer) *Service {
	return &Service{
		resolver:     NewResolver(kubeClient),
		loader:       NewLoader(kubeClient, archiveService),
		kubeClient:   kubeClient,
		transformers: transformers,
	}
}

// BuildManifestsWithDataRestoration is the restore compiler: it walks the Snapshot run tree and
// compiles apply-ready manifests bottom-up (post-order), rewriting data references so the output can
// be applied directly into targetNamespace. It never emits VolumeRestoreRequest or other
// control-plane objects (ADR 2026-06-10).
func (s *Service) BuildManifestsWithDataRestoration(ctx context.Context, opts Options) ([]byte, error) {
	return s.buildRestore(ctx, opts, func() (*RestoreNode, error) {
		return s.resolver.ResolveRestoreTree(ctx, opts.SnapshotNamespace, opts.SnapshotName)
	})
}

// BuildManifestsWithDataRestorationForNode compiles the restore subtree rooted at a specific snapshot
// node (the namespaced root Snapshot or a domain snapshot CR identified by gvk), so the endpoint can
// return apply-ready manifests for that node only, e.g. a single VM or disk snapshot.
func (s *Service) BuildManifestsWithDataRestorationForNode(ctx context.Context, gvk schema.GroupVersionKind, opts Options) ([]byte, error) {
	return s.buildRestore(ctx, opts, func() (*RestoreNode, error) {
		return s.resolver.ResolveRestoreSubtree(ctx, gvk, opts.SnapshotNamespace, opts.SnapshotName)
	})
}

func (s *Service) buildRestore(ctx context.Context, opts Options, resolveRoot func() (*RestoreNode, error)) ([]byte, error) {
	targetNamespace := opts.TargetNamespace
	if targetNamespace == "" {
		targetNamespace = opts.SnapshotNamespace
	}

	root, err := resolveRoot()
	if err != nil {
		return nil, err
	}

	result, err := s.compileNode(ctx, root, targetNamespace)
	if err != nil {
		return nil, err
	}

	return marshalObjects(result.Objects)
}

// compileNode compiles a RestoreNode in post-order: children are compiled first so their results are
// available to the parent transform, then this node's manifests are loaded and transformed.
func (s *Service) compileNode(ctx context.Context, node *RestoreNode, targetNamespace string) (NodeResult, error) {
	childResults := make([]NodeResult, 0, len(node.Children))
	childObjects := make([]unstructured.Unstructured, 0)
	for _, child := range node.Children {
		childResult, err := s.compileNode(ctx, child, targetNamespace)
		if err != nil {
			return NodeResult{}, err
		}
		childResults = append(childResults, childResult)
		childObjects = append(childObjects, childResult.Objects...)
	}

	raw, err := s.loader.LoadManifests(ctx, node.ManifestCheckpointName)
	if err != nil {
		return NodeResult{}, err
	}
	// Pass the compiled children so a parent domain transform can reference its restored children
	// (post-order, bottom-up).
	nodeObjects, err := transformNodeObjects(node, raw, s.transformers, childResults, targetNamespace)
	if err != nil {
		return NodeResult{}, err
	}

	// Emit children before the parent (post-order output): leaf data objects (e.g. a disk) come
	// before objects that depend on them (e.g. a VM), which is friendlier for a straight apply.
	objects := make([]unstructured.Unstructured, 0, len(nodeObjects)+len(childObjects))
	objects = append(objects, childObjects...)
	objects = append(objects, nodeObjects...)
	return NodeResult{Node: node, Objects: objects}, nil
}

func (s *Service) BuildManifests(ctx context.Context, opts Options) ([]byte, error) {
	if opts.TargetNamespace != "" && opts.TargetNamespace != opts.SnapshotNamespace {
		return nil, fmt.Errorf("%w: targetNamespace differs from snapshot namespace (MVP limitation)", ErrBadRequest)
	}
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
	seen := make(map[string]struct{})
	raw := make([]map[string]interface{}, 0, len(objects))
	for i := range objects {
		obj := objects[i]
		namespace := obj.GetNamespace()
		if namespace == "" {
			namespace = "_cluster"
		}
		key := fmt.Sprintf("%s|%s|%s|%s", obj.GetAPIVersion(), obj.GetKind(), namespace, obj.GetName())
		if _, exists := seen[key]; exists {
			return nil, fmt.Errorf("%w: duplicate object %s", ErrContractViolation, key)
		}
		seen[key] = struct{}{}
		raw = append(raw, obj.Object)
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize manifests: %w", err)
	}
	return data, nil
}
