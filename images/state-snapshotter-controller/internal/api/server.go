/*
Copyright 2025 Flant JSC

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

package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

// Server represents the HTTP API server
type Server struct {
	server         *http.Server
	archiveHandler *ArchiveHandler
	logger         logger.LoggerInterface
	tlsCertFile    string
	tlsKeyFile     string
}

// NewServer creates a new HTTP API server
// directClient: used for all CRD operations (ManifestCheckpoint and chunks) without cache
// No informer cache needed - archive-api-service only does GET requests
// caCert: optional CA certificate bytes for mTLS (if provided, mTLS is mandatory - no fallback)
// allowedClientCNs: list of allowed client certificate CNs for mTLS (comma-separated)
// Returns nil if caCert is specified but cannot be parsed
func NewServer(addr string, _ client.Client, directClient client.Client, logger logger.LoggerInterface, tlsCertFile, tlsKeyFile string, caCert []byte, allowedClientCNs []string) *Server {
	// Create archive service with directClient for all operations
	// directClient is used for both ManifestCheckpoint and chunks to avoid informer requirements
	archiveService := usecase.NewArchiveService(directClient, directClient, logger)

	// Create archive handler with directClient for ManifestCheckpoint
	archiveHandler := NewArchiveHandler(directClient, archiveService, logger)

	// Setup routes
	mux := http.NewServeMux()
	archiveHandler.SetupRoutes(mux)

	// Add logging middleware
	handler := loggingMiddleware(mux, logger)

	server := &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  5 * time.Minute, // Increased for large manifest requests
		WriteTimeout: 5 * time.Minute, // Increased for large manifest responses
		IdleTimeout:  60 * time.Second,
	}

	srv := &Server{
		server:         server,
		archiveHandler: archiveHandler,
		logger:         logger,
	}

	// Configure TLS and mTLS
	if tlsCertFile != "" && tlsKeyFile != "" {
		tlsConfig := &tls.Config{
			MinVersion: tls.VersionTLS12,
		}

		// Configure mTLS - CA certificate is mandatory
		if len(caCert) == 0 {
			logger.Error(nil, "CA certificate is required for mTLS, server cannot start")
			return nil
		}

		caCertPool := x509.NewCertPool()
		if !caCertPool.AppendCertsFromPEM(caCert) {
			logger.Error(nil, "Failed to parse CA certificate, mTLS configuration failed")
			// Fail if CA cannot be parsed
			return nil
		}

		// mTLS is required - no fallback
		tlsConfig.ClientAuth = tls.RequireAndVerifyClientCert
		tlsConfig.ClientCAs = caCertPool

		// Apply mTLS middleware
		if len(allowedClientCNs) > 0 {
			handler = mTLSMiddleware(handler, logger, allowedClientCNs)
			server.Handler = handler
		}

		logger.Info("mTLS enabled for API server (client certificate required)",
			"allowed_cns", allowedClientCNs)

		server.TLSConfig = tlsConfig
		srv.tlsCertFile = tlsCertFile
		srv.tlsKeyFile = tlsKeyFile
	}

	return srv
}

// mTLSMiddleware checks client certificate for mTLS authentication
// Health endpoints remain accessible for in-cluster probes (excluded from mTLS check)
func mTLSMiddleware(next http.Handler, logger logger.LoggerInterface, allowedCNs []string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip health check endpoints (for in-cluster probes)
		if r.URL.Path == "/healthz" || r.URL.Path == "/livez" ||
			r.URL.Path == "/readyz" || r.URL.Path == "/health" || r.URL.Path == "/ready" {
			next.ServeHTTP(w, r)
			return
		}

		// Check for TLS connection
		if r.TLS == nil {
			logger.Warning("Non-TLS connection attempt",
				"path", r.URL.Path,
				"remote_addr", r.RemoteAddr)
			http.Error(w, "TLS required", http.StatusBadRequest)
			return
		}

		// Check for client certificate
		if len(r.TLS.PeerCertificates) == 0 {
			logger.Warning("No client certificate provided",
				"path", r.URL.Path,
				"remote_addr", r.RemoteAddr)
			http.Error(w, "Client certificate required", http.StatusUnauthorized)
			return
		}

		// Verify client certificate CN
		cert := r.TLS.PeerCertificates[0]
		cn := cert.Subject.CommonName

		// Log client certificate details for debugging (Debug level to reduce log noise)
		logger.Debug("Client certificate received",
			"client_subject", cert.Subject.String(),
			"client_issuer", cert.Issuer.String(),
			"client_serial", cert.SerialNumber.String(),
			"client_cn", cn,
			"client_dns_names", cert.DNSNames,
			"client_ip_addresses", cert.IPAddresses,
			"client_not_before", cert.NotBefore.Format(time.RFC3339),
			"client_not_after", cert.NotAfter.Format(time.RFC3339),
			"client_is_ca", cert.IsCA,
			"path", r.URL.Path,
			"remote_addr", r.RemoteAddr)

		// Check CN and SANs (Subject Alternative Names)
		allowed := false
		checkCN := func(name string) bool {
			for _, allowedCN := range allowedCNs {
				if name == allowedCN {
					return true
				}
			}
			return false
		}

		if checkCN(cn) {
			allowed = true
		} else {
			// Check SANs
			for _, san := range cert.DNSNames {
				if checkCN(san) {
					allowed = true
					break
				}
			}
		}

		if !allowed {
			logger.Warning("Client certificate CN/SAN not allowed",
				"cn", cn,
				"sans", cert.DNSNames,
				"path", r.URL.Path,
				"remote_addr", r.RemoteAddr,
				"allowed_cns", allowedCNs)
			http.Error(w, "Invalid client certificate", http.StatusForbidden)
			return
		}

		logger.Debug("mTLS authentication successful",
			"cn", cn,
			"path", r.URL.Path)

		next.ServeHTTP(w, r)
	})
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
		if r.URL.Path == "/health" || r.URL.Path == "/ready" || r.URL.Path == "/healthz" || r.URL.Path == "/livez" ||
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

// Start starts the HTTP server
func (s *Server) Start(ctx context.Context) error {
	s.logger.Info("Starting archive API server", "addr", s.server.Addr)

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

	// Start server in goroutine
	go func() {
		var err error
		if s.tlsCertFile != "" && s.tlsKeyFile != "" {
			s.logger.Info("Starting archive API server with TLS", "addr", s.server.Addr, "cert", s.tlsCertFile, "key", s.tlsKeyFile)
			err = s.server.ListenAndServeTLS(s.tlsCertFile, s.tlsKeyFile)
		} else {
			s.logger.Info("Starting archive API server without TLS", "addr", s.server.Addr)
			err = s.server.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			s.logger.Error(err, "Failed to start archive API server")
		}
	}()

	// Wait for context cancellation
	<-ctx.Done()

	s.logger.Info("Shutting down archive API server")

	// Create shutdown context with timeout
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Shutdown server
	shutdownStart := time.Now()
	if err := s.server.Shutdown(shutdownCtx); err != nil {
		s.logger.Error(err, "Failed to shutdown archive API server gracefully")
		return err
	}

	shutdownDuration := time.Since(shutdownStart)
	s.logger.Info("Archive API server shutdown complete", "duration", shutdownDuration)
	return nil
}

// Stop stops the HTTP server
func (s *Server) Stop(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}
