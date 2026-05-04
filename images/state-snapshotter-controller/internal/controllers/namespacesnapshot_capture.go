/*
Copyright 2025 Flant JSC

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

package controllers

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	deckhousev1alpha1 "github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/config"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshotgraphregistry"
)

const labelNamespaceSnapshotUID = "state-snapshotter.deckhouse.io/namespace-snapshot-uid"

// errManifestCapturePlanDrift is returned when an existing MCR for this NamespaceSnapshot has a different
// target set than the current namespace listing; MCR spec is not silently rewritten.
var errManifestCapturePlanDrift = errors.New("manifest capture plan drift")

// deleteNamespaceSnapshotManifestCaptureRequest removes the namespace-flow MCR after capture is persisted.
// NotFound is success; other errors are returned so the reconciler can retry.
func (r *NamespaceSnapshotReconciler) deleteNamespaceSnapshotManifestCaptureRequest(ctx context.Context, nsSnap *storagev1alpha1.NamespaceSnapshot) error {
	key := types.NamespacedName{Namespace: nsSnap.Namespace, Name: namespacemanifest.NamespaceSnapshotMCRName(nsSnap.UID)}
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

// mirrorSubtreeManifestCapturePendingOnContent mirrors E5 subtree wait onto root SnapshotContent so
// the content object does not look Ready while exclude cannot be computed yet.
func (r *NamespaceSnapshotReconciler) mirrorSubtreeManifestCapturePendingOnContent(ctx context.Context, contentName, msg string) error {
	fresh := &storagev1alpha1.SnapshotContent{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: contentName}, fresh); err != nil {
		return err
	}
	meta.SetStatusCondition(&fresh.Status.Conditions, metav1.Condition{
		Type:               snapshot.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             snapshot.ReasonSubtreeManifestCapturePending,
		Message:            msg,
		ObservedGeneration: fresh.Generation,
	})
	return r.Client.Status().Update(ctx, fresh)
}

// reconcileChildrenRefsE6ParentReadyOrPatch applies E6 aggregation for status.childrenSnapshotRefs (strict
// apiVersion/kind/name refs; single Get per child). Returns (allChildrenAllowParentSuccess, result, err):
// when false, result is from patchNamespaceSnapshotReadyFromE6 and the caller must return it; when true, caller may mark root capture complete.
func (r *NamespaceSnapshotReconciler) reconcileChildrenRefsE6ParentReadyOrPatch(
	ctx context.Context,
	parent *storagev1alpha1.NamespaceSnapshot,
	subtreePending bool,
	subtreeMsg string,
	selfCaptureComplete bool,
) (allChildrenAllowParentSuccess bool, res ctrl.Result, err error) {
	if len(parent.Status.ChildrenSnapshotRefs) == 0 {
		return true, ctrl.Result{}, nil
	}
	sum, err := usecase.SummarizeChildrenSnapshotRefsForParentReadyE6(ctx, r.e6ChildStatusReader(), parent.Status.ChildrenSnapshotRefs, parent.Namespace)
	if err != nil {
		return false, ctrl.Result{}, err
	}
	in := usecase.E6ParentReadyPickInput{
		HasChildFailed:                sum.HasFailed,
		ChildFailedMessage:            usecase.JoinNonEmpty(sum.FailedMessages, "; "),
		SubtreeManifestCapturePending: subtreePending,
		SubtreeMessage:                subtreeMsg,
		HasChildPending:               sum.HasPending,
		ChildPendingMessage:           usecase.JoinNonEmpty(sum.PendingParts, "; "),
		SelfCaptureComplete:           selfCaptureComplete,
	}
	out := usecase.PickParentReadyReasonE6(in)
	if out.Ready {
		return true, ctrl.Result{}, nil
	}
	parentKey := types.NamespacedName{Namespace: parent.Namespace, Name: parent.Name}
	res, err = r.patchNamespaceSnapshotReadyFromE6(ctx, parentKey, out.Reason, out.Message)
	return false, res, err
}

// reconcileCaptureN2a drives manifest capture via MCR->ManifestCheckpoint after root SnapshotContent is bound.
func (r *NamespaceSnapshotReconciler) reconcileCaptureN2a(
	ctx context.Context,
	nsSnap *storagev1alpha1.NamespaceSnapshot,
	content *storagev1alpha1.SnapshotContent,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if r.Dynamic == nil {
		return ctrl.Result{}, fmt.Errorf("namespace snapshot reconciler: Dynamic client is nil")
	}
	if r.APIReader == nil {
		return ctrl.Result{}, fmt.Errorf("namespace snapshot reconciler: APIReader is nil")
	}

	_, res, err := r.ensureNamespaceSnapshotRootObjectKeeper(ctx, nsSnap, content)
	if err != nil {
		return ctrl.Result{}, err
	}
	if res.RequeueAfter > 0 || res.Requeue {
		return res, nil
	}

	if err := r.Client.Get(ctx, client.ObjectKey{Name: content.Name}, content); err != nil {
		return ctrl.Result{}, err
	}
	if done, res, err := r.reconcileIfRootManifestCheckpointAlreadyReady(ctx, nsSnap, content); done {
		return res, err
	}

	targets, err := usecase.BuildRootNamespaceManifestCaptureTargets(ctx, r.Archive, r.Dynamic, r.Client, nsSnap, content.Name)
	if err != nil {
		freshParent := &storagev1alpha1.NamespaceSnapshot{}
		if gerr := r.Client.Get(ctx, client.ObjectKey{Namespace: nsSnap.Namespace, Name: nsSnap.Name}, freshParent); gerr != nil {
			return ctrl.Result{}, gerr
		}
		hasSubtree := len(freshParent.Status.ChildrenSnapshotRefs) > 0
		// Transient subtree state while childrenSnapshotRefs are populated or child snapshot is still binding;
		// do not fail capture as ListFailed — requeue like ChildSnapshotPending.
		transientChildGraph := errors.Is(err, usecase.ErrSubtreeManifestCapturePending) ||
			errors.Is(err, usecase.ErrRunGraphChildNotBound) ||
			errors.Is(err, usecase.ErrRunGraphChildSnapshotNotFound) ||
			(hasSubtree && errors.Is(err, usecase.ErrRunGraphChildNotReachable)) ||
			(hasSubtree && errors.Is(err, snapshotgraphregistry.ErrGraphRegistryNotReady))
		if transientChildGraph {
			cur := meta.FindStatusCondition(freshParent.Status.Conditions, snapshot.ConditionReady)
			if cur != nil && cur.Reason == snapshot.ReasonChildSnapshotFailed {
				return ctrl.Result{RequeueAfter: 500 * time.Millisecond}, nil
			}
			var reason string
			var msg string
			if hasSubtree {
				sum, serr := usecase.SummarizeChildrenSnapshotRefsForParentReadyE6(ctx, r.e6ChildStatusReader(), freshParent.Status.ChildrenSnapshotRefs, freshParent.Namespace)
				if serr != nil {
					return ctrl.Result{}, serr
				}
				out := usecase.PickParentReadyReasonE6(usecase.E6ParentReadyPickInput{
					HasChildFailed:                sum.HasFailed,
					ChildFailedMessage:            usecase.JoinNonEmpty(sum.FailedMessages, "; "),
					SubtreeManifestCapturePending: true,
					SubtreeMessage:                err.Error(),
					HasChildPending:               sum.HasPending,
					ChildPendingMessage:           usecase.JoinNonEmpty(sum.PendingParts, "; "),
					SelfCaptureComplete:           false,
				})
				if out.Reason == snapshot.ReasonChildSnapshotFailed {
					parentKey := types.NamespacedName{Namespace: freshParent.Namespace, Name: freshParent.Name}
					return r.patchNamespaceSnapshotReadyFromE6(ctx, parentKey, out.Reason, out.Message)
				}
				reason = out.Reason
				msg = out.Message
			} else {
				reason = snapshot.ReasonChildSnapshotPending
				msg = err.Error()
			}
			// E5 delayed first MCR: do not leave a root MCR while subtree exclude cannot be computed (stale plan vs exclude).
			if hasSubtree {
				if delErr := r.deleteNamespaceSnapshotManifestCaptureRequest(ctx, freshParent); delErr != nil {
					return ctrl.Result{}, delErr
				}
			}
			meta.SetStatusCondition(&freshParent.Status.Conditions, metav1.Condition{
				Type:               snapshot.ConditionReady,
				Status:             metav1.ConditionFalse,
				Reason:             reason,
				Message:            msg,
				ObservedGeneration: freshParent.Generation,
			})
			if uerr := r.Client.Status().Update(ctx, freshParent); uerr != nil {
				return ctrl.Result{}, uerr
			}
			if hasSubtree && reason == snapshot.ReasonSubtreeManifestCapturePending {
				if uerr := r.mirrorSubtreeManifestCapturePendingOnContent(ctx, content.Name, msg); uerr != nil {
					return ctrl.Result{}, uerr
				}
			}
			return ctrl.Result{RequeueAfter: 500 * time.Millisecond}, nil
		}
		if errors.Is(err, usecase.ErrSubtreeManifestCaptureFailed) {
			return r.failCapture(ctx, freshParent, content, "SubtreeManifestFailed", err.Error())
		}
		return r.failCapture(ctx, freshParent, content, "ListFailed", fmt.Sprintf("build capture targets: %v", err))
	}
	mcr, res, err := r.ensureManifestCaptureRequest(ctx, nsSnap, content, targets)
	if err != nil {
		if errors.Is(err, errManifestCapturePlanDrift) {
			freshParent := &storagev1alpha1.NamespaceSnapshot{}
			if gerr := r.Client.Get(ctx, client.ObjectKey{Namespace: nsSnap.Namespace, Name: nsSnap.Name}, freshParent); gerr != nil {
				return ctrl.Result{}, gerr
			}
			// Subtree-root: plan drift is not the primary convergence path; delete stale MCR and retry with fresh targets.
			// Plain N2a (no childrenSnapshotRefs): terminal CapturePlanDrift for operator visibility.
			if len(freshParent.Status.ChildrenSnapshotRefs) > 0 {
				if delErr := r.deleteNamespaceSnapshotManifestCaptureRequest(ctx, freshParent); delErr != nil {
					return ctrl.Result{}, delErr
				}
				return ctrl.Result{RequeueAfter: 200 * time.Millisecond}, nil
			}
			return r.failCapture(ctx, freshParent, content, "CapturePlanDrift", err.Error())
		}
		return ctrl.Result{}, err
	}
	if res.RequeueAfter > 0 || res.Requeue {
		return res, nil
	}

	mcpName := namespacemanifest.GenerateManifestCheckpointNameFromUID(mcr.UID)
	mcp := &ssv1alpha1.ManifestCheckpoint{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: mcpName}, mcp); err != nil {
		if apierrors.IsNotFound(err) {
			meta.SetStatusCondition(&nsSnap.Status.Conditions, metav1.Condition{
				Type:               snapshot.ConditionReady,
				Status:             metav1.ConditionFalse,
				Reason:             "ManifestCheckpointPending",
				Message:            fmt.Sprintf("waiting for ManifestCheckpoint %q", mcpName),
				ObservedGeneration: nsSnap.Generation,
			})
			if err := r.Client.Status().Update(ctx, nsSnap); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: 500 * time.Millisecond}, nil
		}
		return ctrl.Result{}, err
	}

	readyCond := meta.FindStatusCondition(mcp.Status.Conditions, ssv1alpha1.ManifestCheckpointConditionTypeReady)
	if readyCond == nil || readyCond.Status != metav1.ConditionTrue {
		reason := "ManifestCheckpointNotReady"
		msg := "ManifestCheckpoint not ready"
		if readyCond != nil {
			reason = readyCond.Reason
			msg = readyCond.Message
		}
		if readyCond != nil && readyCond.Status == metav1.ConditionFalse &&
			(readyCond.Reason == ssv1alpha1.ManifestCheckpointConditionReasonFailed) {
			return r.failCapture(ctx, nsSnap, content, "ManifestCheckpointFailed", msg)
		}
		meta.SetStatusCondition(&nsSnap.Status.Conditions, metav1.Condition{
			Type:               snapshot.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            msg,
			ObservedGeneration: nsSnap.Generation,
		})
		if err := r.Client.Status().Update(ctx, nsSnap); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 500 * time.Millisecond}, nil
	}

	if readyCond.Reason != ssv1alpha1.ManifestCheckpointConditionReasonCompleted {
		// Ready=True with unexpected reason — still treat as success if True (defensive).
		logger.Info("ManifestCheckpoint Ready=True with non-Completed reason", "reason", readyCond.Reason, "mcp", mcpName)
	}

	// Persist checkpoint name on content and mirror success on SnapshotContent.status.conditions.
	contentKey := client.ObjectKey{Name: content.Name}
	if err := r.Client.Get(ctx, contentKey, content); err != nil {
		return ctrl.Result{}, err
	}
	if content.Status.ManifestCheckpointName != mcpName {
		content.Status.ManifestCheckpointName = mcpName
	}
	meta.SetStatusCondition(&content.Status.Conditions, metav1.Condition{
		Type:               snapshot.ConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             snapshot.ReasonCompleted,
		Message:            fmt.Sprintf("manifest capture persisted (ManifestCheckpoint %s)", mcpName),
		ObservedGeneration: content.Generation,
	})
	if err := r.Client.Status().Update(ctx, content); err != nil {
		return ctrl.Result{}, err
	}

	return r.reconcileN2aRootReadyAfterManifestCapture(ctx, nsSnap, mcpName)
}

// reconcileIfRootManifestCheckpointAlreadyReady handles idempotent steady state: MCP name on SnapshotContent, MCP Ready,
// NamespaceSnapshot Ready, and MCR already removed. Skips recreating MCR when capture is complete.
func (r *NamespaceSnapshotReconciler) reconcileIfRootManifestCheckpointAlreadyReady(
	ctx context.Context,
	nsSnap *storagev1alpha1.NamespaceSnapshot,
	content *storagev1alpha1.SnapshotContent,
) (done bool, res ctrl.Result, err error) {
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

	// Steady-state fast path only after the request is gone; while MCR exists we must run
	// ensureManifestCaptureRequest (capture plan drift vs live namespace).
	mcrKey := types.NamespacedName{Namespace: nsSnap.Namespace, Name: namespacemanifest.NamespaceSnapshotMCRName(nsSnap.UID)}
	staleMCR := &ssv1alpha1.ManifestCaptureRequest{}
	if err := r.Client.Get(ctx, mcrKey, staleMCR); err == nil {
		// MCR can remain while E6 was !allClear (children not ready). MCP is already Ready — retry E6 so
		// child-driven reconciles can finish without requiring another MCP completion cycle.
		res, err := r.reconcileN2aRootReadyAfterManifestCapture(ctx, nsSnap, mcpName)
		if err != nil {
			return true, res, err
		}
		freshNS := &storagev1alpha1.NamespaceSnapshot{}
		nsKey := types.NamespacedName{Namespace: nsSnap.Namespace, Name: nsSnap.Name}
		var gerr error
		if r.APIReader != nil {
			gerr = r.APIReader.Get(ctx, nsKey, freshNS)
		} else {
			gerr = r.Client.Get(ctx, nsKey, freshNS)
		}
		if gerr != nil {
			return true, ctrl.Result{}, gerr
		}
		rc := meta.FindStatusCondition(freshNS.Status.Conditions, snapshot.ConditionReady)
		if rc != nil && rc.Status == metav1.ConditionTrue && rc.Reason == snapshot.ReasonCompleted {
			return true, res, nil
		}
		// MCP is already Ready while MCR still exists (E6 gate). Do not fall through to
		// BuildRootNamespaceManifestCaptureTargets — that path treats the subtree as capture-pending
		// and prevents E6 from converging. Return done=true so this reconcile ends after E6/requeue.
		return true, res, nil
	} else if !apierrors.IsNotFound(err) {
		return true, ctrl.Result{}, err
	}

	res, err = r.reconcileN2aRootReadyAfterManifestCapture(ctx, nsSnap, mcpName)
	return true, res, err
}

func (r *NamespaceSnapshotReconciler) reconcileN2aRootReadyAfterManifestCapture(
	ctx context.Context,
	nsSnap *storagev1alpha1.NamespaceSnapshot,
	mcpName string,
) (ctrl.Result, error) {
	fresh := &storagev1alpha1.NamespaceSnapshot{}
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
	allClear, res, err := r.reconcileChildrenRefsE6ParentReadyOrPatch(ctx, fresh, false, "", true)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !allClear {
		return res, nil
	}
	ready := meta.FindStatusCondition(fresh.Status.Conditions, snapshot.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionTrue || ready.Reason != snapshot.ReasonCompleted {
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			cur := &storagev1alpha1.NamespaceSnapshot{}
			if err := r.Client.Get(ctx, nsKey, cur); err != nil {
				return err
			}
			cur.Status.ObservedGeneration = cur.Generation
			meta.SetStatusCondition(&cur.Status.Conditions, metav1.Condition{
				Type:               snapshot.ConditionReady,
				Status:             metav1.ConditionTrue,
				Reason:             snapshot.ReasonCompleted,
				Message:            namespaceSnapshotReadyMessage(mcpName),
				ObservedGeneration: cur.Generation,
			})
			return r.Client.Status().Update(ctx, cur)
		}); err != nil {
			return ctrl.Result{}, err
		}
	}
	post := &storagev1alpha1.NamespaceSnapshot{}
	if err := r.Client.Get(ctx, nsKey, post); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.deleteNamespaceSnapshotManifestCaptureRequest(ctx, post); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func namespaceSnapshotReadyMessage(mcpName string) string {
	return fmt.Sprintf("manifest capture complete (ManifestCheckpoint %s)", mcpName)
}

func (r *NamespaceSnapshotReconciler) failCapture(ctx context.Context, nsSnap *storagev1alpha1.NamespaceSnapshot, content *storagev1alpha1.SnapshotContent, reason, msg string) (ctrl.Result, error) {
	nsSnap.Status.ObservedGeneration = nsSnap.Generation
	meta.SetStatusCondition(&nsSnap.Status.Conditions, metav1.Condition{
		Type:               snapshot.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: nsSnap.Generation,
	})
	if err := r.Client.Status().Update(ctx, nsSnap); err != nil {
		return ctrl.Result{}, err
	}
	if content != nil && content.Name != "" {
		fresh := &storagev1alpha1.SnapshotContent{}
		if err := r.Client.Get(ctx, client.ObjectKey{Name: content.Name}, fresh); err != nil {
			if apierrors.IsNotFound(err) {
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, err
		}
		meta.SetStatusCondition(&fresh.Status.Conditions, metav1.Condition{
			Type:               snapshot.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            msg,
			ObservedGeneration: fresh.Generation,
		})
		if err := r.Client.Status().Update(ctx, fresh); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

// ensureNamespaceSnapshotRootObjectKeeper creates the cluster-scoped ret-nssnap-* ObjectKeeper.
// Root SnapshotContent gets metadata.ownerReferences -> that ObjectKeeper (controller) so that when the
// Deckhouse ObjectKeeper controller deletes the OK after follow+TTL, Kubernetes GC removes retained root content and
// cascades to MCP / child content. The OK itself must not list SnapshotContent in ownerReferences (wrong direction for TTL).
//
// spec.mode is always FollowObjectWithTTL; spec.followObjectRef targets the root NamespaceSnapshot; spec.ttl is
// SnapshotRootOKTTL from controller config (env override or built-in default). Execution-chain ObjectKeepers (MCR) stay FollowObject without TTL.
func (r *NamespaceSnapshotReconciler) ensureNamespaceSnapshotRootObjectKeeper(ctx context.Context, nsSnap *storagev1alpha1.NamespaceSnapshot, content *storagev1alpha1.SnapshotContent) (*deckhousev1alpha1.ObjectKeeper, ctrl.Result, error) {
	logger := log.FromContext(ctx)
	name := namespacemanifest.NamespaceSnapshotRootObjectKeeperName(nsSnap.Namespace, nsSnap.Name)
	ok := &deckhousev1alpha1.ObjectKeeper{}
	err := r.Client.Get(ctx, client.ObjectKey{Name: name}, ok)

	ttl := config.DefaultSnapshotRootOKTTL
	if r.Config != nil && r.Config.SnapshotRootOKTTL > 0 {
		ttl = r.Config.SnapshotRootOKTTL
	}
	followSnap := &deckhousev1alpha1.FollowObjectRef{
		APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
		Kind:       "NamespaceSnapshot",
		Namespace:  nsSnap.Namespace,
		Name:       nsSnap.Name,
		UID:        string(nsSnap.UID),
	}
	spec := deckhousev1alpha1.ObjectKeeperSpec{
		Mode:            ObjectKeeperModeFollowObjectWithTTL,
		FollowObjectRef: followSnap,
		TTL:             &metav1.Duration{Duration: ttl},
	}

	okMatches := func(o *deckhousev1alpha1.ObjectKeeper) bool {
		if o.Spec.Mode != spec.Mode {
			return false
		}
		if o.Spec.FollowObjectRef == nil || spec.FollowObjectRef == nil {
			return false
		}
		fr := o.Spec.FollowObjectRef
		want := spec.FollowObjectRef
		if fr.APIVersion != want.APIVersion || fr.Kind != want.Kind || fr.Name != want.Name || fr.Namespace != want.Namespace || fr.UID != want.UID {
			return false
		}
		if o.Spec.TTL == nil || spec.TTL == nil || o.Spec.TTL.Duration != spec.TTL.Duration {
			return false
		}
		return true
	}

	switch {
	case apierrors.IsNotFound(err):
		ok = &deckhousev1alpha1.ObjectKeeper{
			TypeMeta: metav1.TypeMeta{
				APIVersion: DeckhouseAPIVersion,
				Kind:       KindObjectKeeper,
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
			},
			Spec: spec,
		}
		if err := r.Client.Create(ctx, ok); err != nil {
			return nil, ctrl.Result{}, fmt.Errorf("create ObjectKeeper %s: %w", name, err)
		}
		if err := r.APIReader.Get(ctx, client.ObjectKey{Name: name}, ok); err != nil {
			return nil, ctrl.Result{RequeueAfter: 200 * time.Millisecond}, nil
		}
		logger.Info("created root ObjectKeeper for NamespaceSnapshot", "objectKeeper", name, "mode", spec.Mode)
		patchRes, err := r.ensureRootSnapshotContentOwnedByObjectKeeper(ctx, content, ok)
		if err != nil {
			return nil, ctrl.Result{}, err
		}
		if patchRes.Requeue || patchRes.RequeueAfter > 0 {
			return ok, patchRes, nil
		}
		return ok, ctrl.Result{}, nil
	case err != nil:
		return nil, ctrl.Result{}, err
	default:
		if !okMatches(ok) {
			// Correct our declared spec in place. ObjectKeeper lifecycle and deletion are owned by the
			// Deckhouse ObjectKeeper controller; we do not delete OKs from this reconciler.
			ok.Spec = spec
			if err := r.Client.Update(ctx, ok); err != nil {
				return nil, ctrl.Result{}, fmt.Errorf("update ObjectKeeper %s (spec drift): %w", name, err)
			}
			logger.Info("updated root ObjectKeeper spec (corrected drift)", "objectKeeper", name)
			if err := r.APIReader.Get(ctx, client.ObjectKey{Name: name}, ok); err != nil {
				return nil, ctrl.Result{RequeueAfter: 200 * time.Millisecond}, nil
			}
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
}

// ensureRootSnapshotContentOwnedByObjectKeeper patches root SnapshotContent to reference the root ObjectKeeper.
func (r *NamespaceSnapshotReconciler) ensureRootSnapshotContentOwnedByObjectKeeper(
	ctx context.Context,
	content *storagev1alpha1.SnapshotContent,
	ok *deckhousev1alpha1.ObjectKeeper,
) (ctrl.Result, error) {
	want := namespaceSnapshotRootContentOwnerReferenceToOK(ok)
	for _, ref := range content.OwnerReferences {
		if ref.APIVersion == want.APIVersion && ref.Kind == want.Kind && ref.Name == want.Name && ref.UID == want.UID {
			return ctrl.Result{}, nil
		}
	}
	base := content.DeepCopy()
	content.OwnerReferences = append(content.OwnerReferences, want)
	if err := r.Client.Patch(ctx, content, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

func namespaceSnapshotRootContentOwnerReferenceToOK(ok *deckhousev1alpha1.ObjectKeeper) metav1.OwnerReference {
	b := true
	return metav1.OwnerReference{
		APIVersion: DeckhouseAPIVersion,
		Kind:       KindObjectKeeper,
		Name:       ok.Name,
		UID:        ok.UID,
		Controller: &b,
	}
}

// namespaceSnapshotOwnerReferenceForMCR is set on ManifestCaptureRequest so Kubernetes GC removes the
// request when the NamespaceSnapshot is deleted (same namespace; in-flight capture cleanup).
func namespaceSnapshotOwnerReferenceForMCR(ns *storagev1alpha1.NamespaceSnapshot) metav1.OwnerReference {
	b := true
	return metav1.OwnerReference{
		APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
		Kind:       "NamespaceSnapshot",
		Name:       ns.Name,
		UID:        ns.UID,
		Controller: &b,
	}
}

func manifestCaptureRequestOwnerRefMatchesNamespaceSnapshot(ref metav1.OwnerReference, ns *storagev1alpha1.NamespaceSnapshot) bool {
	return ref.APIVersion == storagev1alpha1.SchemeGroupVersion.String() &&
		ref.Kind == "NamespaceSnapshot" &&
		ref.Name == ns.Name &&
		ref.UID == ns.UID
}

func manifestCaptureRequestHasOwnerRefToNamespaceSnapshot(refs []metav1.OwnerReference, ns *storagev1alpha1.NamespaceSnapshot) bool {
	for i := range refs {
		if manifestCaptureRequestOwnerRefMatchesNamespaceSnapshot(refs[i], ns) {
			return true
		}
	}
	return false
}

// manifestCaptureRequestConflictingNamespaceSnapshotOwner is true if another NamespaceSnapshot claims this MCR.
func manifestCaptureRequestConflictingNamespaceSnapshotOwner(refs []metav1.OwnerReference, ns *storagev1alpha1.NamespaceSnapshot) bool {
	for i := range refs {
		ref := refs[i]
		if ref.APIVersion == storagev1alpha1.SchemeGroupVersion.String() && ref.Kind == "NamespaceSnapshot" &&
			(ref.Name != ns.Name || ref.UID != ns.UID) {
			return true
		}
	}
	return false
}

func (r *NamespaceSnapshotReconciler) namespaceRootManifestCapturePersistedOnContent(ctx context.Context, content *storagev1alpha1.SnapshotContent) bool {
	mcpName := content.Status.ManifestCheckpointName
	if mcpName == "" {
		return false
	}
	mcp := &ssv1alpha1.ManifestCheckpoint{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: mcpName}, mcp); err != nil {
		return false
	}
	readyCond := meta.FindStatusCondition(mcp.Status.Conditions, ssv1alpha1.ManifestCheckpointConditionTypeReady)
	return readyCond != nil && readyCond.Status == metav1.ConditionTrue
}

func (r *NamespaceSnapshotReconciler) ensureManifestCaptureRequest(ctx context.Context, nsSnap *storagev1alpha1.NamespaceSnapshot, content *storagev1alpha1.SnapshotContent, targets []namespacemanifest.ManifestTarget) (*ssv1alpha1.ManifestCaptureRequest, ctrl.Result, error) {
	name := namespacemanifest.NamespaceSnapshotMCRName(nsSnap.UID)
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
		freshContent := &storagev1alpha1.SnapshotContent{}
		if err := r.Client.Get(ctx, client.ObjectKey{Name: content.Name}, freshContent); err == nil {
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
					namespaceSnapshotOwnerReferenceForMCR(nsSnap),
				},
				Labels: map[string]string{
					labelNamespaceSnapshotUID: string(nsSnap.UID),
				},
				Annotations: map[string]string{
					namespacemanifest.AnnotationBoundSnapshotContent: content.Name,
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
		return created, ctrl.Result{}, nil
	case err != nil:
		return nil, ctrl.Result{}, err
	default:
		if existing.Labels == nil || existing.Labels[labelNamespaceSnapshotUID] != string(nsSnap.UID) {
			return nil, ctrl.Result{}, fmt.Errorf("ManifestCaptureRequest %s exists but is not owned by this NamespaceSnapshot (stale or manual)", key.String())
		}
		if manifestCaptureRequestConflictingNamespaceSnapshotOwner(existing.OwnerReferences, nsSnap) {
			return nil, ctrl.Result{}, fmt.Errorf("ManifestCaptureRequest %s has ownerReference to a different NamespaceSnapshot", key.String())
		}
		if !manifestCaptureRequestHasOwnerRefToNamespaceSnapshot(existing.OwnerReferences, nsSnap) {
			base := existing.DeepCopy()
			existing.OwnerReferences = append(existing.OwnerReferences, namespaceSnapshotOwnerReferenceForMCR(nsSnap))
			if err := r.Client.Patch(ctx, existing, client.MergeFrom(base)); err != nil {
				return nil, ctrl.Result{}, err
			}
			return nil, ctrl.Result{Requeue: true}, nil
		}
		if existing.Annotations == nil || existing.Annotations[namespacemanifest.AnnotationBoundSnapshotContent] != content.Name {
			base := existing.DeepCopy()
			if existing.Annotations == nil {
				existing.Annotations = map[string]string{}
			}
			existing.Annotations[namespacemanifest.AnnotationBoundSnapshotContent] = content.Name
			if err := r.Client.Patch(ctx, existing, client.MergeFrom(base)); err != nil {
				return nil, ctrl.Result{}, err
			}
			return nil, ctrl.Result{Requeue: true}, nil
		}
		if !manifestTargetsEqual(existing.Spec.Targets, specTargets) {
			return nil, ctrl.Result{}, fmt.Errorf("%w: ManifestCaptureRequest %s spec.targets differ from current resolved capture targets; delete the MCR to retry with a fresh plan", errManifestCapturePlanDrift, key.String())
		}
		return existing, ctrl.Result{}, nil
	}
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
