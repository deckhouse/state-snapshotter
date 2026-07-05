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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/deckhouse/state-snapshotter/api/names"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/common"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// ChildVolumeContentName returns the deterministic cluster-scoped SnapshotContent name for a child
// volume node of root, keyed by the orphan CSI VolumeSnapshot UID (the per-PVC leaf identity, unified
// wave4C scheme; see api/names). Variant A models every orphan PVC as its own volume node (≤1 dataRef
// per content), so a multi-PVC scope (root residual/orphan) fans out into one child SnapshotContent per
// PVC — each named for its own VolumeSnapshot — instead of a dataRefs[] list on the parent.
func ChildVolumeContentName(orphanVSUID types.UID) string {
	return names.ContentName(orphanVSUID)
}

// EnsureVolumeChildContent ensures the cluster-scoped child volume-node SnapshotContent backing one PVC.
// It is created on first call with a controlling lifecycle ownerRef to the root content (so GC of the
// root removes the child) and spec.deletionPolicy mirrored from the root (the whole spec is immutable,
// so it must be correct at creation). snapshotRef is the required back-reference to the orphan CSI
// VolumeSnapshot that binds this child via its status.boundSnapshotContentName (INV-ORPHAN4); the GC
// ownerRef (root content) is a separate lifecycle concern and is NOT the handshake subject. It returns
// the live child content for the caller to publish its dataRef / manifestCheckpointName onto. Used only
// by the root residual/orphan capture path (V4): domain leaves already are their own single-PVC node.
func EnsureVolumeChildContent(
	ctx context.Context,
	c client.Client,
	root *storagev1alpha1.SnapshotContent,
	orphanVSUID types.UID,
	snapshotRef *storagev1alpha1.SnapshotSubjectRef,
) (*storagev1alpha1.SnapshotContent, error) {
	childName := ChildVolumeContentName(orphanVSUID)
	child := &storagev1alpha1.SnapshotContent{}
	err := c.Get(ctx, client.ObjectKey{Name: childName}, child)
	if apierrors.IsNotFound(err) {
		// spec is immutable, so the deletionPolicy must be correct at creation. Mirror the root; fall back
		// to Retain when the root policy is empty so the child can never get stuck with an empty policy
		// (Retain is the safe default — the durable VSC is force-Retain regardless, see §3.9.11).
		deletionPolicy := root.Spec.DeletionPolicy
		if deletionPolicy == "" {
			deletionPolicy = storagev1alpha1.SnapshotContentDeletionPolicyRetain
		}
		child = &storagev1alpha1.SnapshotContent{
			ObjectMeta: metav1.ObjectMeta{
				Name:            childName,
				Labels:          map[string]string{snapshotpkg.LabelChildVolumeNode: "true"},
				OwnerReferences: []metav1.OwnerReference{controllercommon.SnapshotContentOwnerReference(root)},
			},
			Spec: controllercommon.NewSnapshotContentSpec(deletionPolicy, snapshotRef),
		}
		if cerr := c.Create(ctx, child); cerr != nil {
			if apierrors.IsAlreadyExists(cerr) {
				return getSnapshotContent(ctx, c, childName)
			}
			return nil, cerr
		}
		return child, nil
	}
	if err != nil {
		return nil, err
	}
	// Pre-existing child (e.g. created on a prior reconcile): make sure the root ownerRef is present so the
	// GC chain holds even if the content was created before the ownerRef write landed.
	if _, oerr := controllercommon.EnsureLifecycleOwnerRef(ctx, c, child, controllercommon.SnapshotContentOwnerReference(root)); oerr != nil {
		return nil, oerr
	}
	return child, nil
}

func getSnapshotContent(ctx context.Context, c client.Client, name string) (*storagev1alpha1.SnapshotContent, error) {
	content := &storagev1alpha1.SnapshotContent{}
	if err := c.Get(ctx, client.ObjectKey{Name: name}, content); err != nil {
		return nil, err
	}
	return content, nil
}

// LinkChildVolumeContentRef idempotently adds childName to root.status.childrenSnapshotContentRefs so the
// content graph (subtree manifest exclude, GC, restore traversal) sees the child volume node.
//
// The current edge set is read via reader (the non-cached APIReader): the append is otherwise computed
// against a possibly stale cache that may not yet show the snapshot-derived domain child edges, and a
// MergeFrom patch off that stale base would clobber them — exactly the write-war that PublishSnapshot-
// ContentChildrenRefs guards against from the other side. A fresh read makes this strictly additive.
func LinkChildVolumeContentRef(ctx context.Context, c client.Client, reader client.Reader, rootContentName, childName string) error {
	if rootContentName == "" || childName == "" {
		return nil
	}
	if reader == nil {
		reader = c
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		root := &storagev1alpha1.SnapshotContent{}
		if err := reader.Get(ctx, client.ObjectKey{Name: rootContentName}, root); err != nil {
			return err
		}
		for _, ref := range root.Status.ChildrenSnapshotContentRefs {
			if ref.Name == childName {
				return nil
			}
		}
		base := root.DeepCopy()
		root.Status.ChildrenSnapshotContentRefs = append(root.Status.ChildrenSnapshotContentRefs,
			storagev1alpha1.SnapshotContentChildRef{Name: childName})
		controllercommon.SortSnapshotContentChildRefs(root.Status.ChildrenSnapshotContentRefs)
		// Optimistic lock: childrenSnapshotContentRefs is co-written by the domain publisher
		// (PublishSnapshotContentChildrenRefs); a concurrent edit turns into a 409 so RetryOnConflict
		// re-reads the fresh list and re-appends, instead of blindly replacing (which would drop the
		// domain child edges). Matches genericbinder.patchSnapshotConditionFromContent.
		return c.Status().Patch(ctx, root, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	})
}
