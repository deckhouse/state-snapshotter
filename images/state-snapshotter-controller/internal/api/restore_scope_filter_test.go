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

// The query parser and the four rules it enforces are shared with every other server of this subresource
// and are tested next to it, in the SDK. What this file proves is the wiring on this side: that the core
// handler goes through that parser instead of re-deciding the query, and that its verdicts reach the
// client as statuses.

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
	seedCoreRootSnapshotWithContentBackRef(t, cl, objects, "snap")
}

// seedCoreRootSnapshotWithContentBackRef is seedCoreRootReadySnapshot with the bound content's
// spec.snapshotRef name spelled out, so a test can seed the anti-spoofing mismatch (a content claimed by a
// snapshot it does not point back at).
func seedCoreRootSnapshotWithContentBackRef(t *testing.T, cl client.Client, objects []map[string]interface{}, contentBackRefName string) {
	t.Helper()
	createReadyMCPForAPI(t, cl, "mcp-root", objects)
	content := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "root-content"},
		Spec: storagev1alpha1.SnapshotContentSpec{
			SnapshotRef: &storagev1alpha1.SnapshotSubjectRef{
				APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
				Kind:       "Snapshot",
				Namespace:  "ns1",
				Name:       contentBackRefName,
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
// 400, a filter miss to 404, and a scope=node / scope=node+filter request to 200 holding only the
// addressed node. Where the answer is addressed is asserted here too, and it is a real assertion rather
// than a restatement: the seeded manifest is captured in a THIRD namespace, so "ns1" can only come from
// the compiler addressing the output into the snapshot's own namespace. That the read takes no namespace
// from its caller is a separate promise, pinned by TestPartnerDocRestoreReadTakesNoQueryNamespace.
func TestCoreHandler_ScopeNodeAndFilter(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(coreSnapshotScheme()).Build()
	seedCoreRootReadySnapshot(t, cl, []map[string]interface{}{
		{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]interface{}{"name": "app-config", "namespace": "captured-elsewhere-ns"}, "data": map[string]interface{}{"k": "v"}},
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

	// 200: scope=node returns only the node's own manifest, addressed into the snapshot's namespace.
	objs := getAggregatedObjects(t, srv.URL+coreRestoreBase+"?scope=node")
	if len(objs) != 1 || !containsKindName(objs, "ConfigMap", "app-config") {
		t.Fatalf("scope=node output = %#v, want single ConfigMap/app-config", objs)
	}
	if ns := (&unstructured.Unstructured{Object: objs[0]}).GetNamespace(); ns != "ns1" {
		t.Fatalf("scope=node namespace = %q, want the addressed snapshot's own ns1", ns)
	}

	// 200: scope=node + filter returns exactly the addressed object.
	objs = getAggregatedObjects(t, srv.URL+coreRestoreBase+"?scope=node&kind=ConfigMap&name=app-config")
	if len(objs) != 1 || !containsKindName(objs, "ConfigMap", "app-config") {
		t.Fatalf("scope=node filter output = %#v, want single ConfigMap/app-config", objs)
	}
}
