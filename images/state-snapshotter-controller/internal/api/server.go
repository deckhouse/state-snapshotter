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
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase/restore"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshotgraphregistry"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

// apiServerName identifies this aggregated apiserver in genericapiserver logging.
const apiServerName = "state-snapshotter-apiserver"

// Server represents the aggregated API server. The connector-style subresource routes live on an
// http.ServeMux (handler); genericapiserver wraps that delegate with the aggregation-layer authn/authz
// handler chain at Start time (see buildAggregatedAPIServer).
type Server struct {
	addr           string
	handler        http.Handler
	archiveHandler *ArchiveHandler
	logger         logger.LoggerInterface
	tlsCertFile    string
	tlsKeyFile     string
}

// NewServer creates a new aggregated API server.
// directClient: used for all CRD operations (ManifestCheckpoint and chunks) without cache
// No informer cache needed - archive-api-service only does GET requests
// domainRestorer: delegates domain snapshot subtrees to the domain controller's aggregated apiserver
// (nil disables delegation; encountering a domain node then fails closed)
// isDomainKind: reports which snapshot kinds are domain-owned so the restore compiler delegates them
// tlsCertFile/tlsKeyFile: serving cert/key for the aggregated apiserver (required at Start)
//
// Authentication (front-proxy requestheader + TokenReview) and authorization (SubjectAccessReview) are
// provided by genericapiserver's delegated authn/authz, wired in Start; NewServer only assembles the
// route handlers so it stays cheap and unit-testable (no port bind, no in-cluster lookup).
func NewServer(addr string, _ client.Client, directClient client.Client, logger logger.LoggerInterface, graphRegistry snapshotgraphregistry.LiveReader, domainRestorer restore.DomainSubtreeRestorer, isDomainKind func(kind string) bool, tlsCertFile, tlsKeyFile string) *Server {
	// Create archive service with directClient for all operations
	// directClient is used for both ManifestCheckpoint and chunks to avoid informer requirements
	archiveService := usecase.NewArchiveService(directClient, directClient, logger)

	// Create archive handler with directClient for ManifestCheckpoint
	archiveHandler := NewArchiveHandler(directClient, archiveService, logger)
	// The restore compiler stays domain-free: domain snapshot subtrees are delegated to the domain
	// controller's aggregated apiserver (manifests-with-data-restoration) over the kube-apiserver
	// aggregation layer; generic nodes are compiled from core's own SnapshotContent.
	restoreService := restore.NewService(directClient, archiveService, domainRestorer, isDomainKind)
	nsAgg := usecase.NewAggregatedNamespaceManifests(directClient, archiveService, graphRegistry)
	importUpload := usecase.NewImportUploadService(directClient)
	restoreHandler := NewRestoreHandler(directClient, restoreService, logger, nsAgg, importUpload)

	// Setup routes
	mux := http.NewServeMux()
	archiveHandler.SetupRoutes(mux)
	restoreHandler.SetupRoutes(mux)
	// subresources.snapshot.storage.k8s.io connector over the generic-PVC extended VolumeSnapshot (C8).
	restoreHandler.SetupVolumeSnapshotConnectorRoutes(mux)

	return &Server{
		addr:           addr,
		handler:        loggingMiddleware(mux, logger),
		archiveHandler: archiveHandler,
		logger:         logger,
		tlsCertFile:    tlsCertFile,
		tlsKeyFile:     tlsKeyFile,
	}
}

// loggingMiddleware adds request logging to HTTP handlers
func loggingMiddleware(next http.Handler, logger logger.LoggerInterface) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Create a response writer wrapper to capture status code
		wrapped := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		// Call the next handler
		next.ServeHTTP(wrapped, r)

		// Skip logging for health check endpoints and API discovery endpoints
		// /openapi/v2, /openapi/v3, /apis are normal discovery requests from kube-apiserver
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" || r.URL.Path == "/livez" ||
			r.URL.Path == "/openapi/v2" || r.URL.Path == "/openapi/v3" || r.URL.Path == "/apis" {
			return
		}

		// Log the request (Debug level to reduce log noise, only log errors at Info level)
		duration := time.Since(start)
		if wrapped.statusCode >= 400 {
			logger.Info("HTTP request error",
				"method", r.Method,
				"path", r.URL.Path,
				"query", r.URL.RawQuery,
				"status", wrapped.statusCode,
				"duration", duration,
				"remote_addr", r.RemoteAddr,
				"user_agent", r.UserAgent())
		} else {
			logger.Debug("HTTP request",
				"method", r.Method,
				"path", r.URL.Path,
				"query", r.URL.RawQuery,
				"status", wrapped.statusCode,
				"duration", duration,
				"remote_addr", r.RemoteAddr,
				"user_agent", r.UserAgent())
		}
	})
}

// responseWriter wraps http.ResponseWriter to capture status code
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// Start builds the genericapiserver (delegated authn/authz) around the route handlers and runs it until
// ctx is cancelled. It blocks; on cancellation genericapiserver performs a graceful shutdown.
func (s *Server) Start(ctx context.Context) error {
	s.logger.Info("Starting aggregated API server", "addr", s.addr)

	// Start cache cleanup goroutine
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				s.archiveHandler.GetArchiveService().CleanupCache()
			case <-ctx.Done():
				return
			}
		}
	}()

	genericServer, err := buildAggregatedAPIServer(apiServerName, s.addr, s.tlsCertFile, s.tlsKeyFile, s.handler)
	if err != nil {
		return err
	}

	s.logger.Info("Aggregated API server: delegated authn (requestheader + TokenReview) and authz (SubjectAccessReview) enabled", "addr", s.addr)
	return genericServer.PrepareRun().RunWithContext(ctx)
}
