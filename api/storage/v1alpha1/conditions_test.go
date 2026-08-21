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

package v1alpha1

import (
	"os"
	"sort"
	"strings"
	"testing"
)

func TestIsReasonDegraded(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		want   bool
	}{
		{name: "recoverable degradation", reason: ReasonChildSnapshotDeleted, want: true},

		// Terminal reasons are NOT degraded (disjoint from DegradedReadyReasons).
		{name: "terminal ChildSnapshotLost", reason: ReasonChildSnapshotLost, want: false},
		{name: "terminal ListFailed", reason: "ListFailed", want: false},
		{name: "terminal VolumeCaptureFailed", reason: "VolumeCaptureFailed", want: false},
		{name: "terminal GraphPlanningFailed", reason: ReasonGraphPlanningFailed, want: false},

		// In-progress (non-terminal, non-degraded) reasons are NOT degraded.
		{name: "in-progress DataCapturePending", reason: "DataCapturePending", want: false},
		{name: "in-progress ChildrenPending", reason: "ChildrenPending", want: false},
		{name: "in-progress ImportPending", reason: "ImportPending", want: false},

		// Success and empty are NOT degraded.
		{name: "success Completed", reason: ReasonCompleted, want: false},
		{name: "empty reason", reason: "", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsReasonDegraded(tc.reason); got != tc.want {
				t.Fatalf("IsReasonDegraded(%q) = %v, want %v", tc.reason, got, tc.want)
			}
		})
	}
}

// TestDegradedReadyReasons_ExactMembership pins the catalog against silent drift: it must contain
// exactly {ChildSnapshotDeleted}. UI and d8 branch on this set, so a change here is a contract change.
func TestDegradedReadyReasons_ExactMembership(t *testing.T) {
	if len(DegradedReadyReasons) != 1 {
		t.Fatalf("DegradedReadyReasons must have exactly 1 member, got %d: %v", len(DegradedReadyReasons), DegradedReadyReasons)
	}
	if _, ok := DegradedReadyReasons[ReasonChildSnapshotDeleted]; !ok {
		t.Fatalf("DegradedReadyReasons must contain %q", ReasonChildSnapshotDeleted)
	}
}

// TestTerminalReadyReasons_ExactMembership pins the catalog against silent drift, exactly like the
// degraded-catalog pin below: UI and d8 branch on this set, so a change here is a contract change.
// ArtifactMissing and DomainCaptureFailed are members on purpose: they originate on SnapshotContent,
// but the Ready mirror copies them verbatim onto owner snapshot objects, so clients observe them.
func TestTerminalReadyReasons_ExactMembership(t *testing.T) {
	want := []string{
		"ListFailed",
		"ManifestCheckpointFailed",
		"NamespaceNotFound",
		"VolumeCaptureFailed",
		ReasonArtifactMissing,
		"DomainCaptureFailed",
		"DuplicateCoveredPVCUID",
		"ChildrenFailed",
		ReasonGraphPlanningFailed,
		ReasonCreateChildFailed,
		ReasonChildSnapshotLost,
	}
	if len(TerminalReadyReasons) != len(want) {
		t.Fatalf("TerminalReadyReasons must have exactly %d members, got %d: %v", len(want), len(TerminalReadyReasons), TerminalReadyReasons)
	}
	for _, reason := range want {
		if _, ok := TerminalReadyReasons[reason]; !ok {
			t.Fatalf("TerminalReadyReasons must contain %q", reason)
		}
	}
}

// TestDegradedAndTerminalReasonsDisjoint proves no reason is classified as both degraded and terminal.
func TestDegradedAndTerminalReasonsDisjoint(t *testing.T) {
	for reason := range DegradedReadyReasons {
		if _, ok := TerminalReadyReasons[reason]; ok {
			t.Errorf("reason %q is in both DegradedReadyReasons and TerminalReadyReasons; the sets must be disjoint", reason)
		}
	}
	for reason := range TerminalReadyReasons {
		if _, ok := DegradedReadyReasons[reason]; ok {
			t.Errorf("reason %q is in both TerminalReadyReasons and DegradedReadyReasons; the sets must be disjoint", reason)
		}
	}
}

// ---------------------------------------------------------------------------
// The reason table of the backup integration document
// ---------------------------------------------------------------------------

// partnerDocs are the two files of the backup integration document — the client wire contract an
// external backup product implements against. The English file is canonical and the other mirrors it.
// The paths are relative to this package: api/storage/v1alpha1 sits three levels under the repository
// root.
var partnerDocs = struct{ canonical, mirror string }{
	canonical: "../../../docs/BACKUP_INTEGRATION.md",
	mirror:    "../../../docs/BACKUP_INTEGRATION_RU.md",
}

// partnerDocClassTranslations pairs each class word of the canonical document with the word its mirror
// uses. The mirror is a translation, so its class column cannot be compared literally — and it must
// still be compared, because a row that changes class in one file only turns the two documents into two
// different contracts. A class word appearing in either file without a pair here fails the guard rather
// than dropping its rows out of the comparison unnoticed.
var partnerDocClassTranslations = map[string]string{
	"Terminal":    "Терминальный",
	"Recoverable": "Восстановимый",
}

// TestPartnerDocReasonTableMatchesCatalogs pins the Ready=False reason table of the backup integration
// document against the condition catalogs, in both directions: every reason the document classifies is
// in the matching catalog, and every catalog member is in the document. An external product branches on
// this classification — terminal means it stops and reports a failure, recoverable means it stops and
// asks for a person — so a catalog that gains a member the document does not list leaves that product
// polling forever on a snapshot that can never become ready.
//
// It also compares the mirror against the canonical file row by row, through the class translations
// declared above.
//
// SCOPE: the reason NAME and its class, i.e. the first two columns. The third column is prose and is
// compared with nothing. The success reason (Ready=True) is deliberately outside the table, which
// describes Ready=False only; the document states it in its own text, where the document guards in the
// controller image check it.
//
// The documents are read from disk rather than embedded: the subject of the check IS the repository
// file, and //go:embed cannot reach outside this package, so an embedded fixture would be a copy of the
// file checking itself.
func TestPartnerDocReasonTableMatchesCatalogs(t *testing.T) {
	canonical := partnerDocReasonTable(t, partnerDocs.canonical)
	mirror := partnerDocReasonTable(t, partnerDocs.mirror)

	terminal := map[string]bool{}
	recoverable := map[string]bool{}
	for reason, class := range canonical {
		switch class {
		case "Terminal":
			terminal[reason] = true
		case "Recoverable":
			recoverable[reason] = true
		default:
			t.Errorf("%s: reason %q is classified as %q, which is neither of the two classes this guard knows (%s) — a class it cannot read is a row it cannot check",
				partnerDocs.canonical, reason, class, strings.Join(sortedPartnerDocClasses(), ", "))
		}
	}

	comparePartnerDocReasons(t, "terminal", terminal, TerminalReadyReasons)
	comparePartnerDocReasons(t, "recoverable", recoverable, DegradedReadyReasons)

	for reason, class := range canonical {
		want, paired := partnerDocClassTranslations[class]
		if !paired {
			continue // already reported above
		}
		got, listed := mirror[reason]
		if !listed {
			t.Errorf("%s: reason %q is missing from the mirror — the two files must describe the same contract", partnerDocs.mirror, reason)
			continue
		}
		if got != want {
			t.Errorf("%s: reason %q is classified as %q, but the canonical file classifies it as %q (%q)", partnerDocs.mirror, reason, got, class, want)
		}
	}
	for reason := range mirror {
		if _, listed := canonical[reason]; !listed {
			t.Errorf("%s: reason %q is absent from the canonical file", partnerDocs.mirror, reason)
		}
	}

	t.Logf("checked %d reason row(s) in the canonical document and %d in its mirror against %d terminal and %d recoverable catalog reason(s)",
		len(canonical), len(mirror), len(TerminalReadyReasons), len(DegradedReadyReasons))
	if len(canonical) == 0 || len(mirror) == 0 {
		t.Fatal("a reason table came back empty — zero rows means the table stopped being found, not that the document agrees with the catalogs")
	}
}

// comparePartnerDocReasons compares one documented class against its catalog in both directions.
func comparePartnerDocReasons(t *testing.T, class string, documented map[string]bool, catalog map[string]struct{}) {
	t.Helper()

	for reason := range documented {
		if _, ok := catalog[reason]; !ok {
			t.Errorf("%s: reason %q is documented as %s, but it is not in that catalog (catalog: %s)",
				partnerDocs.canonical, reason, class, strings.Join(sortedCatalogReasons(catalog), ", "))
		}
	}
	for reason := range catalog {
		if !documented[reason] {
			t.Errorf("%s: the %s catalog contains %q, which the document does not list — a client cannot classify a reason it was never told about",
				partnerDocs.canonical, class, reason)
		}
	}
}

// partnerDocReasonTable returns the reason -> class rows of the document's reason table. The table is
// found by its header, whose first two cells name the reason and its class; it ends at the first line
// that is not a table row, so no other table in the document can be read into it by accident.
func partnerDocReasonTable(t *testing.T, path string) map[string]string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	classes := map[string]string{}
	inTable := false
	for _, line := range strings.Split(string(data), "\n") {
		cells := partnerDocTableCells(line)
		if len(cells) < 2 {
			inTable = false
			continue
		}
		if !inTable {
			if partnerDocIsReasonTableHeader(cells) {
				inTable = true
			}
			continue
		}
		if strings.HasPrefix(cells[0], "---") {
			continue // the header separator row
		}
		reason := strings.Trim(cells[0], "`")
		if previous, duplicate := classes[reason]; duplicate {
			t.Errorf("%s: reason %q appears twice in the table (as %q and %q)", path, reason, previous, cells[1])
		}
		classes[reason] = cells[1]
	}
	return classes
}

// partnerDocIsReasonTableHeader reports whether a table header row opens the reason table. The reason
// column keeps its English name in both files (it is the field name a client reads); the class column
// is a word of the document's own language, so it is recognized through the translations above.
func partnerDocIsReasonTableHeader(cells []string) bool {
	if !strings.EqualFold(cells[0], "Reason") {
		return false
	}
	for class, translated := range partnerDocClassTranslations {
		if strings.EqualFold(cells[1], "Class") || strings.EqualFold(cells[1], class) || strings.EqualFold(cells[1], "Класс") || strings.EqualFold(cells[1], translated) {
			return true
		}
	}
	return false
}

// partnerDocTableCells splits a Markdown table row into trimmed cells, returning nil for anything that
// is not a table row.
func partnerDocTableCells(line string) []string {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "|") {
		return nil
	}
	parts := strings.Split(strings.Trim(trimmed, "|"), "|")
	cells := make([]string, 0, len(parts))
	for _, part := range parts {
		cells = append(cells, strings.TrimSpace(part))
	}
	return cells
}

func sortedCatalogReasons(catalog map[string]struct{}) []string {
	names := make([]string, 0, len(catalog))
	for reason := range catalog {
		names = append(names, reason)
	}
	sort.Strings(names)
	return names
}

func sortedPartnerDocClasses() []string {
	classes := make([]string, 0, len(partnerDocClassTranslations))
	for class := range partnerDocClassTranslations {
		classes = append(classes, class)
	}
	sort.Strings(classes)
	return classes
}
