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

// Guards that keep the user-facing documents under docs/ from naming an aggregated endpoint, a Snapshot
// spec field, or a Ready reason the API does not have. All three defects were live in those documents:
// they advertised a snapshots/manifests subresource that answers 404 "unknown subresource", a
// spec.source selector that does not exist — and since the API server prunes unknown fields BEFORE
// validation, a client following that text was answered "created" and got the opposite of the behaviour
// it asked for — and two reasons that had been removed from the condition catalogs.
//
// LIMITS — read these before trusting the guards:
//   - they compare NAMES only. HTTP method, status codes, response shape and field types are out of scope.
//   - the reason guard reads two positions only; its scope and cost are described on the test itself.
//   - this module's half of the served set is read from its aggregated DISCOVERY document, i.e. what it
//     advertises. That the namespaced routers and discovery name the same subresources, in both
//     directions, is pinned separately by TestRoutedSubresourcesMatchDiscovery; the cluster-scoped routes
//     are covered by neither and are advertised, not compared.
//   - the volume node's endpoints are served by another module and are DECLARED here
//     (docsVolumeNodeSubresources), with the limit that entails written on that function.
//   - a segment written as a {placeholder} is opaque, so whatever it stands for is not checked.
//   - only the core Snapshot kind's spec is pinned. A field of another kind must be written qualified
//     (DataImport.spec.snapshotRef); qualified references are counted and skipped.
//
// They are plain testing guards rather than Ginkgo specs, and they read the documents from disk on
// purpose: the subject of the check IS the repository file, and //go:embed cannot reach outside the
// package directory, so an embedded fixture would be a copy of the file checking itself. Same shape as
// the RBAC template guards under internal/controllers/manifestcapture.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/deckhouse/sds-common-lib/logger"
	"github.com/deckhouse/state-snapshotter/api/names"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// docsRepoRoot is the repository root relative to this package (internal/api -> internal -> controller
// image -> images -> root).
const docsRepoRoot = "../../../.."

// docsCoreDiscoveryPath addresses the core aggregated group's version discovery. It is spelled out
// because the router registers that group/version as a literal mux prefix and exports no constant for
// it; if the group or version is ever renamed, this request stops returning 200 and the guard fails
// loudly instead of validating docs/ against a stale table.
const docsCoreDiscoveryPath = "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1"

// docsAggregatedPathRE matches a reference to one of OUR aggregated groups in a document. Only groups
// named subresources.* are matched: a path into the CRD group (/apis/state-snapshotter.deckhouse.io/...)
// addresses stored objects, is served by the main API server, and is none of this guard's business.
// A query string cannot be part of a match (? is outside the class), and docsParseAggregatedPath cuts
// one off defensively anyway.
var docsAggregatedPathRE = regexp.MustCompile(`/apis/subresources\.[A-Za-z0-9.-]+(?:/[A-Za-z0-9.{}-]+)*`)

// docsSpecFieldRE matches spec.<field> with an optional Kind. qualifier, e.g. "spec.mode",
// "Snapshot.spec.mode", "DataImport.spec.snapshotRef".
var docsSpecFieldRE = regexp.MustCompile(`(?:\b([A-Z][A-Za-z0-9]*)\.)?\bspec\.([A-Za-z][A-Za-z0-9]*)`)

// docsReadyReasonRE matches a reason written next to the Ready state it belongs to — "`Ready=False` /
// `SomeReason`" — which is the shape a removed reason shipped in. Optional backticks let the two halves
// sit in separate inline-code spans.
var docsReadyReasonRE = regexp.MustCompile("Ready=(?:True|False)`?\\s*/\\s*`?([A-Z][A-Za-z0-9]*)")

// docsServedSet maps "<group>/<version>" to "<resource>/<subresource>" to whether it is namespaced.
type docsServedSet map[string]map[string]bool

// docsAPIPath is one aggregated path reference found in a document.
type docsAPIPath struct {
	raw            string
	group          string
	version        string
	namespaced     bool
	resource       string // "" when the reference stops at the group or version
	subresource    string // "" when the reference names no subresource
	hasSubresource bool
}

func (p docsAPIPath) groupVersion() string { return p.group + "/" + p.version }

// TestDocsNameOnlyServedSubresources fails when a document names an aggregated group/version,
// resource, subresource or scope that the module does not serve.
func TestDocsNameOnlyServedSubresources(t *testing.T) {
	served := docsServedSubresources(t)
	files := docsMarkdownFiles(t)

	var pathsFound, tuplesChecked, placeholdersSkipped int
	for _, file := range files {
		name := filepath.Base(file)
		for _, raw := range docsAggregatedPathRE.FindAllString(docsReadFile(t, file), -1) {
			pathsFound++
			parsed, err := docsParseAggregatedPath(raw)
			if err != nil {
				t.Errorf("%s: %v", name, err)
				continue
			}

			if parsed.version == "" {
				// Group-only reference: the version is not stated, so only the group can be checked.
				if !docsServesGroup(served, parsed.group) {
					t.Errorf("%s: %q names aggregated group %q, which nothing serving this contract serves (covered: %s)",
						name, parsed.raw, parsed.group, docsSortedGroupVersions(served))
				}
				continue
			}

			gv, ok := served[parsed.groupVersion()]
			if !ok {
				t.Errorf("%s: %q names aggregated group/version %q, which nothing serving this contract serves (covered: %s) — "+
					"note the two aggregated groups are on DIFFERENT versions",
					name, parsed.raw, parsed.groupVersion(), docsSortedGroupVersions(served))
				continue
			}
			if parsed.resource == "" || docsIsPlaceholder(parsed.resource) {
				placeholdersSkipped++
				continue
			}
			if !docsServesResource(gv, parsed.resource) {
				t.Errorf("%s: %q names resource %q, which %s does not serve (served: %s)",
					name, parsed.raw, parsed.resource, parsed.groupVersion(), docsSortedKeys(gv))
				continue
			}
			if !parsed.hasSubresource || docsIsPlaceholder(parsed.subresource) {
				placeholdersSkipped++
				continue
			}

			key := parsed.resource + "/" + parsed.subresource
			namespaced, ok := gv[key]
			if !ok {
				t.Errorf("%s: %q names subresource %q, which %s does not serve (served: %s)",
					name, parsed.raw, key, parsed.groupVersion(), docsSortedKeys(gv))
				continue
			}
			if namespaced != parsed.namespaced {
				t.Errorf("%s: %q addresses %q as namespaced=%v, but it is served as namespaced=%v",
					name, parsed.raw, key, parsed.namespaced, namespaced)
			}
			tuplesChecked++
		}
	}

	t.Logf("scanned %d document(s) under docs/, found %d aggregated path(s), fully checked %d resource/subresource pair(s), skipped %d placeholder/partial reference(s)",
		len(files), pathsFound, tuplesChecked, placeholdersSkipped)
	if pathsFound == 0 {
		t.Fatal("no aggregated API path found in docs/ — zero findings means the extractor stopped matching, not that the documents are clean")
	}
	if tuplesChecked == 0 {
		t.Fatal("no complete resource/subresource pair checked in docs/ — the guard would stay green on any endpoint name")
	}
}

// TestDocsNameOnlyExistingSpecFields fails when a document names a Snapshot spec field the API does not
// have. This is the guard that would have caught the documented-but-nonexistent source selector.
func TestDocsNameOnlyExistingSpecFields(t *testing.T) {
	valid := docsSnapshotSpecFields(t)
	files := docsMarkdownFiles(t)

	var checked, foreignSkipped int
	for _, file := range files {
		name := filepath.Base(file)
		for _, match := range docsSpecFieldRE.FindAllStringSubmatch(docsReadFile(t, file), -1) {
			qualifier, field := match[1], match[2]
			if qualifier != "" && qualifier != "Snapshot" {
				// A field of another kind. Its owner's schema is not this guard's subject, so it is
				// skipped — which is exactly why the qualifier is required: an unqualified spec.<field>
				// reads as a Snapshot field and is checked as one.
				foreignSkipped++
				continue
			}
			checked++
			if !valid[field] {
				t.Errorf("%s: names Snapshot field %q, which SnapshotSpec does not have (fields: %s). "+
					"The API server prunes unknown fields before validation, so a client following this text is "+
					"answered \"created\" and silently gets the default behaviour. Write a field of another kind "+
					"qualified, e.g. DataImport.spec.snapshotRef",
					name, "spec."+field, docsSortedKeys(valid))
			}
		}
	}

	t.Logf("scanned %d document(s) under docs/, checked %d Snapshot spec field mention(s), skipped %d qualified foreign-kind mention(s)",
		len(files), checked, foreignSkipped)
	if checked == 0 {
		t.Fatal("no Snapshot spec field mentioned in docs/ — zero findings means the extractor stopped matching, not that the documents are clean")
	}
}

// TestDocsNameOnlyExistingReadyReasons fails when a document shows a Ready reason that is not in the
// condition catalogs. Two reasons that no longer exist shipped in these documents at once, so the names
// are pinned rather than trusted.
//
// SCOPE — two positions only, and this is the whole of it:
//   - the REASON column of a kubectl-style table in an output example;
//   - a reason written next to its Ready state, "`Ready=False` / `SomeReason`".
//
// Prose is NOT parsed, deliberately: "a Warning event with reason NamespacePlanUnreadable" is a correct
// sentence about an EVENT reason, which is a different namespace from the Ready condition reasons this
// guard knows. Widening the scan to every capitalized word after the word "reason" would fail on that
// truth. The cost of the narrow scope is real and named here: a wrong reason mentioned only in prose
// stays green.
//
// The accepted set is the contract catalog — terminal, recoverable-degraded, and the success reason.
// In-progress reasons (the core reason taxonomy) are NOT accepted: they are not part of the catalogs
// UI and clients branch on, so an output example pinning one would be pinning a non-contract value.
// Documenting one on purpose means extending the set here on purpose.
func TestDocsNameOnlyExistingReadyReasons(t *testing.T) {
	valid := docsReadyReasons(t)
	files := docsMarkdownFiles(t)

	var fromTables, fromConditions int
	check := func(file, position, reason string) {
		if !valid[reason] {
			t.Errorf("%s: %s shows Ready reason %q, which is not in the condition catalogs (accepted: %s)",
				filepath.Base(file), position, reason, docsSortedKeys(valid))
		}
	}

	for _, file := range files {
		body := docsReadFile(t, file)
		for _, reason := range docsColumnTokens(body, "REASON") {
			fromTables++
			check(file, "the REASON column of an output example", reason)
		}
		for _, match := range docsReadyReasonRE.FindAllStringSubmatch(body, -1) {
			fromConditions++
			check(file, "the text next to a Ready state", match[1])
		}
	}

	t.Logf("scanned %d document(s) under docs/, checked %d reason(s) in output examples and %d written next to a Ready state, against %d catalog reason(s)",
		len(files), fromTables, fromConditions, len(valid))
	if fromTables+fromConditions == 0 {
		t.Fatal("no Ready reason found in docs/ — zero findings means the extractor stopped matching, not that the documents are clean")
	}
}

// docsReadyReasons returns the Ready reasons a document may show: the terminal and the
// recoverable-degraded catalogs plus the success reason, read from the catalogs themselves.
func docsReadyReasons(t *testing.T) map[string]bool {
	t.Helper()

	valid := map[string]bool{storagev1alpha1.ReasonCompleted: true}
	for reason := range storagev1alpha1.TerminalReadyReasons {
		valid[reason] = true
	}
	for reason := range storagev1alpha1.DegradedReadyReasons {
		valid[reason] = true
	}
	if len(valid) < 3 {
		t.Fatalf("the reason catalogs yielded only %d name(s) — they are no longer readable from here", len(valid))
	}
	return valid
}

// docsColumnTokens returns the values under the named column of every kubectl-style table in the
// document. A table starts at a header naming both READY and that column, and ends at the first line
// that does not split into the same number of fields (a blank line, a fence marker, or prose) — so a row
// whose columns are ragged is dropped rather than read at the wrong offset. An empty value ("<none>",
// "-") is not a value and is skipped.
func docsColumnTokens(body, column string) []string {
	var tokens []string
	columnAt, headerWidth := -1, 0

	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(line)
		if at := docsFieldIndex(fields, column); at >= 0 && docsFieldIndex(fields, "READY") >= 0 {
			columnAt, headerWidth = at, len(fields)
			continue
		}
		if columnAt < 0 {
			continue
		}
		if len(fields) != headerWidth {
			columnAt, headerWidth = -1, 0
			continue
		}
		if value := fields[columnAt]; value != "<none>" && value != "-" {
			tokens = append(tokens, value)
		}
	}
	return tokens
}

func docsFieldIndex(fields []string, want string) int {
	for i, field := range fields {
		if field == want {
			return i
		}
	}
	return -1
}

// docsSnapshotSpecFields returns the json field names a client may put under a Snapshot's spec, read
// from the Go type so a rename cannot leave this guard validating docs against a stale list.
func docsSnapshotSpecFields(t *testing.T) map[string]bool {
	t.Helper()

	fields := map[string]bool{}
	specType := reflect.TypeOf(storagev1alpha1.SnapshotSpec{})
	for i := 0; i < specType.NumField(); i++ {
		name, _, _ := strings.Cut(specType.Field(i).Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		fields[name] = true
	}
	if len(fields) == 0 {
		t.Fatal("SnapshotSpec exposes no json fields — the reflection above stopped working")
	}

	// The rejection stub is declared in the schema only so the spec-level CEL rule can REFUSE a request
	// carrying it; documenting it as usable would send clients to a rejected object. Dropping it from the
	// valid set is a narrowing of the guard, so it is asserted rather than assumed: once the deprecation
	// cycle removes the field, this fails and says so instead of quietly becoming a no-op.
	const rejectionStub = "resourceSelector"
	if !fields[rejectionStub] {
		t.Fatalf("SnapshotSpec no longer declares the %q rejection stub — drop this exclusion", rejectionStub)
	}
	delete(fields, rejectionStub)

	return fields
}

// docsServedSubresources builds the set of aggregated endpoints the documents may name: the ones this
// module serves, read from its own discovery document, plus the ones another module serves for a node
// kind these documents are also about (docsVolumeNodeSubresources).
func docsServedSubresources(t *testing.T) docsServedSet {
	t.Helper()

	log := logger.NewNop()

	served := docsServedSet{}
	add := func(list metav1.APIResourceList) {
		if served[list.GroupVersion] == nil {
			served[list.GroupVersion] = map[string]bool{}
		}
		for _, res := range list.APIResources {
			served[list.GroupVersion][res.Name] = res.Namespaced
		}
	}

	// The discovery handler touches neither the client nor the usecases, so nil dependencies are enough.
	add(docsDiscover(t, NewArchiveHandler(nil, nil, log).HandleAPIResourceListDiscovery, docsCoreDiscoveryPath))

	foreign := docsVolumeNodeSubresources()
	for groupVersion, resources := range foreign {
		if _, alsoOurs := served[groupVersion]; alsoOurs {
			t.Fatalf("%q is declared as another module's group but this module serves it too — read it from discovery instead of declaring it", groupVersion)
		}
		served[groupVersion] = resources
	}
	return served
}

// docsVolumeNodeSubresources declares the aggregated endpoints of a VOLUME node — the endpoints these
// documents describe alongside this module's own, because a snapshot tree has nodes of both kinds and a
// client walks them in one pass.
//
// They are declared rather than discovered, and that is forced: they are served by the module that owns
// the CSI VolumeSnapshot, in a different repository, whose handlers this test cannot import. So this
// table is a copy, with everything that follows from it — LIMIT: it proves the documents name a
// resource/subresource/scope from THIS list, not that the other module still serves exactly this list. A
// rename there is caught by the end-to-end suite that calls the endpoints, not here.
//
// What it does buy, and the reason for keeping it instead of skipping foreign groups altogether: a
// misspelled subresource, a wrong version, or a cluster-scoped address in the documents fails here, and
// those are the defects this file was written for — all three were live in these documents once.
func docsVolumeNodeSubresources() docsServedSet {
	return docsServedSet{
		"subresources.storage-foundation.deckhouse.io/v1": {
			"volumesnapshots/manifests-download":                 true,
			"volumesnapshots/manifests-with-data-restoration":    true,
			"volumesnapshots/manifests-and-children-refs-upload": true,
		},
	}
}

func docsDiscover(t *testing.T, handler http.HandlerFunc, path string) metav1.APIResourceList {
	t.Helper()

	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("discovery %s answered %d, want 200: the group/version this guard reads is no longer served, so update it here and in docs/", path, rec.Code)
	}
	var list metav1.APIResourceList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode discovery %s: %v", path, err)
	}
	if list.GroupVersion == "" || len(list.APIResources) == 0 {
		t.Fatalf("discovery %s advertises nothing (groupVersion=%q, %d resources)", path, list.GroupVersion, len(list.APIResources))
	}
	return list
}

// docsParseAggregatedPath splits an aggregated path reference into its addressing components:
//
//	/apis/<group>[/<version>[[/namespaces/<ns>]/<resource>[/<name>[/<subresource>]]]]
//
// Object names and namespaces are opaque, so a {placeholder} in those positions parses like any other
// name. A shape this function cannot place is an error rather than a silent skip: an unrecognized path
// in a user-facing document is exactly what the guard exists to surface.
func docsParseAggregatedPath(raw string) (docsAPIPath, error) {
	cleaned, _, _ := strings.Cut(raw, "?")
	cleaned = strings.TrimSuffix(cleaned, "/")
	segments := strings.Split(strings.TrimPrefix(cleaned, "/apis/"), "/")

	parsed := docsAPIPath{raw: raw, group: segments[0]}
	rest := segments[1:]
	if len(rest) == 0 {
		return parsed, nil
	}
	parsed.version, rest = rest[0], rest[1:]

	if len(rest) > 0 && rest[0] == "namespaces" {
		if len(rest) < 2 {
			return parsed, fmt.Errorf("%q ends at /namespaces without a namespace", raw)
		}
		parsed.namespaced = true
		rest = rest[2:]
	}

	switch len(rest) {
	case 0: // version (or namespace) discovery
	case 1, 2: // <resource> or <resource>/<name>
		parsed.resource = rest[0]
	case 3: // <resource>/<name>/<subresource>
		parsed.resource, parsed.subresource, parsed.hasSubresource = rest[0], rest[2], true
	default:
		return parsed, fmt.Errorf("%q has %d segments after the version and does not look like an API path", raw, len(rest))
	}
	return parsed, nil
}

func docsServesGroup(served docsServedSet, group string) bool {
	for groupVersion := range served {
		if strings.HasPrefix(groupVersion, group+"/") {
			return true
		}
	}
	return false
}

func docsServesResource(groupVersion map[string]bool, resource string) bool {
	for key := range groupVersion {
		if name, _, found := strings.Cut(key, "/"); found && name == resource {
			return true
		}
	}
	return false
}

func docsIsPlaceholder(segment string) bool {
	return strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}")
}

// docsMarkdownFiles returns the user-facing documents of this repository.
func docsMarkdownFiles(t *testing.T) []string {
	t.Helper()

	files, err := filepath.Glob(filepath.Join(filepath.Clean(docsRepoRoot), "docs", "*.md"))
	if err != nil {
		t.Fatalf("glob docs: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no document found under docs/ — the path to the repository root is wrong")
	}
	return files
}

func docsReadFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func docsSortedKeys(m map[string]bool) string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

func docsSortedGroupVersions(served docsServedSet) string {
	keys := make([]string, 0, len(served))
	for key := range served {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// ---------------------------------------------------------------------------
// The backup integration document (the client wire contract) and its mirror
// ---------------------------------------------------------------------------

// partnerDocEN and partnerDocRU are the two files of the backup integration document: the contract an
// external backup product implements against. The English file is canonical, the other mirrors it.
const (
	partnerDocEN = "BACKUP_INTEGRATION.md"
	partnerDocRU = "BACKUP_INTEGRATION_RU.md"
)

// partnerDocGroupVersions are the aggregated group/versions that document is about. They are spelled
// out rather than derived from the served set on purpose: the served set is what this file compares
// documents against, so deriving the expectation from it would make "the document names an endpoint of
// each group" true by construction. The two versions differ (v1alpha1 against v1), which is the single
// most common mistake a client of this API makes, so a document that stopped naming one of the groups
// would stop exercising exactly the check that catches it.
var partnerDocGroupVersions = []string{
	"subresources.state-snapshotter.deckhouse.io/v1alpha1",
	"subresources.storage-foundation.deckhouse.io/v1",
}

// docsRoleNameRE matches a capability role name of any Deckhouse module, not only this one, in both
// role models — the capability/scope names of DKP 1.78 and later and the legacy manage/use ones the
// documents give for older clusters: a document that starts naming a role of a third module must
// surface rather than pass unnoticed, and a mirror that kept only one family of names must fail the
// comparison. The collecting role of the new model sits outside the role model and has no capability
// prefix, so its name is spelled out.
var docsRoleNameRE = regexp.MustCompile(`d8:(?:use:capability:module|namespace-capability):[a-z0-9-]+:[a-z0-9_]+|d8:state-snapshotter:backup-agent`)

// TestPartnerDocCoveredByDocsGuards pins that the guards in this file actually read the backup
// integration document. They scan docs/*.md and fail only when the WHOLE directory yields nothing, so
// renaming that document, moving it, or emptying its endpoint tables would leave them green while the
// one document written for readers outside this repository goes unchecked.
//
// LIMIT: it pins that the document contributes findings to those guards, not that it is complete. What
// belongs in it is an editorial question no test can answer.
func TestPartnerDocCoveredByDocsGuards(t *testing.T) {
	served := docsServedSubresources(t)
	for _, groupVersion := range partnerDocGroupVersions {
		if _, ok := served[groupVersion]; !ok {
			t.Fatalf("%q is documented as an aggregated group/version the client addresses but nothing serves it (served: %s) — the document's API table is stale",
				groupVersion, docsSortedGroupVersions(served))
		}
	}

	scanned := map[string]string{}
	for _, file := range docsMarkdownFiles(t) {
		scanned[filepath.Base(file)] = file
	}

	for _, name := range []string{partnerDocEN, partnerDocRU} {
		path, present := scanned[name]
		if !present {
			t.Fatalf("%s is not among the documents the guards in this file scan — it was renamed or moved, and the guards that keep it honest silently stopped reading it", name)
		}

		body := docsReadFile(t, path)
		endpoints := docsAggregatedEndpointSet(t, body)
		if len(endpoints) == 0 {
			t.Errorf("%s names no complete aggregated endpoint, so TestDocsNameOnlyServedSubresources checks nothing in it", name)
		}
		for _, groupVersion := range partnerDocGroupVersions {
			named := false
			for endpoint := range endpoints {
				if strings.HasPrefix(endpoint, groupVersion+" ") {
					named = true
					break
				}
			}
			if !named {
				t.Errorf("%s names no endpoint of %q (named: %s) — the version of that group is then documented nowhere a guard can read it",
					name, groupVersion, docsSortedKeys(endpoints))
			}
		}
		if fields := docsSnapshotSpecFieldSet(body); len(fields) == 0 {
			t.Errorf("%s mentions no Snapshot spec field, so TestDocsNameOnlyExistingSpecFields checks nothing in it", name)
		}
		if roles := docsRoleNameSet(body); len(roles) == 0 {
			t.Errorf("%s names no capability role, so the permissions it describes are pinned by nothing", name)
		}

		t.Logf("%s: %d aggregated endpoint(s), %d Snapshot spec field(s), %d capability role name(s), %d Ready reason(s)",
			name, len(endpoints), len(docsSnapshotSpecFieldSet(body)), len(docsRoleNameSet(body)), len(docsReasonMentionSet(t, body)))
	}
}

// TestPartnerDocRuMirrorMatchesEn compares the mirror of the backup integration document with its
// canonical file on everything a client copies out of it verbatim: endpoints, Snapshot spec fields,
// capability role names and Ready reasons. Prose is not compared — the mirror is a translation.
//
// Half-updated mirrors are the normal way a translated document rots: the canonical file is corrected,
// the other keeps the old endpoint name, and a reader of the second one implements something that no
// longer exists. Every identifier compared here is language-independent, so equality is the whole rule.
func TestPartnerDocRuMirrorMatchesEn(t *testing.T) {
	scanned := map[string]string{}
	for _, file := range docsMarkdownFiles(t) {
		scanned[filepath.Base(file)] = file
	}
	canonical, mirror := scanned[partnerDocEN], scanned[partnerDocRU]
	if canonical == "" || mirror == "" {
		t.Fatalf("both files of the backup integration document must be under docs/ (found canonical=%q mirror=%q)", canonical, mirror)
	}
	canonicalBody, mirrorBody := docsReadFile(t, canonical), docsReadFile(t, mirror)

	dimensions := []struct {
		what    string
		extract func(string) map[string]bool
	}{
		{"aggregated endpoint", func(body string) map[string]bool { return docsAggregatedEndpointSet(t, body) }},
		{"Snapshot spec field", docsSnapshotSpecFieldSet},
		{"capability role name", docsRoleNameSet},
		{"Ready reason", func(body string) map[string]bool { return docsReasonMentionSet(t, body) }},
	}

	compared := 0
	for _, dimension := range dimensions {
		want, got := dimension.extract(canonicalBody), dimension.extract(mirrorBody)
		if len(want) == 0 {
			t.Errorf("%s mentions no %s at all — the comparison for it would pass on any mirror", partnerDocEN, dimension.what)
			continue
		}
		compared += len(want)
		for key := range want {
			if !got[key] {
				t.Errorf("%s: %s %q is in %s but missing from the mirror", partnerDocRU, dimension.what, key, partnerDocEN)
			}
		}
		for key := range got {
			if !want[key] {
				t.Errorf("%s: %s %q is in the mirror but absent from %s (the canonical file is the one to change first)", partnerDocRU, dimension.what, key, partnerDocEN)
			}
		}
	}

	t.Logf("compared %d identifier(s) across %d dimension(s) between %s and %s", compared, len(dimensions), partnerDocEN, partnerDocRU)
	if compared == 0 {
		t.Fatal("nothing was compared between the two files — zero identifiers means the extractors stopped matching, not that the mirror is faithful")
	}
}

// partnerDocArchiveHeadingEN and partnerDocArchiveHeadingRU open the section describing the file tree a
// stored snapshot is handed to a person as. Unlike the rest of the document that section is not about
// this module's HTTP API at all, so none of the guards above reach a single name in it.
const (
	partnerDocArchiveHeadingEN = "## The snapshot archive"
	partnerDocArchiveHeadingRU = "## Архив снимка"
)

// docsFencedBlockRE matches a fenced code block, fences included.
var docsFencedBlockRE = regexp.MustCompile("(?s)```.*?```")

// docsInlineCodeRE matches one inline code span. A span never spans lines, so a stray backtick costs at
// most one line rather than swallowing the rest of the document.
var docsInlineCodeRE = regexp.MustCompile("`([^`\n]+)`")

// docsPlaceholderRE matches a <placeholder>. Its contents are prose and therefore translated
// (`<kind in lower case>` against `<kind в нижнем регистре>`), while the shape around it is contract.
var docsPlaceholderRE = regexp.MustCompile(`<[^<>]*>`)

// TestPartnerDocArchiveSectionMirrored compares the archive section of the two files on every inline
// code span in it: file and directory names, envelope field names, digest constants, the domain string
// of the children digest, and the client invocations a reader copies out. A span is compared with its
// <placeholders> collapsed, so the prose inside a placeholder may be translated while the shape around
// it may not.
//
// The section is a format specification written from someone else's code, which is the failure mode
// this guards against: the two files drift the moment one of them is corrected against that code and
// the other is not, and a reader of the stale one assembles an archive that is refused with no hint of
// which half of the document lied.
//
// LIMITS:
//   - the wide one: the names and rules in that section belong to the `d8` command-line client and to
//     the web console, both of which live in OTHER repositories that no test here can read. So this
//     pins that the two files say the same thing, NOT that either of them still matches the format
//     those tools implement. Nothing local can establish the latter; only running `d8 snapshot local
//     describe` against a real archive can, and the section says so to its reader as well.
//   - SETS are compared, not occurrences. A span dropped from one of several places it appears in is
//     invisible here — what is pinned is the vocabulary of the section, not each sentence in it.
//   - only what is written in backticks. A translated rule that inverts its meaning while keeping the
//     identifiers is an editorial defect no comparison of names can see. Concretely: a numeric bound
//     stated in prose rather than in a code span is invisible here, which is why the limits table
//     writes its values as code spans — as a plain table it compared nothing but the three file names
//     in its own row labels, and dropping the whole table from one file left this test green.
func TestPartnerDocArchiveSectionMirrored(t *testing.T) {
	scanned := map[string]string{}
	for _, file := range docsMarkdownFiles(t) {
		scanned[filepath.Base(file)] = file
	}
	canonical, mirror := scanned[partnerDocEN], scanned[partnerDocRU]
	if canonical == "" || mirror == "" {
		t.Fatalf("both files of the backup integration document must be under docs/ (found canonical=%q mirror=%q)", canonical, mirror)
	}

	want := docsInlineCodeSet(docsSection(t, partnerDocEN, docsReadFile(t, canonical), partnerDocArchiveHeadingEN))
	got := docsInlineCodeSet(docsSection(t, partnerDocRU, docsReadFile(t, mirror), partnerDocArchiveHeadingRU))

	for span := range want {
		if !got[span] {
			t.Errorf("%s: the archive section names %q, which its mirror does not", partnerDocEN, span)
		}
	}
	for span := range got {
		if !want[span] {
			t.Errorf("%s: the archive section names %q, which is absent from %s (the canonical file is the one to change first)", partnerDocRU, span, partnerDocEN)
		}
	}

	t.Logf("compared %d code span(s) of the archive section against %d in the mirror", len(want), len(got))
	if len(want) == 0 {
		t.Fatal("the archive section of the canonical file names nothing in backticks — zero spans means the section was emptied or the extractor stopped matching, not that the mirror is faithful")
	}
}

// docsSection returns the body of one "## " section, its own heading excluded, up to the next section of
// the same level or the end of the file. Fenced blocks are cut first, so neither a heading nor a
// backtick inside an example can be read as document structure.
func docsSection(t *testing.T, file, body, heading string) string {
	t.Helper()

	var section []string
	inside := false
	for _, line := range strings.Split(docsFencedBlockRE.ReplaceAllString(body, ""), "\n") {
		if strings.TrimSpace(line) == heading {
			inside = true
			continue
		}
		if inside && strings.HasPrefix(line, "## ") {
			break
		}
		if inside {
			section = append(section, line)
		}
	}
	if !inside {
		t.Fatalf("%s has no %q section — it was renamed or removed, and comparing it against the other file would then compare nothing", file, heading)
	}
	return strings.Join(section, "\n")
}

// docsInlineCodeSet returns the inline code spans of a passage, each with its <placeholders> collapsed
// to "<>" so that a translated placeholder is not read as a changed identifier.
func docsInlineCodeSet(passage string) map[string]bool {
	spans := map[string]bool{}
	for _, match := range docsInlineCodeRE.FindAllStringSubmatch(passage, -1) {
		spans[docsPlaceholderRE.ReplaceAllString(match[1], "<>")] = true
	}
	return spans
}

// docsAggregatedEndpointSet returns the complete aggregated endpoints a document names, each normalized
// to "<group>/<version> <resource>/<subresource>" so that the namespace and object name — which differ
// between examples and carry no contract — are not part of the comparison. References that stop short
// of a subresource are skipped here; TestDocsNameOnlyServedSubresources checks those.
func docsAggregatedEndpointSet(t *testing.T, body string) map[string]bool {
	t.Helper()

	endpoints := map[string]bool{}
	for _, raw := range docsAggregatedPathRE.FindAllString(body, -1) {
		parsed, err := docsParseAggregatedPath(raw)
		if err != nil || !parsed.hasSubresource || parsed.version == "" {
			continue
		}
		if docsIsPlaceholder(parsed.resource) || docsIsPlaceholder(parsed.subresource) {
			continue
		}
		endpoints[parsed.groupVersion()+" "+parsed.resource+"/"+parsed.subresource] = true
	}
	return endpoints
}

// docsSnapshotSpecFieldSet returns the Snapshot spec fields a document mentions, qualified references
// to other kinds included (as "Kind.spec.field"): a field that moves from one kind to the other in one
// file only is exactly the kind of drift this comparison is for.
func docsSnapshotSpecFieldSet(body string) map[string]bool {
	fields := map[string]bool{}
	for _, match := range docsSpecFieldRE.FindAllStringSubmatch(body, -1) {
		qualifier, field := match[1], match[2]
		if qualifier == "" {
			fields["spec."+field] = true
			continue
		}
		fields[qualifier+".spec."+field] = true
	}
	return fields
}

// docsRoleNameSet returns the capability role names a document names.
func docsRoleNameSet(body string) map[string]bool {
	roles := map[string]bool{}
	for _, name := range docsRoleNameRE.FindAllString(body, -1) {
		roles[name] = true
	}
	return roles
}

// docsReasonMentionSet returns the catalog Ready reasons a document mentions anywhere, matched as whole
// words so that one reason name being a prefix of another cannot count twice. Reasons outside the
// catalogs are not searched for: they are not contract, and the document says so.
func docsReasonMentionSet(t *testing.T, body string) map[string]bool {
	t.Helper()

	mentioned := map[string]bool{}
	for reason := range docsReadyReasons(t) {
		if regexp.MustCompile(`\b` + regexp.QuoteMeta(reason) + `\b`).MatchString(body) {
			mentioned[reason] = true
		}
	}
	return mentioned
}

// TestDocsShowOnlyGeneratedContentNames checks the CONTENT column of the kubectl-style examples against
// the shape this module actually generates. Content names are derived from the owning object's UID, so
// no cluster ever shows a hand-written one — and a hand-written one is exactly what these examples
// carried until it was noticed by eye rather than by a test.
//
// The shape is derived from a name the generator produces, not written out here, so a changed prefix or
// hash width moves the guard with the code instead of outdating it.
//
// LIMIT: the value's SHAPE, nothing else. Whether the example's snapshot would really bind to a content
// with that particular hash is not a question a document can answer.
func TestDocsShowOnlyGeneratedContentNames(t *testing.T) {
	shape, elided := docsContentNameShapes(t)
	files := docsMarkdownFiles(t)

	var checked, elidedChecked int
	for _, file := range files {
		for _, value := range docsColumnTokens(docsReadFile(t, file), "CONTENT") {
			checked++
			// Cutting a long name short with an ellipsis is legitimate — it is how kubectl output is
			// usually quoted — but what is left visible must still be the beginning of a name this
			// module generates. Skipping elided values outright would have let through the very value
			// this guard was written for, which ended in an ellipsis and was wrong before it.
			if strings.HasSuffix(value, "...") {
				elidedChecked++
				if !elided.MatchString(value) {
					t.Errorf("%s: an output example shows content name %q, whose visible part is not the start of a name this module generates (expected: %s)",
						filepath.Base(file), value, elided)
				}
				continue
			}
			if !shape.MatchString(value) {
				t.Errorf("%s: an output example shows content name %q, which this module cannot generate (expected shape: %s)",
					filepath.Base(file), value, shape)
			}
		}
	}

	t.Logf("scanned %d document(s) under docs/, checked %d content name(s) against %s, %d of them elided", len(files), checked, shape, elidedChecked)
	if checked == 0 {
		t.Fatal("no content name found in an output example under docs/ — zero findings means the column stopped being read, not that the examples are right")
	}
}

// docsContentNameShapes returns the shape of a generated SnapshotContent name and the shape of one cut
// short with an ellipsis, both built from a name the generator actually produces.
func docsContentNameShapes(t *testing.T) (full, elided *regexp.Regexp) {
	t.Helper()

	sample := names.ContentName(types.UID("shape-probe"))
	if !strings.HasPrefix(sample, names.PrefixContent) {
		t.Fatalf("generated content name %q does not start with the published prefix %q", sample, names.PrefixContent)
	}
	hexLen := len(sample) - len(names.PrefixContent)
	if hexLen == 0 {
		t.Fatalf("generated content name %q is the bare prefix — the shape below would match anything", sample)
	}

	prefix := regexp.QuoteMeta(names.PrefixContent)
	full = regexp.MustCompile("^" + prefix + fmt.Sprintf("[0-9a-f]{%d}$", hexLen))
	elided = regexp.MustCompile("^" + prefix + fmt.Sprintf("[0-9a-f]{0,%d}\\.\\.\\.$", hexLen))
	if !full.MatchString(sample) {
		t.Fatalf("the generator produced %q, which the shape derived from it (%s) does not match — the name scheme changed in a way this guard cannot read", sample, full)
	}
	if !elided.MatchString(names.PrefixContent + "...") {
		t.Fatalf("the elided shape %s does not match a name cut right after the prefix — it is built wrong", elided)
	}
	return full, elided
}
