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

// These tests cover the cheap NewServer assembly (route handlers). The aggregation-layer authn/authz
// chain is wired by genericapiserver in Start (requires in-cluster config + a bound port) and is not
// exercised here.
var _ = Describe("NewServer", func() {
	var (
		log        logger.LoggerInterface
		fakeClient client.Client
	)

	BeforeEach(func() {
		var err error
		log, err = logger.NewLogger("ERROR")
		Expect(err).NotTo(HaveOccurred())
		fakeClient = fake.NewClientBuilder().Build()
	})

	It("builds a server whose handler is wired", func() {
		server := NewServer(":8443", fakeClient, fakeClient, log, nil, nil, nil, "", "")
		Expect(server).NotTo(BeNil())
		Expect(server.handler).NotTo(BeNil())
	})

	It("serves the aggregated API group discovery through the delegate handler", func() {
		server := NewServer(":8443", fakeClient, fakeClient, log, nil, nil, nil, "", "")
		req := httptest.NewRequest(http.MethodGet, "/apis/subresources.state-snapshotter.deckhouse.io", nil)
		w := httptest.NewRecorder()

		server.handler.ServeHTTP(w, req)

		Expect(w.Code).To(Equal(http.StatusOK))
		Expect(w.Body.String()).To(ContainSubstring("subresources.state-snapshotter.deckhouse.io"))
	})
})
