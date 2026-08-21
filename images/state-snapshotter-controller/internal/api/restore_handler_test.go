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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

func TestRestoreHandler_RoutingInvalidPaths(t *testing.T) {
	log, _ := logger.NewLogger("error")
	handler := NewRestoreHandler(nil, nil, log, nil, nil)
	mux := http.NewServeMux()
	handler.SetupRoutes(mux)
	server := httptest.NewServer(mux)
	defer server.Close()

	resp, err := http.Get(server.URL + "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/ns/foo")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}

	resp, err = http.Get(server.URL + "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/ns/snapshots/name")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for missing subresource, got %d", resp.StatusCode)
	}
}

// TestRestoreHandler_GenericDomainSubresourcesRemoved pins that the core group serves NONE of the three
// user-facing namespaced subresources for non-core (domain) snapshot kinds — download,
// data-restoration, AND upload are all 404 (address the node's own group) regardless of method — while
// the core Snapshot / cluster-scoped SnapshotContent routes stay alive (they reach their handlers, not the
// 404-unknown fall-through — with nil deps a live handler returns 500, and the POST-only upload returns 405
// on a GET).
func TestRestoreHandler_GenericDomainSubresourcesRemoved(t *testing.T) {
	log, _ := logger.NewLogger("error")
	handler := NewRestoreHandler(nil, nil, log, nil, nil)
	mux := http.NewServeMux()
	handler.SetupRoutes(mux)
	server := httptest.NewServer(mux)
	defer server.Close()

	get := func(path string) int {
		resp, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	post := func(path string) int {
		resp, err := http.Post(server.URL+path, "application/json", nil)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	base := "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1"

	// Generic (domain) GVR: all three user-facing subresources are refused with 404.
	if got := get(base + "/namespaces/ns/demovirtualmachinesnapshots/vm-x/manifests-download"); got != http.StatusNotFound {
		t.Fatalf("generic manifests-download = %d, want 404", got)
	}
	if got := get(base + "/namespaces/ns/demovirtualdisksnapshots/disk-x/manifests-with-data-restoration"); got != http.StatusNotFound {
		t.Fatalf("generic manifests-with-data-restoration = %d, want 404", got)
	}
	// Domain upload under the core group is refused with 404 (address the domain group) — GET and POST alike.
	if got := get(base + "/namespaces/ns/demovirtualmachinesnapshots/vm-x/manifests-and-children-refs-upload"); got != http.StatusNotFound {
		t.Fatalf("generic upload GET = %d, want 404", got)
	}
	if got := post(base + "/namespaces/ns/demovirtualmachinesnapshots/vm-x/manifests-and-children-refs-upload"); got != http.StatusNotFound {
		t.Fatalf("generic upload POST = %d, want 404", got)
	}
	// Core Snapshot manifests-download stays alive: it reaches the handler (nil nsAggregated -> 500), not 404.
	if got := get(base + "/namespaces/ns/snapshots/snap/manifests-download"); got != http.StatusInternalServerError {
		t.Fatalf("core snapshots manifests-download = %d, want 500 (route alive)", got)
	}
	// Cluster-scoped SnapshotContent manifests-download stays alive (nil nsAggregated -> 500), not 404.
	if got := get(base + "/snapshotcontents/c/manifests-download"); got != http.StatusInternalServerError {
		t.Fatalf("cluster-scoped snapshotcontents manifests-download = %d, want 500 (route alive)", got)
	}
	// Cluster-scoped manifests-upload route is wired: GET on the POST-only subresource returns 405 (route
	// alive), not the 404 fall-through.
	if got := get(base + "/snapshotcontents/c/manifests-upload"); got != http.StatusMethodNotAllowed {
		t.Fatalf("cluster-scoped manifests-upload GET = %d, want 405 (route alive)", got)
	}
}

// TestRestoreHandler_BackRefMismatchIsForbidden pins that the anti-spoofing refusal reaches the client as
// 403 and not as the 500 catch-all: a SnapshotContent whose spec.snapshotRef names a different snapshot
// must not serve manifests to the snapshot that claims it. The status a client sees is what makes the
// refusal actionable ("you may not read this"), so the whole path — resolver, handler, status mapping —
// is exercised over HTTP rather than the mapping alone.
func TestRestoreHandler_BackRefMismatchIsForbidden(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(coreSnapshotScheme()).Build()
	seedCoreRootSnapshotWithContentBackRef(t, cl, []map[string]interface{}{
		{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]interface{}{"name": "app-config", "namespace": "ns1"}, "data": map[string]interface{}{"k": "v"}},
	}, "someone-else")
	srv := newCoreSnapshotServer(t, cl)
	defer srv.Close()

	resp, err := http.Get(srv.URL + coreRestoreBase)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("back-ref mismatch = %d, want 403", resp.StatusCode)
	}
}

func TestRestoreHandler_ListSnapshotsMethodNotAllowed(t *testing.T) {
	log, _ := logger.NewLogger("error")
	handler := NewRestoreHandler(nil, nil, log, nil, nil)
	mux := http.NewServeMux()
	handler.SetupRoutes(mux)
	server := httptest.NewServer(mux)
	defer server.Close()

	req, _ := http.NewRequest(http.MethodPost, server.URL+"/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/ns/snapshots", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", resp.StatusCode)
	}
}

// TestRestoreHandler_ForeignGVRRefusalNamesNoGroup pins the TEXT of the refusal a domain GVR gets under
// the core group, not just its status: it must state the rule and name no aggregated group.
//
// The reason it is a guard and not a comment is that the published client contract says exactly this — a
// reader is told the message explains the rule but carries no address to copy into a URL — and the
// message is the one place that claim can go stale silently. It used to name a group: while this module
// served the volume node's endpoints itself, it could point a caller at the group next door. It no longer
// knows the name of any group but its own, so a message that names one can only be a stale literal, and a
// client that copies it would build a request nobody serves.
//
// The check is the absence of "subresources." — the prefix every aggregated group of the platform carries,
// so no group name can slip past it, this module's own included.
//
// LIMIT: it reads the refusal for ONE domain GVR and one subresource. The status codes of all three, and
// of both methods, are TestRestoreHandler_GenericDomainSubresourcesRemoved's subject.
func TestRestoreHandler_ForeignGVRRefusalNamesNoGroup(t *testing.T) {
	log, _ := logger.NewLogger("error")
	handler := NewRestoreHandler(nil, nil, log, nil, nil)
	mux := http.NewServeMux()
	handler.SetupRoutes(mux)
	server := httptest.NewServer(mux)
	defer server.Close()

	const subresource = "manifests-with-data-restoration"
	path := "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/ns/demovirtualdisksnapshots/disk-x/" + subresource

	resp, err := http.Get(server.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read refusal body: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s = %d, want 404: %s", path, resp.StatusCode, body)
	}

	var status metav1.Status
	if err := json.Unmarshal(body, &status); err != nil {
		t.Fatalf("refusal body is not a Status (%s): %v", body, err)
	}
	// Without this the group check below would also pass on an empty message, which is the one way to make
	// this test say nothing.
	if !strings.Contains(status.Message, subresource) {
		t.Fatalf("the refusal does not name the subresource that was asked for (%q): %q", subresource, status.Message)
	}
	if strings.Contains(status.Message, "subresources.") {
		t.Fatalf("the refusal names an aggregated group: %q. It must state the rule only — this module knows no other module's group name, so a group in this text is a literal that goes stale and a client copies it into a request nobody serves",
			status.Message)
	}
	t.Logf("refusal checked: %q", status.Message)
}
