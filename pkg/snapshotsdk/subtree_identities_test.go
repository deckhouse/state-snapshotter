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

package snapshotsdk

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// subtreeChildGV is the GVK the tests give child snapshot refs. The core Snapshot kind is used because it
// is registered in the SDK test scheme; the SDK resolution logic (read status.boundSnapshotContentName via
// unstructured) is kind-agnostic, so this stands in for any domain child kind.
var subtreeChildGV = storagev1alpha1.SchemeGroupVersion.String()

// contentResponse is a canned subresource reply keyed by SnapshotContent name.
type contentResponse struct {
	status     int
	identities []SubtreeManifestIdentity
}

// newSubtreeTestServer serves the subtree-manifest-identities subresource, branching on the SnapshotContent
// name in the request path, and returns a real rest.Interface pointed at it.
func newSubtreeTestServer(t *testing.T, responses map[string]contentResponse) (rest.Interface, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := contentNameFromSubtreePath(r.URL.Path)
		resp, ok := responses[name]
		if !ok {
			writeStatus(w, http.StatusNotFound, "NotFound", "no canned response for "+name)
			return
		}
		if resp.status != 0 && resp.status != http.StatusOK {
			writeStatus(w, resp.status, "Conflict", "subtree not fully persisted")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(SubtreeManifestIdentitiesResponse{Identities: resp.identities})
	}))

	cfg := &rest.Config{
		Host: srv.URL,
		ContentConfig: rest.ContentConfig{
			GroupVersion:         &schema.GroupVersion{Group: "subresources.state-snapshotter.deckhouse.io", Version: "v1alpha1"},
			NegotiatedSerializer: serializer.NewCodecFactory(runtime.NewScheme()).WithoutConversion(),
		},
	}
	rc, err := rest.RESTClientFor(cfg)
	if err != nil {
		srv.Close()
		t.Fatalf("RESTClientFor: %v", err)
	}
	return rc, srv.Close
}

func contentNameFromSubtreePath(path string) string {
	// .../snapshotcontents/<name>/subtree-manifest-identities
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i, p := range parts {
		if p == "snapshotcontents" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

func writeStatus(w http.ResponseWriter, code int, reason, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"kind": "Status", "apiVersion": "v1", "status": "Failure",
		"reason": reason, "code": code, "message": msg,
	})
}

// subtreeAdapter is a minimal SnapshotAdapter carrying a namespace and a set of direct child refs.
type subtreeAdapter struct {
	refreshTestAdapter
}

func newSubtreeAdapter(namespace string, children ...SnapshotChildRef) *subtreeAdapter {
	a := &subtreeAdapter{}
	a.obj = &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "root"}}
	a.domain = DomainCaptureState{ChildrenSnapshotRefs: children}
	return a
}

func childRef(name string) SnapshotChildRef {
	return SnapshotChildRef{APIVersion: subtreeChildGV, Kind: "Snapshot", Name: name}
}

func childSnapshot(name, boundContent string) *storagev1alpha1.Snapshot {
	s := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: name},
		Status:     storagev1alpha1.SnapshotStatus{BoundSnapshotContentName: boundContent},
	}
	s.SetGroupVersionKind(storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot"))
	return s
}

func id(name string) SubtreeManifestIdentity {
	return SubtreeManifestIdentity{APIVersion: "v1", Kind: "ConfigMap", Namespace: "ns1", Name: name, UID: "uid-" + name}
}

// TestSubtreeManifestIdentities_UnionsAndDedups verifies the SDK resolves each child's bound content, calls
// the subresource per child, and returns the de-duplicated union across children (a shared object returned
// by two children appears once).
func TestSubtreeManifestIdentities_UnionsAndDedups(t *testing.T) {
	scheme := newRefreshTestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(childSnapshot("child-a", "content-a"), childSnapshot("child-b", "content-b")).
		Build()

	rc, closeSrv := newSubtreeTestServer(t, map[string]contentResponse{
		"content-a": {identities: []SubtreeManifestIdentity{id("obj-1"), id("shared")}},
		"content-b": {identities: []SubtreeManifestIdentity{id("shared"), id("obj-2")}},
	})
	defer closeSrv()

	s := New(cl, nil, nil, WithSubresourceREST(rc))
	adapter := newSubtreeAdapter("ns1", childRef("child-a"), childRef("child-b"))

	got, err := s.SubtreeManifestIdentities(context.Background(), adapter)
	if err != nil {
		t.Fatalf("SubtreeManifestIdentities: %v", err)
	}
	names := map[string]int{}
	for _, i := range got {
		names[i.Name]++
	}
	if len(got) != 3 {
		t.Fatalf("want 3 unioned identities, got %d: %v", len(got), got)
	}
	for _, want := range []string{"obj-1", "obj-2", "shared"} {
		if names[want] != 1 {
			t.Fatalf("identity %q count = %d, want exactly 1 (union+dedup): %v", want, names[want], got)
		}
	}
}

// TestSubtreeManifestIdentities_NoChildrenEmptySet verifies a childless aggregator returns an empty set
// without needing a REST client wired.
func TestSubtreeManifestIdentities_NoChildrenEmptySet(t *testing.T) {
	scheme := newRefreshTestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	s := New(cl, nil, nil) // no WithSubresourceREST on purpose
	adapter := newSubtreeAdapter("ns1")

	got, err := s.SubtreeManifestIdentities(context.Background(), adapter)
	if err != nil {
		t.Fatalf("SubtreeManifestIdentities: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want empty set for a childless node, got %v", got)
	}
}

// TestSubtreeManifestIdentities_FailClosedOn409 verifies a 409 from the subresource (some subtree
// checkpoint not Ready) surfaces as ErrSubtreeIdentitiesPending, not a partial set.
func TestSubtreeManifestIdentities_FailClosedOn409(t *testing.T) {
	scheme := newRefreshTestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(childSnapshot("child-a", "content-a")).
		Build()

	rc, closeSrv := newSubtreeTestServer(t, map[string]contentResponse{
		"content-a": {status: http.StatusConflict},
	})
	defer closeSrv()

	s := New(cl, nil, nil, WithSubresourceREST(rc))
	adapter := newSubtreeAdapter("ns1", childRef("child-a"))

	_, err := s.SubtreeManifestIdentities(context.Background(), adapter)
	if !errors.Is(err, ErrSubtreeIdentitiesPending) {
		t.Fatalf("want ErrSubtreeIdentitiesPending on 409, got %v", err)
	}
}

// TestSubtreeManifestIdentities_UnboundChildPending verifies a child that has not yet bound its content
// (empty status.boundSnapshotContentName) is fail-closed to ErrSubtreeIdentitiesPending.
func TestSubtreeManifestIdentities_UnboundChildPending(t *testing.T) {
	scheme := newRefreshTestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(childSnapshot("child-a", "")).
		Build()

	rc, closeSrv := newSubtreeTestServer(t, map[string]contentResponse{})
	defer closeSrv()

	s := New(cl, nil, nil, WithSubresourceREST(rc))
	adapter := newSubtreeAdapter("ns1", childRef("child-a"))

	_, err := s.SubtreeManifestIdentities(context.Background(), adapter)
	if !errors.Is(err, ErrSubtreeIdentitiesPending) {
		t.Fatalf("want ErrSubtreeIdentitiesPending for an unbound child, got %v", err)
	}
}

// TestSubtreeManifestIdentities_MissingChildPending verifies a child ref pointing at a not-yet-materialized
// snapshot (NotFound) is fail-closed to ErrSubtreeIdentitiesPending.
func TestSubtreeManifestIdentities_MissingChildPending(t *testing.T) {
	scheme := newRefreshTestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).Build() // child-a absent

	rc, closeSrv := newSubtreeTestServer(t, map[string]contentResponse{})
	defer closeSrv()

	s := New(cl, nil, nil, WithSubresourceREST(rc))
	adapter := newSubtreeAdapter("ns1", childRef("child-a"))

	_, err := s.SubtreeManifestIdentities(context.Background(), adapter)
	if !errors.Is(err, ErrSubtreeIdentitiesPending) {
		t.Fatalf("want ErrSubtreeIdentitiesPending for a missing child, got %v", err)
	}
}

// TestSubtreeManifestIdentities_RequiresRESTClient verifies that an aggregator WITH children but no wired
// subresource client gets a clear configuration error (distinct from the pending sentinel).
func TestSubtreeManifestIdentities_RequiresRESTClient(t *testing.T) {
	scheme := newRefreshTestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(childSnapshot("child-a", "content-a")).
		Build()

	s := New(cl, nil, nil) // no WithSubresourceREST
	adapter := newSubtreeAdapter("ns1", childRef("child-a"))

	_, err := s.SubtreeManifestIdentities(context.Background(), adapter)
	if err == nil || errors.Is(err, ErrSubtreeIdentitiesPending) {
		t.Fatalf("want a configuration error, got %v", err)
	}
	if !strings.Contains(err.Error(), "subresource REST client") {
		t.Fatalf("error should mention the missing REST client, got %v", err)
	}
}
