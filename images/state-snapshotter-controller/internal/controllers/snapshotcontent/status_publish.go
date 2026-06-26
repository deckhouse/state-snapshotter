package snapshotcontent

import (
	"context"
	"errors"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/common"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
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

// PublishSnapshotContentChildrenRefs sets the snapshot-derived (domain) child content edges on
// contentName.status.childrenSnapshotContentRefs.
//
// domainRefs is the authoritative DOMAIN child set (derived from the owning snapshot's
// status.childrenSnapshotRefs). It is published as a full replacement of the domain edges, but
// child-volume-node edges (orphan/root-residual PVC nodes, named <contentName>-vol-<hash>) are
// PRESERVED: those are linked by a separate writer (LinkChildVolumeContentRef) and are not part of
// the snapshot-derived set, so a blind full-replace here would clobber them and start a write-war
// with that linker (each side repeatedly removing the other's edge, churning resourceVersion and
// livelocking the optimistic status update that publishes Ready). The read is done via reader
// (the non-cached APIReader) so the preserve set reflects edges just written by the other writer
// rather than a stale cache that would re-clobber them.
func PublishSnapshotContentChildrenRefs(ctx context.Context, c client.Client, reader client.Reader, contentName string, domainRefs []storagev1alpha1.SnapshotContentChildRef) error {
	if contentName == "" {
		return nil
	}
	if reader == nil {
		reader = c
	}
	// Anchored to THIS content's own vol-node naming (ChildVolumeContentName = contentName + infix +
	// hash). Unlike a bare infix scan this cannot misclassify a domain child: domain child content
	// names derive from the child snapshot, never from the parent content name + "-vol-".
	volNodePrefix := contentName + snapshotpkg.ChildVolumeContentInfix
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		content := &storagev1alpha1.SnapshotContent{}
		if err := reader.Get(ctx, client.ObjectKey{Name: contentName}, content); err != nil {
			return err
		}
		desired := make([]storagev1alpha1.SnapshotContentChildRef, 0, len(domainRefs)+len(content.Status.ChildrenSnapshotContentRefs))
		seen := make(map[string]struct{}, len(domainRefs))
		for _, ref := range domainRefs {
			if ref.Name == "" {
				continue
			}
			if _, ok := seen[ref.Name]; ok {
				continue
			}
			seen[ref.Name] = struct{}{}
			desired = append(desired, ref)
		}
		for _, ref := range content.Status.ChildrenSnapshotContentRefs {
			if _, ok := seen[ref.Name]; ok {
				continue
			}
			if strings.HasPrefix(ref.Name, volNodePrefix) {
				seen[ref.Name] = struct{}{}
				desired = append(desired, ref)
			}
		}
		controllercommon.SortSnapshotContentChildRefs(desired)
		if controllercommon.SnapshotContentChildRefsEqualIgnoreOrder(content.Status.ChildrenSnapshotContentRefs, desired) {
			return nil
		}
		base := content.DeepCopy()
		content.Status.ChildrenSnapshotContentRefs = desired
		// Optimistic lock: childrenSnapshotContentRefs is co-written by LinkChildVolumeContentRef; a
		// concurrent edit turns into a 409 so RetryOnConflict re-reads the fresh (merged) list instead of
		// blindly replacing it (matches the convention in genericbinder.patchSnapshotConditionFromContent).
		return c.Status().Patch(ctx, content, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
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
		return true, PublishSnapshotContentChildrenRefs(ctx, c, readClient, parentContentName, nil)
	}
	parentContent := &storagev1alpha1.SnapshotContent{}
	if err := readClient.Get(ctx, client.ObjectKey{Name: parentContentName}, parentContent); err != nil {
		return false, err
	}
	alreadyPublished := make(map[string]struct{}, len(parentContent.Status.ChildrenSnapshotContentRefs))
	for _, ref := range parentContent.Status.ChildrenSnapshotContentRefs {
		alreadyPublished[ref.Name] = struct{}{}
	}
	out := make([]storagev1alpha1.SnapshotContentChildRef, 0, len(childSnapshotRefs))
	for _, childRef := range childSnapshotRefs {
		if snapshotpkg.IsVolumeSnapshotVisibilityLeaf(childRef) {
			continue
		}
		childContentName, err := usecase.ResolveChildSnapshotRefToBoundContentName(ctx, readClient, childRef, parentNamespace)
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
		found, err := ensureChildSnapshotContentOwnedByParentContent(ctx, c, childContentName, parentContent)
		if err != nil {
			return false, err
		}
		if !found {
			// The child snapshot is bound but its SnapshotContent object is currently absent. Two cases:
			//   - degradation (E3): the edge was already published while the content existed, then the content
			//     was deleted. Preserve the edge so the parent keeps aggregating it as pending
			//     (ChildrenReady=False) — that is how a degraded subtree reaches the root Snapshot.Ready
			//     mirror. Dropping it (or hard-erroring) wedged the parent reconcile and froze Ready at its
			//     last value (root stayed Ready=True even though its content went Ready=False).
			//   - initial-bind / cache lag: the edge is NOT published yet. Do NOT introduce a dangling edge to
			//     a missing content (the later root capture-planning subtree walk would have to resolve it);
			//     requeue until the content becomes visible, matching the pre-existing wait behavior.
			if _, ok := alreadyPublished[childContentName]; !ok {
				return false, nil
			}
			out = append(out, storagev1alpha1.SnapshotContentChildRef{Name: childContentName})
			continue
		}
		out = append(out, storagev1alpha1.SnapshotContentChildRef{Name: childContentName})
	}
	return true, PublishSnapshotContentChildrenRefs(ctx, c, readClient, parentContentName, out)
}

// ensureChildSnapshotContentOwnedByParentContent links parent as a lifecycle owner of the child content
// and reports whether the child content currently exists. A missing child content yields found=false
// WITHOUT an error: the child snapshot can publish its boundSnapshotContentName before the content object
// is created, and a degraded subtree may have had its bound content deleted (E3). Treating NotFound as a
// hard error wedged the parent reconcile and blocked Ready propagation, so callers instead keep the child
// ref tracked (pending) regardless of found.
func ensureChildSnapshotContentOwnedByParentContent(ctx context.Context, c client.Client, childName string, parent *storagev1alpha1.SnapshotContent) (bool, error) {
	found := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		child := &storagev1alpha1.SnapshotContent{}
		if err := c.Get(ctx, client.ObjectKey{Name: childName}, child); err != nil {
			if apierrors.IsNotFound(err) {
				found = false
				return nil
			}
			return err
		}
		found = true
		_, err := controllercommon.EnsureLifecycleOwnerRef(ctx, c, child, controllercommon.SnapshotContentOwnerReference(parent))
		return err
	})
	return found, err
}

func PublishSnapshotContentLeafChildrenRefs(ctx context.Context, c client.Client, reader client.Reader, contentName string) error {
	return PublishSnapshotContentChildrenRefs(ctx, c, reader, contentName, nil)
}
