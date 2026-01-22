package restore

import (
	"context"
	"fmt"
	"strings"

	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/errors"
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
	contentGVKs, contentGVKByKind, err := r.listSnapshotContentGVKs(ctx)
	if err != nil {
		return nil, err
	}

	rootContent, err := r.findRootContent(ctx, contentGVKs, snapshotNamespace, snapshotName)
	if err != nil {
		return nil, err
	}

	return r.buildTree(ctx, contentGVKByKind, rootContent)
}

func (r *Resolver) listSnapshotContentGVKs(ctx context.Context) ([]schema.GroupVersionKind, map[string]schema.GroupVersionKind, error) {
	crdList := &extv1.CustomResourceDefinitionList{}
	if err := r.client.List(ctx, crdList); err != nil {
		return nil, nil, fmt.Errorf("failed to list CRDs: %w", err)
	}

	var gvks []schema.GroupVersionKind
	byKind := make(map[string]schema.GroupVersionKind)
	for _, crd := range crdList.Items {
		if !strings.HasSuffix(crd.Spec.Names.Kind, "SnapshotContent") {
			continue
		}
		var servedVersion string
		for _, version := range crd.Spec.Versions {
			if version.Served {
				servedVersion = version.Name
				break
			}
		}
		if servedVersion == "" {
			continue
		}
		gvk := schema.GroupVersionKind{
			Group:   crd.Spec.Group,
			Version: servedVersion,
			Kind:    crd.Spec.Names.Kind,
		}
		gvks = append(gvks, gvk)
		byKind[gvk.Kind] = gvk
	}

	if len(gvks) == 0 {
		return nil, nil, fmt.Errorf("no SnapshotContent CRDs found")
	}
	return gvks, byKind, nil
}

func (r *Resolver) findRootContent(ctx context.Context, gvks []schema.GroupVersionKind, snapshotNamespace, snapshotName string) (*unstructured.Unstructured, error) {
	var matches []*unstructured.Unstructured
	for _, gvk := range gvks {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk)
		if err := r.client.List(ctx, list); err != nil {
			return nil, fmt.Errorf("failed to list %s: %w", gvk.String(), err)
		}

		for i := range list.Items {
			item := &list.Items[i]
			contentLike, err := snapshot.ExtractSnapshotContentLike(item)
			if err != nil {
				continue
			}
			ref := contentLike.GetSpecSnapshotRef()
			if ref == nil {
				continue
			}
			if ref.Name == snapshotName && ref.Namespace == snapshotNamespace {
				matches = append(matches, item)
			}
		}
	}

	if len(matches) == 0 {
		return nil, errors.NewNotFound(schema.GroupResource{Group: "subresources.state-snapshotter.deckhouse.io", Resource: "snapshots"}, snapshotName)
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf("multiple SnapshotContent objects found for snapshot %s/%s", snapshotNamespace, snapshotName)
	}
	return matches[0], nil
}

func (r *Resolver) buildTree(ctx context.Context, gvkByKind map[string]schema.GroupVersionKind, root *unstructured.Unstructured) (*SnapshotContentNode, error) {
	contentLike, err := snapshot.ExtractSnapshotContentLike(root)
	if err != nil {
		return nil, fmt.Errorf("failed to parse SnapshotContent: %w", err)
	}
	if contentLike.GetStatusManifestCheckpointName() == "" {
		return nil, fmt.Errorf("manifestCheckpointName is empty for SnapshotContent %s", root.GetName())
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
	for _, child := range children {
		gvk, ok := gvkByKind[child.Kind]
		if !ok {
			return nil, fmt.Errorf("child SnapshotContent kind not found in registry: %s", child.Kind)
		}
		childObj := &unstructured.Unstructured{}
		childObj.SetGroupVersionKind(gvk)
		if err := r.client.Get(ctx, client.ObjectKey{Name: child.Name}, childObj); err != nil {
			return nil, fmt.Errorf("child SnapshotContent %s not found: %w", child.Name, err)
		}
		childNode, err := r.buildTree(ctx, gvkByKind, childObj)
		if err != nil {
			return nil, err
		}
		node.Children = append(node.Children, childNode)
	}
	return node, nil
}
