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
	"fmt"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
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
		if dataRef := contentLike.GetStatusDataRef(); dataRef != nil && dataRef.Kind == "VolumeSnapshotContent" {
			if err := r.removeArtifactFinalizer(ctx, "VolumeSnapshotContent", dataRef.Name, "snapshot.storage.k8s.io/v1"); err != nil {
				logger.Error(err, "Failed to remove VolumeSnapshotContent finalizer", "vsc", dataRef.Name)
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
	if isCommonSnapshotContentGVK(obj.GroupVersionKind()) {
		ready, err := r.reconcileCommonSnapshotContentStatus(ctx, obj)
		if err != nil {
			logger.Error(err, "Failed to reconcile common SnapshotContent status")
			return ctrl.Result{}, err
		}
		if !ready {
			return ctrl.Result{RequeueAfter: defaultDemoSnapshotRequeueAfter}, nil
		}
	} else {
		if err := r.checkConsistencyAndSetReady(ctx, contentLike, obj); err != nil {
			logger.Error(err, "Failed to check consistency")
			// Non-fatal: continue reconciliation
		}
	}

	logger.Info("SnapshotContent reconciliation completed")
	return ctrl.Result{}, nil
}

func isCommonSnapshotContentGVK(gvk schema.GroupVersionKind) bool {
	return gvk == unifiedbootstrap.CommonSnapshotContentGVK()
}

type commonContentStatusPlan struct {
	readyStatus  metav1.ConditionStatus
	readyReason  string
	readyMessage string
}

func (r *SnapshotContentController) reconcileCommonSnapshotContentStatus(ctx context.Context, obj *unstructured.Unstructured) (bool, error) {
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

	changed := false
	desired := metav1.Condition{
		Type:               snapshot.ConditionReady,
		Status:             plan.readyStatus,
		Reason:             plan.readyReason,
		Message:            plan.readyMessage,
		ObservedGeneration: obj.GetGeneration(),
	}
	if cur := snapshot.GetCondition(contentLike, snapshot.ConditionReady); cur == nil ||
		cur.Status != desired.Status || cur.Reason != desired.Reason || cur.Message != desired.Message || cur.ObservedGeneration != desired.ObservedGeneration {
		snapshot.SetCondition(contentLike, snapshot.ConditionReady, desired.Status, desired.Reason, desired.Message)
		conditions := contentLike.GetStatusConditions()
		for i := range conditions {
			if conditions[i].Type == snapshot.ConditionReady {
				conditions[i].ObservedGeneration = desired.ObservedGeneration
			}
		}
		obj.Object["status"] = statusMap
		snapshot.SyncConditionsToUnstructured(obj, conditions)
		changed = true
	}

	if !changed {
		return plan.readyStatus == metav1.ConditionTrue, nil
	}
	obj.Object["status"] = statusMap
	if err := r.Status().Update(ctx, obj); err != nil {
		if errors.IsConflict(err) {
			return false, nil
		}
		return false, err
	}
	return plan.readyStatus == metav1.ConditionTrue, nil
}

func (r *SnapshotContentController) buildCommonSnapshotContentStatusPlan(ctx context.Context, obj *unstructured.Unstructured) (commonContentStatusPlan, error) {
	plan := commonContentStatusPlan{
		readyStatus:  metav1.ConditionFalse,
		readyReason:  snapshot.ReasonManifestCapturePending,
		readyMessage: "waiting for manifest capture",
	}

	mcpName, _, err := unstructured.NestedString(obj.Object, "status", "manifestCheckpointName")
	if err != nil {
		return plan, err
	}
	if mcpName == "" {
		plan.readyMessage = "waiting for SnapshotContent.status.manifestCheckpointName"
		return plan, nil
	}

	mcpReady, mcpFailed, mcpMessage, err := r.validateCommonContentManifestCheckpoint(ctx, obj, mcpName)
	if err != nil {
		return plan, err
	}
	if mcpFailed {
		plan.readyReason = "ManifestCheckpointFailed"
		plan.readyMessage = mcpMessage
		return plan, nil
	}
	if !mcpReady {
		plan.readyReason = snapshot.ReasonManifestCapturePending
		plan.readyMessage = mcpMessage
		return plan, nil
	}

	dataReady, dataReason, dataMessage, err := r.resolveDataReadiness(ctx, obj)
	if err != nil {
		return plan, err
	}
	if !dataReady {
		plan.readyReason = dataReason
		plan.readyMessage = dataMessage
		return plan, nil
	}
	childrenReady, childReason, childMessage, err := r.validateCommonContentChildren(ctx, obj)
	if err != nil {
		return plan, err
	}
	if !childrenReady {
		plan.readyReason = childReason
		plan.readyMessage = childMessage
		return plan, nil
	}

	plan.readyStatus = metav1.ConditionTrue
	plan.readyReason = snapshot.ReasonCompleted
	plan.readyMessage = "manifest, data, and child content are ready"
	return plan, nil
}

func (r *SnapshotContentController) validateCommonContentChildren(ctx context.Context, parentContentObj *unstructured.Unstructured) (bool, string, string, error) {
	rawRefs, _, err := unstructured.NestedSlice(parentContentObj.Object, "status", "childrenSnapshotContentRefs")
	if err != nil {
		return false, "", "", err
	}
	for _, raw := range rawRefs {
		refMap, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := refMap["name"].(string)
		if name == "" {
			continue
		}
		childContent := &unstructured.Unstructured{}
		childContent.SetGroupVersionKind(unifiedbootstrap.CommonSnapshotContentGVK())
		if err := r.APIReader.Get(ctx, client.ObjectKey{Name: name}, childContent); err != nil {
			if errors.IsNotFound(err) {
				return false, snapshot.ReasonChildSnapshotPending, fmt.Sprintf("child SnapshotContent %s not found", name), nil
			}
			return false, "", "", err
		}
		childLike, err := snapshot.ExtractSnapshotContentLike(childContent)
		if err != nil {
			return false, "", "", err
		}
		if !snapshot.IsReady(childLike) {
			readyCond := snapshot.GetCondition(childLike, snapshot.ConditionReady)
			if readyCond != nil && readyCond.Status == metav1.ConditionFalse && readyCond.Reason == snapshot.ReasonChildSnapshotFailed {
				return false, snapshot.ReasonChildSnapshotFailed, readyCond.Message, nil
			}
			return false, snapshot.ReasonChildSnapshotPending, fmt.Sprintf("child SnapshotContent %s is not Ready", name), nil
		}
	}
	return true, "", "", nil
}

func (r *SnapshotContentController) ensureChildSnapshotContentOwnedByParent(ctx context.Context, childName string, parentContentObj *unstructured.Unstructured) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		child := &storagev1alpha1.SnapshotContent{}
		if err := r.Client.Get(ctx, client.ObjectKey{Name: childName}, child); err != nil {
			return err
		}
		ownerRef := metav1.OwnerReference{
			APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
			Kind:       "SnapshotContent",
			Name:       parentContentObj.GetName(),
			UID:        parentContentObj.GetUID(),
			Controller: func() *bool { b := true; return &b }(),
		}
		refs, changed, err := snapshotContentControllerOwnerRefsForHandoff(child.OwnerReferences, ownerRef)
		if err != nil {
			return fmt.Errorf("child SnapshotContent %s: %w", childName, err)
		}
		if !changed {
			return nil
		}
		base := child.DeepCopy()
		child.OwnerReferences = refs
		return r.Client.Patch(ctx, child, client.MergeFrom(base))
	})
}

func (r *SnapshotContentController) validateCommonContentManifestCheckpoint(ctx context.Context, contentObj *unstructured.Unstructured, mcpName string) (bool, bool, string, error) {
	resolvedMCPName, ready, failed, msg, err := r.resolveManifestCheckpointReady(ctx, mcpName)
	if err != nil {
		return false, false, "", err
	}
	if ready {
		if err := r.ensureManifestCheckpointOwnedByContent(ctx, resolvedMCPName, contentObj); err != nil {
			return false, false, "", err
		}
	}
	return ready, failed, msg, nil
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
	return out, !ownerReferencesEqual(existing, out), nil
}

func (r *SnapshotContentController) resolveManifestCheckpointReady(ctx context.Context, mcpName string) (string, bool, bool, string, error) {
	mcp := &ssv1alpha1.ManifestCheckpoint{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: mcpName}, mcp); err != nil {
		if errors.IsNotFound(err) {
			return mcpName, false, false, fmt.Sprintf("waiting for ManifestCheckpoint %s", mcpName), nil
		}
		return "", false, false, "", err
	}
	cond := meta.FindStatusCondition(mcp.Status.Conditions, ssv1alpha1.ManifestCheckpointConditionTypeReady)
	if cond == nil {
		return mcp.Name, false, false, fmt.Sprintf("waiting for ManifestCheckpoint %s Ready condition", mcp.Name), nil
	}
	if cond.Status == metav1.ConditionTrue {
		return mcp.Name, true, false, cond.Message, nil
	}
	if cond.Status == metav1.ConditionFalse && cond.Reason == ssv1alpha1.ManifestCheckpointConditionReasonFailed {
		return mcp.Name, false, true, cond.Message, nil
	}
	return mcp.Name, false, false, cond.Message, nil
}

func (r *SnapshotContentController) resolveDataReadiness(_ context.Context, _ *unstructured.Unstructured) (bool, string, string, error) {
	// v0: no data path yet; state-only snapshots do not require dataRef.
	// Future data path must publish SnapshotContent.status.dataRef only after the
	// final artifact exists, is Ready, and has ownerRef -> SnapshotContent.
	// dataRef is apiVersion/kind/name for a durable artifact, never a request ref.
	return true, "", "", nil
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

// checkConsistencyAndSetReady checks if artifacts exist and sets Ready condition
// According to ADR: Ready=False выставляется только для ранее успешных объектов
func (r *SnapshotContentController) checkConsistencyAndSetReady(
	ctx context.Context,
	contentLike snapshot.SnapshotContentLike,
	obj *unstructured.Unstructured,
) error {
	logger := log.FromContext(ctx)
	wasReady := snapshot.IsReady(contentLike)

	// Check ManifestCheckpoint if present
	mcpName := contentLike.GetStatusManifestCheckpointName()
	if mcpName == "" {
		// Ready must not become True without MCP link
		logger.V(1).Info("ManifestCheckpointName is empty; not Ready yet", "snapshotContent", obj.GetName())
		return nil
	}
	if mcpName != "" {
		exists, err := r.checkArtifactExists(ctx, "ManifestCheckpoint", mcpName, "state-snapshotter.deckhouse.io/v1alpha1")
		if err != nil {
			return fmt.Errorf("failed to check ManifestCheckpoint: %w", err)
		}
		if !exists {
			if wasReady {
				// Artifact was lost - set Ready=False
				snapshot.SetCondition(contentLike, snapshot.ConditionReady, metav1.ConditionFalse,
					snapshot.ReasonArtifactMissing, fmt.Sprintf("ManifestCheckpoint %s not found", mcpName))
				snapshot.SyncConditionsToUnstructured(obj, contentLike.GetStatusConditions())
				if err := r.Status().Update(ctx, obj); err != nil {
					return fmt.Errorf("failed to update Ready=False: %w", err)
				}
				logger.Info("ManifestCheckpoint missing, set Ready=False", "mcp", mcpName)
			}
			return nil // Artifact missing, but object was never Ready
		}
		if err := r.ensureArtifactFinalizer(ctx, "ManifestCheckpoint", mcpName, "state-snapshotter.deckhouse.io/v1alpha1"); err != nil {
			return fmt.Errorf("failed to ensure ManifestCheckpoint finalizer: %w", err)
		}
	}

	// Check durable data artifact if present. dataRef must point to the final artifact
	// (for example VolumeSnapshotContent), not to an execution request.
	dataRef := contentLike.GetStatusDataRef()
	if dataRef != nil && dataRef.Kind == "VolumeSnapshotContent" {
		exists, err := r.checkArtifactExists(ctx, "VolumeSnapshotContent", dataRef.Name, "snapshot.storage.k8s.io/v1")
		if err != nil {
			return fmt.Errorf("failed to check VolumeSnapshotContent: %w", err)
		}
		if !exists {
			if wasReady {
				// Artifact was lost - set Ready=False
				snapshot.SetCondition(contentLike, snapshot.ConditionReady, metav1.ConditionFalse,
					snapshot.ReasonArtifactMissing, fmt.Sprintf("VolumeSnapshotContent %s not found", dataRef.Name))
				snapshot.SyncConditionsToUnstructured(obj, contentLike.GetStatusConditions())
				if err := r.Status().Update(ctx, obj); err != nil {
					return fmt.Errorf("failed to update Ready=False: %w", err)
				}
				logger.Info("VolumeSnapshotContent missing, set Ready=False", "vsc", dataRef.Name)
			}
			return nil // Artifact missing, but object was never Ready
		}
		if err := r.ensureArtifactFinalizer(ctx, "VolumeSnapshotContent", dataRef.Name, "snapshot.storage.k8s.io/v1"); err != nil {
			return fmt.Errorf("failed to ensure VolumeSnapshotContent finalizer: %w", err)
		}
	}

	// Check children SnapshotContents if present
	childrenRefs := contentLike.GetStatusChildrenSnapshotContentRefs()
	if len(childrenRefs) > 0 {
		for _, childRef := range childrenRefs {
			childObj := &unstructured.Unstructured{}
			childObj.SetGroupVersionKind(obj.GroupVersionKind())
			if err := r.APIReader.Get(ctx, client.ObjectKey{Name: childRef.Name}, childObj); err != nil {
				if errors.IsNotFound(err) {
					if wasReady {
						snapshot.SetCondition(contentLike, snapshot.ConditionReady, metav1.ConditionFalse,
							snapshot.ReasonArtifactMissing, fmt.Sprintf("Child SnapshotContent %s not found", childRef.Name))
						snapshot.SyncConditionsToUnstructured(obj, contentLike.GetStatusConditions())
						if err := r.Status().Update(ctx, obj); err != nil {
							return fmt.Errorf("failed to update Ready=False: %w", err)
						}
					}
					return nil
				}
				return fmt.Errorf("failed to get child SnapshotContent %s: %w", childRef.Name, err)
			}
			childLike, err := snapshot.ExtractSnapshotContentLike(childObj)
			if err != nil {
				return fmt.Errorf("failed to extract child SnapshotContentLike: %w", err)
			}
			if !snapshot.IsReady(childLike) {
				if wasReady {
					snapshot.SetCondition(contentLike, snapshot.ConditionReady, metav1.ConditionFalse,
						snapshot.ReasonArtifactMissing, fmt.Sprintf("Child SnapshotContent %s is not Ready", childRef.Name))
					snapshot.SyncConditionsToUnstructured(obj, contentLike.GetStatusConditions())
					if err := r.Status().Update(ctx, obj); err != nil {
						return fmt.Errorf("failed to update Ready=False: %w", err)
					}
				}
				return nil
			}
		}
	}

	// All artifacts exist - set Ready=True if not already set
	if !wasReady {
		// Check if InProgress should be cleared
		if snapshot.IsInProgress(contentLike) {
			snapshot.SetCondition(contentLike, snapshot.ConditionInProgress, metav1.ConditionFalse,
				snapshot.ReasonCompleted, "All artifacts exist")
		}
		snapshot.SetCondition(contentLike, snapshot.ConditionReady, metav1.ConditionTrue,
			snapshot.ReasonCompleted, "All artifacts exist and valid")
		snapshot.SyncConditionsToUnstructured(obj, contentLike.GetStatusConditions())
		if err := r.Status().Update(ctx, obj); err != nil {
			return fmt.Errorf("failed to update Ready=True: %w", err)
		}
		logger.Info("All artifacts exist, set Ready=True")
	}

	return nil
}

// checkArtifactExists checks if an artifact exists
// Uses APIReader for read-after-write consistency
func (r *SnapshotContentController) checkArtifactExists(ctx context.Context, kind, name, apiVersion string) (bool, error) {
	// Parse GVK from apiVersion
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
	key := client.ObjectKey{Name: name}

	err := r.APIReader.Get(ctx, key, artifactObj)
	if errors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to get %s %s: %w", kind, name, err)
	}

	return true, nil
}

// ensureArtifactFinalizer adds artifact-protect finalizer to MCP/VSC if missing.
func (r *SnapshotContentController) ensureArtifactFinalizer(ctx context.Context, kind, name, apiVersion string) error {
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

	if snapshot.AddFinalizer(artifactObj, snapshot.FinalizerArtifactProtect) {
		if err := r.Update(ctx, artifactObj); err != nil {
			return err
		}
		log.FromContext(ctx).Info("Added artifact finalizer", "kind", kind, "name", name, "finalizer", snapshot.FinalizerArtifactProtect)
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
// Snapshot GVKs are supplied by bootstrap/DSC runtime wiring; this controller must
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
		if err := builder.Complete(r); err != nil {
			return fmt.Errorf("failed to setup watch for SnapshotContent GVK %s: %w", gvk.String(), err)
		}
		r.activeContentWatchSet[key] = struct{}{}
	}
	return nil
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
