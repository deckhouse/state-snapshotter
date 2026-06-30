package api

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase/restore"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

// maxManifestsUploadBytes bounds the per-CR import upload request body. A node's own manifests (not the
// whole subtree) are modest; this cap protects the apiserver from oversized payloads while staying well
// above realistic per-node sizes.
const maxManifestsUploadBytes = 64 << 20 // 64 MiB

// storageSnapshotGVK returns the GVK of the core Snapshot kind (state-snapshotter.deckhouse.io/v1alpha1).
func storageSnapshotGVK() schema.GroupVersionKind {
	return storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot")
}

type RestoreHandler struct {
	client       client.Client
	service      *restore.Service
	logger       logger.LoggerInterface
	nsAggregated *usecase.AggregatedNamespaceManifests
	importUpload *usecase.ImportUploadService
	restMapper   meta.RESTMapper
}

func NewRestoreHandler(client client.Client, service *restore.Service, logger logger.LoggerInterface, nsAggregated *usecase.AggregatedNamespaceManifests, importUpload *usecase.ImportUploadService, restMappers ...meta.RESTMapper) *RestoreHandler {
	var restMapper meta.RESTMapper
	if len(restMappers) > 0 {
		restMapper = restMappers[0]
	}
	return &RestoreHandler{
		client:       client,
		service:      service,
		logger:       logger,
		nsAggregated: nsAggregated,
		importUpload: importUpload,
		restMapper:   restMapper,
	}
}

func (h *RestoreHandler) SetupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/")
		path = strings.TrimSuffix(path, "/")
		parts := strings.Split(path, "/")
		if len(parts) < 2 {
			h.writeKubernetesErrorResponse(w, http.StatusNotFound, "NotFound", "resource not found")
			return
		}
		namespace := parts[0]
		switch parts[1] {
		case "snapshots":
			if len(parts) == 2 {
				if r.Method != http.MethodGet {
					h.writeKubernetesErrorResponse(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "only GET method is supported for list")
					return
				}
				h.HandleListSnapshots(w, r)
				return
			}
			if len(parts) < 4 {
				h.writeKubernetesErrorResponse(w, http.StatusNotFound, "NotFound", "subresource required")
				return
			}
			h.routeCoreSnapshotSubresource(w, r, namespace, parts[2], parts[3])
		default:
			if len(parts) != 4 {
				h.writeKubernetesErrorResponse(w, http.StatusNotFound, "NotFound", "resource not found")
				return
			}
			h.routeGenericSnapshotSubresource(w, r, namespace, parts[1], parts[2], parts[3])
		}
	})

	// Cluster-scoped snapshotcontents/<name>/manifests-download (no /namespaces/ segment): the import
	// path's DataImport reads a node's original manifest directly off its SnapshotContent before any
	// namespaced snapshot CR binds. Only manifests-download (GET, single-node) is exposed here.
	mux.HandleFunc("/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/snapshotcontents/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/snapshotcontents/")
		path = strings.TrimSuffix(path, "/")
		parts := strings.Split(path, "/")
		if len(parts) != 2 {
			h.writeKubernetesErrorResponse(w, http.StatusNotFound, "NotFound", "expected snapshotcontents/<name>/manifests-download")
			return
		}
		contentName := parts[0]
		switch parts[1] {
		case "manifests-download":
			if r.Method != http.MethodGet {
				h.writeKubernetesErrorResponse(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "only GET method is supported")
				return
			}
			h.HandleContentManifestsDownload(w, r, contentName)
		default:
			h.writeKubernetesErrorResponse(w, http.StatusNotFound, "NotFound", "unknown subresource")
		}
	})
}

// routeCoreSnapshotSubresource dispatches subresources of the core Snapshot kind by method.
func (h *RestoreHandler) routeCoreSnapshotSubresource(w http.ResponseWriter, r *http.Request, namespace, snapshotName, subresource string) {
	switch subresource {
	case "manifests-with-data-restoration":
		if !h.requireMethod(w, r, http.MethodGet) {
			return
		}
		h.HandleGetSnapshotManifestsWithDataRestoration(w, r, namespace, snapshotName)
	case "manifests-download":
		if !h.requireMethod(w, r, http.MethodGet) {
			return
		}
		h.HandleCoreSnapshotManifestsDownload(w, r, namespace, snapshotName)
	case "manifests-and-children-refs-upload":
		if !h.requireMethod(w, r, http.MethodPost) {
			return
		}
		h.HandleSnapshotManifestsAndChildrenUpload(w, r, storageSnapshotGVK(), namespace, snapshotName)
	default:
		h.writeKubernetesErrorResponse(w, http.StatusNotFound, "NotFound", "unknown subresource")
	}
}

// routeGenericSnapshotSubresource dispatches subresources of any registered (non-core) snapshot kind.
func (h *RestoreHandler) routeGenericSnapshotSubresource(w http.ResponseWriter, r *http.Request, namespace, resource, name, subresource string) {
	switch subresource {
	case "manifests-with-data-restoration":
		if !h.requireMethod(w, r, http.MethodGet) {
			return
		}
		h.HandleGenericSnapshotManifestsWithDataRestoration(w, r, namespace, resource, name)
	case "manifests-download":
		if !h.requireMethod(w, r, http.MethodGet) {
			return
		}
		h.HandleGenericSnapshotManifestsDownload(w, r, namespace, resource, name)
	case "manifests-and-children-refs-upload":
		if !h.requireMethod(w, r, http.MethodPost) {
			return
		}
		h.HandleGenericSnapshotManifestsAndChildrenUpload(w, r, namespace, resource, name)
	default:
		h.writeKubernetesErrorResponse(w, http.StatusNotFound, "NotFound", "unknown subresource")
	}
}

// requireMethod enforces the HTTP method, writing a 405 and returning false on mismatch.
func (h *RestoreHandler) requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method != method {
		h.writeKubernetesErrorResponse(w, http.StatusMethodNotAllowed, "MethodNotAllowed", fmt.Sprintf("only %s method is supported", method))
		return false
	}
	return true
}

func (h *RestoreHandler) HandleListSnapshots(w http.ResponseWriter, r *http.Request) {
	selfLink := r.URL.Path
	if r.URL.RawQuery != "" {
		selfLink += "?" + r.URL.RawQuery
	}
	listResponse := map[string]interface{}{
		"kind":       "SnapshotList",
		"apiVersion": "subresources.state-snapshotter.deckhouse.io/v1alpha1",
		"items":      []interface{}{},
		"metadata": map[string]interface{}{
			"resourceVersion": "0",
			"selfLink":        selfLink,
		},
	}
	_ = json.NewEncoder(w).Encode(listResponse)
}

func (h *RestoreHandler) HandleGetSnapshotManifestsWithDataRestoration(w http.ResponseWriter, r *http.Request, namespace, snapshotName string) {
	start := time.Now()
	opts := restore.Options{
		SnapshotName:      snapshotName,
		SnapshotNamespace: namespace,
		TargetNamespace:   r.URL.Query().Get("targetNamespace"),
	}

	data, err := h.service.BuildManifestsWithDataRestoration(r.Context(), opts)
	if err != nil {
		h.writeRestoreError(w, err)
		return
	}

	h.writeJSONResponse(w, r, data)
	h.logger.Info("Returned manifests-with-data-restoration", "snapshot", snapshotName, "namespace", namespace, "duration", time.Since(start))
}

func (h *RestoreHandler) HandleGenericSnapshotManifestsWithDataRestoration(w http.ResponseWriter, r *http.Request, namespace, resource, snapshotName string) {
	start := time.Now()
	snapshotGVK, err := h.resolveNamespacedSnapshotGVK(r.Context(), resource)
	if err != nil {
		h.writeAggregatedError(w, err)
		return
	}
	opts := restore.Options{
		SnapshotName:      snapshotName,
		SnapshotNamespace: namespace,
		TargetNamespace:   r.URL.Query().Get("targetNamespace"),
	}
	data, err := h.service.BuildManifestsWithDataRestorationForNode(r.Context(), snapshotGVK, opts)
	if err != nil {
		h.writeRestoreError(w, err)
		return
	}
	h.writeJSONResponse(w, r, data)
	h.logger.Info("Returned per-node manifests-with-data-restoration", "resource", resource, "snapshot", snapshotName, "namespace", namespace, "gvk", snapshotGVK.String(), "duration", time.Since(start))
}

// HandleCoreSnapshotManifestsDownload returns the own-node (single-node) manifests of a core Snapshot
// root — raw verbatim from MCP (status, managedFields, namespace preserved), WITHOUT walking the subtree.
func (h *RestoreHandler) HandleCoreSnapshotManifestsDownload(w http.ResponseWriter, r *http.Request, namespace, snapshotName string) {
	start := time.Now()
	if h.nsAggregated == nil {
		h.writeKubernetesErrorResponse(w, http.StatusInternalServerError, "InternalError", "manifests handler not configured")
		return
	}
	data, err := h.nsAggregated.BuildSingleNodeJSONForRootSnapshot(r.Context(), namespace, snapshotName)
	if err != nil {
		h.writeAggregatedError(w, err)
		return
	}
	h.writeJSONResponse(w, r, data)
	h.logger.Info("Returned per-CR manifests-download", "snapshot", snapshotName, "namespace", namespace, "duration", time.Since(start))
}

// HandleGenericSnapshotManifestsDownload returns the own-node manifests of any registered (non-core)
// namespaced snapshot kind (single-node, raw verbatim from MCP), resolving the resource to its GVK.
func (h *RestoreHandler) HandleGenericSnapshotManifestsDownload(w http.ResponseWriter, r *http.Request, namespace, resource, snapshotName string) {
	start := time.Now()
	if h.nsAggregated == nil {
		h.writeKubernetesErrorResponse(w, http.StatusInternalServerError, "InternalError", "manifests handler not configured")
		return
	}
	snapshotGVK, err := h.resolveNamespacedSnapshotGVK(r.Context(), resource)
	if err != nil {
		h.writeAggregatedError(w, err)
		return
	}
	data, err := h.nsAggregated.BuildSingleNodeJSONFromSnapshot(r.Context(), snapshotGVK, namespace, snapshotName)
	if err != nil {
		h.writeAggregatedError(w, err)
		return
	}
	h.writeJSONResponse(w, r, data)
	h.logger.Info("Returned per-CR generic manifests-download", "resource", resource, "snapshot", snapshotName, "namespace", namespace, "gvk", snapshotGVK.String(), "duration", time.Since(start))
}

// HandleContentManifestsDownload returns the own-node manifests addressed by cluster-scoped
// SnapshotContent name (single-node, raw verbatim from MCP). Used by DataImport on the import path.
func (h *RestoreHandler) HandleContentManifestsDownload(w http.ResponseWriter, r *http.Request, contentName string) {
	start := time.Now()
	if h.nsAggregated == nil {
		h.writeKubernetesErrorResponse(w, http.StatusInternalServerError, "InternalError", "manifests handler not configured")
		return
	}
	data, err := h.nsAggregated.BuildSingleNodeJSONFromContent(r.Context(), contentName)
	if err != nil {
		h.writeAggregatedError(w, err)
		return
	}
	h.writeJSONResponse(w, r, data)
	h.logger.Info("Returned per-content manifests-download", "content", contentName, "duration", time.Since(start))
}

// HandleSnapshotManifestsAndChildrenUpload persists one core Snapshot node's import payload
// ({manifests, childRefs}): it reconstructs the node's ManifestCheckpoint and records its direct children.
func (h *RestoreHandler) HandleSnapshotManifestsAndChildrenUpload(w http.ResponseWriter, r *http.Request, snapshotGVK schema.GroupVersionKind, namespace, snapshotName string) {
	h.handleManifestsAndChildrenUpload(w, r, snapshotGVK, namespace, snapshotName)
}

// HandleGenericSnapshotManifestsAndChildrenUpload persists a generic (non-core) snapshot node's import
// payload, resolving the resource to its GVK first.
func (h *RestoreHandler) HandleGenericSnapshotManifestsAndChildrenUpload(w http.ResponseWriter, r *http.Request, namespace, resource, snapshotName string) {
	snapshotGVK, err := h.resolveNamespacedSnapshotGVK(r.Context(), resource)
	if err != nil {
		h.writeAggregatedError(w, err)
		return
	}
	h.handleManifestsAndChildrenUpload(w, r, snapshotGVK, namespace, snapshotName)
}

func (h *RestoreHandler) handleManifestsAndChildrenUpload(w http.ResponseWriter, r *http.Request, snapshotGVK schema.GroupVersionKind, namespace, snapshotName string) {
	start := time.Now()
	if h.importUpload == nil {
		h.writeKubernetesErrorResponse(w, http.StatusInternalServerError, "InternalError", "import upload handler not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxManifestsUploadBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.writeKubernetesErrorResponse(w, http.StatusRequestEntityTooLarge, "RequestEntityTooLarge",
				fmt.Sprintf("upload body exceeds %d bytes", maxManifestsUploadBytes))
			return
		}
		h.writeKubernetesErrorResponse(w, http.StatusBadRequest, "BadRequest", fmt.Sprintf("read upload body: %v", err))
		return
	}
	checkpointName, err := h.importUpload.Upload(r.Context(), snapshotGVK, namespace, snapshotName, body)
	if err != nil {
		h.writeAggregatedError(w, err)
		return
	}
	resp := map[string]interface{}{
		"kind":                   "Status",
		"apiVersion":             "v1",
		"status":                 "Success",
		"manifestCheckpointName": checkpointName,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
	h.logger.Info("Accepted manifests-and-children-refs-upload", "resource", snapshotGVK.String(), "snapshot", snapshotName, "namespace", namespace, "checkpoint", checkpointName, "duration", time.Since(start))
}

func (h *RestoreHandler) resolveNamespacedSnapshotGVK(ctx context.Context, resource string) (schema.GroupVersionKind, error) {
	if h.restMapper == nil {
		return schema.GroupVersionKind{}, usecase.NewAggregatedStatusError(http.StatusBadRequest, "BadRequest", "unsupported resource")
	}
	gvks, err := h.restMapper.KindsFor(schema.GroupVersionResource{Resource: resource})
	if err != nil || len(gvks) == 0 {
		return schema.GroupVersionKind{}, usecase.NewAggregatedStatusError(http.StatusBadRequest, "BadRequest", "unsupported resource")
	}
	for _, gvk := range gvks {
		registered, rerr := h.nsAggregated.IsRegisteredSnapshotGVK(ctx, gvk)
		if rerr != nil {
			return schema.GroupVersionKind{}, rerr
		}
		if !registered {
			continue
		}
		mapping, merr := h.restMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if merr != nil {
			continue
		}
		if mapping.Scope.Name() != meta.RESTScopeNameNamespace {
			return schema.GroupVersionKind{}, usecase.NewAggregatedStatusError(http.StatusBadRequest, "BadRequest", "cluster-scoped snapshot resources are not supported")
		}
		return gvk, nil
	}
	return schema.GroupVersionKind{}, usecase.NewAggregatedStatusError(http.StatusBadRequest, "BadRequest", "unsupported resource")
}

func (h *RestoreHandler) writeAggregatedError(w http.ResponseWriter, err error) {
	var st *usecase.AggregatedStatusError
	if errors.As(err, &st) {
		h.writeKubernetesErrorResponse(w, st.HTTPStatus, st.Reason, st.Message)
		return
	}
	h.writeKubernetesErrorResponse(w, http.StatusInternalServerError, "InternalError", err.Error())
}

func (h *RestoreHandler) writeRestoreError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	reason := "InternalError"
	message := err.Error()

	switch {
	case errors.Is(err, restore.ErrBadRequest):
		status = http.StatusBadRequest
		reason = "BadRequest"
	case errors.Is(err, restore.ErrNotFound) || apierrors.IsNotFound(err):
		status = http.StatusNotFound
		reason = "NotFound"
	case errors.Is(err, restore.ErrNotReady) || errors.Is(err, restore.ErrContractViolation):
		status = http.StatusConflict
		reason = "Conflict"
	}

	h.writeKubernetesErrorResponse(w, status, reason, message)
}

func (h *RestoreHandler) writeJSONResponse(w http.ResponseWriter, r *http.Request, data []byte) {
	w.Header().Set("Content-Type", "application/json")
	if shouldCompressResponse(r) {
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		defer gz.Close()
		_, _ = gz.Write(data)
		return
	}
	_, _ = w.Write(data)
}

func (h *RestoreHandler) writeKubernetesErrorResponse(w http.ResponseWriter, code int, reason, message string) {
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
