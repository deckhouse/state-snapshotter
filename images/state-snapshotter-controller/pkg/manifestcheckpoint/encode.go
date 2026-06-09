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

// Package manifestcheckpoint provides shared encoding utilities for
// ManifestCheckpointContentChunk creation used by both the MCR controller
// and the SnapshotImportRequest controller.
package manifestcheckpoint

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"

	"k8s.io/apimachinery/pkg/types"
)

const (
	// ChunkNamePrefix matches the MCR controller's naming (same prefix for all chunk kinds).
	ChunkNamePrefix = "mcp-"
)

// CompressToBytes compresses data with gzip and returns the compressed bytes.
func CompressToBytes(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(data); err != nil {
		return nil, fmt.Errorf("failed to write to gzip: %w", err)
	}
	if err := gw.Close(); err != nil {
		return nil, fmt.Errorf("failed to close gzip: %w", err)
	}
	return buf.Bytes(), nil
}

// CompressToBase64 compresses data with gzip and returns (base64String, rawGzipBytes, error).
func CompressToBase64(data []byte) (string, []byte, error) {
	gzipBytes, err := CompressToBytes(data)
	if err != nil {
		return "", nil, err
	}
	return base64.StdEncoding.EncodeToString(gzipBytes), gzipBytes, nil
}

// CalculateChecksum calculates the SHA256 hash of the raw gzip bytes encoded in base64data.
// This matches the MCR controller's calculateChunkChecksum function.
func CalculateChecksum(base64data string) string {
	data, err := base64.StdEncoding.DecodeString(base64data)
	if err != nil {
		data = []byte(base64data)
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

// ImportCheckpointName returns the stable ManifestCheckpoint name for a specific node
// within a SnapshotImportRequest. Uses a hash of the import request UID + nodeID.
func ImportCheckpointName(importRequestUID types.UID, nodeID string) string {
	hash := sha256.Sum256([]byte(string(importRequestUID) + "|" + nodeID))
	return ChunkNamePrefix + "import-" + hex.EncodeToString(hash[:8])
}

// ImportChunkName returns the name for a ManifestCheckpointContentChunk produced from
// an import node (index is 0-based).
func ImportChunkName(checkpointName string, index int) string {
	return fmt.Sprintf("%s-%d", checkpointName, index)
}
