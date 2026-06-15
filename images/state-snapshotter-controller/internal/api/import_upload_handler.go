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

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapstorage "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase/restore"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

// maxImportUploadChunkBytes bounds a single append body. The chunk CRD itself caps the encoded
// (base64+gzip) payload at 1 MiB, so keep raw chunks comfortably below that.
const maxImportUploadChunkBytes = 512 * 1024

// ImportUploadHandler serves the resumable, idempotent upload subresources of SnapshotImport:
//
//	POST/PUT .../snapshotimports/{name}/index      append index bytes
//	POST/PUT .../snapshotimports/{name}/manifests  append whole-tree manifests bytes
//	HEAD     .../snapshotimports/{name}/{index|manifests}  report the resume offset
//
// Resumable protocol (mirrors the SVDM data upload):
//   - HEAD returns X-Next-Offset: the number of bytes already persisted.
//   - POST/PUT sends X-Offset: the offset the body starts at; the server appends iff it equals the
//     resume offset, is a no-op if below it (retransmit), and 409s on a gap. The response carries the
//     new X-Next-Offset.
//   - A request with ?finalize=true (and an empty or final body) validates the assembled blob and
//     flips the matching SnapshotImport condition (IndexReceived / ManifestsReceived).
type ImportUploadHandler struct {
	client client.Client
	blobs  *usecase.ImportBlobStore
	logger logger.LoggerInterface
}

// NewImportUploadHandler builds the handler. c must be a direct (cache-bypassing) client.
func NewImportUploadHandler(c client.Client, blobs *usecase.ImportBlobStore, l logger.LoggerInterface) *ImportUploadHandler {
	return &ImportUploadHandler{client: c, blobs: blobs, logger: l}
}

// Handle dispatches an upload request for snapshotimports/{name}/{sub}. It is invoked by the shared
// RestoreHandler mux entry (a single ServeMux pattern owns the subresources prefix) for the
// "snapshotimports" resource, so it must not register its own route.
//
// Manifests are per node (plan decision "per-node manifests"): a manifests request carries the node
// selector as the ?node=<snapshotId> query parameter, so each index node gets its own resumable blob.
// A manifests request without ?node and finalize=true is the whole-tree "commit" that flips
// ManifestsReceived once every per-node blob has been uploaded.
func (h *ImportUploadHandler) Handle(w http.ResponseWriter, r *http.Request, namespace, importName, sub string) {
	var blobKind, node, key string
	switch sub {
	case "index":
		blobKind = usecase.ImportBlobKindIndex
		key = usecase.ImportBlobKey(namespace, importName, blobKind)
	case "manifests":
		blobKind = usecase.ImportBlobKindManifests
		node = r.URL.Query().Get("node")
		if node != "" {
			key = usecase.ImportManifestsBlobKey(namespace, importName, node)
		} else {
			key = usecase.ImportBlobKey(namespace, importName, blobKind)
		}
	default:
		h.writeError(w, http.StatusNotFound, "NotFound", "unknown subresource")
		return
	}

	switch r.Method {
	case http.MethodHead:
		h.handleHead(w, r, key)
	case http.MethodPost, http.MethodPut:
		h.handleUpload(w, r, namespace, importName, blobKind, node, key)
	default:
		// GET shares the "get" verb with HEAD in RBAC, but it has no meaning for an upload-only
		// endpoint and is intentionally rejected here (the offset is reported via HEAD).
		h.writeError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "only HEAD, POST and PUT are supported")
	}
}

func (h *ImportUploadHandler) handleHead(w http.ResponseWriter, r *http.Request, key string) {
	persisted, _, err := h.blobs.Status(r.Context(), key)
	if err != nil {
		h.logger.Error(err, "import upload HEAD: status failed", "blob", key)
		h.writeError(w, http.StatusInternalServerError, "InternalError", "internal error")
		return
	}
	w.Header().Set("X-Next-Offset", strconv.FormatInt(persisted, 10))
	w.WriteHeader(http.StatusOK)
}

func (h *ImportUploadHandler) handleUpload(w http.ResponseWriter, r *http.Request, namespace, importName, blobKind, node, key string) {
	// The SnapshotImport must exist before its data is uploaded.
	if _, err := h.getImport(r.Context(), namespace, importName); err != nil {
		h.writeImportError(w, err)
		return
	}

	offset := int64(0)
	if v := r.Header.Get("X-Offset"); v != "" {
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil || parsed < 0 {
			h.writeError(w, http.StatusBadRequest, "BadRequest", "invalid X-Offset header")
			return
		}
		offset = parsed
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxImportUploadChunkBytes+1))
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "BadRequest", "failed to read body")
		return
	}
	if len(body) > maxImportUploadChunkBytes {
		h.writeError(w, http.StatusRequestEntityTooLarge, "RequestEntityTooLarge",
			"upload chunk exceeds limit; split into smaller chunks")
		return
	}

	next, err := h.blobs.Append(r.Context(), key, offset, body)
	if err != nil {
		if errors.Is(err, usecase.ErrUploadOffsetConflict) {
			w.Header().Set("X-Next-Offset", strconv.FormatInt(next, 10))
			h.writeError(w, http.StatusConflict, "Conflict", err.Error())
			return
		}
		h.logger.Error(err, "import upload append failed", "blob", key, "offset", offset)
		h.writeError(w, http.StatusInternalServerError, "InternalError", "internal error")
		return
	}
	w.Header().Set("X-Next-Offset", strconv.FormatInt(next, 10))

	if r.URL.Query().Get("finalize") == "true" {
		if err := h.finalize(r.Context(), namespace, importName, blobKind, node, key); err != nil {
			h.writeImportError(w, err)
			return
		}
		w.WriteHeader(http.StatusOK)
		h.logger.Info("Finalized SnapshotImport upload", "import", importName, "namespace", namespace, "blob", blobKind, "node", node, "bytes", next)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// finalize validates the assembled blob and, for whole-tree finalize calls, flips the matching
// SnapshotImport condition so the controller can pick the import up. Validation fails closed: a
// corrupt index/manifests blob is reported as a 400 and the condition is not set.
//
// For manifests the contract is:
//   - per-node finalize (node != ""): validate that node's JSON array only; no condition flip.
//   - whole-tree finalize (node == ""): the client's "commit" once every per-node blob is uploaded;
//     flips ManifestsReceived. The controller then reconstructs a per-node ManifestCheckpoint from
//     each per-node blob and fails the import if any node's blob is missing.
func (h *ImportUploadHandler) finalize(ctx context.Context, namespace, importName, blobKind, node, key string) error {
	switch blobKind {
	case usecase.ImportBlobKindIndex:
		raw, err := h.blobs.Read(ctx, key)
		if err != nil {
			return usecase.NewAggregatedStatusError(http.StatusConflict, "Conflict", err.Error())
		}
		var idx restore.Index
		if err := json.Unmarshal(raw, &idx); err != nil {
			return usecase.NewAggregatedStatusError(http.StatusBadRequest, "BadRequest", "uploaded index is not valid JSON")
		}
		if idx.Version == "" || len(idx.Snapshots) == 0 {
			return usecase.NewAggregatedStatusError(http.StatusBadRequest, "BadRequest", "uploaded index is empty or missing version")
		}
		return h.setImportCondition(ctx, namespace, importName, snapstorage.SnapshotImportConditionIndexReceived, "IndexUploaded")

	case usecase.ImportBlobKindManifests:
		if node != "" {
			// Per-node finalize: just validate this node's blob shape; do not flip the global condition.
			raw, err := h.blobs.Read(ctx, key)
			if err != nil {
				return usecase.NewAggregatedStatusError(http.StatusConflict, "Conflict", err.Error())
			}
			var objs []json.RawMessage
			if err := json.Unmarshal(raw, &objs); err != nil {
				return usecase.NewAggregatedStatusError(http.StatusBadRequest, "BadRequest", "uploaded manifests is not a JSON array")
			}
			return nil
		}
		// Whole-tree commit: every per-node manifests blob has been uploaded.
		return h.setImportCondition(ctx, namespace, importName, snapstorage.SnapshotImportConditionManifestsReceived, "ManifestsUploaded")
	}
	return nil
}

func (h *ImportUploadHandler) setImportCondition(ctx context.Context, namespace, importName, conditionType, reason string) error {
	imp, err := h.getImport(ctx, namespace, importName)
	if err != nil {
		return err
	}
	meta.SetStatusCondition(&imp.Status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            "Upload received via aggregated subresource",
		ObservedGeneration: imp.Generation,
	})
	if err := h.client.Status().Update(ctx, imp); err != nil {
		h.logger.Error(err, "update SnapshotImport condition failed", "namespace", namespace, "name", importName)
		return usecase.NewAggregatedStatusError(http.StatusInternalServerError, "InternalError", "internal error")
	}
	return nil
}

func (h *ImportUploadHandler) getImport(ctx context.Context, namespace, name string) (*snapstorage.SnapshotImport, error) {
	imp := &snapstorage.SnapshotImport{}
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, imp); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, usecase.NewAggregatedStatusError(http.StatusNotFound, "NotFound",
				"SnapshotImport "+namespace+"/"+name+" not found")
		}
		h.logger.Error(err, "get SnapshotImport failed", "namespace", namespace, "name", name)
		return nil, usecase.NewAggregatedStatusError(http.StatusInternalServerError, "InternalError", "internal error")
	}
	return imp, nil
}

func (h *ImportUploadHandler) writeImportError(w http.ResponseWriter, err error) {
	var st *usecase.AggregatedStatusError
	if errors.As(err, &st) {
		h.writeError(w, st.HTTPStatus, st.Reason, st.Message)
		return
	}
	h.logger.Error(err, "import upload internal error")
	h.writeError(w, http.StatusInternalServerError, "InternalError", "internal error")
}

func (h *ImportUploadHandler) writeError(w http.ResponseWriter, code int, reason, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	status := metav1.Status{
		TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
		Status:   metav1.StatusFailure,
		Message:  message,
		Reason:   metav1.StatusReason(reason),
		Code:     int32(code),
	}
	_ = json.NewEncoder(w).Encode(status)
}
