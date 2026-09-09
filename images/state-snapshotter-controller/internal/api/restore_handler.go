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
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/deckhouse/sds-common-lib/logger"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase/restore"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk/restoreapi"
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
	logger       logger.Logger
	nsAggregated *usecase.AggregatedNamespaceManifests
	importUpload *usecase.ImportUploadService
}

func NewRestoreHandler(client client.Client, service *restore.Service, logger logger.Logger, nsAggregated *usecase.AggregatedNamespaceManifests, importUpload *usecase.ImportUploadService) *RestoreHandler {
	return &RestoreHandler{
		client:       client,
		service:      service,
		logger:       logger,
		nsAggregated: nsAggregated,
		importUpload: importUpload,
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
			h.routeGenericSnapshotSubresource(w, parts[1], parts[3])
		}
	})

	// Cluster-scoped snapshotcontents/<name>/<subresource> (no /namespaces/ segment):
	//   - manifests-download (GET, single-node): the import path's DataImport reads a node's original
	//     manifest directly off its SnapshotContent before any namespaced snapshot CR binds.
	//   - subtree-manifest-identities (GET, recursive): the exclude-computation endpoint an aggregator's
	//    SDK calls on each child content to obtain the subtree identity set (fail-closed).
	//   - manifests-upload (POST, manifests-only, internal): the content-addressed import write. It has NO
	//     bind-gate — the content exists by definition (cluster-scoped addressing), a missing content is a
	//     plain 404, never ImportContentNotBound. It is the target the domain upload facade forwards
	//     manifests to (childRefs stay on the namespaced layer).
	mux.HandleFunc("/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/snapshotcontents/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/snapshotcontents/")
		path = strings.TrimSuffix(path, "/")
		parts := strings.Split(path, "/")
		if len(parts) != 2 {
			h.writeKubernetesErrorResponse(w, http.StatusNotFound, "NotFound", "expected snapshotcontents/<name>/<subresource>")
			return
		}
		contentName := parts[0]
		switch parts[1] {
		case "manifests-download":
			if !h.requireMethod(w, r, http.MethodGet) {
				return
			}
			h.HandleContentManifestsDownload(w, r, contentName)
		case "subtree-manifest-identities":
			if !h.requireMethod(w, r, http.MethodGet) {
				return
			}
			h.HandleContentSubtreeManifestIdentities(w, r, contentName)
		case "manifests-upload":
			if !h.requireMethod(w, r, http.MethodPost) {
				return
			}
			h.HandleContentManifestsUpload(w, r, contentName)
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
//
// The core group serves ALL THREE user-facing namespaced subresources (manifests-download,
// manifests-with-data-restoration, manifests-and-children-refs-upload) ONLY for the core Snapshot kind
// (routeCoreSnapshotSubresource). Every other snapshot kind is addressed through the aggregated group of
// the module that owns it: that module's facade writes childRefs on its own CR and proxies/forwards to
// the cluster-scoped content layer. Serving another module's GVR here would be a redundant delegation
// hop, so every subresource is refused with 404 pointing the client at the node's own group.
func (h *RestoreHandler) routeGenericSnapshotSubresource(w http.ResponseWriter, resource, subresource string) {
	switch subresource {
	case "manifests-download", "manifests-with-data-restoration", "manifests-and-children-refs-upload":
		h.writeKubernetesErrorResponse(w, http.StatusNotFound, "NotFound",
			fmt.Sprintf("subresource %q is not served for %q under the core group; address the snapshot kind through the aggregated group of the module that owns it", subresource, resource))
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
	// The query contract (scope, object filter, and the statuses its violations map to) is shared with
	// every other server of this subresource, so it is parsed and answered by the SDK, never re-decided
	// here: a client must get the same answer to the same query on every group.
	opts, err := restoreapi.ParseQueryOptions(r)
	if err != nil {
		restoreapi.WriteError(w, err)
		return
	}
	if err := restoreapi.RejectForeignTargetNamespace(r, namespace); err != nil {
		restoreapi.WriteError(w, err)
		return
	}
	opts.SnapshotName = snapshotName
	opts.SnapshotNamespace = namespace

	data, err := h.service.BuildManifestsWithDataRestoration(r.Context(), opts)
	if err != nil {
		restoreapi.WriteError(w, err)
		return
	}

	h.writeJSONResponse(w, r, data)
	h.logger.Info("Returned manifests-with-data-restoration", "snapshot", snapshotName, "namespace", namespace, "scope", opts.Scope, "filterKind", opts.FilterKind, "filterName", opts.FilterName, "duration", time.Since(start))
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

// HandleContentSubtreeManifestIdentities returns the fail-closed set of captured object identities across
// the whole subtree of a cluster-scoped SnapshotContent (own node + descendants), addressed by content
// name. It backs the exclude-computation SDK method SubtreeManifestIdentities: a partial subtree yields
// 409 (writeAggregatedError maps the usecase's Conflict status), never a partial list.
func (h *RestoreHandler) HandleContentSubtreeManifestIdentities(w http.ResponseWriter, r *http.Request, contentName string) {
	start := time.Now()
	if h.nsAggregated == nil {
		h.writeKubernetesErrorResponse(w, http.StatusInternalServerError, "InternalError", "manifests handler not configured")
		return
	}
	data, err := h.nsAggregated.BuildSubtreeManifestIdentities(r.Context(), contentName)
	if err != nil {
		h.writeAggregatedError(w, err)
		return
	}
	h.writeJSONResponse(w, r, data)
	h.logger.Info("Returned subtree-manifest-identities", "content", contentName, "duration", time.Since(start))
}

// HandleSnapshotManifestsAndChildrenUpload persists one core Snapshot node's import payload
// ({manifests, childRefs}): it records the node's direct children, enforces bind-first, and forwards the
// manifests to the content-addressed layer. It is the only namespaced upload this module serves: the
// volume node's is served by the module that owns the CSI VolumeSnapshot, under its own group.
func (h *RestoreHandler) HandleSnapshotManifestsAndChildrenUpload(w http.ResponseWriter, r *http.Request, snapshotGVK schema.GroupVersionKind, namespace, snapshotName string) {
	start := time.Now()
	if h.importUpload == nil {
		h.writeKubernetesErrorResponse(w, http.StatusInternalServerError, "InternalError", "import upload handler not configured")
		return
	}
	body, ok := h.readUploadBody(w, r)
	if !ok {
		return
	}
	checkpointName, err := h.importUpload.Upload(r.Context(), snapshotGVK, namespace, snapshotName, body)
	if err != nil {
		h.writeAggregatedError(w, err)
		return
	}
	h.writeUploadSuccess(w, checkpointName)
	h.logger.Info("Accepted manifests-and-children-refs-upload", "resource", snapshotGVK.String(), "snapshot", snapshotName, "namespace", namespace, "checkpoint", checkpointName, "duration", time.Since(start))
}

// HandleContentManifestsUpload persists one node's import manifests addressed by cluster-scoped
// SnapshotContent name (manifests-only, content-addressed layer). It has NO bind-gate: the content exists
// by definition, so a missing content is a plain 404 (never ImportContentNotBound). childRefs are a
// namespaced-layer attribute and are not accepted here.
func (h *RestoreHandler) HandleContentManifestsUpload(w http.ResponseWriter, r *http.Request, contentName string) {
	start := time.Now()
	if h.importUpload == nil {
		h.writeKubernetesErrorResponse(w, http.StatusInternalServerError, "InternalError", "import upload handler not configured")
		return
	}
	body, ok := h.readUploadBody(w, r)
	if !ok {
		return
	}
	var payload usecase.ManifestsUpload
	if err := json.Unmarshal(body, &payload); err != nil {
		h.writeKubernetesErrorResponse(w, http.StatusBadRequest, "BadRequest", fmt.Sprintf("invalid upload payload: %v", err))
		return
	}
	content := &storagev1alpha1.SnapshotContent{}
	if err := h.client.Get(r.Context(), client.ObjectKey{Name: contentName}, content); err != nil {
		if apierrors.IsNotFound(err) {
			h.writeKubernetesErrorResponse(w, http.StatusNotFound, "NotFound", fmt.Sprintf("SnapshotContent %q not found", contentName))
			return
		}
		h.writeKubernetesErrorResponse(w, http.StatusInternalServerError, "InternalError", fmt.Sprintf("get SnapshotContent %q: %v", contentName, err))
		return
	}
	checkpointName, err := h.importUpload.UploadToContent(r.Context(), content, payload.Manifests)
	if err != nil {
		h.writeAggregatedError(w, err)
		return
	}
	h.writeUploadSuccess(w, checkpointName)
	h.logger.Info("Accepted per-content manifests-upload", "content", contentName, "checkpoint", checkpointName, "duration", time.Since(start))
}

// readUploadBody reads (and size-bounds) an upload request body, writing the appropriate error response
// and returning ok=false on failure.
func (h *RestoreHandler) readUploadBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxManifestsUploadBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.writeKubernetesErrorResponse(w, http.StatusRequestEntityTooLarge, "RequestEntityTooLarge",
				fmt.Sprintf("upload body exceeds %d bytes", maxManifestsUploadBytes))
			return nil, false
		}
		h.writeKubernetesErrorResponse(w, http.StatusBadRequest, "BadRequest", fmt.Sprintf("read upload body: %v", err))
		return nil, false
	}
	return body, true
}

// writeUploadSuccess writes the Success Status carrying the reconstructed ManifestCheckpoint name.
func (h *RestoreHandler) writeUploadSuccess(w http.ResponseWriter, checkpointName string) {
	resp := map[string]interface{}{
		"kind":                   "Status",
		"apiVersion":             "v1",
		"status":                 "Success",
		"manifestCheckpointName": checkpointName,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (h *RestoreHandler) writeAggregatedError(w http.ResponseWriter, err error) {
	var st *usecase.AggregatedStatusError
	if errors.As(err, &st) {
		h.writeKubernetesErrorResponse(w, st.HTTPStatus, st.Reason, st.Message)
		return
	}
	h.writeKubernetesErrorResponse(w, http.StatusInternalServerError, "InternalError", err.Error())
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
