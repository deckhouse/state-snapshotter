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
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase/restore"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

// TestParseRestoreQueryOptions_Validation is the shared query-parser contract table: valid inputs map to
// the expected restore.Options, and every invalid combination is an ErrBadRequest.
func TestParseRestoreQueryOptions_Validation(t *testing.T) {
	cases := []struct {
		name       string
		query      string
		wantErr    bool
		wantScope  restore.Scope
		wantKind   string
		wantName   string
		wantAPIVer string
	}{
		{name: "no params -> subtree", query: "", wantScope: restore.ScopeSubtree},
		{name: "explicit subtree", query: "scope=subtree", wantScope: restore.ScopeSubtree},
		{name: "scope node", query: "scope=node", wantScope: restore.ScopeNode},
		{name: "node + filter", query: "scope=node&kind=ConfigMap&name=app", wantScope: restore.ScopeNode, wantKind: "ConfigMap", wantName: "app"},
		{name: "node + filter + apiVersion", query: "scope=node&kind=Gadget&name=g&apiVersion=a.example.com/v1", wantScope: restore.ScopeNode, wantKind: "Gadget", wantName: "g", wantAPIVer: "a.example.com/v1"},
		{name: "targetNamespace ignored by parser", query: "scope=node&targetNamespace=x", wantScope: restore.ScopeNode},

		{name: "unknown scope", query: "scope=bogus", wantErr: true},
		{name: "kind without name", query: "scope=node&kind=ConfigMap", wantErr: true},
		{name: "name without kind", query: "scope=node&name=app", wantErr: true},
		{name: "apiVersion without kind", query: "scope=node&apiVersion=v1", wantErr: true},
		{name: "filter with subtree", query: "scope=subtree&kind=ConfigMap&name=app", wantErr: true},
		{name: "filter with default scope", query: "kind=ConfigMap&name=app", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://x/y?"+tc.query, nil)
			opts, err := parseRestoreQueryOptions(r)
			if tc.wantErr {
				if !errors.Is(err, restore.ErrBadRequest) {
					t.Fatalf("expected ErrBadRequest, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if opts.Scope != tc.wantScope {
				t.Fatalf("scope = %q, want %q", opts.Scope, tc.wantScope)
			}
			if opts.FilterKind != tc.wantKind || opts.FilterName != tc.wantName || opts.FilterAPIVersion != tc.wantAPIVer {
				t.Fatalf("filter = %q/%q/%q, want %q/%q/%q", opts.FilterKind, opts.FilterName, opts.FilterAPIVersion, tc.wantKind, tc.wantName, tc.wantAPIVer)
			}
		})
	}
}

func coreSnapshotScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = ssv1alpha1.AddToScheme(scheme)
	_ = storagev1alpha1.AddToScheme(scheme)
	return scheme
}

func newCoreSnapshotServer(t *testing.T, cl client.Client) *httptest.Server {
	t.Helper()
	log, _ := logger.NewLogger("error")
	arch := usecase.NewArchiveService(cl, cl, log)
	agg := usecase.NewAggregatedNamespaceManifests(cl, arch, nil)
	rs := restore.NewService(cl, arch, nil, nil)
	importUpload := usecase.NewImportUploadService(cl)
	rh := NewRestoreHandler(cl, rs, log, agg, importUpload)
	mux := http.NewServeMux()
	rh.SetupRoutes(mux)
	return httptest.NewServer(mux)
}

// seedCoreRootReadySnapshot creates a Ready root Snapshot "snap"/ns1 with a bound Ready SnapshotContent
// and an MCP holding the given objects, so manifests-with-data-restoration reaches a 200.
func seedCoreRootReadySnapshot(t *testing.T, cl client.Client, objects []map[string]interface{}) {
	t.Helper()
	createReadyMCPForAPI(t, cl, "mcp-root", objects)
	content := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "root-content"},
		Spec: storagev1alpha1.SnapshotContentSpec{
			SnapshotRef: &storagev1alpha1.SnapshotSubjectRef{
				APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
				Kind:       "Snapshot",
				Namespace:  "ns1",
				Name:       "snap",
			},
		},
		Status: storagev1alpha1.SnapshotContentStatus{ManifestCheckpointName: "mcp-root"},
	}
	meta.SetStatusCondition(&content.Status.Conditions, metav1.Condition{Type: snapshot.ConditionReady, Status: metav1.ConditionTrue, Reason: "Completed"})
	if err := cl.Create(context.Background(), content); err != nil {
		t.Fatal(err)
	}
	snap := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "ns1"},
		Status:     storagev1alpha1.SnapshotStatus{BoundSnapshotContentName: "root-content"},
	}
	meta.SetStatusCondition(&snap.Status.Conditions, metav1.Condition{Type: snapshot.ConditionReady, Status: metav1.ConditionTrue, Reason: "Completed"})
	if err := cl.Create(context.Background(), snap); err != nil {
		t.Fatal(err)
	}
}

const coreRestoreBase = "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/ns1/snapshots/snap/manifests-with-data-restoration"

// TestCoreHandler_ScopeNodeAndFilter exercises the wired core handler end to end: a bad request maps to
// 400, a filter miss to 404, and a scope=node / scope=node+filter request to 200 with targetNamespace
// applied.
func TestCoreHandler_ScopeNodeAndFilter(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(coreSnapshotScheme()).Build()
	seedCoreRootReadySnapshot(t, cl, []map[string]interface{}{
		{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]interface{}{"name": "app-config", "namespace": "ns1"}, "data": map[string]interface{}{"k": "v"}},
	})
	srv := newCoreSnapshotServer(t, cl)
	defer srv.Close()

	getStatus := func(url string) int {
		resp, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// 400: object filter without scope=node.
	if got := getStatus(srv.URL + coreRestoreBase + "?kind=ConfigMap&name=app-config"); got != http.StatusBadRequest {
		t.Fatalf("filter without scope=node = %d, want 400", got)
	}
	// 400: unknown scope.
	if got := getStatus(srv.URL + coreRestoreBase + "?scope=bogus"); got != http.StatusBadRequest {
		t.Fatalf("unknown scope = %d, want 400", got)
	}
	// 404: filter miss under scope=node.
	if got := getStatus(srv.URL + coreRestoreBase + "?scope=node&kind=ConfigMap&name=missing"); got != http.StatusNotFound {
		t.Fatalf("filter miss = %d, want 404", got)
	}

	// 200: scope=node returns only the node's own manifest, with targetNamespace applied.
	objs := getAggregatedObjects(t, srv.URL+coreRestoreBase+"?scope=node&targetNamespace=restore-ns", http.StatusOK)
	if len(objs) != 1 || !containsKindName(objs, "ConfigMap", "app-config") {
		t.Fatalf("scope=node output = %#v, want single ConfigMap/app-config", objs)
	}
	if ns := (&unstructured.Unstructured{Object: objs[0]}).GetNamespace(); ns != "restore-ns" {
		t.Fatalf("scope=node targetNamespace not applied: namespace = %q, want restore-ns", ns)
	}

	// 200: scope=node + filter returns exactly the addressed object.
	objs = getAggregatedObjects(t, srv.URL+coreRestoreBase+"?scope=node&kind=ConfigMap&name=app-config", http.StatusOK)
	if len(objs) != 1 || !containsKindName(objs, "ConfigMap", "app-config") {
		t.Fatalf("scope=node filter output = %#v, want single ConfigMap/app-config", objs)
	}
}

// TestVSConnector_ScopeNodeAndFilter proves the VolumeSnapshot connector accepts the same parameters: a
// kind/name filter narrows the leaf output to the PVC, and a filter miss is 404.
func TestVSConnector_ScopeNodeAndFilter(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(vsConnectorScheme()).Build()
	seedVolumeSnapshotLeaf(t, cl, "vs-1", true)
	srv := newVSConnectorServer(t, cl)
	defer srv.Close()

	base := srv.URL + "/apis/subresources.snapshot.storage.k8s.io/v1/namespaces/ns1/volumesnapshots/vs-1/manifests-with-data-restoration"

	// scope=node + filter on the leaf PVC -> the single PVC.
	objs := getAggregatedObjects(t, base+"?scope=node&kind=PersistentVolumeClaim&name=orphan-pvc&targetNamespace=restore-ns", http.StatusOK)
	if len(objs) != 1 || !containsKindName(objs, "PersistentVolumeClaim", "orphan-pvc") {
		t.Fatalf("VS filter output = %#v, want single PVC/orphan-pvc", objs)
	}

	// Filter miss -> 404.
	resp, err := http.Get(base + "?scope=node&kind=PersistentVolumeClaim&name=missing")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("VS filter miss = %d, want 404", resp.StatusCode)
	}
}
