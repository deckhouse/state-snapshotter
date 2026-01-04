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
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	deckhousev1alpha1 "github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/config"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

const (
	DeckhouseAPIVersion = "deckhouse.io/v1alpha1"
	KindObjectKeeper    = "ObjectKeeper"
)

// SnapshotController reconciles generic XxxxSnapshot resources
//
// This controller works with any CRD that implements the SnapshotLike interface
// and follows the unified snapshot pattern from ADR.
//
// Architecture:
// - Uses dynamic client for low-level get/list operations
// - Converts to typed SnapshotLike interface for business logic
// - Centralized conditions management through pkg/snapshot/conditions
// - Creates SnapshotContent and ObjectKeeper for root snapshots
type SnapshotController struct {
	client.Client
	APIReader client.Reader // Required: for reading ObjectKeeper directly from API server after creation
	Scheme    *runtime.Scheme
	Config    *config.Options

	// SnapshotGVKs is a list of GVKs that this controller should watch
	// This allows domain modules to register their snapshot types
	SnapshotGVKs []schema.GroupVersionKind
}

// NewSnapshotController creates a new SnapshotController with validated dependencies
func NewSnapshotController(
	client client.Client,
	apiReader client.Reader,
	scheme *runtime.Scheme,
	cfg *config.Options,
	snapshotGVKs []schema.GroupVersionKind,
) (*SnapshotController, error) {
	if client == nil {
		return nil, fmt.Errorf("Client must not be nil")
	}
	if apiReader == nil {
		return nil, fmt.Errorf("APIReader must not be nil: controllers require APIReader to read ObjectKeeper after creation (UID barrier pattern)")
	}
	if scheme == nil {
		return nil, fmt.Errorf("Scheme must not be nil")
	}
	if cfg == nil {
		return nil, fmt.Errorf("Config must not be nil")
	}

	return &SnapshotController{
		Client:       client,
		APIReader:    apiReader,
		Scheme:       scheme,
		Config:       cfg,
		SnapshotGVKs: snapshotGVKs,
	}, nil
}

// Reconcile processes a Snapshot resource
//
// Step 1 (Skeleton): Only create path - no deletion, no propagation
func (r *SnapshotController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("snapshot", req.NamespacedName)
	logger.Info("Reconciling Snapshot")

	// Get the unstructured object
	// TODO: GVK should come from watch or map[NamespacedName]→GVK
	// For now, we'll try to get it from the request context or use a default
	// This is a placeholder - actual GVK will be determined from the watch setup
	obj := &unstructured.Unstructured{}
	// NOTE: We cannot determine GVK from NamespacedName alone
	// This will be fixed when we set up proper watch with GVK mapping
	// For skeleton, we assume the object already has correct GVK set from watch
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "", // TODO: Get from watch context or GVK map
		Version: "v1alpha1",
		Kind:    "Snapshot", // TODO: Get from watch context or GVK map
	})

	if err := r.Get(ctx, req.NamespacedName, obj); err != nil {
		if errors.IsNotFound(err) {
			logger.V(1).Info("Snapshot not found, skipping")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "failed to get Snapshot")
		return ctrl.Result{}, err
	}

	// Convert to typed interface
	snapshotLike, err := snapshot.ExtractSnapshotLike(obj)
	if err != nil {
		logger.Error(err, "failed to extract SnapshotLike interface")
		return ctrl.Result{}, err
	}

	// Step 0: Handle deletion - propagation Ready=False to parent
	// If Snapshot is being deleted and was Ready=True, propagate Ready=False to parent
	if !obj.GetDeletionTimestamp().IsZero() {
		// Snapshot is being deleted
		// Check if it was Ready=True and has a parent
		if snapshot.IsReady(snapshotLike) {
			// Propagate Ready=False to parent (if exists and not being deleted)
			if err := r.propagateReadyFalseToParent(ctx, snapshotLike, obj); err != nil {
				logger.Error(err, "Failed to propagate Ready=False to parent")
				// Non-fatal: continue with deletion
			}
		}
		// Snapshot is being deleted - no need to continue create-path
		return ctrl.Result{}, nil
	}

	// Step 1: Barrier - Wait for HandledByDomainSpecificController
	// Domain controller must process the snapshot first (create MCR/VCR, set conditions)
	if !snapshot.HasCondition(snapshotLike, snapshot.ConditionHandledByDomainSpecificController, metav1.ConditionTrue) {
		logger.V(1).Info("Waiting for domain controller to handle snapshot")
		return ctrl.Result{}, nil
	}

	// Check if already handled by common controller
	if snapshot.HasCondition(snapshotLike, snapshot.ConditionHandledByCommonController, metav1.ConditionTrue) {
		logger.V(1).Info("Snapshot already handled by common controller, skipping")
		return ctrl.Result{}, nil
	}

	// Step 2: Set InProgress condition
	snapshot.SetCondition(snapshotLike, snapshot.ConditionInProgress, metav1.ConditionTrue, "Processing", "Common controller is processing snapshot")
	if err := r.updateSnapshotStatus(ctx, obj, snapshotLike); err != nil {
		return ctrl.Result{}, err
	}

	// Step 3: Create ObjectKeeper for root snapshots first (needed for SnapshotContent ownerRef)
	var objectKeeper *deckhousev1alpha1.ObjectKeeper
	if snapshot.IsRootSnapshot(obj) {
		var result ctrl.Result
		var err error
		objectKeeper, result, err = r.ensureObjectKeeper(ctx, snapshotLike, obj, "")
		if err != nil {
			return ctrl.Result{}, err
		}
		if result.Requeue {
			return result, nil
		}
	}

	// Step 4: Create SnapshotContent if it doesn't exist
	contentName := snapshotLike.GetStatusContentName()
	if contentName == "" {
		// Generate deterministic name
		contentName = snapshot.GenerateSnapshotContentName(obj.GetName(), string(obj.GetUID()))
		
		// Create SnapshotContent
		contentGVK := r.getSnapshotContentGVK(obj.GetObjectKind().GroupVersionKind())
		contentObj := &unstructured.Unstructured{}
		contentObj.SetGroupVersionKind(contentGVK)
		contentObj.SetName(contentName)
		// SnapshotContent is cluster-scoped, no namespace

		// Set spec.snapshotRef
		spec := map[string]interface{}{
			"snapshotRef": map[string]interface{}{
				"kind":      obj.GetKind(),
				"name":      obj.GetName(),
				"namespace": obj.GetNamespace(),
			},
		}
		contentObj.Object["spec"] = spec

		// Set ownerRef: ObjectKeeper for root snapshots, Snapshot for children
		var ownerRef metav1.OwnerReference
		if objectKeeper != nil {
			// Root snapshot: ObjectKeeper owns SnapshotContent
			ownerRef = metav1.OwnerReference{
				APIVersion: DeckhouseAPIVersion,
				Kind:       KindObjectKeeper,
				Name:       objectKeeper.Name,
				UID:        objectKeeper.UID,
				Controller: func() *bool { b := true; return &b }(),
			}
		} else {
			// Child snapshot: Snapshot owns SnapshotContent (will be updated later by parent)
			ownerRef = metav1.OwnerReference{
				APIVersion: obj.GetObjectKind().GroupVersionKind().GroupVersion().String(),
				Kind:       obj.GetKind(),
				Name:       obj.GetName(),
				UID:        obj.GetUID(),
				Controller: func() *bool { b := true; return &b }(),
			}
		}
		contentObj.SetOwnerReferences([]metav1.OwnerReference{ownerRef})

		if err := r.Create(ctx, contentObj); err != nil {
			logger.Error(err, "Failed to create SnapshotContent", "name", contentName)
			return ctrl.Result{}, err
		}
		logger.Info("Created SnapshotContent", "name", contentName, "owner", ownerRef.Kind)

		// Update Snapshot status.contentName
		status := obj.Object["status"]
		if status == nil {
			status = make(map[string]interface{})
			obj.Object["status"] = status
		}
		statusMap := status.(map[string]interface{})
		statusMap["contentName"] = contentName

		if err := r.Status().Update(ctx, obj); err != nil {
			logger.Error(err, "Failed to update Snapshot status.contentName")
			return ctrl.Result{}, err
		}
		logger.Info("Updated Snapshot status.contentName", "contentName", contentName)
	}

	// Step 5: Set HandledByCommonController condition
	snapshot.SetCondition(snapshotLike, snapshot.ConditionHandledByCommonController, metav1.ConditionTrue, "Handled", "Common controller has started processing")
	if err := r.updateSnapshotStatus(ctx, obj, snapshotLike); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("Snapshot reconciliation completed (create path)")
	return ctrl.Result{}, nil
}

// updateSnapshotStatus updates the status of the Snapshot object
func (r *SnapshotController) updateSnapshotStatus(ctx context.Context, obj *unstructured.Unstructured, snapshotLike snapshot.SnapshotLike) error {
	// Sync conditions from wrapper to unstructured object
	conditions := snapshotLike.GetStatusConditions()
	snapshot.SyncConditionsToUnstructured(obj, conditions)

	return r.Status().Update(ctx, obj)
}

// ensureObjectKeeper creates or gets ObjectKeeper for root snapshot
// Returns ObjectKeeper, ctrl.Result (for requeue), and error
// contentName is optional - used only for updating ownerRef if ObjectKeeper already exists
func (r *SnapshotController) ensureObjectKeeper(
	ctx context.Context,
	snapshotLike snapshot.SnapshotLike,
	obj *unstructured.Unstructured,
	contentName string,
) (*deckhousev1alpha1.ObjectKeeper, ctrl.Result, error) {
	logger := log.FromContext(ctx)
	retainerName := snapshot.GenerateObjectKeeperName(obj.GetKind(), obj.GetName())

	objectKeeper := &deckhousev1alpha1.ObjectKeeper{}
	err := r.Get(ctx, client.ObjectKey{Name: retainerName}, objectKeeper)

	switch {
	case errors.IsNotFound(err):
		// Create ObjectKeeper
		// For root snapshots, ObjectKeeper follows the Snapshot and manages TTL for SnapshotContent
		// TODO: Use FollowObjectWithTTL mode when TTL is implemented
		// For now, use FollowObject mode
		gvk := obj.GetObjectKind().GroupVersionKind()
		objectKeeper = &deckhousev1alpha1.ObjectKeeper{
			TypeMeta: metav1.TypeMeta{
				APIVersion: DeckhouseAPIVersion,
				Kind:       KindObjectKeeper,
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: retainerName,
			},
			Spec: deckhousev1alpha1.ObjectKeeperSpec{
				Mode: "FollowObject", // TODO: Change to FollowObjectWithTTL when TTL is implemented
				FollowObjectRef: &deckhousev1alpha1.FollowObjectRef{
					APIVersion: gvk.GroupVersion().String(),
					Kind:       gvk.Kind,
					Namespace:  obj.GetNamespace(),
					Name:       obj.GetName(),
					UID:        string(obj.GetUID()),
				},
			},
		}

		if err := r.Create(ctx, objectKeeper); err != nil {
			return nil, ctrl.Result{}, fmt.Errorf("failed to create ObjectKeeper: %w", err)
		}
		logger.Info("Created ObjectKeeper", "name", retainerName)

		// UID barrier: Re-read ObjectKeeper via APIReader to get UID
		if err := r.APIReader.Get(ctx, client.ObjectKey{Name: retainerName}, objectKeeper); err != nil {
			return nil, ctrl.Result{RequeueAfter: 200 * time.Millisecond}, nil
		}

		// If SnapshotContent already exists, update its ownerRef to ObjectKeeper
		if contentName != "" {
			contentGVK := r.getSnapshotContentGVK(gvk)
			contentObj := &unstructured.Unstructured{}
			contentObj.SetGroupVersionKind(contentGVK)
			if err := r.Get(ctx, client.ObjectKey{Name: contentName}, contentObj); err == nil {
				// Update ownerRef: ObjectKeeper owns SnapshotContent
				contentObj.SetOwnerReferences([]metav1.OwnerReference{
					{
						APIVersion: DeckhouseAPIVersion,
						Kind:       KindObjectKeeper,
						Name:       retainerName,
						UID:        objectKeeper.UID,
						Controller: func() *bool { b := true; return &b }(),
					},
				})

				if err := r.Update(ctx, contentObj); err != nil {
					logger.Error(err, "Failed to update SnapshotContent ownerRef to ObjectKeeper", "contentName", contentName)
					// Non-fatal: will be retried on next reconcile
				} else {
					logger.Info("Updated SnapshotContent ownerRef to ObjectKeeper", "contentName", contentName, "objectKeeper", retainerName)
				}
			}
		}

		return objectKeeper, ctrl.Result{}, nil

	case err != nil:
		return nil, ctrl.Result{}, fmt.Errorf("failed to get ObjectKeeper: %w", err)

	default:
		// ObjectKeeper exists - validate it belongs to this Snapshot
		if objectKeeper.Spec.FollowObjectRef == nil {
			return nil, ctrl.Result{}, fmt.Errorf("ObjectKeeper %s has no FollowObjectRef", retainerName)
		}
		if objectKeeper.Spec.FollowObjectRef.UID != string(obj.GetUID()) {
			return nil, ctrl.Result{}, fmt.Errorf("ObjectKeeper %s belongs to another Snapshot (UID mismatch)", retainerName)
		}
		logger.V(1).Info("ObjectKeeper already exists", "name", retainerName)
		return objectKeeper, ctrl.Result{}, nil
	}
}

// propagateReadyFalseToParent propagates Ready=False to parent Snapshot if:
// - Snapshot was Ready=True
// - Parent exists and is not being deleted
// - Parent was Ready=True
// This implements the tree consistency rule from deletion algorithm
func (r *SnapshotController) propagateReadyFalseToParent(
	ctx context.Context,
	snapshotLike snapshot.SnapshotLike,
	obj *unstructured.Unstructured,
) error {
	logger := log.FromContext(ctx)

	// Find parent Snapshot through ownerRef
	ownerRefs := obj.GetOwnerReferences()
	var parentRef *metav1.OwnerReference
	for i := range ownerRefs {
		ref := &ownerRefs[i]
		// Check if owner is another snapshot type (ends with "Snapshot")
		if strings.HasSuffix(ref.Kind, "Snapshot") {
			parentRef = ref
			break
		}
	}

	if parentRef == nil {
		// No parent - nothing to propagate
		return nil
	}

	// Get parent Snapshot
	// Parse APIVersion (format: "group/version" or "version" for core APIs)
	var parentGVK schema.GroupVersionKind
	if idx := strings.Index(parentRef.APIVersion, "/"); idx != -1 {
		parentGVK = schema.GroupVersionKind{
			Group:   parentRef.APIVersion[:idx],
			Version: parentRef.APIVersion[idx+1:],
			Kind:    parentRef.Kind,
		}
	} else {
		// Core API (e.g., "v1")
		parentGVK = schema.GroupVersionKind{
			Group:   "",
			Version: parentRef.APIVersion,
			Kind:    parentRef.Kind,
		}
	}

	parentObj := &unstructured.Unstructured{}
	parentObj.SetGroupVersionKind(parentGVK)
	parentKey := client.ObjectKey{
		Name:      parentRef.Name,
		Namespace: obj.GetNamespace(), // Parent should be in the same namespace
	}

	// Use APIReader for read-after-write consistency
	if err := r.APIReader.Get(ctx, parentKey, parentObj); err != nil {
		if errors.IsNotFound(err) {
			// Parent doesn't exist - nothing to propagate
			return nil
		}
		return fmt.Errorf("failed to get parent Snapshot: %w", err)
	}

	// Guards: Don't propagate if:
	// 1. Parent is being deleted (cascade deletion)
	if !parentObj.GetDeletionTimestamp().IsZero() {
		logger.V(1).Info("Parent Snapshot is being deleted, skipping propagation")
		return nil
	}

	// 2. Parent was not Ready=True (don't propagate to already broken snapshots)
	parentLike, err := snapshot.ExtractSnapshotLike(parentObj)
	if err != nil {
		return fmt.Errorf("failed to extract parent SnapshotLike: %w", err)
	}

	if !snapshot.IsReady(parentLike) {
		logger.V(1).Info("Parent Snapshot is not Ready=True, skipping propagation")
		return nil
	}

	// 3. Parent already has Ready=False (preserve existing Reason)
	readyCond := snapshot.GetCondition(parentLike, snapshot.ConditionReady)
	if readyCond != nil && readyCond.Status == metav1.ConditionFalse {
		logger.V(1).Info("Parent Snapshot already has Ready=False, preserving existing Reason", "reason", readyCond.Reason)
		return nil
	}

	// Propagate Ready=False to parent
	// Preserve existing Reason if Ready=False already exists (root-cause preservation)
	reason := snapshot.ReasonChildSnapshotMissing
	if readyCond != nil && readyCond.Status == metav1.ConditionFalse {
		reason = readyCond.Reason // Preserve existing reason
	}

	snapshot.SetCondition(parentLike, snapshot.ConditionReady, metav1.ConditionFalse, reason,
		fmt.Sprintf("Child Snapshot %s/%s was deleted", obj.GetNamespace(), obj.GetName()))
	
	// Sync conditions to unstructured
	snapshot.SyncConditionsToUnstructured(parentObj, parentLike.GetStatusConditions())
	
	if err := r.Status().Update(ctx, parentObj); err != nil {
		return fmt.Errorf("failed to update parent Snapshot Ready=False: %w", err)
	}

	logger.Info("Propagated Ready=False to parent Snapshot",
		"parent", fmt.Sprintf("%s/%s", parentObj.GetNamespace(), parentObj.GetName()),
		"child", fmt.Sprintf("%s/%s", obj.GetNamespace(), obj.GetName()),
		"reason", reason)

	// Recursively propagate to grandparent
	return r.propagateReadyFalseToParent(ctx, parentLike, parentObj)
}

// getSnapshotContentGVK derives SnapshotContent GVK from Snapshot GVK
// Example: virtualization.deckhouse.io/v1alpha1.VirtualMachineSnapshot -> virtualization.deckhouse.io/v1alpha1.VirtualMachineSnapshotContent
func (r *SnapshotController) getSnapshotContentGVK(snapshotGVK schema.GroupVersionKind) schema.GroupVersionKind {
	return schema.GroupVersionKind{
		Group:   snapshotGVK.Group,
		Version: snapshotGVK.Version,
		Kind:    snapshotGVK.Kind + "Content",
	}
}

// SetupWithManager sets up the controller with the Manager
func (r *SnapshotController) SetupWithManager(mgr ctrl.Manager) error {
	// For now, we'll need to register watches for each GVK
	// In the future, this can be done dynamically based on discovered CRDs
	// For skeleton, we'll return nil - actual watch setup will be done later
	return nil
}

