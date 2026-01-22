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

package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

func setupTestHandler() (*ArchiveHandler, client.Client) {
	scheme := runtime.NewScheme()
	_ = storagev1alpha1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	log, _ := logger.NewLogger("error")
	archiveService := usecase.NewArchiveService(fakeClient, fakeClient, log)
	handler := NewArchiveHandler(fakeClient, archiveService, log)

	return handler, fakeClient
}

func setupTestServer(handler *ArchiveHandler) *httptest.Server {
	mux := http.NewServeMux()
	handler.SetupRoutes(mux)
	return httptest.NewServer(mux)
}

//nolint:unparam // name parameter is kept for test flexibility
func createTestCheckpoint(name string, ready bool) *storagev1alpha1.ManifestCheckpoint {
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
			Chunks: []storagev1alpha1.ChunkInfo{
				{Name: "chunk-0", Index: 0, Checksum: "test-checksum-0"},
				{Name: "chunk-1", Index: 1, Checksum: "test-checksum-1"},
			},
			TotalObjects:   2,
			TotalSizeBytes: 1000,
		},
	}

	if ready {
		meta.SetStatusCondition(&checkpoint.Status.Conditions, metav1.Condition{
			Type:   storagev1alpha1.ManifestCheckpointConditionTypeReady,
			Status: metav1.ConditionTrue,
			Reason: storagev1alpha1.ManifestCheckpointConditionReasonCompleted,
		})
	} else {
		meta.SetStatusCondition(&checkpoint.Status.Conditions, metav1.Condition{
			Type:   storagev1alpha1.ManifestCheckpointConditionTypeReady,
			Status: metav1.ConditionFalse,
			Reason: "Failed", // For test purposes, using string literal
		})
	}

	return checkpoint
}


// TestHandleGetManifests_NotFound tests handling of non-existent checkpoint.
// Verifies:
// - Returns HTTP 404 Not Found
// - Response uses Kubernetes Status format
// - Status.Details is populated for NotFound errors
func TestHandleGetManifests_NotFound(t *testing.T) {
	handler, _ := setupTestHandler()

	req := httptest.NewRequest(http.MethodGet, "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/manifestcheckpoints/nonexistent/manifests", nil)
	w := httptest.NewRecorder()

	handler.HandleGetManifests(w, req, "nonexistent")

	if w.Code != http.StatusNotFound {
		t.Errorf("Expected status 404, got %d", w.Code)
	}

	var status metav1.Status
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatalf("Failed to unmarshal status: %v", err)
	}

	if status.Kind != "Status" {
		t.Errorf("Expected kind Status, got %s", status.Kind)
	}

	if status.Status != metav1.StatusFailure {
		t.Errorf("Expected status Failure, got %s", status.Status)
	}

	if status.Reason != metav1.StatusReasonNotFound {
		t.Errorf("Expected reason NotFound, got %v", status.Reason)
	}

	if status.Details == nil {
		t.Error("Expected Details in NotFound response")
	}
}

// TestHandleGetManifests_NotReady tests handling of checkpoint that is not ready.
// Verifies:
// - Returns HTTP 409 Conflict
// - Error message indicates checkpoint is not ready
// - Uses Kubernetes Status format
func TestHandleGetManifests_NotReady(t *testing.T) {
	handler, client := setupTestHandler()

	checkpoint := createTestCheckpoint("test-checkpoint", false)
	_ = client.Create(context.Background(), checkpoint)

	req := httptest.NewRequest(http.MethodGet, "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/manifestcheckpoints/test-checkpoint/manifests", nil)
	w := httptest.NewRecorder()

	handler.HandleGetManifests(w, req, "test-checkpoint")

	if w.Code != http.StatusConflict {
		t.Errorf("Expected status 409, got %d", w.Code)
	}

	var status metav1.Status
	_ = json.Unmarshal(w.Body.Bytes(), &status)

	if status.Reason != metav1.StatusReason("Conflict") {
		t.Errorf("Expected reason Conflict, got %v", status.Reason)
	}

	if !strings.Contains(status.Message, "not ready") {
		t.Errorf("Expected message to contain 'not ready', got %s", status.Message)
	}
}

// TestHandleGetManifests_WithGzip tests gzip compression of response.
// Verifies:
// - Returns HTTP 200 OK
// - Content-Encoding header is set to "gzip"
// - Response can be decompressed and decoded as JSON
// - Archive data is correctly returned
func TestHandleGetManifests_WithGzip(t *testing.T) {
	handler, client := setupTestHandler()

	// Create test chunks with proper encoding first
	data1, checksum1 := encodeTestChunkData([]map[string]interface{}{
		{"apiVersion": "v1", "kind": "Test", "metadata": map[string]interface{}{"name": "test1"}},
	})
	data2, checksum2 := encodeTestChunkData([]map[string]interface{}{
		{"apiVersion": "v1", "kind": "Test", "metadata": map[string]interface{}{"name": "test2"}},
	})

	chunk0 := &storagev1alpha1.ManifestCheckpointContentChunk{
		ObjectMeta: metav1.ObjectMeta{Name: "chunk-0"},
		Spec: storagev1alpha1.ManifestCheckpointContentChunkSpec{
			CheckpointName: "test-checkpoint",
			Index:          0,
			Data:           data1,
			Checksum:       checksum1,
			ObjectsCount:   1,
		},
	}
	chunk1 := &storagev1alpha1.ManifestCheckpointContentChunk{
		ObjectMeta: metav1.ObjectMeta{Name: "chunk-1"},
		Spec: storagev1alpha1.ManifestCheckpointContentChunkSpec{
			CheckpointName: "test-checkpoint",
			Index:          1,
			Data:           data2,
			Checksum:       checksum2,
			ObjectsCount:   1,
		},
	}
	_ = client.Create(context.Background(), chunk0)
	_ = client.Create(context.Background(), chunk1)

	// Create checkpoint with correct checksums
	checkpoint := createTestCheckpoint("test-checkpoint", true)
	checkpoint.Status.Chunks = []storagev1alpha1.ChunkInfo{
		{Name: "chunk-0", Index: 0, Checksum: checksum1},
		{Name: "chunk-1", Index: 1, Checksum: checksum2},
	}
	checkpoint.Status.TotalObjects = 2
	_ = client.Create(context.Background(), checkpoint)

	req := httptest.NewRequest(http.MethodGet, "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/manifestcheckpoints/test-checkpoint/manifests", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()

	handler.HandleGetManifests(w, req, "test-checkpoint")

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d. Body: %s", w.Code, w.Body.String())
		return
	}

	if w.Header().Get("Content-Encoding") != "gzip" {
		t.Error("Expected Content-Encoding: gzip header")
	}

	// Decompress and verify
	gr, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatalf("Failed to create gzip reader: %v", err)
	}
	defer gr.Close()

	var result []interface{}
	if err := json.NewDecoder(gr).Decode(&result); err != nil {
		t.Fatalf("Failed to decode JSON: %v", err)
	}

	if len(result) == 0 {
		t.Error("Expected non-empty result")
	}
}

func TestHandleGetManifests_TrailingSlash(t *testing.T) {
	handler, client := setupTestHandler()
	server := setupTestServer(handler)
	defer server.Close()

	data1, checksum1 := encodeTestChunkData([]map[string]interface{}{
		{"apiVersion": "v1", "kind": "Test", "metadata": map[string]interface{}{"name": "test1"}},
	})
	chunk0 := &storagev1alpha1.ManifestCheckpointContentChunk{
		ObjectMeta: metav1.ObjectMeta{Name: "chunk-0"},
		Spec: storagev1alpha1.ManifestCheckpointContentChunkSpec{
			CheckpointName: "test-checkpoint",
			Index:          0,
			Data:           data1,
			Checksum:       checksum1,
			ObjectsCount:   1,
		},
	}
	_ = client.Create(context.Background(), chunk0)

	checkpoint := createTestCheckpoint("test-checkpoint", true)
	checkpoint.Status.Chunks = []storagev1alpha1.ChunkInfo{
		{Name: "chunk-0", Index: 0, Checksum: checksum1},
	}
	checkpoint.Status.TotalObjects = 1
	_ = client.Create(context.Background(), checkpoint)

	resp, err := http.Get(server.URL + "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/manifestcheckpoints/test-checkpoint/manifests/")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Expected status 200, got %d", resp.StatusCode)
	}
}

func TestHandleAPIResourceListDiscovery_SubresourcesOnly(t *testing.T) {
	handler, _ := setupTestHandler()
	req := httptest.NewRequest(http.MethodGet, "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1", nil)
	w := httptest.NewRecorder()

	handler.HandleAPIResourceListDiscovery(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected status 200, got %d", w.Code)
	}

	var list metav1.APIResourceList
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("Failed to unmarshal APIResourceList: %v", err)
	}

	resources := map[string]metav1.APIResource{}
	for _, res := range list.APIResources {
		resources[res.Name] = res
	}

	if _, exists := resources["manifestcheckpoints"]; exists {
		t.Fatalf("manifestcheckpoints should not be in discovery")
	}
	if _, exists := resources["snapshots"]; exists {
		t.Fatalf("snapshots should not be in discovery")
	}
	if _, exists := resources["manifestcheckpoints/manifests"]; !exists {
		t.Fatalf("manifestcheckpoints/manifests missing in discovery")
	}
	if _, exists := resources["snapshots/manifests"]; !exists {
		t.Fatalf("snapshots/manifests missing in discovery")
	}
	if _, exists := resources["snapshots/manifests-with-data-restoration"]; !exists {
		t.Fatalf("snapshots/manifests-with-data-restoration missing in discovery")
	}
}

// TestHandleGetManifests_WithoutGzip tests uncompressed JSON response.
// Verifies:
// - Returns HTTP 200 OK
// - No Content-Encoding header when gzip not requested
// - Response is valid JSON array
func TestHandleGetManifests_WithoutGzip(t *testing.T) {
	handler, client := setupTestHandler()

	// Create test chunk with proper encoding first
	data1, checksum1 := encodeTestChunkData([]map[string]interface{}{
		{"apiVersion": "v1", "kind": "Test", "metadata": map[string]interface{}{"name": "test1"}},
	})

	chunk0 := &storagev1alpha1.ManifestCheckpointContentChunk{
		ObjectMeta: metav1.ObjectMeta{Name: "chunk-0"},
		Spec: storagev1alpha1.ManifestCheckpointContentChunkSpec{
			CheckpointName: "test-checkpoint",
			Index:          0,
			Data:           data1,
			Checksum:       checksum1,
			ObjectsCount:   1,
		},
	}
	_ = client.Create(context.Background(), chunk0)

	// Create checkpoint with correct checksum
	checkpoint := createTestCheckpoint("test-checkpoint", true)
	checkpoint.Status.Chunks = []storagev1alpha1.ChunkInfo{
		{Name: "chunk-0", Index: 0, Checksum: checksum1},
	}
	checkpoint.Status.TotalObjects = 1
	_ = client.Create(context.Background(), checkpoint)

	req := httptest.NewRequest(http.MethodGet, "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/manifestcheckpoints/test-checkpoint/manifests", nil)
	w := httptest.NewRecorder()

	handler.HandleGetManifests(w, req, "test-checkpoint")

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d. Body: %s", w.Code, w.Body.String())
		return
	}

	if w.Header().Get("Content-Encoding") == "gzip" {
		t.Error("Expected no Content-Encoding header when gzip not requested")
	}

	var result []interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("Failed to unmarshal JSON: %v. Body: %s", err, w.Body.String())
	}

	if len(result) == 0 {
		t.Error("Expected non-empty result")
	}
}

// TestShouldCompressResponse tests gzip compression detection from Accept-Encoding header.
// Verifies:
// - Detects "gzip" in various Accept-Encoding formats
// - Handles quality values (q=0.5)
// - Returns false when gzip not present
func TestShouldCompressResponse(t *testing.T) {
	tests := []struct {
		name           string
		acceptEncoding string
		shouldCompress bool
	}{
		{"gzip present", "gzip", true},
		{"gzip with others", "gzip, deflate, br", true},
		{"gzip with quality", "gzip;q=0.5, identity;q=0.1", true},
		{"no gzip", "deflate", false},
		{"empty", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/test", nil)
			req.Header.Set("Accept-Encoding", tt.acceptEncoding)

			result := shouldCompressResponse(req)
			if result != tt.shouldCompress {
				t.Errorf("shouldCompressResponse() = %v, want %v", result, tt.shouldCompress)
			}
		})
	}
}

// Helper function to encode test chunk data (base64(gzip(json[])))
func encodeTestChunkData(objects []map[string]interface{}) (string, string) {
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

// TestHandleManifestCheckpoints_NoSubresource tests GET without subresource.
func TestHandleManifestCheckpoints_NoSubresource(t *testing.T) {
	handler, client := setupTestHandler()
	_ = client.Create(context.Background(), createTestCheckpoint("cp-1", true))
	server := setupTestServer(handler)
	defer server.Close()

	resp, err := http.Get(server.URL + "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/manifestcheckpoints/cp-1")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestHandleManifestCheckpoints_NoSubresourceTrailingSlash(t *testing.T) {
	handler, client := setupTestHandler()
	_ = client.Create(context.Background(), createTestCheckpoint("cp-1", true))
	server := setupTestServer(handler)
	defer server.Close()

	resp, err := http.Get(server.URL + "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/manifestcheckpoints/cp-1/")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

// TestHandleManifestCheckpoints_UnknownSubresource tests unknown subresource routing.
func TestHandleManifestCheckpoints_UnknownSubresource(t *testing.T) {
	handler, client := setupTestHandler()
	_ = client.Create(context.Background(), createTestCheckpoint("cp-1", true))
	server := setupTestServer(handler)
	defer server.Close()

	resp, err := http.Get(server.URL + "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/manifestcheckpoints/cp-1/unknown")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("Expected status 404, got %d", resp.StatusCode)
	}
}


// TestHandleGetManifests_ReturnsJSON tests that /manifests endpoint always returns JSON.
// Verifies:
// - /manifests endpoint always returns JSON
// - Content-Type is application/json
func TestHandleGetManifests_ReturnsJSON(t *testing.T) {
	handler, client := setupTestHandler()

	// Create test chunks with proper encoding
	data1, checksum1 := encodeTestChunkData([]map[string]interface{}{
		{"apiVersion": "v1", "kind": "Test", "metadata": map[string]interface{}{"name": "test1"}},
	})
	chunk1 := &storagev1alpha1.ManifestCheckpointContentChunk{
		ObjectMeta: metav1.ObjectMeta{Name: "chunk-0"},
		Spec: storagev1alpha1.ManifestCheckpointContentChunkSpec{
			CheckpointName: "test-checkpoint",
			Index:          0,
			Data:           data1,
			Checksum:       checksum1,
			ObjectsCount:   1,
		},
	}
	_ = client.Create(context.Background(), chunk1)

	chunks := []storagev1alpha1.ChunkInfo{
		{Name: "chunk-0", Index: 0, Checksum: checksum1},
	}
	checkpoint := createTestCheckpoint("test-checkpoint", true)
	checkpoint.Status.Chunks = chunks
	checkpoint.Status.TotalObjects = 1
	_ = client.Create(context.Background(), checkpoint)

	req := httptest.NewRequest(http.MethodGet, "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/manifestcheckpoints/test-checkpoint/manifests", nil)
	w := httptest.NewRecorder()

	handler.HandleGetManifests(w, req, "test-checkpoint")

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d. Body: %s", w.Code, w.Body.String())
		return
	}

	contentType := w.Header().Get("Content-Type")
	if !strings.Contains(contentType, "application/json") {
		t.Errorf("Expected Content-Type application/json, got %s", contentType)
	}
}

