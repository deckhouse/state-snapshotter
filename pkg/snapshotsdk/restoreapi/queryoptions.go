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

// Package restoreapi is the wire contract of the manifests-with-data-restoration subresource: how its
// query is parsed and validated, and how a compilation failure becomes an HTTP status. Every aggregated
// apiserver that serves the subresource — the core one and every out-of-process domain facade — uses this
// package, so one query means one thing and one failure answers one way regardless of which group the
// client addressed.
//
// It is the transport half of the shared restore contract; the compilation half (what the parsed options
// then select and how a node's manifests become apply-ready) lives in package
// snapshotsdk/transform, which this package deliberately does not duplicate.
package restoreapi

import (
	"fmt"
	"net/http"

	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk/transform"
)

// ParseQueryOptions reads and validates the manifests-with-data-restoration query parameters (scope,
// kind, name, apiVersion) into the transform.Options subset they control. The snapshot identity and
// targetNamespace are orthogonal and set by the caller from the request path and its own query.
//
// Every validation failure is an ErrBadRequest (the status mapping turns it into 400), and the four rules
// are checked separately so a client learns which one it broke:
//   - unknown scope value;
//   - kind without name, or name without kind;
//   - apiVersion without kind;
//   - any object filter (kind/name/apiVersion) together with scope != node (the object filter is valid
//     only with scope=node, where the output is a single node's manifests).
func ParseQueryOptions(r *http.Request) (transform.Options, error) {
	q := r.URL.Query()
	scope := q.Get("scope")
	kind := q.Get("kind")
	name := q.Get("name")
	apiVersion := q.Get("apiVersion")

	opts := transform.Options{}
	switch scope {
	case "", string(transform.ScopeSubtree):
		opts.Scope = transform.ScopeSubtree
	case string(transform.ScopeNode):
		opts.Scope = transform.ScopeNode
	default:
		return transform.Options{}, fmt.Errorf("%w: unknown scope %q (want %q or %q)", transform.ErrBadRequest, scope, transform.ScopeSubtree, transform.ScopeNode)
	}

	if (kind == "") != (name == "") {
		return transform.Options{}, fmt.Errorf("%w: object filter requires both kind and name", transform.ErrBadRequest)
	}
	if apiVersion != "" && kind == "" {
		return transform.Options{}, fmt.Errorf("%w: apiVersion filter requires kind and name", transform.ErrBadRequest)
	}
	if (kind != "" || apiVersion != "") && opts.Scope != transform.ScopeNode {
		return transform.Options{}, fmt.Errorf("%w: object filter (kind/name/apiVersion) is only allowed with scope=node", transform.ErrBadRequest)
	}

	opts.FilterKind = kind
	opts.FilterName = name
	opts.FilterAPIVersion = apiVersion
	return opts, nil
}
