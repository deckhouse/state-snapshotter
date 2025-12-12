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
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	iretainer "github.com/deckhouse/state-snapshotter/api/v1alpha1/iretainer"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/common"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/config"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

const (
	// ChunkNamePrefix is the prefix for chunk names
	ChunkNamePrefix = "mcp-"
)

// setSingleCondition sets a condition, removing any existing condition of the same type first
// This ensures that each condition type appears only once, preventing duplicates
func setSingleCondition(conds *[]metav1.Condition, cond metav1.Condition) {
	meta.RemoveStatusCondition(conds, cond.Type)
	meta.SetStatusCondition(conds, cond)
}

// ManifestCheckpointController reconciles ManifestCaptureRequest objects
type ManifestCheckpointController struct {
	client.Client
	Scheme *runtime.Scheme
	Logger logger.LoggerInterface
	Config *config.Options
}

func (r *ManifestCheckpointController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	r.Logger.Info("Reconciling ManifestCaptureRequest", "namespace", req.Namespace, "name", req.Name)

	// Load config from ConfigMap (TZ section 7)
	if err := r.loadConfigFromConfigMap(ctx); err != nil {
		r.Logger.Warning("Failed to load config from ConfigMap, using defaults", "error", err)
	}

	mcr := &storagev1alpha1.ManifestCaptureRequest{}
	if err := r.Get(ctx, req.NamespacedName, mcr); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Skip if already Ready - MCR is immutable once Ready
	// This ensures snapshot immutability - checkpoint should not be recreated
	readyCondition := meta.FindStatusCondition(mcr.Status.Conditions, ConditionTypeReady)
	if readyCondition != nil && readyCondition.Status == metav1.ConditionTrue {
		r.Logger.Info("MCR is already Ready - skipping reconcile", "namespace", req.Namespace, "name", req.Name)
		return ctrl.Result{}, nil
	}

	// Skip if already Failed and observed
	if readyCondition != nil && readyCondition.Status == metav1.ConditionFalse && readyCondition.Reason == ConditionReasonInternalError {
		if mcr.Status.ObservedGeneration == mcr.Generation {
			return ctrl.Result{}, nil
		}
	}

	// Check if already has checkpoint
	if mcr.Status.CheckpointName != "" {
		// Verify checkpoint exists
		var checkpoint storagev1alpha1.ManifestCheckpoint
		if err := r.Get(ctx, client.ObjectKey{Name: mcr.Status.CheckpointName}, &checkpoint); err == nil {
			// Checkpoint exists, mark as ready
			base := mcr.DeepCopy()
			mcr.Status.ObservedGeneration = mcr.Generation
			now := metav1.Now()
			if mcr.Status.CompletionTimestamp == nil {
				mcr.Status.CompletionTimestamp = &now
			}
			setSingleCondition(&mcr.Status.Conditions, metav1.Condition{
				Type:               ConditionTypeReady,
				Status:             metav1.ConditionTrue,
				Reason:             ConditionReasonCompleted,
				Message:            fmt.Sprintf("Checkpoint %s already exists", mcr.Status.CheckpointName),
				LastTransitionTime: now,
				ObservedGeneration: mcr.Generation,
			})
			if err := r.Status().Patch(ctx, mcr, client.MergeFrom(base)); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
	}

	// Process the request
	return r.processCaptureRequest(ctx, mcr)
}

func (r *ManifestCheckpointController) processCaptureRequest(ctx context.Context, mcr *storagev1alpha1.ManifestCaptureRequest) (ctrl.Result, error) {
	// Set Processing condition if not set
	processingCondition := meta.FindStatusCondition(mcr.Status.Conditions, ConditionTypeProcessing)
	if processingCondition == nil {
		base := mcr.DeepCopy()
		mcr.Status.ObservedGeneration = mcr.Generation
		now := metav1.Now()
		setSingleCondition(&mcr.Status.Conditions, metav1.Condition{
			Type:               ConditionTypeProcessing,
			Status:             metav1.ConditionTrue,
			Reason:             ConditionReasonInProgress,
			Message:            "Processing capture request",
			LastTransitionTime: now,
			ObservedGeneration: mcr.Generation,
		})
		if err := r.Status().Patch(ctx, mcr, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Validate targets
	if len(mcr.Spec.Targets) == 0 {
		base := mcr.DeepCopy()
		message := "No targets specified"
		mcr.Status.ErrorReason = ConditionReasonInternalError
		mcr.Status.ObservedGeneration = mcr.Generation
		now := metav1.Now()
		mcr.Status.CompletionTimestamp = &now
		setSingleCondition(&mcr.Status.Conditions, metav1.Condition{
			Type:               ConditionTypeReady,
			Status:             metav1.ConditionFalse,
			Reason:             ConditionReasonInternalError,
			Message:            message,
			LastTransitionTime: now,
			ObservedGeneration: mcr.Generation,
		})
		setSingleCondition(&mcr.Status.Conditions, metav1.Condition{
			Type:               ConditionTypeFailed,
			Status:             metav1.ConditionTrue,
			Reason:             ConditionReasonInternalError,
			Message:            message,
			LastTransitionTime: now,
			ObservedGeneration: mcr.Generation,
		})
		if err := r.Status().Patch(ctx, mcr, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Collect all target objects
	objects, err := r.collectTargetObjects(ctx, mcr)
	if err != nil {
		r.Logger.Error(err, "Failed to collect target objects", "mcr", fmt.Sprintf("%s/%s", mcr.Namespace, mcr.Name))
		base := mcr.DeepCopy()
		message := fmt.Sprintf("Failed to collect objects: %v", err)
		mcr.Status.ErrorReason = r.determineErrorReason(err)
		mcr.Status.ObservedGeneration = mcr.Generation
		now := metav1.Now()
		mcr.Status.CompletionTimestamp = &now
		setSingleCondition(&mcr.Status.Conditions, metav1.Condition{
			Type:               ConditionTypeReady,
			Status:             metav1.ConditionFalse,
			Reason:             mcr.Status.ErrorReason,
			Message:            message,
			LastTransitionTime: now,
			ObservedGeneration: mcr.Generation,
		})
		setSingleCondition(&mcr.Status.Conditions, metav1.Condition{
			Type:               ConditionTypeFailed,
			Status:             metav1.ConditionTrue,
			Reason:             mcr.Status.ErrorReason,
			Message:            message,
			LastTransitionTime: now,
			ObservedGeneration: mcr.Generation,
		})
		if err := r.Status().Patch(ctx, mcr, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Filtering and cleaning are now done inside addObject() during collection
	// No need for separate filtering pass

	// Handle empty objects list
	if len(objects) == 0 {
		r.Logger.Info("No objects found for capture request", "name", mcr.Name)
		// Still create checkpoint with empty chunks
	}

	// Determine checkpoint name: use existing if available, otherwise generate new
	var checkpointName string
	if mcr.Status.CheckpointName != "" {
		// Checkpoint already exists - use existing name (e.g., for retainer migration)
		checkpointName = mcr.Status.CheckpointName
		r.Logger.Info("Using existing checkpoint name",
			"mcr", fmt.Sprintf("%s/%s", mcr.Namespace, mcr.Name),
			"checkpoint", checkpointName)
	} else {
		// Generate new checkpoint name
		checkpointName = r.generateCheckpointName()
		r.Logger.Info("Starting checkpoint creation",
			"mcr", fmt.Sprintf("%s/%s", mcr.Namespace, mcr.Name),
			"checkpoint", checkpointName,
			"targets", len(mcr.Spec.Targets))
	}

	// ADR: Create only ONE Retainer: ret-mcr-<namespace>-<mcrName>
	// This Retainer:
	// - Uses FollowObjectWithTTL mode to follow MCR (implements MCR TTL)
	// - Holds the ManifestCheckpoint (MCP has ownerRef to this Retainer)
	// - When MCR is deleted, Retainer continues to live for TTL duration
	// - When Retainer expires, it's deleted → GC deletes MCP (via ownerRef)
	retainerName := fmt.Sprintf("ret-mcr-%s-%s", mcr.Namespace, mcr.Name)
	r.Logger.Info("Step 1: Creating Retainer for MCR", "retainer", retainerName, "mcr", fmt.Sprintf("%s/%s", mcr.Namespace, mcr.Name))

	retainer := &iretainer.IRetainer{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "deckhouse.io/v1alpha1",
			Kind:       "IRetainer",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: retainerName,
			// Retainer has NO ownerRef - it's a top-level resource
		},
		Spec: iretainer.IRetainerSpec{
			Mode: "FollowObjectWithTTL",
			FollowObjectRef: &iretainer.FollowObjectRef{
				APIVersion: "state-snapshotter.deckhouse.io/v1alpha1",
				Kind:       "ManifestCaptureRequest",
				Namespace:  mcr.Namespace,
				Name:       mcr.Name,
				UID:        string(mcr.UID),
			},
			TTL: &metav1.Duration{
				Duration: 10 * time.Minute,
			},
		},
	}

	// Create Retainer first
	if err := r.Create(ctx, retainer); err != nil {
		if !errors.IsAlreadyExists(err) {
			r.Logger.Error(err, "Failed to create Retainer", "name", retainerName, "mode", retainer.Spec.Mode)
			base := mcr.DeepCopy()
			message := fmt.Sprintf("Failed to create Retainer: %v", err)
			mcr.Status.ErrorReason = ConditionReasonInternalError
			mcr.Status.ObservedGeneration = mcr.Generation
			now := metav1.Now()
			mcr.Status.CompletionTimestamp = &now
			setSingleCondition(&mcr.Status.Conditions, metav1.Condition{
				Type:               ConditionTypeReady,
				Status:             metav1.ConditionFalse,
				Reason:             ConditionReasonInternalError,
				Message:            message,
				LastTransitionTime: now,
				ObservedGeneration: mcr.Generation,
			})
			setSingleCondition(&mcr.Status.Conditions, metav1.Condition{
				Type:               ConditionTypeFailed,
				Status:             metav1.ConditionTrue,
				Reason:             ConditionReasonInternalError,
				Message:            message,
				LastTransitionTime: now,
				ObservedGeneration: mcr.Generation,
			})
			if err := r.Status().Patch(ctx, mcr, client.MergeFrom(base)); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, err
		}
		// Retainer already exists, get it and use it
		existingRetainer := &iretainer.IRetainer{}
		if err := r.Get(ctx, client.ObjectKey{Name: retainerName}, existingRetainer); err != nil {
			r.Logger.Error(err, "Failed to get existing Retainer", "name", retainerName)
			return ctrl.Result{}, fmt.Errorf("failed to get existing Retainer: %w", err)
		}
		retainer = existingRetainer
		r.Logger.Info("Retainer already exists, using existing", "retainer", retainerName, "uid", retainer.UID, "mode", retainer.Spec.Mode)
	} else {
		r.Logger.Info("✅ Step 1 complete: Created Retainer",
			"retainer", retainerName,
			"uid", retainer.UID,
			"checkpoint", checkpointName,
			"mcr", fmt.Sprintf("%s/%s", mcr.Namespace, mcr.Name))
	}

	// If checkpoint already exists → skip creation (assume it's correct)
	var existingCheckpoint storagev1alpha1.ManifestCheckpoint
	if err := r.Get(ctx, client.ObjectKey{Name: checkpointName}, &existingCheckpoint); err == nil {
		r.Logger.Info("Checkpoint already exists, skipping creation",
			"checkpoint", checkpointName)
		return ctrl.Result{}, nil
	}

	// Create ManifestCheckpoint with ownerRef to ret-mcr-* Retainer
	// ADR: Checkpoint MUST have ownerRef ONLY on ret-mcr-<namespace>-<mcrName>
	// This is the single Retainer that holds both MCR and MCP
	checkpoint := &storagev1alpha1.ManifestCheckpoint{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "state-snapshotter.deckhouse.io/v1alpha1",
			Kind:       "ManifestCheckpoint",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: checkpointName,
			Labels: map[string]string{
				"state-snapshotter.deckhouse.io/source-namespace": mcr.Namespace,
				"state-snapshotter.deckhouse.io/source-request":   mcr.Name,
			},
			// ADR: Checkpoint has ownerRef ONLY on ret-mcr-<namespace>-<mcrName>
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "deckhouse.io/v1alpha1",
					Kind:       "IRetainer",
					Name:       retainerName, // This is ret-mcr-<namespace>-<mcrName>
					UID:        retainer.UID,
					Controller: func() *bool { b := true; return &b }(),
				},
			},
		},
		Spec: storagev1alpha1.ManifestCheckpointSpec{
			SourceNamespace: mcr.Namespace,
			ManifestCaptureRequestRef: &storagev1alpha1.ObjectReference{
				Name:      mcr.Name,
				Namespace: mcr.Namespace,
				UID:       string(mcr.UID),
			},
		},
		Status: storagev1alpha1.ManifestCheckpointStatus{},
	}

	if err := r.Create(ctx, checkpoint); err != nil {
		if !errors.IsAlreadyExists(err) {
			r.Logger.Error(err, "Failed to create ManifestCheckpoint",
				"checkpoint", checkpointName,
				"owner", retainerName,
				"ownerUID", retainer.UID)
			base := mcr.DeepCopy()
			message := fmt.Sprintf("Failed to create checkpoint: %v", err)
			mcr.Status.ErrorReason = ConditionReasonInternalError
			mcr.Status.ObservedGeneration = mcr.Generation
			now := metav1.Now()
			mcr.Status.CompletionTimestamp = &now
			setSingleCondition(&mcr.Status.Conditions, metav1.Condition{
				Type:               ConditionTypeReady,
				Status:             metav1.ConditionFalse,
				Reason:             ConditionReasonInternalError,
				Message:            message,
				LastTransitionTime: now,
				ObservedGeneration: mcr.Generation,
			})
			setSingleCondition(&mcr.Status.Conditions, metav1.Condition{
				Type:               ConditionTypeFailed,
				Status:             metav1.ConditionTrue,
				Reason:             ConditionReasonInternalError,
				Message:            message,
				LastTransitionTime: now,
				ObservedGeneration: mcr.Generation,
			})
			if err := r.Status().Patch(ctx, mcr, client.MergeFrom(base)); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, err
		}
		// Checkpoint already exists, get it
		if err := r.Get(ctx, client.ObjectKey{Name: checkpointName}, checkpoint); err != nil {
			r.Logger.Error(err, "Failed to get existing checkpoint", "checkpoint", checkpointName)
			return ctrl.Result{}, err
		}
		r.Logger.Info("Checkpoint already exists, using existing",
			"checkpoint", checkpointName,
			"uid", checkpoint.UID,
			"ownerRefs", checkpoint.OwnerReferences)
	} else {
		// Log ownerRef details for debugging
		ownerRefDetails := make([]string, 0, len(checkpoint.OwnerReferences))
		for _, ref := range checkpoint.OwnerReferences {
			ownerRefDetails = append(ownerRefDetails, fmt.Sprintf("%s/%s/%s (UID: %s)", ref.APIVersion, ref.Kind, ref.Name, ref.UID))
		}
		r.Logger.Info("✅ Step 2 complete: Created ManifestCheckpoint",
			"checkpoint", checkpointName,
			"uid", checkpoint.UID,
			"ownerRefs", ownerRefDetails,
			"retainerName", retainerName,
			"retainerUID", retainer.UID)
	}

	// NOW create chunks (checkpoint exists, so ownerRef will work)
	// CRITICAL: checkpoint.UID is now populated after Create, use it in ownerRef
	r.Logger.Info("Step 3: Creating chunks",
		"checkpoint", checkpointName,
		"checkpointUID", checkpoint.UID,
		"objects", len(objects))
	chunks, err := r.createChunks(ctx, checkpointName, string(checkpoint.UID), objects)
	if err != nil {
		r.Logger.Error(err, "❌ Step 3 failed: Failed to create chunks",
			"checkpoint", checkpointName,
			"objects", len(objects),
			"error", err.Error())
		base := mcr.DeepCopy()
		message := fmt.Sprintf("Failed to create chunks: %v", err)
		mcr.Status.ErrorReason = ConditionReasonSerializationError
		mcr.Status.ObservedGeneration = mcr.Generation
		now := metav1.Now()
		mcr.Status.CompletionTimestamp = &now
		setSingleCondition(&mcr.Status.Conditions, metav1.Condition{
			Type:               ConditionTypeReady,
			Status:             metav1.ConditionFalse,
			Reason:             ConditionReasonSerializationError,
			Message:            message,
			LastTransitionTime: now,
			ObservedGeneration: mcr.Generation,
		})
		setSingleCondition(&mcr.Status.Conditions, metav1.Condition{
			Type:               ConditionTypeFailed,
			Status:             metav1.ConditionTrue,
			Reason:             ConditionReasonSerializationError,
			Message:            message,
			LastTransitionTime: now,
			ObservedGeneration: mcr.Generation,
		})
		if err := r.Status().Patch(ctx, mcr, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Update checkpoint with chunks info
	r.Logger.Info("Step 4: Updating checkpoint status with chunks info",
		"checkpoint", checkpointName,
		"chunks", len(chunks))
	totalObjects := 0
	totalSize := int64(0)
	for _, chunk := range chunks {
		totalObjects += chunk.ObjectsCount
		totalSize += chunk.SizeBytes
	}
	r.Logger.Info("Chunks summary",
		"checkpoint", checkpointName,
		"chunksCount", len(chunks),
		"totalObjects", totalObjects,
		"totalSizeBytes", totalSize)
	checkpoint.Status.Chunks = chunks
	checkpoint.Status.TotalObjects = totalObjects
	checkpoint.Status.TotalSizeBytes = totalSize
	now := metav1.Now()
	setSingleCondition(&checkpoint.Status.Conditions, metav1.Condition{
		Type:               ConditionTypeReady,
		Status:             metav1.ConditionTrue,
		Reason:             ConditionReasonCompleted,
		Message:            fmt.Sprintf("Checkpoint created with %d chunks, %d objects", len(chunks), totalObjects),
		LastTransitionTime: now,
	})
	if err := r.Status().Update(ctx, checkpoint); err != nil {
		r.Logger.Error(err, "Failed to update checkpoint status",
			"checkpoint", checkpointName,
			"chunks", len(chunks),
			"totalObjects", totalObjects)
		return ctrl.Result{}, err
	}
	r.Logger.Info("✅ Step 4 complete: Updated checkpoint status",
		"checkpoint", checkpointName,
		"chunks", len(chunks),
		"totalObjects", totalObjects)

	// Update MCR status
	base := mcr.DeepCopy()
	mcr.Status.CheckpointName = checkpointName
	mcr.Status.ObservedGeneration = mcr.Generation
	completionTime := metav1.Now()
	mcr.Status.CompletionTimestamp = &completionTime
	setSingleCondition(&mcr.Status.Conditions, metav1.Condition{
		Type:               ConditionTypeReady,
		Status:             metav1.ConditionTrue,
		Reason:             ConditionReasonCompleted,
		Message:            fmt.Sprintf("Checkpoint %s created successfully", checkpointName),
		LastTransitionTime: completionTime,
		ObservedGeneration: mcr.Generation,
	})
	setSingleCondition(&mcr.Status.Conditions, metav1.Condition{
		Type:               ConditionTypeProcessing,
		Status:             metav1.ConditionFalse,
		Reason:             ConditionReasonCompleted,
		Message:            "Processing completed",
		LastTransitionTime: completionTime,
		ObservedGeneration: mcr.Generation,
	})
	if err := r.Status().Patch(ctx, mcr, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}

	// NOTE: We do NOT set ttl-start-timestamp here for ret-mcr-* retainer
	// ADR: ret-mcr-* is a FollowObjectWithTTL retainer, not a checkpoint retainer
	// For FollowObjectWithTTL mode:
	// - TTL should start ONLY when the followed object (MCR) is deleted
	// - Setting ttl-start-timestamp at MCR completion would cause TTL to start prematurely
	// - RetainerController.reconcileFollowObjectWithTTL will set ttl-start-timestamp when MCR is deleted
	// This ensures correct TTL behavior: retainer lives while MCR exists, TTL starts only after MCR deletion

	r.Logger.Info("ManifestCaptureRequest processed successfully",
		"name", mcr.Name,
		"checkpoint", checkpointName,
		"chunks", len(chunks),
		"objects", totalObjects)

	return ctrl.Result{}, nil
}

func (r *ManifestCheckpointController) collectTargetObjects(ctx context.Context, mcr *storagev1alpha1.ManifestCaptureRequest) ([]unstructured.Unstructured, error) {
	var objects []unstructured.Unstructured
	collected := make(map[string]bool) // Track collected objects to avoid duplicates

	// Helper to add object if not already collected
	// CRITICAL: Filtering and cleaning must happen BEFORE adding (TZ requirement)
	// But only if enableFiltering is true (default: false - include everything as-is)
	addObject := func(obj *unstructured.Unstructured) {
		// RULE 0: Never capture cluster-scoped objects (e.g. Namespace, Node, PersistentVolume, etc.)
		// This is an absolute rule - cluster-scoped objects should never appear in snapshot,
		// even if they are referenced as dependencies or collected through other means
		gv, err := schema.ParseGroupVersion(obj.GetAPIVersion())
		if err != nil {
			r.Logger.Warning("Failed to parse APIVersion, skipping object", "apiVersion", obj.GetAPIVersion(), "kind", obj.GetKind(), "name", obj.GetName(), "error", err)
			return
		}
		if !r.isNamespacedResource(gv, obj.GetKind()) {
			r.Logger.Info("Skipping cluster-scoped object (never captured in snapshot)", "kind", obj.GetKind(), "name", obj.GetName())
			return
		}

		var finalObj *unstructured.Unstructured

		// Apply filtering only if enabled
		if r.Config.EnableFiltering {
			// Step 3: Apply filtering (TZ section 5, step 3) - BEFORE adding
			// Pass excludeKinds from ConfigMap to support runtime configuration
			if common.ShouldSkipObject(obj, r.Config.ExcludeKinds) {
				r.Logger.Info("Skipping object", "kind", obj.GetKind(), "name", obj.GetName(), "namespace", obj.GetNamespace())
				return
			}

			// Apply cleaning (remove metadata, status, annotations)
			// Pass excludeAnnotations from ConfigMap to support runtime configuration
			cleaned := common.CleanObjectForSnapshot(obj, r.Config.ExcludeAnnotations)
			if cleaned == nil {
				r.Logger.Info("Object excluded after cleaning", "kind", obj.GetKind(), "name", obj.GetName())
				return
			}
			finalObj = cleaned
		} else {
			// If filtering disabled, use object as-is (no filtering, no cleaning)
			finalObj = obj
		}

		// FIX: Restore apiVersion/kind because JSON round-trip in CleanObjectForSnapshot
		// or normalizeObjectForJSON may drop TypeMeta fields
		// These fields are stored separately in unstructured.Unstructured and must be preserved
		finalObj.SetAPIVersion(obj.GetAPIVersion())
		finalObj.SetKind(obj.GetKind())

		// Check for duplicates
		key := fmt.Sprintf("%s/%s/%s/%s",
			finalObj.GetAPIVersion(),
			finalObj.GetKind(),
			finalObj.GetNamespace(),
			finalObj.GetName())
		if !collected[key] {
			collected[key] = true
			objects = append(objects, *finalObj)
		}
	}

	// Step 1: Collect target objects (TZ section 5, step 1)
	// TZ: All targets must be namespaced objects in the same namespace as ManifestCaptureRequest
	// Cluster-scoped resources are NOT supported in targets
	for _, target := range mcr.Spec.Targets {
		gv, err := schema.ParseGroupVersion(target.APIVersion)
		if err != nil {
			return nil, fmt.Errorf("invalid apiVersion %s: %w", target.APIVersion, err)
		}

		// Validate: cluster-scoped resources are not allowed in targets
		// ManifestCaptureRequest is namespaced, so all targets must be namespaced too
		if !r.isNamespacedResource(gv, target.Kind) {
			return nil, fmt.Errorf("cluster-scoped resource %s/%s is not allowed in targets. ManifestCaptureRequest only supports namespaced resources", target.Kind, target.Name)
		}

		// Get the resource (all targets are namespaced, so use MCR namespace)
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   gv.Group,
			Version: gv.Version,
			Kind:    target.Kind,
		})
		obj.SetName(target.Name)
		obj.SetNamespace(mcr.Namespace)

		if err := r.Get(ctx, client.ObjectKey{
			Namespace: mcr.Namespace,
			Name:      target.Name,
		}, obj); err != nil {
			return nil, fmt.Errorf("failed to get %s %s/%s: %w", target.Kind, mcr.Namespace, target.Name, err)
		}

		// Add target object (filtering happens inside addObject)
		addObject(obj)

		// Step 2: Recursively collect related objects (TZ section 5, step 2)
		// collectRelatedObjects now uses addObject directly, so filtering is applied
		// Collect related objects (ConfigMaps, Secrets, etc.)
		// Errors are ignored - continue even if related objects collection fails
		r.collectRelatedObjects(ctx, obj, mcr.Namespace, addObject)
	}

	// Step 4: Sort objects (TZ section 5, step 4)
	// Sort by: groupVersion, kind, namespace, name
	r.sortObjects(objects)

	return objects, nil
}

// collectRelatedObjects recursively collects ConfigMaps, Secrets, and volumeClaimTemplates (TZ section 5, step 2)
// CRITICAL: Uses addObject callback to ensure filtering and cleaning are applied immediately
func (r *ManifestCheckpointController) collectRelatedObjects(ctx context.Context, obj *unstructured.Unstructured, namespace string, addObject func(*unstructured.Unstructured)) {
	// Collect ConfigMaps referenced in volumes
	if volumes, found, _ := unstructured.NestedSlice(obj.Object, "spec", "volumes"); found {
		for _, vol := range volumes {
			volMap, ok := vol.(map[string]interface{})
			if !ok {
				continue
			}
			if cm, found := volMap["configMap"]; found {
				cmMap, ok := cm.(map[string]interface{})
				if !ok {
					continue
				}
				if name, found := cmMap["name"]; found {
					if nameStr, ok := name.(string); ok {
						cmObj := &unstructured.Unstructured{}
						cmObj.SetGroupVersionKind(schema.GroupVersionKind{
							Group:   "",
							Version: "v1",
							Kind:    "ConfigMap",
						})
						cmObj.SetName(nameStr)
						cmObj.SetNamespace(namespace)
						if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: nameStr}, cmObj); err == nil {
							// addObject applies filtering and cleaning
							addObject(cmObj)
						}
					}
				}
			}
		}
	}

	// Collect Secrets referenced in volumes (exclude all service account Secrets)
	if volumes, found, _ := unstructured.NestedSlice(obj.Object, "spec", "volumes"); found {
		for _, vol := range volumes {
			volMap, ok := vol.(map[string]interface{})
			if !ok {
				continue
			}
			if secret, found := volMap["secret"]; found {
				secretMap, ok := secret.(map[string]interface{})
				if !ok {
					continue
				}
				if name, found := secretMap["secretName"]; found {
					if nameStr, ok := name.(string); ok {
						secretObj := &unstructured.Unstructured{}
						secretObj.SetGroupVersionKind(schema.GroupVersionKind{
							Group:   "",
							Version: "v1",
							Kind:    "Secret",
						})
						secretObj.SetName(nameStr)
						secretObj.SetNamespace(namespace)
						if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: nameStr}, secretObj); err == nil {
							// addObject applies filtering and cleaning (including ShouldSkipObject for Secrets)
							// No need for manual filtering here - ShouldSkipObject handles all non-Opaque secrets
							addObject(secretObj)
						}
					}
				}
			}
		}
	}

	// volumeClaimTemplates are embedded in StatefulSet, not separate resources
	// They will be included in the main object automatically
	// No need to collect them separately
}

// sortObjects sorts objects by groupVersion, kind, namespace, name (TZ section 5, step 4)
func (r *ManifestCheckpointController) sortObjects(objects []unstructured.Unstructured) {
	sort.Slice(objects, func(i, j int) bool {
		objI := objects[i]
		objJ := objects[j]

		// Compare by groupVersion
		if objI.GetAPIVersion() != objJ.GetAPIVersion() {
			return objI.GetAPIVersion() < objJ.GetAPIVersion()
		}

		// Compare by kind
		if objI.GetKind() != objJ.GetKind() {
			return objI.GetKind() < objJ.GetKind()
		}

		// Compare by namespace
		if objI.GetNamespace() != objJ.GetNamespace() {
			return objI.GetNamespace() < objJ.GetNamespace()
		}

		// Compare by name
		return objI.GetName() < objJ.GetName()
	})
}

// cleanObject is now replaced by common.CleanObjectForSnapshot
// This method is kept for backward compatibility but delegates to the common function
// Uses default excludeAnnotations (nil) - ConfigMap customization should be applied via direct call
func (r *ManifestCheckpointController) cleanObject(obj *unstructured.Unstructured) *unstructured.Unstructured {
	return common.CleanObjectForSnapshot(obj, nil)
}

func (r *ManifestCheckpointController) createChunks(ctx context.Context, checkpointName string, checkpointUID string, objects []unstructured.Unstructured) ([]storagev1alpha1.ChunkInfo, error) {
	r.Logger.Info("createChunks: Starting",
		"checkpoint", checkpointName,
		"objects", len(objects),
		"maxChunkSizeBytes", r.Config.MaxChunkSizeBytes)

	// Handle empty objects
	if len(objects) == 0 {
		r.Logger.Info("createChunks: No objects to chunk, creating empty chunk")
		// Create empty chunk
		emptyJSON := []byte("[]")
		compressed, err := r.compressAndEncode(emptyJSON)
		if err != nil {
			return nil, fmt.Errorf("failed to compress empty chunk: %w", err)
		}

		checkpointID := checkpointName
		if strings.HasPrefix(checkpointName, ChunkNamePrefix) {
			checkpointID = checkpointName[len(ChunkNamePrefix):]
		}
		chunkName := fmt.Sprintf("%s%s-0", ChunkNamePrefix, checkpointID)

		chunk := &storagev1alpha1.ManifestCheckpointContentChunk{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "state-snapshotter.deckhouse.io/v1alpha1",
				Kind:       "ManifestCheckpointContentChunk",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: chunkName,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: "state-snapshotter.deckhouse.io/v1alpha1",
						Kind:       "ManifestCheckpoint",
						Name:       checkpointName,
						UID:        types.UID(checkpointUID),
						// Setting Controller=true to enable GC: when checkpoint is deleted, chunks should be deleted
						// Both ManifestCheckpoint and Chunk are cluster-scoped, so ownerRef is valid
						Controller: func() *bool { b := true; return &b }(),
					},
				},
			},
			Spec: storagev1alpha1.ManifestCheckpointContentChunkSpec{
				CheckpointName: checkpointName,
				Index:          0,
				Data:           compressed,
				ObjectsCount:   0,
				Checksum:       r.calculateChunkChecksum(compressed),
			},
		}

		if err := r.Create(ctx, chunk); err != nil && !errors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("failed to create empty chunk: %w", err)
		}

		return []storagev1alpha1.ChunkInfo{
			{
				Name:         chunkName,
				Index:        0,
				ObjectsCount: 0,
				SizeBytes:    int64(len(compressed)),
			},
		}, nil
	}

	// Convert objects to JSON array format
	// Normalize objects to ensure they are pure map[string]interface{} without yaml.MapSlice
	// This prevents Key/Value serialization when reading chunks later
	jsonObjects := make([]interface{}, 0, len(objects))
	for _, obj := range objects {
		// Normalize object to ensure clean JSON serialization
		normalized := r.normalizeObjectForJSON(obj.Object)

		// FIX: Ensure apiVersion and kind are present in normalized object
		// normalizeObjectForJSON works on obj.Object (map), which doesn't include TypeMeta
		// We need to explicitly add apiVersion and kind to the normalized map
		if normalizedMap, ok := normalized.(map[string]interface{}); ok {
			normalizedMap["apiVersion"] = obj.GetAPIVersion()
			normalizedMap["kind"] = obj.GetKind()
		}

		jsonObjects = append(jsonObjects, normalized)
	}

	// Split objects into chunks based on COMPRESSED size
	// We need to estimate compressed size, so we'll use a conservative approach
	//
	// NOTE: Format implementation (differing from ADR comment about "one gzip + split"):
	// Each chunk contains its own gzipped JSON array of objects, not a split of one global gzip.
	// This allows precise control over final compressed+base64 size per chunk.
	// Compression ratio may be slightly worse than a single global gzip, but is more practical.
	type chunkData struct {
		objects []interface{}
	}
	chunks := make([]chunkData, 0)
	currentChunk := chunkData{objects: make([]interface{}, 0)}

	for _, obj := range jsonObjects {
		// Check if adding this object would exceed the limit
		// We estimate compressed size by checking the current chunk + new object
		testChunk := make([]interface{}, len(currentChunk.objects))
		copy(testChunk, currentChunk.objects)
		testChunk = append(testChunk, obj)

		testJSON, err := json.Marshal(testChunk)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal test chunk: %w", err)
		}

		// Compress to check actual size
		compressed, err := r.compressAndEncode(testJSON)
		if err != nil {
			return nil, fmt.Errorf("failed to compress test chunk: %w", err)
		}

		// If compressed size exceeds limit, finalize current chunk first
		if len(compressed) > int(r.Config.MaxChunkSizeBytes) {
			// If current chunk is not empty, save it
			if len(currentChunk.objects) > 0 {
				chunks = append(chunks, currentChunk)
				currentChunk = chunkData{objects: make([]interface{}, 0)}
			}

			// Now check if single object exceeds limit
			singleObjJSON, _ := json.Marshal([]interface{}{obj})
			singleCompressed, err := r.compressAndEncode(singleObjJSON)
			if err == nil && len(singleCompressed) > int(r.Config.MaxChunkSizeBytes) {
				r.Logger.Warning("Object exceeds MaxChunkSizeBytes - storing as-is, may break etcd on large clusters",
					"compressedSize", len(singleCompressed),
					"maxSize", r.Config.MaxChunkSizeBytes)
				chunks = append(chunks, chunkData{objects: []interface{}{obj}})
				continue
			}
		}

		// Add object to current chunk
		currentChunk.objects = append(currentChunk.objects, obj)
	}

	// Add last chunk if not empty
	if len(currentChunk.objects) > 0 {
		chunks = append(chunks, currentChunk)
	}

	r.Logger.Info("createChunks: Split complete",
		"totalChunks", len(chunks),
		"totalObjects", len(objects))

	// Create chunk resources
	chunkInfos := make([]storagev1alpha1.ChunkInfo, 0, len(chunks))
	for i, chunk := range chunks {
		// Extract ID from checkpoint name (remove prefix if present)
		checkpointID := checkpointName
		if strings.HasPrefix(checkpointName, ChunkNamePrefix) {
			checkpointID = checkpointName[len(ChunkNamePrefix):]
		}
		chunkName := fmt.Sprintf("%s%s-%d", ChunkNamePrefix, checkpointID, i)

		// Marshal chunk objects to JSON array
		chunkJSON, err := json.Marshal(chunk.objects)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal chunk %d: %w", i, err)
		}

		// Compress and encode (each chunk has its own gzip, not a split of one global gzip)
		compressed, err := r.compressAndEncode(chunkJSON)
		if err != nil {
			return nil, fmt.Errorf("failed to compress chunk %d: %w", i, err)
		}

		objectsCount := len(chunk.objects)

		// Create chunk resource
		chunk := &storagev1alpha1.ManifestCheckpointContentChunk{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "state-snapshotter.deckhouse.io/v1alpha1",
				Kind:       "ManifestCheckpointContentChunk",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: chunkName,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: "state-snapshotter.deckhouse.io/v1alpha1",
						Kind:       "ManifestCheckpoint",
						Name:       checkpointName,
						UID:        types.UID(checkpointUID),
						// Setting Controller=true to enable GC: when checkpoint is deleted, chunks should be deleted
						// Both ManifestCheckpoint and Chunk are cluster-scoped, so ownerRef is valid
						Controller: func() *bool { b := true; return &b }(),
					},
				},
			},
			Spec: storagev1alpha1.ManifestCheckpointContentChunkSpec{
				CheckpointName: checkpointName,
				Index:          i,
				Data:           compressed,
				ObjectsCount:   objectsCount,
				Checksum:       r.calculateChunkChecksum(compressed),
			},
		}

		// Create chunk with retry logic
		r.Logger.Info("Creating chunk",
			"chunk", chunkName,
			"index", i,
			"checkpoint", checkpointName,
			"objects", objectsCount,
			"sizeBytes", len(compressed))
		maxRetries := 3
		var createErr error
		for retry := 0; retry < maxRetries; retry++ {
			createErr = r.Create(ctx, chunk)
			if createErr == nil {
				r.Logger.Info("✅ Chunk created successfully",
					"chunk", chunkName,
					"index", i,
					"objects", objectsCount,
					"sizeBytes", len(compressed))
				break
			}
			if errors.IsAlreadyExists(createErr) {
				// Chunk already exists, get it to verify
				r.Logger.Info("Chunk already exists, verifying",
					"chunk", chunkName,
					"index", i,
					"checkpoint", checkpointName)
				if err := r.Get(ctx, client.ObjectKey{Name: chunkName}, chunk); err != nil {
					r.Logger.Error(err, "Failed to get existing chunk", "chunk", chunkName)
					return nil, fmt.Errorf("failed to get existing chunk %s: %w", chunkName, err)
				}
				// Verify it's the same chunk (same index and checkpoint)
				if chunk.Spec.CheckpointName == checkpointName && chunk.Spec.Index == i {
					r.Logger.Info("✅ Chunk already exists and matches",
						"chunk", chunkName,
						"index", i,
						"checkpoint", chunk.Spec.CheckpointName)
					break
				}
				// If it's a different chunk, this is an error
				r.Logger.Error(nil, "Chunk exists but belongs to different checkpoint",
					"chunk", chunkName,
					"expectedCheckpoint", checkpointName,
					"actualCheckpoint", chunk.Spec.CheckpointName,
					"expectedIndex", i,
					"actualIndex", chunk.Spec.Index)
				return nil, fmt.Errorf("chunk %s already exists but belongs to different checkpoint", chunkName)
			}
			// Log retry with error details
			if retry < maxRetries-1 {
				r.Logger.Info("Retrying chunk creation",
					"chunk", chunkName,
					"retry", retry+1,
					"maxRetries", maxRetries,
					"error", createErr.Error())
			} else {
				r.Logger.Error(createErr, "Failed to create chunk after all retries",
					"chunk", chunkName,
					"retries", maxRetries,
					"checkpoint", checkpointName,
					"index", i,
					"sizeBytes", len(compressed))
			}
		}
		if createErr != nil && !errors.IsAlreadyExists(createErr) {
			return nil, fmt.Errorf("failed to create chunk %s after %d retries: %w", chunkName, maxRetries, createErr)
		}

		// Calculate checksum for chunk
		checksum := r.calculateChunkChecksum(compressed)

		chunkInfos = append(chunkInfos, storagev1alpha1.ChunkInfo{
			Name:         chunkName,
			Index:        i,
			ObjectsCount: objectsCount,
			SizeBytes:    int64(len(compressed)),
			Checksum:     checksum,
		})

		r.Logger.Info("Created chunk",
			"checkpoint", checkpointName,
			"chunk", chunkName,
			"index", i,
			"objects", objectsCount,
			"size", len(compressed))
	}

	return chunkInfos, nil
}

func (r *ManifestCheckpointController) compressAndEncode(data []byte) (string, error) {
	// data should already be a JSON array
	// Compress with gzip
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(data); err != nil {
		return "", fmt.Errorf("failed to write to gzip: %w", err)
	}
	if err := gw.Close(); err != nil {
		return "", fmt.Errorf("failed to close gzip: %w", err)
	}

	// Encode to base64
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// normalizeObjectForJSON normalizes an object to ensure it's a pure map[string]interface{}
// without yaml.MapSlice structures. This prevents Key/Value serialization when reading chunks.
// Uses JSON round-trip to ensure clean format.
func (r *ManifestCheckpointController) normalizeObjectForJSON(obj interface{}) interface{} {
	if obj == nil {
		return nil
	}

	// Use JSON round-trip to normalize the object
	// This ensures any yaml.MapSlice or other special structures are converted to standard JSON types
	jsonData, err := json.Marshal(obj)
	if err != nil {
		r.Logger.Warning("Failed to marshal object for normalization, using as-is", "error", err)
		return obj
	}

	var normalized interface{}
	if err := json.Unmarshal(jsonData, &normalized); err != nil {
		r.Logger.Warning("Failed to unmarshal normalized object, using as-is", "error", err)
		return obj
	}

	return normalized
}

// calculateChunkChecksum calculates SHA256 hash of the compressed chunk data
func (r *ManifestCheckpointController) calculateChunkChecksum(compressedData string) string {
	// Decode base64 to get raw compressed data
	data, err := base64.StdEncoding.DecodeString(compressedData)
	if err != nil {
		// If decoding fails, hash the base64 string itself
		data = []byte(compressedData)
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func (r *ManifestCheckpointController) generateCheckpointName() string {
	// Generate random ID using only RFC 1123 compliant characters (a-z0-9)
	// Use hex encoding to guarantee only 0-9a-f characters (all valid for RFC 1123)
	// Must start and end with alphanumeric (hex always does)
	b := make([]byte, 8)
	rand.Read(b)

	// Hex encoding produces only 0-9a-f, all valid for RFC 1123 subdomain
	// 8 bytes = 16 hex chars, which is a good length
	id := hex.EncodeToString(b)

	return fmt.Sprintf("%s%s", ChunkNamePrefix, id)
}

// determineErrorReason determines the error reason based on the error type
func (r *ManifestCheckpointController) determineErrorReason(err error) string {
	if err == nil {
		return ""
	}
	errStr := err.Error()
	if errors.IsNotFound(err) {
		return ConditionReasonNotFound
	}
	if strings.Contains(errStr, "marshal") || strings.Contains(errStr, "serialize") || strings.Contains(errStr, "json") {
		return ConditionReasonSerializationError
	}
	return ConditionReasonInternalError
}

// loadConfigFromConfigMap loads configuration from ConfigMap (TZ section 7)
func (r *ManifestCheckpointController) loadConfigFromConfigMap(ctx context.Context) error {
	if r.Config == nil {
		return fmt.Errorf("config is nil")
	}

	configMap := &corev1.ConfigMap{}
	configMapName := config.ConfigMapName
	namespace := r.Config.ControllerNamespace

	if err := r.Get(ctx, client.ObjectKey{
		Namespace: namespace,
		Name:      configMapName,
	}, configMap); err != nil {
		// ConfigMap not found - use defaults from Go code
		// This is expected if ConfigMap is not created (user didn't override defaults)
		if errors.IsNotFound(err) {
			r.Logger.Info("ConfigMap not found, using defaults from code",
				"configMap", configMapName,
				"namespace", namespace,
				"maxChunkSizeBytes", r.Config.MaxChunkSizeBytes,
				"defaultTTL", r.Config.DefaultTTL)
			return nil
		}
		return fmt.Errorf("failed to get configmap: %w", err)
	}

	// Load config from ConfigMap data
	r.Config.LoadFromConfigMap(configMap.Data)
	r.Logger.Info("Loaded config from ConfigMap",
		"maxChunkSizeBytes", r.Config.MaxChunkSizeBytes,
		"defaultTTL", r.Config.DefaultTTL,
		"excludeKinds", len(r.Config.ExcludeKinds),
		"excludeAnnotations", len(r.Config.ExcludeAnnotations),
		"enableFiltering", r.Config.EnableFiltering)

	return nil
}

// isNamespacedResource checks if a resource is namespaced or cluster-scoped
// Returns true if namespaced, false if cluster-scoped
// Note: ManifestCaptureRequest is namespaced and is NOT included in clusterScopedKinds
func (r *ManifestCheckpointController) isNamespacedResource(gv schema.GroupVersion, kind string) bool {
	// Common cluster-scoped resources
	// Note: ManifestCaptureRequest is namespaced, not cluster-scoped
	clusterScopedKinds := map[string]bool{
		"Namespace":                      true,
		"Node":                           true,
		"PersistentVolume":               true,
		"ClusterRole":                    true,
		"ClusterRoleBinding":             true,
		"StorageClass":                   true,
		"CustomResourceDefinition":       true,
		"APIService":                     true,
		"MutatingWebhookConfiguration":   true,
		"ValidatingWebhookConfiguration": true,
		"PriorityClass":                  true,
		"CSIDriver":                      true,
		"CSINode":                        true,
		"VolumeSnapshotClass":            true,
		"VolumeSnapshotContent":          true,
		"ManifestCheckpoint":             true,
		"ManifestCheckpointContentChunk": true,
		"Retainer":                       true,
	}

	if clusterScopedKinds[kind] {
		return false
	}

	// For core v1 API group, most resources are namespaced except the ones above
	if gv.Group == "" && gv.Version == "v1" {
		return true
	}

	// For other groups, assume namespaced unless explicitly cluster-scoped
	// This is a heuristic - in production you might want to use RESTMapper
	return true
}

func (r *ManifestCheckpointController) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.ManifestCaptureRequest{}).
		Complete(r)
}
