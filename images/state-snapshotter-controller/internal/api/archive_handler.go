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

package api //nolint:revive // package name matches internal/api directory

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
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
				Name:       "snapshots/manifests-with-data-restoration",
				Namespaced: true,
				Kind:       "Snapshot",
				Verbs:      []string{"get"},
			},
			{
				Name:       "snapshots/manifests-download",
				Namespaced: true,
				Kind:       "Snapshot",
				Verbs:      []string{"get"},
			},
			{
				Name:       "snapshots/manifests-and-children-refs-upload",
				Namespaced: true,
				Kind:       "Snapshot",
				Verbs:      []string{"create"},
			},
			{
				Name:       "snapshotcontents/manifests-download",
				Namespaced: false,
				Kind:       "SnapshotContent",
				Verbs:      []string{"get"},
			},
			{
				// Internal, read-only exclude-computation endpoint: returns object IDENTITIES for the
				// whole subtree (own node + descendants), fail-closed. Granted to domain controllers
				// (which have no MCP/generic content RBAC) as one narrow verb (see wave5 §6.3).
				Name:       "snapshotcontents/subtree-manifest-identities",
				Namespaced: false,
				Kind:       "SnapshotContent",
				Verbs:      []string{"get"},
			},
			{
				// Internal, content-addressed import write (manifests-only). No bind-gate: the content
				// exists by definition. It is the target the domain upload facade forwards manifests to.
				Name:       "snapshotcontents/manifests-upload",
				Namespaced: false,
				Kind:       "SnapshotContent",
				Verbs:      []string{"create"},
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
		CheckpointName: checkpointName,
		CheckpointUID:  string(checkpoint.UID),
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

// GetArchiveService returns the archive service for external access
func (h *ArchiveHandler) GetArchiveService() *usecase.ArchiveService {
	return h.archiveService
}

// SetupRoutes sets up HTTP routes for the archive API
func (h *ArchiveHandler) SetupRoutes(mux *http.ServeMux) {
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
