package api

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase/restore"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

type RestoreHandler struct {
	client       client.Client
	service      *restore.Service
	logger       logger.LoggerInterface
	nsAggregated *usecase.AggregatedNamespaceManifests
}

func NewRestoreHandler(client client.Client, service *restore.Service, logger logger.LoggerInterface, nsAggregated *usecase.AggregatedNamespaceManifests) *RestoreHandler {
	return &RestoreHandler{
		client:       client,
		service:      service,
		logger:       logger,
		nsAggregated: nsAggregated,
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
			snapshotName := parts[2]
			subresource := parts[3]
			if r.Method != http.MethodGet {
				h.writeKubernetesErrorResponse(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "only GET method is supported")
				return
			}
			switch subresource {
			case "manifests":
				h.HandleGetSnapshotManifests(w, r, namespace, snapshotName)
			case "manifests-with-data-restoration":
				h.HandleGetSnapshotManifestsWithDataRestoration(w, r, namespace, snapshotName)
			default:
				h.writeKubernetesErrorResponse(w, http.StatusNotFound, "NotFound", "unknown subresource")
			}
		case "namespacesnapshots":
			if len(parts) != 4 {
				h.writeKubernetesErrorResponse(w, http.StatusNotFound, "NotFound", "resource not found")
				return
			}
			if r.Method != http.MethodGet {
				h.writeKubernetesErrorResponse(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "only GET method is supported")
				return
			}
			snapName := parts[2]
			sub := parts[3]
			if sub != "manifests" {
				h.writeKubernetesErrorResponse(w, http.StatusNotFound, "NotFound", "unknown subresource")
				return
			}
			h.HandleNamespaceSnapshotAggregatedManifests(w, r, namespace, snapName)
		default:
			h.writeKubernetesErrorResponse(w, http.StatusNotFound, "NotFound", "resource not found")
		}
	})
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

func (h *RestoreHandler) HandleGetSnapshotManifests(w http.ResponseWriter, r *http.Request, namespace, snapshotName string) {
	start := time.Now()
	opts := restore.Options{
		SnapshotName:      snapshotName,
		SnapshotNamespace: namespace,
		TargetNamespace:   r.URL.Query().Get("targetNamespace"),
		RestoreStrategy:   r.URL.Query().Get("restoreStrategy"),
	}

	data, err := h.service.BuildManifests(r.Context(), opts)
	if err != nil {
		h.writeRestoreError(w, err)
		return
	}

	h.writeJSONResponse(w, r, data)
	h.logger.Info("Returned snapshot manifests", "snapshot", snapshotName, "namespace", namespace, "duration", time.Since(start))
}

func (h *RestoreHandler) HandleGetSnapshotManifestsWithDataRestoration(w http.ResponseWriter, r *http.Request, namespace, snapshotName string) {
	start := time.Now()
	opts := restore.Options{
		SnapshotName:      snapshotName,
		SnapshotNamespace: namespace,
		TargetNamespace:   r.URL.Query().Get("targetNamespace"),
		RestoreStrategy:   r.URL.Query().Get("restoreStrategy"),
	}

	data, err := h.service.BuildManifestsWithDataRestoration(r.Context(), opts)
	if err != nil {
		h.writeRestoreError(w, err)
		return
	}

	h.writeJSONResponse(w, r, data)
	h.logger.Info("Returned manifests-with-data-restoration", "snapshot", snapshotName, "namespace", namespace, "duration", time.Since(start))
}

func (h *RestoreHandler) HandleNamespaceSnapshotAggregatedManifests(w http.ResponseWriter, r *http.Request, namespace, snapshotName string) {
	start := time.Now()
	if h.nsAggregated == nil {
		h.writeKubernetesErrorResponse(w, http.StatusInternalServerError, "InternalError", "aggregated manifests handler not configured")
		return
	}
	data, err := h.nsAggregated.BuildAggregatedJSON(r.Context(), namespace, snapshotName)
	if err != nil {
		h.writeAggregatedError(w, err)
		return
	}
	h.writeJSONResponse(w, r, data)
	h.logger.Info("Returned NamespaceSnapshot aggregated manifests", "namespaceSnapshot", snapshotName, "namespace", namespace, "duration", time.Since(start))
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
