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

package api //nolint:revive

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/config"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/manifestchunk"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

const (
	labelImportMode  = "state-snapshotter.deckhouse.io/import-mode"
	labelValueTrue   = "true"
	labelSourceSnap  = "state-snapshotter.deckhouse.io/source-snapshot"
	labelSourceNS    = "state-snapshotter.deckhouse.io/source-namespace"
	maxImportBodyMB  = 32 // maximum manifest upload body in MB
	maxImportBodyLen = maxImportBodyMB * 1024 * 1024
)

// ImportHandler handles the snapshot import API endpoints:
//   - PUT  .../namespaces/{ns}/snapshots/{name}/import-manifests/{nodeId}
//   - POST .../namespaces/{ns}/snapshots/{name}/import-build
type ImportHandler struct {
	client client.Client
	cfg    *config.Options
	logger logger.LoggerInterface
}

// NewImportHandler creates a new ImportHandler.
func NewImportHandler(c client.Client, cfg *config.Options, log logger.LoggerInterface) *ImportHandler {
	return &ImportHandler{client: c, cfg: cfg, logger: log}
}

// HandleImportManifests handles PUT .../snapshots/{snapshotName}/import-manifests/{nodeId}.
// Body: gzip-compressed JSON array of manifest objects.
// Creates (or idempotently verifies) an import-mode ManifestCheckpoint with Ready=True.
func (h *ImportHandler) HandleImportManifests(
	w http.ResponseWriter, r *http.Request,
	namespace, snapshotName, nodeID string,
) {
	start := time.Now()
	ctx := r.Context()

	if namespace == "" || snapshotName == "" || nodeID == "" {
		h.writeError(w, http.StatusBadRequest, "BadRequest", "namespace, snapshotName and nodeId are required")
		return
	}

	jsonData, err := h.readManifestBody(r)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "BadRequest", err.Error())
		return
	}

	mcpName := importMCPName(namespace, snapshotName, nodeID)

	if err := h.ensureImportManifestCheckpoint(ctx, namespace, snapshotName, nodeID, mcpName, jsonData); err != nil {
		h.logger.Error(err, "failed to ensure import ManifestCheckpoint",
			"namespace", namespace, "snapshot", snapshotName, "nodeID", nodeID)
		h.writeError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}

	resp := map[string]string{"manifestCheckpointName": mcpName}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
	h.logger.Info("import-manifests: checkpoint ready",
		"namespace", namespace, "snapshot", snapshotName, "nodeID", nodeID,
		"mcp", mcpName, "duration", time.Since(start))
}

// HandleImportBuild handles POST .../snapshots/{snapshotName}/import-build.
// Body: ImportBuildRequest JSON.
// Synchronously assembles the SnapshotContent tree and creates the root Snapshot.
func (h *ImportHandler) HandleImportBuild(
	w http.ResponseWriter, r *http.Request,
	namespace, snapshotName string,
) {
	start := time.Now()
	ctx := r.Context()

	if namespace == "" || snapshotName == "" {
		h.writeError(w, http.StatusBadRequest, "BadRequest", "namespace and snapshotName are required")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxImportBodyLen))
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "BadRequest", "failed to read request body")
		return
	}
	var req ImportBuildRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.writeError(w, http.StatusBadRequest, "BadRequest", fmt.Sprintf("invalid JSON: %v", err))
		return
	}
	req.Namespace = namespace
	req.SnapshotName = snapshotName

	result, err := h.buildSnapshotTree(ctx, req)
	if err != nil {
		h.logger.Error(err, "import-build: failed to build snapshot tree",
			"namespace", namespace, "snapshot", snapshotName)
		h.writeError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
	h.logger.Info("import-build: snapshot tree built",
		"namespace", namespace, "snapshot", snapshotName, "duration", time.Since(start))
}

// ── helpers ─────────────────────────────────────────────────────────────────

func (h *ImportHandler) readManifestBody(r *http.Request) ([]byte, error) {
	body := io.LimitReader(r.Body, maxImportBodyLen)

	// Auto-detect gzip by Content-Encoding or magic bytes; decompress if needed.
	if r.Header.Get("Content-Encoding") == "gzip" {
		gr, err := gzip.NewReader(body)
		if err != nil {
			return nil, fmt.Errorf("open gzip reader: %w", err)
		}
		defer gr.Close()
		data, err := io.ReadAll(gr)
		if err != nil {
			return nil, fmt.Errorf("read gzip body: %w", err)
		}
		return data, nil
	}
	return io.ReadAll(body)
}

// ensureImportManifestCheckpoint creates the MCP + chunks idempotently.
func (h *ImportHandler) ensureImportManifestCheckpoint(
	ctx context.Context,
	namespace, snapshotName, nodeID, mcpName string,
	jsonData []byte,
) error {
	mcp := &ssv1alpha1.ManifestCheckpoint{}
	err := h.client.Get(ctx, client.ObjectKey{Name: mcpName}, mcp)
	switch {
	case err == nil:
		// Already exists. If already Ready=True, treat as idempotent success.
		ready := meta.FindStatusCondition(mcp.Status.Conditions, ssv1alpha1.ManifestCheckpointConditionTypeReady)
		if ready != nil && ready.Status == metav1.ConditionTrue {
			return nil
		}
		// Re-run chunk creation (handles partial retries).
	case apierrors.IsNotFound(err):
		// Create the MCP.
		mcp = &ssv1alpha1.ManifestCheckpoint{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "state-snapshotter.deckhouse.io/v1alpha1",
				Kind:       "ManifestCheckpoint",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: mcpName,
				Labels: map[string]string{
					labelImportMode: labelValueTrue,
					labelSourceSnap: snapshotName,
					labelSourceNS:   namespace,
				},
			},
			Spec: ssv1alpha1.ManifestCheckpointSpec{
				SourceNamespace:           namespace,
				ManifestCaptureRequestRef: nil, // import mode
			},
		}
		if cerr := h.client.Create(ctx, mcp); cerr != nil && !apierrors.IsAlreadyExists(cerr) {
			return fmt.Errorf("create ManifestCheckpoint %s: %w", mcpName, cerr)
		}
		// Re-read to get UID.
		if gerr := h.client.Get(ctx, client.ObjectKey{Name: mcpName}, mcp); gerr != nil {
			return fmt.Errorf("get ManifestCheckpoint %s after create: %w", mcpName, gerr)
		}
	default:
		return fmt.Errorf("get ManifestCheckpoint %s: %w", mcpName, err)
	}

	maxChunkSize := int64(config.DefaultMaxChunkSizeBytes)
	if h.cfg != nil && h.cfg.MaxChunkSizeBytes > 0 {
		maxChunkSize = h.cfg.MaxChunkSizeBytes
	}

	chunks, err := manifestchunk.CreateChunks(ctx, h.client, mcpName, string(mcp.UID), jsonData, maxChunkSize)
	if err != nil {
		return fmt.Errorf("create chunks for %s: %w", mcpName, err)
	}

	// Compute totals.
	totalObjects := 0
	var totalSizeBytes int64
	for _, ci := range chunks {
		totalObjects += ci.ObjectsCount
		totalSizeBytes += ci.SizeBytes
	}

	// Publish status: chunks + Ready=True.
	base := mcp.DeepCopy()
	mcp.Status.Chunks = chunks
	mcp.Status.TotalObjects = totalObjects
	mcp.Status.TotalSizeBytes = totalSizeBytes
	meta.SetStatusCondition(&mcp.Status.Conditions, metav1.Condition{
		Type:               ssv1alpha1.ManifestCheckpointConditionTypeReady,
		Status:             metav1.ConditionTrue,
		Reason:             ssv1alpha1.ManifestCheckpointConditionReasonCompleted,
		Message:            fmt.Sprintf("import: %d objects in %d chunk(s)", totalObjects, len(chunks)),
		LastTransitionTime: metav1.Now(),
	})
	if err := h.client.Status().Patch(ctx, mcp, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("patch ManifestCheckpoint %s status: %w", mcpName, err)
	}
	return nil
}

// importMCPName returns a deterministic cluster-scoped MCP name for an import node.
func importMCPName(namespace, snapshotName, nodeID string) string {
	h := sha256.Sum256([]byte(namespace + "/" + snapshotName + "/" + nodeID))
	return "import-" + hex.EncodeToString(h[:8])
}

func (h *ImportHandler) writeError(w http.ResponseWriter, code int, reason, message string) {
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
