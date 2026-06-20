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
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"time"

	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

// Server is the domain aggregated API HTTP server.
type Server struct {
	server      *http.Server
	logger      logger.LoggerInterface
	tlsCertFile string
	tlsKeyFile  string
}

// NewServer builds the domain aggregated API server. When tlsCertFile/tlsKeyFile are set, mTLS is
// mandatory and caCert (the k8s-managed front-proxy CA) must be parseable, otherwise NewServer returns
// nil. allowedClientCNs gates accepted client certificate CNs/SANs (kube-apiserver front-proxy).
func NewServer(addr string, restoreSvc *RestoreService, log logger.LoggerInterface, tlsCertFile, tlsKeyFile string, caCert []byte, allowedClientCNs []string) *Server {
	mux := http.NewServeMux()
	handler := NewHandler(restoreSvc, log)
	handler.SetupRoutes(mux)

	h := loggingMiddleware(mux, log)

	httpServer := &http.Server{
		Addr:         addr,
		Handler:      h,
		ReadTimeout:  5 * time.Minute,
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  60 * time.Second,
	}

	srv := &Server{
		server: httpServer,
		logger: log,
	}

	if tlsCertFile != "" && tlsKeyFile != "" {
		tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}

		if len(caCert) == 0 {
			log.Error(nil, "[domainapi] CA certificate is required for mTLS, server cannot start")
			return nil
		}
		caCertPool := x509.NewCertPool()
		if !caCertPool.AppendCertsFromPEM(caCert) {
			log.Error(nil, "[domainapi] failed to parse CA certificate, mTLS configuration failed")
			return nil
		}
		tlsConfig.ClientAuth = tls.RequireAndVerifyClientCert
		tlsConfig.ClientCAs = caCertPool

		if len(allowedClientCNs) > 0 {
			h = mTLSMiddleware(h, log, allowedClientCNs)
			httpServer.Handler = h
		}

		log.Info("[domainapi] mTLS enabled for domain API server (client certificate required)", "allowed_cns", allowedClientCNs)

		httpServer.TLSConfig = tlsConfig
		srv.tlsCertFile = tlsCertFile
		srv.tlsKeyFile = tlsKeyFile
	}

	return srv
}

// Start runs the server until ctx is cancelled, then shuts it down gracefully.
func (s *Server) Start(ctx context.Context) error {
	s.logger.Info("[domainapi] starting domain API server", "addr", s.server.Addr)

	go func() {
		var err error
		if s.tlsCertFile != "" && s.tlsKeyFile != "" {
			s.logger.Info("[domainapi] starting domain API server with TLS", "addr", s.server.Addr)
			err = s.server.ListenAndServeTLS(s.tlsCertFile, s.tlsKeyFile)
		} else {
			s.logger.Info("[domainapi] starting domain API server without TLS", "addr", s.server.Addr)
			err = s.server.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			s.logger.Error(err, "[domainapi] failed to start domain API server")
		}
	}()

	<-ctx.Done()

	s.logger.Info("[domainapi] shutting down domain API server")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.server.Shutdown(shutdownCtx); err != nil {
		s.logger.Error(err, "[domainapi] failed to shutdown domain API server gracefully")
		return err
	}
	s.logger.Info("[domainapi] domain API server shutdown complete")
	return nil
}

// Stop shuts the server down using the provided context deadline.
func (s *Server) Stop(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}

// mTLSMiddleware enforces that the client certificate CN (or a DNS SAN) is in allowedCNs. Health probes
// are exempt. It mirrors internal/api so the domain server enforces the same front-proxy contract.
func mTLSMiddleware(next http.Handler, log logger.LoggerInterface, allowedCNs []string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" || r.URL.Path == "/livez" {
			next.ServeHTTP(w, r)
			return
		}
		if r.TLS == nil {
			http.Error(w, "TLS required", http.StatusBadRequest)
			return
		}
		if len(r.TLS.PeerCertificates) == 0 {
			http.Error(w, "Client certificate required", http.StatusUnauthorized)
			return
		}
		cert := r.TLS.PeerCertificates[0]
		if !cnAllowed(cert.Subject.CommonName, cert.DNSNames, allowedCNs) {
			log.Warning("[domainapi] client certificate CN/SAN not allowed", "cn", cert.Subject.CommonName, "sans", cert.DNSNames, "path", r.URL.Path, "allowed_cns", allowedCNs)
			http.Error(w, "Invalid client certificate", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func cnAllowed(cn string, sans, allowedCNs []string) bool {
	in := func(name string) bool {
		for _, allowed := range allowedCNs {
			if name == allowed {
				return true
			}
		}
		return false
	}
	if in(cn) {
		return true
	}
	for _, san := range sans {
		if in(san) {
			return true
		}
	}
	return false
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
