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
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/client-go/rest"
	utilversion "k8s.io/component-base/version"
)

// The kube-apiserver OpenAPI aggregation controllers poll /openapi/v2 and /openapi/v3 of every
// aggregated apiserver. Answering 404 there makes them retry and log an error for this APIService for
// as long as it exists, so these specs assert that genericapiserver — not the delegate mux, which has
// no such route — answers both paths.
//
// The specs drive newAggregatedServer, the same function production goes through, so a call to
// installOpenAPIRoutes lost in a refactor fails them. Only the config half is assembled here: in
// production it comes from buildAggregatedAPIServer, which cannot run outside a cluster (its delegated
// authn/authz read the extension-apiserver-authentication ConfigMap, its secure serving binds a port).
var _ = Describe("aggregated apiserver OpenAPI endpoints", func() {
	var (
		server         *genericapiserver.GenericAPIServer
		delegateCalled bool
	)

	BeforeEach(func() {
		delegateCalled = false
		delegate := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			// Mirrors production: the delegate is an http.ServeMux without an /openapi route, so a
			// request that reaches it is answered 404.
			delegateCalled = true
			w.WriteHeader(http.StatusNotFound)
		})

		scheme := runtime.NewScheme()
		metav1.AddToGroupVersion(scheme, schema.GroupVersion{Version: "v1"})
		codecs := serializer.NewCodecFactory(scheme)

		config := genericapiserver.NewConfig(codecs)
		config.EffectiveVersion = utilversion.DefaultBuildEffectiveVersion()
		// Stand-ins for what secure serving provides in production: Complete() derives the external
		// address from the bound port, and New() rejects a nil LoopbackClientConfig.
		config.ExternalAddress = "127.0.0.1:8443"
		config.LoopbackClientConfig = &rest.Config{Host: "https://127.0.0.1:8443"}

		var err error
		server, err = newAggregatedServer(config.Complete(nil), apiServerName, delegate)
		Expect(err).NotTo(HaveOccurred())

		// Production also calls PrepareRun before serving; it starts no listener.
		server.PrepareRun()
	})

	getWith := func(path string, headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		for name, value := range headers {
			req.Header.Set(name, value)
		}
		w := httptest.NewRecorder()
		server.Handler.ServeHTTP(w, req)
		return w
	}

	get := func(path string) *httptest.ResponseRecorder {
		// Reproduces the aggregation controller's own request, which asks for JSON explicitly.
		return getWith(path, map[string]string{"Accept": "application/json"})
	}

	It("serves an OpenAPI v2 document rather than falling through to the delegate", func() {
		w := get("/openapi/v2")

		Expect(w.Code).To(Equal(http.StatusOK))
		Expect(delegateCalled).To(BeFalse(), "/openapi/v2 must be served by genericapiserver, not the delegate mux")

		var doc struct {
			Swagger string `json:"swagger"`
			Info    struct {
				Title   string `json:"title"`
				Version string `json:"version"`
			} `json:"info"`
			Paths map[string]any `json:"paths"`
		}
		Expect(json.Unmarshal(w.Body.Bytes(), &doc)).To(Succeed())
		Expect(doc.Swagger).To(Equal("2.0"))
		Expect(doc.Info.Title).To(Equal(apiServerName))
		Expect(doc.Info.Version).NotTo(BeEmpty())
		// The document describes no path on purpose (see installOpenAPIRoutes); a path appearing here
		// would be published to the cluster-wide spec without anything backing it.
		Expect(doc.Paths).To(BeEmpty())
	})

	It("serves the OpenAPI v3 discovery document rather than falling through to the delegate", func() {
		w := get("/openapi/v3")

		Expect(w.Code).To(Equal(http.StatusOK))
		Expect(delegateCalled).To(BeFalse(), "/openapi/v3 must be served by genericapiserver, not the delegate mux")

		var doc struct {
			Paths map[string]any `json:"paths"`
		}
		Expect(json.Unmarshal(w.Body.Bytes(), &doc)).To(Succeed())
		// No group version is registered: the subresources are routes on the delegate mux, not typed
		// REST storage, so the discovery document is legitimately empty. It only has to exist.
		Expect(doc.Paths).To(BeEmpty())
	})

	It("still routes non-OpenAPI paths to the delegate", func() {
		w := get("/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1")

		Expect(delegateCalled).To(BeTrue())
		Expect(w.Code).To(Equal(http.StatusNotFound))
	})

	// The aggregation controller does not fetch the document the way a browser would: it negotiates.
	// Serving plain JSON was enough to stop the retry loop, but a client that asks for protobuf, for
	// compression or for a conditional refetch has to get a valid answer too, and none of that is code of
	// ours — it belongs to the serving library, which is exactly why a change of library or of how it is
	// wired here can drop it unnoticed.
	Context("content negotiation on /openapi/v2", func() {
		// Both spellings of the protobuf media type are accepted, and both are answered with the canonical
		// one. Clients in the field still send the older spelling with the "@" separator, so losing it
		// would break them while the newer one kept working.
		const canonicalProtobuf = "application/com.github.proto-openapi.spec.v2.v1.0+protobuf"

		for _, accept := range []string{
			"application/com.github.proto-openapi.spec.v2@v1.0+protobuf",
			canonicalProtobuf,
		} {
			It("answers protobuf as protobuf for Accept: "+accept, func() {
				w := getWith("/openapi/v2", map[string]string{"Accept": accept})

				Expect(w.Code).To(Equal(http.StatusOK))
				Expect(w.Header().Get("Content-Type")).To(Equal(canonicalProtobuf))
				Expect(w.Body.Len()).NotTo(BeZero(), "the protobuf document is empty")
				Expect(json.Valid(w.Body.Bytes())).To(BeFalse(), "the answer is JSON; the requested media type was ignored")
			})
		}

		It("gives a gzip-capable client a readable document", func() {
			w := getWith("/openapi/v2", map[string]string{
				"Accept":          "application/json",
				"Accept-Encoding": "gzip",
			})
			Expect(w.Code).To(Equal(http.StatusOK))

			body := w.Body.Bytes()
			// Compression belongs to the serving library, and it applies a minimum size below which the
			// body goes out as-is. This document is deliberately empty and falls under that minimum, so
			// BOTH outcomes are correct and the assertion is on what the client ends up holding, not on
			// which branch ran. Pinning Content-Encoding: gzip would test the library's threshold and would
			// start failing the day the document grows or the threshold moves.
			if w.Header().Get("Content-Encoding") == "gzip" {
				zr, err := gzip.NewReader(bytes.NewReader(body))
				Expect(err).NotTo(HaveOccurred(), "the answer announces gzip but is not gzip")
				body, err = io.ReadAll(zr)
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(json.Valid(body)).To(BeTrue(), "the document a gzip-capable client receives is not JSON")
		})

		It("answers a conditional refetch with 304 and no body", func() {
			first := get("/openapi/v2")
			etag := first.Header().Get("Etag")
			Expect(etag).NotTo(BeEmpty(), "no ETag on the document; a client cannot refetch conditionally")

			second := getWith("/openapi/v2", map[string]string{
				"Accept":        "application/json",
				"If-None-Match": etag,
			})
			Expect(second.Code).To(Equal(http.StatusNotModified))
			Expect(second.Body.Len()).To(BeZero(), "a 304 must carry no body")
		})
	})
})

// coreAggregatedGroup is the aggregated API group this module serves, and after the volume node's
// subresources moved to the module that owns the CSI VolumeSnapshot it is the only one. It is spelled here
// rather than derived from the chart on purpose: the specs below compare THREE independent statements of
// it — the chart's APIService, the discovery this server answers, and this literal — and deriving one from
// another would collapse the comparison into a tautology.
const coreAggregatedGroup = "subresources.state-snapshotter.deckhouse.io"

// coreAggregatedVersion is the version of that group, as registered by the chart and answered by discovery.
const coreAggregatedVersion = "v1alpha1"

// formerVolumeNodeGroup is the group the volume node's subresources used to be served under by this
// module. It is now served by the module that owns the kind, as a compatibility alias, and this module must
// answer nothing under it: a second server for one group is how a client ends up talking to whichever of
// them the aggregation layer happened to route it to.
const formerVolumeNodeGroup = "subresources.snapshot.storage.k8s.io"

// chartAPIService is one APIService object the chart registers.
type chartAPIService struct {
	name    string
	group   string
	version string
}

// chartAPIServices reads every APIService object this chart declares, along with the number of template
// files it went through.
//
// It walks the whole templates tree rather than the one file that declares them today: "this chart
// registers exactly one APIService" is a statement about the chart, and reading a single file would leave
// a second one added elsewhere invisible while the count still said one.
//
// Templates are read as text, not rendered: rendering needs the chart's values, while the three fields
// compared here are plain literals. Indentation is what tells them apart — the object name is a two-space
// key of metadata, and `service.name` (the Service behind the APIService) sits deeper — so the patterns are
// anchored to it rather than matching a key anywhere in the document.
//
// LIMIT: it reads files from disk OUTSIDE this Go module. Go does not know a test depends on them, so a run
// after editing a template can be answered from the test cache: any check of a template change must be run
// with -count=1, or it reports the previous verdict.
func chartAPIServices() (services []chartAPIService, filesRead int, err error) {
	root := filepath.Join("..", "..", "..", "..", "templates")
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("read %s: %w", path, readErr)
		}
		filesRead++
		// Helm comment blocks are prose about the group and the priorities; dropping them keeps a sentence
		// mentioning a key from being read as that key.
		text := regexp.MustCompile(`(?s)\{\{/\*.*?\*/\}\}`).ReplaceAllString(string(raw), "")

		for _, doc := range strings.Split(text, "\n---") {
			if !strings.Contains(doc, "kind: APIService") {
				continue
			}
			svc := chartAPIService{
				name:    soleMatch(doc, `(?m)^  name: (\S+)$`),
				group:   soleMatch(doc, `(?m)^  group: (\S+)$`),
				version: soleMatch(doc, `(?m)^  version: (\S+)$`),
			}
			if svc.name == "" || svc.group == "" || svc.version == "" {
				return fmt.Errorf("an APIService document of %s has no single name/group/version (read %+v)", path, svc)
			}
			services = append(services, svc)
		}
		return nil
	})
	switch {
	case err != nil:
		return nil, filesRead, err
	case filesRead == 0:
		return nil, 0, fmt.Errorf("%s has no template files — nothing was read", root)
	case len(services) == 0:
		return nil, filesRead, fmt.Errorf("%d template file(s) of %s declare no APIService — nothing was compared", filesRead, root)
	}
	return services, filesRead, nil
}

// soleMatch returns the single capture of pattern in text, or "" when it appears zero or several times.
// Refusing several is deliberate: a second occurrence means the anchor no longer identifies one field, and
// silently taking the first would compare the wrong one.
func soleMatch(text, pattern string) string {
	found := regexp.MustCompile(pattern).FindAllStringSubmatch(text, -1)
	if len(found) != 1 {
		return ""
	}
	return found[0][1]
}

// After the move this module serves ONE aggregated group, and three statements of that have to stay level
// with each other: what the chart registers as an APIService, what this server's discovery answers, and
// what the OpenAPI documents the aggregation controller downloads for that APIService contain. A drift
// between them is not a test failure in production — it is the control plane logging
// "failed to download <apiservice>" for as long as the object exists, or a client sent to an endpoint
// nobody serves.
var _ = Describe("the aggregated group this module still serves", func() {
	var server *genericapiserver.GenericAPIServer

	BeforeEach(func() {
		// The real routes, not a stub: what falls through to them is what a cluster reaches.
		delegate := newServedDiscoveryMux()

		scheme := runtime.NewScheme()
		metav1.AddToGroupVersion(scheme, schema.GroupVersion{Version: "v1"})
		codecs := serializer.NewCodecFactory(scheme)

		config := genericapiserver.NewConfig(codecs)
		config.EffectiveVersion = utilversion.DefaultBuildEffectiveVersion()
		config.ExternalAddress = "127.0.0.1:8443"
		config.LoopbackClientConfig = &rest.Config{Host: "https://127.0.0.1:8443"}

		var err error
		server, err = newAggregatedServer(config.Complete(nil), apiServerName, delegate)
		Expect(err).NotTo(HaveOccurred())
		server.PrepareRun()
	})

	serve := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Accept", "application/json")
		w := httptest.NewRecorder()
		server.Handler.ServeHTTP(w, req)
		return w
	}

	It("registers exactly one APIService, for that group", func() {
		services, filesRead, err := chartAPIServices()
		Expect(err).NotTo(HaveOccurred())
		AddReportEntry("template files read / APIService documents found", fmt.Sprintf("%d / %d", filesRead, len(services)))

		Expect(services).To(HaveLen(1), "this chart must register one APIService; the volume node's is registered by the module that owns the kind")
		Expect(services[0]).To(Equal(chartAPIService{
			name:    coreAggregatedVersion + "." + coreAggregatedGroup,
			group:   coreAggregatedGroup,
			version: coreAggregatedVersion,
		}))
	})

	It("answers discovery for the group version the chart registers", func() {
		services, _, err := chartAPIServices()
		Expect(err).NotTo(HaveOccurred())

		checked := 0
		for _, svc := range services {
			w := serve("/apis/" + svc.group + "/" + svc.version)
			Expect(w.Code).To(Equal(http.StatusOK), "the APIService %s points at a group version this server does not answer", svc.name)

			var list metav1.APIResourceList
			Expect(json.Unmarshal(w.Body.Bytes(), &list)).To(Succeed())
			Expect(list.GroupVersion).To(Equal(svc.group + "/" + svc.version))
			Expect(list.APIResources).NotTo(BeEmpty(), "an advertised group version with no resource is an APIService nobody can use")
			checked++
		}
		Expect(checked).NotTo(BeZero(), "no APIService was resolved against discovery")
		AddReportEntry("APIService group versions resolved against discovery", checked)
	})

	It("answers nothing under the group the volume node moved to its owner", func() {
		Expect(serve("/apis/" + formerVolumeNodeGroup + "/v1").Code).To(Equal(http.StatusNotFound))
		Expect(serve("/apis/" + formerVolumeNodeGroup).Code).To(Equal(http.StatusNotFound))
		Expect(serve("/apis/" + formerVolumeNodeGroup + "/v1/namespaces/ns/volumesnapshots/vs/manifests-with-data-restoration").Code).
			To(Equal(http.StatusNotFound))
	})

	It("publishes OpenAPI documents that describe nothing this server does not serve", func() {
		for _, path := range []string{"/openapi/v2", "/openapi/v3"} {
			w := serve(path)
			Expect(w.Code).To(Equal(http.StatusOK), "%s must answer for every APIService of this chart, or the control plane retries and logs forever", path)

			var doc struct {
				Paths map[string]any `json:"paths"`
			}
			Expect(json.Unmarshal(w.Body.Bytes(), &doc)).To(Succeed())
			// Both documents are empty by design (see installOpenAPIRoutes): the subresources are routes on
			// the delegate mux, not typed REST storage, so there is no generated schema to publish. That is
			// also what makes them trivially consistent with what is served, and it is the limit of this
			// expectation: the day a document does publish a path, this has to become a comparison of the
			// published group versions against the served ones, not a check that there are none.
			Expect(doc.Paths).To(BeEmpty(), "%s publishes a path; it would reach the cluster-wide spec with nothing behind it", path)
		}
	})
})
