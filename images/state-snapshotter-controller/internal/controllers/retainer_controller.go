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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	iretainer "github.com/deckhouse/state-snapshotter/api/v1alpha1/iretainer"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

const (
	// ReconcileInterval is the interval for periodic reconciliation
	ReconcileInterval = 5 * time.Minute
	// FollowObjectCheckInterval is the interval for checking FollowObject status
	FollowObjectCheckInterval = 2 * time.Minute
	// TTLCheckInterval is the interval for checking TTL expiration
	TTLCheckInterval = 1 * time.Minute
	// TTLStartTimestampAnnotation is the annotation key for tracking TTL start time
	// Used in:
	// - TTL mode for checkpoint retainers: stores MCR completionTimestamp
	// - FollowObjectWithTTL mode: stores timestamp when followed object disappeared
	// This ensures TTL is calculated from the correct start time
	TTLStartTimestampAnnotation = "retainer.deckhouse.io/ttl-start-timestamp"
	// CheckpointRetainerLabel is the label key to identify checkpoint retainers
	// This makes checkpoint retainer identification explicit and future-proof
	CheckpointRetainerLabel = "state-snapshotter.deckhouse.io/checkpoint-retainer"
	// CheckpointRetainerLabelValue is the label value for checkpoint retainers
	CheckpointRetainerLabelValue = "true"
)

// RetainerController reconciles Retainer objects
// This is a system controller that manages the lifecycle of Retainer resources
// It requires privileged access to GET any namespaced objects
type RetainerController struct {
	client.Client
	Scheme     *runtime.Scheme
	Logger     logger.LoggerInterface
	dyn        dynamic.Interface
	restMapper meta.RESTMapper
}

func (r *RetainerController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	r.Logger.Info("Reconciling Retainer", "name", req.Name)

	retainer := &iretainer.IRetainer{}
	if err := r.Get(ctx, req.NamespacedName, retainer); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Handle deletion
	// No finalizer needed - GC handles cleanup via ownerReferences
	if !retainer.DeletionTimestamp.IsZero() {
		// Retainer is being deleted, nothing to do
		// GC will handle cleanup of dependent objects
		return ctrl.Result{}, nil
	}

	// CRITICAL: Skip checkpoint retainers entirely – they do not depend on namespaces
	// Checkpoint retainers are identified ONLY by label (single source of truth)
	isCheckpointRetainer := retainer.Labels != nil &&
		retainer.Labels[CheckpointRetainerLabel] == CheckpointRetainerLabelValue
	if isCheckpointRetainer {
		r.Logger.Debug("Checkpoint retainer detected, skipping namespace lifecycle checks",
			"retainer", retainer.Name)
		// Checkpoint retainers use TTL mode and are independent of namespace lifecycle
		return r.reconcileTTL(ctx, retainer)
	}

	// Check if retainer follows an object in a namespace that is being deleted
	// This handles namespace deletion events that enqueue retainers
	// Note: checkpoint retainers are already handled above and will never reach this point
	if retainer.Spec.FollowObjectRef != nil && retainer.Spec.FollowObjectRef.Namespace != "" {
		ns := &corev1.Namespace{}
		if err := r.Get(ctx, client.ObjectKey{Name: retainer.Spec.FollowObjectRef.Namespace}, ns); err == nil {
			if ns.DeletionTimestamp != nil {
				// Namespace is being deleted - handle it
				if err := r.handleNamespaceDeletion(ctx, retainer, retainer.Spec.FollowObjectRef.Namespace); err != nil {
					return ctrl.Result{}, err
				}
				// Retainer was deleted or will be deleted
				return ctrl.Result{}, nil
			}
		} else if !errors.IsNotFound(err) {
			r.Logger.Error(err, "Failed to get namespace", "namespace", retainer.Spec.FollowObjectRef.Namespace)
		}
	}

	// Process based on mode
	switch retainer.Spec.Mode {
	case "FollowObject":
		return r.reconcileFollowObject(ctx, retainer)
	case "TTL":
		return r.reconcileTTL(ctx, retainer)
	case "FollowObjectWithTTL":
		return r.reconcileFollowObjectWithTTL(ctx, retainer)
	default:
		return r.setStatusCondition(ctx, retainer, metav1.Condition{
			Type:               ConditionTypeActive,
			Status:             metav1.ConditionFalse,
			Reason:             RetainerConditionReasonInvalidMode,
			Message:            fmt.Sprintf("Unknown mode: %s", retainer.Spec.Mode),
			LastTransitionTime: metav1.Now(),
		})
	}
}

// reconcileFollowObject handles Retainer in FollowObject mode
func (r *RetainerController) reconcileFollowObject(ctx context.Context, retainer *iretainer.IRetainer) (ctrl.Result, error) {
	if retainer.Spec.FollowObjectRef == nil {
		return r.setStatusCondition(ctx, retainer, metav1.Condition{
			Type:               ConditionTypeActive,
			Status:             metav1.ConditionFalse,
			Reason:             RetainerConditionReasonMissingFollowObjectRef,
			Message:            "FollowObjectRef is required for FollowObject mode",
			LastTransitionTime: metav1.Now(),
		})
	}

	ref := retainer.Spec.FollowObjectRef

	// Check namespace lifecycle first
	if ref.Namespace != "" {
		ns := &corev1.Namespace{}
		if err := r.Get(ctx, client.ObjectKey{Name: ref.Namespace}, ns); err == nil {
			if ns.Status.Phase == corev1.NamespaceTerminating {
				// Namespace is terminating - delete Retainer
				r.Logger.Info("Namespace is terminating - deleting Retainer",
					"retainer", retainer.Name,
					"namespace", ref.Namespace)
				if err := r.Delete(ctx, retainer); err != nil {
					return ctrl.Result{}, fmt.Errorf("failed to delete Retainer: %w", err)
				}
				return ctrl.Result{}, nil
			}
		} else if !errors.IsNotFound(err) {
			r.Logger.Error(err, "Failed to get namespace", "namespace", ref.Namespace)
			return ctrl.Result{RequeueAfter: FollowObjectCheckInterval}, nil
		}
	}

	// Parse APIVersion to get Group and Version
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return r.setStatusCondition(ctx, retainer, metav1.Condition{
			Type:               ConditionTypeActive,
			Status:             metav1.ConditionFalse,
			Reason:             RetainerConditionReasonInvalidAPIVersion,
			Message:            fmt.Sprintf("Invalid APIVersion: %s", ref.APIVersion),
			LastTransitionTime: metav1.Now(),
		})
	}

	// Determine resource from Kind (with strict APIVersion validation)
	resource := r.kindToResource(ref.Kind, gv)

	// Get the object using dynamic client with STRICT APIVersion
	// This ensures we use the exact API group specified in FollowObjectRef,
	// preventing conflicts when multiple CRDs with same Kind exist
	gvr := schema.GroupVersionResource{
		Group:    gv.Group,
		Version:  gv.Version,
		Resource: resource,
	}

	r.Logger.Info("Looking up FollowObject using strict GVR",
		"retainer", retainer.Name,
		"gvr", gvr.String(),
		"object", fmt.Sprintf("%s/%s/%s", ref.APIVersion, ref.Kind, ref.Name),
		"namespace", ref.Namespace)

	obj, err := r.dyn.Resource(gvr).Namespace(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			// Object not found - delete Retainer
			r.Logger.Info("FollowObject not found - deleting Retainer",
				"retainer", retainer.Name,
				"object", fmt.Sprintf("%s/%s/%s", ref.APIVersion, ref.Kind, ref.Name),
				"namespace", ref.Namespace)
			if err := r.Delete(ctx, retainer); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to delete Retainer: %w", err)
			}
			return ctrl.Result{}, nil
		}
		// Other error - retry
		r.Logger.Error(err, "Failed to get FollowObject",
			"retainer", retainer.Name,
			"object", fmt.Sprintf("%s/%s/%s", ref.APIVersion, ref.Kind, ref.Name))
		return ctrl.Result{RequeueAfter: FollowObjectCheckInterval}, nil
	}

	// Verify APIVersion matches (critical: prevent using wrong CRD)
	objAPIVersion := obj.GetAPIVersion()
	if objAPIVersion != ref.APIVersion {
		r.Logger.Error(nil, "Object APIVersion mismatch - wrong CRD detected, deleting Retainer",
			"retainer", retainer.Name,
			"expected", ref.APIVersion,
			"got", objAPIVersion,
			"object", fmt.Sprintf("%s/%s/%s", ref.APIVersion, ref.Kind, ref.Name),
			"namespace", ref.Namespace)
		// In FollowObject mode, delete Retainer immediately if wrong CRD detected
		if err := r.Delete(ctx, retainer); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to delete Retainer due to APIVersion mismatch: %w", err)
		}
		return ctrl.Result{}, nil
	}

	// Verify UID matches
	objUID := string(obj.GetUID())
	if objUID != ref.UID {
		// Object was recreated with different UID - delete Retainer
		r.Logger.Info("FollowObject UID mismatch - deleting Retainer",
			"retainer", retainer.Name,
			"expectedUID", ref.UID,
			"actualUID", objUID)
		if err := r.Delete(ctx, retainer); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to delete Retainer: %w", err)
		}
		return ctrl.Result{}, nil
	}

	// Object exists and UID matches - Retainer is active
	message := fmt.Sprintf("Following object %s/%s/%s", ref.APIVersion, ref.Kind, ref.Name)
	return r.setStatusCondition(ctx, retainer, metav1.Condition{
		Type:               ConditionTypeActive,
		Status:             metav1.ConditionTrue,
		Reason:             RetainerConditionReasonObjectExists,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	}, ctrl.Result{RequeueAfter: FollowObjectCheckInterval})
}

// reconcileTTL handles Retainer in TTL mode
// TTL starts from MCR completionTimestamp if retainer is for checkpoint, otherwise from CreationTimestamp
func (r *RetainerController) reconcileTTL(ctx context.Context, retainer *iretainer.IRetainer) (ctrl.Result, error) {
	if retainer.Spec.TTL == nil {
		return r.setStatusCondition(ctx, retainer, metav1.Condition{
			Type:               ConditionTypeActive,
			Status:             metav1.ConditionFalse,
			Reason:             RetainerConditionReasonMissingTTL,
			Message:            "TTL is required for TTL mode",
			LastTransitionTime: metav1.Now(),
		})
	}

	// Validate TTL duration
	if retainer.Spec.TTL.Duration <= 0 {
		return r.setStatusCondition(ctx, retainer, metav1.Condition{
			Type:               ConditionTypeActive,
			Status:             metav1.ConditionFalse,
			Reason:             RetainerConditionReasonInvalidTTL,
			Message:            fmt.Sprintf("Invalid TTL duration: %v (must be positive)", retainer.Spec.TTL.Duration),
			LastTransitionTime: metav1.Now(),
		})
	}

	// Determine TTL start time
	// For checkpoint retainers: TTL starts from MCR completionTimestamp (stored in annotation)
	// For other retainers: TTL starts from CreationTimestamp
	var ttlStartTime metav1.Time
	// Checkpoint retainers are identified ONLY by label (single source of truth)
	isCheckpointRetainer := retainer.Labels != nil &&
		retainer.Labels[CheckpointRetainerLabel] == CheckpointRetainerLabelValue
	if isCheckpointRetainer {
		// This is a checkpoint retainer - check annotation first
		if ttlStartStr, hasAnnotation := retainer.Annotations[TTLStartTimestampAnnotation]; hasAnnotation {
			// Use stored completionTimestamp from annotation
			parsedTime, err := time.Parse(time.RFC3339, ttlStartStr)
			if err == nil {
				ttlStartTime = metav1.NewTime(parsedTime)
			} else {
				// Invalid annotation - try to update it
				r.Logger.Warning("Invalid TTL start timestamp annotation, will try to update",
					"retainer", retainer.Name,
					"annotation", ttlStartStr,
					"error", err)
				ttlStartTime = retainer.CreationTimestamp
			}
		} else {
			// No annotation - try to get MCR completionTimestamp and store it
			// Extract checkpoint name from retainer name (ret-<checkpointName>)
			checkpointName := retainer.Name
			if len(checkpointName) > 4 && checkpointName[:4] == "ret-" {
				checkpointName = checkpointName[4:]
			}

			checkpoint := &storagev1alpha1.ManifestCheckpoint{}
			if err := r.Get(ctx, client.ObjectKey{Name: checkpointName}, checkpoint); err == nil {
				if checkpoint.Spec.ManifestCaptureRequestRef != nil {
					mcr := &storagev1alpha1.ManifestCaptureRequest{}
					mcrKey := client.ObjectKey{
						Namespace: checkpoint.Spec.ManifestCaptureRequestRef.Namespace,
						Name:      checkpoint.Spec.ManifestCaptureRequestRef.Name,
					}
					if err := r.Get(ctx, mcrKey, mcr); err == nil {
						if mcr.Status.CompletionTimestamp != nil {
							ttlStartTime = *mcr.Status.CompletionTimestamp
							// Store in annotation for future reconciles
							base := retainer.DeepCopy()
							if retainer.Annotations == nil {
								retainer.Annotations = make(map[string]string)
							}
							retainer.Annotations[TTLStartTimestampAnnotation] = ttlStartTime.Format(time.RFC3339)
							if err := r.Patch(ctx, retainer, client.MergeFrom(base)); err != nil {
								r.Logger.Error(err, "Failed to store TTL start timestamp in annotation",
									"retainer", retainer.Name)
								// Continue with calculated time
							}
						} else {
							// MCR not completed yet - use retainer creation time as fallback
							ttlStartTime = retainer.CreationTimestamp
						}
					} else {
						// MCR not found - use retainer creation time as fallback
						ttlStartTime = retainer.CreationTimestamp
					}
				} else {
					// No MCR ref - use retainer creation time as fallback
					ttlStartTime = retainer.CreationTimestamp
				}
			} else {
				// Checkpoint not found - use retainer creation time as fallback
				ttlStartTime = retainer.CreationTimestamp
			}
		}
	} else {
		// Not a checkpoint retainer - use CreationTimestamp
		ttlStartTime = retainer.CreationTimestamp
	}

	// Calculate expiration time
	expiresAt := ttlStartTime.Add(retainer.Spec.TTL.Duration)
	now := time.Now()

	if now.After(expiresAt) {
		// TTL expired - delete Retainer
		// WARNING: This will trigger GC to delete checkpoint via ownerRef
		r.Logger.Info("TTL expired - deleting Retainer (this will trigger GC to delete checkpoint)",
			"retainer", retainer.Name,
			"retainerUID", retainer.UID,
			"ttl", retainer.Spec.TTL.Duration,
			"startTime", ttlStartTime,
			"expired", expiresAt,
			"checkpointName", strings.TrimPrefix(retainer.Name, "ret-"))
		if err := r.Delete(ctx, retainer); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to delete Retainer: %w", err)
		}
		r.Logger.Info("Retainer deleted successfully - GC will delete checkpoint",
			"retainer", retainer.Name,
			"retainerUID", retainer.UID)
		return ctrl.Result{}, nil
	}

	// TTL not expired - Retainer is active
	remaining := expiresAt.Sub(now)
	message := fmt.Sprintf("TTL expires in %v", remaining.Round(time.Minute))
	return r.setStatusCondition(ctx, retainer, metav1.Condition{
		Type:               ConditionTypeActive,
		Status:             metav1.ConditionTrue,
		Reason:             RetainerConditionReasonTTLActive,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	}, ctrl.Result{RequeueAfter: TTLCheckInterval})
}

// reconcileFollowObjectWithTTL handles Retainer in FollowObjectWithTTL mode
// According to ADR: if object disappears, start TTL countdown (do NOT delete immediately)
func (r *RetainerController) reconcileFollowObjectWithTTL(ctx context.Context, retainer *iretainer.IRetainer) (ctrl.Result, error) {
	if retainer.Spec.FollowObjectRef == nil {
		return r.setStatusCondition(ctx, retainer, metav1.Condition{
			Type:               ConditionTypeActive,
			Status:             metav1.ConditionFalse,
			Reason:             RetainerConditionReasonMissingFollowObjectRef,
			Message:            "FollowObjectRef is required for FollowObjectWithTTL mode",
			LastTransitionTime: metav1.Now(),
		})
	}

	if retainer.Spec.TTL == nil {
		return r.setStatusCondition(ctx, retainer, metav1.Condition{
			Type:               ConditionTypeActive,
			Status:             metav1.ConditionFalse,
			Reason:             RetainerConditionReasonMissingTTL,
			Message:            "TTL is required for FollowObjectWithTTL mode",
			LastTransitionTime: metav1.Now(),
		})
	}

	ref := retainer.Spec.FollowObjectRef

	// Check namespace lifecycle first
	if ref.Namespace != "" {
		ns := &corev1.Namespace{}
		if err := r.Get(ctx, client.ObjectKey{Name: ref.Namespace}, ns); err == nil {
			if ns.Status.Phase == corev1.NamespaceTerminating {
				// Namespace is terminating - delete Retainer
				r.Logger.Info("Namespace is terminating - deleting Retainer",
					"retainer", retainer.Name,
					"namespace", ref.Namespace)
				if err := r.Delete(ctx, retainer); err != nil {
					return ctrl.Result{}, fmt.Errorf("failed to delete Retainer: %w", err)
				}
				return ctrl.Result{}, nil
			}
		} else if !errors.IsNotFound(err) {
			r.Logger.Error(err, "Failed to get namespace", "namespace", ref.Namespace)
			return ctrl.Result{RequeueAfter: FollowObjectCheckInterval}, nil
		}
	}

	// Parse APIVersion to get Group and Version
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return r.setStatusCondition(ctx, retainer, metav1.Condition{
			Type:               ConditionTypeActive,
			Status:             metav1.ConditionFalse,
			Reason:             RetainerConditionReasonInvalidAPIVersion,
			Message:            fmt.Sprintf("Invalid APIVersion: %s", ref.APIVersion),
			LastTransitionTime: metav1.Now(),
		})
	}

	// Determine resource from Kind using RESTMapper (with strict APIVersion validation)
	resource := r.kindToResource(ref.Kind, gv)

	// Get the object using dynamic client with STRICT APIVersion
	// This ensures we use the exact API group specified in FollowObjectRef,
	// preventing conflicts when multiple CRDs with same Kind exist
	gvr := schema.GroupVersionResource{
		Group:    gv.Group,
		Version:  gv.Version,
		Resource: resource,
	}

	r.Logger.Info("Looking up FollowObject using strict GVR",
		"retainer", retainer.Name,
		"gvr", gvr.String(),
		"object", fmt.Sprintf("%s/%s/%s", ref.APIVersion, ref.Kind, ref.Name),
		"namespace", ref.Namespace)

	obj, err := r.dyn.Resource(gvr).Namespace(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			r.Logger.Info("FollowObject not found (NotFound error)",
				"retainer", retainer.Name,
				"object", fmt.Sprintf("%s/%s/%s", ref.APIVersion, ref.Kind, ref.Name),
				"namespace", ref.Namespace,
				"gvr", gvr.String())
			// Object not found - start TTL countdown (do NOT delete immediately)
			// Check if we already started TTL countdown
			ttlStartStr := retainer.Annotations[TTLStartTimestampAnnotation]
			if ttlStartStr == "" {
				// First time object disappeared - start TTL
				now := metav1.Now()
				nowStr := now.Format(time.RFC3339)
				base := retainer.DeepCopy()
				if retainer.Annotations == nil {
					retainer.Annotations = make(map[string]string)
				}
				// Set ttl-start-timestamp: TTL starts from object deletion time
				retainer.Annotations[TTLStartTimestampAnnotation] = nowStr
				if err := r.Patch(ctx, retainer, client.MergeFrom(base)); err != nil {
					return ctrl.Result{}, fmt.Errorf("failed to start TTL countdown: %w", err)
				}
				r.Logger.Info("Object disappeared - starting TTL countdown",
					"retainer", retainer.Name,
					"object", fmt.Sprintf("%s/%s/%s", ref.APIVersion, ref.Kind, ref.Name),
					"namespace", ref.Namespace,
					"ttl", retainer.Spec.TTL.Duration,
					"ttlStart", nowStr)
				// Requeue immediately to check TTL
				return ctrl.Result{RequeueAfter: TTLCheckInterval}, nil
			}

			// TTL countdown already started - check if expired

			ttlStartTime, err := time.Parse(time.RFC3339, ttlStartStr)
			if err != nil {
				// Invalid timestamp - reset
				base := retainer.DeepCopy()
				now := metav1.Now()
				nowStr := now.Format(time.RFC3339)
				if retainer.Annotations == nil {
					retainer.Annotations = make(map[string]string)
				}
				retainer.Annotations[TTLStartTimestampAnnotation] = nowStr
				if err := r.Patch(ctx, retainer, client.MergeFrom(base)); err != nil {
					return ctrl.Result{}, fmt.Errorf("failed to reset ttl-start-timestamp: %w", err)
				}
				return ctrl.Result{RequeueAfter: TTLCheckInterval}, nil
			}

			expiresAt := ttlStartTime.Add(retainer.Spec.TTL.Duration)
			now := time.Now()

			if now.After(expiresAt) {
				// TTL expired - delete Retainer
				r.Logger.Info("TTL expired after object disappearance - deleting Retainer",
					"retainer", retainer.Name,
					"object", fmt.Sprintf("%s/%s/%s", ref.APIVersion, ref.Kind, ref.Name),
					"namespace", ref.Namespace,
					"ttl", retainer.Spec.TTL.Duration,
					"ttlStart", ttlStartTime,
					"expired", expiresAt)
				if err := r.Delete(ctx, retainer); err != nil {
					return ctrl.Result{}, fmt.Errorf("failed to delete Retainer: %w", err)
				}
				return ctrl.Result{}, nil
			}

			// TTL not expired yet
			remaining := expiresAt.Sub(now)
			message := fmt.Sprintf("Object disappeared, TTL expires in %v", remaining.Round(time.Minute))
			return r.setStatusCondition(ctx, retainer, metav1.Condition{
				Type:               ConditionTypeActive,
				Status:             metav1.ConditionFalse,
				Reason:             RetainerConditionReasonObjectNotFound,
				Message:            message,
				LastTransitionTime: metav1.Now(),
			}, ctrl.Result{RequeueAfter: TTLCheckInterval})
		}
		// Other error - retry
		r.Logger.Error(err, "Failed to get FollowObject",
			"retainer", retainer.Name,
			"object", fmt.Sprintf("%s/%s/%s", ref.APIVersion, ref.Kind, ref.Name))
		return ctrl.Result{RequeueAfter: FollowObjectCheckInterval}, nil
	}

	// Object exists - verify APIVersion matches (critical: prevent using wrong CRD)
	objAPIVersion := obj.GetAPIVersion()
	objUID := string(obj.GetUID())
	r.Logger.Info("FollowObject found",
		"retainer", retainer.Name,
		"object", fmt.Sprintf("%s/%s/%s", ref.APIVersion, ref.Kind, ref.Name),
		"namespace", ref.Namespace,
		"objAPIVersion", objAPIVersion,
		"expectedAPIVersion", ref.APIVersion,
		"objUID", objUID,
		"expectedUID", ref.UID,
		"gvr", gvr.String())
	if objAPIVersion != ref.APIVersion {
		r.Logger.Error(nil, "Object APIVersion mismatch - wrong CRD detected",
			"retainer", retainer.Name,
			"expected", ref.APIVersion,
			"got", objAPIVersion,
			"object", fmt.Sprintf("%s/%s/%s", ref.APIVersion, ref.Kind, ref.Name),
			"namespace", ref.Namespace)
		// Treat as object not found - start TTL countdown
		// This prevents using wrong CRD when multiple CRDs with same Kind exist
		ttlStartStr := retainer.Annotations[TTLStartTimestampAnnotation]
		if ttlStartStr == "" {
			// First time APIVersion mismatch - start TTL
			now := metav1.Now()
			nowStr := now.Format(time.RFC3339)
			base := retainer.DeepCopy()
			if retainer.Annotations == nil {
				retainer.Annotations = make(map[string]string)
			}
			retainer.Annotations[TTLStartTimestampAnnotation] = nowStr
			if err := r.Patch(ctx, retainer, client.MergeFrom(base)); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to start TTL countdown after APIVersion mismatch: %w", err)
			}
			r.Logger.Info("APIVersion mismatch - starting TTL countdown",
				"retainer", retainer.Name,
				"ttl", retainer.Spec.TTL.Duration,
				"ttlStart", nowStr)
			return ctrl.Result{RequeueAfter: TTLCheckInterval}, nil
		}
		// TTL countdown already started - check if expired
		ttlStartTime, err := time.Parse(time.RFC3339, ttlStartStr)
		if err != nil {
			base := retainer.DeepCopy()
			now := metav1.Now()
			nowStr := now.Format(time.RFC3339)
			if retainer.Annotations == nil {
				retainer.Annotations = make(map[string]string)
			}
			retainer.Annotations[TTLStartTimestampAnnotation] = nowStr
			if err := r.Patch(ctx, retainer, client.MergeFrom(base)); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to reset ttl-start-timestamp: %w", err)
			}
			return ctrl.Result{RequeueAfter: TTLCheckInterval}, nil
		}
		expiresAt := ttlStartTime.Add(retainer.Spec.TTL.Duration)
		now := time.Now()
		if now.After(expiresAt) {
			r.Logger.Info("TTL expired after APIVersion mismatch - deleting Retainer",
				"retainer", retainer.Name,
				"ttl", retainer.Spec.TTL.Duration,
				"ttlStart", ttlStartTime,
				"expired", expiresAt)
			if err := r.Delete(ctx, retainer); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to delete Retainer: %w", err)
			}
			return ctrl.Result{}, nil
		}
		remaining := expiresAt.Sub(now)
		message := fmt.Sprintf("APIVersion mismatch (wrong CRD), TTL expires in %v", remaining.Round(time.Minute))
		return r.setStatusCondition(ctx, retainer, metav1.Condition{
			Type:               ConditionTypeActive,
			Status:             metav1.ConditionFalse,
			Reason:             RetainerConditionReasonObjectNotFound,
			Message:            message,
			LastTransitionTime: metav1.Now(),
		}, ctrl.Result{RequeueAfter: TTLCheckInterval})
	}

	// Object exists - verify UID matches
	// objUID was already declared above (line 586) for logging
	if objUID != ref.UID {
		// Object was recreated with different UID - treat as deletion, start TTL countdown
		ttlStartStr := retainer.Annotations[TTLStartTimestampAnnotation]
		if ttlStartStr == "" {
			// First time UID mismatch - start TTL
			now := metav1.Now()
			nowStr := now.Format(time.RFC3339)
			base := retainer.DeepCopy()
			if retainer.Annotations == nil {
				retainer.Annotations = make(map[string]string)
			}
			// Set ttl-start-timestamp: TTL starts from object recreation time
			retainer.Annotations[TTLStartTimestampAnnotation] = nowStr
			if err := r.Patch(ctx, retainer, client.MergeFrom(base)); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to start TTL countdown after UID mismatch: %w", err)
			}
			r.Logger.Info("FollowObject UID mismatch (recreated) - starting TTL countdown",
				"retainer", retainer.Name,
				"expectedUID", ref.UID,
				"actualUID", objUID,
				"ttl", retainer.Spec.TTL.Duration,
				"ttlStart", nowStr)
			return ctrl.Result{RequeueAfter: TTLCheckInterval}, nil
		}

		// TTL countdown already started - check if expired
		ttlStartTime, err := time.Parse(time.RFC3339, ttlStartStr)
		if err != nil {
			// Invalid timestamp - reset
			base := retainer.DeepCopy()
			now := metav1.Now()
			nowStr := now.Format(time.RFC3339)
			if retainer.Annotations == nil {
				retainer.Annotations = make(map[string]string)
			}
			retainer.Annotations[TTLStartTimestampAnnotation] = nowStr
			if err := r.Patch(ctx, retainer, client.MergeFrom(base)); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to reset ttl-start-timestamp: %w", err)
			}
			return ctrl.Result{RequeueAfter: TTLCheckInterval}, nil
		}

		expiresAt := ttlStartTime.Add(retainer.Spec.TTL.Duration)
		now := time.Now()

		if now.After(expiresAt) {
			// TTL expired - delete Retainer
			r.Logger.Info("TTL expired after UID mismatch - deleting Retainer",
				"retainer", retainer.Name,
				"expectedUID", ref.UID,
				"actualUID", objUID,
				"ttl", retainer.Spec.TTL.Duration,
				"ttlStart", ttlStartTime,
				"expired", expiresAt)
			if err := r.Delete(ctx, retainer); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to delete Retainer: %w", err)
			}
			return ctrl.Result{}, nil
		}

		// TTL not expired yet
		remaining := expiresAt.Sub(now)
		message := fmt.Sprintf("Object recreated (UID mismatch), TTL expires in %v", remaining.Round(time.Minute))
		return r.setStatusCondition(ctx, retainer, metav1.Condition{
			Type:               ConditionTypeActive,
			Status:             metav1.ConditionFalse,
			Reason:             RetainerConditionReasonUIDMismatch,
			Message:            message,
			LastTransitionTime: metav1.Now(),
		}, ctrl.Result{RequeueAfter: TTLCheckInterval})
	}

	// Object exists and UID matches - Retainer is active
	// Clear ttl-start-timestamp if it exists (object reappeared)
	// Only patch if annotation was actually present to avoid unnecessary updates
	if retainer.Annotations != nil {
		needsUpdate := false
		hadTTLStart := false
		// Only clear ttl-start-timestamp if it was set for FollowObjectWithTTL (not for checkpoint retainers)
		if _, hasTTLStart := retainer.Annotations[TTLStartTimestampAnnotation]; hasTTLStart {
			hadTTLStart = true
			// Check if this is a checkpoint retainer (has label)
			isCheckpointRetainer := retainer.Labels != nil &&
				retainer.Labels[CheckpointRetainerLabel] == CheckpointRetainerLabelValue
			if !isCheckpointRetainer {
				// This is FollowObjectWithTTL retainer - clear ttl-start-timestamp
				delete(retainer.Annotations, TTLStartTimestampAnnotation)
				needsUpdate = true
			}
		}
		if needsUpdate {
			base := retainer.DeepCopy()
			if err := r.Patch(ctx, retainer, client.MergeFrom(base)); err != nil {
				r.Logger.Error(err, "Failed to clear TTL annotations - object reappeared",
					"retainer", retainer.Name,
					"hadTTLStart", hadTTLStart)
			} else {
				r.Logger.Info("Cleared TTL annotations - object reappeared",
					"retainer", retainer.Name,
					"hadTTLStart", hadTTLStart)
			}
		}
	}

	message := fmt.Sprintf("Following object %s/%s/%s", ref.APIVersion, ref.Kind, ref.Name)
	return r.setStatusCondition(ctx, retainer, metav1.Condition{
		Type:               ConditionTypeActive,
		Status:             metav1.ConditionTrue,
		Reason:             RetainerConditionReasonObjectExists,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	}, ctrl.Result{RequeueAfter: FollowObjectCheckInterval})
}

// setStatusCondition safely updates retainer status condition
// Uses SubResource("status") for safe patching
func (r *RetainerController) setStatusCondition(ctx context.Context, retainer *iretainer.IRetainer, condition metav1.Condition, requeue ...ctrl.Result) (ctrl.Result, error) {
	base := retainer.DeepCopy()
	setSingleCondition(&retainer.Status.Conditions, condition)

	// Use SubResource("status") for safe patching
	if err := r.Status().Patch(ctx, retainer, client.MergeFrom(base)); err != nil {
		if errors.IsConflict(err) {
			// Conflict - requeue to retry
			r.Logger.Info("Status update conflict, requeuing", "retainer", retainer.Name)
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to update status: %w", err)
	}

	if len(requeue) > 0 {
		return requeue[0], nil
	}
	return ctrl.Result{}, nil
}

// kindToResource converts Kind to resource name using RESTMapper
// Falls back to heuristics if RESTMapper fails (e.g., for CRDs not yet discovered)
// IMPORTANT: Uses strict APIVersion matching to avoid conflicts when multiple CRDs
// with the same Kind exist in different API groups (e.g., deckhouse.io, state-snapshotter.deckhouse.io)
func (r *RetainerController) kindToResource(kind string, gv schema.GroupVersion) string {
	// Try RESTMapper first (cached, fast) but with STRICT validation
	if r.restMapper != nil {
		gvk := schema.GroupVersionKind{
			Group:   gv.Group,
			Version: gv.Version,
			Kind:    kind,
		}
		mapping, err := r.restMapper.RESTMapping(gvk.GroupKind(), gv.Version)
		if err == nil {
			// CRITICAL: Verify that RESTMapper returned the correct Group/Version
			// This prevents using wrong CRD when multiple CRDs with same Kind exist
			if mapping.Resource.Group == gv.Group && mapping.Resource.Version == gv.Version {
				return mapping.Resource.Resource
			}
			// RESTMapper returned wrong Group/Version - log warning and use heuristics
			r.Logger.Warning("RESTMapper returned wrong Group/Version, using heuristics",
				"kind", kind,
				"expected", gv.String(),
				"got", fmt.Sprintf("%s/%s", mapping.Resource.Group, mapping.Resource.Version),
				"resource", mapping.Resource.Resource)
		} else {
			// RESTMapper failed - fall back to heuristics
			r.Logger.Info("RESTMapper failed, using heuristics",
				"kind", kind,
				"groupVersion", gv.String(),
				"error", err)
		}
	}
	// Common Kubernetes core resources
	if gv.Group == "" {
		kindToResource := map[string]string{
			"ConfigMap":             "configmaps",
			"Secret":                "secrets",
			"Service":               "services",
			"PersistentVolumeClaim": "persistentvolumeclaims",
			"Pod":                   "pods",
			"ServiceAccount":        "serviceaccounts",
		}
		if resource, ok := kindToResource[kind]; ok {
			return resource
		}
	}

	// Common apps/v1 resources
	if gv.Group == "apps" && gv.Version == "v1" {
		kindToResource := map[string]string{
			"Deployment":  "deployments",
			"StatefulSet": "statefulsets",
			"DaemonSet":   "daemonsets",
			"ReplicaSet":  "replicasets",
		}
		if resource, ok := kindToResource[kind]; ok {
			return resource
		}
	}

	// Common batch resources
	if gv.Group == "batch" {
		kindToResource := map[string]string{
			"Job":     "jobs",
			"CronJob": "cronjobs",
		}
		if resource, ok := kindToResource[kind]; ok {
			return resource
		}
	}

	// Common networking resources
	if gv.Group == "networking.k8s.io" && gv.Version == "v1" {
		kindToResource := map[string]string{
			"Ingress":       "ingresses",
			"NetworkPolicy": "networkpolicies",
		}
		if resource, ok := kindToResource[kind]; ok {
			return resource
		}
	}

	// Common RBAC resources
	if gv.Group == "rbac.authorization.k8s.io" && gv.Version == "v1" {
		kindToResource := map[string]string{
			"Role":        "roles",
			"RoleBinding": "rolebindings",
		}
		if resource, ok := kindToResource[kind]; ok {
			return resource
		}
	}

	// CRITICAL: Explicit static mapping for ALL possible API groups to avoid RESTMapper conflicts
	// when multiple CRDs with same Kind exist (deckhouse.io, state-snapshotter.deckhouse.io, storage.deckhouse.io)
	// This completely bypasses RESTMapper for these resources, ensuring correct GVR selection
	stateSnapshotterKinds := map[string]string{
		"ManifestCaptureRequest":         "manifestcapturerequests",
		"ManifestCheckpoint":             "manifestcheckpoints",
		"ManifestCheckpointContentChunk": "manifestcheckpointcontentchunks",
	}

	// Handle state-snapshotter.deckhouse.io/v1alpha1 (primary API group)
	if gv.Group == "state-snapshotter.deckhouse.io" && gv.Version == "v1alpha1" {
		if resource, ok := stateSnapshotterKinds[kind]; ok {
			return resource
		}
	}

	// Handle storage.deckhouse.io/v1alpha1 (legacy/alternative API group)
	if gv.Group == "storage.deckhouse.io" && gv.Version == "v1alpha1" {
		if resource, ok := stateSnapshotterKinds[kind]; ok {
			return resource
		}
	}

	// Handle deckhouse.io/v1alpha1 (legacy/alternative API group)
	if gv.Group == "deckhouse.io" && gv.Version == "v1alpha1" {
		if resource, ok := stateSnapshotterKinds[kind]; ok {
			return resource
		}
	}

	// For custom resources, convert Kind to resource name using heuristics
	// Kind: VirtualMachine -> resource: virtualmachines
	// Kind: ManifestCaptureRequest -> resource: manifestcapturerequests
	lowerKind := strings.ToLower(kind)

	// Handle special cases
	var resource string
	switch {
	case strings.HasSuffix(lowerKind, "y"):
		resource = strings.TrimSuffix(lowerKind, "y") + "ies"
	case strings.HasSuffix(lowerKind, "s") || strings.HasSuffix(lowerKind, "x") || strings.HasSuffix(lowerKind, "z"):
		resource = lowerKind + "es"
	default:
		resource = lowerKind + "s"
	}

	return resource
}

// mapFollowedObjectToRetainers maps FollowObject events to Retainer reconciliation requests
// This ensures Retainers are reconciled immediately when followed objects are created/updated/deleted
// CRITICAL for FollowObjectWithTTL mode: TTL countdown must start immediately when object is deleted
func (r *RetainerController) mapFollowedObjectToRetainers(ctx context.Context, obj client.Object) []ctrl.Request {
	var retainerList iretainer.IRetainerList
	if err := r.List(ctx, &retainerList); err != nil {
		r.Logger.Error(err, "Failed to list Retainers for FollowObject event")
		return nil
	}

	objGVK := obj.GetObjectKind().GroupVersionKind()
	objName := obj.GetName()
	objNamespace := obj.GetNamespace()

	var requests []ctrl.Request
	for _, retainer := range retainerList.Items {
		// Only process retainers that follow objects
		if retainer.Spec.FollowObjectRef == nil {
			continue
		}

		ref := retainer.Spec.FollowObjectRef

		// Match by name, namespace, kind, and APIVersion
		if ref.Name == objName &&
			ref.Namespace == objNamespace &&
			ref.Kind == objGVK.Kind {
			// Parse ref APIVersion to compare with object APIVersion
			refGV, err := schema.ParseGroupVersion(ref.APIVersion)
			if err != nil {
				r.Logger.Warning("Invalid APIVersion in FollowObjectRef",
					"retainer", retainer.Name,
					"apiversion", ref.APIVersion)
				continue
			}

			// Match Group and Version
			if refGV.Group == objGVK.Group && refGV.Version == objGVK.Version {
				requests = append(requests, ctrl.Request{
					NamespacedName: client.ObjectKeyFromObject(&retainer),
				})
				r.Logger.Info("Enqueuing Retainer for FollowObject event",
					"retainer", retainer.Name,
					"object", fmt.Sprintf("%s/%s/%s", ref.APIVersion, ref.Kind, ref.Name),
					"namespace", ref.Namespace,
					"event", "FollowObject changed")
			}
		}
	}

	return requests
}

// SetupWithManager sets up the controller with the Manager
func (r *RetainerController) SetupWithManager(mgr ctrl.Manager) error {
	// Watch Retainer resources
	retainerPredicate := predicate.Funcs{
		CreateFunc: func(_ event.CreateEvent) bool {
			return true
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			// Reconcile on spec changes
			return e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration()
		},
		DeleteFunc: func(_ event.DeleteEvent) bool {
			return false // No need to reconcile deleted Retainers
		},
		GenericFunc: func(_ event.GenericEvent) bool {
			return true
		},
	}

	// Build controller
	builder := ctrl.NewControllerManagedBy(mgr).
		For(&iretainer.IRetainer{}).
		WithEventFilter(retainerPredicate)

	// Watch Namespace for deletion
	// When namespace is deleted, we need to delete all Retainers that follow objects in that namespace
	// NOTE: This is synchronous - no goroutine to avoid race conditions with migration
	builder = builder.Watches(
		&corev1.Namespace{},
		handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []ctrl.Request {
			ns, ok := obj.(*corev1.Namespace)
			if !ok {
				return nil
			}

			// Only handle deletion
			if ns.DeletionTimestamp == nil {
				return nil
			}

			// Enqueue all retainers for reconciliation
			// This ensures migration happens before namespace deletion handling
			var retainerList iretainer.IRetainerList
			if err := r.List(ctx, &retainerList); err != nil {
				r.Logger.Error(err, "Failed to list Retainers for namespace deletion")
				return nil
			}

			var requests []ctrl.Request
			for _, retainer := range retainerList.Items {
				// Skip checkpoint retainers - they don't follow namespace objects
				// Checkpoint retainers are identified ONLY by label (single source of truth)
				isCheckpointRetainer := retainer.Labels != nil &&
					retainer.Labels[CheckpointRetainerLabel] == CheckpointRetainerLabelValue
				if isCheckpointRetainer {
					continue
				}

				// Only enqueue retainers that follow objects in the deleted namespace
				// Check by Spec, not by name prefix
				if (retainer.Spec.Mode == "FollowObject" || retainer.Spec.Mode == "FollowObjectWithTTL") &&
					retainer.Spec.FollowObjectRef != nil &&
					retainer.Spec.FollowObjectRef.Namespace == ns.Name {
					requests = append(requests, ctrl.Request{
						NamespacedName: client.ObjectKeyFromObject(&retainer),
					})
				}
			}

			return requests
		}),
	)

	// CRITICAL: Watch ManifestCaptureRequest to trigger Retainer reconciliation on MCR deletion
	// This ensures FollowObjectWithTTL mode starts TTL countdown immediately when MCR is deleted
	// Without this watch, Retainer only reconciles periodically (every 2 min), causing delays
	builder = builder.Watches(
		&storagev1alpha1.ManifestCaptureRequest{},
		handler.EnqueueRequestsFromMapFunc(r.mapFollowedObjectToRetainers),
	)

	// Watch ManifestCheckpoint (in case someone follows it in the future)
	builder = builder.Watches(
		&storagev1alpha1.ManifestCheckpoint{},
		handler.EnqueueRequestsFromMapFunc(r.mapFollowedObjectToRetainers),
	)

	// NOTE: We do NOT watch ManifestCheckpointContentChunk
	// ADR requirement: "Жёсткое требование ADR: Никакие другие ClusterRole/Role (включая потребляющие контроллеры)
	// не должны содержать ресурс ManifestCheckpointContentChunk. На этот ресурс не должно быть прав list и watch ни у кого."
	// Watch on Chunk would require list/watch permissions, which violates ADR.
	// Chunks are managed via direct Get/Create/Delete operations using known names from ManifestCheckpoint.status.chunks

	return builder.Complete(r)
}

// handleNamespaceDeletion is called from Reconcile when namespace deletion is detected
// This method is now called synchronously from Reconcile, not from a goroutine
func (r *RetainerController) handleNamespaceDeletion(ctx context.Context, retainer *iretainer.IRetainer, namespaceName string) error {
	// CRITICAL: Check checkpoint retainers FIRST, before any other checks
	// Checkpoint retainers are identified ONLY by label (single source of truth)
	// They MUST NEVER be deleted by namespace deletion rules, regardless of FollowObjectRef state
	// This protects against migration scenarios where FollowObjectRef might be temporarily set
	isCheckpointRetainer := retainer.Labels != nil &&
		retainer.Labels[CheckpointRetainerLabel] == CheckpointRetainerLabelValue
	if isCheckpointRetainer {
		r.Logger.Debug("Checkpoint retainer detected in namespace deletion handler, skipping",
			"retainer", retainer.Name,
			"namespace", namespaceName,
			"note", "Checkpoint retainers are never deleted by namespace lifecycle")
		return nil
	}

	// Only process retainers that follow objects in the deleted namespace
	// Check by Spec, not by name prefix
	if retainer.Spec.FollowObjectRef == nil {
		return nil
	}

	if retainer.Spec.FollowObjectRef.Namespace != namespaceName {
		return nil
	}

	// This is an MCR retainer following an object in the deleted namespace
	// Delete it
	r.Logger.Info("Deleting Retainer due to namespace deletion",
		"retainer", retainer.Name,
		"namespace", namespaceName,
		"mode", retainer.Spec.Mode)
	if err := r.Delete(ctx, retainer); err != nil {
		return fmt.Errorf("failed to delete Retainer: %w", err)
	}

	return nil
}
