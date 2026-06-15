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
	"errors"
	"fmt"
	"io"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

// ErrUploadOffsetConflict is returned when a resumable upload offset does not match the resume point.
var ErrUploadOffsetConflict = errors.New("upload offset conflict")

// ImportBlobStore persists opaque, resumable upload blobs (the import index and the whole-tree
// manifests archive) into cluster-scoped ManifestCheckpointContentChunk objects.
//
// Unlike the capture path it stores raw bytes (base64(gzip(raw))), not a JSON object array, so the
// blob can be uploaded as an arbitrary byte stream and read back verbatim. Chunks are addressed by a
// short, stable, name derived from the blob key, so append/status/read need only Get (no List), which
// keeps the resumable protocol cheap and free of informer caches.
//
// Blobs are not owner-GC'd: a cluster-scoped chunk cannot have a namespaced owner. The SnapshotImport
// controller deletes them explicitly during cleanup.
type ImportBlobStore struct {
	client client.Client
	logger logger.LoggerInterface
}

// NewImportBlobStore creates an ImportBlobStore. client must be a direct (cache-bypassing) client:
// chunk objects are internal-only and not watched by informers.
func NewImportBlobStore(c client.Client, l logger.LoggerInterface) *ImportBlobStore {
	return &ImportBlobStore{client: c, logger: l}
}

// Import blob kinds.
const (
	ImportBlobKindIndex     = "index"
	ImportBlobKindManifests = "manifests"
)

const (
	// importBlobLabel marks a chunk as an opaque resumable import blob so a periodic sweeper can
	// reclaim orphans (an abandoned or force-deleted SnapshotImport whose controller never ran the
	// explicit cleanup) even though a cluster-scoped chunk cannot carry a namespaced owner reference.
	importBlobLabel = "state-snapshotter.deckhouse.io/import-blob"
	// importBlobKeyAnnotation records the full blob key for traceability; the (possibly >63 char) key
	// does not fit a label value.
	importBlobKeyAnnotation = "state-snapshotter.deckhouse.io/import-blob-key"
)

// ImportBlobKey builds the deterministic key for an import blob.
func ImportBlobKey(namespace, importName, blobKind string) string {
	return fmt.Sprintf("snapimport-%s-%s-%s", namespace, importName, blobKind)
}

// ImportManifestsBlobKey builds the deterministic key for one snapshot node's manifests blob.
// Manifests are uploaded per node (see plan decision "per-node manifests"): each node of the index
// gets its own resumable blob, which the SnapshotImport controller reconstructs into a per-node
// ManifestCheckpoint referenced by the recreated SnapshotContent.
func ImportManifestsBlobKey(namespace, importName, nodeID string) string {
	return fmt.Sprintf("snapimport-%s-%s-manifests-%s", namespace, importName, nodeID)
}

// chunkName derives a short, RFC1123-safe chunk object name from the (possibly long) blob key.
func chunkName(key string, index int) string {
	h := sha256.Sum256([]byte(key))
	return fmt.Sprintf("import-blob-%s-%d", hex.EncodeToString(h[:8]), index)
}

// Status returns the number of bytes already persisted for the blob (the resume offset) and how many
// chunks back it. It walks chunks by name from index 0 until the first gap, summing the recorded
// RawBytes so the hot resume path does not gunzip every chunk (it falls back to decoding only for a
// chunk that predates the RawBytes field).
func (s *ImportBlobStore) Status(ctx context.Context, key string) (persistedBytes int64, chunks int, err error) {
	for i := 0; ; i++ {
		var chunk storagev1alpha1.ManifestCheckpointContentChunk
		gerr := s.client.Get(ctx, types.NamespacedName{Name: chunkName(key, i)}, &chunk)
		if apierrors.IsNotFound(gerr) {
			return persistedBytes, i, nil
		}
		if gerr != nil {
			return 0, 0, fmt.Errorf("failed to get import blob chunk %d for %s: %w", i, key, gerr)
		}
		if chunk.Spec.RawBytes > 0 || chunk.Spec.Data == "" {
			persistedBytes += chunk.Spec.RawBytes
			continue
		}
		raw, derr := decodeRawChunk(chunk.Spec.Data, chunk.Spec.Checksum)
		if derr != nil {
			return 0, 0, fmt.Errorf("failed to decode import blob chunk %d for %s: %w", i, key, derr)
		}
		persistedBytes += int64(len(raw))
	}
}

// Append writes data at offset. It is idempotent and resumable:
//   - offset == persisted: data is appended as the next chunk;
//   - offset <  persisted: the bytes are assumed already stored (a retransmit), no-op;
//   - offset >  persisted: a gap, rejected with ErrUploadOffsetConflict.
//
// It returns the new resume offset (persisted bytes after the call).
func (s *ImportBlobStore) Append(ctx context.Context, key string, offset int64, data []byte) (int64, error) {
	persisted, chunks, err := s.Status(ctx, key)
	if err != nil {
		return 0, err
	}
	switch {
	case offset > persisted:
		return persisted, fmt.Errorf("%w: offset %d but only %d bytes persisted", ErrUploadOffsetConflict, offset, persisted)
	case offset < persisted:
		// Already have these bytes (client retransmit). Report the real resume point.
		return persisted, nil
	}
	if len(data) == 0 {
		return persisted, nil
	}
	encoded, checksum, err := encodeRawChunk(data)
	if err != nil {
		return persisted, err
	}
	chunk := &storagev1alpha1.ManifestCheckpointContentChunk{
		ObjectMeta: metav1.ObjectMeta{
			Name:        chunkName(key, chunks),
			Labels:      map[string]string{importBlobLabel: "true"},
			Annotations: map[string]string{importBlobKeyAnnotation: key},
		},
		Spec: storagev1alpha1.ManifestCheckpointContentChunkSpec{
			CheckpointName: key,
			Index:          chunks,
			Data:           encoded,
			ObjectsCount:   0, // raw blob mode
			RawBytes:       int64(len(data)),
			Checksum:       checksum,
		},
	}
	if err := s.client.Create(ctx, chunk); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Concurrent/duplicate append created the same chunk; re-report status.
			np, _, serr := s.Status(ctx, key)
			if serr != nil {
				return persisted, serr
			}
			return np, nil
		}
		return persisted, fmt.Errorf("failed to create import blob chunk %d for %s: %w", chunks, key, err)
	}
	return persisted + int64(len(data)), nil
}

// Read concatenates the raw bytes of all chunks in order.
func (s *ImportBlobStore) Read(ctx context.Context, key string) ([]byte, error) {
	var out bytes.Buffer
	for i := 0; ; i++ {
		var chunk storagev1alpha1.ManifestCheckpointContentChunk
		err := s.client.Get(ctx, types.NamespacedName{Name: chunkName(key, i)}, &chunk)
		if apierrors.IsNotFound(err) {
			if i == 0 {
				return nil, fmt.Errorf("import blob %s is empty", key)
			}
			return out.Bytes(), nil
		}
		if err != nil {
			return nil, fmt.Errorf("failed to get import blob chunk %d for %s: %w", i, key, err)
		}
		raw, derr := decodeRawChunk(chunk.Spec.Data, chunk.Spec.Checksum)
		if derr != nil {
			return nil, fmt.Errorf("failed to decode import blob chunk %d for %s: %w", i, key, derr)
		}
		if _, werr := out.Write(raw); werr != nil {
			return nil, werr
		}
	}
}

// Delete removes every chunk backing the blob. It is idempotent.
func (s *ImportBlobStore) Delete(ctx context.Context, key string) error {
	for i := 0; ; i++ {
		chunk := &storagev1alpha1.ManifestCheckpointContentChunk{
			ObjectMeta: metav1.ObjectMeta{Name: chunkName(key, i)},
		}
		err := s.client.Delete(ctx, chunk)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("failed to delete import blob chunk %d for %s: %w", i, key, err)
		}
	}
}

func encodeRawChunk(raw []byte) (string, string, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(raw); err != nil {
		return "", "", fmt.Errorf("failed to gzip import blob chunk: %w", err)
	}
	if err := gz.Close(); err != nil {
		return "", "", fmt.Errorf("failed to finalize gzip import blob chunk: %w", err)
	}
	compressed := buf.Bytes()
	sum := sha256.Sum256(compressed)
	return base64.StdEncoding.EncodeToString(compressed), hex.EncodeToString(sum[:]), nil
}

func decodeRawChunk(encoded, expectedChecksum string) ([]byte, error) {
	compressed, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("failed to base64-decode chunk: %w", err)
	}
	// Verify the stored integrity hash before trusting the bytes (symmetric with the capture path).
	if expectedChecksum != "" {
		sum := sha256.Sum256(compressed)
		if got := hex.EncodeToString(sum[:]); got != expectedChecksum {
			return nil, fmt.Errorf("chunk checksum mismatch: got %s, want %s", got, expectedChecksum)
		}
	}
	gr, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer gr.Close()
	return io.ReadAll(gr)
}
