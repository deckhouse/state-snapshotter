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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// importPendingMessage is the user-facing status message for an import Snapshot awaiting materialization.
const importPendingMessage = "awaiting import materialization (upload manifests-and-children-refs; data leaves also need DataImport)"

// importPendingRequeueInterval is the safety-net resync cadence for an import Snapshot held in pending.
// Import content is materialized out-of-band and may never arrive (or take a long time), so this is NOT a
// tight poll: the primary wake-up is the SnapshotContent watch (which fires when C5 binds). A coarse
// interval avoids per-snapshot requeue storms while still recovering from a missed watch event.
const importPendingRequeueInterval = time.Minute

// reconcileImportPending holds an import-mode Snapshot (spec.source.import) in a non-terminal pending
// state instead of running dynamic namespace capture. Import content is materialized out-of-band: d8
// uploads per-node manifests+children (manifests-and-children-refs-upload) and, for data leaves, creates
// a DataImport; the import orchestrator then reconstructs SnapshotContent and binds it. Until that path
// is wired, this guard guarantees an import Snapshot never captures the live namespace. It is idempotent:
// it re-patches only when the Ready condition is not already the desired pending state for this generation,
// and the patch itself uses RetryOnConflict against a fresh read (the upload endpoint concurrently writes
// status.childrenSnapshotRefs on the same object).
func (r *SnapshotReconciler) reconcileImportPending(ctx context.Context, nsSnap *storagev1alpha1.Snapshot) (ctrl.Result, error) {
	cur := meta.FindStatusCondition(nsSnap.Status.Conditions, snapshotpkg.ConditionReady)
	if cur != nil && cur.Status == metav1.ConditionFalse && cur.Reason == snapshotpkg.ReasonImportPending &&
		cur.Message == importPendingMessage && cur.ObservedGeneration == nsSnap.Generation {
		return ctrl.Result{RequeueAfter: importPendingRequeueInterval}, nil
	}
	key := client.ObjectKeyFromObject(nsSnap)
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &storagev1alpha1.Snapshot{}
		if err := r.Client.Get(ctx, key, fresh); err != nil {
			return err
		}
		meta.SetStatusCondition(&fresh.Status.Conditions, metav1.Condition{
			Type:               snapshotpkg.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             snapshotpkg.ReasonImportPending,
			Message:            importPendingMessage,
			ObservedGeneration: fresh.Generation,
		})
		return r.Client.Status().Update(ctx, fresh)
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: importPendingRequeueInterval}, nil
}
