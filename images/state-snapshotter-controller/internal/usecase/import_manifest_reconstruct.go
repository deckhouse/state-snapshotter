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

package usecase

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
)

// reconstructMaxChunkBytes bounds a single reconstructed chunk's compressed (gzip) payload. It mirrors
// the capture default (config.MaxChunkSizeBytes) and stays well under the chunk CRD 1 MiB cap.
const reconstructMaxChunkBytes = 800 * 1000

// ReconstructedManifestCheckpointName derives the deterministic cluster-scoped ManifestCheckpoint name
// for one snapshot node on the import path. It is stable across reconciles (idempotency) and unique
// per (import UID, node) pair. The name uses the same prefix as captured checkpoints so the chunk
// naming convention (prefix-stripped id) the archive service relies on holds.
func ReconstructedManifestCheckpointName(importUID types.UID, nodeID string) string {
	h := sha256.Sum256([]byte(string(importUID) + "/" + nodeID))
	return namespacemanifest.CheckpointNamePrefix + hex.EncodeToString(h[:8])
}

// DeleteReconstructedManifestCheckpoint best-effort deletes the deterministically-named per-node
// ManifestCheckpoint that manifests-and-children-refs-upload reconstructed for the import snapshot
// identified by importUID (its chunks cascade via ownerRef). The reconstructed checkpoint is created
// ownerless (a cluster-scoped object cannot be owned by the namespaced snapshot CR); the import
// orchestrator (C5) adopts it onto the SnapshotContent it materializes, after which SnapshotContent
// lifecycle GCs it. This call covers the window before that adoption: if the import snapshot is deleted
// while still pending (no bound content), the orphan is removed here. NotFound is treated as success.
func DeleteReconstructedManifestCheckpoint(ctx context.Context, c client.Client, importUID types.UID) error {
	if importUID == "" {
		return nil
	}
	name := ReconstructedManifestCheckpointName(importUID, "")
	cp := &storagev1alpha1.ManifestCheckpoint{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := c.Delete(ctx, cp); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete reconstructed ManifestCheckpoint %s: %w", name, err)
	}
	return nil
}

// ReconstructManifestCheckpoint builds a canonical, Ready ManifestCheckpoint (plus its chunks) named
// checkpointName from rawManifests (a JSON array of objects, the per-node manifests uploaded on
// import). The produced object is byte-for-byte readable by the restore loader / ArchiveService, so a
// pre-provisioned SnapshotContent that references it restores exactly like a captured one.
//
// It is idempotent: an already-Ready checkpoint is left untouched; chunk creation tolerates
// AlreadyExists. captureRef is the synthetic source request reference required by
// ManifestCheckpointSpec; ownerRefs anchor the checkpoint for GC (the owning import snapshot CR).
func ReconstructManifestCheckpoint(
	ctx context.Context,
	c client.Client,
	checkpointName, sourceNamespace string,
	captureRef *storagev1alpha1.ObjectReference,
	ownerRefs []metav1.OwnerReference,
	rawManifests []byte,
) error {
	existing := &storagev1alpha1.ManifestCheckpoint{}
	if err := c.Get(ctx, types.NamespacedName{Name: checkpointName}, existing); err == nil {
		if meta.IsStatusConditionTrue(existing.Status.Conditions, storagev1alpha1.ManifestCheckpointConditionTypeReady) {
			return nil
		}
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get ManifestCheckpoint %s: %w", checkpointName, err)
	}

	var objects []json.RawMessage
	if err := json.Unmarshal(rawManifests, &objects); err != nil {
		return fmt.Errorf("manifests for %s are not a JSON array: %w", checkpointName, err)
	}

	cp := &storagev1alpha1.ManifestCheckpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:            checkpointName,
			OwnerReferences: ownerRefs,
			Labels: map[string]string{
				"state-snapshotter.deckhouse.io/source-namespace": sourceNamespace,
				"state-snapshotter.deckhouse.io/reconstructed":    "true",
			},
		},
		Spec: storagev1alpha1.ManifestCheckpointSpec{
			SourceNamespace:           sourceNamespace,
			ManifestCaptureRequestRef: captureRef,
		},
	}
	if err := c.Create(ctx, cp); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create ManifestCheckpoint %s: %w", checkpointName, err)
	}
	// Re-get to obtain the UID (needed for chunk owner references).
	if err := c.Get(ctx, types.NamespacedName{Name: checkpointName}, cp); err != nil {
		return fmt.Errorf("get reconstructed ManifestCheckpoint %s: %w", checkpointName, err)
	}

	infos, totalObjects, totalSize, err := writeReconstructedChunks(ctx, c, checkpointName, cp.UID, objects)
	if err != nil {
		return err
	}

	cp.Status.Chunks = infos
	cp.Status.TotalObjects = totalObjects
	cp.Status.TotalSizeBytes = totalSize
	meta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
		Type:    storagev1alpha1.ManifestCheckpointConditionTypeReady,
		Status:  metav1.ConditionTrue,
		Reason:  storagev1alpha1.ManifestCheckpointConditionReasonCompleted,
		Message: fmt.Sprintf("Reconstructed from import with %d chunk(s), %d object(s)", len(infos), totalObjects),
	})
	if err := c.Status().Update(ctx, cp); err != nil {
		return fmt.Errorf("update reconstructed ManifestCheckpoint %s status: %w", checkpointName, err)
	}
	return nil
}

// writeReconstructedChunks splits objects into size-bounded canonical chunks
// (base64(gzip(json[]))), creates them owned by the checkpoint, and returns their ChunkInfo plus
// totals. An empty object list still yields a single empty chunk (matches capture semantics).
func writeReconstructedChunks(
	ctx context.Context,
	c client.Client,
	checkpointName string,
	checkpointUID types.UID,
	objects []json.RawMessage,
) ([]storagev1alpha1.ChunkInfo, int, int64, error) {
	groups := groupObjectsBySize(objects)
	if len(groups) == 0 {
		groups = [][]json.RawMessage{{}}
	}

	controllerTrue := true
	id := strings.TrimPrefix(checkpointName, namespacemanifest.CheckpointNamePrefix)

	infos := make([]storagev1alpha1.ChunkInfo, 0, len(groups))
	totalObjects := 0
	totalSize := int64(0)
	for index, group := range groups {
		payload, err := json.Marshal(group)
		if err != nil {
			return nil, 0, 0, fmt.Errorf("marshal chunk %d for %s: %w", index, checkpointName, err)
		}
		gz, err := gzipBytes(payload)
		if err != nil {
			return nil, 0, 0, err
		}
		encoded := base64.StdEncoding.EncodeToString(gz)
		sum := sha256.Sum256(gz)
		checksum := hex.EncodeToString(sum[:])
		chunkName := fmt.Sprintf("%s%s-%d", namespacemanifest.CheckpointNamePrefix, id, index)

		chunk := &storagev1alpha1.ManifestCheckpointContentChunk{
			ObjectMeta: metav1.ObjectMeta{
				Name: chunkName,
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
					Kind:       "ManifestCheckpoint",
					Name:       checkpointName,
					UID:        checkpointUID,
					Controller: &controllerTrue,
				}},
			},
			Spec: storagev1alpha1.ManifestCheckpointContentChunkSpec{
				CheckpointName: checkpointName,
				Index:          index,
				Data:           encoded,
				ObjectsCount:   len(group),
				Checksum:       checksum,
			},
		}
		if err := c.Create(ctx, chunk); err != nil && !apierrors.IsAlreadyExists(err) {
			return nil, 0, 0, fmt.Errorf("create chunk %s: %w", chunkName, err)
		}
		infos = append(infos, storagev1alpha1.ChunkInfo{
			Name:         chunkName,
			Index:        index,
			ObjectsCount: len(group),
			SizeBytes:    int64(len(gz)),
			Checksum:     checksum,
		})
		totalObjects += len(group)
		totalSize += int64(len(gz))
	}
	return infos, totalObjects, totalSize, nil
}

// groupObjectsBySize greedily packs objects so each group's gzipped JSON array stays under
// reconstructMaxChunkBytes. A single object exceeding the limit is placed in its own group.
func groupObjectsBySize(objects []json.RawMessage) [][]json.RawMessage {
	var groups [][]json.RawMessage
	current := make([]json.RawMessage, 0)
	for _, obj := range objects {
		candidate := append(append([]json.RawMessage{}, current...), obj)
		payload, err := json.Marshal(candidate)
		if err != nil {
			// Should not happen for valid RawMessage; fall back to its own group.
			if len(current) > 0 {
				groups = append(groups, current)
				current = make([]json.RawMessage, 0)
			}
			groups = append(groups, []json.RawMessage{obj})
			continue
		}
		gz, err := gzipBytes(payload)
		if err == nil && len(gz) > reconstructMaxChunkBytes && len(current) > 0 {
			groups = append(groups, current)
			current = []json.RawMessage{obj}
			continue
		}
		current = candidate
	}
	if len(current) > 0 {
		groups = append(groups, current)
	}
	return groups
}

func gzipBytes(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(data); err != nil {
		return nil, fmt.Errorf("gzip write: %w", err)
	}
	if err := gw.Close(); err != nil {
		return nil, fmt.Errorf("gzip close: %w", err)
	}
	return buf.Bytes(), nil
}

// CollectReconstructedManifestObjects decodes every object stored in a ManifestCheckpoint's chunks (the
// base64(gzip(json[])) payload written by writeReconstructedChunks). It is the read twin of that writer.
//
// reader must bypass the informer cache (APIReader): ManifestCheckpoint and its chunks are internal-only
// and not watched. It exists for callers outside the archive/restore HTTP path that need a checkpoint's
// raw objects — currently the VolumeSnapshot import binder, which recovers the single orphan PVC manifest
// a leaf carries so the published dataRef can target that PVC (matching the capture path); a dataRef that
// targeted the VolumeSnapshot handle instead would make the restore compiler emit a data-less PVC.
func CollectReconstructedManifestObjects(ctx context.Context, reader client.Reader, checkpoint *storagev1alpha1.ManifestCheckpoint) ([]unstructured.Unstructured, error) {
	chunks := make([]storagev1alpha1.ChunkInfo, len(checkpoint.Status.Chunks))
	copy(chunks, checkpoint.Status.Chunks)
	sort.Slice(chunks, func(i, j int) bool { return chunks[i].Index < chunks[j].Index })

	out := make([]unstructured.Unstructured, 0, checkpoint.Status.TotalObjects)
	for _, info := range chunks {
		chunk := &storagev1alpha1.ManifestCheckpointContentChunk{}
		if err := reader.Get(ctx, types.NamespacedName{Name: info.Name}, chunk); err != nil {
			return nil, fmt.Errorf("get ManifestCheckpoint chunk %s: %w", info.Name, err)
		}
		objects, err := decodeReconstructedChunk(chunk.Spec.Data, info.Checksum, info.Name)
		if err != nil {
			return nil, err
		}
		out = append(out, objects...)
	}
	return out, nil
}

// ErrCorruptManifestChunk marks a chunk-content fault (bad base64/gzip/JSON or a checksum mismatch) as
// opposed to a transient chunk fetch failure. Callers use errors.Is to decide retry vs. terminal: the
// stored bytes are bad, so retrying the same chunk cannot succeed.
var ErrCorruptManifestChunk = errors.New("corrupt manifest checkpoint chunk")

// decodeReconstructedChunk inverts writeReconstructedChunks for a single chunk: base64 -> checksum verify
// -> gzip-decompress -> JSON array of objects. It deliberately handles only the canonical json[] shape
// produced on the import path (no legacy Key/Value handling), keeping it dependency-free of ArchiveService.
// All failures wrap ErrCorruptManifestChunk: they are content faults, not retryable fetch errors.
func decodeReconstructedChunk(encoded, expectedChecksum, chunkName string) ([]unstructured.Unstructured, error) {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("%w: decode base64 chunk %s: %v", ErrCorruptManifestChunk, chunkName, err)
	}
	if expectedChecksum != "" {
		sum := sha256.Sum256(data)
		if got := hex.EncodeToString(sum[:]); got != expectedChecksum {
			return nil, fmt.Errorf("%w: checksum mismatch for chunk %s: have %s, want %s", ErrCorruptManifestChunk, chunkName, got, expectedChecksum)
		}
	}
	gr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%w: gzip reader for chunk %s: %v", ErrCorruptManifestChunk, chunkName, err)
	}
	defer gr.Close()
	decompressed, err := io.ReadAll(gr)
	if err != nil {
		return nil, fmt.Errorf("%w: decompress chunk %s: %v", ErrCorruptManifestChunk, chunkName, err)
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(decompressed, &raws); err != nil {
		return nil, fmt.Errorf("%w: unmarshal chunk %s as JSON array: %v", ErrCorruptManifestChunk, chunkName, err)
	}
	objects := make([]unstructured.Unstructured, 0, len(raws))
	for i, raw := range raws {
		m := map[string]interface{}{}
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("%w: unmarshal object %d in chunk %s: %v", ErrCorruptManifestChunk, i, chunkName, err)
		}
		objects = append(objects, unstructured.Unstructured{Object: m})
	}
	return objects, nil
}
