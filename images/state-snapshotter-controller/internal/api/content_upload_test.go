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

package api //nolint:revive // package name matches internal/api directory

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

func contentUploadScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = ssv1alpha1.AddToScheme(scheme)
	_ = storagev1alpha1.AddToScheme(scheme)
	return scheme
}

func newContentUploadServer(t *testing.T, cl client.Client) *httptest.Server {
	t.Helper()
	log, _ := logger.NewLogger("error")
	arch := usecase.NewArchiveService(cl, cl, log)
	agg := usecase.NewAggregatedNamespaceManifests(cl, arch, nil)
	importUpload := usecase.NewImportUploadService(cl)
	rh := NewRestoreHandler(cl, nil, log, agg, importUpload)
	mux := http.NewServeMux()
	rh.SetupRoutes(mux)
	return httptest.NewServer(mux)
}

const contentUploadPath = "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/snapshotcontents/"

// TestContentManifestsUpload_HappyPath pins the cluster-scoped content-addressed upload: given an existing
// SnapshotContent it reconstructs the node's MCP owned by that content (no bind-gate, manifests-only).
func TestContentManifestsUpload_HappyPath(t *testing.T) {
	cl := fake.NewClientBuilder().
		WithScheme(contentUploadScheme()).
		WithStatusSubresource(&ssv1alpha1.ManifestCheckpoint{}).
		WithObjects(&storagev1alpha1.SnapshotContent{
			ObjectMeta: metav1.ObjectMeta{Name: "c1", UID: "c1-uid"},
			Spec: storagev1alpha1.SnapshotContentSpec{
				SnapshotRef: &storagev1alpha1.SnapshotSubjectRef{
					APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
					Kind:       "Snapshot",
					Namespace:  "ns1",
					Name:       "snap",
					UID:        "snap-uid-9",
				},
			},
		}).
		Build()
	srv := newContentUploadServer(t, cl)
	defer srv.Close()

	payload := `{"manifests":[{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm","namespace":"ns1"}}]}`
	resp, err := http.Post(srv.URL+contentUploadPath+"c1/manifests-upload", "application/json", bytes.NewReader([]byte(payload)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("content manifests-upload status = %d, want 200", resp.StatusCode)
	}
	var out map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	mcpName, _ := out["manifestCheckpointName"].(string)
	if want := usecase.ReconstructedManifestCheckpointName("snap-uid-9", ""); mcpName != want {
		t.Fatalf("manifestCheckpointName = %q, want %q", mcpName, want)
	}
	mcp := &ssv1alpha1.ManifestCheckpoint{}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: mcpName}, mcp); err != nil {
		t.Fatalf("get MCP: %v", err)
	}
	if ctrlRef := metav1.GetControllerOf(mcp); ctrlRef == nil || ctrlRef.Kind != "SnapshotContent" || ctrlRef.Name != "c1" {
		t.Fatalf("MCP must be owned by the SnapshotContent, got %#v", mcp.OwnerReferences)
	}
}

// TestContentManifestsUpload_MissingContentIs404 pins that the cluster-scoped layer has NO bind-gate: a
// missing content is a plain 404 addressing error, never ImportContentNotBound.
func TestContentManifestsUpload_MissingContentIs404(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(contentUploadScheme()).Build()
	srv := newContentUploadServer(t, cl)
	defer srv.Close()

	payload := `{"manifests":[{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm","namespace":"ns1"}}]}`
	resp, err := http.Post(srv.URL+contentUploadPath+"missing/manifests-upload", "application/json", bytes.NewReader([]byte(payload)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing content upload status = %d, want 404", resp.StatusCode)
	}
	var st metav1.Status
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatalf("decode Status: %v", err)
	}
	if string(st.Reason) == usecase.ReasonImportContentNotBound {
		t.Fatalf("cluster-scoped missing content must NOT use ImportContentNotBound, got reason %q", st.Reason)
	}
}
