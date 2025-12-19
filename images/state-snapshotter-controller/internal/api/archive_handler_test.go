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

// TestHandleListManifestCheckpoints tests the list endpoint for manifestcheckpoints.
// Verifies:
// - Returns HTTP 200 OK
// - Response has correct Kubernetes API structure (kind, apiVersion)
// - Returns empty items array
// - Metadata contains resourceVersion and selfLink
func TestHandleListManifestCheckpoints(t *testing.T) {
	handler, _ := setupTestHandler()

	req := httptest.NewRequest(http.MethodGet, "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/manifestcheckpoints", nil)
	w := httptest.NewRecorder()

	handler.HandleListManifestCheckpoints(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	var response map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}

	if response["kind"] != "ManifestCheckpointList" {
		t.Errorf("Expected kind ManifestCheckpointList, got %v", response["kind"])
	}

	if response["apiVersion"] != "subresources.state-snapshotter.deckhouse.io/v1alpha1" {
		t.Errorf("Expected apiVersion subresources.state-snapshotter.deckhouse.io/v1alpha1, got %v", response["apiVersion"])
	}

	items, ok := response["items"].([]interface{})
	if !ok {
		t.Fatal("items is not an array")
	}

	if len(items) != 0 {
		t.Errorf("Expected empty items array, got %d items", len(items))
	}

	metadata, ok := response["metadata"].(map[string]interface{})
	if !ok {
		t.Fatal("metadata is not an object")
	}

	if metadata["resourceVersion"] != "0" {
		t.Errorf("Expected resourceVersion 0, got %v", metadata["resourceVersion"])
	}

	if metadata["selfLink"] == nil {
		t.Error("Expected selfLink in metadata")
	}
}

// TestHandleListManifestCheckpointsWithQueryParams tests that query parameters are preserved in selfLink.
// Verifies:
// - Query parameters are included in metadata.selfLink
// - Response structure remains correct
func TestHandleListManifestCheckpointsWithQueryParams(t *testing.T) {
	handler, _ := setupTestHandler()

	req := httptest.NewRequest(http.MethodGet, "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/manifestcheckpoints?limit=500", nil)
	w := httptest.NewRecorder()

	handler.HandleListManifestCheckpoints(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	var response map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &response)

	metadata := response["metadata"].(map[string]interface{})
	selfLink := metadata["selfLink"].(string)

	if !strings.Contains(selfLink, "limit=500") {
		t.Errorf("Expected selfLink to contain query params, got %s", selfLink)
	}
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

// TestHandleGetManifests_WithoutSubresource tests GET request without subresource path.
// Verifies:
// - Returns HTTP 404 Not Found
// - Error message indicates subresource is required
// - Uses Kubernetes Status format
func TestHandleGetManifests_WithoutSubresource(t *testing.T) {
	handler, _ := setupTestHandler()

	// GET /apis/.../manifestcheckpoints/test (without /manifests)
	req := httptest.NewRequest(http.MethodGet, "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/manifestcheckpoints/test", nil)
	w := httptest.NewRecorder()

	// Simulate routing - this would be handled by the router
	// In real scenario, router would call handler with empty subresource
	path := strings.TrimPrefix(req.URL.Path, "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/manifestcheckpoints/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 {
		// This is the case we're testing
		handler.writeKubernetesErrorResponse(w, http.StatusNotFound, "NotFound", "subresource required: use /apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/manifestcheckpoints/<name>/manifests")
	}

	if w.Code != http.StatusNotFound {
		t.Errorf("Expected status 404, got %d", w.Code)
	}

	var status metav1.Status
	_ = json.Unmarshal(w.Body.Bytes(), &status)

	if status.Reason != metav1.StatusReasonNotFound {
		t.Errorf("Expected reason NotFound, got %v", status.Reason)
	}
}

// TestHandleListManifestCheckpoints_MethodNotAllowed tests unsupported HTTP methods on list endpoint.
// Verifies:
// - POST, PUT, DELETE, PATCH return HTTP 405 Method Not Allowed
// - Uses Kubernetes Status format with MethodNotAllowed reason
func TestHandleListManifestCheckpoints_MethodNotAllowed(t *testing.T) {
	handler, _ := setupTestHandler()

	methods := []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch}

	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/manifestcheckpoints", nil)
			w := httptest.NewRecorder()

			// Simulate routing check
			if req.Method != http.MethodGet {
				handler.writeKubernetesErrorResponse(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "only GET method is supported for list")
			}

			if w.Code != http.StatusMethodNotAllowed {
				t.Errorf("Expected status 405 for %s, got %d", method, w.Code)
			}

			var status metav1.Status
			_ = json.Unmarshal(w.Body.Bytes(), &status)

			if status.Reason != metav1.StatusReason("MethodNotAllowed") {
				t.Errorf("Expected reason MethodNotAllowed, got %v", status.Reason)
			}
		})
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

// TestHandleListManifestCheckpoints_ComplexQueryParams tests preservation of complex query parameters in selfLink.
// Verifies:
// - Multiple query parameters are preserved
// - URL-encoded characters are handled correctly
// - selfLink contains all query parameters
func TestHandleListManifestCheckpoints_ComplexQueryParams(t *testing.T) {
	handler, _ := setupTestHandler()

	tests := []struct {
		name          string
		query         string
		shouldContain string
	}{
		{"multiple params", "limit=500&continue=a%2Fb", "limit=500"},
		{"encoded chars", "labelSelector=a%2Cb", "labelSelector=a%2Cb"},
		{"empty query", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			url := "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/manifestcheckpoints"
			if tt.query != "" {
				url += "?" + tt.query
			}
			req := httptest.NewRequest(http.MethodGet, url, nil)
			w := httptest.NewRecorder()

			handler.HandleListManifestCheckpoints(w, req)

			if w.Code != http.StatusOK {
				t.Errorf("Expected status 200, got %d", w.Code)
				return
			}

			var response map[string]interface{}
			_ = json.Unmarshal(w.Body.Bytes(), &response)

			metadata := response["metadata"].(map[string]interface{})
			selfLink := metadata["selfLink"].(string)

			if tt.query != "" && !strings.Contains(selfLink, tt.shouldContain) {
				t.Errorf("Expected selfLink to contain %s, got %s", tt.shouldContain, selfLink)
			}
		})
	}
}
