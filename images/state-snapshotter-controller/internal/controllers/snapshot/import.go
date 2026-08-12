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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// importContentPollInterval is the polling fallback cadence while an import Snapshot's SnapshotContent is
// being materialized (uploaded children not yet bound, or content not yet Ready). The SnapshotContent
// watch is the primary wake-up; this only covers a missed event so the import does not stall.
const importContentPollInterval = 2 * time.Second

// reconcileImport holds an import-mode root Snapshot (spec.mode: Import) until the SnapshotContent that
// backs it (materialized from the out-of-band uploaded payload) is created, bound, and Ready — it no longer
// creates the content itself.
//
// Creator = generic binder (content-single-writer design §10, creator/main): the binder creates + binds the
// import root SnapshotContent (owned by the root ObjectKeeper for unified TTL GC, deletionPolicy=Delete)
// exactly as it does for the capture root, and the SnapshotContentController aggregator projects ALL of its
// status — the manifest leg from the reconstructed ManifestCheckpoint (keyed to the Snapshot UID), the
// content-graph edges from the uploaded child refs, and Ready. This orchestrator's sole remaining job for
// import is the namespace-Snapshot-facing lifecycle: hold the non-terminal ImportPending state until the
// binder binds, then mirror the bound content's Ready (single-aggregator, INV-COND4), exiting the hold.
//
// Import content uses deletionPolicy=Delete (capture uses Retain): an imported tree has no live source to
// re-capture from, so deleting the import root must reclaim the materialized content+artifacts rather than
// park them in the TTL bin.
func (r *SnapshotReconciler) reconcileImport(ctx context.Context, nsSnap *storagev1alpha1.Snapshot) (ctrl.Result, error) {
	// Not yet bound: the binder has not created + bound the root content yet (it may still be waiting for the
	// root ObjectKeeper, or its watch has not fired). Hold in the non-terminal ImportPending state.
	if nsSnap.Status.BoundSnapshotContentName == "" {
		return r.reconcileImportPending(ctx, nsSnap)
	}

	content := &storagev1alpha1.SnapshotContent{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: nsSnap.Status.BoundSnapshotContentName}, content); err != nil {
		if errors.IsNotFound(err) {
			// Bound name points at a content that does not exist yet (race between the binder's bind write and
			// the content becoming observable here) — hold pending until it materializes.
			return r.reconcileImportPending(ctx, nsSnap)
		}
		return ctrl.Result{}, err
	}

	// Steady state: mirror the bound content's Ready (single-aggregator, INV-COND4). The aggregator projects
	// the manifest leg + children edges; the content watch wakes this Snapshot on the Ready transition, and
	// the requeue is a missed-event fallback while the content is still converging.
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
