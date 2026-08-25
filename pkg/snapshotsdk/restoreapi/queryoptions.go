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
// kind, name, apiVersion) into the transform.Options subset they control. The snapshot identity is
// orthogonal and set by the caller from the request path. There is deliberately NO namespace parameter:
// a subtree is compiled into the namespace of the node addressed, and a server that adds one of its own
// makes the same query mean different things on different groups — the one thing this package exists to
// prevent.
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

// RejectForeignTargetNamespace refuses a request that names a target namespace other than the one the
// addressed snapshot lives in. It enforces the one thing a caller may still say about namespaces, and the
// answer is always "no": a subtree is compiled into the namespace of the node addressed, so a
// targetNamespace naming any other namespace is a request no server can satisfy. The failure is an
// ErrBadRequest, which the status mapping turns into 400.
//
// Why 400 rather than silently ignoring the parameter, which is what an aggregated apiserver does with
// any unknown query key. Ignoring answers a different question than the one asked and says nothing about
// having done so: a client that asked for namespace B receives objects addressed into A together with a
// 200, applies them into A, and overwrites what is there. The failure lands after the answer looked fine.
// That is not hypothetical; it is what three of this project's own end-to-end specs did.
//
// Honouring it is not the alternative, and that is why there is no parameter left to honour: re-addressing
// alone appears to work for a namespace holding only manifests and produces never-binding claims for one
// holding volume data, because a restored claim reaches its data through a dataSourceRef that carries no
// namespace while the VolumeSnapshot it names stays with the snapshot — and nothing in the request tells
// the two cases apart.
//
// The equal value is still accepted, and deliberately: existing clients pass the addressed namespace and
// mean nothing by it, so refusing them would break working callers to make a point. Refusing ANY
// presence is the end state, once no client sends it.
//
// Every server of the subresource MUST call this, for the same reason they all share ParseQueryOptions:
// one query must mean one thing regardless of which group the client addressed. It is stated as a
// requirement rather than as a fact because it is not yet one everywhere — a server that depends on a
// version older than this function still answers such a request 200. Depending on a newer one does not
// change that by itself: this is a function, not a hook, and it acts only where a server calls it. The
// dependency is the precondition; the call, right after ParseQueryOptions, is the fix.
func RejectForeignTargetNamespace(r *http.Request, snapshotNamespace string) error {
	// EVERY value of the key is checked, not just the first one a lookup would hand back: a query may
	// repeat a key, and reading one value lets a foreign namespace ride along behind an acceptable one.
	//
	// LIMIT: a key=value pair the URL parser cannot decode — a broken percent-escape, say — is dropped
	// out of the query before this sees it, so such a value is neither refused here nor read anywhere
	// else: the same parse feeds the rest of the query contract.
	for _, requested := range r.URL.Query()["targetNamespace"] {
		if requested == "" || requested == snapshotNamespace {
			continue
		}
		return fmt.Errorf(
			"%w: targetNamespace %q is not the namespace of the snapshot addressed (%q); a subtree is compiled into its own namespace, and moving one elsewhere is a download-and-load, not a read",
			transform.ErrBadRequest, requested, snapshotNamespace,
		)
	}
	return nil
}
