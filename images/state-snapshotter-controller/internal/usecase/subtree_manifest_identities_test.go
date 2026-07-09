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
	"net/http"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk"
)

// subtreeObj builds one captured manifest object with an explicit identity (apiVersion v1).
func subtreeObj(kind, name, uid string) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "v1",
		"kind":       kind,
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": "ns1",
			"uid":       uid,
		},
	}
}

// subtreeCreateNode installs a Ready MCP (+chunk) holding objs and a SnapshotContent named contentName
// pointing at that MCP with the given child content refs.
func subtreeCreateNode(ctx context.Context, t *testing.T, cl client.Client, cpName, contentName string, objs []map[string]interface{}, children ...string) {
	t.Helper()
	d, cs := aggManifestEncodeChunk(objs)
	ch := aggManifestCreateChunk("ch-"+cpName, cpName, d, cs)
	if err := cl.Create(ctx, ch); err != nil {
		t.Fatalf("create chunk %s: %v", cpName, err)
	}
	mcp := aggManifestReadyMCP(cpName, "ns1", []ssv1alpha1.ChunkInfo{{Name: ch.Name, Index: 0, Checksum: cs}}, len(objs))
	if err := cl.Create(ctx, mcp); err != nil {
		t.Fatalf("create mcp %s: %v", cpName, err)
	}
	if err := cl.Create(ctx, aggManifestContent(contentName, cpName, children...)); err != nil {
		t.Fatalf("create content %s: %v", contentName, err)
	}
}

func newSubtreeAgg(t *testing.T) (*AggregatedNamespaceManifests, client.Client, context.Context) {
	t.Helper()
	scheme := aggManifestTestScheme(t)
	log, _ := logger.NewLogger("error")
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	return NewAggregatedNamespaceManifests(cl, NewArchiveService(cl, cl, log), nil), cl, context.Background()
}

func decodeIdentities(t *testing.T, raw []byte) []snapshotsdk.SubtreeManifestIdentity {
	t.Helper()
	var resp snapshotsdk.SubtreeManifestIdentitiesResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode identities: %v", err)
	}
	return resp.Identities
}

func identityNames(ids []snapshotsdk.SubtreeManifestIdentity) map[string]struct{} {
	out := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		out[id.Name] = struct{}{}
	}
	return out
}

// TestBuildSubtreeManifestIdentities_RecursesWholeSubtree verifies the endpoint returns the union of every
// node's captured identities across a 3-level tree (root -> child -> grandchild), not just the addressed
// node — the recursion the SDK exclude computation relies on.
func TestBuildSubtreeManifestIdentities_RecursesWholeSubtree(t *testing.T) {
	agg, cl, ctx := newSubtreeAgg(t)

	subtreeCreateNode(ctx, t, cl, "mcp-gc", "gc-content", []map[string]interface{}{subtreeObj("Secret", "gc-own", "uid-gc")})
	subtreeCreateNode(ctx, t, cl, "mcp-child", "child-content", []map[string]interface{}{subtreeObj("ConfigMap", "child-own", "uid-child")}, "gc-content")
	subtreeCreateNode(ctx, t, cl, "mcp-root", "root-content", []map[string]interface{}{subtreeObj("ServiceAccount", "root-own", "uid-root")}, "child-content")

	raw, err := agg.BuildSubtreeManifestIdentities(ctx, "root-content")
	if err != nil {
		t.Fatalf("BuildSubtreeManifestIdentities: %v", err)
	}
	ids := decodeIdentities(t, raw)
	if len(ids) != 3 {
		t.Fatalf("want 3 identities across the subtree, got %d: %v", len(ids), ids)
	}
	names := identityNames(ids)
	for _, want := range []string{"root-own", "child-own", "gc-own"} {
		if _, ok := names[want]; !ok {
			t.Fatalf("subtree identity set missing %q: %v", want, names)
		}
	}
}

// TestBuildSubtreeManifestIdentities_IdentityFields verifies each identity carries the full
// apiVersion/kind/namespace/name/uid projection consumers key on.
func TestBuildSubtreeManifestIdentities_IdentityFields(t *testing.T) {
	agg, cl, ctx := newSubtreeAgg(t)
	subtreeCreateNode(ctx, t, cl, "mcp-root", "root-content", []map[string]interface{}{subtreeObj("ConfigMap", "cm-1", "uid-cm-1")})

	raw, err := agg.BuildSubtreeManifestIdentities(ctx, "root-content")
	if err != nil {
		t.Fatalf("BuildSubtreeManifestIdentities: %v", err)
	}
	ids := decodeIdentities(t, raw)
	if len(ids) != 1 {
		t.Fatalf("want 1 identity, got %d", len(ids))
	}
	got := ids[0]
	want := snapshotsdk.SubtreeManifestIdentity{APIVersion: "v1", Kind: "ConfigMap", Namespace: "ns1", Name: "cm-1", UID: "uid-cm-1"}
	if got != want {
		t.Fatalf("identity = %#v, want %#v", got, want)
	}
}

// TestBuildSubtreeManifestIdentities_FailClosedNotReadyMCP verifies a not-Ready MCP anywhere in the
// subtree (here a descendant) fails the whole request with 409 — never a partial list.
func TestBuildSubtreeManifestIdentities_FailClosedNotReadyMCP(t *testing.T) {
	agg, cl, ctx := newSubtreeAgg(t)

	// Child MCP is NOT ready; build it by hand (subtreeCreateNode only makes Ready MCPs).
	d, cs := aggManifestEncodeChunk([]map[string]interface{}{subtreeObj("ConfigMap", "child-own", "uid-child")})
	ch := aggManifestCreateChunk("ch-mcp-child", "mcp-child", d, cs)
	if err := cl.Create(ctx, ch); err != nil {
		t.Fatal(err)
	}
	notReady := aggManifestNotReadyMCP("mcp-child", "ns1", []ssv1alpha1.ChunkInfo{{Name: ch.Name, Index: 0, Checksum: cs}}, 1)
	if err := cl.Create(ctx, notReady); err != nil {
		t.Fatal(err)
	}
	if err := cl.Create(ctx, aggManifestContent("child-content", "mcp-child")); err != nil {
		t.Fatal(err)
	}
	subtreeCreateNode(ctx, t, cl, "mcp-root", "root-content", []map[string]interface{}{subtreeObj("ServiceAccount", "root-own", "uid-root")}, "child-content")

	_, err := agg.BuildSubtreeManifestIdentities(ctx, "root-content")
	assertAggStatus(t, err, http.StatusConflict)
}

// TestBuildSubtreeManifestIdentities_FailClosedEmptyCheckpointName verifies a subtree node whose content
// has no persisted MCP yet (empty manifestCheckpointName) fails the whole request with 409.
func TestBuildSubtreeManifestIdentities_FailClosedEmptyCheckpointName(t *testing.T) {
	agg, cl, ctx := newSubtreeAgg(t)

	if err := cl.Create(ctx, aggManifestContent("child-content", "")); err != nil {
		t.Fatal(err)
	}
	subtreeCreateNode(ctx, t, cl, "mcp-root", "root-content", []map[string]interface{}{subtreeObj("ServiceAccount", "root-own", "uid-root")}, "child-content")

	_, err := agg.BuildSubtreeManifestIdentities(ctx, "root-content")
	assertAggStatus(t, err, http.StatusConflict)
}

// TestBuildSubtreeManifestIdentities_FailClosedDoubleCapture verifies the same object captured by two
// different nodes (a double-capture the wave barrier prevents) fails the request with 409.
func TestBuildSubtreeManifestIdentities_FailClosedDoubleCapture(t *testing.T) {
	agg, cl, ctx := newSubtreeAgg(t)

	dup := subtreeObj("ConfigMap", "shared", "uid-shared")
	subtreeCreateNode(ctx, t, cl, "mcp-child", "child-content", []map[string]interface{}{dup})
	subtreeCreateNode(ctx, t, cl, "mcp-root", "root-content", []map[string]interface{}{dup}, "child-content")

	_, err := agg.BuildSubtreeManifestIdentities(ctx, "root-content")
	assertAggStatus(t, err, http.StatusConflict)
}

// TestBuildSubtreeManifestIdentities_EmptyName verifies an empty content name is a 400.
func TestBuildSubtreeManifestIdentities_EmptyName(t *testing.T) {
	agg, _, ctx := newSubtreeAgg(t)
	_, err := agg.BuildSubtreeManifestIdentities(ctx, "")
	assertAggStatus(t, err, http.StatusBadRequest)
}

// TestBuildSubtreeManifestIdentities_ContentNotFound verifies a missing root content is a 404.
func TestBuildSubtreeManifestIdentities_ContentNotFound(t *testing.T) {
	agg, _, ctx := newSubtreeAgg(t)
	_, err := agg.BuildSubtreeManifestIdentities(ctx, "missing")
	assertAggStatus(t, err, http.StatusNotFound)
}

// TestBuildSubtreeManifestIdentities_MissingChildContentNotFound verifies a dangling child content ref
// (referenced but absent) fails the request 404 rather than silently dropping that subtree.
func TestBuildSubtreeManifestIdentities_MissingChildContentNotFound(t *testing.T) {
	agg, cl, ctx := newSubtreeAgg(t)
	subtreeCreateNode(ctx, t, cl, "mcp-root", "root-content", []map[string]interface{}{subtreeObj("ServiceAccount", "root-own", "uid-root")}, "ghost-content")

	_, err := agg.BuildSubtreeManifestIdentities(ctx, "root-content")
	assertAggStatus(t, err, http.StatusNotFound)
}
