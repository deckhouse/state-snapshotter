package restore

import (
	"context"
	"fmt"
	"sort"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

type Resolver struct {
	client client.Client
}

func NewResolver(client client.Client) *Resolver {
	return &Resolver{client: client}
}

func (r *Resolver) ResolveSnapshotTree(ctx context.Context, snapshotNamespace, snapshotName string) (*SnapshotContentNode, error) {
	snapshotGVK := schema.GroupVersionKind{
		Group:   "state-snapshotter.deckhouse.io",
		Version: "v1alpha1",
		Kind:    "Snapshot",
	}
	snapshotObj := &unstructured.Unstructured{}
	snapshotObj.SetGroupVersionKind(snapshotGVK)
	if err := r.client.Get(ctx, client.ObjectKey{Namespace: snapshotNamespace, Name: snapshotName}, snapshotObj); err != nil {
		if errors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: snapshot %s/%s", ErrNotFound, snapshotNamespace, snapshotName)
		}
		return nil, fmt.Errorf("failed to get snapshot %s/%s: %w", snapshotNamespace, snapshotName, err)
	}
	if err := ensureSnapshotReady(snapshotObj); err != nil {
		return nil, err
	}

	snapshotLike, err := snapshot.ExtractSnapshotLike(snapshotObj)
	if err != nil {
		return nil, err
	}

	contentGVK := schema.GroupVersionKind{
		Group:   snapshotGVK.Group,
		Version: snapshotGVK.Version,
		Kind:    snapshotGVK.Kind + "Content",
	}

	boundName := snapshotLike.GetStatusContentName()
	if boundName != "" {
		rootContent := &unstructured.Unstructured{}
		rootContent.SetGroupVersionKind(contentGVK)
		if err := r.client.Get(ctx, client.ObjectKey{Name: boundName}, rootContent); err != nil {
			if errors.IsNotFound(err) {
				return nil, fmt.Errorf("%w: bound SnapshotContent %s not found", ErrContractViolation, boundName)
			}
			return nil, fmt.Errorf("failed to get bound SnapshotContent %s: %w", boundName, err)
		}
		if err := ensureReady(rootContent); err != nil {
			return nil, err
		}
		return r.buildTree(ctx, contentGVK, rootContent)
	}

	return nil, fmt.Errorf("%w: snapshot %s/%s has empty boundSnapshotContentName", ErrNotReady, snapshotNamespace, snapshotName)
}

func (r *Resolver) buildTree(ctx context.Context, contentGVK schema.GroupVersionKind, root *unstructured.Unstructured) (*SnapshotContentNode, error) {
	contentLike, err := snapshot.ExtractSnapshotContentLike(root)
	if err != nil {
		return nil, fmt.Errorf("%w: failed to parse SnapshotContent", ErrContractViolation)
	}
	if contentLike.GetStatusManifestCheckpointName() == "" {
		return nil, fmt.Errorf("%w: manifestCheckpointName is empty for SnapshotContent %s", ErrContractViolation, root.GetName())
	}

	node := &SnapshotContentNode{
		Content:                root,
		ManifestCheckpointName: contentLike.GetStatusManifestCheckpointName(),
	}
	if dataRef := contentLike.GetStatusDataRef(); dataRef != nil {
		node.DataRefKind = dataRef.Kind
		node.DataRefName = dataRef.Name
	}

	children := contentLike.GetStatusChildrenSnapshotContentRefs()
	sort.Slice(children, func(i, j int) bool {
		if children[i].Kind == children[j].Kind {
			return children[i].Name < children[j].Name
		}
		return children[i].Kind < children[j].Kind
	})
	for _, child := range children {
		gvk := contentGVK
		if child.Kind != "" && child.Kind != contentGVK.Kind {
			return nil, fmt.Errorf("%w: child SnapshotContent kind mismatch: %s", ErrContractViolation, child.Kind)
		}
		childObj := &unstructured.Unstructured{}
		childObj.SetGroupVersionKind(gvk)
		if err := r.client.Get(ctx, client.ObjectKey{Name: child.Name}, childObj); err != nil {
			return nil, fmt.Errorf("%w: child SnapshotContent %s not found", ErrContractViolation, child.Name)
		}
		if err := ensureReady(childObj); err != nil {
			return nil, err
		}
		childNode, err := r.buildTree(ctx, contentGVK, childObj)
		if err != nil {
			return nil, err
		}
		node.Children = append(node.Children, childNode)
	}
	return node, nil
}

func ensureReady(obj *unstructured.Unstructured) error {
	contentLike, err := snapshot.ExtractSnapshotContentLike(obj)
	if err != nil {
		return fmt.Errorf("%w: failed to parse SnapshotContent", ErrContractViolation)
	}
	conditions := contentLike.GetStatusConditions()
	ready := meta.FindStatusCondition(conditions, "Ready")
	if ready == nil || ready.Status != metav1.ConditionTrue {
		return fmt.Errorf("%w: SnapshotContent %s is not Ready", ErrNotReady, obj.GetName())
	}
	return nil
}

func ensureSnapshotReady(snapshotObj *unstructured.Unstructured) error {
	snapshotLike, err := snapshot.ExtractSnapshotLike(snapshotObj)
	if err != nil {
		return fmt.Errorf("%w: failed to parse Snapshot", ErrContractViolation)
	}
	conditions := snapshotLike.GetStatusConditions()
	ready := meta.FindStatusCondition(conditions, "Ready")
	if ready == nil {
		return nil
	}
	if ready.Status != metav1.ConditionTrue {
		return fmt.Errorf("%w: Snapshot %s is not Ready", ErrNotReady, snapshotObj.GetName())
	}
	return nil
}
