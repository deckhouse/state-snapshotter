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
	"net/http"
	"net/http/httptest"
	"testing"

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
