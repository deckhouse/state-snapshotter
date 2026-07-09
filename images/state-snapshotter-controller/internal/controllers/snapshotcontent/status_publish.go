/*
Copyright 2026 Flant JSC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package snapshotcontent

import (
	"context"
	"errors"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

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

// PublishSnapshotContentChildrenRefs writes the snapshot-derived (domain) child content edges to
// contentName.status.childrenSnapshotContentRefs.
//
// domainRefs is the child set (derived from the owning snapshot's status.childrenSnapshotRefs — orphan/
// residual-PVC VolumeSnapshot children are ordinary domain children now, §11.6). The caller
// (PublishSnapshotContentChildrenFromSnapshotRefs) passes the COMPLETE set all-or-nothing, so in normal
// operation this writes the field ONCE (empty -> complete frozen set) and is a no-op thereafter — the
// frozen-set immutability CEL (Option A, INV-CONTENT-CHILDREN-2) rejects any later change. The merge
// preserves existing edges and adds any missing ones, deduped by name: on the single firing that union
// equals the complete set (existing is a subset), and it keeps an E3-degraded edge (child content deleted)
// rather than dropping it. The aggregator is the sole edge writer (INV-CONTENT-CHILDREN-1) as of Block 3d;
// the optimistic lock below is retained as defense in depth. The read is done via reader (the non-cached
// APIReader) so the preserve set reflects the freshest edges rather than a stale cache.
//
// Frozen-set guard: because the field is immutable once non-empty (Option A CEL), this NEVER attempts to
// grow or replace an already-populated set — that would be rejected by the apiserver and wedge the reconcile
// with a hard error. Only the empty -> complete first write is patched; a non-empty existing set is held
// as-is (see the guard below). This makes the writer upgrade-safe against a legacy partial set written under
// the old append-only rule.
func PublishSnapshotContentChildrenRefs(ctx context.Context, c client.Client, reader client.Reader, contentName string, domainRefs []storagev1alpha1.SnapshotContentChildRef) error {
	if contentName == "" {
		return nil
	}
	if reader == nil {
		reader = c
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		content := &storagev1alpha1.SnapshotContent{}
		if err := reader.Get(ctx, client.ObjectKey{Name: contentName}, content); err != nil {
			return err
		}
		desired := make([]storagev1alpha1.SnapshotContentChildRef, 0, len(domainRefs)+len(content.Status.ChildrenSnapshotContentRefs))
		seen := make(map[string]struct{}, len(domainRefs)+len(content.Status.ChildrenSnapshotContentRefs))
		// Preserve every existing edge first (append-only), then add new domain edges.
		for _, ref := range content.Status.ChildrenSnapshotContentRefs {
			if ref.Name == "" {
				continue
			}
			if _, ok := seen[ref.Name]; ok {
				continue
			}
			seen[ref.Name] = struct{}{}
			desired = append(desired, ref)
		}
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
		controllercommon.SortSnapshotContentChildRefs(desired)
		if controllercommon.SnapshotContentChildRefsEqualIgnoreOrder(content.Status.ChildrenSnapshotContentRefs, desired) {
			return nil
		}
		// Frozen-set guard (Block 4, INV-CONTENT-CHILDREN-2, Option A CEL: oldSelf.size()==0 || self==oldSelf).
		// Reaching here means desired differs from a NON-EMPTY existing set, i.e. an attempt to grow/replace an
		// already-populated frozen set. The all-or-nothing caller never produces this on a fresh deployment (the
		// field goes empty -> complete in one write and every later pass recomputes the identical complete set,
		// caught by the equality no-op above, incl. the E3-degraded re-publish). The only way to get here is a
		// LEGACY partial set carried across an upgrade from the append-only era: completing it would be rejected
		// by the apiserver CEL and turn every reconcile into a hard error, wedging the node in ChildrenLinkPending.
		// Hold the frozen set as-is instead (no patch) — the invariant says a non-empty set is immutable, so there
		// is nothing valid to write. Fresh deployments never take this branch.
		if len(content.Status.ChildrenSnapshotContentRefs) > 0 {
			return nil
		}
		base := content.DeepCopy()
		content.Status.ChildrenSnapshotContentRefs = desired
		// Optimistic lock (defense in depth): the aggregator is the sole edge writer as of Block 3d, but a
		// concurrent edit still turns into a 409 so RetryOnConflict re-reads the fresh list instead of
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
