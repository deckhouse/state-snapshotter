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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
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
		mcp := aggManifestReadyMCP(tc.cpName, []ssv1alpha1.ChunkInfo{{Name: ch.Name, Index: 0, Checksum: cs}}, 1)
		_ = cl.Create(ctx, mcp)
	}
	_ = cl.Create(ctx, aggManifestContent("child-content", "mcp-child"))
	rootContent := aggManifestContent("root-content", "mcp-root", "child-content")
	// The root download path resolves the live Snapshot's status.boundSnapshotContentName and enforces the
	// anti-spoofing back-reference, so the root content must point back at the bound Snapshot (ns1/snap).
	rootContent.Spec.SnapshotRef = &storagev1alpha1.SnapshotSubjectRef{
		APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
		Kind:       "Snapshot",
		Namespace:  aggManifestTestSnapNamespace,
		Name:       aggManifestTestSnapName,
	}
	_ = cl.Create(ctx, rootContent)
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
	mcp := aggManifestReadyMCP("mcp-import", []ssv1alpha1.ChunkInfo{{Name: ch.Name, Index: 0, Checksum: cs}}, 1)
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
	mcp := aggManifestReadyMCP("mcp-capture", []ssv1alpha1.ChunkInfo{{Name: ch.Name, Index: 0, Checksum: cs}}, 1)
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
	if err := ReconstructManifestCheckpoint(ctx, cl, importMCPName, nil, downloaded); err != nil {
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

// TestBuildSingleNodeJSONForRootSnapshot_BackRef pins the anti-spoofing handshake on the CORE Snapshot
// root download (live-CR branch): the content resolved from the live Snapshot's
// status.boundSnapshotContentName must carry a spec.snapshotRef pointing back at that core Snapshot. A
// matching back-ref serves the root's own manifests; a mismatched or missing one is fail-closed 403.
func TestBuildSingleNodeJSONForRootSnapshot_BackRef(t *testing.T) {
	scheme := aggManifestTestScheme(t)
	log, _ := logger.NewLogger("error")

	newAgg := func(t *testing.T, ref *storagev1alpha1.SnapshotSubjectRef) *AggregatedNamespaceManifests {
		t.Helper()
		cl := fake.NewClientBuilder().WithScheme(scheme).Build()
		arch := NewArchiveService(cl, cl, log)
		ctx := context.Background()
		d, cs := aggManifestEncodeChunk([]map[string]interface{}{
			{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]interface{}{"name": "root-own", "namespace": "ns1"}},
		})
		ch := aggManifestCreateChunk("ch-root", "mcp-root", d, cs)
		if err := cl.Create(ctx, ch); err != nil {
			t.Fatal(err)
		}
		if err := cl.Create(ctx, aggManifestReadyMCP("mcp-root", []ssv1alpha1.ChunkInfo{{Name: ch.Name, Index: 0, Checksum: cs}}, 1)); err != nil {
			t.Fatal(err)
		}
		content := aggManifestContent("root-content", "mcp-root")
		content.Spec.SnapshotRef = ref
		if err := cl.Create(ctx, content); err != nil {
			t.Fatal(err)
		}
		if err := cl.Create(ctx, aggManifestNS("root-content")); err != nil {
			t.Fatal(err)
		}
		return NewAggregatedNamespaceManifests(cl, arch, nil)
	}

	t.Run("matching back-ref succeeds", func(t *testing.T) {
		agg := newAgg(t, &storagev1alpha1.SnapshotSubjectRef{APIVersion: storagev1alpha1.SchemeGroupVersion.String(), Kind: "Snapshot", Namespace: aggManifestTestSnapNamespace, Name: aggManifestTestSnapName})
		raw, err := agg.BuildSingleNodeJSONForRootSnapshot(context.Background(), aggManifestTestSnapNamespace, aggManifestTestSnapName)
		if err != nil {
			t.Fatalf("BuildSingleNodeJSONForRootSnapshot: %v", err)
		}
		var arr []map[string]interface{}
		if err := json.Unmarshal(raw, &arr); err != nil {
			t.Fatal(err)
		}
		if len(arr) != 1 || arr[0]["metadata"].(map[string]interface{})["name"] != "root-own" {
			t.Fatalf("want the root node's own object, got %v", arr)
		}
	})

	t.Run("mismatched back-ref 403", func(t *testing.T) {
		agg := newAgg(t, &storagev1alpha1.SnapshotSubjectRef{APIVersion: storagev1alpha1.SchemeGroupVersion.String(), Kind: "Snapshot", Namespace: aggManifestTestSnapNamespace, Name: "other"})
		_, err := agg.BuildSingleNodeJSONForRootSnapshot(context.Background(), aggManifestTestSnapNamespace, aggManifestTestSnapName)
		assertAggStatus(t, err, http.StatusForbidden)
	})

	t.Run("missing back-ref 403", func(t *testing.T) {
		agg := newAgg(t, nil)
		_, err := agg.BuildSingleNodeJSONForRootSnapshot(context.Background(), aggManifestTestSnapNamespace, aggManifestTestSnapName)
		assertAggStatus(t, err, http.StatusForbidden)
	})
}

// TestBuildSingleNodeJSONFromSnapshot_BackRef pins the anti-spoofing handshake on the per-CR download
// facade: after resolving the CR's status.boundSnapshotContentName, the bound SnapshotContent must carry
// a spec.snapshotRef pointing back at that very CR. A matching back-ref serves the node's own manifests;
// a mismatched or missing back-ref is refused fail-closed with 403 (Forbidden).
func TestBuildSingleNodeJSONFromSnapshot_BackRef(t *testing.T) {
	scheme := aggManifestTestScheme(t)
	log, _ := logger.NewLogger("error")
	gvk := storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot")

	newAgg := func(t *testing.T, ref *storagev1alpha1.SnapshotSubjectRef) *AggregatedNamespaceManifests {
		t.Helper()
		cl := fake.NewClientBuilder().WithScheme(scheme).Build()
		arch := NewArchiveService(cl, cl, log)
		ctx := context.Background()
		d, cs := aggManifestEncodeChunk([]map[string]interface{}{
			{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]interface{}{"name": "own", "namespace": "ns1"}},
		})
		ch := aggManifestCreateChunk("ch-bound", "bound-mcp", d, cs)
		if err := cl.Create(ctx, ch); err != nil {
			t.Fatal(err)
		}
		if err := cl.Create(ctx, aggManifestReadyMCP("bound-mcp", []ssv1alpha1.ChunkInfo{{Name: ch.Name, Index: 0, Checksum: cs}}, 1)); err != nil {
			t.Fatal(err)
		}
		content := &storagev1alpha1.SnapshotContent{
			ObjectMeta: metav1.ObjectMeta{Name: "bound-content"},
			Spec:       storagev1alpha1.SnapshotContentSpec{SnapshotRef: ref},
			Status:     storagev1alpha1.SnapshotContentStatus{ManifestCheckpointName: "bound-mcp"},
		}
		meta.SetStatusCondition(&content.Status.Conditions, metav1.Condition{Type: snapshot.ConditionReady, Status: metav1.ConditionTrue, Reason: "Completed"})
		if err := cl.Create(ctx, content); err != nil {
			t.Fatal(err)
		}
		snap := &storagev1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "ns1"},
			Status:     storagev1alpha1.SnapshotStatus{BoundSnapshotContentName: "bound-content"},
		}
		if err := cl.Create(ctx, snap); err != nil {
			t.Fatal(err)
		}
		return NewAggregatedNamespaceManifests(cl, arch, nil)
	}

	t.Run("matching back-ref succeeds", func(t *testing.T) {
		agg := newAgg(t, &storagev1alpha1.SnapshotSubjectRef{APIVersion: gvk.GroupVersion().String(), Kind: "Snapshot", Namespace: "ns1", Name: "snap"})
		raw, err := agg.BuildSingleNodeJSONFromSnapshot(context.Background(), gvk, "ns1", "snap")
		if err != nil {
			t.Fatalf("BuildSingleNodeJSONFromSnapshot: %v", err)
		}
		var arr []map[string]interface{}
		if err := json.Unmarshal(raw, &arr); err != nil {
			t.Fatal(err)
		}
		if len(arr) != 1 || arr[0]["metadata"].(map[string]interface{})["name"] != "own" {
			t.Fatalf("want the node's own object, got %v", arr)
		}
	})

	t.Run("mismatched back-ref 403", func(t *testing.T) {
		agg := newAgg(t, &storagev1alpha1.SnapshotSubjectRef{APIVersion: gvk.GroupVersion().String(), Kind: "Snapshot", Namespace: "ns1", Name: "other"})
		_, err := agg.BuildSingleNodeJSONFromSnapshot(context.Background(), gvk, "ns1", "snap")
		assertAggStatus(t, err, http.StatusForbidden)
	})

	t.Run("missing back-ref 403", func(t *testing.T) {
		agg := newAgg(t, nil)
		_, err := agg.BuildSingleNodeJSONFromSnapshot(context.Background(), gvk, "ns1", "snap")
		assertAggStatus(t, err, http.StatusForbidden)
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
