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

// patchSnapshotChildSnapshotFailedBridge writes a local (non-mirror) Snapshot.Ready=False/ChildrenFailed
// for the child-Snapshot terminal capture-failure bridge: a child Snapshot terminally failed capture
// planning before any child SnapshotContent existed to reflect it, so the content tree cannot represent
// the failure (snapshot-rework/2026-06-03-snapshot-conditions-model.md §5.2).
//
// Mirror contract (INV-COND2 single aggregator / INV-COND4 mirror-not-recompute): Snapshot.Ready is
// mirror-only (a verbatim copy of the bound SnapshotContent.Ready status/reason/message) EXCEPT for
// failures that cannot be represented in the content tree yet. The complete, exhaustive set of local
// (non-mirror) root Snapshot.Ready=False writers — there are NO hidden exceptions — is:
//
//   - PRE-BIND / PRE-PUBLISH PLANNING/CAPTURE FAILURES via failCapture(): these all happen before the root
//     publishes its own truth refs (manifestCheckpointName / dataRefs), so SnapshotContent cannot yet carry
//     them through ManifestsReady/VolumeReady:
//   - "ListFailed"                — building capture targets failed (capture.go);
//   - "CapturePlanDrift"          — plain N2a MCR plan drift, terminal (capture.go);
//   - "VolumeCaptureTargetsFailed"/"VolumeCaptureFailed" — volume capture planning/exec (volume_capture.go);
//   - "DuplicateCoveredPVCUID"    — invalid plan: same PVC UID covered twice (capture.go);
//   - "SubtreeManifestFailed"     — PRE-PUBLISH BRIDGE: a descendant MCP is terminally Failed so the root
//     exclude set / plan cannot be computed and the root MCR/MCP is not created yet. The descendant failure
//     is also representable via content ChildrenReady once child refs are published (deferred conversion).
//   - NamespaceNotFound (no namespace, so no content can be produced) — documented exception;
//   - the child-Snapshot terminal capture-failure bridge below (patchSnapshotChildSnapshotFailedBridge);
//   - the mirrorSnapshotReadyFromBoundContent fallback to ContentBindingPending (pre-publish pending window).
//
// Everything else is a verbatim mirror of the bound SnapshotContent.Ready.
//
// The bridge is NOT limited to the pre-bind window: a parent may already be bound when a child Snapshot
// terminally fails capture planning before any child SnapshotContent exists to carry the failure, so this
// is the one local write that can occur after the parent's own bind/publish. Every other Snapshot.Ready
// transition is a verbatim mirror of the bound SnapshotContent.Ready.
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
	reason := snapshotpkg.ReasonContentBindingPending
	message := "waiting for SnapshotContent Ready condition to be published"
	if transientErr != nil {
		message = transientErr.Error()
	}
	// contentExcluded mirrors the bound content's durable excludedRefs aggregate onto the root Snapshot.
	// Only applied when the content is observable (haveContent): a transient NotFound must not clobber a
	// previously-mirrored set. The aggregate is monotonic on the content side, so a verbatim mirror is safe.
	var contentExcluded []storagev1alpha1.ExcludedObjectRef
	haveContent := false
	if fresh, err := r.getSnapshotContentFresh(ctx, content.Name); err == nil {
		haveContent = true
		contentExcluded = fresh.Status.ExcludedRefs
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
		excludedChanged := haveContent && !excludedObjectRefsEqualIgnoreOrder(cur.Status.ExcludedRefs, contentExcluded)
		existing := meta.FindStatusCondition(cur.Status.Conditions, snapshotpkg.ConditionReady)
		conditionCurrent := existing != nil && existing.Status == status && existing.Reason == reason &&
			existing.Message == message && existing.ObservedGeneration == cur.Generation
		if conditionCurrent && !excludedChanged {
			return nil
		}
		if excludedChanged {
			cur.Status.ExcludedRefs = sortedExcludedObjectRefsSlice(contentExcluded)
		}
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

// sortedExcludedObjectRefsSlice returns a stable, sorted copy of refs (nil-safe).
func sortedExcludedObjectRefsSlice(refs []storagev1alpha1.ExcludedObjectRef) []storagev1alpha1.ExcludedObjectRef {
	if len(refs) == 0 {
		return nil
	}
	out := append([]storagev1alpha1.ExcludedObjectRef(nil), refs...)
	sortExcludedObjectRefs(out)
	return out
}
