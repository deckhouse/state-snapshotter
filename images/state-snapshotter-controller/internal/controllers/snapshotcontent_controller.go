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

	return &SnapshotContentController{
		Client:               client,
		APIReader:            apiReader,
		Scheme:               scheme,
		Config:               cfg,
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
	// TODO: GVK should come from watch or map[Name]→GVK
	// For now, we'll try to get it from the request context or use a default
	// This is a placeholder - actual GVK will be determined from the watch setup
	obj := &unstructured.Unstructured{}
	// NOTE: We cannot determine GVK from Name alone
	// This will be fixed when we set up proper watch with GVK mapping
	// For skeleton, we assume the object already has correct GVK set from watch
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "", // TODO: Get from watch context or GVK map
		Version: "v1alpha1",
		Kind:    "SnapshotContent", // TODO: Get from watch context or GVK map
	})

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

	// Step 1: Manage finalizer (basic protection from manual deletion)
	// At this stage, finalizer only protects from GC - no deletion logic yet
	if obj.GetDeletionTimestamp().IsZero() {
		// Object is not being deleted - ensure finalizer exists
		if snapshot.AddFinalizer(obj, snapshot.FinalizerParentProtect) {
			logger.Info("Added finalizer to SnapshotContent", "finalizer", snapshot.FinalizerParentProtect)
			if err := r.Update(ctx, obj); err != nil {
				logger.Error(err, "Failed to add finalizer")
				return ctrl.Result{}, err
			}
			// Requeue to continue processing after finalizer is added
			return ctrl.Result{Requeue: true}, nil
		}
	} else {
		// Object is being deleted - keep finalizer for now
		// TODO: Later we'll add logic to:
		// - Check if Snapshot exists
		// - Remove finalizer if Snapshot is deleted (orphaning)
		// - Cascade remove finalizers from children
		// For now, just keep the finalizer (prevents GC)
		logger.V(1).Info("SnapshotContent is being deleted, finalizer is held", "finalizer", snapshot.FinalizerParentProtect)
	}

	// TODO: Step 2 - Consistency checks (to be implemented later)
	// - Check if artifacts exist (MCP, VSC)
	// - Set Ready condition

	// TODO: Step 3 - Deletion handling (to be implemented later)
	// - Check if Snapshot exists
	// - Remove finalizer if Snapshot is deleted
	// - Cascade finalizers removal

	logger.Info("SnapshotContent reconciliation completed")
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager
func (r *SnapshotContentController) SetupWithManager(mgr ctrl.Manager) error {
	// For now, we'll need to register watches for each GVK
	// In the future, this can be done dynamically based on discovered CRDs
	// For skeleton, we'll return nil - actual watch setup will be done later
	return nil
}

