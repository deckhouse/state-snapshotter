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
	"reflect"
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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	deckhousev1alpha1 "github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/config"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
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

	// GVKRegistry provides centralized GVK resolution
	GVKRegistry *snapshot.GVKRegistry

	// SnapshotGVKs is a list of GVKs that this controller should watch
	// This allows domain modules to register their snapshot types
	SnapshotGVKs []schema.GroupVersionKind

	watchMu                sync.RWMutex
	activeSnapshotWatchSet map[string]struct{} // snapshot GVK String() -> watch registered with manager
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

	// Initialize GVK Registry and register known GVKs
	registry := snapshot.NewGVKRegistry()
	for _, gvk := range snapshotGVKs {
		if err := registry.RegisterSnapshotGVK(gvk.Kind, gvk.GroupVersion().String()); err != nil {
			return nil, fmt.Errorf("failed to register Snapshot GVK %s: %w", gvk.String(), err)
		}
		// Also register Content GVK (derived from Snapshot Kind)
		contentKind := gvk.Kind + "Content"
		contentGVK := schema.GroupVersionKind{
			Group:   gvk.Group,
			Version: gvk.Version,
			Kind:    contentKind,
		}
		if err := registry.RegisterSnapshotContentGVK(contentKind, contentGVK.GroupVersion().String()); err != nil {
			return nil, fmt.Errorf("failed to register SnapshotContent GVK %s: %w", contentGVK.String(), err)
		}
	}

	return &SnapshotController{
		Client:                 client,
		APIReader:              apiReader,
		Scheme:                 scheme,
		Config:                 cfg,
		GVKRegistry:            registry,
		SnapshotGVKs:           snapshotGVKs,
		activeSnapshotWatchSet: make(map[string]struct{}),
	}, nil
}

// Reconcile processes a Snapshot resource
//
// Step 1 (Skeleton): Only create path - no deletion, no propagation
func (r *SnapshotController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("snapshot", req.NamespacedName)
	logger.Info("Reconciling Snapshot")

	// Get the unstructured object
	// We need to try each registered GVK to find the correct one
	// In practice, each controller instance watches a specific GVK
	obj := &unstructured.Unstructured{}
	var found bool
	var err error
	for _, gvk := range r.snapshotGVKsSnapshot() {
		obj.SetGroupVersionKind(gvk)
		err = r.Get(ctx, req.NamespacedName, obj)
		if err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			logger.Error(err, "failed to get Snapshot")
			return ctrl.Result{}, err
		}
		found = true
		break
	}

	if !found {
		logger.V(1).Info("Snapshot not found in any registered GVK, skipping")
		return ctrl.Result{}, nil
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
		// Remove finalizer from SnapshotContent on parent deletion (watch-driven, no snapshotRef)
		if err := r.removeSnapshotContentFinalizer(ctx, snapshotLike, obj); err != nil {
			logger.Error(err, "Failed to remove SnapshotContent finalizer on snapshot deletion")
			// Non-fatal: continue with deletion
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
		// Snapshot is already handled - check consistency and Ready condition
		if err := r.checkConsistencyAndSetReady(ctx, snapshotLike, obj); err != nil {
			logger.Error(err, "Failed to check consistency")
			// Non-fatal: continue reconciliation
		}
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
		snapshotGVK := obj.GetObjectKind().GroupVersionKind()
		if snapshotGVK.Kind == "" {
			// Fallback: try to get Kind from obj.Object directly
			if kind, ok := obj.Object["kind"].(string); ok && kind != "" {
				snapshotGVK.Kind = kind
			} else {
				logger.Error(nil, "Cannot create SnapshotContent: Snapshot Kind is empty and cannot be determined", "obj", obj.GetName())
				return ctrl.Result{}, fmt.Errorf("cannot determine Snapshot Kind for object %s", obj.GetName())
			}
		}

		contentGVK, err := r.getSnapshotContentGVK(snapshotGVK)
		if err != nil {
			logger.Error(err, "Failed to resolve SnapshotContent GVK")
			return ctrl.Result{}, err
		}
		contentObj := &unstructured.Unstructured{}
		contentObj.SetGroupVersionKind(contentGVK)
		contentObj.SetName(contentName)
		// SnapshotContent is cluster-scoped, no namespace

		// Get BackupClass to extract backupRepositoryName and deletionPolicy
		// Snapshot.spec.backupClassName is required and links to BackupClass
		// BackupClass.spec.backupRepositoryName provides the repository
		// BackupClass.spec.deletionPolicy provides the deletion policy (or default to "Retain")
		var backupRepositoryName string
		deletionPolicy := "Retain" // Default deletion policy
		var backupClassName string

		specObj, ok := obj.Object["spec"].(map[string]interface{})
		if ok {
			if backupClassNameRaw, ok := specObj["backupClassName"].(string); ok && backupClassNameRaw != "" {
				backupClassName = backupClassNameRaw
				// Get BackupClass to extract backupRepositoryName and deletionPolicy
				backupClassObj := &unstructured.Unstructured{}
				backupClassObj.SetGroupVersionKind(schema.GroupVersionKind{
					Group:   "storage.deckhouse.io",
					Version: "v1alpha1",
					Kind:    "BackupClass",
				})
				if err := r.Get(ctx, client.ObjectKey{Name: backupClassNameRaw}, backupClassObj); err == nil {
					// Extract backupRepositoryName and deletionPolicy from BackupClass
					if backupClassSpec, ok := backupClassObj.Object["spec"].(map[string]interface{}); ok {
						if repoName, ok := backupClassSpec["backupRepositoryName"].(string); ok && repoName != "" {
							backupRepositoryName = repoName
						}
						if policy, ok := backupClassSpec["deletionPolicy"].(string); ok && policy != "" {
							deletionPolicy = policy
						}
					}
				} else {
					logger.V(1).Info("BackupClass not found, using defaults", "backupClassName", backupClassNameRaw, "error", err)
				}
			}
		}

		// Set spec.snapshotRef
		// CRD requires: name, namespace
		// NOTE: snapshotRef.kind is intentionally omitted to avoid CRD schema warnings.
		snapshotRef := map[string]interface{}{
			"name":      obj.GetName(),
			"namespace": obj.GetNamespace(),
		}
		spec := map[string]interface{}{
			"snapshotRef": snapshotRef,
		}

		// Add required fields from BackupClass
		if backupRepositoryName == "" {
			logger.Error(nil, "BackupClass does not have backupRepositoryName, cannot create SnapshotContent", "backupClassName", backupClassName)
			return ctrl.Result{}, fmt.Errorf("BackupClass '%s' does not specify backupRepositoryName", backupClassName)
		}
		spec["backupRepositoryName"] = backupRepositoryName
		spec["deletionPolicy"] = deletionPolicy

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

		// Update Snapshot status.boundSnapshotContentName (as per CRD schema)
		status := obj.Object["status"]
		if status == nil {
			status = make(map[string]interface{})
			obj.Object["status"] = status
		}
		statusMap := status.(map[string]interface{})
		statusMap["boundSnapshotContentName"] = contentName

		if err := r.Status().Update(ctx, obj); err != nil {
			logger.Error(err, "Failed to update Snapshot status.boundSnapshotContentName")
			return ctrl.Result{}, err
		}
		// Log both field names for backward compatibility with log parsers
		logger.Info("Updated Snapshot status.boundSnapshotContentName", "boundSnapshotContentName", contentName, "contentName", contentName)
	}

	// Step 4.5: Populate SnapshotContent links from MCR/VCR (if present and Ready)
	if contentName != "" {
		requeue, err := r.ensureSnapshotContentLinks(ctx, snapshotLike, obj, contentName)
		if err != nil {
			logger.Error(err, "Failed to ensure SnapshotContent links")
			return ctrl.Result{}, err
		}
		if requeue {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	}

	// Step 5: Set HandledByCommonController condition
	snapshot.SetCondition(snapshotLike, snapshot.ConditionHandledByCommonController, metav1.ConditionTrue, "Handled", "Common controller has started processing")
	if err := r.updateSnapshotStatus(ctx, obj, snapshotLike); err != nil {
		return ctrl.Result{}, err
	}

	// Step 6: Check consistency and set Ready condition
	// Only check if SnapshotContent already exists (has been processed by SnapshotContentController)
	// This avoids checking consistency in a "half-assembled" state where SnapshotContent
	// might not have finalizer or Ready condition yet.
	// If SnapshotContent is not ready yet, the next reconcile (triggered by SnapshotContentController
	// setting Ready=True) will set Snapshot Ready=True.
	if snapshotLike.GetStatusContentName() != "" {
		if err := r.checkConsistencyAndSetReady(ctx, snapshotLike, obj); err != nil {
			logger.Error(err, "Failed to check consistency after creating SnapshotContent")
			// Non-fatal: will retry on next reconcile
		}
	}

	logger.Info("Snapshot reconciliation completed (create path)")
	return ctrl.Result{}, nil
}

// ensureSnapshotContentLinks populates SnapshotContent.status.manifestCheckpointName/dataRef
// from ready MCR/VCR (if present). Returns requeue=true if requests are not ready yet.
func (r *SnapshotController) ensureSnapshotContentLinks(
	ctx context.Context,
	snapshotLike snapshot.SnapshotLike,
	obj *unstructured.Unstructured,
	contentName string,
) (bool, error) {
	logger := log.FromContext(ctx)

	mcrName := snapshotLike.GetStatusManifestCaptureRequestName()
	vcrName := snapshotLike.GetStatusVolumeCaptureRequestName()

	// No requests yet - nothing to do
	if mcrName == "" && vcrName == "" {
		return false, nil
	}

	// Fetch SnapshotContent
	contentGVK, err := r.getSnapshotContentGVK(obj.GetObjectKind().GroupVersionKind())
	if err != nil {
		return false, fmt.Errorf("failed to resolve SnapshotContent GVK: %w", err)
	}
	contentObj := &unstructured.Unstructured{}
	contentObj.SetGroupVersionKind(contentGVK)
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: contentName}, contentObj); err != nil {
		return false, err
	}

	statusMap, _ := contentObj.Object["status"].(map[string]interface{})
	if statusMap == nil {
		statusMap = map[string]interface{}{}
	}

	needsUpdate := false

	// Handle MCR -> ManifestCheckpointName
	if mcrName != "" {
		mcr := &storagev1alpha1.ManifestCaptureRequest{}
		if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: obj.GetNamespace(), Name: mcrName}, mcr); err != nil {
			return false, err
		}
		readyCond := meta.FindStatusCondition(mcr.Status.Conditions, storagev1alpha1.ManifestCaptureRequestConditionTypeReady)
		if readyCond == nil || readyCond.Status != metav1.ConditionTrue {
			logger.V(1).Info("MCR not ready yet, requeue", "mcr", mcrName)
			return true, nil
		}
		if mcr.Status.CheckpointName == "" {
			logger.V(1).Info("MCR Ready but checkpointName empty, requeue", "mcr", mcrName)
			return true, nil
		}
		if existing, ok := statusMap["manifestCheckpointName"].(string); !ok || existing != mcr.Status.CheckpointName {
			statusMap["manifestCheckpointName"] = mcr.Status.CheckpointName
			needsUpdate = true
		}
	}

	// Handle VCR -> dataRef
	if vcrName != "" {
		vcrGVK := schema.GroupVersionKind{Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "VolumeCaptureRequest"}
		vcrObj := &unstructured.Unstructured{}
		vcrObj.SetGroupVersionKind(vcrGVK)
		if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: obj.GetNamespace(), Name: vcrName}, vcrObj); err != nil {
			return false, err
		}
		conditions, _, _ := unstructured.NestedSlice(vcrObj.Object, "status", "conditions")
		ready := isConditionTrue(conditions, "Ready")
		if !ready {
			logger.V(1).Info("VCR not ready yet, requeue", "vcr", vcrName)
			return true, nil
		}
		dataRefMap, _, _ := unstructured.NestedMap(vcrObj.Object, "status", "dataRef")
		name, _ := dataRefMap["name"].(string)
		kind, _ := dataRefMap["kind"].(string)
		namespace, _ := dataRefMap["namespace"].(string)
		if name == "" || kind == "" {
			logger.V(1).Info("VCR Ready but dataRef incomplete, requeue", "vcr", vcrName)
			return true, nil
		}
		newDataRef := map[string]interface{}{"name": name, "kind": kind}
		if namespace != "" {
			newDataRef["namespace"] = namespace
		}
		if existing, ok := statusMap["dataRef"].(map[string]interface{}); !ok || !reflect.DeepEqual(existing, newDataRef) {
			statusMap["dataRef"] = newDataRef
			needsUpdate = true
		}
	}

	if needsUpdate {
		contentObj.Object["status"] = statusMap
		if err := r.Status().Update(ctx, contentObj); err != nil {
			return false, err
		}
		logger.Info("Updated SnapshotContent links", "content", contentName, "mcr", mcrName, "vcr", vcrName)
	}

	return false, nil
}

func isConditionTrue(conditions []interface{}, condType string) bool {
	for _, c := range conditions {
		cond, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		t, _ := cond["type"].(string)
		if t != condType {
			continue
		}
		status, _ := cond["status"].(string)
		return status == string(metav1.ConditionTrue)
	}
	return false
}

func (r *SnapshotController) removeSnapshotContentFinalizer(
	ctx context.Context,
	snapshotLike snapshot.SnapshotLike,
	obj *unstructured.Unstructured,
) error {
	contentName := snapshotLike.GetStatusContentName()
	if contentName == "" && obj.GetUID() != "" {
		// Fallback to deterministic name to avoid race when status not yet set.
		// UID is available only after the Snapshot is persisted.
		contentName = snapshot.GenerateSnapshotContentName(obj.GetName(), string(obj.GetUID()))
	}
	if contentName == "" {
		return nil
	}

	contentGVK, err := r.getSnapshotContentGVK(obj.GetObjectKind().GroupVersionKind())
	if err != nil {
		return fmt.Errorf("failed to resolve SnapshotContent GVK: %w", err)
	}

	contentObj := &unstructured.Unstructured{}
	contentObj.SetGroupVersionKind(contentGVK)
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: contentName}, contentObj); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}

	updated := false
	annotations := contentObj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	if annotations[snapshot.AnnotationParentDeleted] != "true" {
		annotations[snapshot.AnnotationParentDeleted] = "true"
		contentObj.SetAnnotations(annotations)
		updated = true
	}
	if snapshot.RemoveFinalizer(contentObj, snapshot.FinalizerParentProtect) {
		updated = true
		log.FromContext(ctx).Info("Removed finalizer from SnapshotContent after Snapshot deletion", "content", contentName)
	}
	if updated {
		if err := r.Update(ctx, contentObj); err != nil {
			return err
		}
	}

	return nil
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
	_ snapshot.SnapshotLike,
	obj *unstructured.Unstructured,
	contentName string,
) (*deckhousev1alpha1.ObjectKeeper, ctrl.Result, error) {
	logger := log.FromContext(ctx)
	retainerName := snapshot.GenerateObjectKeeperName(obj.GetKind(), obj.GetName())

	objectKeeper := &deckhousev1alpha1.ObjectKeeper{}
	err := r.Get(ctx, client.ObjectKey{Name: retainerName}, objectKeeper)

	switch {
	case errors.IsNotFound(err):
		// Root snapshot: ObjectKeeper always FollowObjectWithTTL on the snapshot; TTL from config (env or default).
		wantSpec := r.desiredUnifiedRootObjectKeeperSpec(obj)
		objectKeeper = &deckhousev1alpha1.ObjectKeeper{
			TypeMeta: metav1.TypeMeta{
				APIVersion: DeckhouseAPIVersion,
				Kind:       KindObjectKeeper,
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: retainerName,
			},
			Spec: wantSpec,
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
			contentGVK, err := r.getSnapshotContentGVK(obj.GetObjectKind().GroupVersionKind())
			if err != nil {
				return nil, ctrl.Result{}, fmt.Errorf("failed to resolve SnapshotContent GVK: %w", err)
			}
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
		// ObjectKeeper exists — same snapshot UID; align spec (mode/TTL/followRef) with current config.
		if objectKeeper.Spec.FollowObjectRef == nil {
			return nil, ctrl.Result{}, fmt.Errorf("ObjectKeeper %s has no FollowObjectRef", retainerName)
		}
		if objectKeeper.Spec.FollowObjectRef.UID != string(obj.GetUID()) {
			return nil, ctrl.Result{}, fmt.Errorf("ObjectKeeper %s belongs to another Snapshot (UID mismatch)", retainerName)
		}
		wantSpec := r.desiredUnifiedRootObjectKeeperSpec(obj)
		if !unifiedRootObjectKeeperSpecMatches(&wantSpec, objectKeeper) {
			objectKeeper.Spec = wantSpec
			if err := r.Update(ctx, objectKeeper); err != nil {
				return nil, ctrl.Result{}, fmt.Errorf("update ObjectKeeper %s (spec drift): %w", retainerName, err)
			}
			logger.Info("updated unified root ObjectKeeper spec (TTL/mode drift)", "name", retainerName)
			if err := r.APIReader.Get(ctx, client.ObjectKey{Name: retainerName}, objectKeeper); err != nil {
				return nil, ctrl.Result{RequeueAfter: 200 * time.Millisecond}, nil
			}
		}
		logger.V(1).Info("ObjectKeeper already exists", "name", retainerName)
		return objectKeeper, ctrl.Result{}, nil
	}
}

func (r *SnapshotController) desiredUnifiedRootObjectKeeperSpec(obj *unstructured.Unstructured) deckhousev1alpha1.ObjectKeeperSpec {
	gvk := obj.GetObjectKind().GroupVersionKind()
	ttl := config.DefaultSnapshotRootOKTTL
	if r.Config != nil && r.Config.SnapshotRootOKTTL > 0 {
		ttl = r.Config.SnapshotRootOKTTL
	}
	return deckhousev1alpha1.ObjectKeeperSpec{
		Mode: ObjectKeeperModeFollowObjectWithTTL,
		FollowObjectRef: &deckhousev1alpha1.FollowObjectRef{
			APIVersion: gvk.GroupVersion().String(),
			Kind:       gvk.Kind,
			Namespace:  obj.GetNamespace(),
			Name:       obj.GetName(),
			UID:        string(obj.GetUID()),
		},
		TTL: &metav1.Duration{Duration: ttl},
	}
}

func unifiedRootObjectKeeperSpecMatches(want *deckhousev1alpha1.ObjectKeeperSpec, got *deckhousev1alpha1.ObjectKeeper) bool {
	if got.Spec.Mode != want.Mode {
		return false
	}
	if got.Spec.FollowObjectRef == nil || want.FollowObjectRef == nil {
		return false
	}
	fr, w := got.Spec.FollowObjectRef, want.FollowObjectRef
	if fr.APIVersion != w.APIVersion || fr.Kind != w.Kind || fr.Name != w.Name || fr.Namespace != w.Namespace || fr.UID != w.UID {
		return false
	}
	if got.Spec.TTL == nil || want.TTL == nil || got.Spec.TTL.Duration != want.TTL.Duration {
		return false
	}
	return true
}

// propagateReadyFalseToParent propagates Ready=False to parent Snapshot if:
// - Snapshot was Ready=True
// - Parent exists and is not being deleted
// - Parent was Ready=True
// This implements the tree consistency rule from deletion algorithm
func (r *SnapshotController) propagateReadyFalseToParent(
	ctx context.Context,
	_ snapshot.SnapshotLike,
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

	// Get parent Snapshot - resolve GVK through registry
	parentGVK, err := r.GVKRegistry.ResolveSnapshotGVK(parentRef.Kind)
	if err != nil {
		// Fallback: parse from APIVersion if registry doesn't know this GVK
		// This handles edge cases like core APIs or dynamically discovered CRDs
		if idx := strings.Index(parentRef.APIVersion, "/"); idx != -1 {
			parentGVK = schema.GroupVersionKind{
				Group:   parentRef.APIVersion[:idx],
				Version: parentRef.APIVersion[idx+1:],
				Kind:    parentRef.Kind,
			}
		} else {
			parentGVK = schema.GroupVersionKind{
				Group:   "",
				Version: parentRef.APIVersion,
				Kind:    parentRef.Kind,
			}
		}
		logger.V(1).Info("GVK not found in registry, using fallback parsing", "kind", parentRef.Kind)
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

// checkConsistencyAndSetReady checks if SnapshotContent and children exist and sets Ready condition
// According to ADR: Ready=False выставляется только для ранее успешных объектов
func (r *SnapshotController) checkConsistencyAndSetReady(
	ctx context.Context,
	snapshotLike snapshot.SnapshotLike,
	obj *unstructured.Unstructured,
) error {
	logger := log.FromContext(ctx)
	wasReady := snapshot.IsReady(snapshotLike)

	// Step 1: Check if SnapshotContent exists
	contentName := snapshotLike.GetStatusContentName()
	if contentName == "" {
		if wasReady {
			// Content was lost - set Ready=False
			snapshot.SetCondition(snapshotLike, snapshot.ConditionReady, metav1.ConditionFalse,
				snapshot.ReasonContentMissing, "SnapshotContent not found")
			snapshot.SyncConditionsToUnstructured(obj, snapshotLike.GetStatusConditions())
			if err := r.Status().Update(ctx, obj); err != nil {
				return fmt.Errorf("failed to update Ready=False: %w", err)
			}
			logger.Info("SnapshotContent missing, set Ready=False")
			// Propagate Ready=False to parent
			return r.propagateReadyFalseToParent(ctx, snapshotLike, obj)
		}
		return nil // Content missing, but Snapshot was never Ready
	}

	// Step 2: Get SnapshotContent and check its Ready state
	contentGVK, err := r.getSnapshotContentGVK(obj.GetObjectKind().GroupVersionKind())
	if err != nil {
		return fmt.Errorf("failed to resolve SnapshotContent GVK: %w", err)
	}
	contentObj := &unstructured.Unstructured{}
	contentObj.SetGroupVersionKind(contentGVK)
	contentKey := client.ObjectKey{Name: contentName}

	if err := r.APIReader.Get(ctx, contentKey, contentObj); err != nil {
		if errors.IsNotFound(err) {
			if wasReady {
				// Content was deleted - set Ready=False
				snapshot.SetCondition(snapshotLike, snapshot.ConditionReady, metav1.ConditionFalse,
					snapshot.ReasonContentMissing, fmt.Sprintf("SnapshotContent %s not found", contentName))
				snapshot.SyncConditionsToUnstructured(obj, snapshotLike.GetStatusConditions())
				if err := r.Status().Update(ctx, obj); err != nil {
					return fmt.Errorf("failed to update Ready=False: %w", err)
				}
				logger.Info("SnapshotContent deleted, set Ready=False", "content", contentName)
				// Propagate Ready=False to parent
				return r.propagateReadyFalseToParent(ctx, snapshotLike, obj)
			}
			return nil // Content missing, but Snapshot was never Ready
		}
		return fmt.Errorf("failed to get SnapshotContent: %w", err)
	}

	// Check if Content is being deleted
	if !contentObj.GetDeletionTimestamp().IsZero() {
		if wasReady {
			// Content is being deleted - set Ready=False
			snapshot.SetCondition(snapshotLike, snapshot.ConditionReady, metav1.ConditionFalse,
				snapshot.ReasonDeleting, fmt.Sprintf("SnapshotContent %s is being deleted", contentName))
			snapshot.SyncConditionsToUnstructured(obj, snapshotLike.GetStatusConditions())
			if err := r.Status().Update(ctx, obj); err != nil {
				return fmt.Errorf("failed to update Ready=False: %w", err)
			}
			logger.Info("SnapshotContent deleting, set Ready=False", "content", contentName)
			// Propagate Ready=False to parent
			return r.propagateReadyFalseToParent(ctx, snapshotLike, obj)
		}
		return nil
	}

	// Check Content Ready condition
	contentLike, err := snapshot.ExtractSnapshotContentLike(contentObj)
	if err != nil {
		return fmt.Errorf("failed to extract SnapshotContentLike: %w", err)
	}

	if !snapshot.IsReady(contentLike) {
		if wasReady {
			// Content is not Ready - set Ready=False
			readyCond := snapshot.GetCondition(contentLike, snapshot.ConditionReady)
			reason := snapshot.ReasonContentMissing
			message := fmt.Sprintf("SnapshotContent %s is not Ready", contentName)
			if readyCond != nil {
				reason = readyCond.Reason
				message = fmt.Sprintf("SnapshotContent %s: %s", contentName, readyCond.Message)
			}
			snapshot.SetCondition(snapshotLike, snapshot.ConditionReady, metav1.ConditionFalse, reason, message)
			snapshot.SyncConditionsToUnstructured(obj, snapshotLike.GetStatusConditions())
			if err := r.Status().Update(ctx, obj); err != nil {
				return fmt.Errorf("failed to update Ready=False: %w", err)
			}
			logger.Info("SnapshotContent not Ready, set Ready=False", "content", contentName, "reason", reason)
			// Propagate Ready=False to parent
			return r.propagateReadyFalseToParent(ctx, snapshotLike, obj)
		}
		return nil // Content not Ready, but Snapshot was never Ready
	}

	// Step 3: Check if all child Snapshots exist
	childrenRefs := snapshotLike.GetStatusChildrenSnapshotRefs()
	for _, childRef := range childrenRefs {
		// Skip if Snapshot is being deleted (cascade deletion)
		if !obj.GetDeletionTimestamp().IsZero() {
			continue
		}

		childExists, err := r.checkChildSnapshotExists(ctx, &childRef)
		if err != nil {
			return fmt.Errorf("failed to check child Snapshot: %w", err)
		}

		if !childExists {
			if wasReady {
				// Child was deleted - set Ready=False
				snapshot.SetCondition(snapshotLike, snapshot.ConditionReady, metav1.ConditionFalse,
					snapshot.ReasonChildSnapshotMissing,
					fmt.Sprintf("Child Snapshot %s/%s not found", childRef.Namespace, childRef.Name))
				snapshot.SyncConditionsToUnstructured(obj, snapshotLike.GetStatusConditions())
				if err := r.Status().Update(ctx, obj); err != nil {
					return fmt.Errorf("failed to update Ready=False: %w", err)
				}
				logger.Info("Child Snapshot missing, set Ready=False",
					"child", fmt.Sprintf("%s/%s", childRef.Namespace, childRef.Name))
				// Propagate Ready=False to parent
				return r.propagateReadyFalseToParent(ctx, snapshotLike, obj)
			}
			return nil // Child missing, but Snapshot was never Ready
		}
	}

	// All checks passed - set Ready=True if not already set
	if !wasReady {
		// Check if InProgress should be cleared
		if snapshot.IsInProgress(snapshotLike) {
			snapshot.SetCondition(snapshotLike, snapshot.ConditionInProgress, metav1.ConditionFalse,
				snapshot.ReasonCompleted, "SnapshotContent and children are ready")
		}
		snapshot.SetCondition(snapshotLike, snapshot.ConditionReady, metav1.ConditionTrue,
			snapshot.ReasonCompleted, "SnapshotContent and all children are ready")
		snapshot.SyncConditionsToUnstructured(obj, snapshotLike.GetStatusConditions())
		if err := r.Status().Update(ctx, obj); err != nil {
			return fmt.Errorf("failed to update Ready=True: %w", err)
		}
		logger.Info("All checks passed, set Ready=True")
	}

	return nil
}

// checkChildSnapshotExists checks if a child Snapshot exists
// Uses APIReader for read-after-write consistency
func (r *SnapshotController) checkChildSnapshotExists(ctx context.Context, childRef *snapshot.ObjectRef) (bool, error) {
	if childRef == nil {
		return false, nil
	}

	// Resolve child Snapshot GVK through registry
	childGVK, err := r.GVKRegistry.ResolveSnapshotGVK(childRef.Kind)
	if err != nil {
		// Fallback: try to find matching GVK from registered list
		// This handles cases where child snapshot type is not yet registered
		logger := log.FromContext(ctx)
		logger.V(1).Info("Child GVK not found in registry, trying registered GVKs", "kind", childRef.Kind)

		// Try to find a matching GVK by Kind
		for _, gvk := range r.snapshotGVKsSnapshot() {
			if gvk.Kind == childRef.Kind {
				childGVK = gvk
				break
			}
		}

		// If still not found, return error
		if childGVK.Kind == "" {
			return false, fmt.Errorf("child Snapshot GVK not found for kind %s: %w", childRef.Kind, err)
		}
	}

	childObj := &unstructured.Unstructured{}
	childObj.SetGroupVersionKind(childGVK)
	childKey := client.ObjectKey{
		Name:      childRef.Name,
		Namespace: childRef.Namespace,
	}

	getErr := r.APIReader.Get(ctx, childKey, childObj)
	if errors.IsNotFound(getErr) {
		return false, nil
	}
	if getErr != nil {
		return false, fmt.Errorf("failed to get child Snapshot: %w", getErr)
	}

	return true, nil
}

// getSnapshotContentGVK derives SnapshotContent GVK from Snapshot GVK using registry
// Example: virtualization.deckhouse.io/v1alpha1.VirtualMachineSnapshot -> virtualization.deckhouse.io/v1alpha1.VirtualMachineSnapshotContent
func (r *SnapshotController) getSnapshotContentGVK(snapshotGVK schema.GroupVersionKind) (schema.GroupVersionKind, error) {
	return r.GVKRegistry.ResolveSnapshotContentGVK(snapshotGVK.Kind)
}

func (r *SnapshotController) snapshotGVKsSnapshot() []schema.GroupVersionKind {
	r.watchMu.RLock()
	defer r.watchMu.RUnlock()
	out := make([]schema.GroupVersionKind, len(r.SnapshotGVKs))
	copy(out, r.SnapshotGVKs)
	return out
}

// registerSnapshotWatch calls builder.Complete. When the manager is already running, this relies on
// controller-runtime allowing new runnables via Add — behavior is runtime-sensitive; upgrade c-r with care.
func (r *SnapshotController) registerSnapshotWatch(mgr ctrl.Manager, gvk, contentGVK schema.GroupVersionKind) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	contentObj := &unstructured.Unstructured{}
	contentObj.SetGroupVersionKind(contentGVK)
	builder := ctrl.NewControllerManagedBy(mgr).
		For(obj).
		Watches(
			contentObj,
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
				return r.mapSnapshotContentToSnapshot(ctx, o)
			}),
		).
		Named(fmt.Sprintf("snapshot-%s-%s", gvk.Group, gvk.Kind))
	return builder.Complete(r)
}

// AddWatchForPair registers Snapshot + SnapshotContent watches for one pair at runtime (after manager.New).
// Idempotent per snapshot GVK. Uses explicit content GVK (DSC mapping may differ from Kind+"Content").
// If watch setup fails after a new slice entry was appended, that entry is removed and registry entries
// matching this exact pair are reverted (see GVKRegistry.RevertSnapshotRegistrationIfExact). If the
// snapshot GVK was already in the slice (bootstrap), registry is not reverted on failure.
func (r *SnapshotController) AddWatchForPair(mgr ctrl.Manager, snapshotGVK, contentGVK schema.GroupVersionKind) error {
	r.watchMu.Lock()
	defer r.watchMu.Unlock()
	if r.activeSnapshotWatchSet == nil {
		r.activeSnapshotWatchSet = make(map[string]struct{})
	}
	key := snapshotGVK.String()
	if _, ok := r.activeSnapshotWatchSet[key]; ok {
		return nil
	}
	if err := r.GVKRegistry.RegisterSnapshotContentMapping(
		snapshotGVK.Kind, snapshotGVK.GroupVersion().String(),
		contentGVK.Kind, contentGVK.GroupVersion().String(),
	); err != nil {
		return fmt.Errorf("register snapshot/content mapping: %w", err)
	}
	needAppend := true
	for _, g := range r.SnapshotGVKs {
		if g == snapshotGVK {
			needAppend = false
			break
		}
	}
	if needAppend {
		r.SnapshotGVKs = append(r.SnapshotGVKs, snapshotGVK)
	}
	if err := r.registerSnapshotWatch(mgr, snapshotGVK, contentGVK); err != nil {
		if needAppend {
			r.SnapshotGVKs = r.SnapshotGVKs[:len(r.SnapshotGVKs)-1]
			r.GVKRegistry.RevertSnapshotRegistrationIfExact(snapshotGVK.Kind, snapshotGVK, contentGVK)
		}
		return fmt.Errorf("setup snapshot watch for %s: %w", snapshotGVK.String(), err)
	}
	r.activeSnapshotWatchSet[key] = struct{}{}
	return nil
}

// SetupWithManager sets up the controller with the Manager
// Registers watches for all registered Snapshot GVKs and their corresponding SnapshotContent GVKs
// Each GVK gets its own controller instance to ensure correct GVK context
func (r *SnapshotController) SetupWithManager(mgr ctrl.Manager) error {
	r.watchMu.Lock()
	defer r.watchMu.Unlock()
	if r.activeSnapshotWatchSet == nil {
		r.activeSnapshotWatchSet = make(map[string]struct{})
	}
	for _, gvk := range r.SnapshotGVKs {
		key := gvk.String()
		if _, ok := r.activeSnapshotWatchSet[key]; ok {
			continue
		}
		contentGVK, err := r.getSnapshotContentGVK(gvk)
		if err != nil {
			return fmt.Errorf("failed to resolve SnapshotContent GVK for %s: %w", gvk.String(), err)
		}
		if err := r.registerSnapshotWatch(mgr, gvk, contentGVK); err != nil {
			return fmt.Errorf("failed to setup watch for Snapshot GVK %s: %w", gvk.String(), err)
		}
		r.activeSnapshotWatchSet[key] = struct{}{}
	}
	return nil
}

// mapSnapshotContentToSnapshot maps SnapshotContent to its corresponding Snapshot for reconcile
// This ensures SnapshotController reconciles Snapshot when SnapshotContent changes (e.g., becomes Ready=True)
// Signature matches handler.MapFunc = TypedMapFunc[client.Object, reconcile.Request]
// which is func(context.Context, client.Object) []reconcile.Request
func (r *SnapshotController) mapSnapshotContentToSnapshot(ctx context.Context, obj client.Object) []reconcile.Request {
	contentObj, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil
	}

	// Extract snapshotRef from SnapshotContent spec
	spec, ok := contentObj.Object["spec"].(map[string]interface{})
	if !ok {
		log.FromContext(ctx).V(1).Info("SnapshotContent spec is missing or invalid", "content", contentObj.GetName())
		return nil
	}

	snapshotRef, ok := spec["snapshotRef"].(map[string]interface{})
	if !ok {
		log.FromContext(ctx).V(1).Info("SnapshotContent spec.snapshotRef is missing or invalid", "content", contentObj.GetName())
		return nil
	}

	kind, ok := snapshotRef["kind"].(string)
	if !ok || kind == "" {
		log.FromContext(ctx).V(1).Info("SnapshotContent spec.snapshotRef.kind is missing", "content", contentObj.GetName())
		return nil
	}

	name, ok := snapshotRef["name"].(string)
	if !ok || name == "" {
		log.FromContext(ctx).V(1).Info("SnapshotContent spec.snapshotRef.name is missing", "content", contentObj.GetName())
		return nil
	}

	namespace, ok := snapshotRef["namespace"].(string)
	if !ok || namespace == "" {
		if kind != "ClusterSnapshot" {
			log.FromContext(ctx).V(1).Info("SnapshotContent spec.snapshotRef.namespace is missing", "content", contentObj.GetName(), "kind", kind)
			return nil
		}
	}

	// Determine Snapshot Kind from SnapshotContent GVK (registry-aware)
	contentGVK := contentObj.GroupVersionKind()
	snapshotKind, err := r.GVKRegistry.ResolveSnapshotKindByContentGVK(contentGVK)
	if err != nil {
		log.FromContext(ctx).V(1).Info("Snapshot Kind resolution failed for SnapshotContent", "content", contentObj.GetName(), "gvk", contentGVK.String(), "error", err)
		return nil
	}

	snapshotGVK, err := r.GVKRegistry.ResolveSnapshotGVK(snapshotKind)
	if err != nil {
		log.FromContext(ctx).V(1).Info("Snapshot GVK not registered for SnapshotContent", "snapshotKind", snapshotKind, "content", contentObj.GetName(), "error", err)
		return nil
	}
	if snapshotGVK.Group != contentGVK.Group {
		log.FromContext(ctx).V(1).Info("Snapshot GVK group mismatch for SnapshotContent", "snapshotKind", snapshotKind, "snapshotGroup", snapshotGVK.Group, "contentGroup", contentGVK.Group, "content", contentObj.GetName())
		return nil
	}

	return []reconcile.Request{
		{
			NamespacedName: types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			},
		},
	}
}
