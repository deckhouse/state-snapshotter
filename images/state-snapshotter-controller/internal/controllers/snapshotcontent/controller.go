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
	stderrors "errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/common"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/config"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
)

// SnapshotContentController reconciles generic XxxxSnapshotContent resources.
//
// Architectural boundary: SnapshotContentController is a result aggregator and
// lifecycle controller, not a domain planner/executor. It does not decide what
// must be captured and does not create domain execution requests such as MCR,
// VCR, DataExport, or VolumeSnapshot requests. Snapshot/domain controllers own
// planning and request creation because only they know the domain model.
//
// This controller manages the lifecycle and aggregate result of SnapshotContent:
// - Manages finalizers (protection from manual deletion)
// - Reads existing manifest/data result objects and aggregates Ready
// - Publishes SnapshotContent.status fields (MCP/data refs and child content refs)
// - Handles deletion (cascade finalizers removal)
// - Does NOT create SnapshotContent (that's GenericSnapshotBinderController's responsibility)
//
// Architecture:
// - Uses dynamic client for low-level get/list operations
// - Converts to typed SnapshotContentLike interface for business logic
// - Centralized conditions management through pkg/snapshot/conditions
// - Manages finalizers through pkg/snapshot/finalizers (to be implemented)
type SnapshotContentController struct {
	client.Client
	APIReader      client.Reader // Required: for reading resources directly from API server
	Scheme         *runtime.Scheme
	Config         *config.Options
	RESTMapper     meta.RESTMapper
	clusterGVKs    []schema.GroupVersionKind
	namespacedGVKs []schema.GroupVersionKind

	// GVKRegistry provides centralized GVK resolution
	GVKRegistry *snapshot.GVKRegistry

	// SnapshotContentGVKs is a list of GVKs that this controller should watch
	// This allows domain modules to register their snapshot content types
	SnapshotContentGVKs []schema.GroupVersionKind

	watchMu                sync.RWMutex
	activeContentWatchSet  map[string]struct{} // SnapshotContent GVK String()
	activeSnapshotWatchSet map[string]struct{} // Snapshot GVK String() -> status watch registered with manager
}

const defaultSnapshotContentRequeueAfter = 500 * time.Millisecond

// snapshotContentControllerOptions tunes every controller instance that drives this reconciler (the
// per-GVK content controllers and the Snapshot-status wake-up controllers all share r). With the default
// MaxConcurrentReconciles=1 a single worker per instance starved individual content nodes during the
// capture wave: Ready now gates on the whole-subtree ManifestsArchived latch, so every not-ready node
// self-requeues every defaultSnapshotContentRequeueAfter, and one node could wait tens of seconds before
// the lone worker reached it — which in turn stalls the MCR ownerRef handoff that node performs
// (ensureManifestCheckpointOwnedByContent runs inside the same reconcile). Parallelize across distinct
// objects (controller-runtime still serializes reconciles of the same object key, so per-node invariants
// hold) and bound the error backoff like the Snapshot/manifestcapture controllers (200ms floor -> 10s
// ceiling) so transient failures re-run promptly instead of backing off to the ~16min default.
func snapshotContentControllerOptions() controller.Options {
	return controller.Options{
		MaxConcurrentReconciles: 8,
		RateLimiter:             workqueue.NewTypedItemExponentialFailureRateLimiter[ctrl.Request](200*time.Millisecond, 10*time.Second),
	}
}

// NewSnapshotContentController creates a new SnapshotContentController with validated dependencies
func NewSnapshotContentController(
	client client.Client,
	apiReader client.Reader,
	scheme *runtime.Scheme,
	restMapper meta.RESTMapper,
	cfg *config.Options,
	snapshotContentGVKs []schema.GroupVersionKind,
) (*SnapshotContentController, error) {
	if client == nil {
		return nil, fmt.Errorf("Client must not be nil")
	}
	if apiReader == nil {
		return nil, fmt.Errorf("APIReader must not be nil: controllers require APIReader for direct API reads")
	}
	if scheme == nil {
		return nil, fmt.Errorf("Scheme must not be nil")
	}
	if restMapper == nil {
		return nil, fmt.Errorf("RESTMapper must not be nil")
	}
	if cfg == nil {
		return nil, fmt.Errorf("Config must not be nil")
	}

	var clusterGVKs []schema.GroupVersionKind
	var namespacedGVKs []schema.GroupVersionKind
	for _, gvk := range snapshotContentGVKs {
		mapping, mapErr := restMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if mapErr != nil {
			return nil, fmt.Errorf("failed to resolve GVK mapping for %s: %w", gvk.String(), mapErr)
		}
		if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			namespacedGVKs = append(namespacedGVKs, gvk)
		} else {
			clusterGVKs = append(clusterGVKs, gvk)
		}
	}

	// Initialize GVK Registry and register known GVKs
	registry := snapshot.NewGVKRegistry()
	for _, gvk := range snapshotContentGVKs {
		// Extract Snapshot Kind from Content Kind (remove "Content" suffix)
		snapshotKind := strings.TrimSuffix(gvk.Kind, "Content")
		if snapshotKind == gvk.Kind {
			// Content Kind doesn't end with "Content" - skip or handle differently
			continue
		}
		// Register Content GVK
		if err := registry.RegisterSnapshotContentGVK(gvk.Kind, gvk.GroupVersion().String()); err != nil {
			return nil, fmt.Errorf("failed to register SnapshotContent GVK %s: %w", gvk.String(), err)
		}
		// Register Snapshot GVK (derived from Content Kind)
		if err := registry.RegisterSnapshotGVK(snapshotKind, gvk.GroupVersion().String()); err != nil {
			return nil, fmt.Errorf("failed to register Snapshot GVK %s: %w", snapshotKind, err)
		}
	}

	return &SnapshotContentController{
		Client:                 client,
		APIReader:              apiReader,
		Scheme:                 scheme,
		RESTMapper:             restMapper,
		clusterGVKs:            clusterGVKs,
		namespacedGVKs:         namespacedGVKs,
		Config:                 cfg,
		GVKRegistry:            registry,
		SnapshotContentGVKs:    snapshotContentGVKs,
		activeContentWatchSet:  make(map[string]struct{}),
		activeSnapshotWatchSet: make(map[string]struct{}),
	}, nil
}

// Reconcile processes a SnapshotContent resource
//
// Step 1 (Skeleton): Basic structure - no finalizers, no deletion, no consistency checks
func (r *SnapshotContentController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("snapshotcontent", req.Name, "reqNamespace", req.Namespace)
	if req.Name == "" {
		logger.V(1).Info("Skipping reconcile: empty name")
		return ctrl.Result{}, nil
	}

	// Get the unstructured object
	// ARCHITECTURAL NOTE: SnapshotContentController is instantiated per-GVK
	// and registered with exact GVK in SetupWithManager.
	// Each controller instance handles only one specific GVK (e.g., VirtualMachineSnapshotContent).
	// Get the unstructured object
	// We need to try each registered GVK to find the correct one
	// SnapshotContent is cluster-scoped; use Name only (no namespace).
	contentKey := client.ObjectKey{Name: req.Name, Namespace: req.Namespace}
	logger.Info("Reconciling SnapshotContent", "contentKeyNamespace", contentKey.Namespace)

	obj := &unstructured.Unstructured{}
	var found bool
	var err error
	gvksToCheck := r.clusterGVKsSnapshot()
	if contentKey.Namespace != "" {
		gvksToCheck = r.namespacedGVKsSnapshot()
	}
	for _, gvk := range gvksToCheck {
		obj.SetGroupVersionKind(gvk)
		// Read the own object from the cache: gvksToCheck only holds content GVKs that already have a
		// started For-informer (AddWatchForContent registers the GVK and its watch transactionally), so
		// this never forces a new informer and a non-match returns a clean NotFound, same as the uncached
		// reader did. Spec is immutable and conditions are written under a changed-gate, so the cached
		// (eventually consistent) read cannot add reconcile churn.
		err = r.Client.Get(ctx, contentKey, obj)
		if err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			logger.Error(err, "failed to get SnapshotContent")
			return ctrl.Result{}, err
		}
		found = true
		break
	}

	if !found {
		// SnapshotContent not found in any registered GVK - object was deleted or doesn't exist
		// This is normal after finalizer removal when GC deletes the object
		logger.V(1).Info("SnapshotContent not found in any registered GVK, skipping")
		return ctrl.Result{}, nil
	}

	// If we reach here, found == true, which means err == nil (we successfully got the object)
	// The duplicate error check below is unreachable code, but kept for safety
	if err != nil {
		// This should never happen if found == true, but handle it gracefully
		if errors.IsNotFound(err) {
			logger.V(1).Info("SnapshotContent not found (race condition: deleted between Get calls), skipping")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "failed to get SnapshotContent")
		return ctrl.Result{}, err
	}

	// Convert to typed interface
	contentLike, err := snapshot.ExtractSnapshotContentLike(obj)
	if err != nil {
		logger.Error(err, "failed to extract SnapshotContentLike interface")
		return ctrl.Result{}, err
	}

	// Step 1: Ensure finalizer (manual deletion protection)
	if obj.GetDeletionTimestamp().IsZero() {
		if parentDeleted, _, _ := unstructured.NestedBool(obj.Object, "status", "parentDeleted"); parentDeleted {
			// Parent Snapshot is gone; don't re-add finalizer.
			return ctrl.Result{}, nil
		}
		if snapshot.AddFinalizer(obj, snapshot.FinalizerParentProtect) {
			logger.Info("Added finalizer to SnapshotContent", "finalizer", snapshot.FinalizerParentProtect)
			if err := r.Update(ctx, obj); err != nil {
				logger.Error(err, "Failed to add finalizer")
				return ctrl.Result{}, err
			}
			return ctrl.Result{Requeue: true}, nil
		}
	} else {
		// Object is being deleted - handle deletion (Phase 2: Cascade)
		// Invariant Phase 2: SnapshotContent с DeletionTimestamp →
		// сначала cascade finalizers → потом GC через ownerRef

		// Step 2.1: Cascade remove finalizers from children
		// This unlocks GC for children, but does NOT initiate Delete(child-content)
		// GC will handle deletion through ownerRef
		if err := r.cascadeRemoveFinalizersFromChildren(ctx, contentLike, obj); err != nil {
			// A child whose physical reclaim failed keeps its parent-protect finalizer; requeue and KEEP
			// this parent's finalizer too, so the parent is not GC'd (and the subtree not orphaned) until
			// every child's data artifact is reclaimed (C5 teardown: no orphaned physical CSI snapshots).
			logger.Error(err, "Cascade child reclaim/finalizer removal incomplete; keeping finalizer and requeueing")
			return ctrl.Result{}, err
		}

		// Step 2.1.1: Remove finalizers from linked artifacts (MCP/VSC)
		if mcpName := contentLike.GetStatusManifestCheckpointName(); mcpName != "" {
			if err := r.removeArtifactFinalizer(ctx, "ManifestCheckpoint", mcpName, "state-snapshotter.deckhouse.io/v1alpha1"); err != nil {
				logger.Error(err, "Failed to remove ManifestCheckpoint finalizer", "mcp", mcpName)
			}
		}
		// Physical data reclaim (C5, unified import + capture teardown): the VSC was pinned to Retain for
		// its whole life; now that the owning SnapshotContent is going away, flip it Retain->Delete and
		// delete it so the CSI external-snapshotter reclaims the physical snapshot instead of orphaning it.
		// This is a HARD GATE: an import VSC carries a second (DataImport-keeper) ownerRef, so removing the
		// parent-protect/artifact-protect finalizers does NOT GC it — the explicit reclaim below is the only
		// deterministic path. On any reclaim error, requeue and KEEP the finalizers so the physical snapshot
		// is never orphaned; finalizer removal only proceeds once reclaim has succeeded.
		if err := r.reclaimDataArtifactsFromContentObj(ctx, obj); err != nil {
			logger.Error(err, "Failed to reclaim data artifacts on SnapshotContent teardown; keeping finalizer and requeueing")
			return ctrl.Result{}, err
		}
		for _, binding := range contentLike.GetStatusDataRefs() {
			if binding.Artifact.Kind != "VolumeSnapshotContent" || binding.Artifact.Name == "" {
				continue
			}
			if err := r.removeArtifactFinalizer(ctx, "VolumeSnapshotContent", binding.Artifact.Name, "snapshot.storage.k8s.io/v1"); err != nil {
				logger.Error(err, "Failed to remove VolumeSnapshotContent finalizer", "vsc", binding.Artifact.Name)
			}
		}

		// Step 2.2: Remove finalizer from this SnapshotContent
		// This unlocks GC for this object and its artifacts
		if snapshot.RemoveFinalizer(obj, snapshot.FinalizerParentProtect) {
			logger.Info(
				"Removing finalizer from SnapshotContent, GC will handle deletion",
				"finalizer", snapshot.FinalizerParentProtect,
				"snapshotContent", req.Name,
			)
			if err := r.Update(ctx, obj); err != nil {
				logger.Error(err, "Failed to remove finalizer")
				return ctrl.Result{}, err
			}
			// After finalizer is removed, GC will handle deletion through ownerRef
			return ctrl.Result{}, nil
		}

		// Finalizer already removed - GC is handling deletion
		logger.V(1).Info("Finalizer already removed, GC is handling deletion")
		return ctrl.Result{}, nil
	}

	// Step 3: Content status aggregation and Ready condition.
	// The common state-snapshotter.deckhouse.io/SnapshotContent is the ONLY content carrier in the unified
	// runtime (every snapshot kind maps to CommonSnapshotContentGVK), and it owns the aggregate
	// condition model: ManifestsReady + VolumeReady + ChildrenReady + derived Ready
	// (INV-COND2). No non-common SnapshotContent GVK is registered, so there is no other writer.
	if !isCommonSnapshotContentGVK(obj.GroupVersionKind()) {
		logger.V(1).Info("non-common SnapshotContent GVK is not managed by the unified runtime; skipping",
			"gvk", obj.GroupVersionKind().String())
		return ctrl.Result{}, nil
	}

	// Resolve the owning snapshot ONCE per pass and share it across the single-writer projections below
	// (child edges + manifest pointer + data leg). All read the same owner status legs (childrenSnapshotRefs
	// and captureState...manifestCaptureRequestName / boundVolumeSnapshotContentName), so a single
	// APIReader.Get keeps the aggregator's per-pass owner read at the Block 1 level. A second, redundant Get
	// (one per projection) perturbed the Block 0 eager-shell ObjectKeeper create race enough to wedge the
	// orphan wave. See WORKLOG w8-block2.
	//
	// Orphan/standalone VolumeSnapshot children are ordinary domain contents now (content-single-writer
	// design §11.6): their owner (the VolumeSnapshot) carries captureState + boundVolumeSnapshotContentName,
	// so they drive the same projections as every other content — there is no visibility-leaf owner-resolve
	// carve-out.
	owner, ownerNamespace, ownerFound, ownerErr := r.ownerSnapshot(ctx, obj)
	if ownerErr != nil {
		logger.Error(ownerErr, "Failed to resolve owning snapshot")
		return ctrl.Result{}, ownerErr
	}

	// Single-writer child edges (INV-CONTENT-CHILDREN-1, content-single-writer design §3.1/§3.2): the
	// aggregator is the ONLY writer of status.childrenSnapshotContentRefs. Project them from the owning
	// snapshot's childrenSnapshotRefs before aggregating status. The edge write is a separate
	// optimistic-locked status patch (not folded into the condition MergeFrom below); a freshly written
	// edge set is observed on the next pass, driven by the child-content watch and the 500 ms self-requeue
	// while !ready.
	edgesRequeue, err := r.reconcileChildContentEdges(ctx, obj, owner, ownerNamespace, ownerFound)
	if err != nil {
		logger.Error(err, "Failed to project child content edges")
		return ctrl.Result{}, err
	}

	// Single-writer manifest pointer (INV-CONTENT-WRITER-1, content-single-writer design §3.1/§3.2): the
	// aggregator is the ONLY writer of status.manifestCheckpointName. Project it from the owning snapshot's
	// ManifestCaptureRequest BEFORE fillOwnLegs (in reconcileCommonSnapshotContentStatus below) reads it.
	// Like the child edges above, this is a separate optimistic-locked status patch observed on the next
	// pass; the 500 ms self-requeue while !ready drives convergence.
	mcpRequeue, err := r.reconcileManifestCheckpointNameProjection(ctx, obj, owner, ownerNamespace, ownerFound)
	if err != nil {
		logger.Error(err, "Failed to project manifest checkpoint name")
		return ctrl.Result{}, err
	}

	// Single-writer data leg (INV-CONTENT-WRITER-1, content-single-writer design §4 Slice 3 / §11.4): the
	// aggregator is the ONLY writer of status.data for domain owners. Project it from the owning snapshot's
	// VolumeCaptureRequest (VCR domains) or its bound VolumeSnapshotContent (native-CSI VolumeSnapshot)
	// BEFORE fillOwnLegs (in reconcileCommonSnapshotContentStatus below) reads status.dataRefs. Like the
	// child edges + manifest pointer above, this is a separate optimistic-locked status patch observed on
	// the next pass; the 500 ms self-requeue while !ready drives convergence.
	dataRequeue, err := r.reconcileDataLegProjection(ctx, obj, owner, ownerNamespace, ownerFound)
	if err != nil {
		logger.Error(err, "Failed to project data leg")
		return ctrl.Result{}, err
	}

	// Pass dataRequeue as dataLegPending so the aggregation does NOT compute a premature Ready=True on a
	// stale-empty status.data for a content whose data leg is still converging (the aggregator published
	// it via a separate patch this pass, or the VCR/VSC is not ready yet). See
	// reconcileCommonSnapshotContentStatus.
	ready, err := r.reconcileCommonSnapshotContentStatus(ctx, obj, dataRequeue)
	if err != nil {
		logger.Error(err, "Failed to reconcile common SnapshotContent status")
		return ctrl.Result{}, err
	}
	// w7-main-split: mirror the just-computed content.Ready onto the owning Snapshot in the SAME pass
	// (owner resolved from spec.snapshotRef; post-bind writer switch on status.boundSnapshotContentName).
	// Runs regardless of `ready` so a content.Ready=False (e.g. ManifestCapturePending) is reflected on the
	// Snapshot too. Removing the cross-controller hop is what closes the staleness window where the binder
	// re-derived a stale Ready. On a transient API error, requeue and retry.
	if err := r.mirrorReadyToOwnerSnapshot(ctx, obj); err != nil {
		logger.Error(err, "Failed to mirror content Ready onto owner Snapshot")
		return ctrl.Result{}, err
	}
	// Keep actively requeuing until Ready is True. Ready includes the ManifestsArchived subtree latch as a
	// (monotonic) gate, so while any descendant is still archiving this node's Ready stays False — this
	// self-requeue is what drives the child->parent archive wave to converge via active re-evaluation
	// instead of stalling on a droppable wake-up event (declared-but-unlinked child, or a same-binary
	// artifact event seen before its ownerRef handoff) or the next informer resync (~minutes).
	if !ready || edgesRequeue || mcpRequeue || dataRequeue {
		return ctrl.Result{RequeueAfter: defaultSnapshotContentRequeueAfter}, nil
	}

	logger.Info("SnapshotContent reconciliation completed")
	return ctrl.Result{}, nil
}

func isCommonSnapshotContentGVK(gvk schema.GroupVersionKind) bool {
	return gvk == unifiedbootstrap.CommonSnapshotContentGVK()
}

// commonContentStatusPlan is the SnapshotContent aggregation outcome. It carries the own-node legs
// (ManifestsReady = own MCP; VolumeReady = own data refs; ChildrenReady = child SnapshotContents) and
// the derived Ready. Ready is the single aggregate on SnapshotContent:
// Ready = ManifestsReady && VolumeReady && ChildrenReady (INV-COND2).
type commonContentStatusPlan struct {
	manifestsReady   metav1.ConditionStatus
	manifestsReason  string
	manifestsMessage string
	manifestsFailed  bool // terminal (vs pending) when manifestsReady != True

	volumeReady   metav1.ConditionStatus
	volumeReason  string
	volumeMessage string
	volumeFailed  bool // terminal (vs pending) when volumeReady != True

	childrenReady   metav1.ConditionStatus
	childrenReason  string
	childrenMessage string
	childrenFailed  bool // terminal (vs pending) when childrenReady != True

	readyStatus  metav1.ConditionStatus
	readyReason  string
	readyMessage string

	// subtreeManifestsPersisted is the core-internal monotonic recursive latch AND the lowest-priority
	// Ready leg: it gates the first Ready=True; true once this node's own manifest leg reached readiness
	// AND every declared child content has subtreeManifestsPersisted=true; it never re-opens. It is
	// success-only (no Failed value): a terminal subtree failure surfaces on the Ready reason
	// (manifestsFailed/childrenFailed), not here. Persisted to status.subtreeManifestsPersisted (a bool
	// field), not a condition. subtreeManifestsPersistedMessage carries the pending gate message.
	subtreeManifestsPersisted        bool
	subtreeManifestsPersistedMessage string
}

// reconcileCommonSnapshotContentStatus aggregates and publishes the SnapshotContent conditions and
// reports whether the derived Ready condition is True. Ready includes the ManifestsArchived subtree latch
// as a (monotonic) gate, so while any descendant is still archiving Ready stays False; the caller keeps
// requeuing on !ready, which is what drives the child->parent archive wave to converge via active
// re-evaluation instead of stalling on a droppable wake-up event (e.g. a not-yet-linked declared child, or
// a same-binary artifact event observed before its ownerRef handoff) or the next informer resync.
func (r *SnapshotContentController) reconcileCommonSnapshotContentStatus(ctx context.Context, obj *unstructured.Unstructured, dataLegPending bool) (ready bool, err error) {
	// Self-heal data-artifact ownerRefs from the published truth (status.dataRefs[]) so the
	// ownerRef-based VSC wake-up stays robust. Best-effort: never writes status, never fails
	// reconcile (INV-RECONCILE-TRUTH: correctness comes from revalidation below, watches are
	// only a liveness optimization).
	r.selfHealDataArtifactOwnerRefs(ctx, obj)

	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, obj)
	if err != nil {
		return false, err
	}

	// Data-leg-pending downgrade (content-single-writer §4 Slice 3 / §11.4, INV-CONTENT-WRITER-1).
	// reconcileDataLegProjection is the single writer of status.data and it publishes via a SEPARATE
	// status patch, so on this pass `obj` is stale-empty for a data leg it just published or is still
	// capturing. resolveDataReadiness reads an empty status.dataRefs as volume N/A (VolumeReady=True),
	// which — for a content that HAS an expected data leg — would let Ready escalate to True (and
	// mirrorReadyToOwnerSnapshot propagate it) BEFORE the bound VolumeSnapshotContent's readyToUse is ever
	// validated. `dataLegPending` (from reconcileDataLegProjection: the owner declares a VCR/native-CSI
	// data leg that is not yet durably published+covered) forces the volume leg back to a non-terminal
	// DataCapturePending for this pass and re-derives Ready. It ONLY downgrades a would-be-ready leg — a
	// manifest-only content (no data leg -> dataLegPending=false) keeps VolumeReady=True (N/A), and a leg
	// the aggregation already sees as not-ready is left as computed. The next pass re-reads the fresh
	// status.data and validates readyToUse for real, so this delays the FIRST Ready=True by exactly one
	// pass instead of racing it.
	if dataLegPending && plan.volumeReady == metav1.ConditionTrue {
		plan.volumeReady = metav1.ConditionFalse
		plan.volumeReason = snapshot.ReasonDataCapturePending
		plan.volumeMessage = "waiting for the data leg to be published and the volume snapshot artifact to be ready"
		plan.volumeFailed = false
		deriveReadyStatus(&plan)
	}

	contentLike, err := snapshot.ExtractSnapshotContentLike(obj)
	if err != nil {
		return false, fmt.Errorf("extract common SnapshotContentLike: %w", err)
	}
	statusMap, _ := obj.Object["status"].(map[string]interface{})
	if statusMap == nil {
		statusMap = map[string]interface{}{}
	}

	// base for the condition-only MergeFrom patch below, captured as-read BEFORE any condition mutation.
	// upsertContentCondition writes conditions straight into obj.status.conditions (the SnapshotContentLike
	// wrapper is a view over obj), so capturing base here - rather than just before the write - keeps the
	// MergeFrom diff limited to status.conditions. selfHealDataArtifactOwnerRefs already ran above, so its
	// effects are folded into base and never enter the patch.
	base := obj.DeepCopy()

	gen := obj.GetGeneration()
	desired := []metav1.Condition{
		{Type: snapshot.ConditionManifestsReady, Status: plan.manifestsReady, Reason: plan.manifestsReason, Message: plan.manifestsMessage, ObservedGeneration: gen},
		{Type: snapshot.ConditionVolumeReady, Status: plan.volumeReady, Reason: plan.volumeReason, Message: plan.volumeMessage, ObservedGeneration: gen},
		{Type: snapshot.ConditionChildrenReady, Status: plan.childrenReady, Reason: plan.childrenReason, Message: plan.childrenMessage, ObservedGeneration: gen},
		{Type: snapshot.ConditionReady, Status: plan.readyStatus, Reason: plan.readyReason, Message: plan.readyMessage, ObservedGeneration: gen},
	}

	ready = plan.readyStatus == metav1.ConditionTrue

	changed := false
	for _, d := range desired {
		if upsertContentCondition(contentLike, d) {
			changed = true
		}
	}

	// Persist the core-internal subtreeManifestsPersisted latch (monotonic bool status field, not a
	// condition). Written into obj.status so the MergeFrom diff below includes it alongside conditions.
	curPersisted, _, _ := unstructured.NestedBool(obj.Object, "status", "subtreeManifestsPersisted")
	if plan.subtreeManifestsPersisted != curPersisted {
		statusMap["subtreeManifestsPersisted"] = plan.subtreeManifestsPersisted
		changed = true
	}

	// Publish the durable excludedRefs aggregate (this node's own vetoes UNION every descendant's
	// aggregate). SnapshotContent is the single writer and the cluster-scoped source of truth; the
	// namespaced Snapshot/domain CRs only mirror it. The set is +listType=atomic, so the MergeFrom below
	// replaces it wholesale (matching set semantics).
	newExcluded, err := r.computeExcludedRefsAggregate(ctx, obj)
	if err != nil {
		return false, err
	}
	if !excludedObjectRefsEqualIgnoreOrder(parseExcludedRefs(obj, "status", "excludedRefs"), newExcluded) {
		if len(newExcluded) == 0 {
			delete(statusMap, "excludedRefs")
		} else {
			statusMap["excludedRefs"] = excludedRefsToUnstructured(newExcluded)
		}
		changed = true
	}

	if !changed {
		return ready, nil
	}
	obj.Object["status"] = statusMap
	snapshot.SyncConditionsToUnstructured(obj, contentLike.GetStatusConditions())
	// Condition-only MergeFrom patch (not a full Status().Update). base vs obj differ only in
	// status.conditions, so the JSON merge patch is {"status":{"conditions":[...]}} and leaves sibling
	// status fields written by the snapshot reconciler (data, parentDeleted, ...) untouched.
	// conditions are the aggregator's exclusive domain (INV-COND2, single writer) and reconcile of one
	// object is serialized, so no optimistic lock is needed: MergeFrom sends no resourceVersion, so a
	// concurrent sibling-field write does not turn into a 409 (which the previous Status().Update would
	// convert into an extra requeue). Staleness is still safe because the changed-gate above only fires when
	// the monotonic-cache-derived desired conditions actually differ.
	if err := r.Status().Patch(ctx, obj, client.MergeFrom(base)); err != nil {
		return false, err
	}
	return ready, nil
}

// upsertContentCondition sets desired (type/status/reason/message + observedGeneration) on contentLike
// and reports whether anything changed. SetCondition owns LastTransitionTime idempotency but does not
// set observedGeneration, so gen-gating (INV-COND3) is applied here.
func upsertContentCondition(contentLike snapshot.SnapshotContentLike, desired metav1.Condition) bool {
	cur := snapshot.GetCondition(contentLike, desired.Type)
	if cur != nil && cur.Status == desired.Status && cur.Reason == desired.Reason &&
		cur.Message == desired.Message && cur.ObservedGeneration == desired.ObservedGeneration {
		return false
	}
	snapshot.SetCondition(contentLike, desired.Type, desired.Status, desired.Reason, desired.Message)
	conds := contentLike.GetStatusConditions()
	for i := range conds {
		if conds[i].Type == desired.Type {
			conds[i].ObservedGeneration = desired.ObservedGeneration
		}
	}
	contentLike.SetStatusConditions(conds)
	return true
}

// ReconcileCommonSnapshotContentStatus is the exported aggregation entry (tests/utility). It passes
// dataLegPending=false: callers that do not run the data-leg projection this pass get the plain N/A
// treatment for an empty status.data (the aggregator's own Reconcile threads the real pending signal).
func (r *SnapshotContentController) ReconcileCommonSnapshotContentStatus(ctx context.Context, obj *unstructured.Unstructured) (ready bool, err error) {
	return r.reconcileCommonSnapshotContentStatus(ctx, obj, false)
}

// buildCommonSnapshotContentStatusPlan computes ManifestsReady, VolumeReady, ChildrenReady, the
// ManifestsArchived subtree latch, and the derived Ready. Ready priority (single reason when several
// legs are not satisfied):
//
//	manifestsFailed > volumeFailed > childrenFailed > manifestsPending > volumesPending > childrenPending > archivePending > Completed
//
// Terminal failures win over pending (actionable first); own-node legs win over children, and the
// manifest leg wins over the volume leg at equal severity. ManifestsArchived is the LOWEST-priority gate:
// it blocks the FIRST Ready=True until the whole subtree's manifests are archived. It is monotonic, so it
// never drags Ready back down after the first Ready=True. The former residual/orphan-PVC capture latch
// gate is gone: the orphan wave is now gated through ChildrenReady (ChildrenLinkPending, fail-closed
// against declared-but-unlinked orphan volume child content — see validateCommonContentChildren).
// PlanningReady is NOT part of this formula; it is only a gate/barrier upstream.
func (r *SnapshotContentController) buildCommonSnapshotContentStatusPlan(ctx context.Context, obj *unstructured.Unstructured) (commonContentStatusPlan, error) {
	plan := commonContentStatusPlan{
		manifestsReady:   metav1.ConditionFalse,
		manifestsReason:  snapshot.ReasonManifestCapturePending,
		manifestsMessage: "waiting for manifest capture",
		// Volume leg is not evaluated until the manifest leg is Ready (kept sequencing, v1): Unknown,
		// not False, so it does not look like a volume failure.
		volumeReady:     metav1.ConditionUnknown,
		volumeReason:    snapshot.ReasonManifestCapturePending,
		volumeMessage:   "data leg not evaluated until manifest capture is ready",
		childrenReady:   metav1.ConditionTrue,
		childrenReason:  snapshot.ReasonCompleted,
		childrenMessage: "no child content",
	}

	if err := r.fillOwnLegs(ctx, obj, &plan); err != nil {
		return plan, err
	}

	childrenReady, childReason, childMessage, err := r.validateCommonContentChildren(ctx, obj)
	if err != nil {
		return plan, err
	}
	if !childrenReady {
		plan.childrenReady = metav1.ConditionFalse
		plan.childrenReason = childReason
		plan.childrenMessage = childMessage
		plan.childrenFailed = childReason == snapshot.ReasonChildrenFailed
	} else if childMessage != "" {
		plan.childrenMessage = childMessage
	}

	// subtreeManifestsPersisted gates Ready: a node cannot reach its FIRST Ready=True until its own and all
	// descendant manifests are persisted. The latch is monotonic (computeSubtreeManifestsPersisted holds
	// true once true), so after the first Ready=True the gate stays satisfied and Ready is free to flap on
	// the live legs. Computed before the switch so it can participate as the lowest-priority Ready leg.
	if err := r.computeSubtreeManifestsPersisted(ctx, obj, &plan); err != nil {
		return plan, err
	}

	deriveReadyStatus(&plan)
	return plan, nil
}

// deriveReadyStatus computes the aggregate Ready condition (readyStatus/readyReason/readyMessage) from the
// already-populated legs, applying the single-reason priority order (see buildCommonSnapshotContentStatusPlan
// doc). It is factored out of buildCommonSnapshotContentStatusPlan so a post-build leg adjustment (e.g. the
// data-leg-pending downgrade in reconcileCommonSnapshotContentStatus, content-single-writer §4 Slice 3) can
// re-derive Ready without duplicating the priority ladder.
func deriveReadyStatus(plan *commonContentStatusPlan) {
	switch {
	case plan.manifestsReady != metav1.ConditionTrue && plan.manifestsFailed:
		plan.readyStatus = metav1.ConditionFalse
		plan.readyReason = plan.manifestsReason
		plan.readyMessage = plan.manifestsMessage
	case plan.volumeReady != metav1.ConditionTrue && plan.volumeFailed:
		plan.readyStatus = metav1.ConditionFalse
		plan.readyReason = plan.volumeReason
		plan.readyMessage = plan.volumeMessage
	case plan.childrenReady != metav1.ConditionTrue && plan.childrenFailed:
		plan.readyStatus = metav1.ConditionFalse
		plan.readyReason = plan.childrenReason
		plan.readyMessage = plan.childrenMessage
	case plan.manifestsReady != metav1.ConditionTrue:
		plan.readyStatus = metav1.ConditionFalse
		plan.readyReason = plan.manifestsReason
		plan.readyMessage = plan.manifestsMessage
	case plan.volumeReady != metav1.ConditionTrue:
		plan.readyStatus = metav1.ConditionFalse
		plan.readyReason = plan.volumeReason
		plan.readyMessage = plan.volumeMessage
	case plan.childrenReady != metav1.ConditionTrue:
		plan.readyStatus = metav1.ConditionFalse
		plan.readyReason = plan.childrenReason
		plan.readyMessage = plan.childrenMessage
	case !plan.subtreeManifestsPersisted:
		// Subtree persist gate (lowest priority): blocks the first Ready=True until own + all-descendant
		// manifests are persisted. Reached only when every live leg is already satisfied (the cases above did
		// not fire), so in practice this is the transient fail-closed declared-but-unlinked-child wait. A
		// terminal subtree failure cannot reach this case: it implies a not-Ready linked child, which trips
		// ChildrenFailed/ChildrenPending above, and the underlying manifest failure independently surfaces as
		// ManifestCheckpointFailed/ChildrenFailed.
		plan.readyStatus = metav1.ConditionFalse
		plan.readyReason = snapshot.ReasonSubtreeManifestCapturePending
		plan.readyMessage = plan.subtreeManifestsPersistedMessage
	default:
		plan.readyStatus = metav1.ConditionTrue
		plan.readyReason = snapshot.ReasonCompleted
		plan.readyMessage = "manifests (archived), data, and child content are ready"
	}
}

// computeSubtreeManifestsPersisted fills the subtreeManifestsPersisted latch on plan. It is a lifelong
// latch (never re-opens) that records the irreversible fact that this node and its whole subtree
// persisted their manifests. It also acts as the lowest-priority Ready leg (see
// buildCommonSnapshotContentStatusPlan): because it is monotonic, it gates the FIRST Ready=True but
// never drags Ready back down afterwards.
//
//   - If the persisted latch (status.subtreeManifestsPersisted) is already true, it is held true
//     (immune to later ManifestsReady/Ready degradation or to a child disappearing). Snapshot.spec is
//     immutable, so there is no recapture and no generation bookkeeping is needed.
//   - Otherwise it is derived from the current subtree state: own ManifestsReady=True AND all declared
//     children persisted -> true; else false with a pending message. It is success-only: a terminal
//     subtree failure surfaces on the Ready reason (manifestsFailed/childrenFailed), never here.
func (r *SnapshotContentController) computeSubtreeManifestsPersisted(ctx context.Context, obj *unstructured.Unstructured, plan *commonContentStatusPlan) error {
	if cur, _, _ := unstructured.NestedBool(obj.Object, "status", "subtreeManifestsPersisted"); cur {
		plan.subtreeManifestsPersisted = true
		return nil
	}

	childrenPersisted, childMessage, err := r.aggregateChildrenSubtreeManifestsPersisted(ctx, obj)
	if err != nil {
		return err
	}
	if plan.manifestsReady == metav1.ConditionTrue && childrenPersisted {
		plan.subtreeManifestsPersisted = true
		return nil
	}
	plan.subtreeManifestsPersisted = false
	if childMessage != "" {
		plan.subtreeManifestsPersistedMessage = childMessage
	} else {
		plan.subtreeManifestsPersistedMessage = "manifests are being captured: " + plan.manifestsMessage
	}
	return nil
}

// aggregateChildrenSubtreeManifestsPersisted reports whether every child content has
// subtreeManifestsPersisted=true (allPersisted) plus a progress message. A NotFound or not-yet-persisted
// child is pending (not a failure): the subtree is simply not persisted yet. A terminal child failure is
// NOT reported here — it surfaces via the child Ready terminal reason (handled by validateCommonContentChildren
// as ChildrenFailed). This mirrors the child walk in validateCommonContentChildren but keys on the
// subtreeManifestsPersisted bool rather than Ready.
//
// Reliability (fail-closed against declared-but-unlinked children): a published-edges-only view
// (status.childrenSnapshotContentRefs) can momentarily see FEWER (or zero) children than the owning
// snapshot declares, because child content edges are published atomically only AFTER every declared
// child snapshot binds (PublishSnapshotContentChildrenFromSnapshotRefs). Latching persisted=true on that
// partial view is the root cause of duplicate root captures: a not-yet-linked descendant subtree is not
// reached by the root subtree walk and so its manifests are not excluded. Therefore allPersisted also
// requires every DECLARED non-leaf child of the owning snapshot (spec.snapshotRef ->
// status.childrenSnapshotRefs) to be resolved, bound, linked into childrenSnapshotContentRefs, AND
// persisted; any unresolved/unbound/unlinked declared child is pending. Because the latch is one-way
// (never re-opens), this declared-vs-linked check is the only way to guarantee true implies a complete,
// fully linked subtree.
func (r *SnapshotContentController) aggregateChildrenSubtreeManifestsPersisted(ctx context.Context, parentContentObj *unstructured.Unstructured) (bool, string, error) {
	rawRefs, _, err := unstructured.NestedSlice(parentContentObj.Object, "status", "childrenSnapshotContentRefs")
	if err != nil {
		return false, "", err
	}
	// linked tracks every published child content edge so the declared-vs-linked check below can detect a
	// declared child that is not yet present in childrenSnapshotContentRefs.
	linked := make(map[string]struct{}, len(rawRefs))
	total := 0
	persistedCount := 0
	var pendingNames []string
	for _, raw := range rawRefs {
		refMap, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := refMap["name"].(string)
		if name == "" {
			continue
		}
		linked[name] = struct{}{}
		total++
		childContent := &unstructured.Unstructured{}
		childContent.SetGroupVersionKind(unifiedbootstrap.CommonSnapshotContentGVK())
		// Cached read of the child content (same For-informer GVK). A just-created/not-yet-synced child
		// missing from the cache is treated as pending (fail-closed), and the latch is one-way, so a stale
		// read can only delay it, never falsely latch it true.
		if err := r.Client.Get(ctx, client.ObjectKey{Name: name}, childContent); err != nil {
			if errors.IsNotFound(err) {
				pendingNames = append(pendingNames, name)
				continue
			}
			return false, "", err
		}
		if persisted, _, _ := unstructured.NestedBool(childContent.Object, "status", "subtreeManifestsPersisted"); persisted {
			persistedCount++
			continue
		}
		pendingNames = append(pendingNames, name)
	}

	// Fail-closed: every declared non-leaf child must be linked into the published edge set before the
	// subtree can be considered persisted.
	declaredNames, declaredComplete, err := r.declaredNonLeafChildContentNames(ctx, parentContentObj)
	if err != nil {
		return false, "", err
	}
	if !declaredComplete {
		return false, "waiting for declared child snapshot graph to bind and link", nil
	}
	var unlinked []string
	for _, dn := range declaredNames {
		if _, ok := linked[dn]; !ok {
			unlinked = append(unlinked, dn)
		}
	}
	if len(unlinked) > 0 {
		return false, "waiting for declared child content to be linked: " + strings.Join(unlinked, ", "), nil
	}

	if len(pendingNames) > 0 {
		return false, "waiting for child manifests to persist: " + formatReadyProgress(persistedCount, total, pendingNames), nil
	}
	return true, "", nil
}

// declaredNonLeafChildContentNames resolves, from this content's owning snapshot (spec.snapshotRef ->
// status.childrenSnapshotRefs), the bound child SnapshotContent name of every declared non-leaf child.
// It is the authoritative "expected children" set used to fail-close ManifestsArchived against
// declared-but-unlinked children.
//
// Returns (names, complete, err):
//   - complete=true with names: every declared non-leaf child is resolved+bound (names may be empty for a
//     leaf node with no declared children).
//   - complete=false: a child is not yet resolvable/bound, or the owning snapshot is not observable yet
//     (pending, fail-closed; never treat an unverifiable set as archived).
//   - err: only for unexpected API errors.
//
// When spec.snapshotRef is entirely absent (Required by the CRD and set at every creation site; absence
// only occurs for synthetic/legacy objects) the declared set cannot be verified and the function falls
// back to the published-edges-only view (complete=true, no declared names).
func (r *SnapshotContentController) declaredNonLeafChildContentNames(ctx context.Context, contentObj *unstructured.Unstructured) ([]string, bool, error) {
	apiVersion, _, _ := unstructured.NestedString(contentObj.Object, "spec", "snapshotRef", "apiVersion")
	kind, _, _ := unstructured.NestedString(contentObj.Object, "spec", "snapshotRef", "kind")
	name, _, _ := unstructured.NestedString(contentObj.Object, "spec", "snapshotRef", "name")
	namespace, _, _ := unstructured.NestedString(contentObj.Object, "spec", "snapshotRef", "namespace")
	if apiVersion == "" || kind == "" || name == "" {
		return nil, true, nil
	}

	owner := &unstructured.Unstructured{}
	owner.SetGroupVersionKind(schema.FromAPIVersionAndKind(apiVersion, kind))
	// Deliberately uncached (APIReader): this owning Snapshot's status.childrenSnapshotRefs is the
	// AUTHORITATIVE declared-child set that fail-closes the one-way ManifestsArchived latch (see
	// aggregateChildrenManifestsArchived). Unlike the cache-missing child CONTENT reads above - which only
	// ever read as pending and therefore merely DELAY the latch - a stale owner can return a SMALLER
	// declared set that omits a just-declared child while declaredComplete still reports true, letting the
	// latch close True over an unlinked descendant subtree. Because the latch never re-opens, that is a
	// permanent duplicate-root-capture. The declared set must be read fresh from the API server.
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, owner); err != nil {
		if errors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}

	rawRefs, _, err := unstructured.NestedSlice(owner.Object, "status", "childrenSnapshotRefs")
	if err != nil {
		return nil, false, err
	}
	names := make([]string, 0, len(rawRefs))
	for _, raw := range rawRefs {
		m, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		ref := storagev1alpha1.SnapshotChildRef{}
		ref.APIVersion, _ = m["apiVersion"].(string)
		ref.Kind, _ = m["kind"].(string)
		ref.Name, _ = m["name"].(string)
		// Uncached (APIReader) for the same reason as the owner GET above: resolving each declared child
		// snapshot to its bound content name is part of building the authoritative declared set; a stale
		// resolution would weaken the fail-closed declared-vs-linked guard on the one-way archive latch.
		bound, rerr := usecase.ResolveChildSnapshotRefToBoundContentName(ctx, r.APIReader, ref, namespace)
		if rerr != nil {
			if stderrors.Is(rerr, usecase.ErrRunGraphChildNotBound) ||
				stderrors.Is(rerr, usecase.ErrRunGraphChildSnapshotNotFound) {
				return nil, false, nil
			}
			return nil, false, rerr
		}
		if bound == "" {
			return nil, false, nil
		}
		names = append(names, bound)
	}
	return names, true, nil
}

// terminalDataFailureReasons lists data/volume-leg Ready=False reasons treated as terminal (vs pending).
// ArtifactNotReady (VSC not yet readyToUse) is pending; the rest are terminal. The manifest leg sets its
// own terminal flag directly (ManifestCheckpointFailed) and does not go through this set.
var terminalDataFailureReasons = map[string]struct{}{
	snapshot.ReasonDataArtifactInvalid:      {},
	snapshot.ReasonDataArtifactNotSupported: {},
	snapshot.ReasonArtifactMissing:          {},
}

func isTerminalDataFailure(reason string) bool {
	_, ok := terminalDataFailureReasons[reason]
	return ok
}

// fillOwnLegs evaluates this node's own legs into plan: ManifestsReady from the ManifestCheckpoint and
// VolumeReady from data artifacts. Children are not considered here. Sequencing (v1): the volume leg is
// only evaluated once the manifest leg is Ready; until then VolumeReady stays at its initial
// Unknown/ManifestCapturePending (not evaluated yet — not a volume failure). Independent leg evaluation
// is a future follow-up.
func (r *SnapshotContentController) fillOwnLegs(ctx context.Context, obj *unstructured.Unstructured, plan *commonContentStatusPlan) error {
	mcpName, _, err := unstructured.NestedString(obj.Object, "status", "manifestCheckpointName")
	if err != nil {
		return err
	}
	if mcpName == "" {
		plan.manifestsReady = metav1.ConditionFalse
		plan.manifestsReason = snapshot.ReasonManifestCapturePending
		plan.manifestsMessage = "waiting for manifest capture checkpoint to be published"
		return nil
	}

	mcpReady, mcpFailed, mcpMessage, err := r.validateCommonContentManifestCheckpoint(ctx, obj, mcpName)
	if err != nil {
		return err
	}
	if mcpFailed {
		plan.manifestsReady = metav1.ConditionFalse
		plan.manifestsFailed = true
		plan.manifestsReason = snapshot.ReasonManifestCheckpointFailed
		plan.manifestsMessage = mcpMessage
		return nil
	}
	if !mcpReady {
		plan.manifestsReady = metav1.ConditionFalse
		plan.manifestsReason = snapshot.ReasonManifestCapturePending
		plan.manifestsMessage = mcpMessage
		return nil
	}

	// Manifest leg satisfied.
	plan.manifestsReady = metav1.ConditionTrue
	plan.manifestsReason = snapshot.ReasonCompleted
	plan.manifestsMessage = "manifest is ready"

	// Volume leg: evaluated only after the manifest leg is Ready (kept sequencing, v1).
	dataReady, dataReason, dataMessage, err := r.resolveDataReadiness(ctx, obj)
	if err != nil {
		return err
	}
	if !dataReady {
		plan.volumeReady = metav1.ConditionFalse
		plan.volumeReason = dataReason
		plan.volumeMessage = dataMessage
		plan.volumeFailed = isTerminalDataFailure(dataReason)
		return nil
	}

	plan.volumeReady = metav1.ConditionTrue
	plan.volumeReason = snapshot.ReasonCompleted
	plan.volumeMessage = "data is ready"
	return nil
}

// terminalChildContentFailureReasons lists child SnapshotContent Ready=False reasons treated as a
// terminal failure that must propagate up the ancestor chain as ChildrenFailed (INV-FAIL1,
// snapshot-rework/2026-06-03-snapshot-conditions-model.md §5). Any other Ready=False (e.g.
// ArtifactNotReady, ManifestCapturePending, ChildrenPending, or no Ready condition yet) is
// non-terminal and propagates as ChildrenPending so a transient child does not fail the tree.
var terminalChildContentFailureReasons = map[string]struct{}{
	snapshot.ReasonManifestCheckpointFailed: {},
	snapshot.ReasonDataArtifactInvalid:      {},
	snapshot.ReasonDataArtifactNotSupported: {},
	snapshot.ReasonArtifactMissing:          {},
	snapshot.ReasonChildrenFailed:           {},
}

func isTerminalChildContentFailure(reason string) bool {
	_, ok := terminalChildContentFailureReasons[reason]
	return ok
}

// validateCommonContentChildren aggregates child SnapshotContent readiness for the parent Ready plan.
//
// Classification (INV-FAIL1): a child Ready=False with a terminal reason makes the parent
// ChildrenFailed immediately (terminal wins over pending regardless of ref order); any other
// not-Ready child (NotFound, no Ready condition, or non-terminal Ready=False) is collected as pending
// and surfaces as ChildrenPending only when no terminal failure exists. In both branches the
// message names the failed/pending child and carries its original Ready reason/message so a deeper
// leaf failure is not lost as the failure climbs the ancestor chain.
//
// Declared-vs-linked read barrier: orphan/residual-PVC children are ordinary domain children now
// (content-single-writer design §11.6), so they are covered by the generalized declared-non-leaf gate
// below (holds ChildrenReady=False/ChildrenLinkPending until every DECLARED child is linked into the
// frozen edge set) — there is no orphan-specific link gate anymore.
func (r *SnapshotContentController) validateCommonContentChildren(ctx context.Context, parentContentObj *unstructured.Unstructured) (bool, string, string, error) {
	rawRefs, _, err := unstructured.NestedSlice(parentContentObj.Object, "status", "childrenSnapshotContentRefs")
	if err != nil {
		return false, "", "", err
	}
	total := 0
	readyCount := 0
	var pendingNames []string
	linked := make(map[string]struct{}, len(rawRefs))
	for _, raw := range rawRefs {
		refMap, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := refMap["name"].(string)
		if name == "" {
			continue
		}
		linked[name] = struct{}{}
		total++
		childContent := &unstructured.Unstructured{}
		childContent.SetGroupVersionKind(unifiedbootstrap.CommonSnapshotContentGVK())
		// Cached read of the child content (same For-informer GVK). A cache-missing child is collected as
		// pending (fail-closed -> ChildrenPending), so a stale read delays the parent's Ready rather than
		// asserting it.
		if err := r.Client.Get(ctx, client.ObjectKey{Name: name}, childContent); err != nil {
			if errors.IsNotFound(err) {
				pendingNames = append(pendingNames, name)
				continue
			}
			return false, "", "", err
		}
		childLike, err := snapshot.ExtractSnapshotContentLike(childContent)
		if err != nil {
			return false, "", "", err
		}
		if snapshot.IsReady(childLike) {
			readyCount++
			continue
		}
		readyCond := snapshot.GetCondition(childLike, snapshot.ConditionReady)
		if readyCond != nil && readyCond.Status == metav1.ConditionFalse && isTerminalChildContentFailure(readyCond.Reason) {
			// Terminal wins over pending regardless of ref order: build the canonical leaf-chain message
			// (direct child = this child; leaf/reason/message pinned to the original failed leaf below).
			leaf, leafReason, leafMessage := childTerminalLeafInfo(name, readyCond)
			return false, snapshot.ReasonChildrenFailed,
				buildChildrenFailedMessage(name, leaf, leafReason, leafMessage), nil
		}
		pendingNames = append(pendingNames, name)
	}
	if len(pendingNames) > 0 {
		return false, snapshot.ReasonChildrenPending,
			"waiting for child snapshot contents: " + formatReadyProgress(readyCount, total, pendingNames), nil
	}

	// Read barrier (eager shells, content-single-writer design §3.6): with eager content creation a parent
	// shell can exist with DECLARED non-leaf children but an empty/partial childrenSnapshotContentRefs edge
	// set, so total==0 must NOT read as ChildrenReady=True for such a node — that would flip a subtree Ready
	// before its children are linked. Hold ChildrenReady=False (ChildrenLinkPending, fail-closed) until every
	// DECLARED non-leaf child is linked into the frozen edge set. This generalizes the orphan-only gate below
	// to ALL declared children.
	//
	// Skip once status.subtreeManifestsPersisted is latched true: that monotonic latch is set only after the
	// SAME declared-vs-linked check (aggregateChildrenSubtreeManifestsPersisted, on the same
	// declaredNonLeafChildContentNames source) already proved every declared non-leaf child is linked, so the
	// read barrier is redundant then. Using the latch as the guard also prevents re-gating a completed subtree
	// whose owning Snapshot is later gone (recycle-bin): the owner becomes unobservable
	// (declaredComplete=false), which would otherwise fail-close a content that is already complete. (Ready=True
	// implies the latch is true, so this subsumes an alreadyReady guard.)
	if subtreeLatched, _, _ := unstructured.NestedBool(parentContentObj.Object, "status", "subtreeManifestsPersisted"); !subtreeLatched {
		declared, declaredComplete, derr := r.declaredNonLeafChildContentNames(ctx, parentContentObj)
		if derr != nil {
			return false, "", "", derr
		}
		if !declaredComplete {
			return false, snapshot.ReasonChildrenLinkPending,
				"waiting for declared child snapshot contents to resolve and link", nil
		}
		var unlinkedDeclared []string
		for _, n := range declared {
			if _, ok := linked[n]; !ok {
				unlinkedDeclared = append(unlinkedDeclared, n)
			}
		}
		if len(unlinkedDeclared) > 0 {
			return false, snapshot.ReasonChildrenLinkPending,
				"waiting for declared child content to be linked: " + strings.Join(unlinkedDeclared, ", "), nil
		}
	}

	if total == 0 {
		return true, "", "no child content", nil
	}
	return true, "", fmt.Sprintf("%d/%d child content ready", readyCount, total), nil
}

// reconcileChildContentEdges is the aggregator's writer of the DOMAIN/generic/import child edges in
// status.childrenSnapshotContentRefs (INV-CONTENT-CHILDREN-1, content-single-writer design §3.1/§3.2). It
// projects this content's child edges from its owning snapshot's status.childrenSnapshotRefs: each declared
// child (domain/generic/import, incl. orphan/residual-PVC VolumeSnapshot children — all ordinary domain
// children now, §11.6) -> its bound child SnapshotContent name (all-or-nothing per pass, requeue until
// every declared child is bound).
//
// The write is APPEND-ONLY in this block (the atomic frozen-set write lands in Block 4). It is universal
// across capture and import: import owners have no domain phase, so the "planned/complete" gate is exactly
// what PublishSnapshotContentChildrenFromSnapshotRefs enforces internally — an owner that has not planned
// has an empty childrenSnapshotRefs (nothing is projected), and a planned owner publishes only once every
// declared child snapshot has bound its content. It runs decoupled from the condition MergeFrom in
// reconcileCommonSnapshotContentStatus (a separate optimistic-locked status patch), matching the previous
// external publishers; the 500 ms self-requeue while !ready plus the child-content watch drive convergence.
func (r *SnapshotContentController) reconcileChildContentEdges(ctx context.Context, contentObj, owner *unstructured.Unstructured, ownerNamespace string, ownerFound bool) (requeue bool, err error) {
	if !ownerFound {
		// spec.snapshotRef absent (synthetic/legacy) or the owner is not observable yet: nothing to
		// project this pass (fail-soft, same as the removed publishers' pending path).
		return false, nil
	}
	rawRefs, _, err := unstructured.NestedSlice(owner.Object, "status", "childrenSnapshotRefs")
	if err != nil {
		return false, err
	}
	childRefs := snapshotChildRefsFromRaw(rawRefs)
	namespace := ownerNamespace

	// Steady-state short-circuit (perf, INV-CONTENT-CHILDREN-1). Edges are append-only and 1:1 with the
	// owner's declared children (each declared child -> its bound child-content edge, deduped by name), and
	// the owner's childrenSnapshotRefs is set-once at Planned, so the published set can never exceed the
	// declared set. Once it reaches the declared
	// count the edge set is COMPLETE and stable — there is nothing left to add. Returning here (using only
	// the in-memory content, no API reads) avoids the per-child uncached resolution
	// (ResolveChildSnapshotRefToBoundContentName) on every 500 ms readiness self-requeue, which a node keeps
	// issuing for its whole not-ready lifetime while it waits on the subtree archive latch. Without this an
	// otherwise-idle steady state hammered the apiserver and starved unrelated reconciles (observed as
	// envtest client-rate-limiter timeouts under the parallel integration suite).
	currentEdges, _, _ := unstructured.NestedSlice(contentObj.Object, "status", "childrenSnapshotContentRefs")
	if len(currentEdges) >= len(childRefs) {
		return false, nil
	}

	contentName := contentObj.GetName()

	// All declared children (domain/generic/import, incl. orphan/residual-PVC VolumeSnapshot children):
	// resolves each to its bound child content, ensures the parent->child lifecycle ownerRef, all-or-nothing
	// publish.
	published, err := PublishSnapshotContentChildrenFromSnapshotRefs(ctx, r.Client, r.APIReader, namespace, contentName, childRefs)
	if err != nil {
		return false, err
	}
	if !published {
		requeue = true
	}
	return requeue, nil
}

// reconcileManifestCheckpointNameProjection is the aggregator's writer of status.manifestCheckpointName
// (INV-CONTENT-WRITER-1, content-single-writer design §3.1/§3.2, Block 2). It projects the manifest leg
// pointer from the owning snapshot's ManifestCaptureRequest so the aggregator — not the binder — is the
// sole writer of the field that fillOwnLegs then validates (MCP Ready + ownership handoff onto the content).
//
// Root and domain capture owners are one code path: the manifest SDK (EnsureManifestCapture) publishes BOTH
// the root-namespace MCR name and every domain MCR name into the owner's
// status.captureState.domainSpecificController.manifestCaptureRequestName, which is read here. Orphan/
// residual-PVC VolumeSnapshot children are ordinary domain owners now (content-single-writer design §11.6):
// their storage-foundation VS domain controller requests the manifest MCR and publishes its name the same
// way, so this one projection path covers them too — there is no orphan carve-out.
//
// Latch semantics (mirrors the guard the binder documented at ensureSnapshotContentLinks): once published,
// the name is a durable pointer that is never re-derived by chasing a now-deleted MCR. The MCR is a
// core-internal execution handle the binder deletes only AFTER the content durably owns a Ready MCP
// (ManifestCaptureRequestSafeToDelete gates on content.status.manifestCheckpointName + a Ready MCP owned by
// the content), so the MCR is guaranteed to still exist when this projection first needs it — there is no
// publish-vs-delete race. A post-handoff NotFound therefore means "already published, MCR reaped": keep the
// pointer, do not requeue. A pre-publish NotFound (or an MCR whose CheckpointName the checkpoint controller
// has not claimed yet) means "not captured yet": requeue (also covered by the !ready self-requeue).
func (r *SnapshotContentController) reconcileManifestCheckpointNameProjection(ctx context.Context, contentObj, owner *unstructured.Unstructured, ownerNamespace string, ownerFound bool) (requeue bool, err error) {
	if !ownerFound {
		// spec.snapshotRef absent (synthetic/legacy) or owner not observable yet: nothing to project.
		return false, nil
	}
	namespace := ownerNamespace
	mcrName, _, err := unstructured.NestedString(owner.Object, "status", "captureState", "domainSpecificController", "manifestCaptureRequestName")
	if err != nil {
		return false, err
	}
	if mcrName == "" {
		// Owner has not requested manifest capture yet (pre-Planned, or an import owner whose manifest is
		// uploaded rather than captured): nothing to project this pass.
		return false, nil
	}
	published, _, err := unstructured.NestedString(contentObj.Object, "status", "manifestCheckpointName")
	if err != nil {
		return false, err
	}
	mcr := &ssv1alpha1.ManifestCaptureRequest{}
	if getErr := r.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: mcrName}, mcr); getErr != nil {
		if errors.IsNotFound(getErr) {
			// Post-publish: MCR reaped after a durable handoff -> keep the latched pointer, no requeue.
			// Pre-publish: MCR not created yet -> requeue until it appears.
			return published == "", nil
		}
		return false, getErr
	}
	if mcr.Status.CheckpointName == "" {
		// MCR exists but the checkpoint controller has not claimed the deterministic name yet.
		return published == "", nil
	}
	if published == mcr.Status.CheckpointName {
		return false, nil
	}
	if pubErr := PublishSnapshotContentManifestCheckpointName(ctx, r.Client, contentObj.GetName(), mcr.Status.CheckpointName); pubErr != nil {
		return false, pubErr
	}
	return false, nil
}

// ownerSnapshot resolves this content's owning snapshot via spec.snapshotRef and returns the owner object
// plus its namespace. found=false when spec.snapshotRef is absent (synthetic/legacy content) or the owner
// is not observable yet (NotFound, fail-soft). The owner is read fresh from the API server (APIReader) so a
// just-published status field (childrenSnapshotRefs / manifestCaptureRequestName) is never missed.
func (r *SnapshotContentController) ownerSnapshot(ctx context.Context, contentObj *unstructured.Unstructured) (*unstructured.Unstructured, string, bool, error) {
	apiVersion, _, _ := unstructured.NestedString(contentObj.Object, "spec", "snapshotRef", "apiVersion")
	kind, _, _ := unstructured.NestedString(contentObj.Object, "spec", "snapshotRef", "kind")
	name, _, _ := unstructured.NestedString(contentObj.Object, "spec", "snapshotRef", "name")
	namespace, _, _ := unstructured.NestedString(contentObj.Object, "spec", "snapshotRef", "namespace")
	if apiVersion == "" || kind == "" || name == "" {
		return nil, "", false, nil
	}
	owner := &unstructured.Unstructured{}
	owner.SetGroupVersionKind(schema.FromAPIVersionAndKind(apiVersion, kind))
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, owner); err != nil {
		if errors.IsNotFound(err) {
			return nil, "", false, nil
		}
		return nil, "", false, err
	}
	return owner, namespace, true, nil
}

// snapshotChildRefsFromRaw converts the unstructured status.childrenSnapshotRefs slice into typed
// SnapshotChildRef values (apiVersion/kind/name). Non-map entries are skipped; missing string fields
// default to empty. Shared by the edge-write projection (reconcileChildContentEdges) and the orphan-link
// read barrier (unlinkedOrphanChildContents) so the parse stays in one place.
func snapshotChildRefsFromRaw(rawRefs []interface{}) []storagev1alpha1.SnapshotChildRef {
	refs := make([]storagev1alpha1.SnapshotChildRef, 0, len(rawRefs))
	for _, raw := range rawRefs {
		m, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		ref := storagev1alpha1.SnapshotChildRef{}
		ref.APIVersion, _ = m["apiVersion"].(string)
		ref.Kind, _ = m["kind"].(string)
		ref.Name, _ = m["name"].(string)
		refs = append(refs, ref)
	}
	return refs
}

// childTerminalLeafInfo distills a terminal child's Ready condition into the original failed leaf's
// (leaf, reason, message). If the child itself failed on ChildrenFailed (intermediate node), the leaf
// info is parsed from the child's canonical message; otherwise the child is the failed leaf.
func childTerminalLeafInfo(childName string, readyCond *metav1.Condition) (leaf, reason, message string) {
	if readyCond.Reason == snapshot.ReasonChildrenFailed {
		if l, r, m, ok := parseChildrenFailedLeaf(readyCond.Message); ok {
			return l, r, m
		}
	}
	return childName, readyCond.Reason, readyCond.Message
}

func (r *SnapshotContentController) ensureChildSnapshotContentOwnedByParent(ctx context.Context, childName string, parentContentObj *unstructured.Unstructured) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		child := &storagev1alpha1.SnapshotContent{}
		if err := r.Client.Get(ctx, client.ObjectKey{Name: childName}, child); err != nil {
			return err
		}
		parent := &storagev1alpha1.SnapshotContent{}
		parent.Name = parentContentObj.GetName()
		parent.UID = parentContentObj.GetUID()
		_, err := controllercommon.EnsureLifecycleOwnerRef(ctx, r.Client, child, controllercommon.SnapshotContentOwnerReference(parent))
		return err
	})
}

func (r *SnapshotContentController) validateCommonContentManifestCheckpoint(ctx context.Context, contentObj *unstructured.Unstructured, mcpName string) (bool, bool, string, error) {
	resolvedMCPName, ready, failed, msg, err := r.resolveManifestCheckpointReady(ctx, mcpName)
	if err != nil {
		return false, false, "", err
	}
	if !ready {
		return ready, failed, msg, nil
	}
	if err := r.ensureManifestCheckpointOwnedByContent(ctx, resolvedMCPName, contentObj); err != nil {
		return false, false, "", err
	}
	// Phase 2a chunk integrity by exact ref (no list/watch, get-only): every chunk named in
	// MCP.status.chunks[] must still exist. A missing chunk is a terminal integrity loss of the
	// published checkpoint -> ManifestCheckpointFailed (propagates as ChildrenFailed). Chunk deletion
	// does NOT wake reconcile (no chunk watch by design); correctness is produced here on every
	// reconcile, and the read/download/archive path fails immediately on use regardless.
	missingChunk, chunkErr := r.firstMissingManifestCheckpointChunk(ctx, resolvedMCPName)
	if chunkErr != nil {
		return false, false, "", chunkErr
	}
	if missingChunk != "" {
		return false, true, fmt.Sprintf("ManifestCheckpoint %s references missing chunk %s", resolvedMCPName, missingChunk), nil
	}
	return true, false, msg, nil
}

// firstMissingManifestCheckpointChunk validates chunk EXISTENCE by exact ref from MCP.status.chunks[].
// It deliberately does NOT read/decode chunk content (.spec.data) or verify checksums — content/integrity
// validation belongs to the explicit read/download/archive path, not to every reconcile. The MCP itself is
// read from the cache (its informer is already running: ensureManifestCheckpointOwnedByContent does a
// cached Get on it). Each chunk, however, is fetched metadata-only (PartialObjectMetadata) via the
// APIReader (direct, uncached) on purpose: a cached chunk Get would force the controller cache to start a
// ManifestCheckpointContentChunk informer (implicit list/watch), which Phase 2a avoids (chunks keep
// get-only RBAC). It stops at the first chunk that is NotFound and returns its name; transient
// (non-NotFound) errors are returned so reconcile requeues instead of falsely failing the tree.
func (r *SnapshotContentController) firstMissingManifestCheckpointChunk(ctx context.Context, mcpName string) (string, error) {
	mcp := &ssv1alpha1.ManifestCheckpoint{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: mcpName}, mcp); err != nil {
		if errors.IsNotFound(err) {
			// MCP vanished between checks; resolveManifestCheckpointReady reclassifies on the next reconcile.
			return "", nil
		}
		return "", err
	}
	chunkGVK := schema.GroupVersionKind{
		Group:   ssv1alpha1.SchemeGroupVersion.Group,
		Version: ssv1alpha1.SchemeGroupVersion.Version,
		Kind:    "ManifestCheckpointContentChunk",
	}
	for _, ch := range mcp.Status.Chunks {
		if ch.Name == "" {
			continue
		}
		meta := &metav1.PartialObjectMetadata{}
		meta.SetGroupVersionKind(chunkGVK)
		if err := r.APIReader.Get(ctx, client.ObjectKey{Name: ch.Name}, meta); err != nil {
			if errors.IsNotFound(err) {
				return ch.Name, nil
			}
			return "", err
		}
	}
	return "", nil
}

func (r *SnapshotContentController) ensureManifestCheckpointOwnedByContent(ctx context.Context, mcpName string, contentObj *unstructured.Unstructured) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		mcp := &ssv1alpha1.ManifestCheckpoint{}
		if err := r.Client.Get(ctx, client.ObjectKey{Name: mcpName}, mcp); err != nil {
			return err
		}
		ownerRef := metav1.OwnerReference{
			APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
			Kind:       "SnapshotContent",
			Name:       contentObj.GetName(),
			UID:        contentObj.GetUID(),
			Controller: func() *bool { b := true; return &b }(),
		}
		refs, changed, err := snapshotContentControllerOwnerRefsForHandoff(mcp.OwnerReferences, ownerRef)
		if err != nil {
			return fmt.Errorf("ManifestCheckpoint %s: %w", mcpName, err)
		}
		if !changed {
			return nil
		}
		base := mcp.DeepCopy()
		mcp.OwnerReferences = refs
		return r.Client.Patch(ctx, mcp, client.MergeFrom(base))
	})
}

// selfHealDataArtifactOwnerRefs re-asserts the SnapshotContent ownerRef on every VolumeSnapshotContent
// referenced by status.dataRefs[] (truth). This keeps ownerRef-only wake-up reliable without using
// dataRefs as a wake-up routing index (INV-OWNCHAIN). It reuses the same handoff owner-ref shape as the
// snapshot-side publisher (Controller=true, kind SnapshotContent) so the two writers converge and never
// flip-flop. It is best-effort: missing VSC is left to data readiness (ArtifactMissing), a deleting VSC
// is not patched, and any error is logged but does not fail reconcile or touch status.
func (r *SnapshotContentController) selfHealDataArtifactOwnerRefs(ctx context.Context, obj *unstructured.Unstructured) {
	logger := log.FromContext(ctx)
	contentLike, err := snapshot.ExtractSnapshotContentLike(obj)
	if err != nil {
		return
	}
	content := &storagev1alpha1.SnapshotContent{}
	content.Name = obj.GetName()
	content.UID = obj.GetUID()
	for _, binding := range contentLike.GetStatusDataRefs() {
		art := binding.Artifact
		if art.Kind != kindVolumeSnapshotContent || art.Name == "" {
			continue
		}
		vsc := &unstructured.Unstructured{}
		vsc.SetGroupVersionKind(artifactGVK(volumeSnapshotContentAPIVersion, kindVolumeSnapshotContent))
		if getErr := r.Client.Get(ctx, client.ObjectKey{Name: art.Name}, vsc); getErr != nil {
			if !errors.IsNotFound(getErr) {
				logger.V(1).Info("self-heal: failed to get VolumeSnapshotContent; revalidation will backstop",
					"vsc", art.Name, "err", getErr.Error())
			}
			continue
		}
		if !vsc.GetDeletionTimestamp().IsZero() {
			// Deleting artifact: do not repair ownerRef; data readiness handles it.
			continue
		}
		if healErr := ensureVolumeSnapshotContentOwnedByContent(ctx, r.Client, art.Name, content); healErr != nil {
			logger.V(1).Info("self-heal: failed to ensure VolumeSnapshotContent ownerRef; wake-up may be degraded until next reconcile",
				"vsc", art.Name, "err", healErr.Error())
		}
	}
}

func snapshotContentControllerOwnerRefsForHandoff(existing []metav1.OwnerReference, desired metav1.OwnerReference) ([]metav1.OwnerReference, bool, error) {
	out := make([]metav1.OwnerReference, 0, len(existing)+1)
	desiredSet := false
	for _, ref := range existing {
		if ref.APIVersion == storagev1alpha1.SchemeGroupVersion.String() && ref.Kind == "SnapshotContent" {
			if ref.Name != desired.Name || (ref.UID != "" && desired.UID != "" && ref.UID != desired.UID) {
				return nil, false, fmt.Errorf("already owned by SnapshotContent %s", ref.Name)
			}
			if !desiredSet {
				out = append(out, desired)
				desiredSet = true
			}
			continue
		}
		if ref.Controller != nil && *ref.Controller {
			// Handoff replaces the old controller owner (for example ObjectKeeper or child Snapshot)
			// with SnapshotContent while preserving non-controller references.
			continue
		}
		out = append(out, ref)
	}
	if !desiredSet {
		out = append(out, desired)
	}
	return out, !controllercommon.OwnerReferencesEqual(existing, out), nil
}

// resolveManifestCheckpointReady reads the published ManifestCheckpoint (truth = status.manifestCheckpointName)
// and classifies it for the requests leg. Phase 2a uses only the current MCP state (no Ready-watermark):
//   - NotFound / no Ready condition / Ready=False non-terminal -> pending (ManifestCapturePending). NotFound is
//     a legitimate initial window because manifestCheckpointName is published before the MCP is created.
//   - Ready=False with terminal Failed reason -> failed (ManifestCheckpointFailed), original MCP message kept.
//   - Ready=True -> ready.
//
// Returns (resolvedName, ready, failed, message, error).
func (r *SnapshotContentController) resolveManifestCheckpointReady(ctx context.Context, mcpName string) (string, bool, bool, string, error) {
	mcp := &ssv1alpha1.ManifestCheckpoint{}
	// Cached read: the MCP informer is already started (ensureManifestCheckpointOwnedByContent reads it
	// via the cache), so this forces no new watch. A stale/absent MCP keeps the manifest leg pending
	// (fail-closed), which the self-requeue resolves once the cache catches up.
	if err := r.Client.Get(ctx, client.ObjectKey{Name: mcpName}, mcp); err != nil {
		if errors.IsNotFound(err) {
			return mcpName, false, false, fmt.Sprintf("waiting for ManifestCheckpoint %s to become Ready", mcpName), nil
		}
		return "", false, false, "", err
	}
	cond := meta.FindStatusCondition(mcp.Status.Conditions, ssv1alpha1.ManifestCheckpointConditionTypeReady)
	if cond == nil {
		return mcp.Name, false, false, fmt.Sprintf("waiting for ManifestCheckpoint %s to become Ready", mcp.Name), nil
	}
	if cond.Status == metav1.ConditionTrue {
		return mcp.Name, true, false, cond.Message, nil
	}
	if cond.Status == metav1.ConditionFalse && cond.Reason == ssv1alpha1.ManifestCheckpointConditionReasonFailed {
		return mcp.Name, false, true, cond.Message, nil
	}
	return mcp.Name, false, false, fmt.Sprintf("waiting for ManifestCheckpoint %s to become Ready", mcp.Name), nil
}

// cascadeRemoveFinalizersFromChildren removes finalizers from child SnapshotContent objects
// This unlocks GC for children, but does NOT initiate Delete(child-content)
// GC will handle deletion through ownerRef
//
// Important: Handles broken links gracefully to avoid deadlock
func (r *SnapshotContentController) cascadeRemoveFinalizersFromChildren(
	ctx context.Context,
	contentLike snapshot.SnapshotContentLike,
	obj *unstructured.Unstructured,
) error {
	logger := log.FromContext(ctx)
	childrenRefs := contentLike.GetStatusChildrenSnapshotContentRefs()

	if len(childrenRefs) == 0 {
		// No children - nothing to cascade
		return nil
	}

	logger.Info("Cascading finalizer removal to children", "childrenCount", len(childrenRefs))

	// Get Content GVK to derive child Content GVK
	contentGVK := obj.GetObjectKind().GroupVersionKind()

	var childErrors []error
	for _, childRef := range childrenRefs {
		childContentGVK := contentGVK
		if childRef.Kind != "" && !isCommonSnapshotContentGVK(contentGVK) {
			// Resolve child Content GVK through registry for legacy generic content shapes.
			resolvedGVK, err := r.GVKRegistry.ResolveSnapshotContentGVK(childRef.Kind)
			if err != nil {
				// Fallback: derive from parent Content GVK if registry doesn't know
				resolvedGVK = schema.GroupVersionKind{
					Group:   contentGVK.Group,
					Version: contentGVK.Version,
					Kind:    childRef.Kind,
				}
				logger.V(1).Info("Child Content GVK not found in registry, using fallback", "kind", childRef.Kind)
			}
			childContentGVK = resolvedGVK
		}

		childObj := &unstructured.Unstructured{}
		childObj.SetGroupVersionKind(childContentGVK)
		childKey := client.ObjectKey{Name: childRef.Name}

		// Try to get child Content
		childGetErr := r.Get(ctx, childKey, childObj)
		if childGetErr != nil {
			if errors.IsNotFound(childGetErr) {
				// Child already deleted - skip (broken link, but not an error)
				logger.V(1).Info("Child SnapshotContent not found, skipping", "child", childRef.Name)
				continue
			}
			// Other error - log but continue
			logger.Error(childGetErr, "Failed to get child SnapshotContent", "child", childRef.Name)
			childErrors = append(childErrors, fmt.Errorf("failed to get child %s: %w", childRef.Name, childGetErr))
			continue
		}

		// Reclaim the child's physical data artifacts BEFORE removing its finalizer: stripping a direct
		// child's parent-protect finalizer here lets GC delete it WITHOUT running its own deletion handler
		// (and thus without its own reclaim). So reclaim is a gate: if it fails, DO NOT remove this child's
		// finalizer — leave parent-protect in place so the child's own deletion handler runs as the backstop
		// reclaim, and record the error so the parent cascade requeues. This keeps the unified teardown
		// reclaim complete for every node (no orphaned physical CSI snapshot). Idempotent across retries.
		if err := r.reclaimDataArtifactsFromContentObj(ctx, childObj); err != nil {
			logger.Error(err, "Failed to reclaim child data artifacts during cascade; keeping child finalizer", "child", childRef.Name)
			childErrors = append(childErrors, fmt.Errorf("reclaim child %s data artifacts: %w", childRef.Name, err))
			continue
		}

		// Remove finalizer from child
		if snapshot.RemoveFinalizer(childObj, snapshot.FinalizerParentProtect) {
			logger.Info("Removed finalizer from child SnapshotContent", "child", childRef.Name)
			childUpdateErr := r.Update(ctx, childObj)
			if childUpdateErr != nil {
				if errors.IsNotFound(childUpdateErr) {
					// Child was deleted between Get and Update - skip
					logger.V(1).Info("Child SnapshotContent was deleted, skipping update", "child", childRef.Name)
					continue
				}
				logger.Error(childUpdateErr, "Failed to remove finalizer from child", "child", childRef.Name)
				childErrors = append(childErrors, fmt.Errorf("failed to update child %s: %w", childRef.Name, childUpdateErr))
				continue
			}
		} else {
			// Finalizer already removed - child is already being processed
			logger.V(1).Info("Child SnapshotContent finalizer already removed", "child", childRef.Name)
		}
	}

	// Return error only if all children failed (partial success is acceptable)
	if len(childErrors) > 0 && len(childErrors) == len(childrenRefs) {
		return fmt.Errorf("failed to remove finalizers from all children: %v", childErrors)
	}

	if len(childErrors) > 0 {
		logger.Info("Some children failed, but cascade continues", "failedCount", len(childErrors), "totalCount", len(childrenRefs))
	}

	return nil
}

// removeArtifactFinalizer removes artifact-protect finalizer from MCP/VSC if present.
func (r *SnapshotContentController) removeArtifactFinalizer(ctx context.Context, kind, name, apiVersion string) error {
	var gvk schema.GroupVersionKind
	if idx := strings.Index(apiVersion, "/"); idx != -1 {
		gvk = schema.GroupVersionKind{
			Group:   apiVersion[:idx],
			Version: apiVersion[idx+1:],
			Kind:    kind,
		}
	} else {
		gvk = schema.GroupVersionKind{
			Group:   "",
			Version: apiVersion,
			Kind:    kind,
		}
	}

	artifactObj := &unstructured.Unstructured{}
	artifactObj.SetGroupVersionKind(gvk)
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: name}, artifactObj); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}

	if snapshot.RemoveFinalizer(artifactObj, snapshot.FinalizerArtifactProtect) {
		if err := r.Update(ctx, artifactObj); err != nil {
			return err
		}
		log.FromContext(ctx).Info("Removed artifact finalizer", "kind", kind, "name", name, "finalizer", snapshot.FinalizerArtifactProtect)
	}

	return nil
}

func (r *SnapshotContentController) clusterGVKsSnapshot() []schema.GroupVersionKind {
	r.watchMu.RLock()
	defer r.watchMu.RUnlock()
	out := make([]schema.GroupVersionKind, len(r.clusterGVKs))
	copy(out, r.clusterGVKs)
	return out
}

func (r *SnapshotContentController) namespacedGVKsSnapshot() []schema.GroupVersionKind {
	r.watchMu.RLock()
	defer r.watchMu.RUnlock()
	out := make([]schema.GroupVersionKind, len(r.namespacedGVKs))
	copy(out, r.namespacedGVKs)
	return out
}

// AddWatchForContent registers a SnapshotContent GVK with the manager at runtime. Idempotent per content GVK.
// On Complete failure, slice entries appended in this call are removed; registry is reverted only if at least
// one such slice was extended (same bootstrap-protection idea as GenericSnapshotBinderController.AddWatchForPair).
func (r *SnapshotContentController) AddWatchForContent(mgr ctrl.Manager, snapshotGVK, contentGVK schema.GroupVersionKind) error {
	r.watchMu.Lock()
	defer r.watchMu.Unlock()
	if r.activeContentWatchSet == nil {
		r.activeContentWatchSet = make(map[string]struct{})
	}
	key := contentGVK.String()
	if _, ok := r.activeContentWatchSet[key]; ok {
		return nil
	}
	mapping, err := r.RESTMapper.RESTMapping(contentGVK.GroupKind(), contentGVK.Version)
	if err != nil {
		return fmt.Errorf("RESTMapping for %s: %w", contentGVK.String(), err)
	}
	if err := r.GVKRegistry.RegisterSnapshotContentMapping(
		snapshotGVK.Kind, snapshotGVK.GroupVersion().String(),
		contentGVK.Kind, contentGVK.GroupVersion().String(),
	); err != nil {
		return fmt.Errorf("register snapshot/content mapping: %w", err)
	}
	needAppendMain := true
	for _, g := range r.SnapshotContentGVKs {
		if g == contentGVK {
			needAppendMain = false
			break
		}
	}
	if needAppendMain {
		r.SnapshotContentGVKs = append(r.SnapshotContentGVKs, contentGVK)
	}
	nsScoped := mapping.Scope.Name() == meta.RESTScopeNameNamespace
	needAppendScope := true
	if nsScoped {
		for _, g := range r.namespacedGVKs {
			if g == contentGVK {
				needAppendScope = false
				break
			}
		}
		if needAppendScope {
			r.namespacedGVKs = append(r.namespacedGVKs, contentGVK)
		}
	} else {
		for _, g := range r.clusterGVKs {
			if g == contentGVK {
				needAppendScope = false
				break
			}
		}
		if needAppendScope {
			r.clusterGVKs = append(r.clusterGVKs, contentGVK)
		}
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(contentGVK)
	builder := ctrl.NewControllerManagedBy(mgr).
		For(obj).
		WithOptions(snapshotContentControllerOptions()).
		Named(fmt.Sprintf("snapshotcontent-%s-%s", contentGVK.Group, contentGVK.Kind))
	if err := builder.Complete(r); err != nil {
		if needAppendMain {
			r.SnapshotContentGVKs = r.SnapshotContentGVKs[:len(r.SnapshotContentGVKs)-1]
		}
		if needAppendScope {
			if nsScoped {
				r.namespacedGVKs = r.namespacedGVKs[:len(r.namespacedGVKs)-1]
			} else {
				r.clusterGVKs = r.clusterGVKs[:len(r.clusterGVKs)-1]
			}
		}
		if needAppendMain || needAppendScope {
			r.GVKRegistry.RevertSnapshotRegistrationIfExact(snapshotGVK.Kind, snapshotGVK, contentGVK)
		}
		return fmt.Errorf("setup SnapshotContent watch for %s: %w", contentGVK.String(), err)
	}
	r.activeContentWatchSet[key] = struct{}{}
	return nil
}

// AddSnapshotStatusWatch registers a snapshot status watch that maps
// status.boundSnapshotContentName changes back to the bound common SnapshotContent.
// Snapshot GVKs are supplied by bootstrap/CSD runtime wiring; this controller must
// not hardcode domain snapshot kinds.
func (r *SnapshotContentController) AddSnapshotStatusWatch(mgr ctrl.Manager, snapshotGVK schema.GroupVersionKind) error {
	r.watchMu.Lock()
	defer r.watchMu.Unlock()
	return r.addSnapshotStatusWatchLocked(mgr, snapshotGVK)
}

// SetupWithManager sets up the controller with the Manager
// Registers watches for all registered SnapshotContent GVKs
// Each GVK gets its own controller instance to ensure correct GVK context
func (r *SnapshotContentController) SetupWithManager(mgr ctrl.Manager) error {
	r.watchMu.Lock()
	defer r.watchMu.Unlock()
	gvkStrings := make([]string, 0, len(r.SnapshotContentGVKs))
	for _, gvk := range r.SnapshotContentGVKs {
		gvkStrings = append(gvkStrings, gvk.String())
	}
	ctrl.Log.WithName("snapshotcontent-controller").Info("SnapshotContent controller configured", "gvks", gvkStrings)

	if r.activeContentWatchSet == nil {
		r.activeContentWatchSet = make(map[string]struct{})
	}
	for _, gvk := range r.SnapshotContentGVKs {
		key := gvk.String()
		if _, ok := r.activeContentWatchSet[key]; ok {
			continue
		}
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)
		builder := ctrl.NewControllerManagedBy(mgr).
			For(obj).
			WithOptions(snapshotContentControllerOptions()).
			Watches(obj, handler.EnqueueRequestsFromMapFunc(mapSnapshotContentToParentContent)).
			Named(fmt.Sprintf("snapshotcontent-%s-%s", gvk.Group, gvk.Kind))
		// Damaged-artifact wake-up (Phase 2a): enqueue the owning SnapshotContent when its durable
		// MCP/VSC artifacts change. Guarded by RESTMapping so a not-yet-installed CRD (e.g.
		// VolumeSnapshotContent under envtest) degrades to "no watch" instead of failing startup;
		// revalidation on the next reconcile still recomputes state (INV-RECONCILE-TRUTH).
		builder = r.addArtifactWakeUpWatches(builder)
		if err := builder.Complete(r); err != nil {
			return fmt.Errorf("failed to setup watch for SnapshotContent GVK %s: %w", gvk.String(), err)
		}
		r.activeContentWatchSet[key] = struct{}{}
	}
	return nil
}

// addArtifactWakeUpWatches adds enqueue-only Watches for the durable artifacts (ManifestCheckpoint,
// VolumeSnapshotContent) routed by ownerRef -> SnapshotContent. Each watch is added only when the GVK is
// RESTMappable so a missing CRD does not fail manager startup (the watch is simply absent and the next
// reconcile still revalidates from truth refs). Handlers never write status.
func (r *SnapshotContentController) addArtifactWakeUpWatches(b *builder.Builder) *builder.Builder {
	logger := ctrl.Log.WithName("snapshotcontent-controller")
	for _, gvk := range artifactWakeUpGVKs() {
		if _, err := r.RESTMapper.RESTMapping(gvk.GroupKind(), gvk.Version); err != nil {
			logger.Info("artifact wake-up watch skipped (GVK not RESTMappable yet); relying on reconcile-time revalidation",
				"gvk", gvk.String(), "reason", err.Error())
			continue
		}
		artifactObj := &unstructured.Unstructured{}
		artifactObj.SetGroupVersionKind(gvk)
		b = b.Watches(artifactObj, handler.EnqueueRequestsFromMapFunc(mapArtifactToOwningSnapshotContent))
	}
	return b
}

func (r *SnapshotContentController) addSnapshotStatusWatchLocked(mgr ctrl.Manager, snapshotGVK schema.GroupVersionKind) error {
	if r.activeSnapshotWatchSet == nil {
		r.activeSnapshotWatchSet = make(map[string]struct{})
	}
	key := snapshotGVK.String()
	if _, ok := r.activeSnapshotWatchSet[key]; ok {
		return nil
	}
	if _, err := r.RESTMapper.RESTMapping(snapshotGVK.GroupKind(), snapshotGVK.Version); err != nil {
		return fmt.Errorf("RESTMapping for snapshot status watch %s: %w", snapshotGVK.String(), err)
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(snapshotGVK)
	if err := ctrl.NewControllerManagedBy(mgr).
		Watches(obj, handler.EnqueueRequestsFromMapFunc(mapSnapshotStatusToBoundCommonContent)).
		// Manifest-leg wake-up (content-single-writer design §3.2, Block 2): the aggregator is the single
		// writer of status.manifestCheckpointName (reconcileManifestCheckpointNameProjection), which reads
		// the owning snapshot's ManifestCaptureRequest status.checkpointName. The checkpoint controller
		// claims that name on the MCR WITHOUT touching the snapshot/content/MCP, so without this watch the
		// projection would only observe it on the 500 ms self-requeue. This enqueue-only watch makes the
		// MCR.checkpointName -> content.manifestCheckpointName handoff event-driven (the binder watches the
		// MCR for the same reason, mcr_watch.go); the self-requeue remains a safety net for missed events.
		Watches(&ssv1alpha1.ManifestCaptureRequest{}, handler.EnqueueRequestsFromMapFunc(r.mapMCRToBoundContent(snapshotGVK))).
		WithOptions(snapshotContentControllerOptions()).
		Named(fmt.Sprintf("snapshotcontent-snapshot-%s-%s", snapshotGVK.Group, snapshotGVK.Kind)).
		Complete(r); err != nil {
		return fmt.Errorf("setup SnapshotContent snapshot status watch for %s: %w", snapshotGVK.String(), err)
	}
	r.activeSnapshotWatchSet[key] = struct{}{}
	return nil
}

func mapSnapshotStatusToBoundCommonContent(_ context.Context, obj client.Object) []reconcile.Request {
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil
	}
	boundName, _, err := unstructured.NestedString(raw, "status", "boundSnapshotContentName")
	if err != nil || boundName == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: boundName}}}
}

func mapSnapshotContentToParentContent(_ context.Context, obj client.Object) []reconcile.Request {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.APIVersion == storagev1alpha1.SchemeGroupVersion.String() && ref.Kind == "SnapshotContent" && ref.Name != "" {
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: ref.Name}}}
		}
	}
	return nil
}

// mapMCRToBoundContent returns a watch map function that routes a ManifestCaptureRequest change to the
// SnapshotContent(s) whose owning snapshot (of snapshotGVK, in the MCR's namespace) references it via
// status.captureState.domainSpecificController.manifestCaptureRequestName — the SAME truth ref the manifest
// projection reads (reconcileManifestCheckpointNameProjection). The MCR carries no reverse content
// reference and snapshot GVKs register at runtime (CSD-driven), so a namespaced List+filter resolves the
// owner for both bootstrap and runtime registration (mirrors the binder's mapMCRToOwningSnapshots). The
// handler only enqueues; it never writes status. A missing/empty boundSnapshotContentName is skipped (the
// owning snapshot is not bound yet; the aggregator has nothing to project for it).
func (r *SnapshotContentController) mapMCRToBoundContent(snapshotGVK schema.GroupVersionKind) func(context.Context, client.Object) []reconcile.Request {
	listGVK := schema.GroupVersionKind{
		Group:   snapshotGVK.Group,
		Version: snapshotGVK.Version,
		Kind:    snapshotGVK.Kind + "List",
	}
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		if obj == nil || obj.GetName() == "" {
			return nil
		}
		mcrName := obj.GetName()
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(listGVK)
		opts := []client.ListOption{}
		if ns := obj.GetNamespace(); ns != "" {
			opts = append(opts, client.InNamespace(ns))
		}
		if err := r.List(ctx, list, opts...); err != nil {
			log.FromContext(ctx).V(1).Info("mcr wake-up: failed to list snapshots",
				"snapshotKind", snapshotGVK.Kind, "mcr", mcrName, "error", err.Error())
			return nil
		}
		var reqs []reconcile.Request
		for i := range list.Items {
			name, _, _ := unstructured.NestedString(list.Items[i].Object, "status", "captureState", "domainSpecificController", "manifestCaptureRequestName")
			if name == "" || name != mcrName {
				continue
			}
			boundContent, _, _ := unstructured.NestedString(list.Items[i].Object, "status", "boundSnapshotContentName")
			if boundContent == "" {
				continue
			}
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: boundContent}})
		}
		return reqs
	}
}

// manifestCheckpointGVK / volumeSnapshotContentGVK are the durable top-level artifacts whose
// create/update/delete events must wake the owning SnapshotContent (INV-OWNCHAIN). Routing is strictly
// by the artifact's ownerRef -> SnapshotContent; no reverse-index by dataRefs[] and no chunk shortcut.
func artifactWakeUpGVKs() []schema.GroupVersionKind {
	return []schema.GroupVersionKind{
		{Group: ssv1alpha1.SchemeGroupVersion.Group, Version: ssv1alpha1.SchemeGroupVersion.Version, Kind: "ManifestCheckpoint"},
		{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotContent"},
	}
}

// mapArtifactToOwningSnapshotContent routes a durable-artifact event (ManifestCheckpoint /
// VolumeSnapshotContent) to its owning SnapshotContent by ownerRef only. It NEVER writes conditions or
// patches status — it only enqueues a reconcile.Request. If the ownerRef chain is missing or broken it
// logs a diagnostic and drops the event; the next reconcile recomputes state from truth refs
// (INV-RECONCILE-TRUTH). Tombstone/last-known objects on delete still carry ownerRefs, so routing works.
func mapArtifactToOwningSnapshotContent(ctx context.Context, obj client.Object) []reconcile.Request {
	if obj == nil {
		return nil
	}
	for _, ref := range obj.GetOwnerReferences() {
		if ref.APIVersion == storagev1alpha1.SchemeGroupVersion.String() && ref.Kind == "SnapshotContent" && ref.Name != "" {
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: ref.Name}}}
		}
	}
	log.FromContext(ctx).V(1).Info(
		"artifact event has no owning SnapshotContent ownerRef; dropping (revalidation backstops on next reconcile)",
		"artifactKind", obj.GetObjectKind().GroupVersionKind().Kind,
		"artifact", obj.GetName(),
	)
	return nil
}
