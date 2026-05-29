package snapshotcontent

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/common"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
)

func PublishSnapshotContentManifestCheckpointName(ctx context.Context, c client.Client, contentName, mcpName string) error {
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

func PublishSnapshotContentChildrenRefs(ctx context.Context, c client.Client, contentName string, refs []storagev1alpha1.SnapshotContentChildRef) error {
	if contentName == "" {
		return nil
	}
	controllercommon.SortSnapshotContentChildRefs(refs)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		content := &storagev1alpha1.SnapshotContent{}
		if err := c.Get(ctx, client.ObjectKey{Name: contentName}, content); err != nil {
			return err
		}
		if controllercommon.SnapshotContentChildRefsEqualIgnoreOrder(content.Status.ChildrenSnapshotContentRefs, refs) {
			return nil
		}
		base := content.DeepCopy()
		content.Status.ChildrenSnapshotContentRefs = append([]storagev1alpha1.SnapshotContentChildRef(nil), refs...)
		return c.Status().Patch(ctx, content, client.MergeFrom(base))
	})
}

func PublishSnapshotContentChildrenFromSnapshotRefs(
	ctx context.Context,
	c client.Client,
	readClient client.Reader,
	parentNamespace string,
	parentContentName string,
	childSnapshotRefs []storagev1alpha1.SnapshotChildRef,
) (bool, error) {
	if readClient == nil {
		readClient = c
	}
	if parentContentName == "" {
		return false, nil
	}
	if len(childSnapshotRefs) == 0 {
		return true, PublishSnapshotContentChildrenRefs(ctx, c, parentContentName, nil)
	}
	parentContent := &storagev1alpha1.SnapshotContent{}
	if err := readClient.Get(ctx, client.ObjectKey{Name: parentContentName}, parentContent); err != nil {
		return false, err
	}
	out := make([]storagev1alpha1.SnapshotContentChildRef, 0, len(childSnapshotRefs))
	for _, childRef := range childSnapshotRefs {
		childContentName, err := usecase.ResolveChildSnapshotRefToBoundContentName(ctx, readClient, childRef, parentNamespace)
		if err != nil {
			if errors.Is(err, usecase.ErrRunGraphChildNotBound) ||
				errors.Is(err, usecase.ErrRunGraphChildSnapshotNotFound) {
				log.FromContext(ctx).Info("graph-publish-diag: child snapshot ref not ready for content publish",
					"parentContent", parentContentName, "parentNamespace", parentNamespace,
					"childAPIVersion", childRef.APIVersion, "childKind", childRef.Kind, "childName", childRef.Name,
					"reason", err.Error())
				return false, nil
			}
			return false, err
		}
		if childContentName == "" {
			log.FromContext(ctx).Info("graph-publish-diag: empty bound content for child snapshot ref",
				"parentContent", parentContentName, "childKind", childRef.Kind, "childName", childRef.Name)
			return false, nil
		}
		if err := ensureChildSnapshotContentOwnedByParentContent(ctx, c, childContentName, parentContent); err != nil {
			log.FromContext(ctx).Info("graph-publish-diag: ensure child content ownerRef failed",
				"parentContent", parentContentName, "childContent", childContentName, "error", err.Error())
			return false, err
		}
		out = append(out, storagev1alpha1.SnapshotContentChildRef{Name: childContentName})
	}
	return true, PublishSnapshotContentChildrenRefs(ctx, c, parentContentName, out)
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
		_, err := controllercommon.EnsureLifecycleOwnerRef(ctx, c, child, controllercommon.SnapshotContentOwnerReference(parent))
		return err
	})
}

func PublishSnapshotContentLeafChildrenRefs(ctx context.Context, c client.Client, contentName string) error {
	return PublishSnapshotContentChildrenRefs(ctx, c, contentName, nil)
}
