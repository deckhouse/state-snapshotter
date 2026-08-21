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

package transform

// Scope selects the compilation depth of manifests-with-data-restoration.
type Scope string

const (
	// ScopeSubtree compiles the addressed node and its whole run-tree recursively (post-order). It is
	// the default when no scope is requested — the historical behavior, fully backward compatible — and
	// fails closed: any not-ready descendant aborts the whole request (409).
	ScopeSubtree Scope = "subtree"
	// ScopeNode compiles ONLY the addressed node. The node's own validations still run (Ready gate,
	// bound content Ready, anti-spoofing back-ref 403 for the root, non-empty MCP), but the run-tree
	// children are not read at all — a not-ready child does NOT fail the request.
	ScopeNode Scope = "node"
)

// Options is the already-validated form of one manifests-with-data-restoration request: which node is
// addressed, how deep to compile it, where to restore it, and optionally which single object to return.
// It is shared by every server of the subresource so the same query means the same thing on every group;
// the validation that produces it is restoreapi.ParseQueryOptions.
type Options struct {
	SnapshotName      string
	SnapshotNamespace string
	TargetNamespace   string

	// Scope selects the compilation depth (see Scope). An empty value is treated as ScopeSubtree, so a
	// zero Options keeps the historical recursive behavior. Set by the handler from the ?scope= query.
	Scope Scope

	// FilterKind, FilterName and FilterAPIVersion select a single object from the compiled node output
	// (?kind=&name=&apiVersion=). The object filter is valid ONLY together with ScopeNode; the parser
	// enforces that contract (an object filter with scope != node is rejected with ErrBadRequest) and
	// this struct only carries the already-validated values. FilterAPIVersion is optional and may be a
	// full "group/version" or a bare "group" (used to disambiguate a kind+name present in two groups).
	FilterKind       string
	FilterName       string
	FilterAPIVersion string
}
