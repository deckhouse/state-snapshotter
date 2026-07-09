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
	"sigs.k8s.io/controller-runtime/pkg/source"

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

	// cacheReader is the manager cache (indexed informers). The enqueue-only reverse-lookup Lists that resolve
	// a changed artifact/child to the owning/parent SnapshotContent MUST read through this, not r.Client: the
	// manager client does not cache unstructured objects by default (Client.Cache.Unstructured=false), so a
	// client List with client.MatchingFields is sent to the API server as a field selector, which the API
	// server rejects for custom status fields ("field label not supported") — silently degrading every
	// manifest-leg wake to the 500ms self-requeue backstop. The cache uses the registered indexKey* indexes.
	// Set in SetupWithManager / AddWatchForContent; nil in unit tests (which wire an indexed fake as Client).
	cacheReader client.Reader

	// primaryContentController is the retained handle of the single common-SnapshotContent controller
	// (For(CommonSnapshotContentGVK)). Snapshot-status wakes are attached to it via Controller.Watch(...)
	// rather than constructing a second controller-runtime Controller per Snapshot GVK.
	primaryContentController controller.Controller
}

const defaultSnapshotContentRequeueAfter = 500 * time.Millisecond

// indexKeyManifestCheckpointName is the cache field-index key on SnapshotContent.status.manifestCheckpointName.
// It backs the pre-adoption reverse-lookup path of mapManifestCheckpointToContent (L9a): before the MCP is
// adopted (ownerRef handoff), the only stable MCP→content link is this deterministic 1:1 name.
const indexKeyManifestCheckpointName = ".status.manifestCheckpointName"

// indexKeyChildContentName is the cache field-index key on SnapshotContent.status.childrenSnapshotContentRefs[].name.
// It backs the forward-edge reverse-lookup wake-up (C-2): a parent's ManifestsArchived subtree latch aggregates
// its published child content edges, so when a CHILD content's status changes (ManifestsArchived / Ready / any
// leg) the parent(s) that reference it by name must be enqueued to re-evaluate immediately — event-driven —
// instead of waiting for the parent's 500ms self-requeue archive wave. This complements the ownerRef path
// (mapSnapshotContentToParentContent): the ownerRef child→parent handoff can be set after the child already
// reached its terminal state, so the ownerRef event is droppable/late; the published-edge index is populated
// when the parent links the child and is stable through the child's later archive transition.
const indexKeyChildContentName = ".status.childrenSnapshotContentRefs.name"

// indexKeyDataRefArtifactName is the cache field-index key on SnapshotContent.status.dataRef.artifact.name
// (only when the artifact is a VolumeSnapshotContent). It backs the dual-path VolumeSnapshotContent wake-up
// (Commit C): a leaf content latches VolumesReady from its published status.dataRef.artifact (the VSC name),
// but the VSC is created owned by an execution ObjectKeeper and only reparented to the SnapshotContent at
// handoff, so the ownerRef-only path drops readyToUse events that flip before adoption. This durable data
// edge is published when the content links the VSC and is stable across the VSC readyToUse transition, so it
// lets the readyToUse flip wake the owning content event-driven instead of via the 500ms self-requeue poll.
const indexKeyDataRefArtifactName = ".status.dataRef.artifact.name"

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
		annotations := obj.GetAnnotations()
		if annotations != nil && annotations[snapshot.AnnotationParentDeleted] == "true" {
			// Parent Snapshot is gone; don't re-add finalizer.
			return ctrl.Result{}, nil
		}
		if snapshot.AddFinalizer(obj, snapshot.FinalizerParentProtect) {
			logger.Info("Added finalizer to SnapshotContent", "finalizer", snapshot.FinalizerParentProtect)
			if err := r.Update(ctx, obj); err != nil {
				// L9c: a 409 here is benign. The same SnapshotContent is reconciled by multiple
				// controller instances that share this Reconciler (the For-content controller and the
				// per-snapshot status-watch controllers), so two workers can race on this finalizer
				// Update. Surfacing the conflict as a Reconciler error backs the item off on the
				// rate limiter (200ms→10s) for nothing; instead requeue and re-read. AddFinalizer is
				// idempotent, so whichever writer lands first wins and the next pass is a no-op.
				if errors.IsConflict(err) {
					logger.V(1).Info("Finalizer add conflicted with a concurrent update; requeueing (benign)",
						"finalizer", snapshot.FinalizerParentProtect)
					return ctrl.Result{Requeue: true}, nil
				}
				logger.Error(err, "Failed to add finalizer")
				return ctrl.Result{}, err
			}
			return ctrl.Result{Requeue: true}, nil
		}
	} else {
		// Object is being deleted - handle deletion (Phase 2: Cascade)
		// Invariant Phase 2: SnapshotContent with DeletionTimestamp →
		// first cascade finalizers → then GC via ownerRef

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
	// The common storage.deckhouse.io/SnapshotContent is the ONLY content carrier in the unified
	// runtime (every snapshot kind maps to CommonSnapshotContentGVK), and it owns the aggregate
	// condition model: ManifestsReady + VolumesReady + ChildrenReady + derived Ready
	// (INV-COND2). No non-common SnapshotContent GVK is registered, so there is no other writer.
	if !isCommonSnapshotContentGVK(obj.GroupVersionKind()) {
		logger.V(1).Info("non-common SnapshotContent GVK is not managed by the unified runtime; skipping",
			"gvk", obj.GroupVersionKind().String())
		return ctrl.Result{}, nil
	}
	ready, err := r.reconcileCommonSnapshotContentStatus(ctx, obj)
	if err != nil {
		logger.Error(err, "Failed to reconcile common SnapshotContent status")
		return ctrl.Result{}, err
	}
	// Keep actively requeuing until Ready is True. Ready includes the ManifestsArchived subtree latch as a
	// (monotonic) gate, so while any descendant is still archiving this node's Ready stays False — this
	// self-requeue is what drives the child->parent archive wave to converge via active re-evaluation
	// instead of stalling on a droppable wake-up event (declared-but-unlinked child, or a same-binary
	// artifact event seen before its ownerRef handoff) or the next informer resync (~minutes).
	if !ready {
		return ctrl.Result{RequeueAfter: defaultSnapshotContentRequeueAfter}, nil
	}

	logger.Info("SnapshotContent reconciliation completed")
	return ctrl.Result{}, nil
}

func isCommonSnapshotContentGVK(gvk schema.GroupVersionKind) bool {
	return gvk == unifiedbootstrap.CommonSnapshotContentGVK()
}

// commonContentStatusPlan is the SnapshotContent aggregation outcome. It carries the own-node legs
// (ManifestsReady = own MCP; VolumesReady = own data refs; ChildrenReady = child SnapshotContents) and
// the derived Ready. Ready is the single aggregate on SnapshotContent:
// Ready = ManifestsReady && VolumesReady && ChildrenReady (INV-COND2).
type commonContentStatusPlan struct {
	manifestsReady   metav1.ConditionStatus
	manifestsReason  string
	manifestsMessage string
	manifestsFailed  bool // terminal (vs pending) when manifestsReady != True

	volumesReady   metav1.ConditionStatus
	volumesReason  string
	volumesMessage string
	volumesFailed  bool // terminal (vs pending) when volumesReady != True

	childrenReady   metav1.ConditionStatus
	childrenReason  string
	childrenMessage string
	childrenFailed  bool // terminal (vs pending) when childrenReady != True

	readyStatus  metav1.ConditionStatus
	readyReason  string
	readyMessage string

	// ManifestsArchived is the subtree latch AND the second-lowest-priority Ready leg (the residual-sweep
	// gate is below it): it gates the first Ready=True; True once this node's own manifest leg reached
	// readiness AND every child content is ManifestsArchived=True; it never re-opens. Failed is terminal
	// (own manifest leg failed before archive, or a child can never be archived). Being monotonic, it gates
	// the first Ready=True without ever dragging Ready back down afterwards. See pkg/snapshot ConditionManifestsArchived.
	manifestsArchivedStatus  metav1.ConditionStatus
	manifestsArchivedReason  string
	manifestsArchivedMessage string

	// ResidualSweep gates the FIRST Ready=True on a namespace-root content until the reconciler latches
	// status.residualVolumeCapture.phase=Complete (the residual/orphan-PVC capture wave has finished). It
	// is a fail-closed, monotonic, lowest-priority Ready leg (below ManifestsArchived): leaf/domain-child/
	// non-root contents and roots whose Ready is already True are never gated. False is non-terminal
	// (ResidualVolumeCapturePending). See computeResidualSweepGate / api/storage ResidualVolumeCaptureStatus.
	residualSweepStatus  metav1.ConditionStatus
	residualSweepReason  string
	residualSweepMessage string
}

// reconcileCommonSnapshotContentStatus aggregates and publishes the SnapshotContent conditions and
// reports whether the derived Ready condition is True. Ready includes the ManifestsArchived subtree latch
// as a (monotonic) gate, so while any descendant is still archiving Ready stays False; the caller keeps
// requeuing on !ready, which is what drives the child->parent archive wave to converge via active
// re-evaluation instead of stalling on a droppable wake-up event (e.g. a not-yet-linked declared child, or
// a same-binary artifact event observed before its ownerRef handoff) or the next informer resync.
func (r *SnapshotContentController) reconcileCommonSnapshotContentStatus(ctx context.Context, obj *unstructured.Unstructured) (ready bool, err error) {
	start := time.Now() // L3b trace: per-reconcile wall time (see traceSnapshotContent).
	// Self-heal data-artifact ownerRefs from the published truth (status.dataRefs[]) so the
	// ownerRef-based VSC wake-up stays robust. Best-effort: never writes status, never fails
	// reconcile (INV-RECONCILE-TRUTH: correctness comes from revalidation below, watches are
	// only a liveness optimization).
	r.selfHealDataArtifactOwnerRefs(ctx, obj)

	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, obj)
	if err != nil {
		return false, err
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
		{Type: snapshot.ConditionVolumesReady, Status: plan.volumesReady, Reason: plan.volumesReason, Message: plan.volumesMessage, ObservedGeneration: gen},
		{Type: snapshot.ConditionChildrenReady, Status: plan.childrenReady, Reason: plan.childrenReason, Message: plan.childrenMessage, ObservedGeneration: gen},
		{Type: snapshot.ConditionReady, Status: plan.readyStatus, Reason: plan.readyReason, Message: plan.readyMessage, ObservedGeneration: gen},
		{Type: snapshot.ConditionManifestsArchived, Status: plan.manifestsArchivedStatus, Reason: plan.manifestsArchivedReason, Message: plan.manifestsArchivedMessage, ObservedGeneration: gen},
	}

	ready = plan.readyStatus == metav1.ConditionTrue

	changed := false
	for _, d := range desired {
		if upsertContentCondition(contentLike, d) {
			changed = true
		}
	}

	if !changed {
		r.traceSnapshotContent(ctx, obj, plan, "noop", start)
		return ready, nil
	}
	obj.Object["status"] = statusMap
	snapshot.SyncConditionsToUnstructured(obj, contentLike.GetStatusConditions())
	// Condition-only MergeFrom patch (not a full Status().Update). base vs obj differ only in
	// status.conditions, so the JSON merge patch is {"status":{"conditions":[...]}} and leaves sibling
	// status fields written by the snapshot reconciler (residualVolumeCapture, dataRefs, ...) untouched.
	// conditions are the aggregator's exclusive domain (INV-COND2, single writer) and reconcile of one
	// object is serialized, so no optimistic lock is needed: MergeFrom sends no resourceVersion, so a
	// concurrent sibling-field write does not turn into a 409 (which the previous Status().Update would
	// convert into an extra requeue). Staleness is still safe because the changed-gate above only fires when
	// the monotonic-cache-derived desired conditions actually differ.
	if err := r.Status().Patch(ctx, obj, client.MergeFrom(base)); err != nil {
		outcome := "patch-error"
		if errors.IsConflict(err) {
			outcome = "conflict"
		}
		r.traceSnapshotContent(ctx, obj, plan, outcome, start)
		return false, err
	}
	r.traceSnapshotContent(ctx, obj, plan, "changed", start)
	return ready, nil
}

// traceSnapshotContent emits a single structured per-reconcile diagnostic line for the SnapshotContent
// aggregation. It NEVER changes state or control flow. It is logged at V(1) (debug), not INFO, so it does
// not spam production at the default verbosity; raise verbosity to rebuild a TREES=N burst timeline
// (which leg still gates Ready, whether the status patch changed / was a no-op / conflicted, the declared
// child count, and the reconcile wall time). Used to prove L3b: the long tree-Ready tail was the manager
// client's default rate limiter (QPS 5 / Burst 10) inflating reconcile durMs, not archive-wave logic.
func (r *SnapshotContentController) traceSnapshotContent(ctx context.Context, obj *unstructured.Unstructured, plan commonContentStatusPlan, patch string, start time.Time) {
	childRefs, _, _ := unstructured.NestedSlice(obj.Object, "status", "childrenSnapshotContentRefs")
	log.FromContext(ctx).V(1).Info("snapshotcontent trace",
		"content", obj.GetName(),
		"uid", string(obj.GetUID()),
		"gen", obj.GetGeneration(),
		"childRefs", len(childRefs),
		"manifestsReady", condShort(plan.manifestsReady),
		"volumesReady", condShort(plan.volumesReady),
		"childrenReady", condShort(plan.childrenReady),
		"manifestsArchived", condShort(plan.manifestsArchivedStatus),
		"ready", condShort(plan.readyStatus),
		"gate", plan.readyReason,
		"patch", patch,
		"durMs", time.Since(start).Milliseconds(),
	)
}

// condShort renders a condition status as a single greppable character (T/F/U) for the trace line.
func condShort(s metav1.ConditionStatus) string {
	switch s {
	case metav1.ConditionTrue:
		return "T"
	case metav1.ConditionFalse:
		return "F"
	default:
		return "U"
	}
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

func (r *SnapshotContentController) ReconcileCommonSnapshotContentStatus(ctx context.Context, obj *unstructured.Unstructured) (ready bool, err error) {
	return r.reconcileCommonSnapshotContentStatus(ctx, obj)
}

// buildCommonSnapshotContentStatusPlan computes ManifestsReady, VolumesReady, ChildrenReady, the
// ManifestsArchived subtree latch, the residual-sweep gate, and the derived Ready. Ready priority (single
// reason when several legs are not satisfied):
//
//	manifestsFailed > volumesFailed > childrenFailed > manifestsPending > volumesPending > childrenPending > archivePending > residualSweepPending > Completed
//
// Terminal failures win over pending (actionable first); own-node legs win over children, and the
// manifest leg wins over the volume leg at equal severity. ManifestsArchived is a low-priority gate: it
// blocks the FIRST Ready=True until the whole subtree's manifests are archived. The residual-sweep gate is
// the LOWEST priority: on a namespace-root content it blocks the FIRST Ready=True until the residual/
// orphan-PVC capture wave is latched Complete (fail-closed). Both are monotonic, so neither ever drags
// Ready back down after the first Ready=True. ChildrenSnapshotReady is NOT part of this formula; it is
// only a gate/barrier upstream.
func (r *SnapshotContentController) buildCommonSnapshotContentStatusPlan(ctx context.Context, obj *unstructured.Unstructured) (commonContentStatusPlan, error) {
	plan := commonContentStatusPlan{
		manifestsReady:   metav1.ConditionFalse,
		manifestsReason:  snapshot.ReasonManifestCapturePending,
		manifestsMessage: "waiting for manifest capture",
		// Volume leg is not evaluated until the manifest leg is Ready (kept sequencing, v1): Unknown,
		// not False, so it does not look like a volume failure.
		volumesReady:    metav1.ConditionUnknown,
		volumesReason:   snapshot.ReasonManifestCapturePending,
		volumesMessage:  "data leg not evaluated until manifest capture is ready",
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

	// ManifestsArchived gates Ready: a node cannot reach its FIRST Ready=True until its own and all
	// descendant manifests are archived. The latch is monotonic (computeManifestsArchived holds True once
	// True), so after the first Ready=True the gate stays satisfied and Ready is free to flap on the live
	// legs. Computed before the switch so it can participate as the lowest-priority Ready leg.
	if err := r.computeManifestsArchived(ctx, obj, &plan); err != nil {
		return plan, err
	}

	// Residual-sweep gate (lowest-priority Ready leg): on a namespace-root content, hold the FIRST
	// Ready=True until the reconciler latches the residual/orphan-PVC wave Complete. Local read only
	// (no owner GET): the discriminator comes from this content's own spec.snapshotRef.
	if err := r.computeResidualSweepGate(obj, &plan); err != nil {
		return plan, err
	}

	switch {
	case plan.manifestsReady != metav1.ConditionTrue && plan.manifestsFailed:
		plan.readyStatus = metav1.ConditionFalse
		plan.readyReason = plan.manifestsReason
		plan.readyMessage = plan.manifestsMessage
	case plan.volumesReady != metav1.ConditionTrue && plan.volumesFailed:
		plan.readyStatus = metav1.ConditionFalse
		plan.readyReason = plan.volumesReason
		plan.readyMessage = plan.volumesMessage
	case plan.childrenReady != metav1.ConditionTrue && plan.childrenFailed:
		plan.readyStatus = metav1.ConditionFalse
		plan.readyReason = plan.childrenReason
		plan.readyMessage = plan.childrenMessage
	case plan.manifestsReady != metav1.ConditionTrue:
		plan.readyStatus = metav1.ConditionFalse
		plan.readyReason = plan.manifestsReason
		plan.readyMessage = plan.manifestsMessage
	case plan.volumesReady != metav1.ConditionTrue:
		plan.readyStatus = metav1.ConditionFalse
		plan.readyReason = plan.volumesReason
		plan.readyMessage = plan.volumesMessage
	case plan.childrenReady != metav1.ConditionTrue:
		plan.readyStatus = metav1.ConditionFalse
		plan.readyReason = plan.childrenReason
		plan.readyMessage = plan.childrenMessage
	case plan.manifestsArchivedStatus != metav1.ConditionTrue:
		// Subtree archive gate (lowest priority): blocks the first Ready=True until own + all-descendant
		// manifests are archived. Reached only when every live leg is already satisfied (the cases above did
		// not fire), so in practice archived here is the transient ManifestsCapturing state — the fail-closed
		// declared-but-unlinked-child wait. A terminal ManifestsArchiveFailed cannot reach this case: it
		// implies a not-Ready linked child (archived also gates the child's Ready), which trips
		// ChildrenFailed/ChildrenPending above, and the underlying manifest failure independently surfaces as
		// ManifestCheckpointFailed/ChildrenFailed.
		plan.readyStatus = metav1.ConditionFalse
		plan.readyReason = plan.manifestsArchivedReason
		plan.readyMessage = plan.manifestsArchivedMessage
	case plan.residualSweepStatus != metav1.ConditionTrue:
		// Residual/orphan-PVC capture gate (lowest priority, fail-closed): on a namespace-root content,
		// block the FIRST Ready=True until the reconciler latches residualVolumeCapture.phase=Complete.
		// Reached only when every other leg (including the archive latch) is already satisfied. Monotonic:
		// computeResidualSweepGate opens the gate once Ready is already True (or the latch is Complete), so
		// this case never fires afterwards and cannot drag Ready back down (no True->False flap).
		plan.readyStatus = metav1.ConditionFalse
		plan.readyReason = plan.residualSweepReason
		plan.readyMessage = plan.residualSweepMessage
	default:
		plan.readyStatus = metav1.ConditionTrue
		plan.readyReason = snapshot.ReasonCompleted
		plan.readyMessage = "manifests (archived), data, and child content are ready"
	}

	return plan, nil
}

// computeManifestsArchived fills the ManifestsArchived subtree latch on plan. It is a lifelong latch
// (never re-opens) that records the irreversible fact that this node and its whole subtree had their
// manifests captured. It also acts as the second-lowest-priority Ready leg (the residual-sweep gate is
// below it; see buildCommonSnapshotContentStatusPlan): because it is monotonic, it gates the FIRST
// Ready=True but never drags Ready back down afterwards.
//
//   - If the current condition is already True (Archived) or terminally False (Failed), it is held as-is
//     (immune to later ManifestsReady/Ready degradation or to a child disappearing). Snapshot.spec is
//     immutable, so there is no recapture and no generation bookkeeping is needed.
//   - Otherwise it is derived from the current subtree state: own manifest leg failed terminally before
//     archive OR a child is Failed -> Failed; own ManifestsReady=True AND all children Archived -> True;
//     else Capturing (transient; includes the fail-closed NamespaceCaptureIncomplete wait).
//
// manifestLegAlreadyLatched reports whether this content's manifest leg is already verified ready AND the
// monotonic ManifestsArchived latch is closed for the current generation. When true, fillOwnLegs skips the
// expensive per-poll manifest re-validation (cached MCP Get + adoption patch + uncached APIReader chunk
// GETs): the latch never re-opens, so re-checking cannot change the manifest verdict, and chunk integrity
// is re-verified on the read/download/archive path rather than on this readiness gate. Requires BOTH
// ManifestsReady=True and ManifestsArchived=True at the current generation (a generation bump re-validates).
func manifestLegAlreadyLatched(obj *unstructured.Unstructured) bool {
	like, err := snapshot.ExtractSnapshotContentLike(obj)
	if err != nil {
		return false
	}
	gen := obj.GetGeneration()
	mr := snapshot.GetCondition(like, snapshot.ConditionManifestsReady)
	ma := snapshot.GetCondition(like, snapshot.ConditionManifestsArchived)
	return mr != nil && mr.Status == metav1.ConditionTrue && mr.ObservedGeneration == gen &&
		ma != nil && ma.Status == metav1.ConditionTrue && ma.ObservedGeneration == gen
}

func (r *SnapshotContentController) computeManifestsArchived(ctx context.Context, obj *unstructured.Unstructured, plan *commonContentStatusPlan) error {
	contentLike, err := snapshot.ExtractSnapshotContentLike(obj)
	if err != nil {
		return fmt.Errorf("extract SnapshotContentLike for ManifestsArchived: %w", err)
	}
	cur := snapshot.GetCondition(contentLike, snapshot.ConditionManifestsArchived)
	if cur != nil && cur.Status == metav1.ConditionTrue {
		plan.manifestsArchivedStatus = metav1.ConditionTrue
		plan.manifestsArchivedReason = snapshot.ReasonManifestsArchived
		plan.manifestsArchivedMessage = cur.Message
		return nil
	}
	if cur != nil && cur.Status == metav1.ConditionFalse && cur.Reason == snapshot.ReasonManifestsArchiveFailed {
		plan.manifestsArchivedStatus = metav1.ConditionFalse
		plan.manifestsArchivedReason = snapshot.ReasonManifestsArchiveFailed
		plan.manifestsArchivedMessage = cur.Message
		return nil
	}

	childrenArchived, anyChildFailed, childMessage, err := r.aggregateChildrenManifestsArchived(ctx, obj, plan.manifestsReady == metav1.ConditionTrue)
	if err != nil {
		return err
	}
	switch {
	case plan.manifestsFailed:
		plan.manifestsArchivedStatus = metav1.ConditionFalse
		plan.manifestsArchivedReason = snapshot.ReasonManifestsArchiveFailed
		plan.manifestsArchivedMessage = "own manifest capture failed terminally before archive: " + plan.manifestsMessage
	case anyChildFailed:
		plan.manifestsArchivedStatus = metav1.ConditionFalse
		plan.manifestsArchivedReason = snapshot.ReasonManifestsArchiveFailed
		plan.manifestsArchivedMessage = childMessage
	case plan.manifestsReady == metav1.ConditionTrue && childrenArchived:
		plan.manifestsArchivedStatus = metav1.ConditionTrue
		plan.manifestsArchivedReason = snapshot.ReasonManifestsArchived
		plan.manifestsArchivedMessage = "manifests for this node and all descendants are archived"
	default:
		plan.manifestsArchivedStatus = metav1.ConditionFalse
		plan.manifestsArchivedReason = snapshot.ReasonManifestsCapturing
		if childMessage != "" {
			plan.manifestsArchivedMessage = childMessage
		} else {
			plan.manifestsArchivedMessage = "manifests are being captured: " + plan.manifestsMessage
		}
	}
	return nil
}

// computeResidualSweepGate fills the residual-sweep gate on plan: the LOWEST-priority Ready leg that holds
// the FIRST Ready=True on a namespace-root content until the snapshot reconciler latches the residual/
// orphan-PVC capture wave Complete (status.residualVolumeCapture.phase=Complete). It is fail-closed and
// purely LOCAL: the aggregator never reads the owning Snapshot.
//
// Discriminator (all from this content's OWN object, no owner GET):
//   - A child-volume-node content (LabelChildVolumeNode) models a single PVC and has no residual wave -> open.
//   - Only a namespace-root content (spec.snapshotRef -> a core storage.deckhouse.io Snapshot) carries the
//     residual wave; domain XxxxSnapshot / orphan-VolumeSnapshot-bound contents are never gated -> open.
//   - Upgrade-guard (monotonicity): if Ready is ALREADY persisted True, never re-gate. The gate blocks only
//     the FIRST Ready=True (like ManifestsArchived); on a controller rollout this prevents an already-ready
//     root - whose latch field is still absent because it predates this feature - from being dragged back to
//     False (the very flap this feature removes). On a clean install Ready=True can only have been persisted
//     after the gate opened (latch Complete, monotonic), so the guard merely confirms a valid state.
//
// When none of the above opens the gate, it stays closed until the latch reaches Complete.
func (r *SnapshotContentController) computeResidualSweepGate(obj *unstructured.Unstructured, plan *commonContentStatusPlan) error {
	// Default open: leaf/domain-child/non-root, an already-Ready root, and a Complete latch all leave this True.
	plan.residualSweepStatus = metav1.ConditionTrue
	plan.residualSweepReason = snapshot.ReasonCompleted
	plan.residualSweepMessage = "residual volume capture complete"

	if obj.GetLabels()[snapshot.LabelChildVolumeNode] == "true" {
		return nil
	}
	apiVersion, _, _ := unstructured.NestedString(obj.Object, "spec", "snapshotRef", "apiVersion")
	kind, _, _ := unstructured.NestedString(obj.Object, "spec", "snapshotRef", "kind")
	if kind != "Snapshot" || apiVersion != storagev1alpha1.SchemeGroupVersion.String() {
		return nil
	}

	contentLike, err := snapshot.ExtractSnapshotContentLike(obj)
	if err != nil {
		return fmt.Errorf("extract SnapshotContentLike for residual gate: %w", err)
	}
	if cur := snapshot.GetCondition(contentLike, snapshot.ConditionReady); cur != nil && cur.Status == metav1.ConditionTrue {
		return nil
	}

	phase, _, _ := unstructured.NestedString(obj.Object, "status", "residualVolumeCapture", "phase")
	if phase != storagev1alpha1.ResidualVolumeCapturePhaseComplete {
		plan.residualSweepStatus = metav1.ConditionFalse
		plan.residualSweepReason = snapshot.ReasonResidualVolumeCapturePending
		plan.residualSweepMessage = "waiting for residual/orphan-PVC volume capture wave to complete"
	}
	return nil
}

// aggregateChildrenManifestsArchived reports the children's ManifestsArchived latch state for this
// node: allArchived (every child content ManifestsArchived=True), anyFailed (a child is terminally
// Failed -> the subtree can never be archived), and a progress message. A NotFound or not-yet-archived
// child is pending (not a failure): the subtree is simply not archived yet. This mirrors the child walk
// in validateCommonContentChildren but keys on ManifestsArchived rather than Ready.
//
// Reliability (fail-closed against declared-but-unlinked children): a published-edges-only view
// (status.childrenSnapshotContentRefs) can momentarily see FEWER (or zero) children than the owning
// snapshot declares, because child content edges are published atomically only AFTER every declared
// child snapshot binds (PublishSnapshotContentChildrenFromSnapshotRefs). Latching ManifestsArchived=True
// on that partial view is the root cause of duplicate root captures: a not-yet-linked descendant subtree
// is not reached by the root subtree walk and so its manifests are not excluded. Therefore allArchived
// also requires every DECLARED non-leaf child of the owning snapshot (spec.snapshotRef ->
// status.childrenSnapshotRefs) to be resolved, bound, linked into childrenSnapshotContentRefs, AND
// archived; any unresolved/unbound/unlinked declared child is pending (not a failure). Because the latch
// is one-way (never re-opens), this declared-vs-linked check is the only way to guarantee True implies a
// complete, fully linked subtree.
//
// T-cost: ownManifestReady tells the aggregator whether this node's own manifest leg is Ready. The expensive
// declared-vs-linked walk (uncached owner GET + one uncached resolve per declared child) is deferred until the
// only pass that could actually latch True (ownManifestReady && every linked child archived); see the gate
// below for why deferring it is safe.
func (r *SnapshotContentController) aggregateChildrenManifestsArchived(ctx context.Context, parentContentObj *unstructured.Unstructured, ownManifestReady bool) (bool, bool, string, error) {
	rawRefs, _, err := unstructured.NestedSlice(parentContentObj.Object, "status", "childrenSnapshotContentRefs")
	if err != nil {
		return false, false, "", err
	}
	// linked tracks every published child content edge so the declared-vs-linked check below can detect a
	// declared child that is not yet present in childrenSnapshotContentRefs.
	linked := make(map[string]struct{}, len(rawRefs))
	total := 0
	archivedCount := 0
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
		// missing from the cache is treated as pending (fail-closed), and ManifestsArchived is a one-way
		// latch, so a stale read can only delay the archive, never falsely latch it True.
		if err := r.Client.Get(ctx, client.ObjectKey{Name: name}, childContent); err != nil {
			if errors.IsNotFound(err) {
				pendingNames = append(pendingNames, name)
				continue
			}
			return false, false, "", err
		}
		childLike, err := snapshot.ExtractSnapshotContentLike(childContent)
		if err != nil {
			return false, false, "", err
		}
		cond := snapshot.GetCondition(childLike, snapshot.ConditionManifestsArchived)
		if cond != nil && cond.Status == metav1.ConditionTrue {
			archivedCount++
			continue
		}
		if cond != nil && cond.Status == metav1.ConditionFalse && cond.Reason == snapshot.ReasonManifestsArchiveFailed {
			return false, true, fmt.Sprintf("child snapshot content %s manifests archive failed: %s", name, cond.Message), nil
		}
		pendingNames = append(pendingNames, name)
	}

	// T-cost short-circuit: the expensive declared-vs-linked fail-close (uncached owner GET + one uncached
	// APIReader resolve per declared child) is only ever REQUIRED on the single pass that could latch
	// ManifestsArchived=True — computeManifestsArchived sets True only when this node's own manifest leg is
	// Ready AND every linked child is archived. In any other state (own manifest not yet Ready, or a linked
	// child still pending) the subtree is not archived regardless of the declared set, so skipping the walk
	// cannot produce a false True latch (the one-way invariant is preserved); it only avoids O(children)
	// uncached reads on the many convergence-window reconciles that dominate the tail. Terminal child failure
	// is already detected by the linked-children walk above, independent of this gate.
	if !ownManifestReady || len(pendingNames) > 0 {
		if len(pendingNames) > 0 {
			return false, false, "waiting for child manifests archive: " + formatReadyProgress(archivedCount, total, pendingNames), nil
		}
		return false, false, "", nil
	}

	// Own manifest leg Ready and all linked children archived: run the authoritative declared-vs-linked
	// fail-close (uncached) before latching True — every declared non-leaf child must be linked into the
	// published edge set.
	declaredNames, declaredComplete, err := r.declaredNonLeafChildContentNames(ctx, parentContentObj)
	if err != nil {
		return false, false, "", err
	}
	if !declaredComplete {
		return false, false, "waiting for declared child snapshot graph to bind and link", nil
	}
	var unlinked []string
	for _, dn := range declaredNames {
		if _, ok := linked[dn]; !ok {
			unlinked = append(unlinked, dn)
		}
	}
	if len(unlinked) > 0 {
		return false, false, "waiting for declared child content to be linked: " + strings.Join(unlinked, ", "), nil
	}
	return true, false, "", nil
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
	// A child-volume-node content (orphan/root-residual single PVC, Variant A) is a LEAF by construction:
	// it models a single PVC and never declares child snapshots in its owning snapshot, so its declared
	// non-leaf set is always empty regardless of its spec.snapshotRef. Short-circuiting here also keeps it
	// robust if the referenced orphan VolumeSnapshot is momentarily unobservable (it is durable now, but
	// consulting the ref below on a transient miss would hit the NotFound -> declaredComplete=false
	// fail-close and pin ManifestsArchived=Capturing forever). Treat it as a complete, childless set.
	if contentObj.GetLabels()[snapshot.LabelChildVolumeNode] == "true" {
		return nil, true, nil
	}

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
		if snapshot.IsVolumeSnapshotVisibilityLeaf(ref) {
			continue
		}
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
// VolumesReady from data artifacts. Children are not considered here. Sequencing (v1): the volume leg is
// only evaluated once the manifest leg is Ready; until then VolumesReady stays at its initial
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

	if manifestLegAlreadyLatched(obj) {
		// Cost-cut (Commit C): the manifest leg already reached readiness and the monotonic
		// ManifestsArchived latch is closed for this generation, so re-validating the ManifestCheckpoint
		// (cached MCP Get + adoption patch) and re-checking chunk existence (uncached APIReader GETs) on
		// every 500ms volumes-pending poll cannot change the manifest verdict — the latch never re-opens.
		// Chunk integrity is defense-in-depth re-verified on the read/download/archive path, not on this
		// readiness gate, so skipping it here is safe and removes the dominant per-reconcile cost while a
		// leaf waits on its volume leg. The volume leg below is still evaluated every pass.
		plan.manifestsReady = metav1.ConditionTrue
		plan.manifestsReason = snapshot.ReasonCompleted
		plan.manifestsMessage = "manifest is ready"
	} else {
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
	}

	// Volume leg: evaluated only after the manifest leg is Ready (kept sequencing, v1).
	dataReady, dataReason, dataMessage, err := r.resolveDataReadiness(ctx, obj)
	if err != nil {
		return err
	}
	if !dataReady {
		plan.volumesReady = metav1.ConditionFalse
		plan.volumesReason = dataReason
		plan.volumesMessage = dataMessage
		plan.volumesFailed = isTerminalDataFailure(dataReason)
		return nil
	}

	plan.volumesReady = metav1.ConditionTrue
	plan.volumesReason = snapshot.ReasonCompleted
	plan.volumesMessage = "data is ready"
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
	// ManifestsArchiveFailed is a terminal subtree-latch failure: if a child's subtree can never be
	// archived, the parent's cannot either. Today a child never surfaces it on Ready (the archive gate only
	// reports the transient ManifestsCapturing — see buildCommonSnapshotContentStatusPlan), so this entry is
	// defense-in-depth that keeps terminal propagation correct if the Ready priority ever changes.
	snapshot.ReasonManifestsArchiveFailed: {},
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
func (r *SnapshotContentController) validateCommonContentChildren(ctx context.Context, parentContentObj *unstructured.Unstructured) (bool, string, string, error) {
	rawRefs, _, err := unstructured.NestedSlice(parentContentObj.Object, "status", "childrenSnapshotContentRefs")
	if err != nil {
		return false, "", "", err
	}
	total := 0
	readyCount := 0
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
	if total == 0 {
		return true, "", "no child content", nil
	}
	return true, "", fmt.Sprintf("%d/%d child content ready", readyCount, total), nil
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
		if err := r.Get(ctx, client.ObjectKey{Name: childName}, child); err != nil {
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
		if err := r.Get(ctx, client.ObjectKey{Name: mcpName}, mcp); err != nil {
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
		return r.Patch(ctx, mcp, client.MergeFrom(base))
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
		vsc.SetGroupVersionKind(artifactGVK())
		if getErr := r.Get(ctx, client.ObjectKey{Name: art.Name}, vsc); getErr != nil {
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
	if r.cacheReader == nil {
		r.cacheReader = mgr.GetCache()
	}
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
	built, err := builder.Build(r)
	if err != nil {
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
	if contentGVK == unifiedbootstrap.CommonSnapshotContentGVK() {
		r.primaryContentController = built
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
	// Reverse-lookup Lists must read from the cache (indexed informers), not the client (uncached
	// unstructured → API-server field selector the server rejects). See reverseLookupReader.
	r.cacheReader = mgr.GetCache()
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
		// L9a: index this content GVK by status.manifestCheckpointName so the ManifestCheckpoint
		// wake-up can reverse-resolve the owning content before adoption (pre-ownerRef). Registered
		// once per GVK (the activeContentWatchSet guard above makes this body run once).
		if err := mgr.GetFieldIndexer().IndexField(context.Background(), obj.DeepCopy(), indexKeyManifestCheckpointName, extractManifestCheckpointNameIndex); err != nil {
			return fmt.Errorf("failed to register manifestCheckpointName index for SnapshotContent GVK %s: %w", gvk.String(), err)
		}
		// C-2: index published child content edges so a child status change can reverse-resolve and wake
		// its parent(s) event-driven (mapChildContentToParentContentsByEdge), removing the archive wave's
		// dependence on the parent's 500ms self-requeue.
		if err := mgr.GetFieldIndexer().IndexField(context.Background(), obj.DeepCopy(), indexKeyChildContentName, extractChildContentNamesIndex); err != nil {
			return fmt.Errorf("failed to register childrenSnapshotContentRefs index for SnapshotContent GVK %s: %w", gvk.String(), err)
		}
		// Commit C: index the published VSC data edge (status.dataRef.artifact.name) so a
		// VolumeSnapshotContent readyToUse flip can reverse-resolve and wake the owning leaf content
		// event-driven (mapVolumeSnapshotContentToContent) even when the flip preceded the ownerRef handoff.
		if err := mgr.GetFieldIndexer().IndexField(context.Background(), obj.DeepCopy(), indexKeyDataRefArtifactName, extractDataRefArtifactNameIndex); err != nil {
			return fmt.Errorf("failed to register dataRef artifact name index for SnapshotContent GVK %s: %w", gvk.String(), err)
		}
		builder := ctrl.NewControllerManagedBy(mgr).
			For(obj).
			WithOptions(snapshotContentControllerOptions()).
			// Dual-path child→parent wake-up: ownerRef handoff (mapSnapshotContentToParentContent) PLUS the
			// forward-edge reverse-lookup (mapChildContentToParentContentsByEdge). The ownerRef event is
			// droppable/late (set after the child may already be terminal); the published-edge index closes
			// that gap so parent archive re-evaluation is event-driven, not 500ms-poll-driven (C-2).
			Watches(obj, handler.EnqueueRequestsFromMapFunc(mapSnapshotContentToParentContent)).
			Watches(obj, handler.EnqueueRequestsFromMapFunc(r.mapChildContentToParentContentsByEdge)).
			Named(fmt.Sprintf("snapshotcontent-%s-%s", gvk.Group, gvk.Kind))
		// Damaged-artifact wake-up (Phase 2a): enqueue the owning SnapshotContent when its durable
		// MCP/VSC artifacts change. Guarded by RESTMapping so a not-yet-installed CRD (e.g.
		// VolumeSnapshotContent under envtest) degrades to "no watch" instead of failing startup;
		// revalidation on the next reconcile still recomputes state (INV-RECONCILE-TRUTH).
		builder = r.addArtifactWakeUpWatches(builder)
		// Build (== Complete + return the controller handle) so the common-SnapshotContent controller can be
		// retained for AddSnapshotStatusWatch to attach snapshot-status wakes as additional Watches on THIS
		// single controller, instead of building a second Controller per Snapshot GVK.
		built, err := builder.Build(r)
		if err != nil {
			return fmt.Errorf("failed to setup watch for SnapshotContent GVK %s: %w", gvk.String(), err)
		}
		if gvk == unifiedbootstrap.CommonSnapshotContentGVK() {
			r.primaryContentController = built
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
		// Both durable artifacts use a dual-path resolver (ownerRef + pre-adoption reverse-lookup) so a
		// status flip observed before the ownerRef handoff still wakes the owning content: ManifestCheckpoint
		// by status.manifestCheckpointName (L9a), VolumeSnapshotContent by status.dataRef.artifact.name
		// (Commit C). Any other artifact falls back to the ownerRef-only resolver.
		mapper := mapArtifactToOwningSnapshotContent
		switch gvk.Kind {
		case "ManifestCheckpoint":
			mapper = r.mapManifestCheckpointToContent
		case kindVolumeSnapshotContent:
			mapper = r.mapVolumeSnapshotContentToContent
		}
		b = b.Watches(artifactObj, handler.EnqueueRequestsFromMapFunc(mapper))
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
	if r.primaryContentController == nil {
		return fmt.Errorf("primary SnapshotContent controller not initialized before snapshot status watch %s (SetupWithManager must run first)", snapshotGVK.String())
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(snapshotGVK)
	// Attach the snapshot-status wake as an additional event source on the SINGLE primary SnapshotContent
	// controller. Controller.Watch supports adding sources before OR after Start, so this preserves the
	// registry-driven / dynamic-CSD activation model while eliminating the former per-GVK second Controller
	// (which duplicated reconciliation of the same SnapshotContent object and caused 409 write conflicts).
	// The enqueue mapping (status.boundSnapshotContentName -> bound content) is unchanged.
	src := source.Kind(mgr.GetCache(), client.Object(obj), handler.EnqueueRequestsFromMapFunc(mapSnapshotStatusToBoundCommonContent))
	if err := r.primaryContentController.Watch(src); err != nil {
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

// manifestCheckpointGVK / volumeSnapshotContentGVK are the durable top-level artifacts whose
// create/update/delete events must wake the owning SnapshotContent (INV-OWNCHAIN). Routing is strictly
// by the artifact's ownerRef -> SnapshotContent; no reverse-index by dataRefs[] and no chunk shortcut.
func artifactWakeUpGVKs() []schema.GroupVersionKind {
	return []schema.GroupVersionKind{
		{Group: ssv1alpha1.SchemeGroupVersion.Group, Version: ssv1alpha1.SchemeGroupVersion.Version, Kind: "ManifestCheckpoint"},
		{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotContent"},
	}
}

// ownerRefToContentRequests returns the enqueue request for the SnapshotContent that controller-owns the
// artifact via ownerRef, or nil if the artifact carries no SnapshotContent ownerRef yet. Read-only; never
// writes. Tombstone/last-known objects on delete still carry ownerRefs, so routing works.
func ownerRefToContentRequests(obj client.Object) []reconcile.Request {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.APIVersion == storagev1alpha1.SchemeGroupVersion.String() && ref.Kind == "SnapshotContent" && ref.Name != "" {
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: ref.Name}}}
		}
	}
	return nil
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
	if reqs := ownerRefToContentRequests(obj); reqs != nil {
		return reqs
	}
	log.FromContext(ctx).V(1).Info(
		"artifact event has no owning SnapshotContent ownerRef; dropping (revalidation backstops on next reconcile)",
		"artifactKind", obj.GetObjectKind().GroupVersionKind().Kind,
		"artifact", obj.GetName(),
	)
	return nil
}

// mapManifestCheckpointToContent is the dual-path artifact resolver for ManifestCheckpoint wake-up. It
// only enqueues reconciles; it NEVER changes ownership or state.
//
//	path 1 (ownerRef): once a SnapshotContent has adopted the MCP (handoff added the SnapshotContent
//	  ownerRef), route by that ownerRef — same as every other artifact.
//	path 2 (pre-adoption reverse lookup): before adoption the MCP is controller-owned by the execution
//	  ObjectKeeper, so path 1 finds nothing. Resolve the owning content by the deterministic 1:1 link
//	  content.status.manifestCheckpointName == mcp.Name (field index indexKeyManifestCheckpointName).
//
// This breaks the wake-up⇄adoption cycle that previously forced the content to discover a Ready MCP only
// on its 500ms self-requeue. Safety: a stale/missing index entry can only mis-time a wake-up (spurious
// enqueue → idempotent no-op reconcile, or missed enqueue → the 500ms self-requeue still backstops); it
// can never produce a wrong owner because adoption logic is untouched.
func (r *SnapshotContentController) mapManifestCheckpointToContent(ctx context.Context, obj client.Object) []reconcile.Request {
	if obj == nil {
		return nil
	}
	if reqs := ownerRefToContentRequests(obj); reqs != nil {
		return reqs
	}
	if reqs := r.lookupContentsByManifestCheckpointName(ctx, obj.GetName()); len(reqs) > 0 {
		return reqs
	}
	log.FromContext(ctx).V(1).Info(
		"ManifestCheckpoint event resolved to no SnapshotContent (no ownerRef, no name match); dropping (self-requeue backstops)",
		"manifestCheckpoint", obj.GetName(),
	)
	return nil
}

// reverseLookupReader returns the reader for the enqueue-only reverse-lookup Lists (MCP/VSC/child → owning
// or parent SnapshotContent). It MUST be the manager cache so client.MatchingFields resolves via the
// registered indexKey* field index instead of an API-server field selector the server rejects for custom
// status fields ("field label not supported"). Falls back to r.Client when the cache handle is unset (unit
// tests wire an indexed fake client as Client). These reads only enqueue reconcile.Requests and are fully
// backstopped by the 500ms self-requeue, so an eventually-consistent cache read is safe here.
func (r *SnapshotContentController) reverseLookupReader() client.Reader {
	if r.cacheReader != nil {
		return r.cacheReader
	}
	return r.Client
}

// lookupContentsByManifestCheckpointName resolves the SnapshotContent(s) whose published
// status.manifestCheckpointName equals mcpName, via the cache field index. The link is 1:1 (the name is
// derived from the per-content MCR UID), so this normally returns a single request. Read-only; a List
// error degrades to the self-requeue backstop rather than failing the wake-up.
func (r *SnapshotContentController) lookupContentsByManifestCheckpointName(ctx context.Context, mcpName string) []reconcile.Request {
	if mcpName == "" {
		return nil
	}
	var out []reconcile.Request
	seen := make(map[string]struct{})
	for _, gvk := range r.SnapshotContentGVKs {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(schema.GroupVersionKind{Group: gvk.Group, Version: gvk.Version, Kind: gvk.Kind + "List"})
		if err := r.reverseLookupReader().List(ctx, list, client.MatchingFields{indexKeyManifestCheckpointName: mcpName}); err != nil {
			log.FromContext(ctx).V(1).Info("reverse lookup of SnapshotContent by manifestCheckpointName failed; self-requeue backstops",
				"manifestCheckpoint", mcpName, "gvk", gvk.String(), "err", err.Error())
			continue
		}
		for i := range list.Items {
			name := list.Items[i].GetName()
			if name == "" {
				continue
			}
			if _, dup := seen[name]; dup {
				continue
			}
			seen[name] = struct{}{}
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
		}
	}
	return out
}

// mapVolumeSnapshotContentToContent is the dual-path artifact resolver for VolumeSnapshotContent wake-up
// (Commit C), mirroring mapManifestCheckpointToContent. It only enqueues reconciles; it NEVER changes
// ownership or state.
//
//	path 1 (ownerRef): once the SnapshotContent has adopted the VSC (handoff reparented the ownerRef from
//	  the execution ObjectKeeper to the SnapshotContent), route by that ownerRef — same as every artifact.
//	path 2 (data-edge reverse lookup): before/independently of adoption, resolve the owning content by the
//	  durable published edge content.status.dataRef.artifact.name == vsc.Name (indexKeyDataRefArtifactName).
//
// This closes the wake gap where a VSC flips status.readyToUse=true before the ownerRef handoff (the event
// was previously dropped, forcing the content to discover readiness only on its 500ms self-requeue). Safety:
// a stale/missing index entry can only mis-time a wake-up (spurious enqueue → idempotent no-op reconcile, or
// missed enqueue → the 500ms self-requeue still backstops); it can never produce a wrong owner because
// adoption logic is untouched.
func (r *SnapshotContentController) mapVolumeSnapshotContentToContent(ctx context.Context, obj client.Object) []reconcile.Request {
	if obj == nil {
		return nil
	}
	if reqs := ownerRefToContentRequests(obj); reqs != nil {
		return reqs
	}
	if reqs := r.lookupContentsByDataRefArtifactName(ctx, obj.GetName()); len(reqs) > 0 {
		return reqs
	}
	log.FromContext(ctx).V(1).Info(
		"VolumeSnapshotContent event resolved to no SnapshotContent (no ownerRef, no dataRef match); dropping (self-requeue backstops)",
		"volumeSnapshotContent", obj.GetName(),
	)
	return nil
}

// lookupContentsByDataRefArtifactName resolves the SnapshotContent(s) whose published
// status.dataRef.artifact.name equals vscName, via the cache field index. The link is 1:1 (a content binds a
// single VSC data leg), so this normally returns a single request. Read-only; a List error degrades to the
// self-requeue backstop rather than failing the wake-up.
func (r *SnapshotContentController) lookupContentsByDataRefArtifactName(ctx context.Context, vscName string) []reconcile.Request {
	if vscName == "" {
		return nil
	}
	var out []reconcile.Request
	seen := make(map[string]struct{})
	for _, gvk := range r.SnapshotContentGVKs {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(schema.GroupVersionKind{Group: gvk.Group, Version: gvk.Version, Kind: gvk.Kind + "List"})
		if err := r.reverseLookupReader().List(ctx, list, client.MatchingFields{indexKeyDataRefArtifactName: vscName}); err != nil {
			log.FromContext(ctx).V(1).Info("reverse lookup of SnapshotContent by dataRef artifact failed; self-requeue backstops",
				"volumeSnapshotContent", vscName, "gvk", gvk.String(), "err", err.Error())
			continue
		}
		for i := range list.Items {
			name := list.Items[i].GetName()
			if name == "" {
				continue
			}
			if _, dup := seen[name]; dup {
				continue
			}
			seen[name] = struct{}{}
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
		}
	}
	return out
}

// extractDataRefArtifactNameIndex is the field-index extractor for indexKeyDataRefArtifactName: it projects a
// SnapshotContent's published status.dataRef.artifact.name when that artifact is a VolumeSnapshotContent, so
// the VSC dual-path wake-up (path 2) can find the owning content by its durable data edge independently of
// the ownerRef handoff. A non-VSC artifact or an empty/absent name yields no index entry.
func extractDataRefArtifactNameIndex(obj client.Object) []string {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil
	}
	kind, _, err := unstructured.NestedString(u.Object, "status", "dataRef", "artifact", "kind")
	if err != nil || kind != kindVolumeSnapshotContent {
		return nil
	}
	name, found, err := unstructured.NestedString(u.Object, "status", "dataRef", "artifact", "name")
	if err != nil || !found || name == "" {
		return nil
	}
	return []string{name}
}

// extractManifestCheckpointNameIndex is the field-index extractor for indexKeyManifestCheckpointName: it
// projects a SnapshotContent's status.manifestCheckpointName so the MCP reverse-lookup (path 2) can find
// the owning content before adoption. Empty/absent yields no index entry.
func extractManifestCheckpointNameIndex(obj client.Object) []string {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil
	}
	name, found, err := unstructured.NestedString(u.Object, "status", "manifestCheckpointName")
	if err != nil || !found || name == "" {
		return nil
	}
	return []string{name}
}

// extractChildContentNamesIndex is the field-index extractor for indexKeyChildContentName: it projects every
// name in a SnapshotContent's status.childrenSnapshotContentRefs so the forward-edge reverse-lookup (C-2) can
// find the PARENT content(s) that aggregate a given child. A parent with no published child edges yields no
// index entry.
func extractChildContentNamesIndex(obj client.Object) []string {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil
	}
	refs, found, err := unstructured.NestedSlice(u.Object, "status", "childrenSnapshotContentRefs")
	if err != nil || !found || len(refs) == 0 {
		return nil
	}
	out := make([]string, 0, len(refs))
	for _, raw := range refs {
		m, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if name, _ := m["name"].(string); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// mapChildContentToParentContentsByEdge is the forward-edge reverse-lookup wake-up (C-2). On any
// SnapshotContent event it enqueues every parent content whose published status.childrenSnapshotContentRefs
// includes the changed object's name, so a child's ManifestsArchived/Ready/leg transition wakes its parent
// immediately (event-driven) rather than waiting for the parent's 500ms self-requeue archive wave. It only
// enqueues reconcile.Requests; it never writes state. A stale/missing index entry can only mis-time a
// wake-up (spurious enqueue → idempotent no-op reconcile, or missed enqueue → the 500ms self-requeue still
// backstops); it can never change the aggregation result, which is recomputed from truth on every reconcile.
func (r *SnapshotContentController) mapChildContentToParentContentsByEdge(ctx context.Context, obj client.Object) []reconcile.Request {
	if obj == nil || obj.GetName() == "" {
		return nil
	}
	childName := obj.GetName()
	var out []reconcile.Request
	seen := make(map[string]struct{})
	for _, gvk := range r.SnapshotContentGVKs {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(schema.GroupVersionKind{Group: gvk.Group, Version: gvk.Version, Kind: gvk.Kind + "List"})
		if err := r.reverseLookupReader().List(ctx, list, client.MatchingFields{indexKeyChildContentName: childName}); err != nil {
			log.FromContext(ctx).V(1).Info("forward-edge reverse lookup of parent SnapshotContent failed; self-requeue backstops",
				"childContent", childName, "gvk", gvk.String(), "err", err.Error())
			continue
		}
		for i := range list.Items {
			parentName := list.Items[i].GetName()
			if parentName == "" || parentName == childName {
				continue
			}
			if _, dup := seen[parentName]; dup {
				continue
			}
			seen[parentName] = struct{}{}
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Name: parentName}})
		}
	}
	return out
}
