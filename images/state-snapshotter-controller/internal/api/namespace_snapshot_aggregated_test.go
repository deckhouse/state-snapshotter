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
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase/restore"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

func TestNamespaceSnapshotAggregatedManifests_HTTP_OK(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = ssv1alpha1.AddToScheme(scheme)
	_ = storagev1alpha1.AddToScheme(scheme)

	log, _ := logger.NewLogger("error")
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	arch := usecase.NewArchiveService(cl, cl, log)
	agg := usecase.NewAggregatedNamespaceManifests(cl, arch)

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
	agg := usecase.NewAggregatedNamespaceManifests(cl, arch)

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
