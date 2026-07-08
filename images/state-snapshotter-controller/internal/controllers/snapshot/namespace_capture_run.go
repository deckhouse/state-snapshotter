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
	stderrors "errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	volumecaptureuc "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase/volumecapture"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/csdregistry"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshotgraphregistry"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk"
)

// captureSDK constructs the in-process capture SDK the namespace root drives — the SAME SDK external/demo
// domains use ("dogfooding", wave5). Mirrors the demo controllers' capture() helper
// (snapshotsdk.New(client, apiReader, NewStorageFoundationProvider(client))). The root's manifest-exclude
// leg is computed in-reconciler from the bound content subtree (BuildRootNamespaceManifestCaptureTargets),
// so the SDK's optional subresource REST client (WithSubresourceREST) is intentionally NOT wired here.
func (r *SnapshotReconciler) captureSDK() snapshotsdk.CaptureSDK {
	return snapshotsdk.New(r.Client, r.APIReader, snapshotsdk.NewStorageFoundationProvider(r.Client))
}

// reconcileNamespaceCapture drives the namespace-root capture through the snapshotsdk recipe (wave5
// content-free flip). It replaces the bespoke create-content + parent_graph + reconcileCaptureN2a path:
// the generic binder (which now watches the root — see unifiedbootstrap.DomainCaptureSnapshotKinds) is the
// SOLE SnapshotContent creator/binder/Ready-mirror, while the root only PLANS via the SDK and drives its
// own residual/orphan + manifest legs.
//
// Recipe (mirrors demo/virtualmachinesnapshot_controller.go, adapted for the aggregator root whose
// manifest leg is subtree-gated and therefore runs AFTER the content is bound):
//  1. PublishSnapshotSource (the captured Namespace) — d8-cli import-mode recreation.
//  2. planNamespaceChildren -> EnsureChildren (create/adopt + ADDITIVE publish of childrenSnapshotRefs).
//  3. Residual/orphan PVC wave (root-owned), CONTENT-FREE and BEFORE barrier 1 ("late Planned"): create
//     CSI VolumeSnapshots for uncovered PVCs and publish their leaves onto childrenSnapshotRefs, so the
//     FULL child set (domain + orphan) is enumerated before the content is created/frozen.
//  4. MarkPlanned (barrier 1) — unblocks the binder to create/bind the root SnapshotContent with the full
//     frozen child set.
//  5. Wait for the binder to bind the content; then maintain the RBAC latch.
//  6. Orphan child-content linking (post-bind): materialize + link each orphan PVC's child volume node.
//  7. Manifest-exclude leg: EnsureManifestCapture(base namespace allowlist − subtree already captured).
//  8. ConfirmConsistent (barrier 2) once the manifest leg is captured (CoreCaptureOutcome==Captured).
func (r *SnapshotReconciler) reconcileNamespaceCapture(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	ns *corev1.Namespace,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	key := types.NamespacedName{Namespace: nsSnap.Namespace, Name: nsSnap.Name}
	adapter := NewNamespaceSnapshotAdapter(nsSnap)
	sdk := r.captureSDK()

	// 1. Publish the captured live source (the Namespace) into status.snapshotSource.
	if err := sdk.PublishSnapshotSource(ctx, adapter, snapshotsdk.SnapshotSource{
		APIVersion: "v1",
		Kind:       "Namespace",
		Name:       nsSnap.Namespace,
		UID:        ns.UID,
	}); err != nil {
		return ctrl.Result{}, err
	}

	// 2. Plan the domain child graph (pure planner: builds ChildSpecs, no creation, no status writes).
	mappings, err := csdregistry.EligibleResourceSnapshotMappings(ctx, r.snapshotReader(), r.Mgr.GetRESTMapper())
	if err != nil {
		return ctrl.Result{}, err
	}
	plan, err := r.planNamespaceChildren(ctx, nsSnap, mappings)
	if err != nil {
		// A hard planning error (e.g. resourceSelector parse, coverage read): degrade Ready and requeue.
		// The binder is still gated on phase>=Planned here (MarkPlanned not reached), so it does not touch
		// Ready — no dual-writer. (A source-list Forbidden is not an error here: planNamespaceChildren folds
		// it into the non-terminal Forbidden outcome, handled below via the not-AllPlanned requeue.)
		if perr := r.patchSnapshotReadyLocal(ctx, key, metav1.ConditionFalse, snapshotpkg.ReasonGraphPlanningFailed, err.Error()); perr != nil {
			return ctrl.Result{}, perr
		}
		return ctrl.Result{}, err
	}
	if plan.outcome == namespaceChildrenTerminal {
		// Terminal child-graph failure the content tree cannot express yet (binder still gated pre-Planned):
		// surface it directly on Ready (matches the bespoke reconcileParentOwnedChildGraph terminal path).
		if perr := r.patchSnapshotReadyLocal(ctx, key, metav1.ConditionFalse, plan.reason, plan.message); perr != nil {
			return ctrl.Result{}, perr
		}
		return ctrl.Result{}, nil
	}

	// 3. Create/adopt the planned children and publish their refs. Publication is ADDITIVE (union): the
	//    residual/orphan VolumeSnapshot wave (§5) co-writes childrenSnapshotRefs and must be preserved.
	if err := sdk.EnsureChildren(ctx, adapter, plan.desired, plan.excluded); err != nil {
		return ctrl.Result{}, err
	}
	// A weight layer is still pending (or a mapped source kind is RBAC-forbidden): the graph is not fully
	// planned. Requeue on the child-graph poll cadence (child watches are the primary wake-up). Unbounded
	// by design — a child may stay pending for hours; never a deadline.
	if plan.outcome != namespaceChildrenAllPlanned {
		logger.V(1).Info("namespace child graph not fully planned; requeue", "reason", plan.reason, "message", plan.message)
		return ctrl.Result{RequeueAfter: snapshotChildGraphPollInterval}, nil
	}

	// 4. Residual/orphan PVC wave (root-owned), CONTENT-FREE and BEFORE barrier 1 ("late Planned"): create
	//    the CSI VolumeSnapshots for uncovered PVCs and declare them as REGULAR domain children via the SDK
	//    EnsureChildren (content-single-writer design §11.6). Each orphan VolumeSnapshot is a standard
	//    domain snapshot now (adopted + planned by the storage-foundation VolumeSnapshot domain controller,
	//    §11.2/§11.3): its content shell is created + bound by the generic binder and ALL its content status
	//    (data, manifestCheckpointName, childrenSnapshotContentRefs, Ready) is projected by the aggregator —
	//    the namespace domain writes NO SnapshotContent (INV-CONTENT-WRITER-1 STRICT). Running the
	//    enumeration before MarkPlanned means the FULL child set (domain children + orphan VolumeSnapshots)
	//    is present when the binder freezes the root SnapshotContent, so no orphan child is missed.
	if res, err := r.ensureOrphanVolumeSnapshotsPrePlanned(ctx, nsSnap, adapter, sdk, plan.excluded); err != nil {
		return ctrl.Result{}, err
	} else if res.Requeue || res.RequeueAfter > 0 {
		return res, nil
	}
	// Re-read so the orphan leaves just published on childrenSnapshotRefs are observed downstream.
	if err := r.snapshotReader().Get(ctx, key, nsSnap); err != nil {
		return ctrl.Result{}, err
	}

	// 5. Barrier 1 (phase=Planned): unblocks the generic binder to create + bind the root SnapshotContent
	//    with the full, frozen child set.
	if err := sdk.MarkPlanned(ctx, adapter); err != nil {
		return ctrl.Result{}, err
	}

	// 6. The binder owns the SnapshotContent now: wait for it to create + bind before the linking/manifest
	//    legs, which read the bound content.
	if nsSnap.Status.BoundSnapshotContentName == "" {
		return ctrl.Result{RequeueAfter: 500 * time.Millisecond}, nil
	}
	content := &storagev1alpha1.SnapshotContent{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: nsSnap.Status.BoundSnapshotContentName}, content); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: 500 * time.Millisecond}, nil
		}
		return ctrl.Result{}, err
	}

	// 7. Maintain the RBAC latch (commonController.manifestCaptured mirror of content.subtreeManifestsPersisted).
	//    The per-namespace capture-RBAC hook (040) reads it to drop the transient wide-read RoleBinding.
	if err := r.stampRootManifestCaptured(ctx, key, content.Status.SubtreeManifestsPersisted); err != nil {
		return ctrl.Result{}, err
	}

	// Refresh the root so the commonController latch (stamp/binder) and any concurrent SDK status writes are
	// observed by the manifest-leg gate and the barrier-2 outcome switch below. The orphan VolumeSnapshots
	// are ordinary domain children now: the binder creates + binds their content and the aggregator projects
	// their status (data from the bound VSC, manifestCheckpointName from the VS domain's MCR, Ready mirror),
	// and their content edges are linked into the root's childrenSnapshotContentRefs by the aggregator — no
	// snapshot-side orphan content-materialization step remains (content-single-writer design §11.6).
	if err := r.snapshotReader().Get(ctx, key, nsSnap); err != nil {
		return ctrl.Result{}, err
	}

	// 8. Manifest-exclude leg: base namespace allowlist minus the subtree already captured by descendants,
	//    published via the SDK MCR. The binder chases the published manifestCaptureRequestName -> MCP.
	if res, err := r.reconcileNamespaceManifestLeg(ctx, nsSnap, content, adapter, sdk); err != nil {
		return ctrl.Result{}, err
	} else if res.Requeue || res.RequeueAfter > 0 {
		return res, nil
	}

	// 9. Barrier 2: the manifest leg is captured. Confirm consistency (phase=Finished) or surface a
	//    terminal capture failure the core latches produced (mirrors the demo VM aggregator switch).
	switch outcome := snapshotsdk.CoreCaptureOutcome(adapter); outcome.Outcome {
	case snapshotsdk.CaptureOutcomeFailed:
		return ctrl.Result{}, sdk.Reject(ctx, adapter, snapshotsdk.FailSpec{
			Reason:  snapshotsdk.Reason(outcome.Reason),
			Message: outcome.Message,
		})
	case snapshotsdk.CaptureOutcomeCaptured:
		return ctrl.Result{}, sdk.ConfirmConsistent(ctx, adapter)
	default:
		return ctrl.Result{RequeueAfter: 500 * time.Millisecond}, nil
	}
}

// ensureOrphanVolumeSnapshotsPrePlanned runs the root residual/orphan PVC volume wave BEFORE barrier 1
// (MarkPlanned), CONTENT-FREE of the root SnapshotContent (which does not exist yet). It creates the CSI
// VolumeSnapshots for uncovered PVCs and declares them as REGULAR domain children (via the SDK
// EnsureChildren), so the full child set is enumerated before the binder creates + freezes the root
// content ("late Planned"). The orphan VolumeSnapshot is a standard domain snapshot from here on: it is
// adopted + planned by the storage-foundation VolumeSnapshot domain controller, its content is created +
// bound by the generic binder, and its content status is projected by the aggregator (content-single-
// writer design §11.6) — the namespace domain writes no SnapshotContent.
//
// The wave is gated on all declared domain children being Ready so the subtree-covered PVC UID set (read
// from each descendant's bound content) is complete — a PVC a domain child covers is never momentarily
// treated as orphan. While the gate is closed, or while coverage is transiently unavailable (a child not
// created/bound yet), it requeues on the child-graph poll cadence. A duplicate covered PVC UID is a
// terminal invalid-plan failure; with no bound root content yet to carry it, it is surfaced on the
// Snapshot's own Ready.
func (r *SnapshotReconciler) ensureOrphanVolumeSnapshotsPrePlanned(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	adapter NamespaceSnapshotAdapter,
	sdk snapshotsdk.CaptureSDK,
	excluded []storagev1alpha1.ExcludedObjectRef,
) (ctrl.Result, error) {
	if !volumecaptureuc.IsResidualRootPVCCaptureScope(nsSnap, nil) {
		return ctrl.Result{}, nil
	}
	// The gate exists to guarantee the subtree-covered PVC UID set is COMPLETE before residual enumeration,
	// so a PVC a domain child subtree covers is never momentarily mis-classified as orphan. That set is
	// derived only from the COVERAGE-PROVIDING domain children — NOT from the orphan wave's own CSI
	// VolumeSnapshot output (each orphan VS covers only its own already-residual PVC and is re-adopted
	// idempotently). Gating on the orphan VS children too would (a) needlessly serialize MarkPlanned behind
	// every orphan becoming fully Ready and (b) let a single stuck orphan VS wedge the whole root capture
	// pre-Planned. So exclude CSI VolumeSnapshot refs from the readiness gate (they are still enumerated into
	// coverage by CollectSubtreeCoveredPVCUIDs, which self-heals via re-adoption).
	coverageChildren := nonOrphanCSIVolumeSnapshotChildRefs(nsSnap.Status.ChildrenSnapshotRefs)
	ready, pending, err := r.allDeclaredDomainChildSnapshotsReady(ctx, nsSnap.Namespace, coverageChildren)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ready {
		log.FromContext(ctx).V(1).Info("deferring orphan PVC volume wave until domain children are Ready", "pending", summarizePendingChildren(pending))
		return ctrl.Result{RequeueAfter: snapshotChildGraphPollInterval}, nil
	}
	dataBearing, dbErr := r.dataBearingKindFunc()
	if dbErr != nil {
		// Registry not built yet: requeue (fail-closed, never enumerate residuals with an empty coverage set).
		return ctrl.Result{RequeueAfter: snapshotChildGraphPollInterval}, nil
	}
	targets, err := volumecaptureuc.ListOwnedPVCTargetsForLogicalContent(ctx, r.Client, nsSnap, nil, dataBearing)
	if err != nil {
		key := types.NamespacedName{Namespace: nsSnap.Namespace, Name: nsSnap.Name}
		if stderrors.Is(err, volumecaptureuc.ErrDuplicateCoveredPVCUID) {
			return ctrl.Result{}, r.patchSnapshotReadyLocal(ctx, key, metav1.ConditionFalse, "DuplicateCoveredPVCUID", err.Error())
		}
		// Transient (a child not created/bound yet, PVC list): requeue on the child-graph poll cadence.
		return ctrl.Result{RequeueAfter: snapshotChildGraphPollInterval}, nil
	}
	// ensureOrphanPVCVolumeSnapshots creates the CSI VolumeSnapshots and declares them as regular domain
	// children (EnsureChildren), routing any terminal VolumeSnapshotClass failure onto the Snapshot's own
	// Ready (failOrphanCaptureTerminal) — pre-Planned there is no bound root content to carry it.
	if err := r.ensureOrphanPVCVolumeSnapshots(ctx, nsSnap, adapter, sdk, excluded, targets); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// nonOrphanCSIVolumeSnapshotChildRefs drops the root's own orphan-wave output (CSI VolumeSnapshot children)
// from a child-ref slice, leaving only the coverage-providing domain children. Used to gate the residual
// wave on coverage completeness WITHOUT waiting on (or wedging behind) the orphan VolumeSnapshots the wave
// itself produces (content-single-writer design §11.6; see ensureOrphanVolumeSnapshotsPrePlanned).
func nonOrphanCSIVolumeSnapshotChildRefs(refs []storagev1alpha1.SnapshotChildRef) []storagev1alpha1.SnapshotChildRef {
	out := make([]storagev1alpha1.SnapshotChildRef, 0, len(refs))
	for _, ref := range refs {
		if ref.APIVersion == snapshotpkg.CSISnapshotAPIVersion && ref.Kind == snapshotpkg.KindVolumeSnapshot {
			continue
		}
		out = append(out, ref)
	}
	return out
}

// reconcileNamespaceManifestLeg ensures the root namespace manifest MCR via the SDK, using the proven
// base-minus-subtree exclude computation (BuildRootNamespaceManifestCaptureTargets, which also enforces
// the subtree-persisted wave barrier). It returns a non-requeuing result ONLY once the manifest leg is
// captured (commonController.manifestCaptured latched by the binder), so the caller proceeds to barrier 2.
func (r *SnapshotReconciler) reconcileNamespaceManifestLeg(
	ctx context.Context,
	nsSnap *storagev1alpha1.Snapshot,
	content *storagev1alpha1.SnapshotContent,
	adapter NamespaceSnapshotAdapter,
	sdk snapshotsdk.CaptureSDK,
) (ctrl.Result, error) {
	// Manifest leg already captured: nothing to plan (skip the costly namespace list). The SDK's
	// EnsureManifestCapture is itself suppressed by the same latch, but short-circuiting here avoids the
	// full-namespace discovery listing on every steady-state reconcile.
	if manifestLegCaptured(nsSnap) {
		return ctrl.Result{}, nil
	}
	if r.Dynamic == nil || r.Discovery == nil {
		return ctrl.Result{}, fmt.Errorf("snapshot reconciler: Dynamic/Discovery client is nil")
	}
	snapshotKinds, skErr := r.buildSnapshotMachineryGVKs()
	if skErr != nil {
		// Fail-closed: the live snapshot-kind registry (mechanism-1 dedup) is not built yet. Planning now
		// would risk capturing our own snapshot machinery, so requeue without building targets.
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}
	// RBAC gate before the (single) full namespace list: the per-namespace capture RoleBinding propagates
	// asynchronously, so a SelfSubjectAccessReview (same authorizer as the list) confirms readability first.
	if allowed, sarErr := r.namespaceCaptureRBACReady(ctx, nsSnap.Namespace); sarErr != nil {
		return ctrl.Result{}, sarErr
	} else if !allowed {
		return ctrl.Result{RequeueAfter: 500 * time.Millisecond}, nil
	}
	dataBearing, dbErr := r.dataBearingKindFunc()
	if dbErr != nil {
		// Registry not built yet (same fail-closed contract as buildSnapshotMachineryGVKs above): requeue.
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}
	targets, unreadable, err := usecase.BuildRootNamespaceManifestCaptureTargets(ctx, r.Archive, r.Dynamic, r.Discovery, r.Client, nsSnap, content.Name, snapshotKinds, dataBearing)
	if err != nil {
		// Transient subtree/child-graph state (children still binding, descendant MCPs not yet Ready,
		// registry not built): requeue like ChildrenPending, do NOT fail capture.
		if stderrors.Is(err, usecase.ErrSubtreeManifestCapturePending) ||
			stderrors.Is(err, volumecaptureuc.ErrSubtreeDataRefsPending) ||
			stderrors.Is(err, usecase.ErrRunGraphChildNotBound) ||
			stderrors.Is(err, usecase.ErrRunGraphChildSnapshotNotFound) ||
			stderrors.Is(err, usecase.ErrRunGraphChildNotReachable) ||
			stderrors.Is(err, snapshotgraphregistry.ErrGraphRegistryNotReady) ||
			isTransientCaptureTargetError(err) {
			return ctrl.Result{RequeueAfter: 500 * time.Millisecond}, nil
		}
		// Terminal capture failures. Surfaced via the same failCapture bridge the bespoke path used (the
		// root content has no manifestCheckpointName yet, so it cannot express the failure itself).
		if stderrors.Is(err, usecase.ErrSubtreeManifestCaptureFailed) {
			return r.failCapture(ctx, nsSnap, content, "SubtreeManifestFailed", err.Error())
		}
		if stderrors.Is(err, volumecaptureuc.ErrDuplicateCoveredPVCUID) {
			return r.failCapture(ctx, nsSnap, content, "DuplicateCoveredPVCUID", err.Error())
		}
		return r.failCapture(ctx, nsSnap, content, "ListFailed", fmt.Sprintf("build capture targets: %v", err))
	}
	if len(unreadable) > 0 {
		// Incomplete plan (Forbidden/partial discovery): fail-closed — do NOT build a partial MCR. The
		// binder mirrors the bound content's (still-pending) Ready; requeue until the types become readable.
		return ctrl.Result{RequeueAfter: 500 * time.Millisecond}, nil
	}
	if err := sdk.EnsureManifestCapture(ctx, adapter, snapshotsdk.ManifestCaptureSpec{Targets: namespaceSDKManifestTargets(targets)}); err != nil {
		return ctrl.Result{}, err
	}
	// MCR published: the binder chases manifestCaptureRequestName -> MCP -> content ManifestsReady and
	// mirrors Ready + latches manifestCaptured. Requeue until that latch flips (which short-circuits above).
	return ctrl.Result{RequeueAfter: 500 * time.Millisecond}, nil
}

// manifestLegCaptured reports whether the root's core-owned manifest leg latch
// (captureState.commonController.manifestCaptured) is true.
func manifestLegCaptured(nsSnap *storagev1alpha1.Snapshot) bool {
	cs := nsSnap.Status.CaptureState
	return cs != nil && cs.CommonController != nil &&
		cs.CommonController.ManifestCaptured != nil && *cs.CommonController.ManifestCaptured
}

// namespaceSDKManifestTargets converts the in-reconciler manifest targets (namespacemanifest.ManifestTarget)
// to the SDK's base target set (snapshotsdk.ManifestTarget == ssv1alpha1.ManifestTarget). Namespace is
// implicit (the MCR's own namespace), so only apiVersion/kind/name are carried.
func namespaceSDKManifestTargets(targets []namespacemanifest.ManifestTarget) []snapshotsdk.ManifestTarget {
	out := make([]snapshotsdk.ManifestTarget, 0, len(targets))
	for _, t := range targets {
		out = append(out, snapshotsdk.ManifestTarget{
			APIVersion: t.APIVersion,
			Kind:       t.Kind,
			Name:       t.Name,
		})
	}
	return out
}
