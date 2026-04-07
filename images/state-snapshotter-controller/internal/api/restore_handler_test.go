package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

func TestRestoreHandler_RoutingInvalidPaths(t *testing.T) {
	log, _ := logger.NewLogger("error")
	handler := NewRestoreHandler(nil, nil, log)
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
	handler := NewRestoreHandler(nil, nil, log)
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
