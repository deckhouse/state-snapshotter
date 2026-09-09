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
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/deckhouse/sds-common-lib/logger"
)

// TestRoutedSubresourcesMatchDiscovery holds the namespaced subresource routers and the discovery
// documents to the SAME set, in both directions.
//
// Why both directions, and why this test exists at all: everything downstream of discovery — clients
// building their calls from it, and the RBAC guard that requires every advertised endpoint to be
// grantable — is only as good as the claim that discovery describes what the server serves. Membership
// tests of individual names cannot make that claim: a new case in a router that nobody added to
// discovery leaves an endpoint that is served, invisible, and reachable by nobody, with every other
// guard still green. The reverse gap is just as real: an advertised endpoint that no router serves is
// a 404 the client was told to expect to work.
//
// The served side is read from the routers' source with go/ast rather than from a list kept here: a
// list would be a copy, and a copy is what drifts. Three shapes are refused rather than skipped,
// because each would leave a real route uncompared while this test stayed green:
//
//   - a case value that is not a plain string literal, which is a route this parser cannot read;
//   - any reading of the subresource segment outside the dispatching switch — an
//     "if subresource == ... { return }" before it serves a name that appears in no case value;
//   - a default clause that does anything but refuse. Falling through is where an unenumerated name
//     arrives, so a default that routes (inline, or by handing the request to another function that
//     switches on its own) serves exactly the names this comparison cannot see.
//
// LIMITS:
//   - only the namespaced subresource router of this module's aggregated group is compared. The
//     cluster-scoped snapshotcontents routes and the manifestcheckpoints route are served by other
//     functions, are addressed without a namespace, and are granted by no user-facing role. The volume
//     node's subresources are served by the module that owns the CSI VolumeSnapshot, in its own
//     repository, so nothing here can read either side of that pair.
//   - routeGenericSnapshotSubresource is deliberately not among them, and this is not an oversight: it
//     dispatches no handler at all. Every branch of it answers 404 — the three names it enumerates with a
//     message stating that the kind is addressed through the group of the module that owns it, anything
//     else with a plain unknown-subresource error — so its case values are refusals, and discovery must
//     NOT advertise them, which is why comparing them here would be exactly wrong. Its guards are
//     TestRestoreHandler_GenericDomainSubresourcesRemoved for the statuses and
//     TestRestoreHandler_ForeignGVRRefusalNamesNoGroup for the message, which names no group: this module
//     knows no other module's group name. Should it ever grow a branch that serves something, that branch
//     belongs in the table below.
//   - names only. Which HTTP method a subresource takes, and therefore which verb it is authorized
//     under, is asserted next to each route by the routing tests in this package.
func TestRoutedSubresourcesMatchDiscovery(t *testing.T) {
	cases := []struct {
		name         string
		file         string
		routerFunc   string
		discoveryURL string
		resource     string
	}{
		{
			name:         "core Snapshot subresources",
			file:         "restore_handler.go",
			routerFunc:   "routeCoreSnapshotSubresource",
			discoveryURL: "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1",
			resource:     "snapshots",
		},
	}

	mux := servedDiscoveryMux(t)
	comparisons := 0
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			routed := routedSubresourceCases(t, tc.file, tc.routerFunc)
			advertised := advertisedSubresources(t, mux, tc.discoveryURL, tc.resource)

			for _, subresource := range routed {
				if !contains(advertised, subresource) {
					t.Errorf("%s routes %q, which %s does not advertise (advertised: %s) — a served endpoint missing from discovery is invisible to clients and to the RBAC coverage guard",
						tc.routerFunc, subresource, tc.discoveryURL, strings.Join(advertised, ", "))
				}
			}
			for _, subresource := range advertised {
				if !contains(routed, subresource) {
					t.Errorf("%s advertises %q, which %s does not route (routed: %s) — an advertised endpoint nobody serves answers 404",
						tc.discoveryURL, subresource, tc.routerFunc, strings.Join(routed, ", "))
				}
			}

			t.Logf("%s: compared %d routed against %d advertised subresource(s) of %q", tc.routerFunc, len(routed), len(advertised), tc.resource)
			comparisons += len(routed)
		})
	}

	if comparisons == 0 {
		t.Fatal("no subresource compared — zero findings means the routers stopped being read, not that they agree with discovery")
	}
}

// subresourceSegment is the name the routers bind the routed path segment to. Reading it anywhere but
// the dispatching switch is a route this guard cannot see, so the name is pinned here.
const subresourceSegment = "subresource"

// routedSubresourceCases returns the subresource names a router function dispatches, read from the
// case values of its switch over the subresource segment.
func routedSubresourceCases(t *testing.T, file, funcName string) []string {
	t.Helper()

	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	var decl *ast.FuncDecl
	for _, node := range parsed.Decls {
		fn, ok := node.(*ast.FuncDecl)
		if ok && fn.Name.Name == funcName {
			decl = fn
			break
		}
	}
	if decl == nil {
		t.Fatalf("%s: function %s is gone — the routing it did moved somewhere this guard no longer reads", file, funcName)
	}

	var switches []*ast.SwitchStmt
	ast.Inspect(decl, func(node ast.Node) bool {
		stmt, ok := node.(*ast.SwitchStmt)
		if !ok {
			return true
		}
		if tag, isIdent := stmt.Tag.(*ast.Ident); isIdent && tag.Name == subresourceSegment {
			switches = append(switches, stmt)
		}
		return true
	})
	if len(switches) != 1 {
		t.Fatalf("%s: %s holds %d switch(es) over %s, want exactly one — the shape this guard reads has changed", file, funcName, len(switches), subresourceSegment)
	}
	assertSubresourceReadOnlyByDispatch(t, fset, file, funcName, decl, switches[0])
	assertUnenumeratedSubresourceIsRefused(t, fset, file, funcName, switches[0])

	var subresources []string
	for _, stmt := range switches[0].Body.List {
		clause, ok := stmt.(*ast.CaseClause)
		if !ok || clause.List == nil { // default:
			continue
		}
		for _, expr := range clause.List {
			lit, isLit := expr.(*ast.BasicLit)
			if !isLit || lit.Kind != token.STRING {
				t.Fatalf("%s: %s has a non-literal case value %T — this guard can only compare literal subresource names, so teach it the new shape rather than leaving the routes uncompared", file, funcName, expr)
			}
			value, uerr := strconv.Unquote(lit.Value)
			if uerr != nil {
				t.Fatalf("%s: unquote case value %s: %v", file, lit.Value, uerr)
			}
			subresources = append(subresources, value)
		}
	}
	if len(subresources) == 0 {
		t.Fatalf("%s: %s dispatches no subresource case at all", file, funcName)
	}
	sort.Strings(subresources)
	return subresources
}

// assertSubresourceReadOnlyByDispatch fails when the router looks at the routed segment anywhere but
// the switch whose case values this guard compares. Without it the comparison covers only the routes
// that happen to be written as cases: an equality test before the switch, a second switch, or a lookup
// in a map serves an endpoint that no case value mentions, so it never reaches discovery and no role
// can grant it — the exact drift this test exists to catch, walking straight past it.
//
// Binding the segment is not reading it, so the parameter and the := that unpacks it from the path are
// allowed, as is everything inside the dispatch: within a case clause the name is already one of the
// compared values, so a handler taking it or an error message quoting it decides nothing. The default
// clause is the exception to that argument — it is reached by exactly the names no case enumerates —
// and it is not covered here but by assertUnenumeratedSubresourceIsRefused, which constrains its body
// whether or not the segment is mentioned in it. A plain reassignment is not allowed anywhere: it
// would route one name under another.
func assertSubresourceReadOnlyByDispatch(t *testing.T, fset *token.FileSet, file, funcName string, decl *ast.FuncDecl, dispatch *ast.SwitchStmt) {
	t.Helper()

	binding := map[*ast.Ident]bool{}
	if decl.Type.Params != nil {
		for _, field := range decl.Type.Params.List {
			for _, name := range field.Names {
				binding[name] = true
			}
		}
	}
	ast.Inspect(decl, func(node ast.Node) bool {
		if assign, ok := node.(*ast.AssignStmt); ok && assign.Tok == token.DEFINE {
			for _, lhs := range assign.Lhs {
				if ident, isIdent := lhs.(*ast.Ident); isIdent {
					binding[ident] = true
				}
			}
		}
		return true
	})
	ast.Inspect(dispatch, func(node ast.Node) bool {
		if ident, ok := node.(*ast.Ident); ok {
			binding[ident] = true
		}
		return true
	})

	ast.Inspect(decl, func(node ast.Node) bool {
		ident, ok := node.(*ast.Ident)
		if !ok || ident.Name != subresourceSegment || binding[ident] {
			return true
		}
		t.Errorf("%s: %s reads %q at %s, outside the switch this guard compares — a route decided there is served without appearing in any case value, so it reaches neither discovery nor any role. Route it through the switch, or teach this guard the new shape",
			file, funcName, subresourceSegment, fset.Position(ident.Pos()))
		return true
	})
}

// assertUnenumeratedSubresourceIsRefused requires the dispatch to end in a default clause whose whole
// body is one writeKubernetesErrorResponse with http.StatusNotFound. That single statement is the
// invariant the comparison rests on: a name the switch does not enumerate is refused, never served.
//
// Constraining the body rather than merely forbidding the segment to be read there is deliberate, and
// the weaker rule was tried first and does not hold. A default clause may route without ever naming
// the segment — "h.routeSomethingElse(w, r, namespace, name)" whose callee re-derives the path from
// the request — and delegating out of default is the most natural way for this code to grow, not an
// exotic shape. Pinning the body closes both forms at once, and it costs nothing legitimate: a router
// that wants to serve a new subresource writes a case for it, which is precisely what makes the name
// visible to discovery, to this comparison, and to the roles.
func assertUnenumeratedSubresourceIsRefused(t *testing.T, fset *token.FileSet, file, funcName string, dispatch *ast.SwitchStmt) {
	t.Helper()

	var defaults []*ast.CaseClause
	for _, stmt := range dispatch.Body.List {
		if clause, ok := stmt.(*ast.CaseClause); ok && clause.List == nil {
			defaults = append(defaults, clause)
		}
	}
	if len(defaults) != 1 {
		t.Errorf("%s: %s has %d default clause(s), want exactly one — without it an unenumerated subresource falls out of the switch unanswered instead of being refused",
			file, funcName, len(defaults))
		return
	}

	clause := defaults[0]
	position := fset.Position(clause.Pos())
	if len(clause.Body) != 1 {
		t.Errorf("%s: %s has a default clause of %d statements at %s, want the single refusal — anything else there serves the names this guard cannot see, since the default is reached by exactly the names no case enumerates",
			file, funcName, len(clause.Body), position)
		return
	}
	expr, ok := clause.Body[0].(*ast.ExprStmt)
	if !ok {
		t.Errorf("%s: %s: the default clause at %s is a %T, want a single call refusing the request", file, funcName, position, clause.Body[0])
		return
	}
	call, ok := expr.X.(*ast.CallExpr)
	if !ok {
		t.Errorf("%s: %s: the default clause at %s is not a call, want the single refusal", file, funcName, position)
		return
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "writeKubernetesErrorResponse" {
		t.Errorf("%s: %s: the default clause at %s does not call writeKubernetesErrorResponse — an unenumerated subresource must be refused there, not handled", file, funcName, position)
		return
	}
	for _, arg := range call.Args {
		status, isSelector := arg.(*ast.SelectorExpr)
		if !isSelector || status.Sel.Name != "StatusNotFound" {
			continue
		}
		if pkg, isIdent := status.X.(*ast.Ident); isIdent && pkg.Name == "http" {
			return
		}
	}
	t.Errorf("%s: %s: the default clause at %s answers with something other than http.StatusNotFound — a name the switch does not enumerate is not served, so it is a 404",
		file, funcName, position)
}

// advertisedSubresources returns the subresources of one resource named by a discovery document.
func advertisedSubresources(t *testing.T, mux *http.ServeMux, discoveryURL, resource string) []string {
	t.Helper()

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, discoveryURL, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("discovery %s answered %d, want 200", discoveryURL, rec.Code)
	}
	var list metav1.APIResourceList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode discovery %s: %v", discoveryURL, err)
	}

	var subresources []string
	for _, entry := range list.APIResources {
		name, subresource, found := strings.Cut(entry.Name, "/")
		if !found || name != resource {
			continue
		}
		subresources = append(subresources, subresource)
	}
	if len(subresources) == 0 {
		t.Fatalf("discovery %s advertises no subresource of %q", discoveryURL, resource)
	}
	sort.Strings(subresources)
	return subresources
}

// servedDiscoveryMux registers the real routes of this module's aggregated group. Discovery touches
// neither the client nor the usecases, so nil dependencies are enough.
func servedDiscoveryMux(t *testing.T) *http.ServeMux {
	t.Helper()

	return newServedDiscoveryMux()
}

// newServedDiscoveryMux mounts the production routes on a fresh mux. It is framework-agnostic so that the
// specs which need the SAME delegate — the OpenAPI ones, which run under Ginkgo and have no *testing.T —
// mount it through this function instead of keeping a second copy of the wiring.
func newServedDiscoveryMux() *http.ServeMux {
	mux := http.NewServeMux()
	NewArchiveHandler(nil, nil, logger.NewNop()).SetupRoutes(mux)
	return mux
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
