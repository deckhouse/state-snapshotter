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

package api //nolint:revive

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1snap "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/config"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

// ── helpers ──────────────────────────────────────────────────────────────────

func buildImportTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = ssv1alpha1.AddToScheme(s)
	_ = storagev1alpha1snap.AddToScheme(s)
	return s
}

func newImportFakeClient() client.Client {
	return fake.NewClientBuilder().
		WithScheme(buildImportTestScheme()).
		WithStatusSubresource(
			&ssv1alpha1.ManifestCheckpoint{},
			&storagev1alpha1snap.SnapshotContent{},
			&storagev1alpha1snap.Snapshot{},
		).
		Build()
}

func newImportHandler(c client.Client) *ImportHandler {
	log, _ := logger.NewLogger("error")
	cfg := &config.Options{MaxChunkSizeBytes: 800_000}
	return NewImportHandler(c, cfg, log)
}

func gzipJSON(t *testing.T, v interface{}) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, _ = w.Write(raw)
	_ = w.Close()
	return buf.Bytes()
}

// ── topoSort ─────────────────────────────────────────────────────────────────

func TestTopoSort_SingleNode(t *testing.T) {
	nodeByID := map[string]*ImportBuildNode{
		"root": {NodeID: "root"},
	}
	order, err := topoSort("root", nodeByID)
	if err != nil {
		t.Fatalf("topoSort error: %v", err)
	}
	if len(order) != 1 || order[0] != "root" {
		t.Fatalf("unexpected order: %v", order)
	}
}

func TestTopoSort_LinearChain(t *testing.T) {
	nodeByID := map[string]*ImportBuildNode{
		"root":  {NodeID: "root", ChildNodeIDs: []string{"child"}},
		"child": {NodeID: "child"},
	}
	order, err := topoSort("root", nodeByID)
	if err != nil {
		t.Fatalf("topoSort error: %v", err)
	}
	// child must appear before root
	if len(order) != 2 || order[0] != "child" || order[1] != "root" {
		t.Fatalf("unexpected order: %v", order)
	}
}

func TestTopoSort_CycleDetected(t *testing.T) {
	nodeByID := map[string]*ImportBuildNode{
		"a": {NodeID: "a", ChildNodeIDs: []string{"b"}},
		"b": {NodeID: "b", ChildNodeIDs: []string{"a"}},
	}
	_, err := topoSort("a", nodeByID)
	if err == nil {
		t.Fatal("expected cycle error, got nil")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected 'cycle' in error, got: %v", err)
	}
}

// ── importDataRefBindings ─────────────────────────────────────────────────────

func TestImportDataRefBindings_Valid(t *testing.T) {
	refs := []ImportBuildDataRef{
		{
			TargetUID:                 "uid-1",
			OriginalKind:              "PersistentVolumeClaim",
			OriginalAPIVersion:        "v1",
			OriginalName:              "pvc-a",
			OriginalNamespace:         "ns",
			VolumeSnapshotContentName: "snapcontent-abc",
		},
	}
	bindings, err := importDataRefBindings(refs)
	if err != nil {
		t.Fatalf("importDataRefBindings error: %v", err)
	}
	if len(bindings) != 1 {
		t.Fatalf("expected 1 binding, got %d", len(bindings))
	}
	b := bindings[0]
	if b.TargetUID != "uid-1" {
		t.Errorf("unexpected TargetUID: %s", b.TargetUID)
	}
	if b.Artifact.Kind != "VolumeSnapshotContent" {
		t.Errorf("unexpected artifact kind: %s", b.Artifact.Kind)
	}
	if b.Artifact.APIVersion != "snapshot.storage.k8s.io/v1" {
		t.Errorf("unexpected artifact apiVersion: %s", b.Artifact.APIVersion)
	}
	if b.Artifact.Name != "snapcontent-abc" {
		t.Errorf("unexpected artifact name: %s", b.Artifact.Name)
	}
}

func TestImportDataRefBindings_MissingVSCName(t *testing.T) {
	refs := []ImportBuildDataRef{{TargetUID: "uid-1", VolumeSnapshotContentName: ""}}
	_, err := importDataRefBindings(refs)
	if err == nil {
		t.Fatal("expected error for missing VSC name")
	}
}

func TestImportDataRefBindings_MissingTargetUID(t *testing.T) {
	refs := []ImportBuildDataRef{{TargetUID: "", VolumeSnapshotContentName: "sc"}}
	_, err := importDataRefBindings(refs)
	if err == nil {
		t.Fatal("expected error for missing targetUID")
	}
}

// ── ensureVSCRetainPolicy ─────────────────────────────────────────────────────

func TestEnsureVSCRetainPolicy_Patches(t *testing.T) {
	ctx := context.Background()

	vsc := &unstructured.Unstructured{}
	vsc.SetGroupVersionKind(vscGVK)
	vsc.SetName("snap-content-1")
	_ = unstructured.SetNestedField(vsc.Object, "Delete", "spec", "deletionPolicy")

	scheme := runtime.NewScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(vsc).Build()

	if err := ensureVSCRetainPolicy(ctx, c, "snap-content-1"); err != nil {
		t.Fatalf("ensureVSCRetainPolicy error: %v", err)
	}

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(vscGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: "snap-content-1"}, got); err != nil {
		t.Fatalf("get VSC: %v", err)
	}
	policy, _, _ := unstructured.NestedString(got.Object, "spec", "deletionPolicy")
	if policy != "Retain" {
		t.Errorf("expected deletionPolicy=Retain, got %q", policy)
	}
}

func TestEnsureVSCRetainPolicy_Idempotent(t *testing.T) {
	ctx := context.Background()

	vsc := &unstructured.Unstructured{}
	vsc.SetGroupVersionKind(vscGVK)
	vsc.SetName("snap-content-2")
	_ = unstructured.SetNestedField(vsc.Object, "Retain", "spec", "deletionPolicy")

	scheme := runtime.NewScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(vsc).Build()

	// Call twice — no error.
	for i := 0; i < 2; i++ {
		if err := ensureVSCRetainPolicy(ctx, c, "snap-content-2"); err != nil {
			t.Fatalf("call %d: ensureVSCRetainPolicy error: %v", i, err)
		}
	}
}

func TestEnsureVSCRetainPolicy_NotFound(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	// VSC does not exist → should return nil (not ready yet, caller retries).
	if err := ensureVSCRetainPolicy(ctx, c, "missing-vsc"); err != nil {
		t.Fatalf("expected nil error for not-found VSC, got: %v", err)
	}
}

// ── buildSnapshotTreeImpl ─────────────────────────────────────────────────────

func TestBuildSnapshotTreeImpl_SingleNode(t *testing.T) {
	ctx := context.Background()
	c := newImportFakeClient()

	req := ImportBuildRequest{
		Namespace:    "test-ns",
		SnapshotName: "my-snapshot",
		RootNodeID:   "root",
		Nodes: []ImportBuildNode{
			{NodeID: "root", ManifestCheckpointName: "import-mcp-abc"},
		},
	}

	result, err := buildSnapshotTreeImpl(ctx, c, req)
	if err != nil {
		t.Fatalf("buildSnapshotTreeImpl error: %v", err)
	}
	if result.SnapshotName != "my-snapshot" {
		t.Errorf("unexpected snapshot name: %s", result.SnapshotName)
	}
	if result.RootSnapshotContentName == "" {
		t.Errorf("expected non-empty root content name")
	}

	// Verify Snapshot was created with existingContentRef.
	snap := &storagev1alpha1snap.Snapshot{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "test-ns", Name: "my-snapshot"}, snap); err != nil {
		t.Fatalf("get Snapshot: %v", err)
	}
	if snap.Spec.ExistingContentRef == nil {
		t.Fatal("Snapshot.Spec.ExistingContentRef is nil")
	}
	if snap.Spec.ExistingContentRef.Name != result.RootSnapshotContentName {
		t.Errorf("existingContentRef.Name=%q != rootContentName=%q",
			snap.Spec.ExistingContentRef.Name, result.RootSnapshotContentName)
	}

	// Verify SnapshotContent was created.
	ct := &storagev1alpha1snap.SnapshotContent{}
	if err := c.Get(ctx, client.ObjectKey{Name: result.RootSnapshotContentName}, ct); err != nil {
		t.Fatalf("get SnapshotContent: %v", err)
	}
	if ct.Labels[labelImportMode] != labelValueTrue {
		t.Errorf("expected import-mode label, got: %v", ct.Labels)
	}
}

func TestBuildSnapshotTreeImpl_TreeWithChild(t *testing.T) {
	ctx := context.Background()
	c := newImportFakeClient()

	req := ImportBuildRequest{
		Namespace:    "test-ns",
		SnapshotName: "tree-snap",
		RootNodeID:   "root",
		Nodes: []ImportBuildNode{
			{NodeID: "root", ChildNodeIDs: []string{"child"}, ManifestCheckpointName: "mcp-root"},
			{NodeID: "child"},
		},
	}

	result, err := buildSnapshotTreeImpl(ctx, c, req)
	if err != nil {
		t.Fatalf("buildSnapshotTreeImpl error: %v", err)
	}

	// Root SnapshotContent must exist.
	rootCt := &storagev1alpha1snap.SnapshotContent{}
	if err := c.Get(ctx, client.ObjectKey{Name: result.RootSnapshotContentName}, rootCt); err != nil {
		t.Fatalf("get root SnapshotContent: %v", err)
	}

	// Child SnapshotContent must exist with ownerRef → root.
	childName := importContentName("test-ns", "tree-snap", "child")
	childCt := &storagev1alpha1snap.SnapshotContent{}
	if err := c.Get(ctx, client.ObjectKey{Name: childName}, childCt); err != nil {
		t.Fatalf("get child SnapshotContent: %v", err)
	}

	foundOwner := false
	for _, ref := range childCt.OwnerReferences {
		if ref.Kind == "SnapshotContent" && ref.Name == result.RootSnapshotContentName {
			foundOwner = true
			break
		}
	}
	if !foundOwner {
		t.Errorf("child SnapshotContent missing ownerRef to root SnapshotContent; refs=%v", childCt.OwnerReferences)
	}
}

func TestBuildSnapshotTreeImpl_Idempotent(t *testing.T) {
	ctx := context.Background()
	c := newImportFakeClient()

	req := ImportBuildRequest{
		Namespace:    "test-ns",
		SnapshotName: "idem-snap",
		RootNodeID:   "root",
		Nodes: []ImportBuildNode{
			{NodeID: "root"},
		},
	}

	r1, err := buildSnapshotTreeImpl(ctx, c, req)
	if err != nil {
		t.Fatalf("first call error: %v", err)
	}
	r2, err := buildSnapshotTreeImpl(ctx, c, req)
	if err != nil {
		t.Fatalf("second call error: %v", err)
	}
	if r1.RootSnapshotContentName != r2.RootSnapshotContentName {
		t.Errorf("root content name changed across calls: %s != %s", r1.RootSnapshotContentName, r2.RootSnapshotContentName)
	}
}

func TestBuildSnapshotTreeImpl_ValidationErrors(t *testing.T) {
	ctx := context.Background()
	c := newImportFakeClient()

	tests := []struct {
		name string
		req  ImportBuildRequest
	}{
		{"empty nodes", ImportBuildRequest{Namespace: "ns", SnapshotName: "s", RootNodeID: "r", Nodes: nil}},
		{"empty rootNodeId", ImportBuildRequest{Namespace: "ns", SnapshotName: "s", Nodes: []ImportBuildNode{{NodeID: "r"}}}},
		{"rootNodeId not in nodes", ImportBuildRequest{Namespace: "ns", SnapshotName: "s", RootNodeID: "missing", Nodes: []ImportBuildNode{{NodeID: "r"}}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildSnapshotTreeImpl(ctx, c, tc.req)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

// ── HandleImportManifests (HTTP handler) ──────────────────────────────────────

func TestHandleImportManifests_CreatesReady(t *testing.T) {
	ctx := context.Background()
	c := newImportFakeClient()
	h := newImportHandler(c)

	body := gzipJSON(t, []interface{}{map[string]interface{}{"kind": "Pod"}})
	req := httptest.NewRequest(http.MethodPut, "/", bytes.NewReader(body))
	req.Header.Set("Content-Encoding", "gzip")
	rr := httptest.NewRecorder()

	h.HandleImportManifests(rr, req, "test-ns", "snap1", "node-abc")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	mcpName, ok := resp["manifestCheckpointName"]
	if !ok || mcpName == "" {
		t.Fatalf("missing manifestCheckpointName in response: %v", resp)
	}

	// Verify MCP exists and is Ready.
	mcp := &ssv1alpha1.ManifestCheckpoint{}
	if err := c.Get(ctx, client.ObjectKey{Name: mcpName}, mcp); err != nil {
		t.Fatalf("get MCP: %v", err)
	}
	readyCond := apimeta.FindStatusCondition(mcp.Status.Conditions, ssv1alpha1.ManifestCheckpointConditionTypeReady)
	if readyCond == nil || readyCond.Status != metav1.ConditionTrue {
		t.Errorf("expected MCP Ready=True, got: %v", mcp.Status.Conditions)
	}

	// Labels must include import-mode.
	if mcp.Labels[labelImportMode] != labelValueTrue {
		t.Errorf("expected import-mode label, got: %v", mcp.Labels)
	}
}

func TestHandleImportManifests_Idempotent(t *testing.T) {
	c := newImportFakeClient()
	h := newImportHandler(c)

	body := gzipJSON(t, []interface{}{map[string]interface{}{"kind": "Pod"}})

	// First call.
	req1 := httptest.NewRequest(http.MethodPut, "/", bytes.NewReader(body))
	req1.Header.Set("Content-Encoding", "gzip")
	rr1 := httptest.NewRecorder()
	h.HandleImportManifests(rr1, req1, "ns", "snap", "node-1")
	if rr1.Code != http.StatusOK {
		t.Fatalf("first call: expected 200, got %d", rr1.Code)
	}

	// Second call with the same inputs — should also return 200.
	body2 := gzipJSON(t, []interface{}{map[string]interface{}{"kind": "Pod"}})
	req2 := httptest.NewRequest(http.MethodPut, "/", bytes.NewReader(body2))
	req2.Header.Set("Content-Encoding", "gzip")
	rr2 := httptest.NewRecorder()
	h.HandleImportManifests(rr2, req2, "ns", "snap", "node-1")
	if rr2.Code != http.StatusOK {
		t.Fatalf("second call: expected 200, got %d: %s", rr2.Code, rr2.Body.String())
	}
}

// ── HandleImportBuild (HTTP handler) ─────────────────────────────────────────

func TestHandleImportBuild_BuildsTree(t *testing.T) {
	ctx := context.Background()
	c := newImportFakeClient()
	h := newImportHandler(c)

	reqBody := ImportBuildRequest{
		Nodes: []ImportBuildNode{
			{NodeID: "root"},
		},
		RootNodeID: "root",
	}
	raw, _ := json.Marshal(reqBody)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw))
	rr := httptest.NewRecorder()

	h.HandleImportBuild(rr, req, "test-ns", "build-snap")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var result ImportBuildResult
	if err := json.NewDecoder(rr.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result.SnapshotName != "build-snap" {
		t.Errorf("unexpected snapshot name: %s", result.SnapshotName)
	}
	if result.RootSnapshotContentName == "" {
		t.Errorf("expected non-empty root content name")
	}

	// Verify Snapshot exists.
	snap := &storagev1alpha1snap.Snapshot{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "test-ns", Name: "build-snap"}, snap); err != nil {
		t.Fatalf("get Snapshot: %v", err)
	}
}

func TestHandleImportBuild_InvalidJSON(t *testing.T) {
	c := newImportFakeClient()
	h := newImportHandler(c)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("not-json"))
	rr := httptest.NewRecorder()

	h.HandleImportBuild(rr, req, "ns", "snap")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}
