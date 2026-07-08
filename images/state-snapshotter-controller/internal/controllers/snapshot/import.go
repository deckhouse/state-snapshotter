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
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	deckhousev1alpha1 "github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/common"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// importContentPollInterval is the polling fallback cadence while an import Snapshot's SnapshotContent is
// being materialized (uploaded children not yet bound, or content not yet Ready). The SnapshotContent
// watch is the primary wake-up; this only covers a missed event so the import does not stall.
const importContentPollInterval = 2 * time.Second

// reconcileImport materializes the SnapshotContent that backs an import-mode root Snapshot
// (spec.source.import) from the out-of-band uploaded payload, instead of capturing the live namespace.
//
// The materialization is the import twin of the capture path and uses the SAME common controllers: it
// creates the cluster-scoped SnapshotContent (owned by the root ObjectKeeper for unified TTL GC),
// publishes the manifest leg from the reconstructed ManifestCheckpoint
// (manifests-and-children-refs-upload keyed to the Snapshot UID), and publishes the content-graph edges
// from the uploaded namespaced child refs. The SnapshotContentController then computes Ready, which the
// Snapshot mirrors (single-aggregator), exiting the ImportPending hold.
//
// Import content uses deletionPolicy=Delete (capture uses Retain): an imported tree has no live source to
// re-capture from, so deleting the import root must reclaim the materialized content+artifacts rather than
// park them in the TTL bin.
func (r *SnapshotReconciler) reconcileImport(ctx context.Context, nsSnap *storagev1alpha1.Snapshot, rootOK *deckhousev1alpha1.ObjectKeeper) (ctrl.Result, error) {
	// Precondition: the reconstructed ManifestCheckpoint (uploaded node manifests) must exist. Until d8
	// uploads this node there is nothing to back the content, so hold in the non-terminal pending state.
	mcpName := usecase.ReconstructedManifestCheckpointName(nsSnap.UID, "")
	mcp := &ssv1alpha1.ManifestCheckpoint{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: mcpName}, mcp); err != nil {
		if errors.IsNotFound(err) {
			return r.reconcileImportPending(ctx, nsSnap)
		}
		return ctrl.Result{}, err
	}

	expectedName := snapshotContentName(nsSnap)

	content := &storagev1alpha1.SnapshotContent{}
	err := r.Client.Get(ctx, client.ObjectKey{Name: expectedName}, content)
	if errors.IsNotFound(err) {
		om := snapshotContentObjectMeta(nsSnap)
		om.OwnerReferences = []metav1.OwnerReference{controllercommon.RootObjectKeeperOwnerReference(rootOK)}
		newContent := &storagev1alpha1.SnapshotContent{
			ObjectMeta: om,
			Spec:       desiredImportSnapshotContentSpec(nsSnap),
		}
		if err := r.Client.Create(ctx, newContent); err != nil {
			if errors.IsAlreadyExists(err) {
				return r.finishReconcileWithExistingContent(ctx, nsSnap, expectedName)
			}
			return ctrl.Result{}, err
		}
		if err := r.bindImportSnapshotContent(ctx, nsSnap, expectedName); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	if nsSnap.Status.BoundSnapshotContentName != expectedName {
		if err := r.bindImportSnapshotContent(ctx, nsSnap, expectedName); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Manifest leg moved to the SnapshotContentController aggregator (INV-CONTENT-WRITER-1,
	// content-single-writer design §10): the aggregator is the single writer of status.manifestCheckpointName
	// for import too, projecting the reconstructed checkpoint name (keyed to the root Snapshot UID) once the
	// upload endpoint has created it, and then adopting the (import-ObjectKeeper-owned) MCP onto the content
	// via ownerRef when it validates the manifest leg (§10.1). The import orchestrator no longer publishes it;
	// the bound content's mirrored Ready holds the root pending until the aggregator projects the leg.

	// Content-graph edges moved to the SnapshotContentController aggregator (INV-CONTENT-CHILDREN-1,
	// content-single-writer design §3.1/§3.2): the aggregator projects childrenSnapshotContentRefs from the
	// root Snapshot's uploaded status.childrenSnapshotRefs (same code path as capture). The import
	// orchestrator no longer publishes the edge set; the bound content's mirrored Ready (gated by the
	// aggregator's ChildrenReady) holds the root pending until its children are linked, so the Ready-poll
	// below is the convergence driver (bottom-up).

	// Import has no live residual/orphan-PVC capture wave of its own: any reconstructed VS visibility leaves
	// have no live VolumeSnapshot, so the aggregator's fail-closed orphan-link gate skips them and never
	// holds the import root at Ready=False. Nothing to latch — mirror Ready directly.
	// Steady state: mirror the bound content's Ready (single-aggregator, INV-COND4). The content watch
	// also wakes this Snapshot on the Ready transition; the requeue is a missed-event fallback.
	if err := r.mirrorSnapshotReadyFromBoundContent(ctx, nsSnap, content, nil); err != nil {
		return ctrl.Result{}, err
	}
	if fresh, ferr := r.getSnapshotContentFresh(ctx, content.Name); ferr == nil {
		if cond := meta.FindStatusCondition(fresh.Status.Conditions, snapshotpkg.ConditionReady); cond != nil && cond.Status == metav1.ConditionTrue {
			return ctrl.Result{}, nil
		}
	} else if !errors.IsNotFound(ferr) {
		return ctrl.Result{}, ferr
	}
	return ctrl.Result{RequeueAfter: importContentPollInterval}, nil
}

// desiredImportSnapshotContentSpec returns the SnapshotContent spec for an imported node: deletionPolicy
// Delete (vs Retain on capture). The spec is immutable; all data/result wiring is published into status.
func desiredImportSnapshotContentSpec(nsSnap *storagev1alpha1.Snapshot) storagev1alpha1.SnapshotContentSpec {
	return controllercommon.NewSnapshotContentSpec(
		storagev1alpha1.SnapshotContentDeletionPolicyDelete,
		controllercommon.SnapshotSubjectRefFromSnapshot(nsSnap),
	)
}

// bindImportSnapshotContent sets status.boundSnapshotContentName (+ observedGeneration) under conflict
// retry against a fresh read (the upload endpoint concurrently writes status.childrenSnapshotRefs).
func (r *SnapshotReconciler) bindImportSnapshotContent(ctx context.Context, nsSnap *storagev1alpha1.Snapshot, contentName string) error {
	key := client.ObjectKey{Namespace: nsSnap.Namespace, Name: nsSnap.Name}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &storagev1alpha1.Snapshot{}
		if err := r.Client.Get(ctx, key, cur); err != nil {
			return err
		}
		if cur.Status.BoundSnapshotContentName == contentName {
			return nil
		}
		cur.Status.BoundSnapshotContentName = contentName
		return r.Client.Status().Update(ctx, cur)
	})
}
