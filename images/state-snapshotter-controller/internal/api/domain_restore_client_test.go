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
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

// volumeSnapshotDelegateGVK is the snapshot kind whose restore this module delegates to the module that
// owns it: a volume node inside a tree.
func volumeSnapshotDelegateGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: snapshot.CSISnapshotGroup, Version: snapshot.CSISnapshotVersion, Kind: snapshot.KindVolumeSnapshot}
}

// demoDelegateGVK is a snapshot kind of the ordinary shape — its objects live in the API group of the
// module that also serves their subresources — so the mechanical rule applies to it. The group is
// synthetic on purpose: no real module's name should decide whether this passes.
func demoDelegateGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: "widgets.example.com", Version: "v1alpha1", Kind: "WidgetSnapshot"}
}

// delegateRESTMapper resolves both kinds above to their plural resource, which is what the request path
// carries. The real client gets this from discovery.
func delegateRESTMapper() meta.RESTMapper {
	m := meta.NewDefaultRESTMapper(nil)
	m.Add(volumeSnapshotDelegateGVK(), meta.RESTScopeNamespace)
	m.Add(demoDelegateGVK(), meta.RESTScopeNamespace)
	return m
}

// recordedDelegateRequest is what a stand-in owner apiserver saw.
type recordedDelegateRequest struct {
	path  string
	query url.Values
}

// newDelegateAgainst builds a DomainRestoreClient pointed at a stand-in owner apiserver and returns the
// requests it receives. respond writes the answer; count is the number of requests that reached it.
func newDelegateAgainst(respond func(w http.ResponseWriter, r *http.Request)) (*DomainRestoreClient, *httptest.Server, *atomic.Int32, *[]recordedDelegateRequest) {
	var count atomic.Int32
	seen := &[]recordedDelegateRequest{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		*seen = append(*seen, recordedDelegateRequest{path: r.URL.Path, query: r.URL.Query()})
		respond(w, r)
	}))

	log, err := logger.NewLogger("error")
	Expect(err).NotTo(HaveOccurred())
	client, err := NewDomainRestoreClient(&rest.Config{Host: srv.URL}, delegateRESTMapper(), log)
	Expect(err).NotTo(HaveOccurred())
	return client, srv, &count, seen
}

var _ = Describe("subresourceGroupVersionFor", func() {
	It("prefixes the CR's own group for a kind whose module owns that group", func() {
		gvk := demoDelegateGVK()
		Expect(subresourceGroupVersionFor(gvk)).To(Equal(schema.GroupVersion{
			Group:   "subresources.widgets.example.com",
			Version: "v1alpha1",
		}))
	})

	It("addresses the owning module's own group for the CSI VolumeSnapshot, not the prefixed CSI group", func() {
		gvk := volumeSnapshotDelegateGVK()
		got := subresourceGroupVersionFor(gvk)

		Expect(got).To(Equal(schema.GroupVersion{
			Group:   "subresources.storage-foundation.deckhouse.io",
			Version: "v1",
		}))
		// Stated as the rule rather than as a second literal: the CSI group belongs to no module, so
		// prefixing it names a group nobody serves. Comparing against the computed value is what keeps
		// this from passing if the override is dropped AND the expectation above is edited to match.
		Expect(got.Group).NotTo(Equal(domainSubresourceGroupPrefix + gvk.Group))
	})
})

var _ = Describe("DomainRestoreClient.RestoreDomainSubtree", func() {
	It("requests the volume node's restore subresource in the owning module's group", func() {
		client, srv, count, seen := newDelegateAgainst(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"apiVersion":"v1","kind":"PersistentVolumeClaim","metadata":{"name":"restored"}}]`))
		})
		defer srv.Close()

		objs, err := client.RestoreDomainSubtree(context.Background(), volumeSnapshotDelegateGVK(), "src-ns", "vs-1", "restore-ns")
		Expect(err).NotTo(HaveOccurred())
		Expect(objs).To(HaveLen(1))
		Expect(objs[0].GetName()).To(Equal("restored"))

		// The whole point of this spec is the ADDRESS, so the path is asserted verbatim rather than
		// "the delegate was called": the group cannot be derived from the kind (see
		// subresourceGroupVersionFor), so a wrong group is a request that 404s in a live cluster while
		// every call-count assertion stays green.
		Expect(count.Load()).To(Equal(int32(1)))
		Expect((*seen)[0].path).To(Equal(
			"/apis/subresources.storage-foundation.deckhouse.io/v1/namespaces/src-ns/volumesnapshots/vs-1/manifests-with-data-restoration"))
		Expect((*seen)[0].query.Get("targetNamespace")).To(Equal("restore-ns"))
		// Setting the per-call budget also states it in the request, so the value reaches the wire and is
		// observable here — this is what pins the 30s decision to the request rather than to a constant
		// nobody reads.
		Expect((*seen)[0].query.Get("timeout")).To(Equal(delegateCallTimeout.String()))
		Expect(delegateCallTimeout).To(Equal(30 * time.Second))
	})

	It("aborts a call that outlives the budget instead of waiting for the owner", func() {
		release := make(chan struct{})
		client, srv, count, _ := newDelegateAgainst(func(w http.ResponseWriter, _ *http.Request) {
			<-release
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		})
		defer srv.Close()
		// Bounded, so that a client which does NOT apply the budget fails this spec by succeeding rather
		// than by hanging until the suite's own deadline.
		go func() {
			time.Sleep(2 * time.Second)
			close(release)
		}()

		client.timeout = 100 * time.Millisecond
		start := time.Now()
		_, err := client.RestoreDomainSubtree(context.Background(), volumeSnapshotDelegateGVK(), "src-ns", "vs-1", "restore-ns")
		Expect(err).To(HaveOccurred())
		Expect(time.Since(start)).To(BeNumerically("<", 2*time.Second))
		Expect(count.Load()).To(Equal(int32(1)))
	})

	It("does not retry a Retry-After refusal, however inviting the header", func() {
		client, srv, count, _ := newDelegateAgainst(func(w http.ResponseWriter, _ *http.Request) {
			// The exact shape client-go retries on by default, up to ten times: 429 plus a Retry-After it
			// honours. A delegated call must not be repeated — its sibling subresource is not idempotent —
			// so the only acceptable observation here is ONE request.
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","code":429}`))
		})
		defer srv.Close()

		_, err := client.RestoreDomainSubtree(context.Background(), volumeSnapshotDelegateGVK(), "src-ns", "vs-1", "restore-ns")
		Expect(err).To(HaveOccurred())
		Expect(count.Load()).To(Equal(int32(1)), "the delegated call must reach the owner exactly once")
	})
})
