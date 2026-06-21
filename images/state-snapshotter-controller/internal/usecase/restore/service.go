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
	resolver   *Resolver
	loader     *Loader
	kubeClient client.Client
	// domainRestorer delegates the restore of domain snapshot subtrees to the domain controller's
	// aggregated apiserver. Nil means no domain delegation is configured: encountering a domain node
	// then fails closed (a domain subtree would otherwise be silently dropped). Core stays domain-free.
	domainRestorer DomainSubtreeRestorer
}

// NewService builds the restore compiler. domainRestorer delegates domain snapshot subtrees to the
// domain controller's aggregated apiserver (nil disables delegation); isDomainKind reports which
// snapshot kinds are domain-owned so the resolver stops at them and compileNode delegates (nil treats
// every kind as generic). Both are nil in focused tests and the generic-only path.
func NewService(kubeClient client.Client, archiveService *usecase.ArchiveService, domainRestorer DomainSubtreeRestorer, isDomainKind func(kind string) bool) *Service {
	resolver := NewResolver(kubeClient)
	resolver.isDomainKind = isDomainKind
	return &Service{
		resolver:       resolver,
		loader:         NewLoader(kubeClient, archiveService),
		kubeClient:     kubeClient,
		domainRestorer: domainRestorer,
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

// compileNode compiles a RestoreNode in post-order: children are compiled first so their objects are
// emitted before the parent, then this node's manifests are loaded and compiled. A domain node is not
// compiled in-process: its whole subtree is delegated to the domain controller's aggregated apiserver.
func (s *Service) compileNode(ctx context.Context, node *RestoreNode, targetNamespace string) (NodeResult, error) {
	if node.Domain {
		return s.compileDomainNode(ctx, node, targetNamespace)
	}

	childObjects := make([]unstructured.Unstructured, 0)
	for _, child := range node.Children {
		childResult, err := s.compileNode(ctx, child, targetNamespace)
		if err != nil {
			return NodeResult{}, err
		}
		childObjects = append(childObjects, childResult.Objects...)
	}

	raw, err := s.loader.LoadManifests(ctx, node.ManifestCheckpointName)
	if err != nil {
		return NodeResult{}, err
	}
	nodeObjects, err := transformNodeObjects(node, raw, targetNamespace)
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

// compileDomainNode delegates a domain snapshot subtree to the domain controller's aggregated
// apiserver (it resolves and compiles its own subtree, fetching base manifests from core). The
// returned objects are already apply-ready; they are spliced as this node's result. It fails closed
// when no delegate is configured rather than silently dropping the subtree.
func (s *Service) compileDomainNode(ctx context.Context, node *RestoreNode, targetNamespace string) (NodeResult, error) {
	if s.domainRestorer == nil {
		return NodeResult{}, fmt.Errorf(
			"%w: domain snapshot %s %s/%s requires the domain restore delegate, which is not configured",
			ErrContractViolation, node.SnapshotRef.Kind, node.SnapshotRef.Namespace, node.SnapshotRef.Name,
		)
	}
	gvk := schema.FromAPIVersionAndKind(node.SnapshotRef.APIVersion, node.SnapshotRef.Kind)
	objects, err := s.domainRestorer.RestoreDomainSubtree(ctx, gvk, node.SnapshotRef.Namespace, node.SnapshotRef.Name, targetNamespace)
	if err != nil {
		return NodeResult{}, fmt.Errorf(
			"delegate domain restore for %s %s/%s: %w",
			node.SnapshotRef.Kind, node.SnapshotRef.Namespace, node.SnapshotRef.Name, err,
		)
	}
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
