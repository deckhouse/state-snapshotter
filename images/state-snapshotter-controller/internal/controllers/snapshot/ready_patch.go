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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// patchSnapshotChildSnapshotFailedBridge writes the ONE non-mirror Snapshot.Ready value permitted by the
// single-aggregator contract (snapshot-rework/2026-06-03-snapshot-conditions-model.md): Ready=False/
// ChildrenFailed when a child Snapshot terminally failed capture planning before any child
// SnapshotContent could reflect it (the content tree cannot represent a child-Snapshot capture failure).
// Every other Snapshot.Ready transition is a mirror of the bound SnapshotContent.Ready.
func (r *SnapshotReconciler) patchSnapshotChildSnapshotFailedBridge(
	ctx context.Context,
	parentKey types.NamespacedName,
	msg string,
) (ctrl.Result, error) {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		nsSnap := &storagev1alpha1.Snapshot{}
		if err := r.Client.Get(ctx, parentKey, nsSnap); err != nil {
			return err
		}
		nsSnap.Status.ObservedGeneration = nsSnap.Generation
		meta.SetStatusCondition(&nsSnap.Status.Conditions, metav1.Condition{
			Type:               snapshotpkg.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             snapshotpkg.ReasonChildrenFailed,
			Message:            msg,
			ObservedGeneration: nsSnap.Generation,
		})
		return r.Client.Status().Update(ctx, nsSnap)
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 500 * time.Millisecond}, nil
}

// mirrorSnapshotReadyFromBoundContent sets the parent Snapshot.Ready to a verbatim mirror of the bound
// SnapshotContent.Ready (status/reason/message), gen-gated on the Snapshot. This enforces the
// single-aggregator contract during the pre-capture pending window (the parent cannot build its own
// capture plan yet because the subtree exclude set is not ready). If the content has no Ready condition
// yet, it falls back to Ready=False/ManifestCapturePending carrying transientErr for diagnostics.
func (r *SnapshotReconciler) mirrorSnapshotReadyFromBoundContent(
	ctx context.Context,
	parent *storagev1alpha1.Snapshot,
	content *storagev1alpha1.SnapshotContent,
	transientErr error,
) error {
	status := metav1.ConditionFalse
	reason := snapshotpkg.ReasonManifestCapturePending
	message := ""
	if transientErr != nil {
		message = transientErr.Error()
	}
	if fresh, err := r.getSnapshotContentFresh(ctx, content.Name); err == nil {
		if cond := meta.FindStatusCondition(fresh.Status.Conditions, snapshotpkg.ConditionReady); cond != nil {
			status = cond.Status
			reason = cond.Reason
			message = cond.Message
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	parentKey := types.NamespacedName{Namespace: parent.Namespace, Name: parent.Name}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &storagev1alpha1.Snapshot{}
		if err := r.Client.Get(ctx, parentKey, cur); err != nil {
			return err
		}
		existing := meta.FindStatusCondition(cur.Status.Conditions, snapshotpkg.ConditionReady)
		if existing != nil && existing.Status == status && existing.Reason == reason &&
			existing.Message == message && existing.ObservedGeneration == cur.Generation {
			return nil
		}
		cur.Status.ObservedGeneration = cur.Generation
		meta.SetStatusCondition(&cur.Status.Conditions, metav1.Condition{
			Type:               snapshotpkg.ConditionReady,
			Status:             status,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: cur.Generation,
		})
		return r.Client.Status().Update(ctx, cur)
	})
}
