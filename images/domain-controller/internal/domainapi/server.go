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

// Package domainapi hosts the domain controller's aggregated API server. It serves the demo snapshot
// kinds' restore subresources (manifests, manifests-with-data-restoration) by fetching BASE manifests
// from the core aggregated API server and applying the demo restore mutation in-process. It never reads
// SnapshotContent/ManifestCheckpoint, so the domain controller needs no RBAC on those resources.
package domainapi

import (
	"context"
	"net/http"
	"time"

	"github.com/deckhouse/state-snapshotter/images/domain-controller/internal/logger"
)

// domainAPIServerName identifies this aggregated apiserver in genericapiserver logging.
const domainAPIServerName = "state-snapshotter-domain-apiserver"

// Server is the domain aggregated API server. The connector-style restore subresource routes live on an
// http.ServeMux (handler); genericapiserver wraps that delegate with the aggregation-layer authn/authz
// handler chain at Start time (see buildAggregatedAPIServer).
type Server struct {
	addr        string
	handler     http.Handler
	logger      logger.LoggerInterface
	tlsCertFile string
	tlsKeyFile  string
}

// NewServer assembles the domain aggregated API server's route handlers. Authentication (front-proxy
// requestheader + TokenReview) and authorization (SubjectAccessReview) are provided by genericapiserver's
// delegated authn/authz, wired in Start; NewServer stays cheap and unit-testable (no port bind, no
// in-cluster lookup). tlsCertFile/tlsKeyFile are the serving cert/key (required at Start).
func NewServer(addr string, restoreSvc *RestoreService, log logger.LoggerInterface, tlsCertFile, tlsKeyFile string) *Server {
	mux := http.NewServeMux()
	handler := NewHandler(restoreSvc, log)
	handler.SetupRoutes(mux)

	return &Server{
		addr:        addr,
		handler:     loggingMiddleware(mux, log),
		logger:      log,
		tlsCertFile: tlsCertFile,
		tlsKeyFile:  tlsKeyFile,
	}
}

// Start builds the genericapiserver (delegated authn/authz) around the route handlers and runs it until
// ctx is cancelled. It blocks; on cancellation genericapiserver performs a graceful shutdown.
func (s *Server) Start(ctx context.Context) error {
	s.logger.Info("[domainapi] starting domain aggregated API server", "addr", s.addr)

	genericServer, err := buildAggregatedAPIServer(domainAPIServerName, s.addr, s.tlsCertFile, s.tlsKeyFile, s.handler)
	if err != nil {
		return err
	}

	s.logger.Info("[domainapi] delegated authn (requestheader + TokenReview) and authz (SubjectAccessReview) enabled", "addr", s.addr)
	return genericServer.PrepareRun().RunWithContext(ctx)
}

// loggingMiddleware logs non-trivial requests (errors at Info, the rest at Debug).
func loggingMiddleware(next http.Handler, log logger.LoggerInterface) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		wrapped := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(wrapped, r)

		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" || r.URL.Path == "/livez" {
			return
		}
		duration := time.Since(start)
		if wrapped.statusCode >= 400 {
			log.Info("[domainapi] HTTP request error", "method", r.Method, "path", r.URL.Path, "status", wrapped.statusCode, "duration", duration)
		} else {
			log.Debug("[domainapi] HTTP request", "method", r.Method, "path", r.URL.Path, "status", wrapped.statusCode, "duration", duration)
		}
	})
}

type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}
