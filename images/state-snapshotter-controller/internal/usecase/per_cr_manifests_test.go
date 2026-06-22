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

	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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
	if _, ok := m["namespace"]; ok {
		t.Fatalf("single-node output must be namespace-relative, got %v", m)
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
