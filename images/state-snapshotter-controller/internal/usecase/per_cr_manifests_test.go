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
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

// perCRNodeFixture installs a root SnapshotContent (with one child) whose own MCP holds a single object,
// plus a child SnapshotContent+MCP holding a different object. Returns the configured service.
func perCRNodeFixture(t *testing.T) *AggregatedNamespaceManifests {
	t.Helper()
	scheme := aggManifestTestScheme(t)
	log, _ := logger.NewLogger("error")
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	arch := NewArchiveService(cl, cl, log)

	ctx := context.Background()
	for _, tc := range []struct {
		cpName, kind, name string
	}{
		{"mcp-root", "ConfigMap", "root-own"},
		{"mcp-child", "Secret", "child-own"},
	} {
		d, cs := aggManifestEncodeChunk([]map[string]interface{}{
			{"apiVersion": "v1", "kind": tc.kind, "metadata": map[string]interface{}{"name": tc.name, "namespace": "ns1"}},
		})
		ch := aggManifestCreateChunk("ch-"+tc.cpName, tc.cpName, d, cs)
		_ = cl.Create(ctx, ch)
		mcp := aggManifestReadyMCP(tc.cpName, "ns1", []ssv1alpha1.ChunkInfo{{Name: ch.Name, Index: 0, Checksum: cs}}, 1)
		_ = cl.Create(ctx, mcp)
	}
	_ = cl.Create(ctx, aggManifestContent("child-content", "mcp-child"))
	_ = cl.Create(ctx, aggManifestContent("root-content", "mcp-root", "child-content"))
	_ = cl.Create(ctx, aggManifestNS("root-content"))

	return NewAggregatedNamespaceManifests(cl, arch, nil)
}

func TestBuildSingleNodeJSON_OwnNodeOnly_NoSubtree(t *testing.T) {
	agg := perCRNodeFixture(t)

	// Per-CR download returns only the node's own MCP objects; the child subtree is NOT walked.
	raw, err := agg.BuildSingleNodeJSON(context.Background(), "root-content")
	if err != nil {
		t.Fatalf("BuildSingleNodeJSON: %v", err)
	}
	var arr []map[string]interface{}
	if err := json.Unmarshal(raw, &arr); err != nil {
		t.Fatal(err)
	}
	if len(arr) != 1 {
		t.Fatalf("want exactly the node's own object (no subtree), got %d: %v", len(arr), arr)
	}
	m := arr[0]["metadata"].(map[string]interface{})
	if m["name"] != "root-own" {
		t.Fatalf("want own object root-own, got %v", m["name"])
	}
	if ns, ok := m["namespace"].(string); !ok || ns != "ns1" {
		t.Fatalf("single-node output must preserve metadata.namespace (raw), got %v", m["namespace"])
	}
}

func TestBuildSingleNodeJSONForRootSnapshot_ResolvesBoundContent(t *testing.T) {
	agg := perCRNodeFixture(t)
	raw, err := agg.BuildSingleNodeJSONForRootSnapshot(context.Background(), aggManifestTestSnapNamespace, aggManifestTestSnapName)
	if err != nil {
		t.Fatalf("BuildSingleNodeJSONForRootSnapshot: %v", err)
	}
	var arr []map[string]interface{}
	_ = json.Unmarshal(raw, &arr)
	if len(arr) != 1 || arr[0]["metadata"].(map[string]interface{})["name"] != "root-own" {
		t.Fatalf("want only the root node's own object, got %v", arr)
	}
}

func TestBuildSingleNodeJSONFromContent_ChildNodeOwnOnly(t *testing.T) {
	agg := perCRNodeFixture(t)
	raw, err := agg.BuildSingleNodeJSONFromContent(context.Background(), "child-content")
	if err != nil {
		t.Fatalf("BuildSingleNodeJSONFromContent: %v", err)
	}
	var arr []map[string]interface{}
	_ = json.Unmarshal(raw, &arr)
	if len(arr) != 1 || arr[0]["metadata"].(map[string]interface{})["name"] != "child-own" {
		t.Fatalf("want only the child node's own object, got %v", arr)
	}
}

// TestBuildSingleNodeJSON_ImportReconstructedObjectsKept verifies that objects stored in an import-
// reconstructed MCP (including those without metadata.namespace) are returned verbatim on download.
func TestBuildSingleNodeJSON_ImportReconstructedObjectsKept(t *testing.T) {
	scheme := aggManifestTestScheme(t)
	log, _ := logger.NewLogger("error")
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	arch := NewArchiveService(cl, cl, log)
	ctx := context.Background()

	// Stored verbatim as the import upload persists it.
	d, cs := aggManifestEncodeChunk([]map[string]interface{}{
		{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": map[string]interface{}{"name": "imported-pvc"}},
	})
	ch := aggManifestCreateChunk("ch-mcp-import", "mcp-import", d, cs)
	if err := cl.Create(ctx, ch); err != nil {
		t.Fatal(err)
	}
	mcp := aggManifestReadyMCP("mcp-import", "ns1", []ssv1alpha1.ChunkInfo{{Name: ch.Name, Index: 0, Checksum: cs}}, 1)
	if err := cl.Create(ctx, mcp); err != nil {
		t.Fatal(err)
	}
	if err := cl.Create(ctx, aggManifestContent("import-content", "mcp-import")); err != nil {
		t.Fatal(err)
	}

	agg := NewAggregatedNamespaceManifests(cl, arch, nil)
	raw, err := agg.BuildSingleNodeJSONFromContent(ctx, "import-content")
	if err != nil {
		t.Fatalf("BuildSingleNodeJSONFromContent: %v", err)
	}
	var arr []map[string]interface{}
	if err := json.Unmarshal(raw, &arr); err != nil {
		t.Fatal(err)
	}
	if len(arr) != 1 {
		t.Fatalf("import-reconstructed object must be kept, got %d: %v", len(arr), arr)
	}
	if arr[0]["kind"] != "PersistentVolumeClaim" {
		t.Fatalf("want PersistentVolumeClaim, got %v", arr[0]["kind"])
	}
}

// TestBuildSingleNodeJSON_DownloadUploadRoundTripPreservesRawFields verifies that manifests-download
// output can be stored verbatim via import upload (ReconstructManifestCheckpoint) and read back with
// namespace and status intact — the shape DataImport needs for PVC parameter extraction.
func TestBuildSingleNodeJSON_DownloadUploadRoundTripPreservesRawFields(t *testing.T) {
	scheme := aggManifestTestScheme(t)
	log, _ := logger.NewLogger("error")
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&ssv1alpha1.ManifestCheckpoint{}).
		Build()
	arch := NewArchiveService(cl, cl, log)
	ctx := context.Background()

	pvcObj := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]interface{}{
			"name":      "disk-pvc",
			"namespace": "ns1",
		},
		"spec": map[string]interface{}{
			"storageClassName": "local",
			"resources": map[string]interface{}{
				"requests": map[string]interface{}{"storage": "1Gi"},
			},
		},
		"status": map[string]interface{}{
			"capacity": map[string]interface{}{"storage": "1Gi"},
		},
	}
	d, cs := aggManifestEncodeChunk([]map[string]interface{}{pvcObj})
	ch := aggManifestCreateChunk("ch-mcp-capture", "mcp-capture", d, cs)
	if err := cl.Create(ctx, ch); err != nil {
		t.Fatal(err)
	}
	mcp := aggManifestReadyMCP("mcp-capture", "ns1", []ssv1alpha1.ChunkInfo{{Name: ch.Name, Index: 0, Checksum: cs}}, 1)
	if err := cl.Create(ctx, mcp); err != nil {
		t.Fatal(err)
	}
	if err := cl.Create(ctx, aggManifestContent("capture-content", "mcp-capture")); err != nil {
		t.Fatal(err)
	}

	agg := NewAggregatedNamespaceManifests(cl, arch, nil)
	downloaded, err := agg.BuildSingleNodeJSONFromContent(ctx, "capture-content")
	if err != nil {
		t.Fatalf("download: %v", err)
	}

	importMCPName := "mcp-import-roundtrip"
	if err := ReconstructManifestCheckpoint(ctx, cl, importMCPName, "ns1", nil, downloaded); err != nil {
		t.Fatalf("reconstruct import MCP: %v", err)
	}
	if err := cl.Create(ctx, aggManifestContent("import-content", importMCPName)); err != nil {
		t.Fatal(err)
	}

	roundTripped, err := agg.BuildSingleNodeJSONFromContent(ctx, "import-content")
	if err != nil {
		t.Fatalf("download after upload: %v", err)
	}
	var arr []map[string]interface{}
	if err := json.Unmarshal(roundTripped, &arr); err != nil {
		t.Fatal(err)
	}
	if len(arr) != 1 {
		t.Fatalf("want 1 object after round-trip, got %d", len(arr))
	}
	meta := arr[0]["metadata"].(map[string]interface{})
	if meta["namespace"] != "ns1" {
		t.Fatalf("namespace must survive round-trip, got %v", meta["namespace"])
	}
	status, ok := arr[0]["status"].(map[string]interface{})
	if !ok {
		t.Fatal("status must survive round-trip")
	}
	capacity, ok := status["capacity"].(map[string]interface{})
	if !ok || capacity["storage"] != "1Gi" {
		t.Fatalf("status.capacity.storage must survive round-trip, got %v", status["capacity"])
	}
}

func TestBuildSingleNodeJSON_Errors(t *testing.T) {
	scheme := aggManifestTestScheme(t)
	log, _ := logger.NewLogger("error")
	ctx := context.Background()

	t.Run("content not found 404", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).Build()
		agg := NewAggregatedNamespaceManifests(cl, NewArchiveService(cl, cl, log), nil)
		_, err := agg.BuildSingleNodeJSON(ctx, "missing")
		assertAggStatus(t, err, http.StatusNotFound)
	})

	t.Run("empty manifestCheckpointName 409", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).Build()
		agg := NewAggregatedNamespaceManifests(cl, NewArchiveService(cl, cl, log), nil)
		content := aggManifestContent("c", "")
		_ = cl.Create(ctx, content)
		_, err := agg.BuildSingleNodeJSON(ctx, "c")
		assertAggStatus(t, err, http.StatusConflict)
	})

	t.Run("empty name 400", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).Build()
		agg := NewAggregatedNamespaceManifests(cl, NewArchiveService(cl, cl, log), nil)
		_, err := agg.BuildSingleNodeJSON(ctx, "")
		assertAggStatus(t, err, http.StatusBadRequest)
	})
}

func assertAggStatus(t *testing.T, err error, want int) {
	t.Helper()
	var st *AggregatedStatusError
	if !errors.As(err, &st) {
		t.Fatalf("expected AggregatedStatusError, got %T: %v", err, err)
	}
	if st.HTTPStatus != want {
		t.Fatalf("expected HTTP %d, got %d (%s)", want, st.HTTPStatus, st.Message)
	}
}
