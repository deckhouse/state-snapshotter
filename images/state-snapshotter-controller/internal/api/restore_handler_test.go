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
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

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

// TestRestoreHandler_GenericDomainManifestReadsRemoved pins that the core group no longer serves
// manifests-download / manifests-with-data-restoration for non-core (domain) snapshot kinds: both are
// 404 (address the node's own group), while the generic upload route and the core Snapshot /
// cluster-scoped SnapshotContent read routes stay alive (they reach their handlers, not the 404-unknown
// fall-through — with nil deps a live read handler returns 500, and the POST-only upload returns 405 on a GET).
func TestRestoreHandler_GenericDomainManifestReadsRemoved(t *testing.T) {
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
	base := "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1"

	// Generic (domain) GVR: manifests-download and manifests-with-data-restoration are refused with 404.
	if got := get(base + "/namespaces/ns/demovirtualmachinesnapshots/vm-x/manifests-download"); got != http.StatusNotFound {
		t.Fatalf("generic manifests-download = %d, want 404", got)
	}
	if got := get(base + "/namespaces/ns/demovirtualdisksnapshots/disk-x/manifests-with-data-restoration"); got != http.StatusNotFound {
		t.Fatalf("generic manifests-with-data-restoration = %d, want 404", got)
	}
	// Generic upload route stays alive: GET on the POST-only subresource is 405, not the 404 fall-through.
	if got := get(base + "/namespaces/ns/demovirtualmachinesnapshots/vm-x/manifests-and-children-refs-upload"); got != http.StatusMethodNotAllowed {
		t.Fatalf("generic upload GET = %d, want 405 (route alive)", got)
	}
	// Core Snapshot manifests-download stays alive: it reaches the handler (nil nsAggregated -> 500), not 404.
	if got := get(base + "/namespaces/ns/snapshots/snap/manifests-download"); got != http.StatusInternalServerError {
		t.Fatalf("core snapshots manifests-download = %d, want 500 (route alive)", got)
	}
	// Cluster-scoped SnapshotContent manifests-download stays alive (nil nsAggregated -> 500), not 404.
	if got := get(base + "/snapshotcontents/c/manifests-download"); got != http.StatusInternalServerError {
		t.Fatalf("cluster-scoped snapshotcontents manifests-download = %d, want 500 (route alive)", got)
	}
}

// TestRestoreHandler_WriteRestoreError_ForbiddenMapsTo403 pins that the restore resolver's anti-spoofing
// apierrors.NewForbidden surfaces as HTTP 403 (not the 500 catch-all).
func TestRestoreHandler_WriteRestoreError_ForbiddenMapsTo403(t *testing.T) {
	log, _ := logger.NewLogger("error")
	h := NewRestoreHandler(nil, nil, log, nil, nil)
	rec := httptest.NewRecorder()
	h.writeRestoreError(rec, apierrors.NewForbidden(
		schema.GroupResource{Group: "state-snapshotter.deckhouse.io", Resource: "snapshotcontents"}, "c",
		fmt.Errorf("bound SnapshotContent does not belong to snapshot ns/snap"),
	))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("writeRestoreError(Forbidden) status = %d, want 403", rec.Code)
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
