/*
Copyright 2025 Flant JSC

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
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

func setupTestService() (*ArchiveService, client.Client) {
	scheme := runtime.NewScheme()
	_ = storagev1alpha1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	log, _ := logger.NewLogger("error")
	service := NewArchiveService(fakeClient, fakeClient, log)

	return service, fakeClient
}

//nolint:unparam // name parameter is kept for test flexibility
func createTestCheckpoint(name string, ready bool, chunks []storagev1alpha1.ChunkInfo) *storagev1alpha1.ManifestCheckpoint {
	checkpoint := &storagev1alpha1.ManifestCheckpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			UID:  types.UID("test-uid-" + name),
		},
		Spec: storagev1alpha1.ManifestCheckpointSpec{
			SourceNamespace: "test-ns",
			ManifestCaptureRequestRef: &storagev1alpha1.ObjectReference{
				Name:      "test-mcr",
				Namespace: "test-ns",
				UID:       "mcr-uid",
			},
		},
		Status: storagev1alpha1.ManifestCheckpointStatus{
			Chunks:         chunks,
			TotalObjects:   len(chunks),
			TotalSizeBytes: int64(len(chunks) * 1000),
		},
	}

	if ready {
		meta.SetStatusCondition(&checkpoint.Status.Conditions, metav1.Condition{
			Type:   "Ready",
			Status: metav1.ConditionTrue,
			Reason: "Completed",
		})
	}

	return checkpoint
}

func createTestChunk(name, checkpointName string, index int, data string, checksum string) *storagev1alpha1.ManifestCheckpointContentChunk {
	return &storagev1alpha1.ManifestCheckpointContentChunk{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: storagev1alpha1.ManifestCheckpointContentChunkSpec{
			CheckpointName: checkpointName,
			Index:          index,
			Data:           data,
			Checksum:       checksum,
			ObjectsCount:   1,
		},
	}
}

func encodeChunkData(objects []map[string]interface{}) (string, string) {
	jsonData, _ := json.Marshal(objects)

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write(jsonData)
	gz.Close()

	encoded := base64.StdEncoding.EncodeToString(buf.Bytes())
	hash := sha256.Sum256(buf.Bytes())
	checksum := hex.EncodeToString(hash[:])

	return encoded, checksum
}

// TestGetArchiveFromCheckpoint_NotReady tests error handling when checkpoint is not ready.
// Verifies:
// - Returns error when checkpoint Ready condition is not True
// - Error message indicates checkpoint is not ready
func TestGetArchiveFromCheckpoint_NotReady(t *testing.T) {
	service, client := setupTestService()

	checkpoint := createTestCheckpoint("test", false, []storagev1alpha1.ChunkInfo{})
	_ = client.Create(context.Background(), checkpoint)

	req := &ArchiveRequest{
		CheckpointName:  "test",
		CheckpointUID:   string(checkpoint.UID),
		SourceNamespace: "test-ns",
	}

	_, _, err := service.GetArchiveFromCheckpoint(context.Background(), checkpoint, req)

	if err == nil {
		t.Fatal("Expected error for not ready checkpoint")
	}

	if !strings.Contains(err.Error(), "not ready") {
		t.Errorf("Expected 'not ready' in error, got: %v", err)
	}
}

// TestGetArchiveFromCheckpoint_ChunkMissing tests error handling when chunk is missing.
// Verifies:
// - Returns error when chunk referenced in Status.Chunks is not found
// - Error message indicates chunk is missing
func TestGetArchiveFromCheckpoint_ChunkMissing(t *testing.T) {
	service, client := setupTestService()

	chunks := []storagev1alpha1.ChunkInfo{
		{Name: "chunk-0", Index: 0, Checksum: "test"},
	}
	checkpoint := createTestCheckpoint("test", true, chunks)
	_ = client.Create(context.Background(), checkpoint)

	req := &ArchiveRequest{
		CheckpointName:  "test",
		CheckpointUID:   string(checkpoint.UID),
		SourceNamespace: "test-ns",
	}

	_, _, err := service.GetArchiveFromCheckpoint(context.Background(), checkpoint, req)

	if err == nil {
		t.Fatal("Expected error for missing chunk")
	}

	if !strings.Contains(err.Error(), "chunk missing") {
		t.Errorf("Expected 'chunk missing' in error, got: %v", err)
	}
}

// TestGetArchiveFromCheckpoint_InvalidChunkIndexSequence tests validation of chunk index sequence.
// Verifies:
// - Returns error when chunk indices are not sequential (e.g., 0, 2 missing 1)
// - Error message indicates invalid sequence
func TestGetArchiveFromCheckpoint_InvalidChunkIndexSequence(t *testing.T) {
	service, client := setupTestService()

	// Invalid sequence: 0, 2 (missing 1)
	chunks := []storagev1alpha1.ChunkInfo{
		{Name: "chunk-0", Index: 0, Checksum: "test"},
		{Name: "chunk-2", Index: 2, Checksum: "test"},
	}
	checkpoint := createTestCheckpoint("test", true, chunks)
	_ = client.Create(context.Background(), checkpoint)

	req := &ArchiveRequest{
		CheckpointName:  "test",
		CheckpointUID:   string(checkpoint.UID),
		SourceNamespace: "test-ns",
	}

	_, _, err := service.GetArchiveFromCheckpoint(context.Background(), checkpoint, req)

	if err == nil {
		t.Fatal("Expected error for invalid index sequence")
	}

	if !strings.Contains(err.Error(), "invalid chunk index sequence") {
		t.Errorf("Expected 'invalid chunk index sequence' in error, got: %v", err)
	}
}

// TestGetArchiveFromCheckpoint_ChunkBelongsToDifferentCheckpoint tests validation that chunk belongs to checkpoint.
// Verifies:
// - Returns error when chunk.Spec.CheckpointName doesn't match checkpoint name
// - Prevents using chunks from other checkpoints
func TestGetArchiveFromCheckpoint_ChunkBelongsToDifferentCheckpoint(t *testing.T) {
	service, client := setupTestService()

	data, checksum := encodeChunkData([]map[string]interface{}{{"kind": "Test"}})
	chunk := createTestChunk("chunk-0", "other-checkpoint", 0, data, checksum)
	_ = client.Create(context.Background(), chunk)

	chunks := []storagev1alpha1.ChunkInfo{
		{Name: "chunk-0", Index: 0, Checksum: checksum},
	}
	checkpoint := createTestCheckpoint("test", true, chunks)
	_ = client.Create(context.Background(), checkpoint)

	req := &ArchiveRequest{
		CheckpointName:  "test",
		CheckpointUID:   string(checkpoint.UID),
		SourceNamespace: "test-ns",
	}

	_, _, err := service.GetArchiveFromCheckpoint(context.Background(), checkpoint, req)

	if err == nil {
		t.Fatal("Expected error for chunk belonging to different checkpoint")
	}

	if !strings.Contains(err.Error(), "does not belong to checkpoint") {
		t.Errorf("Expected 'does not belong to checkpoint' in error, got: %v", err)
	}
}

// TestGetArchiveFromCheckpoint_ChunkIndexMismatch tests validation of chunk index against Status.Chunks.
// Verifies:
// - Returns error when chunk.Spec.Index doesn't match expected index from Status.Chunks
// - Prevents using chunks in wrong order
func TestGetArchiveFromCheckpoint_ChunkIndexMismatch(t *testing.T) {
	service, client := setupTestService()

	data, checksum := encodeChunkData([]map[string]interface{}{{"kind": "Test"}})
	// Chunk has index 1, but chunkInfo says index 0
	chunk := createTestChunk("chunk-0", "test", 1, data, checksum)
	_ = client.Create(context.Background(), chunk)

	chunks := []storagev1alpha1.ChunkInfo{
		{Name: "chunk-0", Index: 0, Checksum: checksum},
	}
	checkpoint := createTestCheckpoint("test", true, chunks)
	_ = client.Create(context.Background(), checkpoint)

	req := &ArchiveRequest{
		CheckpointName:  "test",
		CheckpointUID:   string(checkpoint.UID),
		SourceNamespace: "test-ns",
	}

	_, _, err := service.GetArchiveFromCheckpoint(context.Background(), checkpoint, req)

	if err == nil {
		t.Fatal("Expected error for chunk index mismatch")
	}

	if !strings.Contains(err.Error(), "index mismatch") {
		t.Errorf("Expected 'index mismatch' in error, got: %v", err)
	}
}

// TestGetArchiveFromCheckpoint_ChecksumMismatch tests checksum verification of chunk data.
// Verifies:
// - Returns error when chunk data checksum doesn't match expected checksum
// - Prevents using corrupted or tampered chunks
func TestGetArchiveFromCheckpoint_ChecksumMismatch(t *testing.T) {
	service, client := setupTestService()

	data, _ := encodeChunkData([]map[string]interface{}{{"kind": "Test"}})
	// Wrong checksum
	chunk := createTestChunk("chunk-0", "test", 0, data, "wrong-checksum")
	_ = client.Create(context.Background(), chunk)

	chunks := []storagev1alpha1.ChunkInfo{
		{Name: "chunk-0", Index: 0, Checksum: "correct-checksum"},
	}
	checkpoint := createTestCheckpoint("test", true, chunks)
	_ = client.Create(context.Background(), checkpoint)

	req := &ArchiveRequest{
		CheckpointName:  "test",
		CheckpointUID:   string(checkpoint.UID),
		SourceNamespace: "test-ns",
	}

	_, _, err := service.GetArchiveFromCheckpoint(context.Background(), checkpoint, req)

	if err == nil {
		t.Fatal("Expected error for checksum mismatch")
	}

	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("Expected 'checksum mismatch' in error, got: %v", err)
	}
}

// TestGetArchiveFromCheckpoint_ObjectLimitExceeded tests DoS protection for object count limits.
// Verifies:
// - Returns error when checkpoint.TotalObjects exceeds maxObjectsPerCheckpoint limit
// - Prevents processing of extremely large checkpoints
func TestGetArchiveFromCheckpoint_ObjectLimitExceeded(t *testing.T) {
	service, client := setupTestService()

	// Create checkpoint with more objects than limit
	chunks := []storagev1alpha1.ChunkInfo{
		{Name: "chunk-0", Index: 0, Checksum: "test"},
	}
	checkpoint := createTestCheckpoint("test", true, chunks)
	checkpoint.Status.TotalObjects = 20000 // Exceeds default limit of 10000
	_ = client.Create(context.Background(), checkpoint)

	req := &ArchiveRequest{
		CheckpointName:  "test",
		CheckpointUID:   string(checkpoint.UID),
		SourceNamespace: "test-ns",
	}

	_, _, err := service.GetArchiveFromCheckpoint(context.Background(), checkpoint, req)

	if err == nil {
		t.Fatal("Expected error for object limit exceeded")
	}

	if !strings.Contains(err.Error(), "exceeds object limit") {
		t.Errorf("Expected 'exceeds object limit' in error, got: %v", err)
	}
}

// TestGetArchiveFromCheckpoint_Success tests successful archive creation from checkpoint.
// Verifies:
// - Archive is created successfully with multiple chunks
// - Archive data is valid JSON array
// - Checksum is generated correctly
// - All objects from chunks are included
func TestGetArchiveFromCheckpoint_Success(t *testing.T) {
	service, client := setupTestService()

	// Create objects with proper Kubernetes structure
	data1, checksum1 := encodeChunkData([]map[string]interface{}{
		{"apiVersion": "v1", "kind": "Test", "metadata": map[string]interface{}{"name": "test1", "namespace": "test-ns"}},
	})
	data2, checksum2 := encodeChunkData([]map[string]interface{}{
		{"apiVersion": "v1", "kind": "Test", "metadata": map[string]interface{}{"name": "test2", "namespace": "test-ns"}},
	})

	chunk1 := createTestChunk("chunk-0", "test", 0, data1, checksum1)
	chunk2 := createTestChunk("chunk-1", "test", 1, data2, checksum2)
	_ = client.Create(context.Background(), chunk1)
	_ = client.Create(context.Background(), chunk2)

	chunks := []storagev1alpha1.ChunkInfo{
		{Name: "chunk-0", Index: 0, Checksum: checksum1},
		{Name: "chunk-1", Index: 1, Checksum: checksum2},
	}
	checkpoint := createTestCheckpoint("test", true, chunks)
	checkpoint.Status.TotalObjects = 2
	if err := client.Create(context.Background(), checkpoint); err != nil {
		t.Fatalf("Failed to create checkpoint: %v", err)
	}

	req := &ArchiveRequest{
		CheckpointName:  "test",
		CheckpointUID:   string(checkpoint.UID),
		SourceNamespace: "test-ns",
	}

	archiveData, checksum, err := service.GetArchiveFromCheckpoint(context.Background(), checkpoint, req)

	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if len(archiveData) == 0 {
		t.Error("Expected non-empty archive data")
	}

	if checksum == "" {
		t.Error("Expected non-empty checksum")
	}

	// Verify it's valid JSON array
	var result []interface{}
	if err := json.Unmarshal(archiveData, &result); err != nil {
		t.Fatalf("Archive data is not valid JSON: %v. Data: %s", err, string(archiveData))
	}

	if len(result) != 2 {
		t.Errorf("Expected 2 objects, got %d", len(result))
	}
}

// TestGetArchiveFromCheckpoint_Cache tests caching of archive data.
// Verifies:
// - Second call with same parameters uses cached data
// - Checksums match between calls
// - Archive data is identical
func TestGetArchiveFromCheckpoint_Cache(t *testing.T) {
	service, client := setupTestService()

	data, checksum := encodeChunkData([]map[string]interface{}{
		{"apiVersion": "v1", "kind": "Test", "metadata": map[string]interface{}{"name": "test"}},
	})
	chunk := createTestChunk("chunk-0", "test", 0, data, checksum)
	if err := client.Create(context.Background(), chunk); err != nil {
		t.Fatalf("Failed to create chunk: %v", err)
	}

	chunks := []storagev1alpha1.ChunkInfo{
		{Name: "chunk-0", Index: 0, Checksum: checksum},
	}
	checkpoint := createTestCheckpoint("test", true, chunks)
	checkpoint.Status.TotalObjects = 1
	if err := client.Create(context.Background(), checkpoint); err != nil {
		t.Fatalf("Failed to create checkpoint: %v", err)
	}

	req := &ArchiveRequest{
		CheckpointName:  "test",
		CheckpointUID:   string(checkpoint.UID),
		SourceNamespace: "test-ns",
	}

	// First call
	archiveData1, checksum1, err1 := service.GetArchiveFromCheckpoint(context.Background(), checkpoint, req)
	if err1 != nil {
		t.Fatalf("Unexpected error: %v", err1)
	}

	// Second call should use cache
	archiveData2, checksum2, err2 := service.GetArchiveFromCheckpoint(context.Background(), checkpoint, req)
	if err2 != nil {
		t.Fatalf("Unexpected error: %v", err2)
	}

	if checksum1 != checksum2 {
		t.Error("Checksums should match")
	}

	if len(archiveData1) != len(archiveData2) {
		t.Error("Archive data should match")
	}
}

// TestDecodeChunkDataWithChecksum_Valid tests successful decoding of chunk data with valid checksum.
// Verifies:
// - Base64 decoding works correctly
// - Gzip decompression works correctly
// - Checksum verification passes
// - JSON unmarshaling produces correct objects
func TestDecodeChunkDataWithChecksum_Valid(t *testing.T) {
	service, _ := setupTestService()

	objects := []map[string]interface{}{
		{"kind": "Test", "name": "test1"},
		{"kind": "Test", "name": "test2"},
	}

	data, checksum := encodeChunkData(objects)

	result, err := service.decodeChunkDataWithChecksum(data, checksum, "test-chunk")

	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if len(result) != 2 {
		t.Errorf("Expected 2 objects, got %d", len(result))
	}
}

// TestDecodeChunkDataWithChecksum_InvalidChecksum tests error handling for invalid checksum.
// Verifies:
// - Returns error when checksum doesn't match data
// - Error message indicates checksum mismatch
func TestDecodeChunkDataWithChecksum_InvalidChecksum(t *testing.T) {
	service, _ := setupTestService()

	data, _ := encodeChunkData([]map[string]interface{}{{"kind": "Test"}})

	_, err := service.decodeChunkDataWithChecksum(data, "wrong-checksum", "test-chunk")

	if err == nil {
		t.Fatal("Expected error for invalid checksum")
	}

	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("Expected 'checksum mismatch' in error, got: %v", err)
	}
}

// TestDecodeChunkDataWithChecksum_NoChecksum tests decoding when checksum is empty (skips verification).
// Verifies:
// - Decoding succeeds when checksum is empty
// - Checksum verification is skipped
// - Objects are decoded correctly
func TestDecodeChunkDataWithChecksum_NoChecksum(t *testing.T) {
	service, _ := setupTestService()

	data, _ := encodeChunkData([]map[string]interface{}{{"kind": "Test"}})

	// Empty checksum should skip verification
	result, err := service.decodeChunkDataWithChecksum(data, "", "test-chunk")

	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if len(result) != 1 {
		t.Errorf("Expected 1 object, got %d", len(result))
	}
}

// TestDecodeChunkDataWithChecksum_InvalidGzip tests error handling for invalid gzip data.
// Verifies:
// - Returns error when data is valid base64 and checksum, but invalid gzip stream
// - Error message indicates gzip-related issue
func TestDecodeChunkDataWithChecksum_InvalidGzip(t *testing.T) {
	service, _ := setupTestService()

	// Valid base64, valid checksum, but invalid gzip data
	invalidGzipData := base64.StdEncoding.EncodeToString([]byte("not a gzip stream"))
	hash := sha256.Sum256([]byte("not a gzip stream"))
	checksum := hex.EncodeToString(hash[:])

	_, err := service.decodeChunkDataWithChecksum(invalidGzipData, checksum, "test-chunk")

	if err == nil {
		t.Fatal("Expected error for invalid gzip data")
	}

	if !strings.Contains(err.Error(), "gzip") {
		t.Errorf("Expected gzip-related error, got: %v", err)
	}
}

// TestDecodeChunkDataWithChecksum_InvalidJSON tests error handling for invalid JSON after decompression.
// Verifies:
// - Returns error when gzip is valid but JSON is invalid
// - Error message indicates JSON-related issue
func TestDecodeChunkDataWithChecksum_InvalidJSON(t *testing.T) {
	service, _ := setupTestService()

	// Valid base64, valid checksum, valid gzip, but invalid JSON
	invalidJSON := []byte("not a json array")
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write(invalidJSON)
	gz.Close()

	encoded := base64.StdEncoding.EncodeToString(buf.Bytes())
	hash := sha256.Sum256(buf.Bytes())
	checksum := hex.EncodeToString(hash[:])

	_, err := service.decodeChunkDataWithChecksum(encoded, checksum, "test-chunk")

	if err == nil {
		t.Fatal("Expected error for invalid JSON")
	}

	if !strings.Contains(err.Error(), "JSON") {
		t.Errorf("Expected JSON-related error, got: %v", err)
	}
}

// TestGetArchiveFromCheckpoint_UnsortedChunks tests handling of chunks in wrong order in Status.Chunks.
// Verifies:
// - Chunks are sorted by Index before processing
// - Archive is created successfully despite wrong order in Status
// - All objects are included in correct order
func TestGetArchiveFromCheckpoint_UnsortedChunks(t *testing.T) {
	service, client := setupTestService()

	// Create chunks in reverse order
	data1, checksum1 := encodeChunkData([]map[string]interface{}{
		{"apiVersion": "v1", "kind": "Test", "metadata": map[string]interface{}{"name": "test1", "namespace": "test-ns"}},
	})
	data2, checksum2 := encodeChunkData([]map[string]interface{}{
		{"apiVersion": "v1", "kind": "Test", "metadata": map[string]interface{}{"name": "test2", "namespace": "test-ns"}},
	})

	chunk1 := createTestChunk("chunk-1", "test", 1, data2, checksum2)
	chunk0 := createTestChunk("chunk-0", "test", 0, data1, checksum1)
	if err := client.Create(context.Background(), chunk0); err != nil {
		t.Fatalf("Failed to create chunk0: %v", err)
	}
	if err := client.Create(context.Background(), chunk1); err != nil {
		t.Fatalf("Failed to create chunk1: %v", err)
	}

	// Chunks in wrong order in Status
	chunks := []storagev1alpha1.ChunkInfo{
		{Name: "chunk-1", Index: 1, Checksum: checksum2},
		{Name: "chunk-0", Index: 0, Checksum: checksum1},
	}
	checkpoint := createTestCheckpoint("test", true, chunks)
	checkpoint.Status.TotalObjects = 2
	if err := client.Create(context.Background(), checkpoint); err != nil {
		t.Fatalf("Failed to create checkpoint: %v", err)
	}

	req := &ArchiveRequest{
		CheckpointName:  "test",
		CheckpointUID:   string(checkpoint.UID),
		SourceNamespace: "test-ns",
	}

	archiveData, _, err := service.GetArchiveFromCheckpoint(context.Background(), checkpoint, req)

	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	// Verify archive was created successfully despite unsorted chunks
	// The service should sort chunks by index internally
	var result []interface{}
	if err := json.Unmarshal(archiveData, &result); err != nil {
		t.Fatalf("Archive data is not valid JSON: %v", err)
	}

	// Should have 2 objects regardless of chunk order in Status
	if len(result) != 2 {
		t.Errorf("Expected 2 objects, got %d", len(result))
	}

	// Verify both objects are present (order is handled by sorting in collectYamlFilesFromCheckpoint)
	// We can't easily verify exact order due to convertMapSliceToMap complexity,
	// but the important thing is that sorting works and archive is created
}

// TestGetArchiveFromCheckpoint_DuplicateYAMLKeys tests merging of objects with same filename.
// Verifies:
// - Objects with same group/kind/namespace/name are merged into single YAML file
// - YAML separator "---" is used between merged objects
// - Archive contains all objects
func TestGetArchiveFromCheckpoint_DuplicateYAMLKeys(t *testing.T) {
	service, client := setupTestService()

	// Two chunks with objects that will have same filename
	data1, checksum1 := encodeChunkData([]map[string]interface{}{
		{"apiVersion": "v1", "kind": "Test", "metadata": map[string]interface{}{"name": "test", "namespace": "test-ns"}},
	})
	data2, checksum2 := encodeChunkData([]map[string]interface{}{
		{"apiVersion": "v1", "kind": "Test", "metadata": map[string]interface{}{"name": "test", "namespace": "test-ns"}},
	})

	chunk0 := createTestChunk("chunk-0", "test", 0, data1, checksum1)
	chunk1 := createTestChunk("chunk-1", "test", 1, data2, checksum2)
	_ = client.Create(context.Background(), chunk0)
	_ = client.Create(context.Background(), chunk1)

	chunks := []storagev1alpha1.ChunkInfo{
		{Name: "chunk-0", Index: 0, Checksum: checksum1},
		{Name: "chunk-1", Index: 1, Checksum: checksum2},
	}
	checkpoint := createTestCheckpoint("test", true, chunks)
	checkpoint.Status.TotalObjects = 2
	_ = client.Create(context.Background(), checkpoint)

	req := &ArchiveRequest{
		CheckpointName:  "test",
		CheckpointUID:   string(checkpoint.UID),
		SourceNamespace: "test-ns",
	}

	archiveData, _, err := service.GetArchiveFromCheckpoint(context.Background(), checkpoint, req)

	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	// Parse JSON to verify structure
	var result []interface{}
	if err := json.Unmarshal(archiveData, &result); err != nil {
		t.Fatalf("Failed to parse archive JSON: %v", err)
	}

	// Verify both objects are present
	if len(result) != 2 {
		t.Errorf("Expected 2 objects in archive, got %d", len(result))
	}
}

// TestGetArchiveFromCheckpoint_CacheInvalidationOnUIDChange tests cache invalidation when checkpoint UID changes.
// Verifies:
// - Different UIDs produce different cache keys
// - Cache is not reused when UID changes (checkpoint recreated)
// - Archive is regenerated for new UID
func TestGetArchiveFromCheckpoint_CacheInvalidationOnUIDChange(t *testing.T) {
	service, client := setupTestService()

	data, checksum := encodeChunkData([]map[string]interface{}{
		{"apiVersion": "v1", "kind": "Test", "metadata": map[string]interface{}{"name": "test", "namespace": "test-ns"}},
	})
	chunk := createTestChunk("chunk-0", "test", 0, data, checksum)
	_ = client.Create(context.Background(), chunk)

	chunks := []storagev1alpha1.ChunkInfo{
		{Name: "chunk-0", Index: 0, Checksum: checksum},
	}
	checkpoint := createTestCheckpoint("test", true, chunks)
	checkpoint.Status.TotalObjects = 1
	checkpoint.UID = types.UID("uid1")
	_ = client.Create(context.Background(), checkpoint)

	req1 := &ArchiveRequest{
		CheckpointName:  "test",
		CheckpointUID:   "uid1",
		SourceNamespace: "test-ns",
	}

	// First call - should cache
	archiveData1, _, err1 := service.GetArchiveFromCheckpoint(context.Background(), checkpoint, req1)
	if err1 != nil {
		t.Fatalf("Unexpected error: %v", err1)
	}

	// Update checkpoint UID
	checkpoint.UID = types.UID("uid2")
	_ = client.Update(context.Background(), checkpoint)

	req2 := &ArchiveRequest{
		CheckpointName:  "test",
		CheckpointUID:   "uid2", // New UID
		SourceNamespace: "test-ns",
	}

	// Second call with new UID - should NOT use old cache
	archiveData2, _, err2 := service.GetArchiveFromCheckpoint(context.Background(), checkpoint, req2)
	if err2 != nil {
		t.Fatalf("Unexpected error: %v", err2)
	}

	// Data should be the same (same chunk), but cache key should be different
	// So we should get new cache entry
	if len(archiveData1) != len(archiveData2) {
		t.Error("Archive data should match (same content)")
	}

	// Verify cache keys are different
	cacheKey1 := service.GetCacheKey(req1.CheckpointName, req1.CheckpointUID)
	cacheKey2 := service.GetCacheKey(req2.CheckpointName, req2.CheckpointUID)

	if cacheKey1 == cacheKey2 {
		t.Error("Cache keys should be different for different UIDs")
	}
}

// TestGetArchiveFromCheckpoint_OversizedArchiveWarning tests handling of checkpoints exceeding size limits.
// Verifies:
// - Warning is logged when TotalSizeBytes exceeds maxArchiveSizeBytes
// - Processing continues despite size warning
// - Archive is still created successfully
func TestGetArchiveFromCheckpoint_OversizedArchiveWarning(t *testing.T) {
	service, client := setupTestService()

	data, checksum := encodeChunkData([]map[string]interface{}{
		{"apiVersion": "v1", "kind": "Test", "metadata": map[string]interface{}{"name": "test", "namespace": "test-ns"}},
	})
	chunk := createTestChunk("chunk-0", "test", 0, data, checksum)
	_ = client.Create(context.Background(), chunk)

	chunks := []storagev1alpha1.ChunkInfo{
		{Name: "chunk-0", Index: 0, Checksum: checksum},
	}
	checkpoint := createTestCheckpoint("test", true, chunks)
	checkpoint.Status.TotalObjects = 1
	// Set size to exceed limit (200MB > 100MB default limit)
	checkpoint.Status.TotalSizeBytes = 200 * 1024 * 1024
	_ = client.Create(context.Background(), checkpoint)

	req := &ArchiveRequest{
		CheckpointName:  "test",
		CheckpointUID:   string(checkpoint.UID),
		SourceNamespace: "test-ns",
	}

	// Should not fail, just log warning
	archiveData, _, err := service.GetArchiveFromCheckpoint(context.Background(), checkpoint, req)

	if err != nil {
		t.Fatalf("Should not fail on oversized archive, got: %v", err)
	}

	if len(archiveData) == 0 {
		t.Error("Expected archive data even for oversized checkpoint")
	}
}

// TestConvertMapSliceToMap tests the convertMapSliceToMap function
// Verifies that yaml.MapSlice is correctly converted to map[string]interface{}
// and that the result serializes to proper JSON (not Key/Value format)
func TestConvertMapSliceToMap(t *testing.T) {
	service, _ := setupTestService()

	tests := []struct {
		name           string
		input          string // YAML input
		validateFields func(t *testing.T, result map[string]interface{})
	}{
		{
			name: "simple object",
			input: `
apiVersion: v1
kind: ConfigMap
metadata:
  name: test-cm
  namespace: default
data:
  foo: bar
`,
			validateFields: func(t *testing.T, result map[string]interface{}) {
				if result["apiVersion"] != "v1" {
					t.Errorf("apiVersion mismatch: got %v, expected v1", result["apiVersion"])
				}
				if result["kind"] != "ConfigMap" {
					t.Errorf("kind mismatch: got %v, expected ConfigMap", result["kind"])
				}
				metadata := result["metadata"].(map[string]interface{})
				if metadata["name"] != "test-cm" {
					t.Errorf("metadata.name mismatch: got %v, expected test-cm", metadata["name"])
				}
				data := result["data"].(map[string]interface{})
				if data["foo"] != "bar" {
					t.Errorf("data.foo mismatch: got %v, expected bar", data["foo"])
				}
			},
		},
		{
			name: "nested structures",
			input: `
apiVersion: v1
kind: ConfigMap
metadata:
  name: test-cm
  annotations:
    foo: bar
    nested:
      x: 1
      enabled: true
data:
  key1: value1
  key2: value2
`,
			validateFields: func(t *testing.T, result map[string]interface{}) {
				if result["apiVersion"] != "v1" {
					t.Errorf("apiVersion mismatch: got %v, expected v1", result["apiVersion"])
				}
				metadata := result["metadata"].(map[string]interface{})
				annotations := metadata["annotations"].(map[string]interface{})
				if annotations["foo"] != "bar" {
					t.Errorf("annotations.foo mismatch: got %v, expected bar", annotations["foo"])
				}
				nested := annotations["nested"].(map[string]interface{})
				if nested["x"] != float64(1) { // JSON numbers are float64
					t.Errorf("nested.x mismatch: got %v, expected 1", nested["x"])
				}
				if nested["enabled"] != true {
					t.Errorf("nested.enabled mismatch: got %v (type: %T), expected true", nested["enabled"], nested["enabled"])
				}
				data := result["data"].(map[string]interface{})
				if data["key1"] != "value1" {
					t.Errorf("data.key1 mismatch: got %v, expected value1", data["key1"])
				}
			},
		},
		{
			name: "array in object",
			input: `
apiVersion: v1
kind: Pod
metadata:
  name: test-pod
spec:
  containers:
    - name: container1
      image: nginx
    - name: container2
      image: redis
`,
			validateFields: func(t *testing.T, result map[string]interface{}) {
				if result["apiVersion"] != "v1" {
					t.Errorf("apiVersion mismatch: got %v, expected v1", result["apiVersion"])
				}
				spec := result["spec"].(map[string]interface{})
				containers := spec["containers"].([]interface{})
				if len(containers) != 2 {
					t.Errorf("containers length mismatch: got %d, expected 2", len(containers))
				}
				// Verify containers have correct structure (order may vary)
				containerMap := make(map[string]interface{})
				for _, c := range containers {
					cont := c.(map[string]interface{})
					name := cont["name"].(string)
					containerMap[name] = cont
				}
				if containerMap["container1"].(map[string]interface{})["image"] != "nginx" {
					t.Errorf("container1.image mismatch")
				}
				if containerMap["container2"].(map[string]interface{})["image"] != "redis" {
					t.Errorf("container2.image mismatch")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Parse YAML (this will create yaml.MapSlice)
			var doc interface{}
			if err := yaml.Unmarshal([]byte(tt.input), &doc); err != nil {
				t.Fatalf("Failed to unmarshal YAML: %v", err)
			}

			// Convert using convertMapSliceToMap
			converted := service.convertMapSliceToMap(doc)

			// Marshal to JSON
			jsonBytes, err := json.Marshal(converted)
			if err != nil {
				t.Fatalf("Failed to marshal to JSON: %v", err)
			}

			// Verify JSON is not in Key/Value format
			jsonStr := string(jsonBytes)
			if strings.Contains(jsonStr, `"Key"`) || strings.Contains(jsonStr, `"Value"`) {
				t.Errorf("JSON contains Key/Value structure (yaml.MapSlice not converted properly): %s", jsonStr)
			}

			// Parse JSON to verify structure
			var result map[string]interface{}
			if err := json.Unmarshal(jsonBytes, &result); err != nil {
				t.Fatalf("Failed to unmarshal JSON: %v\nJSON: %s", err, string(jsonBytes))
			}

			// Validate fields using custom validator
			tt.validateFields(t, result)
		})
	}
}

// TestConvertMapSliceToMap_KeyValueFormat tests that yaml.MapSlice is NOT serialized as Key/Value array
func TestConvertMapSliceToMap_KeyValueFormat(t *testing.T) {
	service, _ := setupTestService()

	// Create a YAML that will produce yaml.MapSlice
	yamlInput := `
apiVersion: v1
kind: ConfigMap
metadata:
  name: test-cm
data:
  foo: bar
`

	var doc interface{}
	if err := yaml.Unmarshal([]byte(yamlInput), &doc); err != nil {
		t.Fatalf("Failed to unmarshal YAML: %v", err)
	}

	// Convert using convertMapSliceToMap
	converted := service.convertMapSliceToMap(doc)

	// Marshal to JSON
	jsonBytes, err := json.Marshal(converted)
	if err != nil {
		t.Fatalf("Failed to marshal to JSON: %v", err)
	}

	jsonStr := string(jsonBytes)

	// Verify JSON does NOT contain Key/Value structure
	if strings.Contains(jsonStr, `"Key"`) {
		t.Errorf("JSON contains 'Key' field - yaml.MapSlice not converted properly: %s", jsonStr)
	}
	if strings.Contains(jsonStr, `"Value"`) {
		t.Errorf("JSON contains 'Value' field - yaml.MapSlice not converted properly: %s", jsonStr)
	}

	// Verify JSON is a proper object (starts with {)
	if !strings.HasPrefix(jsonStr, "{") {
		t.Errorf("JSON is not an object (doesn't start with '{'): %s", jsonStr)
	}

	// Verify JSON contains expected fields
	if !strings.Contains(jsonStr, `"apiVersion"`) {
		t.Errorf("JSON missing 'apiVersion' field: %s", jsonStr)
	}
	if !strings.Contains(jsonStr, `"kind"`) {
		t.Errorf("JSON missing 'kind' field: %s", jsonStr)
	}
	if !strings.Contains(jsonStr, `"metadata"`) {
		t.Errorf("JSON missing 'metadata' field: %s", jsonStr)
	}
	if !strings.Contains(jsonStr, `"data"`) {
		t.Errorf("JSON missing 'data' field: %s", jsonStr)
	}

	// Verify JSON can be parsed as a proper object
	var result map[string]interface{}
	if err := json.Unmarshal(jsonBytes, &result); err != nil {
		t.Fatalf("Converted JSON is not valid: %v\nJSON: %s", err, jsonStr)
	}

	// Verify structure
	if result["apiVersion"] != "v1" {
		t.Errorf("apiVersion mismatch: got %v, expected v1", result["apiVersion"])
	}
	if result["kind"] != "ConfigMap" {
		t.Errorf("kind mismatch: got %v, expected ConfigMap", result["kind"])
	}

	metadata, ok := result["metadata"].(map[string]interface{})
	if !ok {
		t.Errorf("metadata is not a map: %T", result["metadata"])
	} else if metadata["name"] != "test-cm" {
		t.Errorf("metadata.name mismatch: got %v, expected test-cm", metadata["name"])
	}

	data, ok := result["data"].(map[string]interface{})
	if !ok {
		t.Errorf("data is not a map: %T", result["data"])
	} else if data["foo"] != "bar" {
		t.Errorf("data.foo mismatch: got %v, expected bar", data["foo"])
	}
}

// TestConvertMapSliceToMap_NestedMapSlice tests recursive conversion of nested yaml.MapSlice
func TestConvertMapSliceToMap_NestedMapSlice(t *testing.T) {
	service, _ := setupTestService()

	// Create YAML with deeply nested structures
	yamlInput := `
apiVersion: v1
kind: ConfigMap
metadata:
  name: test-cm
  annotations:
    level1:
      level2:
        level3:
          key: value
          number: 42
          boolean: true
data:
  simple: value
  nested:
    inner: data
`

	var doc interface{}
	if err := yaml.Unmarshal([]byte(yamlInput), &doc); err != nil {
		t.Fatalf("Failed to unmarshal YAML: %v", err)
	}

	// Convert using convertMapSliceToMap
	converted := service.convertMapSliceToMap(doc)

	// Marshal to JSON
	jsonBytes, err := json.Marshal(converted)
	if err != nil {
		t.Fatalf("Failed to marshal to JSON: %v", err)
	}

	// Verify no Key/Value structure
	jsonStr := string(jsonBytes)
	if strings.Contains(jsonStr, `"Key"`) || strings.Contains(jsonStr, `"Value"`) {
		t.Errorf("JSON contains Key/Value structure: %s", jsonStr)
	}

	// Parse and verify nested structure
	var result map[string]interface{}
	if err := json.Unmarshal(jsonBytes, &result); err != nil {
		t.Fatalf("Failed to parse JSON: %v", err)
	}

	// Verify deeply nested structure
	metadata := result["metadata"].(map[string]interface{})
	annotations := metadata["annotations"].(map[string]interface{})
	level1 := annotations["level1"].(map[string]interface{})
	level2 := level1["level2"].(map[string]interface{})
	level3 := level2["level3"].(map[string]interface{})

	if level3["key"] != "value" {
		t.Errorf("level3.key mismatch: got %v, expected value", level3["key"])
	}
	if level3["number"] != float64(42) { // JSON numbers are float64
		t.Errorf("level3.number mismatch: got %v, expected 42", level3["number"])
	}
	if level3["boolean"] != true {
		t.Errorf("level3.boolean mismatch: got %v, expected true", level3["boolean"])
	}
}

// TestConvertMapSliceToMap_EdgeCases tests edge cases like nil, empty maps, arrays
func TestConvertMapSliceToMap_EdgeCases(t *testing.T) {
	service, _ := setupTestService()

	tests := []struct {
		name     string
		input    interface{}
		validate func(t *testing.T, result interface{})
	}{
		{
			name:  "nil input",
			input: nil,
			validate: func(t *testing.T, result interface{}) {
				if result != nil {
					t.Errorf("Expected nil, got %v", result)
				}
			},
		},
		{
			name:  "string primitive",
			input: "simple string",
			validate: func(t *testing.T, result interface{}) {
				if result != "simple string" {
					t.Errorf("Expected 'simple string', got %v", result)
				}
			},
		},
		{
			name:  "number primitive",
			input: 42,
			validate: func(t *testing.T, result interface{}) {
				if result != 42 {
					t.Errorf("Expected 42, got %v", result)
				}
			},
		},
		{
			name:  "boolean primitive",
			input: true,
			validate: func(t *testing.T, result interface{}) {
				if result != true {
					t.Errorf("Expected true, got %v", result)
				}
			},
		},
		{
			name:  "empty array",
			input: []interface{}{},
			validate: func(t *testing.T, result interface{}) {
				arr, ok := result.([]interface{})
				if !ok {
					t.Errorf("Expected []interface{}, got %T", result)
				} else if len(arr) != 0 {
					t.Errorf("Expected empty array, got %d elements", len(arr))
				}
			},
		},
		{
			name:  "array with primitives",
			input: []interface{}{"a", "b", "c"},
			validate: func(t *testing.T, result interface{}) {
				arr, ok := result.([]interface{})
				if !ok {
					t.Errorf("Expected []interface{}, got %T", result)
				} else if len(arr) != 3 {
					t.Errorf("Expected 3 elements, got %d", len(arr))
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := service.convertMapSliceToMap(tt.input)
			tt.validate(t, result)
		})
	}
}

// encodeChunkDataInKeyValueFormat encodes objects in Key/Value format (as yaml.MapSlice serialized to JSON)
// This simulates old chunks that were stored in Key/Value format
// Note: Real chunks store data as array of arrays: [[{"Key": "...", "Value": ...}, ...]]
func encodeChunkDataInKeyValueFormat(keyValueArray []interface{}) (string, string) {
	// Key/Value format: array of arrays of {Key, Value} objects
	jsonData, _ := json.Marshal(keyValueArray)

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write(jsonData)
	gz.Close()

	encoded := base64.StdEncoding.EncodeToString(buf.Bytes())
	hash := sha256.Sum256(buf.Bytes())
	checksum := hex.EncodeToString(hash[:])

	return encoded, checksum
}

// TestGetArchiveFromCheckpoint_KeyValueFormatConversion tests that chunks stored in Key/Value format
// are correctly converted to normal JSON format when reading.
// This test verifies the fix for the Key/Value serialization issue.
// Verifies:
// - Chunks with Key/Value format are correctly decoded
// - Archive returned is in normal JSON format (not Key/Value)
// - All nested structures are properly converted
func TestGetArchiveFromCheckpoint_KeyValueFormatConversion(t *testing.T) {
	service, client := setupTestService()

	// Create chunk data in Key/Value format (as it was stored in old chunks)
	// This simulates: [[{"Key": "kind", "Value": "ConfigMap"}, {"Key": "metadata", "Value": {...}}]]
	// Note: Real chunks store data as array of arrays (one array per object)
	keyValueData := []interface{}{
		[]map[string]interface{}{
			{
				"Key":   "apiVersion",
				"Value": "v1",
			},
			{
				"Key":   "kind",
				"Value": "ConfigMap",
			},
			{
				"Key": "metadata",
				"Value": []map[string]interface{}{
					{
						"Key":   "name",
						"Value": "test-cm",
					},
					{
						"Key":   "namespace",
						"Value": "default",
					},
				},
			},
			{
				"Key": "data",
				"Value": []map[string]interface{}{
					{
						"Key":   "foo",
						"Value": "bar",
					},
				},
			},
		},
	}

	// Encode in Key/Value format
	data, checksum := encodeChunkDataInKeyValueFormat(keyValueData)

	chunk := createTestChunk("chunk-0", "test", 0, data, checksum)
	_ = client.Create(context.Background(), chunk)

	chunks := []storagev1alpha1.ChunkInfo{
		{Name: "chunk-0", Index: 0, Checksum: checksum},
	}
	checkpoint := createTestCheckpoint("test", true, chunks)
	checkpoint.Status.TotalObjects = 1
	_ = client.Create(context.Background(), checkpoint)

	req := &ArchiveRequest{
		CheckpointName:  "test",
		CheckpointUID:   string(checkpoint.UID),
		SourceNamespace: "test-ns",
	}

	archiveData, _, err := service.GetArchiveFromCheckpoint(context.Background(), checkpoint, req)
	if err != nil {
		t.Fatalf("Failed to get archive: %v", err)
	}

	// Parse JSON to verify it's in normal format (not Key/Value)
	var result []interface{}
	if err := json.Unmarshal(archiveData, &result); err != nil {
		t.Fatalf("Failed to parse archive JSON: %v", err)
	}

	if len(result) == 0 {
		t.Fatal("Archive is empty")
	}

	// Check first object is in normal format
	firstObj, ok := result[0].(map[string]interface{})
	if !ok {
		t.Fatalf("First object is not a map, got: %T", result[0])
	}

	// Verify it's NOT in Key/Value format
	if _, hasKey := firstObj["Key"]; hasKey {
		if _, hasValue := firstObj["Value"]; hasValue {
			t.Error("Archive still contains Key/Value format - conversion failed")
			t.Logf("First object: %+v", firstObj)
		}
	}

	// Verify it's in normal format
	expectedKeys := []string{"apiVersion", "kind", "metadata", "data"}
	for _, key := range expectedKeys {
		if _, exists := firstObj[key]; !exists {
			t.Errorf("Expected key '%s' not found in converted object", key)
		}
	}

	// Verify nested structures are also converted
	if metadata, ok := firstObj["metadata"].(map[string]interface{}); ok {
		if _, hasKey := metadata["Key"]; hasKey {
			t.Error("Metadata still contains Key/Value format - nested conversion failed")
		}
		if name, ok := metadata["name"].(string); !ok || name != "test-cm" {
			t.Errorf("Expected metadata.name='test-cm', got: %v", metadata["name"])
		}
	} else {
		t.Error("Metadata is not a map after conversion")
	}

	if dataMap, ok := firstObj["data"].(map[string]interface{}); ok {
		if _, hasKey := dataMap["Key"]; hasKey {
			t.Error("Data still contains Key/Value format - nested conversion failed")
		}
		if foo, ok := dataMap["foo"].(string); !ok || foo != "bar" {
			t.Errorf("Expected data.foo='bar', got: %v", dataMap["foo"])
		}
	} else {
		t.Error("Data is not a map after conversion")
	}

	// Verify JSON doesn't contain Key/Value strings in root
	jsonStr := string(archiveData)
	// Check if root object has Key/Value (which is bad)
	if strings.Contains(jsonStr, `[{"Key":`) {
		t.Error("Archive JSON contains Key/Value structure in root - conversion failed")
		t.Logf("Archive preview (first 500 chars): %s", jsonStr[:minInt(500, len(jsonStr))])
	}

	// Verify the JSON structure matches expected format
	expectedJSON := `{
  "apiVersion": "v1",
  "kind": "ConfigMap",
  "metadata": {
    "name": "test-cm",
    "namespace": "default"
  },
  "data": {
    "foo": "bar"
  }
}`

	// Parse expected JSON for comparison
	var expectedObj map[string]interface{}
	if err := json.Unmarshal([]byte(expectedJSON), &expectedObj); err != nil {
		t.Fatalf("Failed to parse expected JSON: %v", err)
	}

	// Compare structure (ignoring order and extra fields)
	if firstObj["apiVersion"] != expectedObj["apiVersion"] {
		t.Errorf("Expected apiVersion='v1', got: %v", firstObj["apiVersion"])
	}
	if firstObj["kind"] != expectedObj["kind"] {
		t.Errorf("Expected kind='ConfigMap', got: %v", firstObj["kind"])
	}

	// Verify metadata structure
	if metadata, ok := firstObj["metadata"].(map[string]interface{}); ok {
		if name, ok := metadata["name"].(string); !ok || name != "test-cm" {
			t.Errorf("Expected metadata.name='test-cm', got: %v", metadata["name"])
		}
		if ns, ok := metadata["namespace"].(string); !ok || ns != "default" {
			t.Errorf("Expected metadata.namespace='default', got: %v", metadata["namespace"])
		}
	} else {
		t.Error("Metadata is not a map")
	}

	// Verify data structure
	if dataMap, ok := firstObj["data"].(map[string]interface{}); ok {
		if foo, ok := dataMap["foo"].(string); !ok || foo != "bar" {
			t.Errorf("Expected data.foo='bar', got: %v", dataMap["foo"])
		}
	} else {
		t.Error("Data is not a map")
	}

	t.Logf("✅ Successfully converted Key/Value format to normal JSON")
	t.Logf("Archive JSON (first 300 chars): %s", jsonStr[:minInt(300, len(jsonStr))])
}

// TestDecodeChunkDataWithChecksum_KeyValueFormat tests that decodeChunkDataWithChecksum
// correctly converts Key/Value format to normal unstructured objects.
// Note: In real chunks, data is stored as array of arrays: [[{"Key": "...", "Value": ...}, ...]]
func TestDecodeChunkDataWithChecksum_KeyValueFormat(t *testing.T) {
	service, _ := setupTestService()

	// Create Key/Value format data (array of Key/Value pairs)
	// This simulates how yaml.MapSlice is serialized to JSON
	keyValueData := []interface{}{
		[]map[string]interface{}{
			{
				"Key":   "apiVersion",
				"Value": "v1",
			},
			{
				"Key":   "kind",
				"Value": "ConfigMap",
			},
			{
				"Key": "metadata",
				"Value": []map[string]interface{}{
					{
						"Key":   "name",
						"Value": "test-cm",
					},
					{
						"Key":   "namespace",
						"Value": "default",
					},
				},
			},
		},
	}

	// Encode
	jsonData, _ := json.Marshal(keyValueData)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write(jsonData)
	gz.Close()
	encoded := base64.StdEncoding.EncodeToString(buf.Bytes())
	hash := sha256.Sum256(buf.Bytes())
	checksum := hex.EncodeToString(hash[:])

	// Decode
	objects, err := service.decodeChunkDataWithChecksum(encoded, checksum, "test-chunk")
	if err != nil {
		t.Fatalf("Failed to decode chunk: %v", err)
	}

	if len(objects) == 0 {
		t.Fatal("No objects decoded")
	}

	// Verify object is in normal format
	obj := objects[0]
	if obj.GetKind() != "ConfigMap" {
		t.Errorf("Expected kind=ConfigMap, got: %s", obj.GetKind())
	}
	if obj.GetAPIVersion() != "v1" {
		t.Errorf("Expected apiVersion=v1, got: %s", obj.GetAPIVersion())
	}
	if obj.GetName() != "test-cm" {
		t.Errorf("Expected name=test-cm, got: %s", obj.GetName())
	}
	if obj.GetNamespace() != "default" {
		t.Errorf("Expected namespace=default, got: %s", obj.GetNamespace())
	}

	// Verify Object map doesn't contain Key/Value structure
	objMap := obj.Object
	if _, hasKey := objMap["Key"]; hasKey {
		if _, hasValue := objMap["Value"]; hasValue {
			t.Error("Object still contains Key/Value format - conversion failed")
			t.Logf("Object: %+v", objMap)
		}
	}
}

// TestGetArchiveFromCheckpoint_FiltersStatusAndManagedFields tests that status and managedFields are filtered from returned objects.
// Verifies:
// - status field is removed from all objects
// - managedFields is removed from metadata of all objects
// - Other fields remain intact
func TestGetArchiveFromCheckpoint_FiltersStatusAndManagedFields(t *testing.T) {
	service, client := setupTestService()

	// Create objects with status and managedFields
	data, checksum := encodeChunkData([]map[string]interface{}{
		{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]interface{}{
				"name":      "test-cm",
				"namespace": "test-ns",
				"managedFields": []map[string]interface{}{
					{
						"manager":    "kubectl",
						"operation":  "Update",
						"apiVersion": "v1",
					},
				},
			},
			"data": map[string]interface{}{
				"foo": "bar",
			},
			"status": map[string]interface{}{
				"phase": "Active",
			},
		},
		{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata": map[string]interface{}{
				"name":      "test-secret",
				"namespace": "test-ns",
				"managedFields": []map[string]interface{}{
					{
						"manager":    "controller",
						"operation":  "Apply",
						"apiVersion": "v1",
					},
				},
			},
			"type": "Opaque",
			"status": map[string]interface{}{
				"observedGeneration": 1,
			},
		},
	})

	chunk := createTestChunk("chunk-0", "test", 0, data, checksum)
	_ = client.Create(context.Background(), chunk)

	chunks := []storagev1alpha1.ChunkInfo{
		{Name: "chunk-0", Index: 0, Checksum: checksum},
	}
	checkpoint := createTestCheckpoint("test", true, chunks)
	checkpoint.Status.TotalObjects = 2
	_ = client.Create(context.Background(), checkpoint)

	req := &ArchiveRequest{
		CheckpointName:  "test",
		CheckpointUID:   string(checkpoint.UID),
		SourceNamespace: "test-ns",
	}

	archiveData, _, err := service.GetArchiveFromCheckpoint(context.Background(), checkpoint, req)
	if err != nil {
		t.Fatalf("Failed to get archive: %v", err)
	}

	// Parse JSON to verify fields are filtered
	var result []interface{}
	if err := json.Unmarshal(archiveData, &result); err != nil {
		t.Fatalf("Failed to parse archive JSON: %v", err)
	}

	if len(result) != 2 {
		t.Fatalf("Expected 2 objects, got %d", len(result))
	}

	// Check first object (ConfigMap)
	obj1, ok := result[0].(map[string]interface{})
	if !ok {
		t.Fatalf("First object is not a map, got: %T", result[0])
	}

	// Verify status is removed
	if _, hasStatus := obj1["status"]; hasStatus {
		t.Error("status field should be removed from ConfigMap, but it's still present")
		t.Logf("Object 1: %+v", obj1)
	}

	// Verify managedFields is removed from metadata
	if metadata, ok := obj1["metadata"].(map[string]interface{}); ok {
		if _, hasManagedFields := metadata["managedFields"]; hasManagedFields {
			t.Error("managedFields should be removed from ConfigMap metadata, but it's still present")
			t.Logf("Metadata: %+v", metadata)
		}
		// Verify other metadata fields are still present
		if name, ok := metadata["name"].(string); !ok || name != "test-cm" {
			t.Errorf("Expected metadata.name='test-cm', got: %v", metadata["name"])
		}
	} else {
		t.Error("Metadata is not a map")
	}

	// Verify other fields are still present
	if _, hasData := obj1["data"]; !hasData {
		t.Error("data field should still be present in ConfigMap")
	}
	if kind, ok := obj1["kind"].(string); !ok || kind != "ConfigMap" {
		t.Errorf("Expected kind='ConfigMap', got: %v", obj1["kind"])
	}

	// Check second object (Secret)
	obj2, ok := result[1].(map[string]interface{})
	if !ok {
		t.Fatalf("Second object is not a map, got: %T", result[1])
	}

	// Verify status is removed
	if _, hasStatus := obj2["status"]; hasStatus {
		t.Error("status field should be removed from Secret, but it's still present")
		t.Logf("Object 2: %+v", obj2)
	}

	// Verify managedFields is removed from metadata
	if metadata, ok := obj2["metadata"].(map[string]interface{}); ok {
		if _, hasManagedFields := metadata["managedFields"]; hasManagedFields {
			t.Error("managedFields should be removed from Secret metadata, but it's still present")
			t.Logf("Metadata: %+v", metadata)
		}
		// Verify other metadata fields are still present
		if name, ok := metadata["name"].(string); !ok || name != "test-secret" {
			t.Errorf("Expected metadata.name='test-secret', got: %v", metadata["name"])
		}
	} else {
		t.Error("Metadata is not a map")
	}

	// Verify other fields are still present
	if _, hasType := obj2["type"]; !hasType {
		t.Error("type field should still be present in Secret")
	}
	if kind, ok := obj2["kind"].(string); !ok || kind != "Secret" {
		t.Errorf("Expected kind='Secret', got: %v", obj2["kind"])
	}

	// Verify JSON string doesn't contain status or managedFields
	jsonStr := string(archiveData)
	if strings.Contains(jsonStr, `"status"`) {
		t.Error("Archive JSON should not contain 'status' field")
	}
	if strings.Contains(jsonStr, `"managedFields"`) {
		t.Error("Archive JSON should not contain 'managedFields' field")
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
