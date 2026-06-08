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
	"fmt"
	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/common"
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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
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
		err = r.APIReader.Get(ctx, contentKey, obj)
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
			logger.Error(err, "Failed to cascade remove finalizers from children")
			// Non-fatal: continue with finalizer removal
		}

		// Step 2.1.1: Remove finalizers from linked artifacts (MCP/VSC)
		if mcpName := contentLike.GetStatusManifestCheckpointName(); mcpName != "" {
			if err := r.removeArtifactFinalizer(ctx, "ManifestCheckpoint", mcpName, "state-snapshotter.deckhouse.io/v1alpha1"); err != nil {
				logger.Error(err, "Failed to remove ManifestCheckpoint finalizer", "mcp", mcpName)
			}
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
	// condition model: RequestsReady + ChildrenReady + derived Ready = RequestsReady && ChildrenReady
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
	if !ready {
		return ctrl.Result{RequeueAfter: defaultSnapshotContentRequeueAfter}, nil
	}

	logger.Info("SnapshotContent reconciliation completed")
	return ctrl.Result{}, nil
}

func isCommonSnapshotContentGVK(gvk schema.GroupVersionKind) bool {
	return gvk == unifiedbootstrap.CommonSnapshotContentGVK()
}

// commonContentStatusPlan is the SnapshotContent aggregation outcome. It carries the two sub-legs
// (RequestsReady = own MCP + data; ChildrenReady = child SnapshotContents) and the derived Ready.
// Ready is the single aggregate on SnapshotContent: Ready = RequestsReady && ChildrenReady (INV-COND2).
type commonContentStatusPlan struct {
	requestsReady   metav1.ConditionStatus
	requestsReason  string
	requestsMessage string
	requestsFailed  bool // terminal (vs pending) when requestsReady != True

	childrenReady   metav1.ConditionStatus
	childrenReason  string
	childrenMessage string
	childrenFailed  bool // terminal (vs pending) when childrenReady != True

	readyStatus  metav1.ConditionStatus
	readyReason  string
	readyMessage string
}

func (r *SnapshotContentController) reconcileCommonSnapshotContentStatus(ctx context.Context, obj *unstructured.Unstructured) (bool, error) {
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

	gen := obj.GetGeneration()
	desired := []metav1.Condition{
		{Type: snapshot.ConditionRequestsReady, Status: plan.requestsReady, Reason: plan.requestsReason, Message: plan.requestsMessage, ObservedGeneration: gen},
		{Type: snapshot.ConditionChildrenReady, Status: plan.childrenReady, Reason: plan.childrenReason, Message: plan.childrenMessage, ObservedGeneration: gen},
		{Type: snapshot.ConditionReady, Status: plan.readyStatus, Reason: plan.readyReason, Message: plan.readyMessage, ObservedGeneration: gen},
	}

	changed := false
	for _, d := range desired {
		if upsertContentCondition(contentLike, d) {
			changed = true
		}
	}

	if !changed {
		return plan.readyStatus == metav1.ConditionTrue, nil
	}
	obj.Object["status"] = statusMap
	snapshot.SyncConditionsToUnstructured(obj, contentLike.GetStatusConditions())
	if err := r.Status().Update(ctx, obj); err != nil {
		if errors.IsConflict(err) {
			return false, nil
		}
		return false, err
	}
	return plan.readyStatus == metav1.ConditionTrue, nil
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

func (r *SnapshotContentController) ReconcileCommonSnapshotContentStatus(ctx context.Context, obj *unstructured.Unstructured) (bool, error) {
	return r.reconcileCommonSnapshotContentStatus(ctx, obj)
}

// buildCommonSnapshotContentStatusPlan computes RequestsReady, ChildrenReady and the derived Ready.
// Ready priority (single reason when several legs are not satisfied):
//
//	RequestsFailed > ChildrenFailed > RequestsPending > ChildrenPending > Completed
//
// Terminal failures win over pending (actionable first) and the node's own leg wins over children at
// equal severity. DomainReady is NOT part of this formula; it is only a gate/barrier upstream.
func (r *SnapshotContentController) buildCommonSnapshotContentStatusPlan(ctx context.Context, obj *unstructured.Unstructured) (commonContentStatusPlan, error) {
	plan := commonContentStatusPlan{
		requestsReady:   metav1.ConditionFalse,
		requestsReason:  snapshot.ReasonManifestCapturePending,
		requestsMessage: "waiting for manifest capture",
		childrenReady:   metav1.ConditionTrue,
		childrenReason:  snapshot.ReasonCompleted,
		childrenMessage: "no child content or all child content ready",
	}

	if err := r.fillRequestsLeg(ctx, obj, &plan); err != nil {
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
	}

	switch {
	case plan.requestsReady != metav1.ConditionTrue && plan.requestsFailed:
		plan.readyStatus = metav1.ConditionFalse
		plan.readyReason = plan.requestsReason
		plan.readyMessage = plan.requestsMessage
	case plan.childrenReady != metav1.ConditionTrue && plan.childrenFailed:
		plan.readyStatus = metav1.ConditionFalse
		plan.readyReason = plan.childrenReason
		plan.readyMessage = plan.childrenMessage
	case plan.requestsReady != metav1.ConditionTrue:
		plan.readyStatus = metav1.ConditionFalse
		plan.readyReason = plan.requestsReason
		plan.readyMessage = plan.requestsMessage
	case plan.childrenReady != metav1.ConditionTrue:
		plan.readyStatus = metav1.ConditionFalse
		plan.readyReason = plan.childrenReason
		plan.readyMessage = plan.childrenMessage
	default:
		plan.readyStatus = metav1.ConditionTrue
		plan.readyReason = snapshot.ReasonCompleted
		plan.readyMessage = "manifest, data, and child content are ready"
	}
	return plan, nil
}

// terminalRequestsFailureReasons lists requests-leg Ready=False reasons treated as terminal (vs pending).
// ArtifactNotReady (VSC not yet readyToUse) and ManifestCapturePending are pending; the rest are terminal.
var terminalRequestsFailureReasons = map[string]struct{}{
	snapshot.ReasonManifestCheckpointFailed: {},
	snapshot.ReasonDataArtifactInvalid:      {},
	snapshot.ReasonDataArtifactNotSupported: {},
	snapshot.ReasonArtifactMissing:          {},
}

func isTerminalRequestsFailure(reason string) bool {
	_, ok := terminalRequestsFailureReasons[reason]
	return ok
}

// fillRequestsLeg evaluates this node's own requests (ManifestCheckpoint + data artifacts) into the
// RequestsReady leg of plan. Children are not considered here. Data is only evaluated once the MCP is
// Ready (preserves the prior sequencing where data refs are published alongside capture completion).
func (r *SnapshotContentController) fillRequestsLeg(ctx context.Context, obj *unstructured.Unstructured, plan *commonContentStatusPlan) error {
	mcpName, _, err := unstructured.NestedString(obj.Object, "status", "manifestCheckpointName")
	if err != nil {
		return err
	}
	if mcpName == "" {
		plan.requestsReady = metav1.ConditionFalse
		plan.requestsReason = snapshot.ReasonManifestCapturePending
		plan.requestsMessage = "waiting for manifest capture checkpoint to be published"
		return nil
	}

	mcpReady, mcpFailed, mcpMessage, err := r.validateCommonContentManifestCheckpoint(ctx, obj, mcpName)
	if err != nil {
		return err
	}
	if mcpFailed {
		plan.requestsReady = metav1.ConditionFalse
		plan.requestsFailed = true
		plan.requestsReason = snapshot.ReasonManifestCheckpointFailed
		plan.requestsMessage = mcpMessage
		return nil
	}
	if !mcpReady {
		plan.requestsReady = metav1.ConditionFalse
		plan.requestsReason = snapshot.ReasonManifestCapturePending
		plan.requestsMessage = mcpMessage
		return nil
	}

	dataReady, dataReason, dataMessage, err := r.resolveDataReadiness(ctx, obj)
	if err != nil {
		return err
	}
	if !dataReady {
		plan.requestsReady = metav1.ConditionFalse
		plan.requestsReason = dataReason
		plan.requestsMessage = dataMessage
		plan.requestsFailed = isTerminalRequestsFailure(dataReason)
		return nil
	}

	plan.requestsReady = metav1.ConditionTrue
	plan.requestsReason = snapshot.ReasonCompleted
	plan.requestsMessage = "manifest and data are ready"
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
		if err := r.APIReader.Get(ctx, client.ObjectKey{Name: name}, childContent); err != nil {
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
	return true, "", "", nil
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
// validation belongs to the explicit read/download/archive path, not to every reconcile. Each chunk is
// fetched metadata-only (PartialObjectMetadata) so .spec.data is never transferred. It uses APIReader
// (direct, uncached) on purpose: a cached Get would force the controller cache to start a
// ManifestCheckpointContentChunk informer (implicit list/watch), which Phase 2a avoids (chunks keep
// get-only RBAC). It stops at the first chunk that is NotFound and returns its name; transient
// (non-NotFound) errors are returned so reconcile requeues instead of falsely failing the tree.
func (r *SnapshotContentController) firstMissingManifestCheckpointChunk(ctx context.Context, mcpName string) (string, error) {
	mcp := &ssv1alpha1.ManifestCheckpoint{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: mcpName}, mcp); err != nil {
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
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: mcpName}, mcp); err != nil {
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
