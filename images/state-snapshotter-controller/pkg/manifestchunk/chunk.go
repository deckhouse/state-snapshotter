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

// Package manifestchunk provides helpers for creating ManifestCheckpointContentChunk resources
// and computing chunk metadata. It is used by both the ManifestCheckpointController (live capture)
// and the import API handler (d8 snapshot upload).
package manifestchunk

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
)

const defaultMaxChunkSizeBytes = 800_000

// CompressToBytes gzip-compresses data and returns the raw compressed bytes.
func CompressToBytes(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(data); err != nil {
		return nil, fmt.Errorf("write to gzip: %w", err)
	}
	if err := gw.Close(); err != nil {
		return nil, fmt.Errorf("close gzip: %w", err)
	}
	return buf.Bytes(), nil
}

// CalculateChecksum returns the SHA-256 hex digest of the base64-encoded compressed data.
// If decoding fails, the checksum is computed over the raw string bytes.
func CalculateChecksum(base64Compressed string) string {
	data, err := base64.StdEncoding.DecodeString(base64Compressed)
	if err != nil {
		data = []byte(base64Compressed)
	}
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// chunkName returns the deterministic chunk resource name for the given checkpoint and index.
func chunkName(checkpointName string, index int) string {
	id := checkpointName
	if strings.HasPrefix(checkpointName, namespacemanifest.CheckpointNamePrefix) {
		id = checkpointName[len(namespacemanifest.CheckpointNamePrefix):]
	}
	return fmt.Sprintf("%s%s-%d", namespacemanifest.CheckpointNamePrefix, id, index)
}

// CreateChunks splits jsonArrayData (a JSON array of objects) into size-bounded chunks,
// creates ManifestCheckpointContentChunk resources, and returns the corresponding ChunkInfo
// entries for inclusion in ManifestCheckpoint.status.chunks.
//
// checkpointUID must be non-empty – it is set in each chunk's ownerReference.
// If maxChunkSizeBytes <= 0 the default (800 000 bytes compressed) is used.
func CreateChunks(
	ctx context.Context,
	c client.Client,
	checkpointName string,
	checkpointUID string,
	jsonArrayData []byte,
	maxChunkSizeBytes int64,
) ([]ssv1alpha1.ChunkInfo, error) {
	if maxChunkSizeBytes <= 0 {
		maxChunkSizeBytes = defaultMaxChunkSizeBytes
	}

	// Unmarshal the JSON array.
	var objects []interface{}
	if err := json.Unmarshal(jsonArrayData, &objects); err != nil {
		return nil, fmt.Errorf("unmarshal manifest JSON array: %w", err)
	}

	ownerRef := manifestCheckpointOwnerRef(checkpointName, checkpointUID)

	// Empty manifest list → single empty chunk.
	if len(objects) == 0 {
		return createSingleChunk(ctx, c, checkpointName, ownerRef, 0, []byte("[]"), 0)
	}

	// Split into size-bounded groups.
	groups, err := splitObjects(objects, maxChunkSizeBytes)
	if err != nil {
		return nil, err
	}

	chunkInfos := make([]ssv1alpha1.ChunkInfo, 0, len(groups))
	for i, group := range groups {
		chunkJSON, err := json.Marshal(group)
		if err != nil {
			return nil, fmt.Errorf("marshal chunk %d: %w", i, err)
		}
		infos, err := createSingleChunk(ctx, c, checkpointName, ownerRef, i, chunkJSON, len(group))
		if err != nil {
			return nil, err
		}
		chunkInfos = append(chunkInfos, infos...)
	}
	return chunkInfos, nil
}

// createSingleChunk compresses chunkJSON, persists the chunk resource, and returns its ChunkInfo.
func createSingleChunk(
	ctx context.Context,
	c client.Client,
	checkpointName string,
	ownerRef metav1.OwnerReference,
	index int,
	chunkJSON []byte,
	objectsCount int,
) ([]ssv1alpha1.ChunkInfo, error) {
	gzipBytes, err := CompressToBytes(chunkJSON)
	if err != nil {
		return nil, fmt.Errorf("compress chunk %d: %w", index, err)
	}
	compressed := base64.StdEncoding.EncodeToString(gzipBytes)
	checksum := CalculateChecksum(compressed)
	name := chunkName(checkpointName, index)

	chunk := &ssv1alpha1.ManifestCheckpointContentChunk{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "state-snapshotter.deckhouse.io/v1alpha1",
			Kind:       "ManifestCheckpointContentChunk",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		Spec: ssv1alpha1.ManifestCheckpointContentChunkSpec{
			CheckpointName: checkpointName,
			Index:          index,
			Data:           compressed,
			ObjectsCount:   objectsCount,
			Checksum:       checksum,
		},
	}

	if err := c.Create(ctx, chunk); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("create chunk %s: %w", name, err)
		}
		// Idempotency: verify existing chunk matches.
		existing := &ssv1alpha1.ManifestCheckpointContentChunk{}
		if err := c.Get(ctx, client.ObjectKey{Name: name}, existing); err != nil {
			return nil, fmt.Errorf("get existing chunk %s: %w", name, err)
		}
		if existing.Spec.CheckpointName != checkpointName || existing.Spec.Index != index {
			return nil, fmt.Errorf("chunk %s belongs to a different checkpoint or index", name)
		}
		if existing.Spec.Checksum != checksum {
			return nil, fmt.Errorf("chunk %s checksum mismatch: expected %s, got %s", name, checksum, existing.Spec.Checksum)
		}
	}

	return []ssv1alpha1.ChunkInfo{{
		Name:         name,
		Index:        index,
		ObjectsCount: objectsCount,
		SizeBytes:    int64(len(gzipBytes)),
		Checksum:     checksum,
	}}, nil
}

// splitObjects groups objects into slices whose compressed JSON does not exceed maxBytes.
func splitObjects(objects []interface{}, maxBytes int64) ([][]interface{}, error) {
	var groups [][]interface{}
	current := make([]interface{}, 0)

	for _, obj := range objects {
		candidate := append(current, obj) //nolint:gocritic // intentional copy
		testJSON, err := json.Marshal(candidate)
		if err != nil {
			return nil, fmt.Errorf("marshal candidate chunk: %w", err)
		}
		gzipBytes, err := CompressToBytes(testJSON)
		if err != nil {
			return nil, fmt.Errorf("compress candidate chunk: %w", err)
		}
		if int64(len(gzipBytes)) > maxBytes && len(current) > 0 {
			// Flush current group, start fresh.
			groups = append(groups, current)
			current = []interface{}{obj}
		} else {
			current = candidate
		}
	}
	if len(current) > 0 {
		groups = append(groups, current)
	}
	return groups, nil
}

func manifestCheckpointOwnerRef(checkpointName, checkpointUID string) metav1.OwnerReference {
	controller := true
	return metav1.OwnerReference{
		APIVersion: "state-snapshotter.deckhouse.io/v1alpha1",
		Kind:       "ManifestCheckpoint",
		Name:       checkpointName,
		UID:        types.UID(checkpointUID),
		Controller: &controller,
	}
}
