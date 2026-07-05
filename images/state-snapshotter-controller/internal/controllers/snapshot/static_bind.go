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

package snapshot

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/deckhouse/state-snapshotter/api/names"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/common"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/snapshotcontent"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// staticBindContentPollInterval is how often a static-bind Snapshot re-checks for its referenced
// (not-yet-created) pre-provisioned SnapshotContent before the import controller materializes it.
const staticBindContentPollInterval = 2 * time.Second

// reconcileStaticBind implements CSI-like static (pre-provisioning) binding for a root Snapshot whose
// spec.source.snapshotContentName references an already-existing cluster-scoped SnapshotContent.
//
// It mirrors the VolumeSnapshot <-> VolumeSnapshotContent handshake: the bind succeeds only when the
// referenced content points back at this Snapshot via spec.snapshotRef. The whole capture pipeline
// (ObjectKeeper re-own, MCR/VCR, manifest checkpoint, child graph) is skipped: the content already
// carries a manifestCheckpointName and dataRefs from the import path. The Snapshot's Ready is then a
// pure mirror of the bound content's Ready.
func (r *SnapshotReconciler) reconcileStaticBind(ctx context.Context, nsSnap *storagev1alpha1.Snapshot) (ctrl.Result, error) {
	contentName := nsSnap.Spec.Source.SnapshotContentName

	content := &storagev1alpha1.SnapshotContent{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: contentName}, content); err != nil {
		if apierrors.IsNotFound(err) {
			// The import controller may not have created the content yet; retry without a terminal failure.
			if _, ferr := r.failCapture(ctx, nsSnap, nil, snapshotpkg.ReasonSourceContentNotFound,
				fmt.Sprintf("pre-provisioned SnapshotContent %q not found", contentName)); ferr != nil {
				return ctrl.Result{}, ferr
			}
			return ctrl.Result{RequeueAfter: staticBindContentPollInterval}, nil
		}
		return ctrl.Result{}, err
	}

	// Static-binding handshake: the content MUST point back at this Snapshot. A mismatch is a permanent
	// misconfiguration (cross-binding two snapshots to one content), so fail terminally.
	if !staticBindRefMatches(content.Spec.SnapshotRef, nsSnap) {
		return r.failCapture(ctx, nsSnap, nil, snapshotpkg.ReasonSnapshotContentMisbound,
			fmt.Sprintf("SnapshotContent %q spec.snapshotRef does not point back at Snapshot %s/%s", contentName, nsSnap.Namespace, nsSnap.Name))
	}

	// Bind once: a static bind never points at the deterministic capture name, so the main reconcile's
	// expectedName reset MUST NOT run for these snapshots (the caller branches before it).
	if nsSnap.Status.BoundSnapshotContentName != contentName {
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			cur := &storagev1alpha1.Snapshot{}
			if err := r.Client.Get(ctx, client.ObjectKey{Namespace: nsSnap.Namespace, Name: nsSnap.Name}, cur); err != nil {
				return err
			}
			cur.Status.BoundSnapshotContentName = contentName
			return r.Client.Status().Update(ctx, cur)
		}); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// A statically-bound content has no residual/orphan-PVC wave of its own (it was materialized by the
	// import path or pre-provisioned out of band). Latch the residual gate Complete (idempotent: import
	// content already carries it; pre-provisioned content gets it here) so the aggregator's fail-closed
	// residual gate cannot hold the mirror's first Ready=True forever.
	if err := snapshotcontent.MarkResidualVolumeCaptureComplete(ctx, r.Client, content.Name, nil); err != nil {
		return ctrl.Result{}, err
	}

	// Recycle-bin restore orchestration (wave4B): the surviving content tree that outlived its namespaced
	// Snapshot is re-attached here. Walk the durable childrenSnapshotContentRefs graph, idempotently
	// re-create each domain XxxxSnapshot child as a StaticBind CR (owned by this Snapshot), and re-point
	// every child content's spec.snapshotRef at its re-created CR (relaxed-CEL, gated on parentDeleted).
	// The domain genericbinder then binds each child and mirrors Ready upward, so the root's own Ready
	// mirror below reflects the whole re-attached subtree. Idempotent: a no-op once fully re-attached.
	if requeue, err := r.reconcileStaticBindRestoreTree(ctx, nsSnap, content); err != nil {
		return ctrl.Result{}, err
	} else if requeue {
		return ctrl.Result{RequeueAfter: staticBindContentPollInterval}, nil
	}

	// Steady state: mirror the bound content's Ready condition onto the Snapshot (single-aggregator
	// contract, INV-COND4). If the content is not Ready yet, the mirror sets a pending reason.
	if err := r.mirrorSnapshotReadyFromBoundContent(ctx, nsSnap, content, nil); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// reconcileStaticBindRestoreTree re-materializes the child subtree of a recycle-bin restore. It walks the
// durable SnapshotContent graph rooted at rootContent (status.childrenSnapshotContentRefs) and for every
// child:
//   - domain child content -> re-create its XxxxSnapshot CR (StaticBind) and re-point the content's
//     back-reference at it, then recurse into that child's own children;
//   - orphan CSI volume-node leaf (LabelChildVolumeNode) -> re-create the CSI VolumeSnapshot handle
//     (a NEW uid, statically bound to the surviving durable VolumeSnapshotContent), re-point the leaf
//     content's back-reference at it, and write the INV-ORPHAN4 handle
//     (VolumeSnapshot.status.boundSnapshotContentName). The durable leaf content itself is NOT re-created
//     (that would orphan its dataRef -> VolumeSnapshotContent).
//
// It also reconstructs the root Snapshot's status.childrenSnapshotRefs (the Snapshot-tree the restore
// resolver walks) from rootContent's direct children, since a StaticBind root runs no capture wave to
// populate them. It returns requeue=true whenever it mutated cluster state, so the caller re-runs
// promptly; in steady state it returns false and performs no writes.
func (r *SnapshotReconciler) reconcileStaticBindRestoreTree(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	rootContent *storagev1alpha1.SnapshotContent,
) (bool, error) {
	changed := false
	visited := map[string]struct{}{}
	var rootChildRefs []storagev1alpha1.SnapshotChildRef

	type queued struct {
		content *storagev1alpha1.SnapshotContent
		isRoot  bool
	}
	queue := []queued{{content: rootContent, isRoot: true}}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if _, ok := visited[cur.content.Name]; ok {
			continue
		}
		visited[cur.content.Name] = struct{}{}

		for _, childRef := range cur.content.Status.ChildrenSnapshotContentRefs {
			childContent := &storagev1alpha1.SnapshotContent{}
			if err := r.Client.Get(ctx, client.ObjectKey{Name: childRef.Name}, childContent); err != nil {
				if apierrors.IsNotFound(err) {
					// A durable child ref whose content is gone: nothing to re-attach; skip.
					continue
				}
				return false, err
			}

			if snapshotpkg.IsChildVolumeNodeContent(childContent) {
				// Orphan volume-node leaf: re-create the VolumeSnapshot handle + re-point, keep the content.
				snapRef, ch, err := r.ensureRestoredOrphanVolumeLeaf(ctx, nsSnap, childContent)
				if err != nil {
					return false, err
				}
				changed = changed || ch
				if cur.isRoot && snapRef != nil {
					rootChildRefs = append(rootChildRefs, *snapRef)
				}
				continue
			}

			snapRef, ch, err := r.ensureRestoredDomainSnapshot(ctx, nsSnap, childContent)
			if err != nil {
				return false, err
			}
			changed = changed || ch
			if cur.isRoot && snapRef != nil {
				rootChildRefs = append(rootChildRefs, *snapRef)
			}
			queue = append(queue, queued{content: childContent})
		}
	}

	// Reconstruct the root Snapshot's childrenSnapshotRefs (Snapshot-tree) so the restore resolver can
	// walk the re-attached subtree (a StaticBind root runs no capture wave to publish them).
	refsChanged, err := r.ensureRestoredRootSnapshotChildRefs(ctx, nsSnap, rootChildRefs)
	if err != nil {
		return false, err
	}
	return changed || refsChanged, nil
}

// ensureRestoredDomainSnapshot idempotently re-creates the domain XxxxSnapshot CR that owns childContent
// (as a StaticBind leaf pointing back at the surviving content) and re-points childContent.spec.snapshotRef
// onto the re-created CR. The re-created CR is named deterministically from (root Snapshot UID, child
// content UID) so repeated reconciles converge on the same object, and it is owned by the root Snapshot so
// the whole restored view is garbage-collected together while the durable content survives in the bin.
//
// It returns the Snapshot-tree child ref for the re-created CR (for the parent's childrenSnapshotRefs) and
// changed=true when it created the CR or re-pointed the content.
func (r *SnapshotReconciler) ensureRestoredDomainSnapshot(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	childContent *storagev1alpha1.SnapshotContent,
) (*storagev1alpha1.SnapshotChildRef, bool, error) {
	ref := childContent.Spec.SnapshotRef
	if ref == nil {
		return nil, false, fmt.Errorf("child SnapshotContent %q has no spec.snapshotRef to restore", childContent.Name)
	}
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return nil, false, fmt.Errorf("child SnapshotContent %q snapshotRef.apiVersion %q: %w", childContent.Name, ref.APIVersion, err)
	}
	gvk := gv.WithKind(ref.Kind)
	name := names.ChildSnapshotName(nsSnap.UID, childContent.UID)
	key := client.ObjectKey{Namespace: nsSnap.Namespace, Name: name}

	domainCR := &unstructured.Unstructured{}
	domainCR.SetGroupVersionKind(gvk)
	created := false
	if err := r.Client.Get(ctx, key, domainCR); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, false, err
		}
		desired := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": gvk.GroupVersion().String(),
			"kind":       gvk.Kind,
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": nsSnap.Namespace,
			},
			"spec": map[string]interface{}{
				"mode": string(storagev1alpha1.SnapshotModeStaticBind),
				"source": map[string]interface{}{
					"snapshotContentName": childContent.Name,
				},
			},
		}}
		desired.SetGroupVersionKind(gvk)
		desired.SetOwnerReferences([]metav1.OwnerReference{
			controllercommon.SnapshotOwnerReference(storagev1alpha1.SchemeGroupVersion.String(), controllercommon.KindSnapshot, nsSnap.Name, nsSnap.UID),
		})
		if cerr := r.Client.Create(ctx, desired); cerr != nil && !apierrors.IsAlreadyExists(cerr) {
			return nil, false, cerr
		} else if cerr == nil {
			created = true
		}
		// Re-read (whether we created it or lost a create race) to obtain the assigned UID for the re-point.
		domainCR = &unstructured.Unstructured{}
		domainCR.SetGroupVersionKind(gvk)
		if gerr := r.Client.Get(ctx, key, domainCR); gerr != nil {
			return nil, false, gerr
		}
	}

	// Re-point the child content's back-reference onto the re-created CR (relaxed-CEL, gated on
	// status.parentDeleted). Only then does the domain binder's static-bind handshake accept the bind.
	repointed, err := r.repointContentSnapshotRef(ctx, childContent.Name, storagev1alpha1.SnapshotSubjectRef{
		APIVersion: gvk.GroupVersion().String(),
		Kind:       gvk.Kind,
		Namespace:  nsSnap.Namespace,
		Name:       name,
		UID:        domainCR.GetUID(),
	})
	if err != nil {
		return nil, false, err
	}
	return &storagev1alpha1.SnapshotChildRef{
		APIVersion: gvk.GroupVersion().String(),
		Kind:       gvk.Kind,
		Name:       name,
	}, created || repointed, nil
}

// ensureRestoredOrphanVolumeLeaf re-attaches one orphan volume-node leaf on restore (Variant A). It does
// NOT re-create the durable leaf SnapshotContent (which still carries the PVC manifest + the dataRef to
// the surviving Retain VolumeSnapshotContent); instead it re-creates the namespaced CSI VolumeSnapshot
// HANDLE that was GC'd with the original Snapshot, statically bound to that surviving VolumeSnapshotContent
// (spec.source.volumeSnapshotContentName). The re-created handle gets a NEW uid, so the leaf content's
// back-reference is re-pointed (relaxed-CEL) onto it and the INV-ORPHAN4 handle
// (VolumeSnapshot.status.boundSnapshotContentName -> leaf content) is (re)written.
//
// It returns the Snapshot-tree VolumeSnapshot leaf ref (for the parent's childrenSnapshotRefs) and
// changed=true when it created the handle, re-pointed the content, or wrote the handle.
func (r *SnapshotReconciler) ensureRestoredOrphanVolumeLeaf(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	leafContent *storagev1alpha1.SnapshotContent,
) (*storagev1alpha1.SnapshotChildRef, bool, error) {
	dataRef := leafContent.Status.DataRef
	if dataRef == nil || dataRef.Artifact.Name == "" {
		return nil, false, fmt.Errorf("orphan child SnapshotContent %q has no dataRef artifact to restore", leafContent.Name)
	}
	pvcUID := types.UID(dataRef.TargetUID)
	vscName := dataRef.Artifact.Name
	vsName := names.OrphanVolumeSnapshotName(nsSnap.UID, pvcUID)
	key := client.ObjectKey{Namespace: nsSnap.Namespace, Name: vsName}

	vs := &unstructured.Unstructured{}
	vs.SetGroupVersionKind(csiVolumeSnapshotGVK)
	created := false
	if err := r.Client.Get(ctx, key, vs); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, false, err
		}
		desired := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": snapshotpkg.CSISnapshotAPIVersion,
			"kind":       snapshotpkg.KindVolumeSnapshot,
			"metadata": map[string]interface{}{
				"name":      vsName,
				"namespace": nsSnap.Namespace,
				"ownerReferences": []interface{}{
					ownerRefToMap(volumeSnapshotOwnerReferenceForSnapshot(nsSnap)),
				},
			},
			// Pre-provisioned (static) source: bind to the surviving durable VolumeSnapshotContent rather
			// than re-snapshotting the (possibly gone) PVC. The CSI snapshot-controller re-binds the
			// existing VSC and sets status.readyToUse/boundVolumeSnapshotContentName.
			"spec": map[string]interface{}{
				"source": map[string]interface{}{
					"volumeSnapshotContentName": vscName,
				},
			},
		}}
		desired.SetGroupVersionKind(csiVolumeSnapshotGVK)
		if cerr := r.Client.Create(ctx, desired); cerr != nil && !apierrors.IsAlreadyExists(cerr) {
			return nil, false, cerr
		} else if cerr == nil {
			created = true
		}
		vs = &unstructured.Unstructured{}
		vs.SetGroupVersionKind(csiVolumeSnapshotGVK)
		if gerr := r.Client.Get(ctx, key, vs); gerr != nil {
			return nil, false, gerr
		}
	}

	// Re-point the leaf content's back-reference onto the re-created handle's NEW uid (relaxed-CEL).
	repointed, err := r.repointContentSnapshotRef(ctx, leafContent.Name, storagev1alpha1.SnapshotSubjectRef{
		APIVersion: snapshotpkg.CSISnapshotAPIVersion,
		Kind:       snapshotpkg.KindVolumeSnapshot,
		Namespace:  nsSnap.Namespace,
		Name:       vsName,
		UID:        vs.GetUID(),
	})
	if err != nil {
		return nil, false, err
	}

	// (Re)write the INV-ORPHAN4 handle so the resolver can reach the durable leaf content from the VS.
	bound, err := r.setOrphanVSBoundContent(ctx, key, leafContent.Name)
	if err != nil {
		return nil, false, err
	}

	return &storagev1alpha1.SnapshotChildRef{
		APIVersion: snapshotpkg.CSISnapshotAPIVersion,
		Kind:       snapshotpkg.KindVolumeSnapshot,
		Name:       vsName,
	}, created || repointed || bound, nil
}

// setOrphanVSBoundContent writes VolumeSnapshot.status.boundSnapshotContentName -> childContentName
// (INV-ORPHAN4 handle) under an optimistic-lock merge patch, returning changed=true only when it had to
// write. It is the restore twin of bindOrphanVSToChildContent (capture path) addressed by VS key.
func (r *SnapshotReconciler) setOrphanVSBoundContent(ctx context.Context, key client.ObjectKey, childContentName string) (bool, error) {
	changed := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		vs := &unstructured.Unstructured{}
		vs.SetGroupVersionKind(csiVolumeSnapshotGVK)
		if err := r.Client.Get(ctx, key, vs); err != nil {
			return err
		}
		curName, _, _ := unstructured.NestedString(vs.Object, "status", "boundSnapshotContentName")
		if curName == childContentName {
			changed = false
			return nil
		}
		base := vs.DeepCopy()
		if err := unstructured.SetNestedField(vs.Object, childContentName, "status", "boundSnapshotContentName"); err != nil {
			return err
		}
		if err := r.Client.Status().Patch(ctx, vs, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		changed = true
		return nil
	})
	return changed, err
}

// ensureRestoredRootSnapshotChildRefs sets the root Snapshot's status.childrenSnapshotRefs to the
// reconstructed set (domain child CRs + orphan VolumeSnapshot leaves), preserving order-independence and
// writing only on change. Restore fully owns this Snapshot-tree reconstruction (a StaticBind root runs no
// capture wave), so the desired set replaces the previous one rather than merging.
func (r *SnapshotReconciler) ensureRestoredRootSnapshotChildRefs(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	desired []storagev1alpha1.SnapshotChildRef,
) (bool, error) {
	controllercommon.SortSnapshotChildRefs(desired)
	changed := false
	key := client.ObjectKey{Namespace: nsSnap.Namespace, Name: nsSnap.Name}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &storagev1alpha1.Snapshot{}
		if err := r.Client.Get(ctx, key, cur); err != nil {
			return err
		}
		if snapshotChildRefsEqualIgnoreOrder(cur.Status.ChildrenSnapshotRefs, desired) {
			changed = false
			return nil
		}
		base := cur.DeepCopy()
		cur.Status.ChildrenSnapshotRefs = desired
		if err := r.Client.Status().Patch(ctx, cur, client.MergeFrom(base)); err != nil {
			return err
		}
		changed = true
		return nil
	})
	return changed, err
}

// repointContentSnapshotRef updates content.spec.snapshotRef to want when it differs, gated on the
// relaxed-CEL precondition (status.parentDeleted): the immutability rule only admits a snapshotRef change
// once the content is in the recycle bin. Until the bin latch is set the update would be rejected, so we
// skip and let a later reconcile retry. Returns changed=true when it issued the update.
func (r *SnapshotReconciler) repointContentSnapshotRef(
	ctx context.Context,
	contentName string,
	want storagev1alpha1.SnapshotSubjectRef,
) (bool, error) {
	changed := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &storagev1alpha1.SnapshotContent{}
		if err := r.Client.Get(ctx, client.ObjectKey{Name: contentName}, cur); err != nil {
			return err
		}
		if cur.Spec.SnapshotRef != nil && *cur.Spec.SnapshotRef == want {
			changed = false
			return nil
		}
		if !cur.Status.ParentDeleted {
			// Not in the recycle bin yet: the CEL gate would reject the re-point. Skip; retry later.
			changed = false
			return nil
		}
		refCopy := want
		cur.Spec.SnapshotRef = &refCopy
		if err := r.Client.Update(ctx, cur); err != nil {
			return err
		}
		changed = true
		return nil
	})
	return changed, err
}

// staticBindRefMatches reports whether a SnapshotContent.spec.snapshotRef points back at nsSnap.
// When the back-reference carries a UID it must equal this Snapshot's UID: this prevents a stale
// pre-provisioned content from binding a freshly re-created Snapshot that reuses the same
// name/namespace (mirrors the CSI VolumeSnapshot<->VolumeSnapshotContent bound-UID check). A pre-
// provisioned content legitimately created before the Snapshot exists may leave the UID empty.
func staticBindRefMatches(ref *storagev1alpha1.SnapshotSubjectRef, nsSnap *storagev1alpha1.Snapshot) bool {
	if ref == nil {
		return false
	}
	if ref.UID != "" && ref.UID != nsSnap.UID {
		return false
	}
	return ref.APIVersion == storagev1alpha1.SchemeGroupVersion.String() &&
		ref.Kind == "Snapshot" &&
		ref.Name == nsSnap.Name &&
		ref.Namespace == nsSnap.Namespace
}
