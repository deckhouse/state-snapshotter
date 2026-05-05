package controllers

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
)

func publishSnapshotContentManifestCheckpointName(ctx context.Context, c client.Client, contentName, mcpName string) error {
	if contentName == "" || mcpName == "" {
		return nil
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		content := &storagev1alpha1.SnapshotContent{}
		if err := c.Get(ctx, client.ObjectKey{Name: contentName}, content); err != nil {
			return err
		}
		if content.Status.ManifestCheckpointName == mcpName {
			return nil
		}
		base := content.DeepCopy()
		content.Status.ManifestCheckpointName = mcpName
		return c.Status().Patch(ctx, content, client.MergeFrom(base))
	})
}

func publishSnapshotContentChildrenRefs(ctx context.Context, c client.Client, contentName string, refs []storagev1alpha1.SnapshotContentChildRef) error {
	if contentName == "" {
		return nil
	}
	sortSnapshotContentChildRefs(refs)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		content := &storagev1alpha1.SnapshotContent{}
		if err := c.Get(ctx, client.ObjectKey{Name: contentName}, content); err != nil {
			return err
		}
		if snapshotContentChildRefsEqualIgnoreOrder(content.Status.ChildrenSnapshotContentRefs, refs) {
			return nil
		}
		base := content.DeepCopy()
		content.Status.ChildrenSnapshotContentRefs = append([]storagev1alpha1.SnapshotContentChildRef(nil), refs...)
		return c.Status().Patch(ctx, content, client.MergeFrom(base))
	})
}

func publishSnapshotContentChildrenFromSnapshotRefs(
	ctx context.Context,
	c client.Client,
	parentNamespace string,
	parentContentName string,
	childSnapshotRefs []storagev1alpha1.SnapshotChildRef,
) (bool, error) {
	if parentContentName == "" {
		return false, nil
	}
	if len(childSnapshotRefs) == 0 {
		return true, publishSnapshotContentChildrenRefs(ctx, c, parentContentName, nil)
	}
	parentContent := &storagev1alpha1.SnapshotContent{}
	if err := c.Get(ctx, client.ObjectKey{Name: parentContentName}, parentContent); err != nil {
		return false, err
	}
	out := make([]storagev1alpha1.SnapshotContentChildRef, 0, len(childSnapshotRefs))
	for _, childRef := range childSnapshotRefs {
		childContentName, err := usecase.ResolveChildSnapshotRefToBoundContentName(ctx, c, childRef, parentNamespace)
		if err != nil {
			if errors.Is(err, usecase.ErrRunGraphChildNotBound) ||
				errors.Is(err, usecase.ErrRunGraphChildSnapshotNotFound) {
				return false, nil
			}
			return false, err
		}
		if childContentName == "" {
			return false, nil
		}
		if err := ensureChildSnapshotContentOwnedByParentContent(ctx, c, childContentName, parentContent); err != nil {
			return false, err
		}
		out = append(out, storagev1alpha1.SnapshotContentChildRef{Name: childContentName})
	}
	return true, publishSnapshotContentChildrenRefs(ctx, c, parentContentName, out)
}

func ensureChildSnapshotContentOwnedByParentContent(ctx context.Context, c client.Client, childName string, parent *storagev1alpha1.SnapshotContent) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		child := &storagev1alpha1.SnapshotContent{}
		if err := c.Get(ctx, client.ObjectKey{Name: childName}, child); err != nil {
			if apierrors.IsNotFound(err) {
				return fmt.Errorf("child SnapshotContent %s not found", childName)
			}
			return err
		}
		_, err := ensureLifecycleOwnerRef(ctx, c, child, snapshotContentOwnerReference(parent))
		return err
	})
}

func publishSnapshotContentLeafChildrenRefs(ctx context.Context, c client.Client, contentName string) error {
	return publishSnapshotContentChildrenRefs(ctx, c, contentName, nil)
}
