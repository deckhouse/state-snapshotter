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
	"errors"
	"fmt"
	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/common"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/manifestcapture"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/snapshotcontent"
	"io"
	"net"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	deckhousev1alpha1 "github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	volumecaptureuc "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase/volumecapture"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshotgraphregistry"
)

const labelSnapshotUID = "state-snapshotter.deckhouse.io/snapshot-uid"

// errManifestCapturePlanDrift is returned when an existing MCR for this Snapshot has a different
// target set than the current namespace listing; MCR spec is not silently rewritten.
var errManifestCapturePlanDrift = errors.New("manifest capture plan drift")

// deleteSnapshotManifestCaptureRequest removes the namespace-flow MCR after capture is persisted.
// NotFound is success; other errors are returned so the reconciler can retry.
func (r *SnapshotReconciler) deleteSnapshotManifestCaptureRequest(ctx context.Context, nsSnap *storagev1alpha1.Snapshot) error {
	key := types.NamespacedName{Namespace: nsSnap.Namespace, Name: namespacemanifest.SnapshotMCRName(nsSnap.UID)}
	mcr := &ssv1alpha1.ManifestCaptureRequest{}
	if err := r.Client.Get(ctx, key, mcr); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get ManifestCaptureRequest %s: %w", key.String(), err)
	}
	if err := r.Client.Delete(ctx, mcr); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete ManifestCaptureRequest %s: %w", key.String(), err)
	}
	return nil
}

// buildSnapshotMachineryGVKs builds the mechanism-1 (kind-level dedup) set from the live GVK registry:
// every registered snapshot kind and content kind. These are objects the snapshotter creates itself, so
// they MUST be excluded from namespace capture. Returns snapshotgraphregistry.ErrGraphRegistryNotReady
// when the registry has not been built yet (fail-closed: callers requeue instead of capturing machinery).
func (r *SnapshotReconciler) buildSnapshotMachineryGVKs() (namespacemanifest.SnapshotMachineryGVKs, error) {
	if r.SnapshotGraphRegistry == nil {
		return nil, snapshotgraphregistry.ErrGraphRegistryNotReady
	}
	reg := r.SnapshotGraphRegistry.Current()
	if reg == nil {
		return nil, snapshotgraphregistry.ErrGraphRegistryNotReady
	}
	set := make(namespacemanifest.SnapshotMachineryGVKs)
	for _, kind := range reg.RegisteredSnapshotKinds() {
		gvk, err := reg.ResolveSnapshotGVK(kind)
		if err != nil {
			continue
		}
		set[gvk] = struct{}{}
	}
	for _, gvk := range reg.RegisteredContentGVKs() {
		set[gvk] = struct{}{}
	}
	return set, nil
}

// reconcileCaptureN2a drives manifest capture via MCR->ManifestCheckpoint after root SnapshotContent is bound.
func (r *SnapshotReconciler) reconcileCaptureN2a(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	content *storagev1alpha1.SnapshotContent,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if r.Dynamic == nil {
		return ctrl.Result{}, fmt.Errorf("snapshot reconciler: Dynamic client is nil")
	}
	if r.APIReader == nil {
		return ctrl.Result{}, fmt.Errorf("snapshot reconciler: APIReader is nil")
	}

	_, res, err := r.ensureSnapshotRootObjectKeeper(ctx, nsSnap, content)
	if err != nil {
		return ctrl.Result{}, err
	}
	if res.RequeueAfter > 0 || res.Requeue {
		return res, nil
	}

	if err := r.Client.Get(ctx, client.ObjectKey{Name: content.Name}, content); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureVolumeCaptureLeg(ctx, nsSnap, content); err != nil {
		return ctrl.Result{}, err
	}
	if _, err := r.reconcileVolumeCapturePublish(ctx, nsSnap, content, false); err != nil {
		return ctrl.Result{}, err
	}

	if done, res, err := r.reconcileIfRootManifestCheckpointAlreadyReady(ctx, nsSnap, content); done {
		return res, err
	}

	if r.Discovery == nil {
		return ctrl.Result{}, fmt.Errorf("snapshot reconciler: Discovery client is nil")
	}
	snapshotKinds, skErr := r.buildSnapshotMachineryGVKs()
	if skErr != nil {
		// Fail-closed: the live snapshot-kind registry (mechanism 1 dedup) is not built yet. Planning now
		// would risk capturing our own snapshot machinery, so requeue without building targets.
		logger.V(1).Info("snapshot graph registry not ready; requeue capture planning", "err", skErr)
		return r.n2aReturnAfterVolumePublish(ctx, nsSnap, content, ctrl.Result{RequeueAfter: 2 * time.Second}, nil)
	}
	targets, unreadable, err := usecase.BuildRootNamespaceManifestCaptureTargets(ctx, r.Archive, r.Dynamic, r.Discovery, r.Client, nsSnap, content.Name, snapshotKinds)
	if err != nil {
		freshParent := &storagev1alpha1.Snapshot{}
		if gerr := r.Client.Get(ctx, client.ObjectKey{Namespace: nsSnap.Namespace, Name: nsSnap.Name}, freshParent); gerr != nil {
			return ctrl.Result{}, gerr
		}
		hasSubtree := hasNonVisibilitySnapshotChildren(freshParent.Status.ChildrenSnapshotRefs)
		// Transient subtree state while childrenSnapshotRefs are populated or child snapshot is still binding;
		// do not fail capture as ListFailed — requeue like ChildrenPending.
		transientChildGraph := errors.Is(err, usecase.ErrSubtreeManifestCapturePending) ||
			errors.Is(err, volumecaptureuc.ErrSubtreeDataRefsPending) ||
			errors.Is(err, usecase.ErrRunGraphChildNotBound) ||
			errors.Is(err, usecase.ErrRunGraphChildSnapshotNotFound) ||
			(hasSubtree && errors.Is(err, usecase.ErrRunGraphChildNotReachable)) ||
			(hasSubtree && errors.Is(err, snapshotgraphregistry.ErrGraphRegistryNotReady))
		if transientChildGraph {
			cur := meta.FindStatusCondition(freshParent.Status.Conditions, snapshotpkg.ConditionReady)
			if cur != nil && cur.Reason == snapshotpkg.ReasonChildrenFailed {
				return ctrl.Result{RequeueAfter: 500 * time.Millisecond}, nil
			}
			// Single-aggregator contract (snapshot-rework/2026-06-03-snapshot-conditions-model.md):
			// Snapshot.Ready is a mirror of the bound SnapshotContent.Ready. The ONE allowed exception is
			// the child-Snapshot capture-failure bridge below: a child Snapshot can terminally fail capture
			// planning before any child SnapshotContent reflects it, so the content tree cannot represent
			// that failure. Pending states are NOT an exception — they are mirrored from Content.Ready.
			if hasSubtree {
				sum, serr := usecase.SummarizeChildSnapshotTerminalFailures(ctx, r.childSnapshotStatusReader(), freshParent.Status.ChildrenSnapshotRefs, freshParent.Namespace)
				if serr != nil {
					return ctrl.Result{}, serr
				}
				if sum.HasFailed {
					parentKey := types.NamespacedName{Namespace: freshParent.Namespace, Name: freshParent.Name}
					return r.patchSnapshotChildSnapshotFailedBridge(ctx, parentKey, usecase.JoinNonEmpty(sum.Messages, "; "))
				}
				// E5 delayed first MCR: do not leave a root MCR while subtree exclude cannot be computed (stale plan vs exclude).
				if delErr := r.deleteSnapshotManifestCaptureRequest(ctx, freshParent); delErr != nil {
					return ctrl.Result{}, delErr
				}
			}
			// Pending window: mirror bound SnapshotContent.Ready instead of computing a local reason.
			if mErr := r.mirrorSnapshotReadyFromBoundContent(ctx, freshParent, content, err); mErr != nil {
				return ctrl.Result{}, mErr
			}
			return r.n2aReturnAfterVolumePublish(ctx, nsSnap, content, ctrl.Result{RequeueAfter: 500 * time.Millisecond}, nil)
		}
		if errors.Is(err, usecase.ErrSubtreeManifestCaptureFailed) {
			// Classification (Slice 3): PRE-PUBLISH BRIDGE. This fires during root capture-target planning
			// (BuildRootNamespaceManifestCaptureTargets) when a descendant ManifestCheckpoint is terminally
			// Failed, so the root's exclude set cannot be computed and the root MCR/MCP is NOT created yet.
			// At this point the root SnapshotContent has no published manifestCheckpointName, so it cannot
			// express the root's own manifest leg (ManifestsReady) terminal failure — hence the local failCapture bridge.
			// The underlying descendant terminal failure is ALSO representable via the content tree
			// (descendant content ManifestsReady=False -> ancestor ChildrenReady=False -> root content Ready
			// =False -> root Snapshot mirror) once child content refs are published; converting this to pure
			// content-driven propagation is a deferred follow-up (out of scope for Slice 3), not a hidden
			// post-publish recompute.
			return r.failCapture(ctx, freshParent, content, "SubtreeManifestFailed", err.Error())
		}
		if errors.Is(err, volumecaptureuc.ErrDuplicateCoveredPVCUID) {
			return r.failCapture(ctx, freshParent, content, "DuplicateCoveredPVCUID", err.Error())
		}
		if isTransientCaptureTargetError(err) {
			return r.n2aReturnAfterVolumePublish(ctx, nsSnap, content, ctrl.Result{RequeueAfter: 2 * time.Second}, nil)
		}
		return r.failCapture(ctx, freshParent, content, "ListFailed", fmt.Sprintf("build capture targets: %v", err))
	}
	// Fail-closed on an incomplete capture plan: some namespaced types could not be read (Forbidden — the
	// transient per-namespace RoleBinding has not propagated yet — or partial discovery). Do NOT create the
	// root MCR with a partial plan; degrade Ready transiently and requeue until the types become readable.
	if len(unreadable) > 0 {
		freshParent := &storagev1alpha1.Snapshot{}
		if gerr := r.Client.Get(ctx, client.ObjectKey{Namespace: nsSnap.Namespace, Name: nsSnap.Name}, freshParent); gerr != nil {
			return ctrl.Result{}, gerr
		}
		// Do not leave a stale root MCR built from a previous (possibly complete) plan while capture is incomplete.
		if delErr := r.deleteSnapshotManifestCaptureRequest(ctx, freshParent); delErr != nil {
			return ctrl.Result{}, delErr
		}
		msg := fmt.Sprintf("namespace capture incomplete; cannot read: %s", formatUnreadableGVRs(unreadable))
		if err := r.setSnapshotReadyFalse(ctx, freshParent, snapshotpkg.ReasonNamespaceCaptureIncomplete, msg); err != nil {
			return ctrl.Result{}, err
		}
		return r.n2aReturnAfterVolumePublish(ctx, nsSnap, content, ctrl.Result{RequeueAfter: 2 * time.Second}, nil)
	}
	mcr, res, err := r.ensureManifestCaptureRequest(ctx, nsSnap, content, targets)
	if err != nil {
		if errors.Is(err, errManifestCapturePlanDrift) {
			freshParent := &storagev1alpha1.Snapshot{}
			if gerr := r.Client.Get(ctx, client.ObjectKey{Namespace: nsSnap.Namespace, Name: nsSnap.Name}, freshParent); gerr != nil {
				return ctrl.Result{}, gerr
			}
			// Subtree-root: plan drift is not the primary convergence path; delete stale MCR and retry with fresh targets.
			// Plain N2a (no real domain children; VS visibility leaves do not count): terminal CapturePlanDrift for operator visibility.
			if hasNonVisibilitySnapshotChildren(freshParent.Status.ChildrenSnapshotRefs) {
				if delErr := r.deleteSnapshotManifestCaptureRequest(ctx, freshParent); delErr != nil {
					return ctrl.Result{}, delErr
				}
				return r.n2aReturnAfterVolumePublish(ctx, nsSnap, content, ctrl.Result{RequeueAfter: 200 * time.Millisecond}, nil)
			}
			return r.failCapture(ctx, freshParent, content, "CapturePlanDrift", err.Error())
		}
		return ctrl.Result{}, err
	}
	if res.RequeueAfter > 0 || res.Requeue {
		return r.n2aReturnAfterVolumePublish(ctx, nsSnap, content, res, nil)
	}

	mcpName := namespacemanifest.GenerateManifestCheckpointNameFromUID(mcr.UID)
	if err := snapshotcontent.PublishSnapshotContentManifestCheckpointName(ctx, r.Client, content.Name, mcpName); err != nil {
		return ctrl.Result{}, err
	}
	mcp := &ssv1alpha1.ManifestCheckpoint{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: mcpName}, mcp); err != nil {
		if apierrors.IsNotFound(err) {
			// After bind, Snapshot.Ready is a mirror of bound SnapshotContent.Ready (INV-COND4): the content
			// controller computes ManifestsReady from the published manifestCheckpointName. Do NOT write a
			// local pending reason here (no second source of truth).
			if mErr := r.mirrorSnapshotReadyFromBoundContent(ctx, nsSnap, content, nil); mErr != nil {
				return ctrl.Result{}, mErr
			}
			return r.n2aReturnAfterVolumePublish(ctx, nsSnap, content, ctrl.Result{RequeueAfter: 500 * time.Millisecond}, nil)
		}
		return ctrl.Result{}, err
	}

	readyCond := meta.FindStatusCondition(mcp.Status.Conditions, ssv1alpha1.ManifestCheckpointConditionTypeReady)
	if readyCond == nil || readyCond.Status != metav1.ConditionTrue {
		// After bind the Snapshot never writes semantic Ready state (INV-COND4) — not even a terminal MCP
		// failure. SnapshotContent aggregation owns ManifestsReady/Ready (ManifestCheckpointFailed included)
		// from the published manifestCheckpointName; the Snapshot only mirrors it. Eventual consistency: if
		// content has not recomputed yet the mirror falls back to ContentBindingPending and we requeue.
		if mErr := r.mirrorSnapshotReadyFromBoundContent(ctx, nsSnap, content, nil); mErr != nil {
			return ctrl.Result{}, mErr
		}
		return r.n2aReturnAfterVolumePublish(ctx, nsSnap, content, ctrl.Result{RequeueAfter: 500 * time.Millisecond}, nil)
	}

	if readyCond.Reason != ssv1alpha1.ManifestCheckpointConditionReasonCompleted {
		// Ready=True with unexpected reason — still treat as success if True (defensive).
		logger.Info("ManifestCheckpoint Ready=True with non-Completed reason", "reason", readyCond.Reason, "mcp", mcpName)
	}

	res, err = r.reconcileN2aRootReadyAfterManifestCapture(ctx, nsSnap, mcpName)
	return r.n2aReturnAfterVolumePublish(ctx, nsSnap, content, res, err)
}

// reconcileIfRootManifestCheckpointAlreadyReady handles idempotent steady state: MCP name on SnapshotContent, MCP Ready,
// Snapshot Ready, and MCR already removed. Skips recreating MCR when capture is complete.
func (r *SnapshotReconciler) reconcileIfRootManifestCheckpointAlreadyReady(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	content *storagev1alpha1.SnapshotContent,
) (done bool, res ctrl.Result, err error) {
	freshContent, err := r.getSnapshotContentFresh(ctx, content.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, ctrl.Result{}, nil
		}
		return true, ctrl.Result{}, err
	}
	content = freshContent
	mcpName := content.Status.ManifestCheckpointName
	if mcpName == "" {
		return false, ctrl.Result{}, nil
	}
	mcp := &ssv1alpha1.ManifestCheckpoint{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: mcpName}, mcp); err != nil {
		if apierrors.IsNotFound(err) {
			return false, ctrl.Result{}, nil
		}
		return true, ctrl.Result{}, err
	}
	readyCond := meta.FindStatusCondition(mcp.Status.Conditions, ssv1alpha1.ManifestCheckpointConditionTypeReady)
	if readyCond == nil || readyCond.Status != metav1.ConditionTrue {
		return false, ctrl.Result{}, nil
	}

	// Publisher-complete: bound content Ready means capture is done; mirror snapshot Ready even if MCR
	// still exists (avoids re-entering BuildRootNamespaceManifestCaptureTargets after transient ListFailed).
	contentReady := meta.FindStatusCondition(content.Status.Conditions, snapshotpkg.ConditionReady)
	if contentReady != nil && contentReady.Status == metav1.ConditionTrue {
		res, err = r.reconcileN2aRootReadyAfterManifestCapture(ctx, nsSnap, mcpName)
		res, err = r.n2aReturnAfterVolumePublish(ctx, nsSnap, content, res, err)
		return true, res, err
	}

	// Steady-state fast path only after the request is gone; while MCR exists we must run
	// ensureManifestCaptureRequest (capture plan drift vs live namespace).
	mcrKey := types.NamespacedName{Namespace: nsSnap.Namespace, Name: namespacemanifest.SnapshotMCRName(nsSnap.UID)}
	staleMCR := &ssv1alpha1.ManifestCaptureRequest{}
	if err := r.Client.Get(ctx, mcrKey, staleMCR); err == nil {
		readyCond := meta.FindStatusCondition(nsSnap.Status.Conditions, snapshotpkg.ConditionReady)
		if readyCond != nil && readyCond.Status == metav1.ConditionFalse && readyCond.Reason == "CapturePlanDrift" {
			return true, ctrl.Result{}, nil
		}
		return false, ctrl.Result{}, nil
	} else if !apierrors.IsNotFound(err) {
		return true, ctrl.Result{}, err
	}

	res, err = r.reconcileN2aRootReadyAfterManifestCapture(ctx, nsSnap, mcpName)
	res, err = r.n2aReturnAfterVolumePublish(ctx, nsSnap, content, res, err)
	return true, res, err
}

func (r *SnapshotReconciler) reconcileN2aRootReadyAfterManifestCapture(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	_ string,
) (ctrl.Result, error) {
	fresh := &storagev1alpha1.Snapshot{}
	nsKey := types.NamespacedName{Namespace: nsSnap.Namespace, Name: nsSnap.Name}
	var gerr error
	if r.APIReader != nil {
		gerr = r.APIReader.Get(ctx, nsKey, fresh)
	} else {
		gerr = r.Client.Get(ctx, nsKey, fresh)
	}
	if gerr != nil {
		return ctrl.Result{}, gerr
	}
	if fresh.Status.BoundSnapshotContentName == "" {
		return ctrl.Result{RequeueAfter: 500 * time.Millisecond}, nil
	}
	content, err := r.getSnapshotContentFresh(ctx, fresh.Status.BoundSnapshotContentName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: 500 * time.Millisecond}, nil
		}
		return ctrl.Result{}, err
	}
	contentReady := meta.FindStatusCondition(content.Status.Conditions, snapshotpkg.ConditionReady)
	if contentReady == nil || contentReady.Status != metav1.ConditionTrue {
		reason := snapshotpkg.ReasonManifestCapturePending
		message := fmt.Sprintf("waiting for SnapshotContent %q Ready", content.Name)
		if contentReady != nil {
			reason = contentReady.Reason
			message = contentReady.Message
		}
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			cur := &storagev1alpha1.Snapshot{}
			if err := r.Client.Get(ctx, nsKey, cur); err != nil {
				return err
			}
			cur.Status.ObservedGeneration = cur.Generation
			meta.SetStatusCondition(&cur.Status.Conditions, metav1.Condition{
				Type:               snapshotpkg.ConditionReady,
				Status:             metav1.ConditionFalse,
				Reason:             reason,
				Message:            message,
				ObservedGeneration: cur.Generation,
			})
			return r.Client.Status().Update(ctx, cur)
		}); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 500 * time.Millisecond}, nil
	}
	ready := meta.FindStatusCondition(fresh.Status.Conditions, snapshotpkg.ConditionReady)
	if ready == nil || ready.Status != contentReady.Status || ready.Reason != contentReady.Reason || ready.Message != contentReady.Message {
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			cur := &storagev1alpha1.Snapshot{}
			if err := r.Client.Get(ctx, nsKey, cur); err != nil {
				return err
			}
			cur.Status.ObservedGeneration = cur.Generation
			meta.SetStatusCondition(&cur.Status.Conditions, metav1.Condition{
				Type:               snapshotpkg.ConditionReady,
				Status:             contentReady.Status,
				Reason:             contentReady.Reason,
				Message:            contentReady.Message,
				ObservedGeneration: cur.Generation,
			})
			return r.Client.Status().Update(ctx, cur)
		}); err != nil {
			return ctrl.Result{}, err
		}
	}
	post := &storagev1alpha1.Snapshot{}
	if err := r.Client.Get(ctx, nsKey, post); err != nil {
		return ctrl.Result{}, err
	}
	mcrReady, err := r.snapshotManifestCaptureRequestReadyForCleanup(ctx, post)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !mcrReady {
		return ctrl.Result{RequeueAfter: 500 * time.Millisecond}, nil
	}
	if err := r.deleteSnapshotManifestCaptureRequest(ctx, post); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.patchSnapshotManifestCaptureRequestName(ctx, post, ""); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *SnapshotReconciler) snapshotManifestCaptureRequestReadyForCleanup(ctx context.Context, nsSnap *storagev1alpha1.Snapshot) (bool, error) {
	key := types.NamespacedName{Namespace: nsSnap.Namespace, Name: namespacemanifest.SnapshotMCRName(nsSnap.UID)}
	return manifestcapture.ManifestCaptureRequestSafeToDelete(ctx, r.snapshotContentReader(), key, nsSnap.Status.BoundSnapshotContentName)
}

// isTransientCaptureTargetError classifies a capture-target build error as a transient apiserver/network
// hiccup (requeue, NOT terminal ListFailed). Discovery lists ALL namespaced types (plus flaky aggregated
// APIServers), so the window for these errors is large; treating them as terminal would stick the snapshot
// (failCapture returns no requeue). Forbidden is NOT classified here — it is collected as unreadable and
// handled as fail-closed transient (ReasonNamespaceCaptureIncomplete) separately.
func isTransientCaptureTargetError(err error) bool {
	if err == nil {
		return false
	}
	if apierrors.IsServerTimeout(err) ||
		apierrors.IsTimeout(err) ||
		apierrors.IsTooManyRequests(err) ||
		apierrors.IsServiceUnavailable(err) ||
		apierrors.IsInternalError(err) ||
		apierrors.IsUnexpectedServerError(err) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "Informer to sync") ||
		strings.Contains(msg, "failed waiting for") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "connection reset")
}

// formatUnreadableGVRs renders unreadable GVRs for a degraded-condition message (stable order).
func formatUnreadableGVRs(gvrs []schema.GroupVersionResource) string {
	parts := make([]string, 0, len(gvrs))
	for _, gvr := range gvrs {
		parts = append(parts, gvr.String())
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

// setSnapshotReadyFalse writes Ready=False with reason/msg on a fresh Snapshot (conflict-retried).
// Used by pre-publish degrade/fail paths where a local Ready reason is allowed (no bound content mirror yet).
func (r *SnapshotReconciler) setSnapshotReadyFalse(ctx context.Context, nsSnap *storagev1alpha1.Snapshot, reason, msg string) error {
	nsKey := types.NamespacedName{Namespace: nsSnap.Namespace, Name: nsSnap.Name}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &storagev1alpha1.Snapshot{}
		if err := r.Client.Get(ctx, nsKey, fresh); err != nil {
			return err
		}
		fresh.Status.ObservedGeneration = fresh.Generation
		meta.SetStatusCondition(&fresh.Status.Conditions, metav1.Condition{
			Type:               snapshotpkg.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            msg,
			ObservedGeneration: fresh.Generation,
		})
		return r.Client.Status().Update(ctx, fresh)
	})
}

func (r *SnapshotReconciler) failCapture(ctx context.Context, nsSnap *storagev1alpha1.Snapshot, content *storagev1alpha1.SnapshotContent, reason, msg string) (ctrl.Result, error) {
	if err := r.setSnapshotReadyFalse(ctx, nsSnap, reason, msg); err != nil {
		return ctrl.Result{}, err
	}
	_ = content
	return ctrl.Result{}, nil
}

// ensureSnapshotRootObjectKeeper creates the cluster-scoped ret-snap-* ObjectKeeper.
// Root SnapshotContent gets metadata.ownerReferences -> that ObjectKeeper (controller) so that when the
// Deckhouse ObjectKeeper controller deletes the OK after follow+TTL, Kubernetes GC removes retained root content and
// cascades to MCP / child content. The OK itself must not list SnapshotContent in ownerReferences (wrong direction for TTL).
//
// spec.mode is always FollowObjectWithTTL; spec.followObjectRef targets the root Snapshot; spec.ttl is
// SnapshotRootOKTTL from controller config (env override or built-in default). Execution-chain ObjectKeepers (MCR) stay FollowObject without TTL.
func (r *SnapshotReconciler) ensureSnapshotRootObjectKeeper(ctx context.Context, nsSnap *storagev1alpha1.Snapshot, content *storagev1alpha1.SnapshotContent) (*deckhousev1alpha1.ObjectKeeper, ctrl.Result, error) {
	ok, res, err := controllercommon.EnsureRootObjectKeeperWithTTL(
		ctx,
		r.Client,
		r.APIReader,
		r.Config,
		nsSnap,
		storagev1alpha1.SchemeGroupVersion.WithKind(controllercommon.KindSnapshot),
	)
	if err != nil {
		return nil, ctrl.Result{}, err
	}
	if res.Requeue || res.RequeueAfter > 0 {
		return ok, res, nil
	}
	patchRes, err := r.ensureRootSnapshotContentOwnedByObjectKeeper(ctx, content, ok)
	if err != nil {
		return nil, ctrl.Result{}, err
	}
	if patchRes.Requeue || patchRes.RequeueAfter > 0 {
		return ok, patchRes, nil
	}
	return ok, ctrl.Result{}, nil
}

// ensureRootSnapshotContentOwnedByObjectKeeper patches root SnapshotContent to reference the root ObjectKeeper.
func (r *SnapshotReconciler) ensureRootSnapshotContentOwnedByObjectKeeper(
	ctx context.Context,
	content *storagev1alpha1.SnapshotContent,
	ok *deckhousev1alpha1.ObjectKeeper,
) (ctrl.Result, error) {
	changed, err := controllercommon.EnsureLifecycleOwnerRef(ctx, r.Client, content, controllercommon.RootObjectKeeperOwnerReference(ok))
	if err != nil {
		return ctrl.Result{}, err
	}
	if changed {
		return ctrl.Result{Requeue: true}, nil
	}
	return ctrl.Result{}, nil
}

// snapshotOwnerReferenceForMCR is set on ManifestCaptureRequest so Kubernetes GC removes the
// request when the Snapshot is deleted (same namespace; in-flight capture cleanup).
func snapshotOwnerReferenceForMCR(ns *storagev1alpha1.Snapshot) metav1.OwnerReference {
	b := true
	return metav1.OwnerReference{
		APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
		Kind:       "Snapshot",
		Name:       ns.Name,
		UID:        ns.UID,
		Controller: &b,
	}
}

func manifestCaptureRequestOwnerRefMatchesSnapshot(ref metav1.OwnerReference, ns *storagev1alpha1.Snapshot) bool {
	return ref.APIVersion == storagev1alpha1.SchemeGroupVersion.String() &&
		ref.Kind == "Snapshot" &&
		ref.Name == ns.Name &&
		ref.UID == ns.UID
}

func manifestCaptureRequestHasOwnerRefToSnapshot(refs []metav1.OwnerReference, ns *storagev1alpha1.Snapshot) bool {
	for i := range refs {
		if manifestCaptureRequestOwnerRefMatchesSnapshot(refs[i], ns) {
			return true
		}
	}
	return false
}

// manifestCaptureRequestConflictingSnapshotOwner is true if another Snapshot claims this MCR.
func manifestCaptureRequestConflictingSnapshotOwner(refs []metav1.OwnerReference, ns *storagev1alpha1.Snapshot) bool {
	for i := range refs {
		ref := refs[i]
		if ref.APIVersion == storagev1alpha1.SchemeGroupVersion.String() && ref.Kind == "Snapshot" &&
			(ref.Name != ns.Name || ref.UID != ns.UID) {
			return true
		}
	}
	return false
}

func (r *SnapshotReconciler) namespaceRootManifestCapturePersistedOnContent(ctx context.Context, content *storagev1alpha1.SnapshotContent) bool {
	mcpName := content.Status.ManifestCheckpointName
	if mcpName == "" {
		return false
	}
	mcp := &ssv1alpha1.ManifestCheckpoint{}
	if err := r.snapshotContentReader().Get(ctx, client.ObjectKey{Name: mcpName}, mcp); err != nil {
		return false
	}
	readyCond := meta.FindStatusCondition(mcp.Status.Conditions, ssv1alpha1.ManifestCheckpointConditionTypeReady)
	return readyCond != nil && readyCond.Status == metav1.ConditionTrue
}

func (r *SnapshotReconciler) ensureManifestCaptureRequest(ctx context.Context, nsSnap *storagev1alpha1.Snapshot, content *storagev1alpha1.SnapshotContent, targets []namespacemanifest.ManifestTarget) (*ssv1alpha1.ManifestCaptureRequest, ctrl.Result, error) {
	name := namespacemanifest.SnapshotMCRName(nsSnap.UID)
	key := types.NamespacedName{Namespace: nsSnap.Namespace, Name: name}

	specTargets := make([]ssv1alpha1.ManifestTarget, 0, len(targets))
	for _, t := range targets {
		specTargets = append(specTargets, ssv1alpha1.ManifestTarget{
			APIVersion: t.APIVersion,
			Kind:       t.Kind,
			Name:       t.Name,
		})
	}

	existing := &ssv1alpha1.ManifestCaptureRequest{}
	err := r.Client.Get(ctx, key, existing)
	switch {
	case apierrors.IsNotFound(err):
		freshContent, err := r.getSnapshotContentFresh(ctx, content.Name)
		if err == nil {
			if r.namespaceRootManifestCapturePersistedOnContent(ctx, freshContent) {
				// Another reconcile finished capture and deleted the MCR; avoid recreating the request.
				return nil, ctrl.Result{RequeueAfter: 100 * time.Millisecond}, nil
			}
		}
		mcr := &ssv1alpha1.ManifestCaptureRequest{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: nsSnap.Namespace,
				OwnerReferences: []metav1.OwnerReference{
					snapshotOwnerReferenceForMCR(nsSnap),
				},
				Labels: map[string]string{
					labelSnapshotUID: string(nsSnap.UID),
				},
			},
			Spec: ssv1alpha1.ManifestCaptureRequestSpec{Targets: specTargets},
		}
		if err := r.Client.Create(ctx, mcr); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return r.ensureManifestCaptureRequest(ctx, nsSnap, content, targets)
			}
			return nil, ctrl.Result{}, err
		}
		created := &ssv1alpha1.ManifestCaptureRequest{}
		if err := r.Client.Get(ctx, key, created); err != nil {
			return nil, ctrl.Result{RequeueAfter: 200 * time.Millisecond}, nil
		}
		if err := r.patchSnapshotManifestCaptureRequestName(ctx, nsSnap, name); err != nil {
			return nil, ctrl.Result{}, err
		}
		return created, ctrl.Result{}, nil
	case err != nil:
		return nil, ctrl.Result{}, err
	default:
		if existing.Labels == nil || existing.Labels[labelSnapshotUID] != string(nsSnap.UID) {
			return nil, ctrl.Result{}, fmt.Errorf("ManifestCaptureRequest %s exists but is not owned by this Snapshot (stale or manual)", key.String())
		}
		if manifestCaptureRequestConflictingSnapshotOwner(existing.OwnerReferences, nsSnap) {
			return nil, ctrl.Result{}, fmt.Errorf("ManifestCaptureRequest %s has ownerReference to a different Snapshot", key.String())
		}
		if !manifestCaptureRequestHasOwnerRefToSnapshot(existing.OwnerReferences, nsSnap) {
			base := existing.DeepCopy()
			existing.OwnerReferences = append(existing.OwnerReferences, snapshotOwnerReferenceForMCR(nsSnap))
			if err := r.Client.Patch(ctx, existing, client.MergeFrom(base)); err != nil {
				return nil, ctrl.Result{}, err
			}
			return nil, ctrl.Result{Requeue: true}, nil
		}
		if !manifestTargetsEqual(existing.Spec.Targets, specTargets) {
			return nil, ctrl.Result{}, fmt.Errorf("%w: ManifestCaptureRequest %s spec.targets differ from current resolved capture targets; delete the MCR to retry with a fresh plan", errManifestCapturePlanDrift, key.String())
		}
		if err := r.patchSnapshotManifestCaptureRequestName(ctx, nsSnap, name); err != nil {
			return nil, ctrl.Result{}, err
		}
		return existing, ctrl.Result{}, nil
	}
}

func (r *SnapshotReconciler) patchSnapshotManifestCaptureRequestName(ctx context.Context, nsSnap *storagev1alpha1.Snapshot, name string) error {
	key := types.NamespacedName{Namespace: nsSnap.Namespace, Name: nsSnap.Name}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &storagev1alpha1.Snapshot{}
		if err := r.Client.Get(ctx, key, fresh); err != nil {
			return err
		}
		if fresh.Status.ManifestCaptureRequestName == name {
			return nil
		}
		base := fresh.DeepCopy()
		fresh.Status.ManifestCaptureRequestName = name
		return r.Client.Status().Patch(ctx, fresh, client.MergeFrom(base))
	})
}

// manifestTargetsEqual compares capture plans in canonical order (APIVersion, Kind, Name).
// New MCR targets are always sorted; existing spec may be unsorted, so both sides are sorted before compare.
func manifestTargetsEqual(a, b []ssv1alpha1.ManifestTarget) bool {
	aa := append([]ssv1alpha1.ManifestTarget(nil), a...)
	bb := append([]ssv1alpha1.ManifestTarget(nil), b...)
	sortManifestSpecTargets(aa)
	sortManifestSpecTargets(bb)
	if len(aa) != len(bb) {
		return false
	}
	for i := range aa {
		if aa[i].APIVersion != bb[i].APIVersion || aa[i].Kind != bb[i].Kind || aa[i].Name != bb[i].Name {
			return false
		}
	}
	return true
}

func sortManifestSpecTargets(ts []ssv1alpha1.ManifestTarget) {
	sort.Slice(ts, func(i, j int) bool {
		a, b := ts[i], ts[j]
		if a.APIVersion != b.APIVersion {
			return a.APIVersion < b.APIVersion
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Name < b.Name
	})
}
