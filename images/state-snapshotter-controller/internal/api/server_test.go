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
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

func TestServer(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Server Suite")
}

var _ = Describe("NewServer", func() {
	var (
		log            logger.LoggerInterface
		fakeClient     client.Client
		tempDir        string
		serverCertFile string
		serverKeyFile  string
		caCertFile     string
		caKeyFile      string
	)

	BeforeEach(func() {
		var err error
		log, err = logger.NewLogger("ERROR")
		Expect(err).NotTo(HaveOccurred())

		fakeClient = fake.NewClientBuilder().Build()

		// Create temporary directory for certificates
		tempDir, err = os.MkdirTemp("", "mtls-test-*")
		Expect(err).NotTo(HaveOccurred())

		// Generate CA
		caKey, err := rsa.GenerateKey(rand.Reader, 2048)
		Expect(err).NotTo(HaveOccurred())

		caCert := &x509.Certificate{
			SerialNumber: big.NewInt(1),
			Subject: pkix.Name{
				CommonName: "test-ca",
			},
			NotBefore:             time.Now(),
			NotAfter:              time.Now().Add(365 * 24 * time.Hour),
			IsCA:                  true,
			KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
			BasicConstraintsValid: true,
		}

		caCertDER, err := x509.CreateCertificate(rand.Reader, caCert, caCert, &caKey.PublicKey, caKey)
		Expect(err).NotTo(HaveOccurred())

		caKeyFile = filepath.Join(tempDir, "ca.key")
		caKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(caKey)})
		err = os.WriteFile(caKeyFile, caKeyPEM, 0600)
		Expect(err).NotTo(HaveOccurred())

		caCertFile = filepath.Join(tempDir, "ca.crt")
		caCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCertDER})
		err = os.WriteFile(caCertFile, caCertPEM, 0644)
		Expect(err).NotTo(HaveOccurred())

		// Generate server certificate
		serverKey, err := rsa.GenerateKey(rand.Reader, 2048)
		Expect(err).NotTo(HaveOccurred())

		serverCert := &x509.Certificate{
			SerialNumber: big.NewInt(2),
			Subject: pkix.Name{
				CommonName: "server",
			},
			NotBefore:   time.Now(),
			NotAfter:    time.Now().Add(365 * 24 * time.Hour),
			KeyUsage:    x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}

		serverCertDER, err := x509.CreateCertificate(rand.Reader, serverCert, caCert, &serverKey.PublicKey, caKey)
		Expect(err).NotTo(HaveOccurred())

		serverKeyFile = filepath.Join(tempDir, "server.key")
		serverKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(serverKey)})
		err = os.WriteFile(serverKeyFile, serverKeyPEM, 0600)
		Expect(err).NotTo(HaveOccurred())

		serverCertFile = filepath.Join(tempDir, "server.crt")
		serverCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverCertDER})
		err = os.WriteFile(serverCertFile, serverCertPEM, 0644)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if tempDir != "" {
			os.RemoveAll(tempDir)
		}
	})

	Describe("mTLS is mandatory", func() {
		It("should fail to create server when CA is not provided", func() {
			server := NewServer(":8443", fakeClient, fakeClient, log, nil, serverCertFile, serverKeyFile, nil, nil)
			Expect(server).To(BeNil())
		})
	})

	Describe("mTLS mode", func() {
		It("should create server in mTLS mode when CA is provided", func() {
			caCertBytes, err := os.ReadFile(caCertFile)
			Expect(err).NotTo(HaveOccurred())
			server := NewServer(":8443", fakeClient, fakeClient, log, nil, serverCertFile, serverKeyFile, caCertBytes, []string{"system:kube-apiserver"})
			Expect(server).NotTo(BeNil())
			Expect(server.server.TLSConfig).NotTo(BeNil())
			Expect(server.server.TLSConfig.ClientAuth).To(Equal(tls.RequireAndVerifyClientCert))
			Expect(server.server.TLSConfig.ClientCAs).NotTo(BeNil())
		})

		It("should fail to create server when CA is invalid", func() {
			invalidCA := []byte("invalid certificate data")
			server := NewServer(":8443", fakeClient, fakeClient, log, nil, serverCertFile, serverKeyFile, invalidCA, []string{"system:kube-apiserver"})
			Expect(server).To(BeNil())
		})

		It("should apply mTLS middleware when CA and allowed CNs are provided", func() {
			caCertBytes, err := os.ReadFile(caCertFile)
			Expect(err).NotTo(HaveOccurred())
			server := NewServer(":8443", fakeClient, fakeClient, log, nil, serverCertFile, serverKeyFile, caCertBytes, []string{"system:kube-apiserver"})
			Expect(server).NotTo(BeNil())
			// Middleware is applied, we can't directly check it, but we can test via HTTP requests
		})
	})
})

var _ = Describe("mTLSMiddleware", func() {
	var (
		log        logger.LoggerInterface
		handler    http.Handler
		allowedCNs []string
	)

	BeforeEach(func() {
		var err error
		log, err = logger.NewLogger("ERROR")
		Expect(err).NotTo(HaveOccurred())

		allowedCNs = []string{"system:kube-apiserver", "kubernetes"}

		// Create a simple handler
		handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("OK"))
		})
	})

	Describe("Health endpoints", func() {
		It("should allow access to /healthz without client certificate", func() {
			middleware := mTLSMiddleware(handler, log, allowedCNs)
			req := httptest.NewRequest("GET", "/healthz", nil)
			req.TLS = &tls.ConnectionState{} // TLS connection but no client cert
			w := httptest.NewRecorder()

			middleware.ServeHTTP(w, req)

			Expect(w.Code).To(Equal(http.StatusOK))
		})

		It("should allow access to /livez without client certificate", func() {
			middleware := mTLSMiddleware(handler, log, allowedCNs)
			req := httptest.NewRequest("GET", "/livez", nil)
			req.TLS = &tls.ConnectionState{}
			w := httptest.NewRecorder()

			middleware.ServeHTTP(w, req)

			Expect(w.Code).To(Equal(http.StatusOK))
		})

		It("should allow access to /readyz without client certificate", func() {
			middleware := mTLSMiddleware(handler, log, allowedCNs)
			req := httptest.NewRequest("GET", "/readyz", nil)
			req.TLS = &tls.ConnectionState{}
			w := httptest.NewRecorder()

			middleware.ServeHTTP(w, req)

			Expect(w.Code).To(Equal(http.StatusOK))
		})
	})

	Describe("Non-TLS connections", func() {
		It("should reject non-TLS connections", func() {
			middleware := mTLSMiddleware(handler, log, allowedCNs)
			req := httptest.NewRequest("GET", "/api/test", nil)
			// req.TLS is nil
			w := httptest.NewRecorder()

			middleware.ServeHTTP(w, req)

			Expect(w.Code).To(Equal(http.StatusBadRequest))
			Expect(w.Body.String()).To(ContainSubstring("TLS required"))
		})
	})

	Describe("Client certificate validation", func() {
		It("should reject requests without client certificate", func() {
			middleware := mTLSMiddleware(handler, log, allowedCNs)
			req := httptest.NewRequest("GET", "/api/test", nil)
			req.TLS = &tls.ConnectionState{
				PeerCertificates: []*x509.Certificate{}, // Empty certificates
			}
			w := httptest.NewRecorder()

			middleware.ServeHTTP(w, req)

			Expect(w.Code).To(Equal(http.StatusUnauthorized))
			Expect(w.Body.String()).To(ContainSubstring("Client certificate required"))
		})

		It("should reject requests with disallowed CN", func() {
			middleware := mTLSMiddleware(handler, log, allowedCNs)

			// Create certificate with disallowed CN
			cert := &x509.Certificate{
				Subject: pkix.Name{
					CommonName: "unauthorized-client",
				},
			}

			req := httptest.NewRequest("GET", "/api/test", nil)
			req.TLS = &tls.ConnectionState{
				PeerCertificates: []*x509.Certificate{cert},
			}
			w := httptest.NewRecorder()

			middleware.ServeHTTP(w, req)

			Expect(w.Code).To(Equal(http.StatusForbidden))
			Expect(w.Body.String()).To(ContainSubstring("Invalid client certificate"))
		})

		It("should allow requests with allowed CN", func() {
			middleware := mTLSMiddleware(handler, log, allowedCNs)

			// Create certificate with allowed CN
			cert := &x509.Certificate{
				Subject: pkix.Name{
					CommonName: "system:kube-apiserver",
				},
			}

			req := httptest.NewRequest("GET", "/api/test", nil)
			req.TLS = &tls.ConnectionState{
				PeerCertificates: []*x509.Certificate{cert},
			}
			w := httptest.NewRecorder()

			middleware.ServeHTTP(w, req)

			Expect(w.Code).To(Equal(http.StatusOK))
		})

		It("should allow requests with allowed CN in SANs", func() {
			middleware := mTLSMiddleware(handler, log, allowedCNs)

			// Create certificate with allowed CN in SANs
			cert := &x509.Certificate{
				Subject: pkix.Name{
					CommonName: "other-cn",
				},
				DNSNames: []string{"kubernetes"},
			}

			req := httptest.NewRequest("GET", "/api/test", nil)
			req.TLS = &tls.ConnectionState{
				PeerCertificates: []*x509.Certificate{cert},
			}
			w := httptest.NewRecorder()

			middleware.ServeHTTP(w, req)

			Expect(w.Code).To(Equal(http.StatusOK))
		})
	})
})
