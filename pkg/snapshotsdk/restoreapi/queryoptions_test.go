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

package restoreapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk/transform"
)

// TestParseQueryOptions_Validation is the query contract table: valid inputs map to the expected
// transform.Options, and every invalid combination is an ErrBadRequest. Each of the four rules is listed
// with its own case (and each rejection case violates exactly ONE of them), so removing a single rule
// from the parser reddens its own case rather than a neighbour's.
func TestParseQueryOptions_Validation(t *testing.T) {
	cases := []struct {
		name       string
		query      string
		wantErr    bool
		wantScope  transform.Scope
		wantKind   string
		wantName   string
		wantAPIVer string
	}{
		{name: "no params -> subtree", query: "", wantScope: transform.ScopeSubtree},
		{name: "explicit subtree", query: "scope=subtree", wantScope: transform.ScopeSubtree},
		{name: "scope node", query: "scope=node", wantScope: transform.ScopeNode},
		{name: "node + filter", query: "scope=node&kind=ConfigMap&name=app", wantScope: transform.ScopeNode, wantKind: "ConfigMap", wantName: "app"},
		{name: "node + filter + apiVersion", query: "scope=node&kind=Gadget&name=g&apiVersion=a.example.com/v1", wantScope: transform.ScopeNode, wantKind: "Gadget", wantName: "g", wantAPIVer: "a.example.com/v1"},
		{name: "targetNamespace ignored by parser", query: "scope=node&targetNamespace=x", wantScope: transform.ScopeNode},

		{name: "unknown scope", query: "scope=bogus", wantErr: true},
		{name: "kind without name", query: "scope=node&kind=ConfigMap", wantErr: true},
		{name: "name without kind", query: "scope=node&name=app", wantErr: true},
		{name: "apiVersion without kind", query: "scope=node&apiVersion=v1", wantErr: true},
		{name: "filter with subtree", query: "scope=subtree&kind=ConfigMap&name=app", wantErr: true},
		{name: "filter with default scope", query: "kind=ConfigMap&name=app", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://x/y?"+tc.query, nil)
			opts, err := ParseQueryOptions(r)
			if tc.wantErr {
				if !errors.Is(err, transform.ErrBadRequest) {
					t.Fatalf("expected ErrBadRequest, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if opts.Scope != tc.wantScope {
				t.Fatalf("scope = %q, want %q", opts.Scope, tc.wantScope)
			}
			if opts.FilterKind != tc.wantKind || opts.FilterName != tc.wantName || opts.FilterAPIVersion != tc.wantAPIVer {
				t.Fatalf("filter = %q/%q/%q, want %q/%q/%q", opts.FilterKind, opts.FilterName, opts.FilterAPIVersion, tc.wantKind, tc.wantName, tc.wantAPIVer)
			}
		})
	}
}

// TestParseQueryOptions_LeavesSubjectToCaller pins the division of labour: the parser fills ONLY what the
// query controls. A parser that also invented the snapshot identity would give two servers of the same
// subresource different defaults for the fields their own routing decides — the identity comes from the
// request path, which only the server that routed it has read.
//
// The query below carries a parameter the parser knows nothing about, because that is the shape of a real
// request from a client still sending the removed name: the parser must ignore it and leave the refusal
// to RejectForeignTargetNamespace, rather than fail on it or quietly turn it into something.
func TestParseQueryOptions_LeavesSubjectToCaller(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://x/y?scope=node&targetNamespace=restore-ns", nil)

	opts, err := ParseQueryOptions(r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.SnapshotName != "" || opts.SnapshotNamespace != "" {
		t.Fatalf("parser must not fill the subject, got %#v", opts)
	}
	if opts.Scope != transform.ScopeNode || opts.FilterKind != "" || opts.FilterName != "" || opts.FilterAPIVersion != "" {
		t.Fatalf("parser must read the query it owns and nothing else, got %#v", opts)
	}
}

// The parameter is gone from the contract, but a caller can still put it on the wire, and what happens
// then is itself a contract: a foreign value is refused, the addressed namespace is tolerated. Both
// halves matter and fail in opposite directions — refusing the equal value breaks working clients,
// accepting a foreign one hands back objects addressed somewhere the caller did not ask for, with a 200.
//
// The repeated-key cases are the ones that make this a gate rather than a spot check: a query may carry
// the same key more than once, and reading only one of its values lets a foreign namespace ride along
// behind an acceptable one.
//
// LIMIT: values the URL parser accepts. A key=value pair it cannot decode at all — a broken
// percent-escape, say — is dropped out of the query before any reader sees it, so it is neither refused
// here nor acted on anywhere else; no case below can cover it.
func TestRejectForeignTargetNamespace(t *testing.T) {
	cases := []struct {
		name    string
		query   string
		wantBad bool
	}{
		{name: "absent is fine", query: "", wantBad: false},
		{name: "empty value is fine", query: "?targetNamespace=", wantBad: false},
		{name: "the addressed namespace is tolerated", query: "?targetNamespace=ns1", wantBad: false},
		{name: "the addressed namespace repeated is tolerated", query: "?targetNamespace=ns1&targetNamespace=ns1", wantBad: false},
		{name: "a foreign namespace is refused", query: "?targetNamespace=other-ns", wantBad: true},
		{name: "a foreign namespace hidden behind the addressed one is refused", query: "?targetNamespace=ns1&targetNamespace=other-ns", wantBad: true},
		{name: "a foreign namespace ahead of the addressed one is refused", query: "?targetNamespace=other-ns&targetNamespace=ns1", wantBad: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/apis/x/v1/namespaces/ns1/snapshots/s/manifests-with-data-restoration"+tc.query, nil)
			err := RejectForeignTargetNamespace(r, "ns1")
			if tc.wantBad {
				if !errors.Is(err, transform.ErrBadRequest) {
					t.Fatalf("error = %v, want one wrapping ErrBadRequest (400 is what a client acts on)", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}

	t.Logf("checked %d targetNamespace query shape(s) against an addressed namespace of ns1", len(cases))
}
