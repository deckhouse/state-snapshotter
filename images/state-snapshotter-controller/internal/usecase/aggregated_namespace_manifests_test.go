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
	"errors"
	"net/http"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshotgraphregistry"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

const (
	aggManifestTestSnapNamespace = "ns1"
	aggManifestTestSnapName      = "snap"
)

func aggManifestTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := ssv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	if err := storagev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme storage: %v", err)
	}
	return s
}

func aggManifestEncodeChunk(objects []map[string]interface{}) (data string, checksum string) {
	jsonData, _ := json.Marshal(objects)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write(jsonData)
	_ = gz.Close()
	encoded := base64.StdEncoding.EncodeToString(buf.Bytes())
	hash := sha256.Sum256(buf.Bytes())
	return encoded, hex.EncodeToString(hash[:])
}

func aggManifestCreateChunk(name, cpName string, data, checksum string) *ssv1alpha1.ManifestCheckpointContentChunk {
	return &ssv1alpha1.ManifestCheckpointContentChunk{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: ssv1alpha1.ManifestCheckpointContentChunkSpec{
			CheckpointName: cpName,
			Index:          0,
			Data:           data,
			Checksum:       checksum,
			ObjectsCount:   1,
		},
	}
}

func aggManifestReadyMCP(name, srcNS string, chunks []ssv1alpha1.ChunkInfo, totalObj int) *ssv1alpha1.ManifestCheckpoint {
	cp := &ssv1alpha1.ManifestCheckpoint{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("uid-" + name)},
		Spec: ssv1alpha1.ManifestCheckpointSpec{
			SourceNamespace: srcNS,
		},
		Status: ssv1alpha1.ManifestCheckpointStatus{
			Chunks:       chunks,
			TotalObjects: totalObj,
		},
	}
	meta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
		Type:   ssv1alpha1.ManifestCheckpointConditionTypeReady,
		Status: metav1.ConditionTrue,
		Reason: ssv1alpha1.ManifestCheckpointConditionReasonCompleted,
	})
	return cp
}

func aggManifestNotReadyMCP(name, srcNS string, chunks []ssv1alpha1.ChunkInfo, totalObj int) *ssv1alpha1.ManifestCheckpoint {
	cp := aggManifestReadyMCP(name, srcNS, chunks, totalObj)
	meta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
		Type:   ssv1alpha1.ManifestCheckpointConditionTypeReady,
		Status: metav1.ConditionFalse,
		Reason: "Pending",
	})
	return cp
}

func aggManifestNSC(name, mcpName string, children ...string) *storagev1alpha1.NamespaceSnapshotContent {
	var refs []storagev1alpha1.NamespaceSnapshotContentChildRef
	for _, c := range children {
		refs = append(refs, storagev1alpha1.NamespaceSnapshotContentChildRef{Name: c})
	}
	nsc := &storagev1alpha1.NamespaceSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: storagev1alpha1.NamespaceSnapshotContentSpec{
			NamespaceSnapshotRef: storagev1alpha1.SnapshotSubjectRef{
				Kind:       "NamespaceSnapshot",
				APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
				Name:       aggManifestTestSnapName,
				Namespace:  aggManifestTestSnapNamespace,
			},
			DeletionPolicy: storagev1alpha1.SnapshotContentDeletionPolicyRetain,
		},
		Status: storagev1alpha1.NamespaceSnapshotContentStatus{
			ManifestCheckpointName:      mcpName,
			ChildrenSnapshotContentRefs: refs,
		},
	}
	meta.SetStatusCondition(&nsc.Status.Conditions, metav1.Condition{
		Type: snapshot.ConditionReady, Status: metav1.ConditionTrue, Reason: "Completed",
	})
	return nsc
}

func aggManifestNS(bound string) *storagev1alpha1.NamespaceSnapshot {
	return &storagev1alpha1.NamespaceSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: aggManifestTestSnapName, Namespace: aggManifestTestSnapNamespace},
		Status: storagev1alpha1.NamespaceSnapshotStatus{
			BoundSnapshotContentName: bound,
		},
	}
}

func aggManifestDedicatedContent(gvk schema.GroupVersionKind, name, mcpName string, children ...string) *unstructured.Unstructured {
	var refs []map[string]interface{}
	for _, c := range children {
		refs = append(refs, map[string]interface{}{"name": c})
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetName(name)
	u.Object["status"] = map[string]interface{}{
		"manifestCheckpointName": mcpName,
	}
	if len(refs) > 0 {
		u.Object["status"].(map[string]interface{})["childrenSnapshotContentRefs"] = refs
	}
	return u
}

func TestAggregatedNamespaceManifests_RetainedWithoutSnapshot(t *testing.T) {
	scheme := aggManifestTestScheme(t)
	log, _ := logger.NewLogger("error")
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	arch := NewArchiveService(cl, cl, log)
	agg := NewAggregatedNamespaceManifests(cl, arch, nil)

	d1, c1 := aggManifestEncodeChunk([]map[string]interface{}{
		{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]interface{}{"name": "cm1", "namespace": "ns1"}},
	})
	ch := aggManifestCreateChunk("ch0", "mcp-root", d1, c1)
	_ = cl.Create(context.Background(), ch)
	mcp := aggManifestReadyMCP("mcp-root", "ns1", []ssv1alpha1.ChunkInfo{{Name: "ch0", Index: 0, Checksum: c1}}, 1)
	_ = cl.Create(context.Background(), mcp)
	root := aggManifestNSC("root-nsc", "mcp-root")
	_ = cl.Create(context.Background(), root)
	// No NamespaceSnapshot object — retained content only.

	raw, err := agg.BuildAggregatedJSON(context.Background(), "ns1", "snap")
	if err != nil {
		t.Fatal(err)
	}
	var arr []map[string]interface{}
	if err := json.Unmarshal(raw, &arr); err != nil {
		t.Fatal(err)
	}
	if len(arr) != 1 {
		t.Fatalf("len=%d", len(arr))
	}
}

func TestAggregatedNamespaceManifests_ParentOnly(t *testing.T) {
	scheme := aggManifestTestScheme(t)
	log, _ := logger.NewLogger("error")
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	arch := NewArchiveService(cl, cl, log)
	agg := NewAggregatedNamespaceManifests(cl, arch, nil)

	d1, c1 := aggManifestEncodeChunk([]map[string]interface{}{
		{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]interface{}{"name": "a", "namespace": "ns1"}},
	})
	ch := aggManifestCreateChunk("ch0", "mcp-root", d1, c1)
	_ = cl.Create(context.Background(), ch)
	mcp := aggManifestReadyMCP("mcp-root", "ns1", []ssv1alpha1.ChunkInfo{{Name: "ch0", Index: 0, Checksum: c1}}, 1)
	_ = cl.Create(context.Background(), mcp)
	root := aggManifestNSC("root-nsc", "mcp-root")
	_ = cl.Create(context.Background(), root)
	ns := aggManifestNS("root-nsc")
	_ = cl.Create(context.Background(), ns)

	raw, err := agg.BuildAggregatedJSON(context.Background(), "ns1", "snap")
	if err != nil {
		t.Fatal(err)
	}
	var arr []map[string]interface{}
	if err := json.Unmarshal(raw, &arr); err != nil {
		t.Fatal(err)
	}
	if len(arr) != 1 {
		t.Fatalf("len=%d", len(arr))
	}
}

func TestAggregatedNamespaceManifests_UnreferencedChildNSCNotWalked(t *testing.T) {
	// §3-E3 / INV-REF-C1: an extra NamespaceSnapshotContent+MCP may exist in the cluster, but if the root
	// content node has empty childrenSnapshotContentRefs, aggregation MUST NOT reach it (no list/search fallback).
	scheme := aggManifestTestScheme(t)
	log, _ := logger.NewLogger("error")
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	arch := NewArchiveService(cl, cl, log)
	agg := NewAggregatedNamespaceManifests(cl, arch, nil)

	objRoot := []map[string]interface{}{{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]interface{}{"name": "root", "namespace": "ns1"}}}
	objOrphan := []map[string]interface{}{{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]interface{}{"name": "orphan", "namespace": "ns1"}}}

	for _, tc := range []struct {
		cpName string
		objs   []map[string]interface{}
	}{
		{"mcp-root", objRoot},
		{"mcp-orphan", objOrphan},
	} {
		d, cs := aggManifestEncodeChunk(tc.objs)
		ch := aggManifestCreateChunk("ch-"+tc.cpName, tc.cpName, d, cs)
		_ = cl.Create(context.Background(), ch)
		mcp := aggManifestReadyMCP(tc.cpName, "ns1", []ssv1alpha1.ChunkInfo{{Name: ch.Name, Index: 0, Checksum: cs}}, 1)
		_ = cl.Create(context.Background(), mcp)
	}

	orphan := aggManifestNSC("orphan-nsc", "mcp-orphan")
	_ = cl.Create(context.Background(), orphan)

	root := aggManifestNSC("root-nsc", "mcp-root") // no childrenSnapshotContentRefs
	_ = cl.Create(context.Background(), root)
	ns := aggManifestNS("root-nsc")
	_ = cl.Create(context.Background(), ns)

	raw, err := agg.BuildAggregatedJSON(context.Background(), "ns1", "snap")
	if err != nil {
		t.Fatal(err)
	}
	var arr []map[string]interface{}
	if err := json.Unmarshal(raw, &arr); err != nil {
		t.Fatal(err)
	}
	if len(arr) != 1 {
		t.Fatalf("want only root MCP objects, got %d", len(arr))
	}
	meta0 := arr[0]["metadata"].(map[string]interface{})
	if meta0["name"] != "root" {
		t.Fatalf("want root object only, got name %v", meta0["name"])
	}
}

func TestAggregatedNamespaceManifests_ParentTwoChildren_OrderAndDedup(t *testing.T) {
	scheme := aggManifestTestScheme(t)
	log, _ := logger.NewLogger("error")
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	arch := NewArchiveService(cl, cl, log)
	agg := NewAggregatedNamespaceManifests(cl, arch, nil)

	objRoot := []map[string]interface{}{{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]interface{}{"name": "root", "namespace": "ns1"}}}
	objB := []map[string]interface{}{{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]interface{}{"name": "b", "namespace": "ns1"}}}
	objC := []map[string]interface{}{{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]interface{}{"name": "c", "namespace": "ns1"}}}

	for _, tc := range []struct {
		cpName string
		objs   []map[string]interface{}
	}{
		{"mcp-root", objRoot},
		{"mcp-b", objB},
		{"mcp-c", objC},
	} {
		d, cs := aggManifestEncodeChunk(tc.objs)
		ch := aggManifestCreateChunk("ch-"+tc.cpName, tc.cpName, d, cs)
		_ = cl.Create(context.Background(), ch)
		mcp := aggManifestReadyMCP(tc.cpName, "ns1", []ssv1alpha1.ChunkInfo{{Name: ch.Name, Index: 0, Checksum: cs}}, 1)
		_ = cl.Create(context.Background(), mcp)
	}

	// child-b before child-c lexicographically
	childB := aggManifestNSC("child-b", "mcp-b")
	childC := aggManifestNSC("child-c", "mcp-c")
	_ = cl.Create(context.Background(), childB)
	_ = cl.Create(context.Background(), childC)

	root := aggManifestNSC("root-nsc", "mcp-root", "child-c", "child-b") // unsorted input; walk sorts
	_ = cl.Create(context.Background(), root)
	ns := aggManifestNS("root-nsc")
	_ = cl.Create(context.Background(), ns)

	raw, err := agg.BuildAggregatedJSON(context.Background(), "ns1", "snap")
	if err != nil {
		t.Fatal(err)
	}
	var arr []map[string]interface{}
	_ = json.Unmarshal(raw, &arr)
	if len(arr) != 3 {
		t.Fatalf("want 3 objects, got %d", len(arr))
	}
	meta0 := arr[0]["metadata"].(map[string]interface{})
	if meta0["name"] != "root" {
		t.Fatalf("first object name: %v", meta0["name"])
	}
	meta1 := arr[1]["metadata"].(map[string]interface{})
	meta2 := arr[2]["metadata"].(map[string]interface{})
	if meta1["name"] != "b" || meta2["name"] != "c" {
		t.Fatalf("order b,c expected, got %v %v", meta1["name"], meta2["name"])
	}
}

func TestAggregatedNamespaceManifests_DedicatedContentNodeMCPIncluded(t *testing.T) {
	scheme := aggManifestTestScheme(t)
	log, _ := logger.NewLogger("error")
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	arch := NewArchiveService(cl, cl, log)

	reg := snapshot.NewGVKRegistry()
	if err := reg.RegisterSnapshotContentMapping(
		"DemoVirtualDiskSnapshot",
		"demo.state-snapshotter.deckhouse.io/v1alpha1",
		"DemoVirtualDiskSnapshotContent",
		"demo.state-snapshotter.deckhouse.io/v1alpha1",
	); err != nil {
		t.Fatalf("register mapping: %v", err)
	}
	agg := NewAggregatedNamespaceManifests(cl, arch, snapshotgraphregistry.NewStatic(reg))

	objRoot := []map[string]interface{}{{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]interface{}{"name": "root", "namespace": "ns1"}}}
	objChild := []map[string]interface{}{{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]interface{}{"name": "child", "namespace": "ns1"}}}
	for _, tc := range []struct {
		cpName string
		objs   []map[string]interface{}
	}{
		{"mcp-root", objRoot},
		{"mcp-disk", objChild},
	} {
		d, cs := aggManifestEncodeChunk(tc.objs)
		ch := aggManifestCreateChunk("ch-"+tc.cpName, tc.cpName, d, cs)
		_ = cl.Create(context.Background(), ch)
		mcp := aggManifestReadyMCP(tc.cpName, "ns1", []ssv1alpha1.ChunkInfo{{Name: ch.Name, Index: 0, Checksum: cs}}, 1)
		_ = cl.Create(context.Background(), mcp)
	}

	diskGVK := schema.GroupVersionKind{
		Group:   "demo.state-snapshotter.deckhouse.io",
		Version: "v1alpha1",
		Kind:    "DemoVirtualDiskSnapshotContent",
	}
	diskContent := aggManifestDedicatedContent(diskGVK, "disk-content", "mcp-disk")
	_ = cl.Create(context.Background(), diskContent)

	root := aggManifestNSC("root-nsc", "mcp-root", "disk-content")
	_ = cl.Create(context.Background(), root)
	ns := aggManifestNS("root-nsc")
	_ = cl.Create(context.Background(), ns)

	raw, err := agg.BuildAggregatedJSON(context.Background(), "ns1", "snap")
	if err != nil {
		t.Fatal(err)
	}
	var arr []map[string]interface{}
	if err := json.Unmarshal(raw, &arr); err != nil {
		t.Fatal(err)
	}
	if len(arr) != 2 {
		t.Fatalf("want root+child objects, got %d", len(arr))
	}
}

func TestAggregatedNamespaceManifests_DedicatedContentWithoutMCPFailsClosed(t *testing.T) {
	scheme := aggManifestTestScheme(t)
	log, _ := logger.NewLogger("error")
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	arch := NewArchiveService(cl, cl, log)

	reg := snapshot.NewGVKRegistry()
	if err := reg.RegisterSnapshotContentMapping(
		"DemoVirtualDiskSnapshot",
		"demo.state-snapshotter.deckhouse.io/v1alpha1",
		"DemoVirtualDiskSnapshotContent",
		"demo.state-snapshotter.deckhouse.io/v1alpha1",
	); err != nil {
		t.Fatalf("register mapping: %v", err)
	}
	agg := NewAggregatedNamespaceManifests(cl, arch, snapshotgraphregistry.NewStatic(reg))

	d, cs := aggManifestEncodeChunk([]map[string]interface{}{
		{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]interface{}{"name": "root", "namespace": "ns1"}},
	})
	ch := aggManifestCreateChunk("ch-root", "mcp-root", d, cs)
	_ = cl.Create(context.Background(), ch)
	mcp := aggManifestReadyMCP("mcp-root", "ns1", []ssv1alpha1.ChunkInfo{{Name: ch.Name, Index: 0, Checksum: cs}}, 1)
	_ = cl.Create(context.Background(), mcp)

	diskGVK := schema.GroupVersionKind{
		Group:   "demo.state-snapshotter.deckhouse.io",
		Version: "v1alpha1",
		Kind:    "DemoVirtualDiskSnapshotContent",
	}
	diskContent := aggManifestDedicatedContent(diskGVK, "disk-content", "")
	_ = cl.Create(context.Background(), diskContent)

	root := aggManifestNSC("root-nsc", "mcp-root", "disk-content")
	_ = cl.Create(context.Background(), root)
	ns := aggManifestNS("root-nsc")
	_ = cl.Create(context.Background(), ns)

	_, err := agg.BuildAggregatedJSON(context.Background(), "ns1", "snap")
	var st *AggregatedStatusError
	if !errors.As(err, &st) || st.HTTPStatus != http.StatusInternalServerError {
		t.Fatalf("want fail-closed 500, got %v", err)
	}
}

func TestAggregatedNamespaceManifests_Errors(t *testing.T) {
	scheme := aggManifestTestScheme(t)
	log, _ := logger.NewLogger("error")
	ctx := context.Background()

	t.Run("ns not found", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).Build()
		agg := NewAggregatedNamespaceManifests(cl, NewArchiveService(cl, cl, log), nil)
		_, err := agg.BuildAggregatedJSON(ctx, "ns", "snap")
		var st *AggregatedStatusError
		if !errors.As(err, &st) || st.HTTPStatus != http.StatusNotFound {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("bound empty 409", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
			&storagev1alpha1.NamespaceSnapshot{ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "ns"}},
		).Build()
		agg := NewAggregatedNamespaceManifests(cl, NewArchiveService(cl, cl, log), nil)
		_, err := agg.BuildAggregatedJSON(ctx, "ns", "snap")
		var st *AggregatedStatusError
		if !errors.As(err, &st) || st.HTTPStatus != http.StatusConflict {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("mcp not found 404", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).Build()
		agg := NewAggregatedNamespaceManifests(cl, NewArchiveService(cl, cl, log), nil)
		root := aggManifestNSC("root-nsc", "missing-mcp")
		_ = cl.Create(ctx, root)
		ns := aggManifestNS("root-nsc")
		_ = cl.Create(ctx, ns)
		_, err := agg.BuildAggregatedJSON(ctx, "ns1", "snap")
		var st *AggregatedStatusError
		if !errors.As(err, &st) || st.HTTPStatus != http.StatusNotFound {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("nsc not found 404", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
			aggManifestNS("no-such-nsc"),
		).Build()
		agg := NewAggregatedNamespaceManifests(cl, NewArchiveService(cl, cl, log), nil)
		_, err := agg.BuildAggregatedJSON(ctx, "ns1", "snap")
		var st *AggregatedStatusError
		if !errors.As(err, &st) || st.HTTPStatus != http.StatusNotFound {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("mcp not ready 409", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).Build()
		arch := NewArchiveService(cl, cl, log)
		agg := NewAggregatedNamespaceManifests(cl, arch, nil)
		d1, c1 := aggManifestEncodeChunk([]map[string]interface{}{
			{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]interface{}{"name": "a", "namespace": "ns1"}},
		})
		ch := aggManifestCreateChunk("ch0", "mcp-bad", d1, c1)
		_ = cl.Create(ctx, ch)
		mcp := aggManifestNotReadyMCP("mcp-bad", "ns1", []ssv1alpha1.ChunkInfo{{Name: "ch0", Index: 0, Checksum: c1}}, 1)
		_ = cl.Create(ctx, mcp)
		root := aggManifestNSC("root-nsc", "mcp-bad")
		_ = cl.Create(ctx, root)
		ns := aggManifestNS("root-nsc")
		_ = cl.Create(ctx, ns)
		_, err := agg.BuildAggregatedJSON(ctx, "ns1", "snap")
		var st *AggregatedStatusError
		if !errors.As(err, &st) || st.HTTPStatus != http.StatusConflict {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("duplicate 500", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).Build()
		arch := NewArchiveService(cl, cl, log)
		agg := NewAggregatedNamespaceManifests(cl, arch, nil)
		dupObj := []map[string]interface{}{
			{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]interface{}{"name": "same", "namespace": "ns1"}},
		}
		for _, name := range []string{"mcp-root", "mcp-child"} {
			d, cs := aggManifestEncodeChunk(dupObj)
			ch := aggManifestCreateChunk("ch-"+name, name, d, cs)
			_ = cl.Create(ctx, ch)
			mcp := aggManifestReadyMCP(name, "ns1", []ssv1alpha1.ChunkInfo{{Name: ch.Name, Index: 0, Checksum: cs}}, 1)
			_ = cl.Create(ctx, mcp)
		}
		child := aggManifestNSC("child-nsc", "mcp-child")
		_ = cl.Create(ctx, child)
		root := aggManifestNSC("root-nsc", "mcp-root", "child-nsc")
		_ = cl.Create(ctx, root)
		ns := aggManifestNS("root-nsc")
		_ = cl.Create(ctx, ns)
		_, err := agg.BuildAggregatedJSON(ctx, "ns1", "snap")
		var st *AggregatedStatusError
		if !errors.As(err, &st) || st.HTTPStatus != http.StatusInternalServerError {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("cycle 500", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).Build()
		arch := NewArchiveService(cl, cl, log)
		agg := NewAggregatedNamespaceManifests(cl, arch, nil)
		d, cs := aggManifestEncodeChunk([]map[string]interface{}{
			{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]interface{}{"name": "x", "namespace": "ns1"}},
		})
		for _, pair := range []struct{ cp, ch string }{{"mcp-a", "ch-a"}, {"mcp-b", "ch-b"}} {
			ch := aggManifestCreateChunk(pair.ch, pair.cp, d, cs)
			_ = cl.Create(ctx, ch)
			mcp := aggManifestReadyMCP(pair.cp, "ns1", []ssv1alpha1.ChunkInfo{{Name: ch.Name, Index: 0, Checksum: cs}}, 1)
			_ = cl.Create(ctx, mcp)
		}
		a := aggManifestNSC("nsc-a", "mcp-a", "nsc-b")
		b := aggManifestNSC("nsc-b", "mcp-b", "nsc-a")
		_ = cl.Create(ctx, a)
		_ = cl.Create(ctx, b)
		ns := aggManifestNS("nsc-a")
		_ = cl.Create(ctx, ns)
		_, err := agg.BuildAggregatedJSON(ctx, "ns1", "snap")
		var st *AggregatedStatusError
		if !errors.As(err, &st) || st.HTTPStatus != http.StatusInternalServerError {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("missing manifestCheckpointName 500", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).Build()
		agg := NewAggregatedNamespaceManifests(cl, NewArchiveService(cl, cl, log), nil)
		nsc := aggManifestNSC("root-nsc", "x")
		nsc.Status.ManifestCheckpointName = ""
		_ = cl.Create(ctx, nsc)
		ns := aggManifestNS("root-nsc")
		_ = cl.Create(ctx, ns)
		_, err := agg.BuildAggregatedJSON(ctx, "ns1", "snap")
		var st *AggregatedStatusError
		if !errors.As(err, &st) || st.HTTPStatus != http.StatusInternalServerError {
			t.Fatalf("got %v", err)
		}
	})
}
