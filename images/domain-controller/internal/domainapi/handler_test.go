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

package domainapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newTestHandlerMux(t *testing.T, fetcher CoreBaseManifestsFetcher) *http.ServeMux {
	t.Helper()
	// Seed a Ready dsnap-a so the restore subresource passes the readiness gate and the routing
	// assertions exercise the handler, not the gate.
	reader := fake.NewClientBuilder().WithScheme(domainScheme(t)).WithObjects(diskSnapWithReady(true)).Build()
	svc := NewRestoreService(reader, fetcher, nil)
	mux := http.NewServeMux()
	NewHandler(svc, nil).SetupRoutes(mux)
	return mux
}

func TestHandleSubtree_Routing(t *testing.T) {
	mux := newTestHandlerMux(t, stubFetcher{})

	cases := []struct {
		name       string
		method     string
		path       string
		wantStatus int
	}{
		{"group discovery", http.MethodGet, "/apis/" + domainGroup, http.StatusOK},
		{"version discovery", http.MethodGet, "/apis/" + domainGroup + "/v1alpha1", http.StatusOK},
		{"disk manifests", http.MethodGet, "/apis/" + domainGroup + "/v1alpha1/namespaces/ns1/demovirtualdisksnapshots/dsnap-a/manifests", http.StatusOK},
		{"restore subresource", http.MethodGet, "/apis/" + domainGroup + "/v1alpha1/namespaces/ns1/demovirtualdisksnapshots/dsnap-a/manifests-with-data-restoration", http.StatusOK},
		{"method not allowed", http.MethodPost, "/apis/" + domainGroup + "/v1alpha1/namespaces/ns1/demovirtualdisksnapshots/dsnap-a/manifests", http.StatusMethodNotAllowed},
		{"unsupported resource", http.MethodGet, "/apis/" + domainGroup + "/v1alpha1/namespaces/ns1/configmaps/x/manifests", http.StatusNotFound},
		{"unknown subresource", http.MethodGet, "/apis/" + domainGroup + "/v1alpha1/namespaces/ns1/demovirtualdisksnapshots/dsnap-a/whatever", http.StatusNotFound},
		{"invalid namespace", http.MethodGet, "/apis/" + domainGroup + "/v1alpha1/namespaces/Bad_NS/demovirtualdisksnapshots/dsnap-a/manifests", http.StatusBadRequest},
		{"too few segments", http.MethodGet, "/apis/" + domainGroup + "/v1alpha1/namespaces/ns1/demovirtualdisksnapshots", http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, tc.path, nil)
			mux.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("%s %s: got status %d want %d (body=%s)", tc.method, tc.path, rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}

func TestHealthEndpoint(t *testing.T) {
	mux := newTestHandlerMux(t, stubFetcher{})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz: got %d", rec.Code)
	}
}
