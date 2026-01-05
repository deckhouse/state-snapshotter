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

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/config"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// SnapshotContentController reconciles generic XxxxSnapshotContent resources
//
// This controller manages the lifecycle of SnapshotContent:
// - Manages finalizers (protection from manual deletion)
// - Checks consistency (artifacts exist, Ready condition)
// - Handles deletion (cascade finalizers removal)
// - Does NOT create SnapshotContent (that's SnapshotController's responsibility)
//
// Architecture:
// - Uses dynamic client for low-level get/list operations
// - Converts to typed SnapshotContentLike interface for business logic
// - Centralized conditions management through pkg/snapshot/conditions
// - Manages finalizers through pkg/snapshot/finalizers (to be implemented)
type SnapshotContentController struct {
	client.Client
	APIReader client.Reader // Required: for reading resources directly from API server
	Scheme    *runtime.Scheme
	Config    *config.Options

	// GVKRegistry provides centralized GVK resolution
	GVKRegistry *snapshot.GVKRegistry

	// SnapshotContentGVKs is a list of GVKs that this controller should watch
	// This allows domain modules to register their snapshot content types
	SnapshotContentGVKs []schema.GroupVersionKind
}

// NewSnapshotContentController creates a new SnapshotContentController with validated dependencies
func NewSnapshotContentController(
	client client.Client,
	apiReader client.Reader,
	scheme *runtime.Scheme,
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
	if cfg == nil {
		return nil, fmt.Errorf("Config must not be nil")
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
		Client:               client,
		APIReader:            apiReader,
		Scheme:               scheme,
		Config:               cfg,
		GVKRegistry:          registry,
		SnapshotContentGVKs: snapshotContentGVKs,
	}, nil
}

// Reconcile processes a SnapshotContent resource
//
// Step 1 (Skeleton): Basic structure - no finalizers, no deletion, no consistency checks
func (r *SnapshotContentController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("snapshotcontent", req.Name)
	logger.Info("Reconciling SnapshotContent")

	// Get the unstructured object
	// ARCHITECTURAL NOTE: SnapshotContentController is instantiated per-GVK
	// and registered with exact GVK in SetupWithManager.
	// Each controller instance handles only one specific GVK (e.g., VirtualMachineSnapshotContent).
	// The GVK is determined from the watch context (req.GroupVersionKind set by controller-runtime).
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(req.GroupVersionKind)

	if err := r.Get(ctx, req.NamespacedName, obj); err != nil {
		if errors.IsNotFound(err) {
			logger.V(1).Info("SnapshotContent not found, skipping")
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

	// Step 1: Manage finalizer and orphaning
	// Invariant: SnapshotContent без Snapshot обязан стать orphaned и перейти под управление ObjectKeeper
	
	if obj.GetDeletionTimestamp().IsZero() {
		// Object is not being deleted
		
		// Step 1.1: Ensure finalizer exists
		if snapshot.AddFinalizer(obj, snapshot.FinalizerParentProtect) {
			logger.Info("Added finalizer to SnapshotContent", "finalizer", snapshot.FinalizerParentProtect)
			if err := r.Update(ctx, obj); err != nil {
				logger.Error(err, "Failed to add finalizer")
				return ctrl.Result{}, err
			}
			// Requeue to continue processing after finalizer is added
			return ctrl.Result{Requeue: true}, nil
		}

		// Step 1.2: Check if Snapshot exists (orphaning check)
		// Use APIReader for read-after-write consistency
		snapshotRef := contentLike.GetSpecSnapshotRef()
		if snapshotRef == nil {
			logger.V(1).Info("SnapshotContent has no snapshotRef, skipping orphaning check")
		} else {
			snapshotExists, err := r.checkSnapshotExists(ctx, snapshotRef)
			if err != nil {
				logger.Error(err, "Failed to check if Snapshot exists", "snapshot", fmt.Sprintf("%s/%s", snapshotRef.Namespace, snapshotRef.Name))
				return ctrl.Result{}, err
			}

			if !snapshotExists {
				// Snapshot was deleted - remove finalizer (orphaning)
				// This allows SnapshotContent to become orphaned and be managed by ObjectKeeper TTL
				if snapshot.RemoveFinalizer(obj, snapshot.FinalizerParentProtect) {
					logger.Info(
						"SnapshotContent is orphaned: Snapshot was deleted, removing finalizer",
						"snapshot", fmt.Sprintf("%s/%s", snapshotRef.Namespace, snapshotRef.Name),
						"snapshotContent", req.Name,
					)
					if err := r.Update(ctx, obj); err != nil {
						logger.Error(err, "Failed to remove finalizer after orphaning")
						return ctrl.Result{}, err
					}
					// Requeue to continue processing after finalizer is removed
					return ctrl.Result{Requeue: true}, nil
				}
			}
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

	// Step 3: Consistency checks and Ready condition
	// Check if artifacts exist and set Ready condition
	if err := r.checkConsistencyAndSetReady(ctx, contentLike, obj); err != nil {
		logger.Error(err, "Failed to check consistency")
		// Non-fatal: continue reconciliation
	}

	logger.Info("SnapshotContent reconciliation completed")
	return ctrl.Result{}, nil
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
	
	var errors []error
	for _, childRef := range childrenRefs {
		// Resolve child Content GVK through registry
		childContentGVK, err := r.GVKRegistry.ResolveSnapshotContentGVK(childRef.Kind)
		if err != nil {
			// Fallback: derive from parent Content GVK if registry doesn't know
			childContentGVK = schema.GroupVersionKind{
				Group:   contentGVK.Group,
				Version: contentGVK.Version,
				Kind:    childRef.Kind,
			}
			logger.V(1).Info("Child Content GVK not found in registry, using fallback", "kind", childRef.Kind)
		}

		childObj := &unstructured.Unstructured{}
		childObj.SetGroupVersionKind(childContentGVK)
		childKey := client.ObjectKey{Name: childRef.Name}

		// Try to get child Content
		if err := r.Get(ctx, childKey, childObj); err != nil {
			if errors.IsNotFound(err) {
				// Child already deleted - skip (broken link, but not an error)
				logger.V(1).Info("Child SnapshotContent not found, skipping", "child", childRef.Name)
				continue
			}
			// Other error - log but continue
			logger.Error(err, "Failed to get child SnapshotContent", "child", childRef.Name)
			errors = append(errors, fmt.Errorf("failed to get child %s: %w", childRef.Name, err))
			continue
		}

		// Remove finalizer from child
		if snapshot.RemoveFinalizer(childObj, snapshot.FinalizerParentProtect) {
			logger.Info("Removed finalizer from child SnapshotContent", "child", childRef.Name)
			if err := r.Update(ctx, childObj); err != nil {
				if errors.IsNotFound(err) {
					// Child was deleted between Get and Update - skip
					logger.V(1).Info("Child SnapshotContent was deleted, skipping update", "child", childRef.Name)
					continue
				}
				logger.Error(err, "Failed to remove finalizer from child", "child", childRef.Name)
				errors = append(errors, fmt.Errorf("failed to update child %s: %w", childRef.Name, err))
				continue
			}
		} else {
			// Finalizer already removed - child is already being processed
			logger.V(1).Info("Child SnapshotContent finalizer already removed", "child", childRef.Name)
		}
	}

	// Return error only if all children failed (partial success is acceptable)
	if len(errors) > 0 && len(errors) == len(childrenRefs) {
		return fmt.Errorf("failed to remove finalizers from all children: %v", errors)
	}

	if len(errors) > 0 {
		logger.Info("Some children failed, but cascade continues", "failedCount", len(errors), "totalCount", len(childrenRefs))
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
	}

	// Check VolumeSnapshotContent if present (dataRef)
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

// checkSnapshotExists checks if the referenced Snapshot exists
// Uses APIReader for read-after-write consistency (direct API, no cache)
func (r *SnapshotContentController) checkSnapshotExists(ctx context.Context, snapshotRef *snapshot.ObjectRef) (bool, error) {
	if snapshotRef == nil {
		return false, nil
	}

	// Resolve Snapshot GVK through registry
	snapshotGVK, err := r.GVKRegistry.ResolveSnapshotGVK(snapshotRef.Kind)
	if err != nil {
		// Fallback: parse from APIVersion if registry doesn't know this GVK
		if idx := strings.Index(snapshotRef.APIVersion, "/"); idx != -1 {
			snapshotGVK = schema.GroupVersionKind{
				Group:   snapshotRef.APIVersion[:idx],
				Version: snapshotRef.APIVersion[idx+1:],
				Kind:    snapshotRef.Kind,
			}
		} else {
			snapshotGVK = schema.GroupVersionKind{
				Group:   "",
				Version: snapshotRef.APIVersion,
				Kind:    snapshotRef.Kind,
			}
		}
		logger := log.FromContext(ctx)
		logger.V(1).Info("Snapshot GVK not found in registry, using fallback parsing", "kind", snapshotRef.Kind)
	}

	snapshotObj := &unstructured.Unstructured{}
	snapshotObj.SetGroupVersionKind(snapshotGVK)

	key := client.ObjectKey{
		Name:      snapshotRef.Name,
		Namespace: snapshotRef.Namespace,
	}

	err := r.APIReader.Get(ctx, key, snapshotObj)
	if errors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to get Snapshot: %w", err)
	}

	return true, nil
}

// SetupWithManager sets up the controller with the Manager
// Registers watches for all registered SnapshotContent GVKs
// Each GVK gets its own controller instance to ensure correct GVK context
func (r *SnapshotContentController) SetupWithManager(mgr ctrl.Manager) error {
	// Register watch for each SnapshotContent GVK
	for _, gvk := range r.SnapshotContentGVKs {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)
		
		// Create a controller builder for this specific GVK
		builder := ctrl.NewControllerManagedBy(mgr).
			For(obj).
			Named(fmt.Sprintf("snapshotcontent-%s-%s", gvk.Group, gvk.Kind))
		
		if err := builder.Complete(r); err != nil {
			return fmt.Errorf("failed to setup watch for SnapshotContent GVK %s: %w", gvk.String(), err)
		}
	}
	
	return nil
}

