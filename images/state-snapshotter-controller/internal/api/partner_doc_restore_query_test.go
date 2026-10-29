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

// The restoration read takes no namespace from its caller: a subtree is compiled into the namespace of
// the Snapshot that was addressed, and nothing in the request can say otherwise. Moving a namespace
// elsewhere is the download-and-import path, which carries the volume data with it; there is no
// address-only variant, because one would work for a namespace with no volumes and silently emit
// never-binding claims for one with them — the difference a caller cannot see.
//
// The promise is easy to un-make by accident: an unknown query parameter is not rejected by an
// aggregated apiserver, so a handler that started reading one again would change where answers are
// addressed while every existing caller kept working. So this guard asserts three things at once — that
// NEITHER file names a query parameter of this read; that a request which tries to address the output into
// another namespace is refused with 400 rather than quietly answered elsewhere, while the addressed
// Snapshot's own namespace is still served; and that both files NAME that refusal.
//
// The first of the three is two checks, because neither covers the other, and both run over BOTH files —
// a client author works from one file, not from the pair, so a property that holds only for the canonical
// one is not a property of the contract. A pattern catches a parameter of ANY name, wherever the name sits
// in a code span next to the word for "parameter", in either order and in either language; what it cannot
// see is a name with no markup on it. So the ONE name that was removed is additionally matched as a bare
// substring over the whole of each file, which catches it in a code span, in prose and in a URL alike.
//
// Making the pattern bilingual is not decoration. The mirror does not contain the word "query" anywhere,
// and Russian puts the name after the words instead of before, so a pattern built around the English
// phrasing runs over the mirror and matches nothing that could ever be there — a check that reports
// success because it is looking for something the file would never say.
//
// The pattern also runs against passages it MUST match, one per form it claims, because its expected yield
// over the real documents is zero and a pattern that stopped matching anything would otherwise read as
// clean files. Those controls name a parameter that is NOT the removed one — a control built from the
// removed name would still pass after the pattern was narrowed down to that single name, and a narrowed
// pattern is blind to precisely what the pattern is for, a NEW parameter appearing in the document — and
// there is one control per language and word order, so a pattern that quietly stopped covering the mirror
// cannot keep looking armed.
//
// The last of the three exists because the refusal runs against the platform's own convention: an
// aggregated apiserver ignores a query key it does not recognize, so a client author who reads nothing
// about it will assume it was ignored. A wire contract that stays silent there is wrong by omission, and
// the code named in it is checked against the code the endpoint actually served, so the two cannot drift.
//
// The other guards over that document compare endpoints, spec fields, role names and reasons; a query
// parameter is none of those, so nothing else here would notice.
//
// This is a plain testing guard rather than a Ginkgo spec, and it reads the document from disk, for the
// same reason as its neighbours in this file's package: the subject of the check IS the repository file,
// and //go:embed cannot reach outside the package directory, so an embedded fixture would be a copy of
// the document checking itself.

import (
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// partnerDocRestoreHeadingEN opens the section of the canonical document that describes the restoration
// read. The check is scoped there so a code span of the same shape elsewhere cannot be mistaken for it.
const partnerDocRestoreHeadingEN = "## Reading a snapshot for restoration"

// partnerDocQueryParamRE matches a query parameter the document names: a name in a code span next to the
// word for "parameter", in either order. It catches a parameter of ANY name, which no literal can do.
//
// Both languages and both word orders are in it because the two files genuinely differ. The mirror does
// not contain the word "query" ANYWHERE — it calls the same thing "параметр запроса", and Russian puts the
// name after the words rather than before — so a pattern written around the English phrase would be a
// check that silently does nothing on half of the contract. The vocabulary here is taken from what the two
// files actually write ("query parameter", "параметр запроса", "параметр"), not from invented forms; the
// stem is what is matched, so a case ending or a "query-параметр" spelling is covered without enumerating
// declensions.
//
// LIMIT: the name has to be in a code span, and within a short distance of the word. A parameter
// introduced in running prose with no markup is invisible here. For the ONE name that was removed the
// literal check below closes that gap in any wording; for a name nobody has thought of yet, the gap stays
// with the reviewer.
var partnerDocQueryParamRE = regexp.MustCompile(
	"(?i)`([A-Za-z][A-Za-z0-9]*)`[^`\n]{0,40}?(?:parameter|параметр)" +
		"|(?:parameter|параметр)[^`\n]{0,40}?`([A-Za-z][A-Za-z0-9]*)`")

// partnerDocQueryParamControlName is the parameter name the control passages below are written around.
// It is deliberately NOT the removed one: building a control out of that name would leave the pattern free
// to be narrowed down to it — the obvious "do not write the name twice" refactor — and a pattern narrowed
// to one name still passes its control while going blind to exactly what it is for, a NEW parameter
// appearing in the document. A name no document would ever carry proves the generality as well as the
// shape.
const partnerDocQueryParamControlName = "noSuchParameterEverExisted"

// partnerDocQueryParamControls are passages partnerDocQueryParamRE MUST match, one per form it claims to
// cover. An extractor whose expected yield over the real documents is zero cannot otherwise tell "the
// documents are clean" from "the pattern stopped matching anything": a reworded pattern, or one narrowed
// to the English phrasing, would silently disarm it and read as a pass. The neighbouring guards over this
// document get that assurance for free, because theirs expect a non-empty result.
//
// One control per form is what makes the Russian half real rather than nominal: with only the English one,
// a pattern that had stopped matching the mirror's vocabulary would still look armed.
//
// LIMIT: a control catches the pattern being narrowed, not the control being rewritten. Someone who edits
// these passages to suit a narrowed pattern in the same change gets a green test, and no positive control
// can prevent that — it only means the disarming has to be deliberate rather than a side effect of
// tidying the name up.
var partnerDocQueryParamControls = map[string]string{
	"English, name before the words":    "you may pass the `" + partnerDocQueryParamControlName + "` query parameter to name it",
	"Russian, name after the words":     "можно передать query-параметр `" + partnerDocQueryParamControlName + "`",
	"Russian, the mirror's own wording": "принимает параметр запроса `" + partnerDocQueryParamControlName + "` для выбора",
}

// partnerDocRestoreHeadingRU opens the same section of the mirror. Every check below runs over both files:
// a client author works from one of them, not from the pair, so a property that holds for the canonical
// file alone is not a property of the contract.
const partnerDocRestoreHeadingRU = "## Чтение снимка для восстановления"

// partnerDocStatusCodeRE matches an HTTP status code written the way both files write one: in a code
// span. Prose digits are deliberately not matched — a code the document states as contract is marked up
// as one.
var partnerDocStatusCodeRE = regexp.MustCompile("`([1-5][0-9]{2})`")

// removedRestoreQueryParam is the name the endpoint used to read. It is written here rather than taken
// out of the document precisely because the document must no longer contain it: this is the one guard in
// the set whose subject is an ABSENCE, so it has to carry the name itself. It is matched as a bare
// substring, with no markup around it, so that the name is caught in a code span, in plain prose, and in
// a URL alike.
const removedRestoreQueryParam = "targetNamespace"

// partnerDocQueryParamName returns the parameter name out of one partnerDocQueryParamRE match. The pattern
// has one alternative per word order, so exactly one of its two groups is filled and the other is empty.
func partnerDocQueryParamName(match []string) string {
	if match[1] != "" {
		return match[1]
	}
	return match[2]
}

// TestPartnerDocRestoreReadTakesNoQueryNamespace proves the restoration read has no caller-chosen target
// namespace, on both sides at once: the section names no query parameter, and a request passing the name
// that was removed cannot move the answer — a foreign namespace is refused with 400, and the addressed
// Snapshot's own namespace is served and is where the objects come back addressed. Then it closes the
// loop back onto the document: the status served is the status both files name. The behavioural half is
// what makes this more than a spell-check — a reworded document over a handler still reading the name
// would pass the first and fail the second.
//
// LIMIT: the namespace parameter, in the two files of the backup integration document only. The
// endpoint's other query parameters (scope and the object filter) are real, appear nowhere in this
// document, and are pinned next to the parser that decides them. And docs/USER_GUIDE*.md describes the
// same endpoint in the same terms — that the caller cannot choose where the objects land, and that a
// request trying to is refused — without being covered here; widening the scope of this guard to a second
// document is a decision of its own, so for now those two files are held by review.
//
// LIMIT: markup is the boundary. The removed name is caught in any wording at all; any OTHER name is
// caught only where the document puts it in a code span near the word for "parameter". A new parameter
// introduced in bare prose — "add destination=<ns> to the query" — passes both halves. Closing that would
// mean pinning the document's prose, which is not something a guard can hold; it stays with the reviewer.
func TestPartnerDocRestoreReadTakesNoQueryNamespace(t *testing.T) {
	scanned := map[string]string{}
	for _, file := range docsMarkdownFiles(t) {
		scanned[filepath.Base(file)] = file
	}
	canonical, mirror := scanned[partnerDocEN], scanned[partnerDocRU]
	if canonical == "" || mirror == "" {
		t.Fatalf("both files of the backup integration document must be under docs/ (found canonical=%q mirror=%q)", canonical, mirror)
	}

	docs := []struct {
		name    string
		file    string
		heading string
	}{
		{name: partnerDocEN, file: canonical, heading: partnerDocRestoreHeadingEN},
		{name: partnerDocRU, file: mirror, heading: partnerDocRestoreHeadingRU},
	}

	for form, control := range partnerDocQueryParamControls {
		got := partnerDocQueryParamRE.FindAllStringSubmatch(control, -1)
		if len(got) != 1 {
			t.Fatalf("the query-parameter pattern matched %d time(s) in its %s control passage %q, want exactly 1 — it no longer recognizes what it exists to find, so its silence over the documents below would mean nothing",
				len(got), form, control)
		}
		if name := partnerDocQueryParamName(got[0]); name != partnerDocQueryParamControlName {
			t.Fatalf("the query-parameter pattern read the name %q out of its %s control passage %q, want %q — it matches the passage but not the name in it, so what it reports out of a document cannot be trusted either",
				name, form, control, partnerDocQueryParamControlName)
		}
	}
	t.Logf("query-parameter pattern proven on %d control form(s)", len(partnerDocQueryParamControls))
	for _, doc := range docs {
		// The section names NO query parameter, of any name. The document says how to use the read, not
		// what it used to take: a client that follows it never sends a namespace, so the refusal below is
		// a guard rail it cannot hit, and describing the guard rail would only teach the name back.
		names := map[string]bool{}
		for _, match := range partnerDocQueryParamRE.FindAllStringSubmatch(docsSection(t, doc.name, docsReadFile(t, doc.file), doc.heading), -1) {
			names[partnerDocQueryParamName(match)] = true
		}
		if len(names) != 0 {
			t.Errorf("%s names query parameter(s) %s of the restoration read in %q; the read takes no namespace from its caller, so either it grew one and this guard needs replacing, or the sentence describes something that does not exist",
				doc.name, docsSortedKeys(names), doc.heading)
		}
		// The removed name is checked separately, as a bare substring over the WHOLE file: the pattern
		// above sees only one shape, and "you may pass the query parameter targetNamespace to name it" is
		// ordinary English in none of it. Both files get both checks — the canonical one is what an
		// external product implements against, so holding it to less than the translation would be
		// backwards.
		if strings.Contains(docsReadFile(t, doc.file), removedRestoreQueryParam) {
			t.Errorf("%s mentions %q; neither file may teach a parameter the endpoint refuses — a client that implements it gets a 400",
				doc.name, removedRestoreQueryParam)
		}
	}

	cl := fake.NewClientBuilder().WithScheme(coreSnapshotScheme()).Build()
	seedCoreRootReadySnapshot(t, cl, []map[string]interface{}{
		{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]interface{}{"name": "app-config", "namespace": "captured-elsewhere-ns"}, "data": map[string]interface{}{"k": "v"}},
	})
	srv := newCoreSnapshotServer(t, cl)
	defer srv.Close()

	// A foreign value is REFUSED, not ignored. Silently answering it is what let a caller ask for
	// namespace B, receive objects addressed into A together with a 200, and apply them over A.
	resp, err := http.Get(srv.URL + coreRestoreBase + "?scope=node&" + removedRestoreQueryParam + "=a-namespace-nobody-can-ask-for")
	if err != nil {
		t.Fatalf("request with a foreign target namespace: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a request naming a foreign %s answered %d, want 400 — a namespace the read cannot deliver must be refused, not silently replaced with the snapshot's own",
			removedRestoreQueryParam, resp.StatusCode)
	}

	// The equal value is still accepted: today's clients pass the addressed namespace and mean nothing
	// by it. Refusing ANY presence is the end state, once no client sends it.
	objs := getAggregatedObjects(t, srv.URL+coreRestoreBase+"?scope=node&"+removedRestoreQueryParam+"=ns1")
	if len(objs) != 1 {
		t.Fatalf("restoration read returned %d object(s), want the single seeded ConfigMap — with nothing returned the namespace assertion below checks nothing", len(objs))
	}
	if got := (&unstructured.Unstructured{Object: objs[0]}).GetNamespace(); got != "ns1" {
		t.Errorf("answer addressed into %q, want the addressed snapshot's own ns1 (the manifest was captured in a third namespace, so this is a real rewrite and not a restatement)", got)
	}

	// Both files must state the code that refusal carries, and the code checked is the one the endpoint
	// just answered rather than a second copy written here — so a document that stops saying it, and a
	// document that keeps saying a code the endpoint no longer serves, both redden.
	//
	// LIMIT: the presence of the code and its agreement with the behaviour, not the sentence around it. A
	// section naming the code for an unrelated reason would satisfy this; what it cannot do is fall silent
	// or name only codes the endpoint does not serve here.
	for _, doc := range docs {
		codes := map[string]bool{}
		for _, match := range partnerDocStatusCodeRE.FindAllStringSubmatch(docsSection(t, doc.name, docsReadFile(t, doc.file), doc.heading), -1) {
			codes[match[1]] = true
		}
		if !codes[strconv.Itoa(resp.StatusCode)] {
			t.Errorf("%s: %q does not name the %d the endpoint answers a request that tries to address the output into another namespace (the section names: %q); an aggregated apiserver ignores a query key it does not know, so a client author reads silence here as \"ignored\"",
				doc.name, doc.heading, resp.StatusCode, docsSortedKeys(codes))
		}
	}

	t.Logf("checked %d file(s): each names no query parameter of the restoration read and does not mention %q anywhere; a foreign value is refused with %d, the equal value is still served, and both files name that status",
		len(docs), removedRestoreQueryParam, resp.StatusCode)
}
