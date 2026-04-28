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

package api //nolint:revive // package name matches internal/api directory

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

// ArchiveHandler handles HTTP requests for archive operations
type ArchiveHandler struct {
	client         client.Client
	archiveService *usecase.ArchiveService
	logger         logger.LoggerInterface
}

// NewArchiveHandler creates a new ArchiveHandler
func NewArchiveHandler(client client.Client, archiveService *usecase.ArchiveService, logger logger.LoggerInterface) *ArchiveHandler {
	return &ArchiveHandler{
		client:         client,
		archiveService: archiveService,
		logger:         logger,
	}
}

// ErrorResponse represents an error response
type ErrorResponse struct {
	Error   string `json:"error"`
	Details string `json:"details,omitempty"`
}

// ============================================================================
// Checkpoint API Endpoints
// ============================================================================

// HandleGetCheckpointArchive handles GET /api/v1/checkpoints/<name>/archive
func (h *ArchiveHandler) HandleGetCheckpointArchive(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// Extract checkpoint name from URL path: /api/v1/checkpoints/<name>/archive
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/checkpoints/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		h.writeErrorResponse(w, http.StatusBadRequest, "Invalid URL path. Expected: /api/v1/checkpoints/<name>/archive", "")
		return
	}

	checkpointName := parts[0]

	// Get checkpoint
	var checkpoint storagev1alpha1.ManifestCheckpoint
	if err := h.client.Get(r.Context(), types.NamespacedName{Name: checkpointName}, &checkpoint); err != nil {
		if errors.IsNotFound(err) {
			h.writeErrorResponse(w, http.StatusNotFound, "Checkpoint not found", "")
			return
		}
		h.logger.Error(err, "Failed to get ManifestCheckpoint", "name", checkpointName)
		h.writeErrorResponse(w, http.StatusInternalServerError, "Failed to get checkpoint", err.Error())
		return
	}

	// Check if checkpoint is ready (using Ready condition)
	readyCondition := meta.FindStatusCondition(checkpoint.Status.Conditions, storagev1alpha1.ManifestCheckpointConditionTypeReady)
	if readyCondition == nil || readyCondition.Status != metav1.ConditionTrue {
		phaseMsg := "Unknown"
		if readyCondition != nil {
			phaseMsg = fmt.Sprintf("Ready condition: %s (Reason: %s)", readyCondition.Status, readyCondition.Reason)
		}
		h.writeErrorResponse(w, http.StatusConflict, "Checkpoint is not ready yet", phaseMsg)
		return
	}

	// Get the archive using already-loaded checkpoint to avoid duplicate Get/Ready checks
	req := &usecase.ArchiveRequest{
		CheckpointName:  checkpointName,
		CheckpointUID:   string(checkpoint.UID),
		SourceNamespace: checkpoint.Spec.SourceNamespace,
	}

	archiveData, checksum, err := h.archiveService.GetArchiveFromCheckpoint(r.Context(), &checkpoint, req)
	if err != nil {
		h.logger.Error(err, "Failed to get archive", "checkpoint", checkpointName)
		h.writeErrorResponse(w, http.StatusInternalServerError, "Failed to create archive", err.Error())
		return
	}

	// Set appropriate headers
	setArchiveHeaders(w, checkpointName, len(archiveData), checksum)

	// Write the archive data
	if _, err := w.Write(archiveData); err != nil {
		h.logger.Error(err, "Failed to write archive data")
		return
	}

	duration := time.Since(start)
	h.logger.Info("Successfully served checkpoint archive",
		"checkpoint", checkpointName,
		"size", len(archiveData),
		"checksum", checksum,
		"duration", duration)
}

// HandleGetCheckpointInfo handles GET /api/v1/checkpoints/<name>/info
func (h *ArchiveHandler) HandleGetCheckpointInfo(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// Extract checkpoint name from URL path
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/checkpoints/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		h.writeErrorResponse(w, http.StatusBadRequest, "Invalid URL path. Expected: /api/v1/checkpoints/<name>/info", "")
		return
	}

	checkpointName := parts[0]

	// Get checkpoint
	var checkpoint storagev1alpha1.ManifestCheckpoint
	if err := h.client.Get(r.Context(), types.NamespacedName{Name: checkpointName}, &checkpoint); err != nil {
		if errors.IsNotFound(err) {
			h.writeErrorResponse(w, http.StatusNotFound, "Checkpoint not found", "")
			return
		}
		h.logger.Error(err, "Failed to get ManifestCheckpoint", "name", checkpointName)
		h.writeErrorResponse(w, http.StatusInternalServerError, "Failed to get checkpoint", err.Error())
		return
	}

	// Get archive info
	req := &usecase.ArchiveRequest{
		CheckpointName:  checkpointName,
		CheckpointUID:   string(checkpoint.UID),
		SourceNamespace: checkpoint.Spec.SourceNamespace,
	}

	info, err := h.archiveService.GetArchiveInfo(r.Context(), req)
	if err != nil {
		h.logger.Error(err, "Failed to get archive info", "checkpoint", checkpointName)
		h.writeErrorResponse(w, http.StatusInternalServerError, "Failed to get archive info", err.Error())
		return
	}

	// Check Ready condition
	readyCondition := meta.FindStatusCondition(checkpoint.Status.Conditions, storagev1alpha1.ManifestCheckpointConditionTypeReady)
	isReady := readyCondition != nil && readyCondition.Status == metav1.ConditionTrue

	// Get source capture request name from ref
	sourceCaptureRequest := ""
	if checkpoint.Spec.ManifestCaptureRequestRef != nil {
		sourceCaptureRequest = checkpoint.Spec.ManifestCaptureRequestRef.Name
	}

	// Build response with checkpoint metadata
	response := &CheckpointInfoResponse{
		CheckpointName:       checkpointName,
		SourceNamespace:      checkpoint.Spec.SourceNamespace,
		SourceCaptureRequest: sourceCaptureRequest,
		TotalObjects:         checkpoint.Status.TotalObjects,
		TotalSizeBytes:       checkpoint.Status.TotalSizeBytes,
		ChunksCount:          len(checkpoint.Status.Chunks),
		Format:               "json",
		Ready:                isReady,
		CreatedAt:            checkpoint.CreationTimestamp.Format(time.RFC3339),
		Labels:               checkpoint.Labels,
		Annotations:          checkpoint.Annotations,
		FileCount:            info.FileCount,
		EstimatedSize:        info.EstimatedSize,
	}

	// Try to get checksum from cache if available
	cacheKey := h.archiveService.GetCacheKey(req.CheckpointName, req.CheckpointUID)
	if cached := h.archiveService.GetCacheItem(cacheKey); cached != nil {
		response.Checksum = cached.Checksum
		response.Cached = true
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		h.logger.Error(err, "Failed to encode checkpoint info response")
		h.writeErrorResponse(w, http.StatusInternalServerError, "Failed to encode response", "")
		return
	}

	duration := time.Since(start)
	h.logger.Info("Served checkpoint info",
		"checkpoint", checkpointName,
		"duration", duration)
}

// CheckpointInfoResponse represents checkpoint information
type CheckpointInfoResponse struct {
	CheckpointName       string            `json:"checkpointName"`
	SourceNamespace      string            `json:"sourceNamespace"`
	SourceCaptureRequest string            `json:"sourceCaptureRequest"`
	TotalObjects         int               `json:"totalObjects"`
	TotalSizeBytes       int64             `json:"totalSizeBytes"`
	ChunksCount          int               `json:"chunksCount"`
	Format               string            `json:"format"`
	Ready                bool              `json:"ready"`
	CreatedAt            string            `json:"createdAt"`
	Labels               map[string]string `json:"labels,omitempty"`
	Annotations          map[string]string `json:"annotations,omitempty"`
	FileCount            int               `json:"fileCount"`
	EstimatedSize        int64             `json:"estimatedSize"`
	Checksum             string            `json:"checksum,omitempty"`
	Cached               bool              `json:"cached,omitempty"`
}

// ============================================================================
// Kubernetes APIService Endpoints
// ============================================================================

// HandleAPIGroupDiscovery handles GET /apis/subresources.state-snapshotter.deckhouse.io
// Returns APIGroup information for Kubernetes API discovery
// Handles paths with or without trailing slash and query parameters
func (h *ArchiveHandler) HandleAPIGroupDiscovery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		h.writeKubernetesErrorResponse(w, http.StatusMethodNotAllowed, "MethodNotAllowed", fmt.Sprintf("Method %s is not allowed", r.Method))
		return
	}

	// Remove trailing slash and check if this is the group discovery endpoint
	// Kubernetes may call with /apis/.../ or /apis/...?timeout=32s
	trimmed := strings.TrimSuffix(r.URL.Path, "/")
	if trimmed != "/apis/subresources.state-snapshotter.deckhouse.io" {
		// This should not happen if routing is correct, but handle gracefully
		h.writeKubernetesErrorResponse(w, http.StatusNotFound, "NotFound", "Resource not found")
		return
	}

	apiGroup := metav1.APIGroup{
		TypeMeta: metav1.TypeMeta{
			Kind:       "APIGroup",
			APIVersion: "v1",
		},
		Name: "subresources.state-snapshotter.deckhouse.io",
		Versions: []metav1.GroupVersionForDiscovery{
			{
				GroupVersion: "subresources.state-snapshotter.deckhouse.io/v1alpha1",
				Version:      "v1alpha1",
			},
		},
		PreferredVersion: metav1.GroupVersionForDiscovery{
			GroupVersion: "subresources.state-snapshotter.deckhouse.io/v1alpha1",
			Version:      "v1alpha1",
		},
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(apiGroup); err != nil {
		h.logger.Error(err, "Failed to encode APIGroup response")
		h.writeKubernetesErrorResponse(w, http.StatusInternalServerError, "InternalError", "Failed to encode response")
		return
	}
}

// HandleAPIResourceListDiscovery handles GET /apis/subresources.state-snapshotter.deckhouse.io/v1alpha1
// Returns APIResourceList for Kubernetes API version discovery
// Handles paths with or without trailing slash and query parameters
func (h *ArchiveHandler) HandleAPIResourceListDiscovery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		h.writeKubernetesErrorResponse(w, http.StatusMethodNotAllowed, "MethodNotAllowed", fmt.Sprintf("Method %s is not allowed", r.Method))
		return
	}

	// Remove trailing slash and check if this is the version discovery endpoint
	// Kubernetes may call with /apis/.../v1alpha1/ or /apis/.../v1alpha1?timeout=32s
	trimmed := strings.TrimSuffix(r.URL.Path, "/")
	if trimmed != "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1" {
		// This should not happen if routing is correct, but handle gracefully
		h.writeKubernetesErrorResponse(w, http.StatusNotFound, "NotFound", "Resource not found")
		return
	}

	apiResourceList := metav1.APIResourceList{
		TypeMeta: metav1.TypeMeta{
			Kind:       "APIResourceList",
			APIVersion: "v1",
		},
		GroupVersion: "subresources.state-snapshotter.deckhouse.io/v1alpha1",
		APIResources: []metav1.APIResource{
			// Primary resource: required by Kubernetes API aggregation layer
			// Subresource: the actual /manifests endpoint
			{
				Name:       "manifestcheckpoints/manifests",
				Namespaced: false, // ManifestCheckpoint is cluster-scoped
				Kind:       "ManifestCheckpoint",
				Verbs:      []string{"get"},
			},
			{
				Name:       "snapshots/manifests",
				Namespaced: true,
				Kind:       "Snapshot",
				Verbs:      []string{"get"},
			},
			{
				Name:       "snapshots/manifests-with-data-restoration",
				Namespaced: true,
				Kind:       "Snapshot",
				Verbs:      []string{"get"},
			},
			{
				Name:       "namespacesnapshots/manifests",
				Namespaced: true,
				Kind:       "NamespaceSnapshot",
				Verbs:      []string{"get"},
			},
		},
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(apiResourceList); err != nil {
		h.logger.Error(err, "Failed to encode APIResourceList response")
		h.writeKubernetesErrorResponse(w, http.StatusInternalServerError, "InternalError", "Failed to encode response")
		return
	}
}

// HandleGetManifests handles GET /apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/manifestcheckpoints/<name>/manifests
// This is the Kubernetes APIService endpoint for the /manifests subresource
func (h *ArchiveHandler) HandleGetManifests(w http.ResponseWriter, r *http.Request, checkpointName string) {
	start := time.Now()

	// Get checkpoint
	var checkpoint storagev1alpha1.ManifestCheckpoint
	if err := h.client.Get(r.Context(), types.NamespacedName{Name: checkpointName}, &checkpoint); err != nil {
		if errors.IsNotFound(err) {
			status := metav1.Status{
				TypeMeta: metav1.TypeMeta{
					Kind:       "Status",
					APIVersion: "v1",
				},
				Status:  metav1.StatusFailure,
				Message: fmt.Sprintf("checkpoint %s not found", checkpointName),
				Reason:  metav1.StatusReasonNotFound,
				Code:    http.StatusNotFound,
				Details: &metav1.StatusDetails{
					Name: checkpointName,
					Kind: "ManifestCheckpoint",
				},
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(status)
			return
		}
		h.logger.Error(err, "Failed to get checkpoint", "checkpoint", checkpointName)
		h.writeKubernetesErrorResponse(w, http.StatusInternalServerError, "InternalError", fmt.Sprintf("failed to get checkpoint: %v", err))
		return
	}

	// Check if checkpoint is Ready
	readyCondition := meta.FindStatusCondition(checkpoint.Status.Conditions, storagev1alpha1.ManifestCheckpointConditionTypeReady)
	if readyCondition == nil || readyCondition.Status != metav1.ConditionTrue {
		reason := "Unknown"
		if readyCondition != nil {
			reason = readyCondition.Reason
		}
		h.logger.Warning("Checkpoint is not ready",
			"checkpoint", checkpointName,
			"reason", reason)
		h.writeKubernetesErrorResponse(w, http.StatusConflict, "Conflict", fmt.Sprintf("checkpoint not ready: reason=%s", reason))
		return
	}

	// Use archive service to get JSON archive using already-loaded checkpoint
	req := &usecase.ArchiveRequest{
		CheckpointName:  checkpointName,
		CheckpointUID:   string(checkpoint.UID),
		SourceNamespace: checkpoint.Spec.SourceNamespace,
	}

	archiveData, _, err := h.archiveService.GetArchiveFromCheckpoint(r.Context(), &checkpoint, req)
	if err != nil {
		h.logger.Error(err, "Failed to get archive", "checkpoint", checkpointName)
		h.writeKubernetesErrorResponse(w, http.StatusInternalServerError, "InternalError", fmt.Sprintf("failed to get archive: %v", err))
		return
	}

	// Return JSON directly without double serialization
	// archiveData is already a valid JSON array from createJSONArchive
	w.Header().Set("Content-Type", "application/json")

	// Check if client accepts gzip encoding
	if shouldCompressResponse(r) {
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		defer gz.Close()
		if _, err := gz.Write(archiveData); err != nil {
			h.logger.Error(err, "Failed to write gzipped response", "checkpoint", checkpointName)
			return
		}
	} else {
		if _, err := w.Write(archiveData); err != nil {
			h.logger.Error(err, "Failed to write response", "checkpoint", checkpointName)
			return
		}
	}

	duration := time.Since(start)
	h.logger.Info("Successfully returned manifests",
		"checkpoint", checkpointName,
		"size", len(archiveData),
		"duration", duration)
}

// writeKubernetesErrorResponse writes an error response in Kubernetes API format
// Uses metav1.Status for proper Kubernetes API compatibility
func (h *ArchiveHandler) writeKubernetesErrorResponse(w http.ResponseWriter, code int, reason, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)

	status := metav1.Status{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Status",
			APIVersion: "v1",
		},
		Status:  metav1.StatusFailure,
		Message: message,
		Reason:  metav1.StatusReason(reason),
		Code:    int32(code),
	}

	_ = json.NewEncoder(w).Encode(status)
}

// shouldCompressResponse checks if the client accepts gzip encoding
func shouldCompressResponse(r *http.Request) bool {
	acceptEncoding := r.Header.Get("Accept-Encoding")
	return strings.Contains(acceptEncoding, "gzip")
}

// ============================================================================
// List Endpoints
// ============================================================================

// HandleListCheckpoints handles GET /api/v1/checkpoints
func (h *ArchiveHandler) HandleListCheckpoints(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	sourceNamespace := r.URL.Query().Get("namespace")

	var checkpointList storagev1alpha1.ManifestCheckpointList
	if err := h.client.List(r.Context(), &checkpointList); err != nil {
		h.logger.Error(err, "Failed to list ManifestCheckpoints")
		h.writeErrorResponse(w, http.StatusInternalServerError, "Failed to list checkpoints", "")
		return
	}

	// Convert to response format
	checkpoints := make([]CheckpointListItem, 0)
	for _, checkpoint := range checkpointList.Items {
		// Filter by source namespace if specified
		if sourceNamespace != "" && checkpoint.Spec.SourceNamespace != sourceNamespace {
			continue
		}
		// Get source capture request name from ref
		sourceCaptureRequest := ""
		if checkpoint.Spec.ManifestCaptureRequestRef != nil {
			sourceCaptureRequest = checkpoint.Spec.ManifestCaptureRequestRef.Name
		}

		// Check Ready condition
		readyCondition := meta.FindStatusCondition(checkpoint.Status.Conditions, storagev1alpha1.ManifestCheckpointConditionTypeReady)
		isReady := readyCondition != nil && readyCondition.Status == metav1.ConditionTrue

		checkpoints = append(checkpoints, CheckpointListItem{
			Name:                 checkpoint.Name,
			SourceNamespace:      checkpoint.Spec.SourceNamespace,
			SourceCaptureRequest: sourceCaptureRequest,
			TotalObjects:         checkpoint.Status.TotalObjects,
			TotalSizeBytes:       checkpoint.Status.TotalSizeBytes,
			ChunksCount:          len(checkpoint.Status.Chunks),
			Ready:                isReady,
			CreatedAt:            checkpoint.CreationTimestamp.Format(time.RFC3339),
		})
	}

	response := &ListCheckpointsResponse{
		Items: checkpoints,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		h.logger.Error(err, "Failed to encode checkpoints list response")
		h.writeErrorResponse(w, http.StatusInternalServerError, "Failed to encode response", "")
		return
	}

	duration := time.Since(start)
	h.logger.Info("Served checkpoints list",
		"count", len(checkpoints),
		"namespace", sourceNamespace,
		"duration", duration)
}

// CheckpointListItem represents a checkpoint in the list
type CheckpointListItem struct {
	Name                 string `json:"name"`
	SourceNamespace      string `json:"sourceNamespace"`
	SourceCaptureRequest string `json:"sourceCaptureRequest"`
	TotalObjects         int    `json:"totalObjects"`
	TotalSizeBytes       int64  `json:"totalSizeBytes"`
	ChunksCount          int    `json:"chunksCount"`
	Ready                bool   `json:"ready"`
	CreatedAt            string `json:"createdAt"`
}

// ListCheckpointsResponse represents the response for listing checkpoints
type ListCheckpointsResponse struct {
	Items []CheckpointListItem `json:"items"`
}

// ============================================================================
// Health Check Endpoints
// ============================================================================

// HandleHealth handles GET /healthz (Kubernetes standard endpoint)
func (h *ArchiveHandler) HandleHealth(w http.ResponseWriter, _ *http.Request) {
	response := map[string]string{
		"status": "ok",
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

// HandleLive handles GET /livez (Kubernetes standard endpoint)
func (h *ArchiveHandler) HandleLive(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte("ok")); err != nil {
		return
	}
}

// ============================================================================
// Helper Functions
// ============================================================================

// writeErrorResponse writes an error response in JSON format
func (h *ArchiveHandler) writeErrorResponse(w http.ResponseWriter, statusCode int, message, details string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)

	response := ErrorResponse{
		Error:   message,
		Details: details,
	}

	_ = json.NewEncoder(w).Encode(response)
}

// setArchiveHeaders sets appropriate HTTP headers for archive download
func setArchiveHeaders(w http.ResponseWriter, name string, size int, checksum string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s.json\"", name))
	w.Header().Set("Content-Length", strconv.Itoa(size))
	w.Header().Set("X-Archive-Checksum", checksum)
	w.Header().Set("Cache-Control", "max-age=300") // 5 minutes cache
	w.Header().Set("Access-Control-Allow-Origin", "*")
}

// GetArchiveService returns the archive service for external access
func (h *ArchiveHandler) GetArchiveService() *usecase.ArchiveService {
	return h.archiveService
}

// SetupRoutes sets up HTTP routes for the archive API
func (h *ArchiveHandler) SetupRoutes(mux *http.ServeMux) {
	// Checkpoint endpoints
	// Use pattern matching for better route handling
	mux.HandleFunc("/api/v1/checkpoints/", func(w http.ResponseWriter, r *http.Request) {
		// Extract path after /api/v1/checkpoints/
		path := strings.TrimPrefix(r.URL.Path, "/api/v1/checkpoints/")
		if path == "" {
			// List checkpoints
			h.HandleListCheckpoints(w, r)
			return
		}

		// Split path to get checkpoint name and action
		parts := strings.SplitN(path, "/", 2)
		if len(parts) == 2 {
			action := parts[1]
			switch action {
			case "archive":
				h.HandleGetCheckpointArchive(w, r)
			case "info":
				h.HandleGetCheckpointInfo(w, r)
			default:
				h.writeErrorResponse(w, http.StatusNotFound, "Not found", fmt.Sprintf("Unknown action: %s", action))
			}
		} else {
			// Just checkpoint name without action - redirect to info
			h.HandleGetCheckpointInfo(w, r)
		}
	})

	// List checkpoints (exact match)
	mux.HandleFunc("/api/v1/checkpoints", h.HandleListCheckpoints)

	// Kubernetes APIService endpoints
	// Path: /apis/subresources.state-snapshotter.deckhouse.io
	// Handle API group discovery endpoint
	// Kubernetes API server calls this to discover the API group
	// Register both with and without trailing slash to handle all cases
	mux.HandleFunc("/apis/subresources.state-snapshotter.deckhouse.io", h.HandleAPIGroupDiscovery)
	mux.HandleFunc("/apis/subresources.state-snapshotter.deckhouse.io/", h.HandleAPIGroupDiscovery)

	// Path: /apis/subresources.state-snapshotter.deckhouse.io/v1alpha1
	// Handle API version resource list discovery endpoint
	// Kubernetes API server calls this to discover available resources in this version
	// Register both with and without trailing slash to handle all cases (with query params too)
	mux.HandleFunc("/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1", h.HandleAPIResourceListDiscovery)
	mux.HandleFunc("/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/", h.HandleAPIResourceListDiscovery)

	// Path: /apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/manifestcheckpoints/<name>
	mux.HandleFunc("/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/manifestcheckpoints/", func(w http.ResponseWriter, r *http.Request) {
		// Extract path after /apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/manifestcheckpoints/
		path := strings.TrimPrefix(r.URL.Path, "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/manifestcheckpoints/")
		path = strings.TrimSuffix(path, "/")

		if path == "" {
			// Should not happen due to routing, but handle gracefully
			h.writeKubernetesErrorResponse(w, http.StatusNotFound, "NotFound", "subresource required: use /apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/manifestcheckpoints/<name>/manifests")
			return
		}

		// Split path: <name>[/<subresource>]
		parts := strings.SplitN(path, "/", 2)
		checkpointName := parts[0]
		if len(parts) == 1 {
			h.writeKubernetesErrorResponse(w, http.StatusNotFound, "NotFound", "subresource required: use /apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/manifestcheckpoints/<name>/manifests")
			return
		}

		subresource := parts[1]
		switch subresource {
		case "manifests":
			if r.Method != http.MethodGet {
				h.writeKubernetesErrorResponse(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "only GET method is supported")
				return
			}
			h.HandleGetManifests(w, r, checkpointName)
		default:
			h.writeKubernetesErrorResponse(w, http.StatusNotFound, "NotFound", fmt.Sprintf("unknown subresource: %s", subresource))
		}
	})

	// Health check endpoints (Kubernetes standard)
	mux.HandleFunc("/healthz", h.HandleHealth)
	mux.HandleFunc("/readyz", h.HandleHealth)
	mux.HandleFunc("/livez", h.HandleLive)

	// Log health check endpoint registration
	h.logger.Info("Health check endpoints registered", "healthz", "/healthz", "readyz", "/readyz", "livez", "/livez")
}
