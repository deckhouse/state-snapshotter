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
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	deckhousev1alpha1 "github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
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
//
// ObjectKeeper is read via APIReader (direct API, no cache) after creation to get UID.
// This ensures read-after-write consistency for the UID barrier pattern.
//
// Controllers MUST read ObjectKeeper via APIReader after creation. APIReader is a required dependency.
type ManifestCheckpointController struct {
	client.Client
	APIReader client.Reader // Required: for reading ObjectKeeper directly from API server after creation
	Scheme    *runtime.Scheme
	Logger    logger.LoggerInterface
	Config    *config.Options
}

// NewManifestCheckpointController creates a new ManifestCheckpointController with validated dependencies.
// All parameters are required and will be validated:
//   - Client: Kubernetes client for reading/writing resources
//   - APIReader: Required for reading ObjectKeeper directly from API server after creation (UID barrier pattern)
//   - Scheme: Kubernetes runtime scheme
//   - Logger: Logger interface for logging
//   - Config: Controller configuration options
func NewManifestCheckpointController(
	client client.Client,
	apiReader client.Reader,
	scheme *runtime.Scheme,
	logger logger.LoggerInterface,
	cfg *config.Options,
) (*ManifestCheckpointController, error) {
	if client == nil {
		return nil, fmt.Errorf("Client must not be nil")
	}
	if apiReader == nil {
		return nil, fmt.Errorf("APIReader must not be nil: controllers require APIReader to read ObjectKeeper after creation (UID barrier pattern)")
	}
	if scheme == nil {
		return nil, fmt.Errorf("Scheme must not be nil")
	}
	if logger == nil {
		return nil, fmt.Errorf("Logger must not be nil")
	}
	if cfg == nil {
		return nil, fmt.Errorf("Config must not be nil")
	}

	return &ManifestCheckpointController{
		Client:    client,
		APIReader: apiReader,
		Scheme:    scheme,
		Logger:    logger,
		Config:    cfg,
	}, nil
}

func (r *ManifestCheckpointController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	r.Logger.Info("Reconciling ManifestCaptureRequest", "namespace", req.Namespace, "name", req.Name)

	// Load config from ConfigMap (TZ section 7)
	if err := r.loadConfigFromConfigMap(ctx); err != nil {
		// If config is nil, this is a fatal misconfiguration - return error
		if r.Config == nil {
			r.Logger.Error(err, "Config is nil - fatal misconfiguration, cannot proceed")
			return ctrl.Result{}, fmt.Errorf("config is nil: %w", err)
		}
		// Other errors (e.g., ConfigMap not found) are non-fatal - use defaults
		r.Logger.Warning("Failed to load config from ConfigMap, using defaults", "error", err)
	}

	mcr := &storagev1alpha1.ManifestCaptureRequest{}
	if err := r.Get(ctx, req.NamespacedName, mcr); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Guard: Check if already in terminal state (Ready=True|False) - MCR is immutable once terminal
	// Terminal state means no further processing - TTL cleanup is handled by background scanner
	if r.isTerminal(mcr) {
		return ctrl.Result{}, nil
	}

	// Guard: Check if checkpoint already exists (idempotency check)
	// If checkpoint exists but MCR is not terminal, finalize MCR status
	if mcr.Status.CheckpointName != "" {
		var checkpoint storagev1alpha1.ManifestCheckpoint
		if err := r.Get(ctx, client.ObjectKey{Name: mcr.Status.CheckpointName}, &checkpoint); err == nil {
			// Checkpoint exists - finalize MCR status (idempotent finalization)
			if err := r.finalizeMCR(ctx, mcr, metav1.ConditionTrue, storagev1alpha1.ManifestCaptureRequestConditionReasonCompleted, fmt.Sprintf("Checkpoint %s already exists", mcr.Status.CheckpointName)); err != nil {
				if errors.IsNotFound(err) {
					return ctrl.Result{}, nil
				}
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
		// Checkpoint doesn't exist - proceed with normal processing
	}

	// Process the request
	return r.processCaptureRequest(ctx, mcr)
}

func (r *ManifestCheckpointController) processCaptureRequest(ctx context.Context, mcr *storagev1alpha1.ManifestCaptureRequest) (ctrl.Result, error) {
	// Set Processing condition if not already set
	// CRITICAL: This must be done BEFORE any long-running operations
	readyCondition := meta.FindStatusCondition(mcr.Status.Conditions, storagev1alpha1.ManifestCaptureRequestConditionTypeReady)
	if readyCondition == nil || readyCondition.Reason != storagev1alpha1.ManifestCaptureRequestConditionReasonProcessing {
		// Handle version conflicts: retry if conflict occurs
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			current := &storagev1alpha1.ManifestCaptureRequest{}
			if err := r.Get(ctx, client.ObjectKeyFromObject(mcr), current); err != nil {
				return err
			}
			// Re-check: maybe Processing was already set by another reconcile
			currentReadyCondition := meta.FindStatusCondition(current.Status.Conditions, storagev1alpha1.ManifestCaptureRequestConditionTypeReady)
			if currentReadyCondition != nil && currentReadyCondition.Reason == storagev1alpha1.ManifestCaptureRequestConditionReasonProcessing {
				// Already Processing - update local mcr and return
				mcr.Status = current.Status
				return nil
			}
			// Set Processing condition on current object
			setSingleCondition(&current.Status.Conditions, metav1.Condition{
				Type:               storagev1alpha1.ManifestCaptureRequestConditionTypeReady,
				Status:             metav1.ConditionFalse,
				Reason:             storagev1alpha1.ManifestCaptureRequestConditionReasonProcessing,
				Message:            "Operation started",
				LastTransitionTime: metav1.Now(),
			})
			// Update status immediately to reflect Processing state
			// This is UX-critical: user must see that operation started
			if err := r.Status().Update(ctx, current); err != nil {
				return err
			}
			// Update local mcr object to reflect the change
			mcr.Status = current.Status
			return nil
		}); err != nil {
			if errors.IsNotFound(err) {
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, err
		}
	}

	// Validate targets
	if len(mcr.Spec.Targets) == 0 {
		if err := r.finalizeMCR(ctx, mcr, metav1.ConditionFalse, storagev1alpha1.ManifestCaptureRequestConditionReasonFailed, "No targets specified"); err != nil {
			if errors.IsNotFound(err) {
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Collect all target objects
	// Note: NotFound errors from collectTargetObjects are always from target objects, not related objects.
	// Related objects (ConfigMaps, Secrets from volumes) are collected in collectRelatedObjects,
	// which silently ignores NotFound errors (checks err == nil). So if we get NotFound here, it's a target.
	objects, err := r.collectTargetObjects(ctx, mcr)
	if err != nil {
		// Simple and clean: NotFound → Ready=False immediately
		// Kubernetes is declarative: if object appears later, user must delete and recreate MCR
		if err := r.finalizeMCR(ctx, mcr, metav1.ConditionFalse, storagev1alpha1.ManifestCaptureRequestConditionReasonFailed, fmt.Sprintf("Failed to collect objects: %v", err)); err != nil {
			if errors.IsNotFound(err) {
				return ctrl.Result{}, nil
			}
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

	// Determine checkpoint name: use existing if available, otherwise generate deterministic name
	// Checkpoint name is deterministic based on MCR UID to prevent duplicate checkpoints
	// if reconciliation happens multiple times before status is updated
	var checkpointName string
	if mcr.Status.CheckpointName != "" {
		// Checkpoint already exists - use existing name (e.g., for retainer migration)
		checkpointName = mcr.Status.CheckpointName
		r.Logger.Info("Using existing checkpoint name",
			"mcr", fmt.Sprintf("%s/%s", mcr.Namespace, mcr.Name),
			"checkpoint", checkpointName)
	} else {
		// Generate deterministic checkpoint name based on MCR UID
		// This prevents creating multiple checkpoints if reconciliation happens multiple times
		// before status is successfully updated
		checkpointName = r.generateCheckpointNameFromUID(string(mcr.UID))
		r.Logger.Info("Starting checkpoint creation",
			"mcr", fmt.Sprintf("%s/%s", mcr.Namespace, mcr.Name),
			"checkpoint", checkpointName,
			"targets", len(mcr.Spec.Targets))
	}

	// ADR: Create only ONE ObjectKeeper: ret-mcr-<namespace>-<mcrName>
	// This ObjectKeeper:
	// - Uses FollowObject mode to follow MCR (no TTL)
	// - Holds the ManifestCheckpoint (MCP has ownerRef to this ObjectKeeper)
	// - When MCR is deleted, ObjectKeeper is automatically deleted (FollowObject)
	// - When ObjectKeeper is deleted, GC deletes MCP (via ownerRef)
	// - TTL and request cleanup are handled by MCR controller, not ObjectKeeper
	retainerName := fmt.Sprintf("ret-mcr-%s-%s", mcr.Namespace, mcr.Name)
	r.Logger.Info("Step 1: Creating ObjectKeeper for MCR", "objectKeeper", retainerName, "mcr", fmt.Sprintf("%s/%s", mcr.Namespace, mcr.Name))
	// Update Processing message to show progress
	_ = r.updateProcessingMessage(ctx, mcr, "Creating ObjectKeeper...")

	objectKeeper := &deckhousev1alpha1.ObjectKeeper{}
	err = r.Get(ctx, client.ObjectKey{Name: retainerName}, objectKeeper)
	switch {
	case errors.IsNotFound(err):
		objectKeeper = &deckhousev1alpha1.ObjectKeeper{
			TypeMeta: metav1.TypeMeta{
				APIVersion: DeckhouseAPIVersion,
				Kind:       KindObjectKeeper,
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: retainerName,
			},
			Spec: deckhousev1alpha1.ObjectKeeperSpec{
				Mode: "FollowObject",
				FollowObjectRef: &deckhousev1alpha1.FollowObjectRef{
					APIVersion: "state-snapshotter.deckhouse.io/v1alpha1",
					Kind:       "ManifestCaptureRequest",
					Namespace:  mcr.Namespace,
					Name:       mcr.Name,
					UID:        string(mcr.UID),
				},
			},
		}
		if err := r.Create(ctx, objectKeeper); err != nil {
			r.Logger.Error(err, "Failed to create ObjectKeeper", "name", retainerName)
			if err := r.finalizeMCR(ctx, mcr, metav1.ConditionFalse, storagev1alpha1.ManifestCaptureRequestConditionReasonFailed, fmt.Sprintf("Failed to create ObjectKeeper: %v", err)); err != nil {
				if errors.IsNotFound(err) {
					return ctrl.Result{}, nil
				}
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, err
		}
		r.Logger.Info("Created ObjectKeeper", "name", retainerName)
		// After create, ensure it's observable via apiserver (UID barrier pattern)
		// Always read via APIReader (direct API, no cache) to ensure read-after-write consistency
		// This guarantees that ObjectKeeper is visible in apiserver before proceeding
		// This is not retry logic - it's handling cache lag after Create()
		if err := r.APIReader.Get(ctx, client.ObjectKey{Name: retainerName}, objectKeeper); err != nil {
			// UID barrier: if object is not yet observable via APIReader, requeue briefly
			// This ensures read-after-write consistency for ownerRef UID
			// This is not retry logic - it's handling cache lag after Create()
			return ctrl.Result{RequeueAfter: 200 * time.Millisecond}, nil
		}
		r.Logger.Info("✅ Step 1 complete: Created ObjectKeeper",
			"objectKeeper", retainerName,
			"uid", objectKeeper.UID,
			"checkpoint", checkpointName,
			"mcr", fmt.Sprintf("%s/%s", mcr.Namespace, mcr.Name))
	case err != nil:
		return ctrl.Result{}, fmt.Errorf("failed to get ObjectKeeper: %w", err)
	default:
		// ObjectKeeper exists - validate it belongs to this MCR
		// This protects against race conditions where MCR was deleted and recreated with same name
		if objectKeeper.Spec.FollowObjectRef == nil {
			return ctrl.Result{}, fmt.Errorf("ObjectKeeper %s has no FollowObjectRef", retainerName)
		}
		if objectKeeper.Spec.FollowObjectRef.UID != string(mcr.UID) {
			return ctrl.Result{}, fmt.Errorf("ObjectKeeper %s belongs to another MCR (UID mismatch: expected %s, got %s)",
				retainerName, string(mcr.UID), objectKeeper.Spec.FollowObjectRef.UID)
		}
		r.Logger.Info("ObjectKeeper already exists, using existing", "objectKeeper", retainerName, "uid", objectKeeper.UID)
	}

	// If checkpoint already exists → finalize MCR (idempotent finalization)
	// This handles the case where checkpoint was created but MCR status update failed
	var existingCheckpoint storagev1alpha1.ManifestCheckpoint
	if err := r.Get(ctx, client.ObjectKey{Name: checkpointName}, &existingCheckpoint); err == nil {
		r.Logger.Info("Checkpoint already exists, finalizing MCR",
			"checkpoint", checkpointName)
		mcr.Status.CheckpointName = checkpointName
		if err := r.finalizeMCR(ctx, mcr, metav1.ConditionTrue, storagev1alpha1.ManifestCaptureRequestConditionReasonCompleted, fmt.Sprintf("Checkpoint %s already exists", checkpointName)); err != nil {
			if errors.IsNotFound(err) {
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Create ManifestCheckpoint with ownerRef to ret-mcr-* ObjectKeeper
	// ADR: Checkpoint MUST have ownerRef ONLY on ret-mcr-<namespace>-<mcrName>
	// This is the single ObjectKeeper that holds both MCR and MCP
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
					APIVersion: DeckhouseAPIVersion,
					Kind:       KindObjectKeeper,
					Name:       retainerName, // This is ret-mcr-<namespace>-<mcrName>
					UID:        objectKeeper.UID,
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
				"ownerUID", objectKeeper.UID)
			if err := r.finalizeMCR(ctx, mcr, metav1.ConditionFalse, storagev1alpha1.ManifestCaptureRequestConditionReasonFailed, fmt.Sprintf("Failed to create checkpoint: %v", err)); err != nil {
				if errors.IsNotFound(err) {
					return ctrl.Result{}, nil
				}
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
		// Checkpoint already exists - check if it's still Processing
		readyCondition := meta.FindStatusCondition(checkpoint.Status.Conditions, storagev1alpha1.ManifestCheckpointConditionTypeReady)
		if readyCondition == nil || readyCondition.Reason != storagev1alpha1.ManifestCheckpointConditionReasonProcessing {
			// Not Processing - set Processing condition
			now := metav1.Now()
			setSingleCondition(&checkpoint.Status.Conditions, metav1.Condition{
				Type:               storagev1alpha1.ManifestCheckpointConditionTypeReady,
				Status:             metav1.ConditionFalse,
				Reason:             storagev1alpha1.ManifestCheckpointConditionReasonProcessing,
				Message:            "Resuming checkpoint creation, creating chunks...",
				LastTransitionTime: now,
			})
			if err := r.Status().Update(ctx, checkpoint); err != nil {
				r.Logger.Error(err, "Failed to set Processing condition on existing checkpoint", "checkpoint", checkpointName)
				// Non-critical error, continue processing
			}
		}
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
			"objectKeeperName", retainerName,
			"objectKeeperUID", objectKeeper.UID)
		// Set Processing condition on checkpoint immediately after creation
		now := metav1.Now()
		setSingleCondition(&checkpoint.Status.Conditions, metav1.Condition{
			Type:               storagev1alpha1.ManifestCheckpointConditionTypeReady,
			Status:             metav1.ConditionFalse,
			Reason:             storagev1alpha1.ManifestCheckpointConditionReasonProcessing,
			Message:            "Checkpoint created, creating chunks...",
			LastTransitionTime: now,
		})
		if err := r.Status().Update(ctx, checkpoint); err != nil {
			r.Logger.Error(err, "Failed to set Processing condition on checkpoint", "checkpoint", checkpointName)
			// Non-critical error, continue processing
		}
		// Update Processing message to show progress
		_ = r.updateProcessingMessage(ctx, mcr, fmt.Sprintf("Created checkpoint %s, collecting objects...", checkpointName))
	}

	// NOW create chunks (checkpoint exists, so ownerRef will work)
	// CRITICAL: checkpoint.UID is now populated after Create, use it in ownerRef
	r.Logger.Info("Step 3: Creating chunks",
		"checkpoint", checkpointName,
		"checkpointUID", checkpoint.UID,
		"objects", len(objects))
	// Update Processing message to show progress
	_ = r.updateProcessingMessage(ctx, mcr, fmt.Sprintf("Creating chunks for checkpoint %s (%d objects)...", checkpointName, len(objects)))
	chunks, err := r.createChunks(ctx, checkpointName, string(checkpoint.UID), objects)
	if err != nil {
		r.Logger.Error(err, "❌ Step 3 failed: Failed to create chunks",
			"checkpoint", checkpointName,
			"objects", len(objects),
			"error", err.Error())
		// Update checkpoint status to Failed
		now := metav1.Now()
		setSingleCondition(&checkpoint.Status.Conditions, metav1.Condition{
			Type:               storagev1alpha1.ManifestCheckpointConditionTypeReady,
			Status:             metav1.ConditionFalse,
			Reason:             storagev1alpha1.ManifestCheckpointConditionReasonFailed,
			Message:            fmt.Sprintf("Failed to create chunks: %v", err),
			LastTransitionTime: now,
		})
		if updateErr := r.Status().Update(ctx, checkpoint); updateErr != nil {
			r.Logger.Error(updateErr, "Failed to update checkpoint status to Failed", "checkpoint", checkpointName)
		}
		if err := r.finalizeMCR(ctx, mcr, metav1.ConditionFalse, storagev1alpha1.ManifestCaptureRequestConditionReasonFailed, fmt.Sprintf("Failed to create chunks: %v", err)); err != nil {
			if errors.IsNotFound(err) {
				return ctrl.Result{}, nil
			}
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
		Type:               storagev1alpha1.ManifestCheckpointConditionTypeReady,
		Status:             metav1.ConditionTrue,
		Reason:             storagev1alpha1.ManifestCheckpointConditionReasonCompleted,
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
	mcr.Status.CheckpointName = checkpointName
	if err := r.finalizeMCR(ctx, mcr, metav1.ConditionTrue, storagev1alpha1.ManifestCaptureRequestConditionReasonCompleted, fmt.Sprintf("Checkpoint %s created successfully", checkpointName)); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// NOTE: ObjectKeeper uses FollowObject mode (no TTL)
	// ObjectKeeper follows MCR lifecycle and is automatically deleted when MCR is deleted
	// TTL and request cleanup are handled by MCR controller, not ObjectKeeper

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
			// Preserve original error for IsNotFound check in caller
			// errors.IsNotFound works with wrapped errors (fmt.Errorf with %w preserves error type via errors.Unwrap)
			// NotFound → Ready=False immediately (terminal state)
			// To retry, user must delete and recreate MCR
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
		// Get gzip bytes first for size calculation
		gzipBytes, err := r.compressToBytes(emptyJSON)
		if err != nil {
			return nil, fmt.Errorf("failed to compress empty chunk: %w", err)
		}
		// Encode to base64 for storage
		compressed := base64.StdEncoding.EncodeToString(gzipBytes)

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

		// Calculate checksum for ChunkInfo (same as in chunk resource)
		checksum := r.calculateChunkChecksum(compressed)

		return []storagev1alpha1.ChunkInfo{
			{
				Name:         chunkName,
				Index:        0,
				ObjectsCount: 0,
				SizeBytes:    int64(len(gzipBytes)), // Use gzip bytes size, not base64 string size
				Checksum:     checksum,
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

		// Compress to check actual size (compare gzip bytes, not base64 string)
		gzipBytes, err := r.compressToBytes(testJSON)
		if err != nil {
			return nil, fmt.Errorf("failed to compress test chunk: %w", err)
		}

		// If compressed size exceeds limit, finalize current chunk first
		// Compare gzip bytes size (not base64 string) to match etcd/apiserver object size limits
		if len(gzipBytes) > int(r.Config.MaxChunkSizeBytes) {
			// If current chunk is not empty, save it
			if len(currentChunk.objects) > 0 {
				chunks = append(chunks, currentChunk)
				currentChunk = chunkData{objects: make([]interface{}, 0)}
			}

			// Now check if single object exceeds limit
			singleObjJSON, _ := json.Marshal([]interface{}{obj})
			singleGzipBytes, err := r.compressToBytes(singleObjJSON)
			if err == nil && len(singleGzipBytes) > int(r.Config.MaxChunkSizeBytes) {
				r.Logger.Warning("Object exceeds MaxChunkSizeBytes - storing as-is, may break etcd on large clusters",
					"compressedSizeBytes", len(singleGzipBytes),
					"maxSizeBytes", r.Config.MaxChunkSizeBytes)
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
		// Get gzip bytes first for size calculation (SizeBytes should reflect etcd/apiserver object size)
		gzipBytes, err := r.compressToBytes(chunkJSON)
		if err != nil {
			return nil, fmt.Errorf("failed to compress chunk %d: %w", i, err)
		}
		// Encode to base64 for storage
		compressed := base64.StdEncoding.EncodeToString(gzipBytes)

		objectsCount := len(chunk.objects)

		// Calculate checksum once (used both in chunk resource and ChunkInfo)
		checksum := r.calculateChunkChecksum(compressed)

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
				Checksum:       checksum,
			},
		}

		// Create chunk (fail-fast semantics - consistent with MCR lifecycle)
		r.Logger.Info("Creating chunk",
			"chunk", chunkName,
			"index", i,
			"checkpoint", checkpointName,
			"objects", objectsCount,
			"sizeBytes", len(gzipBytes))
		err = r.Create(ctx, chunk)
		switch {
		case err == nil:
			r.Logger.Info("✅ Chunk created successfully",
				"chunk", chunkName,
				"index", i,
				"objects", objectsCount,
				"sizeBytes", len(gzipBytes))
		case errors.IsAlreadyExists(err):
			// Chunk already exists, get it to verify (idempotency check)
			r.Logger.Info("Chunk already exists, verifying",
				"chunk", chunkName,
				"index", i,
				"checkpoint", checkpointName)
			if err := r.Get(ctx, client.ObjectKey{Name: chunkName}, chunk); err != nil {
				r.Logger.Error(err, "Failed to get existing chunk", "chunk", chunkName)
				return nil, fmt.Errorf("failed to get existing chunk %s: %w", chunkName, err)
			}
			// Verify it's the same chunk (same index, checkpoint, and checksum)
			if chunk.Spec.CheckpointName == checkpointName && chunk.Spec.Index == i {
				// Verify checksum matches to ensure data consistency (idempotency check)
				if chunk.Spec.Checksum != checksum {
					r.Logger.Error(nil, "Chunk exists but checksum mismatch (data inconsistency detected)",
						"chunk", chunkName,
						"expectedChecksum", checksum,
						"actualChecksum", chunk.Spec.Checksum,
						"checkpoint", checkpointName,
						"index", i)
					return nil, fmt.Errorf("chunk %s already exists but checksum mismatch: expected %s, got %s", chunkName, checksum, chunk.Spec.Checksum)
				}
				r.Logger.Info("✅ Chunk already exists and matches (index, checkpoint, checksum)",
					"chunk", chunkName,
					"index", i,
					"checkpoint", chunk.Spec.CheckpointName,
					"checksum", checksum)
			} else {
				// If it's a different chunk, this is an error
				r.Logger.Error(nil, "Chunk exists but belongs to different checkpoint",
					"chunk", chunkName,
					"expectedCheckpoint", checkpointName,
					"actualCheckpoint", chunk.Spec.CheckpointName,
					"expectedIndex", i,
					"actualIndex", chunk.Spec.Index)
				return nil, fmt.Errorf("chunk %s already exists but belongs to different checkpoint", chunkName)
			}
		default:
			// Fail-fast: any other error → terminal failure
			r.Logger.Error(err, "Failed to create chunk",
				"chunk", chunkName,
				"checkpoint", checkpointName,
				"index", i,
				"sizeBytes", len(gzipBytes))
			return nil, fmt.Errorf("failed to create chunk %s: %w", chunkName, err)
		}

		// checksum already calculated above, reuse it
		chunkInfos = append(chunkInfos, storagev1alpha1.ChunkInfo{
			Name:         chunkName,
			Index:        i,
			ObjectsCount: objectsCount,
			SizeBytes:    int64(len(gzipBytes)), // Use gzip bytes size, not base64 string size
			Checksum:     checksum,              // Reuse checksum calculated above
		})

		r.Logger.Info("Created chunk",
			"checkpoint", checkpointName,
			"chunk", chunkName,
			"index", i,
			"objects", objectsCount,
			"sizeBytes", len(gzipBytes))
	}

	return chunkInfos, nil
}

// compressToBytes compresses data with gzip and returns the compressed bytes.
// This is used for size checking against MaxChunkSizeBytes limit.
func (r *ManifestCheckpointController) compressToBytes(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(data); err != nil {
		return nil, fmt.Errorf("failed to write to gzip: %w", err)
	}
	if err := gw.Close(); err != nil {
		return nil, fmt.Errorf("failed to close gzip: %w", err)
	}
	return buf.Bytes(), nil
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

// generateCheckpointNameFromUID generates a deterministic checkpoint name from MCR UID
// This prevents creating multiple checkpoints if reconciliation happens multiple times
// before status is successfully updated (e.g., due to status update failures)
func (r *ManifestCheckpointController) generateCheckpointNameFromUID(mcrUID string) string {
	// Use SHA256 hash of MCR UID to get deterministic, RFC 1123 compliant name
	// Take first 16 hex chars (8 bytes) for checkpoint ID
	hash := sha256.Sum256([]byte(mcrUID))
	id := hex.EncodeToString(hash[:8]) // 8 bytes = 16 hex chars

	return fmt.Sprintf("%s%s", ChunkNamePrefix, id)
}

// loadConfigFromConfigMap loads controller configuration from optional ConfigMap
// ConfigMap name: state-snapshotter-config (in controller namespace)
// This ConfigMap is optional - if not found, controller uses defaults from Go code
// ConfigMap allows runtime configuration without restart:
//   - maxChunkSizeBytes: maximum chunk size for checkpoint content (default: 800000)
//   - defaultTTL: default TTL for ManifestCaptureRequest (default: 168h, see config.DefaultTTL)
//   - excludeKinds: comma-separated list of kinds to exclude from snapshots
//   - excludeAnnotations: comma-separated list of annotations to exclude
//   - enableFiltering: enable object filtering/cleaning (default: false)
//
// See templates/controller/configmap.yaml for ConfigMap structure
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
		// This is expected and normal if user didn't provide custom configuration via Helm values
		// ConfigMap is only created when user sets controller.config.* values in Helm chart
		if errors.IsNotFound(err) {
			r.Logger.Debug("Optional controller ConfigMap not found, using defaults from code",
				"configMap", configMapName,
				"namespace", namespace,
				"note", "ConfigMap is optional - create it via Helm values (controller.config.*) to override defaults",
				"defaultMaxChunkSizeBytes", r.Config.MaxChunkSizeBytes,
				"defaultTTL", r.Config.DefaultTTL)
			return nil
		}
		return fmt.Errorf("failed to get controller ConfigMap %s/%s: %w", namespace, configMapName, err)
	}

	// Load config from ConfigMap data (overrides defaults)
	r.Config.LoadFromConfigMap(configMap.Data)
	r.Logger.Info("Loaded controller configuration from ConfigMap",
		"configMap", fmt.Sprintf("%s/%s", namespace, configMapName),
		"maxChunkSizeBytes", r.Config.MaxChunkSizeBytes,
		"defaultTTL", r.Config.DefaultTTL,
		"excludeKinds", len(r.Config.ExcludeKinds),
		"excludeAnnotations", len(r.Config.ExcludeAnnotations),
		"enableFiltering", r.Config.EnableFiltering)

	return nil
}

// isTerminal checks if MCR is in terminal state.
// Terminal MCRs are immutable and should not be processed further.
// Terminality is determined by reason, not just status:
// - Processing is NOT terminal (operation in progress)
// - Completed, Failed are terminal
func (r *ManifestCheckpointController) isTerminal(mcr *storagev1alpha1.ManifestCaptureRequest) bool {
	readyCondition := meta.FindStatusCondition(mcr.Status.Conditions, storagev1alpha1.ManifestCaptureRequestConditionTypeReady)
	if readyCondition == nil {
		return false // No condition = not terminal
	}

	// Processing is NOT terminal
	if readyCondition.Reason == storagev1alpha1.ManifestCaptureRequestConditionReasonProcessing {
		return false
	}

	// Terminal states:
	// - True (always with Completed reason per contract)
	// - False with Failed (covers all failure cases)
	return readyCondition.Status == metav1.ConditionTrue ||
		readyCondition.Reason == storagev1alpha1.ManifestCaptureRequestConditionReasonFailed
}

// updateProcessingMessage updates the message in Processing condition without changing reason or LastTransitionTime.
// This allows showing progress to the user during long-running operations.
func (r *ManifestCheckpointController) updateProcessingMessage(
	ctx context.Context,
	mcr *storagev1alpha1.ManifestCaptureRequest,
	message string,
) error {
	readyCondition := meta.FindStatusCondition(mcr.Status.Conditions, storagev1alpha1.ManifestCaptureRequestConditionTypeReady)
	if readyCondition == nil || readyCondition.Reason != storagev1alpha1.ManifestCaptureRequestConditionReasonProcessing {
		// Not in Processing state - nothing to update
		return nil
	}

	// Update only message, preserve reason and LastTransitionTime
	setSingleCondition(&mcr.Status.Conditions, metav1.Condition{
		Type:               storagev1alpha1.ManifestCaptureRequestConditionTypeReady,
		Status:             metav1.ConditionFalse,
		Reason:             storagev1alpha1.ManifestCaptureRequestConditionReasonProcessing,
		Message:            message,
		LastTransitionTime: readyCondition.LastTransitionTime, // Preserve original time
	})

	// Update status (best-effort, no retry for progress updates)
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &storagev1alpha1.ManifestCaptureRequest{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(mcr), current); err != nil {
			return err
		}
		// Re-check: still Processing?
		currentReadyCondition := meta.FindStatusCondition(current.Status.Conditions, storagev1alpha1.ManifestCaptureRequestConditionTypeReady)
		if currentReadyCondition == nil || currentReadyCondition.Reason != storagev1alpha1.ManifestCaptureRequestConditionReasonProcessing {
			// No longer Processing - skip update
			return nil
		}
		setSingleCondition(&current.Status.Conditions, metav1.Condition{
			Type:               storagev1alpha1.ManifestCaptureRequestConditionTypeReady,
			Status:             metav1.ConditionFalse,
			Reason:             storagev1alpha1.ManifestCaptureRequestConditionReasonProcessing,
			Message:            message,
			LastTransitionTime: currentReadyCondition.LastTransitionTime, // Preserve original time
		})
		return r.Status().Update(ctx, current)
	}); err != nil {
		// Log but don't fail - progress updates are best-effort
		r.Logger.Debug("Failed to update Processing message (non-critical)", "error", err)
		return nil
	}
	return nil
}

// finalizeMCR finalizes MCR by setting Ready condition, CompletionTimestamp, updating status, and TTL annotation.
// This is a unified helper to eliminate code duplication across all finalization paths.
func (r *ManifestCheckpointController) finalizeMCR(
	ctx context.Context,
	mcr *storagev1alpha1.ManifestCaptureRequest,
	status metav1.ConditionStatus,
	reason, message string,
) error {
	// Validate reason matches status per contract
	if status == metav1.ConditionTrue {
		// Completed is the ONLY allowed reason for True
		if reason != storagev1alpha1.ManifestCaptureRequestConditionReasonCompleted {
			reason = storagev1alpha1.ManifestCaptureRequestConditionReasonCompleted // Force Completed for True
		}
	}
	if status == metav1.ConditionFalse {
		// Must have explicit reason for False
		if reason == "" || reason == storagev1alpha1.ManifestCaptureRequestConditionReasonCompleted {
			reason = storagev1alpha1.ManifestCaptureRequestConditionReasonFailed // Default to Failed for False
		}
	}

	now := metav1.Now()
	if mcr.Status.CompletionTimestamp == nil {
		mcr.Status.CompletionTimestamp = &now
	}
	setSingleCondition(&mcr.Status.Conditions, metav1.Condition{
		Type:               storagev1alpha1.ManifestCaptureRequestConditionTypeReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	})

	// Update status (retry only for status conflicts)
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &storagev1alpha1.ManifestCaptureRequest{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(mcr), current); err != nil {
			return err
		}
		base := current.DeepCopy()
		current.Status = mcr.Status
		return r.Status().Patch(ctx, current, client.MergeFrom(base))
	}); err != nil {
		if errors.IsNotFound(err) {
			return nil // Object deleted - fine
		}
		return fmt.Errorf("failed to update MCR status: %w", err)
	}

	// Update TTL annotation (informational only, best-effort, no retry)
	current := &storagev1alpha1.ManifestCaptureRequest{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(mcr), current); err == nil {
		base := current.DeepCopy()
		r.setTTLAnnotation(current)
		_ = r.Patch(ctx, current, client.MergeFrom(base)) // Ignore errors - annotation is informational
	}

	return nil
}

// setTTLAnnotation sets TTL annotation on the object.
//
// IMPORTANT TTL SEMANTICS:
// - TTL annotation (state-snapshotter.deckhouse.io/ttl) is INFORMATIONAL ONLY.
// - Actual TTL deletion timing is controlled by controller configuration (config.DefaultTTL).
// - TTL scanner uses config.DefaultTTL, NOT the annotation value.
// - Annotation is set for observability and post-mortem analysis, but does not affect deletion timing.
//
// TTL is set when Ready/Failed condition is set during finalization.
// TTL comes from configuration (state-snapshotter module settings), not from MCR spec.
// If annotation already exists, it is not overwritten (idempotent).
func (r *ManifestCheckpointController) setTTLAnnotation(mcr *storagev1alpha1.ManifestCaptureRequest) {
	// Don't overwrite if annotation already exists
	if mcr.Annotations != nil {
		if _, exists := mcr.Annotations[AnnotationKeyTTL]; exists {
			return
		}
	}
	if mcr.Annotations == nil {
		mcr.Annotations = make(map[string]string)
	}
	// Get TTL from configuration (default: 168h)
	ttlStr := config.DefaultTTLStr
	if r.Config != nil && r.Config.DefaultTTLStr != "" {
		ttlStr = r.Config.DefaultTTLStr
	}
	mcr.Annotations[AnnotationKeyTTL] = ttlStr
}

func (r *ManifestCheckpointController) SetupWithManager(mgr ctrl.Manager) error {
	// Runtime assertion: ensure required dependencies are set (fail-fast on incorrect wiring)
	if r.APIReader == nil {
		return fmt.Errorf("APIReader is required (UID barrier pattern)")
	}
	if r.Config == nil {
		return fmt.Errorf("Config is required")
	}
	if r.Logger == nil {
		return fmt.Errorf("Logger is required")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.ManifestCaptureRequest{}).
		Complete(r)
}

// StartTTLScanner starts the TTL scanner.
// Should be called from manager.RunnableFunc to ensure it runs only on the leader replica.
// Scanner periodically lists all MCRs and deletes expired ones based on completionTimestamp + TTL.
//
// IMPORTANT: This method should be called from manager.RunnableFunc to ensure leader-only execution.
// RunnableFunc already runs in a separate goroutine, so we don't need an additional go statement.
// When leadership changes, ctx.Done() triggers graceful shutdown of the scanner.
// Scanner uses List() to get all MCRs and checks completionTimestamp + TTL from controller config.
// This approach is simpler than per-object reconcile and doesn't block the reconcile loop.
//
// TTL SCANNER CONTRACT:
//
// 1. Works only with terminal MCRs:
//   - Ready=True (completed successfully)
//   - Ready=False (failed, terminal error)
//   - Non-terminal MCRs are never touched
//
// 2. TTL source:
//   - TTL is ALWAYS taken from controller configuration (config.DefaultTTL), NOT from MCR annotations
//   - TTL annotation (state-snapshotter.deckhouse.io/ttl) is informational only and does not affect deletion timing
//   - This ensures predictable cluster-wide retention policy
//
// 3. TTL calculation:
//   - TTL starts counting from CompletionTimestamp (when MCR reaches Ready=True or Ready=False)
//   - Expiration time = CompletionTimestamp + config.DefaultTTL
//   - Only MCRs with CompletionTimestamp are eligible for deletion
//
// 4. Scanner behavior:
//   - Scanner does NOT update status
//   - Scanner does NOT patch objects
//   - Scanner only performs List() and Delete() operations
//   - Deletion of MCR triggers GC of ObjectKeeper and ManifestCheckpoint through ownerReferences
//
// 5. Leader-only execution:
//   - Scanner runs only on the leader replica (enforced by manager.RunnableFunc)
//   - When leadership changes, ctx.Done() triggers graceful shutdown
func (r *ManifestCheckpointController) StartTTLScanner(ctx context.Context, client client.Client) {
	// Scanner interval: check every 5 minutes
	// This is a reasonable balance between responsiveness and API load
	scannerInterval := 5 * time.Minute
	ticker := time.NewTicker(scannerInterval)
	defer ticker.Stop()

	l := log.FromContext(ctx)
	l.Info("TTL scanner started", "interval", scannerInterval)

	// Run immediately on startup, then periodically
	r.scanAndDeleteExpiredMCRs(ctx, client)

	for {
		select {
		case <-ctx.Done():
			l.Info("TTL scanner stopped (context cancelled)")
			return
		case <-ticker.C:
			r.scanAndDeleteExpiredMCRs(ctx, client)
		}
	}
}

// scanAndDeleteExpiredMCRs lists all MCRs and deletes those where completionTimestamp + TTL < now.
//
// IMPORTANT:
// TTL annotation (state-snapshotter.deckhouse.io/ttl) is informational only.
// Actual TTL is controlled exclusively by controller configuration.
// This ensures predictable cluster-wide retention policy.
//
// TTL SEMANTICS:
// - TTL is ALWAYS taken from controller configuration (config.DefaultTTL), NOT from MCR annotations.
// - TTL annotation (state-snapshotter.deckhouse.io/ttl) is informational only and is IGNORED by the scanner.
// - This ensures consistent cleanup behavior: all MCRs use the same TTL policy defined by controller config.
// - TTL starts counting from CompletionTimestamp (when MCR reaches Ready=True or Ready=False).
func (r *ManifestCheckpointController) scanAndDeleteExpiredMCRs(ctx context.Context, client client.Client) {
	// Get TTL from controller config (this is the ONLY source of TTL timing)
	// TTL annotation is informational only and is ignored here
	defaultTTL := config.DefaultTTL
	if r.Config != nil && r.Config.DefaultTTL > 0 {
		defaultTTL = r.Config.DefaultTTL
	}

	// Guard: if TTL is disabled (<= 0), skip scanning
	if defaultTTL <= 0 {
		log.FromContext(ctx).V(1).Info("TTL scanner disabled (ttl <= 0)")
		return
	}

	// List all MCRs across all namespaces
	mcrList := &storagev1alpha1.ManifestCaptureRequestList{}
	if err := client.List(ctx, mcrList); err != nil {
		log.FromContext(ctx).Error(err, "Failed to list ManifestCaptureRequests for TTL scan")
		return
	}

	now := time.Now()
	deletedCount := 0
	skippedCount := 0

	for i := range mcrList.Items {
		mcr := &mcrList.Items[i]

		// Skip if not terminal (Ready=True or Ready=False)
		if !r.isTerminal(mcr) {
			skippedCount++
			continue // Skip non-terminal MCRs
		}

		// Skip if no completionTimestamp
		if mcr.Status.CompletionTimestamp == nil {
			skippedCount++
			continue
		}

		// Check if TTL expired: completionTimestamp + defaultTTL < now
		completionTime := mcr.Status.CompletionTimestamp.Time
		expirationTime := completionTime.Add(defaultTTL)

		if now.After(expirationTime) {
			// TTL expired, delete the object
			log.FromContext(ctx).Info("TTL expired, deleting ManifestCaptureRequest",
				"namespace", mcr.Namespace,
				"name", mcr.Name,
				"completionTime", completionTime,
				"expirationTime", expirationTime,
				"ttl", defaultTTL)

			// Best-effort cleanup of artifacts that have no ownerRef
			if err := r.cleanupArtifactsForMCR(ctx, mcr); err != nil {
				log.FromContext(ctx).Error(err, "Failed to cleanup MCR artifacts (ownerRef missing)",
					"namespace", mcr.Namespace,
					"name", mcr.Name)
			}

			if err := client.Delete(ctx, mcr); err != nil {
				if errors.IsNotFound(err) {
					// Already deleted, that's fine (double-delete is safe)
					log.FromContext(ctx).V(1).Info("MCR already deleted during TTL scan",
						"namespace", mcr.Namespace,
						"name", mcr.Name)
				} else {
					log.FromContext(ctx).Error(err, "Failed to delete expired ManifestCaptureRequest",
						"namespace", mcr.Namespace,
						"name", mcr.Name)
				}
			} else {
				deletedCount++
			}
		}
	}

	if deletedCount > 0 || skippedCount > 0 {
		log.FromContext(ctx).V(1).Info("TTL scan completed",
			"total", len(mcrList.Items),
			"deleted", deletedCount,
			"skipped", skippedCount)
	}
}

// cleanupArtifactsForMCR deletes MCR-created artifacts if they have no ownerRef.
// This is a best-effort cleanup used by the TTL scanner to avoid orphaned artifacts.
func (r *ManifestCheckpointController) cleanupArtifactsForMCR(ctx context.Context, mcr *storagev1alpha1.ManifestCaptureRequest) error {
	checkpointName := mcr.Status.CheckpointName
	if checkpointName == "" {
		return nil
	}

	checkpoint := &storagev1alpha1.ManifestCheckpoint{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: checkpointName}, checkpoint); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}

	// If checkpoint has ownerRef, assume it is managed by SnapshotContent and skip deletion.
	if hasSnapshotContentOwnerRef(checkpoint.OwnerReferences) {
		return nil
	}

	// Delete chunks that have no ownerRef.
	for _, chunkInfo := range checkpoint.Status.Chunks {
		if chunkInfo.Name == "" {
			continue
		}
		chunk := &storagev1alpha1.ManifestCheckpointContentChunk{}
		if err := r.APIReader.Get(ctx, client.ObjectKey{Name: chunkInfo.Name}, chunk); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return err
		}
		if hasSnapshotContentOwnerRef(chunk.OwnerReferences) {
			continue
		}
		if err := r.Delete(ctx, chunk); err != nil && !errors.IsNotFound(err) {
			return err
		}
	}

	// Delete checkpoint itself (no ownerRef)
	if err := r.Delete(ctx, checkpoint); err != nil && !errors.IsNotFound(err) {
		return err
	}

	return nil
}

func hasSnapshotContentOwnerRef(refs []metav1.OwnerReference) bool {
	for _, ref := range refs {
		if strings.HasSuffix(ref.Kind, "SnapshotContent") {
			return true
		}
	}
	return false
}
