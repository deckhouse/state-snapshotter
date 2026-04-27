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
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase/restore"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshotgraphregistry"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

func TestNamespaceSnapshotAggregatedManifests_HTTP_OK(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = ssv1alpha1.AddToScheme(scheme)
	_ = storagev1alpha1.AddToScheme(scheme)

	log, _ := logger.NewLogger("error")
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	arch := usecase.NewArchiveService(cl, cl, log)
	agg := usecase.NewAggregatedNamespaceManifests(cl, arch, nil)

	data1, checksum1 := encodeTestChunkData([]map[string]interface{}{
		{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]interface{}{"name": "a", "namespace": "ns1"}},
	})
	ch0 := &ssv1alpha1.ManifestCheckpointContentChunk{
		ObjectMeta: metav1.ObjectMeta{Name: "chunk-0"},
		Spec: ssv1alpha1.ManifestCheckpointContentChunkSpec{
			CheckpointName: "mcp-root",
			Index:          0,
			Data:           data1,
			Checksum:       checksum1,
			ObjectsCount:   1,
		},
	}
	_ = cl.Create(context.Background(), ch0)

	mcp := &ssv1alpha1.ManifestCheckpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "mcp-root", UID: types.UID("uid-root")},
		Spec:       ssv1alpha1.ManifestCheckpointSpec{SourceNamespace: "ns1"},
		Status: ssv1alpha1.ManifestCheckpointStatus{
			Chunks:       []ssv1alpha1.ChunkInfo{{Name: "chunk-0", Index: 0, Checksum: checksum1}},
			TotalObjects: 1,
		},
	}
	meta.SetStatusCondition(&mcp.Status.Conditions, metav1.Condition{
		Type: ssv1alpha1.ManifestCheckpointConditionTypeReady, Status: metav1.ConditionTrue,
		Reason: ssv1alpha1.ManifestCheckpointConditionReasonCompleted,
	})
	_ = cl.Create(context.Background(), mcp)

	nsc := &storagev1alpha1.NamespaceSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "root-nsc"},
		Spec: storagev1alpha1.NamespaceSnapshotContentSpec{
			NamespaceSnapshotRef: storagev1alpha1.SnapshotSubjectRef{
				APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
				Kind:       "NamespaceSnapshot",
				Name:       "snap",
				Namespace:  "ns1",
			},
			DeletionPolicy: storagev1alpha1.SnapshotContentDeletionPolicyRetain,
		},
		Status: storagev1alpha1.NamespaceSnapshotContentStatus{ManifestCheckpointName: "mcp-root"},
	}
	meta.SetStatusCondition(&nsc.Status.Conditions, metav1.Condition{
		Type: snapshot.ConditionReady, Status: metav1.ConditionTrue, Reason: "Completed",
	})
	_ = cl.Create(context.Background(), nsc)

	ns := &storagev1alpha1.NamespaceSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "ns1"},
		Status:     storagev1alpha1.NamespaceSnapshotStatus{BoundSnapshotContentName: "root-nsc"},
	}
	_ = cl.Create(context.Background(), ns)

	ah := NewArchiveHandler(cl, arch, log)
	rs := restore.NewService(cl, arch)
	rh := NewRestoreHandler(cl, rs, log, agg)
	mux := http.NewServeMux()
	ah.SetupRoutes(mux)
	rh.SetupRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/ns1/namespacesnapshots/snap/manifests")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var arr []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&arr); err != nil {
		t.Fatal(err)
	}
	if len(arr) != 1 {
		t.Fatalf("len %d", len(arr))
	}
}

func TestNamespaceSnapshotAggregatedManifests_HTTP_Gzip(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = ssv1alpha1.AddToScheme(scheme)
	_ = storagev1alpha1.AddToScheme(scheme)

	log, _ := logger.NewLogger("error")
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	arch := usecase.NewArchiveService(cl, cl, log)
	agg := usecase.NewAggregatedNamespaceManifests(cl, arch, nil)

	data1, checksum1 := encodeTestChunkData([]map[string]interface{}{
		{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]interface{}{"name": "a", "namespace": "ns1"}},
	})
	ch0 := &ssv1alpha1.ManifestCheckpointContentChunk{
		ObjectMeta: metav1.ObjectMeta{Name: "chunk-0"},
		Spec: ssv1alpha1.ManifestCheckpointContentChunkSpec{
			CheckpointName: "mcp-root",
			Index:          0,
			Data:           data1,
			Checksum:       checksum1,
			ObjectsCount:   1,
		},
	}
	_ = cl.Create(context.Background(), ch0)

	mcp := &ssv1alpha1.ManifestCheckpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "mcp-root", UID: types.UID("uid-root")},
		Spec:       ssv1alpha1.ManifestCheckpointSpec{SourceNamespace: "ns1"},
		Status: ssv1alpha1.ManifestCheckpointStatus{
			Chunks:       []ssv1alpha1.ChunkInfo{{Name: "chunk-0", Index: 0, Checksum: checksum1}},
			TotalObjects: 1,
		},
	}
	meta.SetStatusCondition(&mcp.Status.Conditions, metav1.Condition{
		Type: ssv1alpha1.ManifestCheckpointConditionTypeReady, Status: metav1.ConditionTrue,
		Reason: ssv1alpha1.ManifestCheckpointConditionReasonCompleted,
	})
	_ = cl.Create(context.Background(), mcp)

	nsc := &storagev1alpha1.NamespaceSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "root-nsc"},
		Spec: storagev1alpha1.NamespaceSnapshotContentSpec{
			NamespaceSnapshotRef: storagev1alpha1.SnapshotSubjectRef{
				APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
				Kind:       "NamespaceSnapshot",
				Name:       "snap",
				Namespace:  "ns1",
			},
			DeletionPolicy: storagev1alpha1.SnapshotContentDeletionPolicyRetain,
		},
		Status: storagev1alpha1.NamespaceSnapshotContentStatus{ManifestCheckpointName: "mcp-root"},
	}
	meta.SetStatusCondition(&nsc.Status.Conditions, metav1.Condition{
		Type: snapshot.ConditionReady, Status: metav1.ConditionTrue, Reason: "Completed",
	})
	_ = cl.Create(context.Background(), nsc)

	ns := &storagev1alpha1.NamespaceSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "ns1"},
		Status:     storagev1alpha1.NamespaceSnapshotStatus{BoundSnapshotContentName: "root-nsc"},
	}
	_ = cl.Create(context.Background(), ns)

	ah := NewArchiveHandler(cl, arch, log)
	rs := restore.NewService(cl, arch)
	rh := NewRestoreHandler(cl, rs, log, agg)
	mux := http.NewServeMux()
	ah.SetupRoutes(mux)
	rh.SetupRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/ns1/namespacesnapshots/snap/manifests", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("expected gzip, got %q", resp.Header.Get("Content-Encoding"))
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	var arr []map[string]interface{}
	if err := json.Unmarshal(raw, &arr); err != nil {
		t.Fatal(err)
	}
	if len(arr) != 1 {
		t.Fatalf("len %d", len(arr))
	}
}

func TestGenericSnapshotAggregatedManifests_HTTP_VMAndDiskSubtrees(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = ssv1alpha1.AddToScheme(scheme)
	_ = storagev1alpha1.AddToScheme(scheme)
	_ = demov1alpha1.AddToScheme(scheme)

	log, _ := logger.NewLogger("error")
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	createReadyMCPForAPI(t, cl, "mcp-vm", []map[string]interface{}{
		{"apiVersion": demov1alpha1.SchemeGroupVersion.String(), "kind": "DemoVirtualMachine", "metadata": map[string]interface{}{"name": "vm-1", "namespace": "ns1"}},
	})
	createReadyMCPForAPI(t, cl, "mcp-disk", []map[string]interface{}{
		{"apiVersion": demov1alpha1.SchemeGroupVersion.String(), "kind": "DemoVirtualDisk", "metadata": map[string]interface{}{"name": "disk-a", "namespace": "ns1"}},
	})
	_ = cl.Create(context.Background(), &demov1alpha1.DemoVirtualMachineSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "vm-1", Namespace: "ns1"},
		Status:     demov1alpha1.DemoVirtualMachineSnapshotStatus{BoundSnapshotContentName: "vm-content"},
	})
	_ = cl.Create(context.Background(), &demov1alpha1.DemoVirtualMachineSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "vm-content"},
		Status: demov1alpha1.DemoVirtualMachineSnapshotContentStatus{
			ManifestCheckpointName:      "mcp-vm",
			ChildrenSnapshotContentRefs: []storagev1alpha1.NamespaceSnapshotContentChildRef{{Name: "disk-content"}},
		},
	})
	_ = cl.Create(context.Background(), &demov1alpha1.DemoVirtualDiskSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "disk-a", Namespace: "ns1"},
		Status:     demov1alpha1.DemoVirtualDiskSnapshotStatus{BoundSnapshotContentName: "disk-content"},
	})
	_ = cl.Create(context.Background(), &demov1alpha1.DemoVirtualDiskSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "disk-content"},
		Status:     demov1alpha1.DemoVirtualDiskSnapshotContentStatus{ManifestCheckpointName: "mcp-disk"},
	})

	srv := newGenericAggregatedTestServer(t, cl, log)
	defer srv.Close()

	vmObjects := getAggregatedObjects(t, srv.URL+"/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/ns1/demovirtualmachinesnapshots/vm-1/manifests", http.StatusOK)
	if !containsKindName(vmObjects, "DemoVirtualMachine", "vm-1") || !containsKindName(vmObjects, "DemoVirtualDisk", "disk-a") {
		t.Fatalf("VM subtree should contain VM and disk objects: %#v", vmObjects)
	}

	diskObjects := getAggregatedObjects(t, srv.URL+"/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/ns1/demovirtualdisksnapshots/disk-a/manifests", http.StatusOK)
	if containsKindName(diskObjects, "DemoVirtualMachine", "vm-1") || !containsKindName(diskObjects, "DemoVirtualDisk", "disk-a") {
		t.Fatalf("disk subtree should contain only disk object from this tree: %#v", diskObjects)
	}

	ambiguousSrv := newGenericAggregatedTestServerWithRESTMapper(t, cl, log, genericAggregatedAmbiguousRESTMapper())
	defer ambiguousSrv.Close()
	ambiguousObjects := getAggregatedObjects(t, ambiguousSrv.URL+"/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/ns1/demovirtualmachinesnapshots/vm-1/manifests", http.StatusOK)
	if !containsKindName(ambiguousObjects, "DemoVirtualMachine", "vm-1") {
		t.Fatalf("ambiguous resource resolution should select registered snapshot GVK: %#v", ambiguousObjects)
	}
}

func TestGenericSnapshotAggregatedManifests_HTTP_Errors(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = ssv1alpha1.AddToScheme(scheme)
	_ = storagev1alpha1.AddToScheme(scheme)
	_ = demov1alpha1.AddToScheme(scheme)

	log, _ := logger.NewLogger("error")

	t.Run("duplicate object returns 409", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).Build()
		dup := []map[string]interface{}{
			{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]interface{}{"name": "same", "namespace": "ns1"}},
		}
		createReadyMCPForAPI(t, cl, "mcp-vm", dup)
		createReadyMCPForAPI(t, cl, "mcp-disk", dup)
		_ = cl.Create(context.Background(), &demov1alpha1.DemoVirtualMachineSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "vm-dup", Namespace: "ns1"},
			Status:     demov1alpha1.DemoVirtualMachineSnapshotStatus{BoundSnapshotContentName: "vm-content"},
		})
		_ = cl.Create(context.Background(), &demov1alpha1.DemoVirtualMachineSnapshotContent{
			ObjectMeta: metav1.ObjectMeta{Name: "vm-content"},
			Status: demov1alpha1.DemoVirtualMachineSnapshotContentStatus{
				ManifestCheckpointName:      "mcp-vm",
				ChildrenSnapshotContentRefs: []storagev1alpha1.NamespaceSnapshotContentChildRef{{Name: "disk-content"}},
			},
		})
		_ = cl.Create(context.Background(), &demov1alpha1.DemoVirtualDiskSnapshotContent{
			ObjectMeta: metav1.ObjectMeta{Name: "disk-content"},
			Status:     demov1alpha1.DemoVirtualDiskSnapshotContentStatus{ManifestCheckpointName: "mcp-disk"},
		})
		srv := newGenericAggregatedTestServer(t, cl, log)
		defer srv.Close()
		body := getRawResponse(t, srv.URL+"/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/ns1/demovirtualmachinesnapshots/vm-dup/manifests", http.StatusConflict)
		if !jsonContainsString(body, "duplicate object detected in snapshot tree") {
			t.Fatalf("expected duplicate error, got %s", string(body))
		}
	})

	t.Run("empty bound content returns 400", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&demov1alpha1.DemoVirtualDiskSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "disk-unbound", Namespace: "ns1"},
		}).Build()
		srv := newGenericAggregatedTestServer(t, cl, log)
		defer srv.Close()
		_ = getRawResponse(t, srv.URL+"/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/ns1/demovirtualdisksnapshots/disk-unbound/manifests", http.StatusBadRequest)
	})

	t.Run("snapshot not found returns 404", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).Build()
		srv := newGenericAggregatedTestServer(t, cl, log)
		defer srv.Close()
		_ = getRawResponse(t, srv.URL+"/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/ns1/demovirtualdisksnapshots/missing/manifests", http.StatusNotFound)
	})

	t.Run("unsupported resource returns 400", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).Build()
		srv := newGenericAggregatedTestServer(t, cl, log)
		defer srv.Close()
		_ = getRawResponse(t, srv.URL+"/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/ns1/notasnapshots/x/manifests", http.StatusBadRequest)
	})
}

func newGenericAggregatedTestServer(t *testing.T, cl client.Client, log logger.LoggerInterface) *httptest.Server {
	t.Helper()
	return newGenericAggregatedTestServerWithRESTMapper(t, cl, log, genericAggregatedRESTMapper())
}

func newGenericAggregatedTestServerWithRESTMapper(t *testing.T, cl client.Client, log logger.LoggerInterface, mapper meta.RESTMapper) *httptest.Server {
	t.Helper()
	arch := usecase.NewArchiveService(cl, cl, log)
	reg, err := snapshot.NewGVKRegistryFromParallelSnapshotContentPairs(
		[]schema.GroupVersionKind{
			{Group: demov1alpha1.SchemeGroupVersion.Group, Version: demov1alpha1.SchemeGroupVersion.Version, Kind: "DemoVirtualMachineSnapshot"},
			{Group: demov1alpha1.SchemeGroupVersion.Group, Version: demov1alpha1.SchemeGroupVersion.Version, Kind: "DemoVirtualDiskSnapshot"},
		},
		[]schema.GroupVersionKind{
			{Group: demov1alpha1.SchemeGroupVersion.Group, Version: demov1alpha1.SchemeGroupVersion.Version, Kind: "DemoVirtualMachineSnapshotContent"},
			{Group: demov1alpha1.SchemeGroupVersion.Group, Version: demov1alpha1.SchemeGroupVersion.Version, Kind: "DemoVirtualDiskSnapshotContent"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	agg := usecase.NewAggregatedNamespaceManifests(cl, arch, snapshotgraphregistry.NewStatic(reg))
	rs := restore.NewService(cl, arch)
	rh := NewRestoreHandler(cl, rs, log, agg, mapper)
	mux := http.NewServeMux()
	rh.SetupRoutes(mux)
	return httptest.NewServer(mux)
}

func genericAggregatedRESTMapper() meta.RESTMapper {
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{demov1alpha1.SchemeGroupVersion, {Group: "", Version: "v1"}})
	mapper.Add(schema.GroupVersionKind{Group: demov1alpha1.SchemeGroupVersion.Group, Version: demov1alpha1.SchemeGroupVersion.Version, Kind: "DemoVirtualMachineSnapshot"}, meta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{Group: demov1alpha1.SchemeGroupVersion.Group, Version: demov1alpha1.SchemeGroupVersion.Version, Kind: "DemoVirtualDiskSnapshot"}, meta.RESTScopeNamespace)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("ConfigMap"), meta.RESTScopeNamespace)
	return mapper
}

func genericAggregatedAmbiguousRESTMapper() meta.RESTMapper {
	otherGV := schema.GroupVersion{Group: "other.example.io", Version: "v1"}
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{otherGV, demov1alpha1.SchemeGroupVersion})
	mapper.Add(otherGV.WithKind("DemoVirtualMachineSnapshot"), meta.RESTScopeNamespace)
	mapper.Add(demov1alpha1.SchemeGroupVersion.WithKind("DemoVirtualMachineSnapshot"), meta.RESTScopeNamespace)
	mapper.Add(demov1alpha1.SchemeGroupVersion.WithKind("DemoVirtualDiskSnapshot"), meta.RESTScopeNamespace)
	return mapper
}

func createReadyMCPForAPI(t *testing.T, cl client.Client, mcpName string, objects []map[string]interface{}) {
	t.Helper()
	data, checksum := encodeTestChunkData(objects)
	chunk := &ssv1alpha1.ManifestCheckpointContentChunk{
		ObjectMeta: metav1.ObjectMeta{Name: "chunk-" + mcpName},
		Spec: ssv1alpha1.ManifestCheckpointContentChunkSpec{
			CheckpointName: mcpName,
			Index:          0,
			Data:           data,
			Checksum:       checksum,
			ObjectsCount:   len(objects),
		},
	}
	if err := cl.Create(context.Background(), chunk); err != nil {
		t.Fatal(err)
	}
	mcp := &ssv1alpha1.ManifestCheckpoint{
		ObjectMeta: metav1.ObjectMeta{Name: mcpName, UID: types.UID("uid-" + mcpName)},
		Spec:       ssv1alpha1.ManifestCheckpointSpec{SourceNamespace: "ns1"},
		Status: ssv1alpha1.ManifestCheckpointStatus{
			Chunks:       []ssv1alpha1.ChunkInfo{{Name: chunk.Name, Index: 0, Checksum: checksum}},
			TotalObjects: len(objects),
		},
	}
	meta.SetStatusCondition(&mcp.Status.Conditions, metav1.Condition{
		Type:   ssv1alpha1.ManifestCheckpointConditionTypeReady,
		Status: metav1.ConditionTrue,
		Reason: ssv1alpha1.ManifestCheckpointConditionReasonCompleted,
	})
	if err := cl.Create(context.Background(), mcp); err != nil {
		t.Fatal(err)
	}
}

func getAggregatedObjects(t *testing.T, url string, wantStatus int) []map[string]interface{} {
	t.Helper()
	body := getRawResponse(t, url, wantStatus)
	var arr []map[string]interface{}
	if err := json.Unmarshal(body, &arr); err != nil {
		t.Fatal(err)
	}
	return arr
}

func getRawResponse(t *testing.T, url string, wantStatus int) []byte {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("status %d, want %d: %s", resp.StatusCode, wantStatus, string(body))
	}
	return body
}

func containsKindName(objects []map[string]interface{}, kind, name string) bool {
	for _, obj := range objects {
		if obj["kind"] != kind {
			continue
		}
		metaObj, ok := obj["metadata"].(map[string]interface{})
		if ok && metaObj["name"] == name {
			return true
		}
	}
	return false
}

func jsonContainsString(body []byte, needle string) bool {
	return strings.Contains(string(body), needle)
}
