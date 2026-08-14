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

package dataleg

import (
	"strings"
	"testing"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// completeBinding is a settled binding whose source carried every value: the state the MustBeSet cells are
// a contract about. Fixtures below start from it and remove exactly one thing, so a red result names one
// cause.
func completeBinding() *storagev1alpha1.SnapshotDataBinding {
	return &storagev1alpha1.SnapshotDataBinding{
		SourceRef: storagev1alpha1.SnapshotSubjectRef{
			APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc-a", Namespace: "ns1", UID: "pvc-a-uid",
		},
		ArtifactRef: storagev1alpha1.SnapshotDataArtifactRef{
			APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: "vsc-a", UID: "vsc-a-uid",
		},
		VolumeMode:       "Filesystem",
		FsType:           "ext4",
		StorageClassName: "sc-a",
		Size:             "500Mi",
	}
}

func blockBinding() *storagev1alpha1.SnapshotDataBinding {
	b := completeBinding()
	b.VolumeMode = blockVolumeMode
	b.FsType = "" // a raw block volume has no filesystem
	return b
}

// TestMatrix_DeclaredForEveryReflectedFieldOfEveryScenario is the contract's own shape check, and it prints
// the whole table plus the number of cells so that a green run says how much was declared rather than just
// "ok".
func TestMatrix_DeclaredForEveryReflectedFieldOfEveryScenario(t *testing.T) {
	if err := Validate(); err != nil {
		t.Fatalf("the live status.data completeness matrix is not valid:\n%v", err)
	}

	names := FieldNames()
	if len(names) == 0 {
		t.Fatal("reflection over SnapshotDataBinding yielded no fields; every check would be vacuous")
	}
	if got, want := len(Scenarios()), 4; got != want {
		t.Fatalf("scenario axis has %d cell(s), want %d (native capture, native import, domain capture, domain import)", got, want)
	}
	if got, want := Cells(), len(names)*len(Scenarios()); got != want {
		t.Fatalf("Cells() = %d, want fields*scenarios = %d", got, want)
	}
	t.Logf("declared %d cell(s) = %d field(s) %v x %d scenario(s) %v", Cells(), len(names), names, len(Scenarios()), Scenarios())
	t.Log("\n" + Table())
}

// TestMatrix_FieldAddedToTheStructWithNoExpectationIsRejected is the test the whole block exists for: a new
// field of SnapshotDataBinding must not be able to appear without its author declaring, per scenario, what
// that path is expected to publish. The added field is injected into the reflected set rather than added to
// the api type, which is the same input Validate sees when the struct really grows.
func TestMatrix_FieldAddedToTheStructWithNoExpectationIsRejected(t *testing.T) {
	c := liveContract()
	// Index 0 deliberately: the injected field has no counterpart in the struct, and check() must reject the
	// table before it ever reads a value — a valid index keeps that assertion honest instead of turning a
	// regression into a panic.
	c.fields = append(c.fields, field{Name: "SnapshotHandle", JSON: "snapshotHandle", Index: 0})

	err := c.validate()
	if err == nil {
		t.Fatal("a field with no expectation in any scenario was accepted; the matrix would silently ignore it")
	}
	for _, s := range Scenarios() {
		if !strings.Contains(err.Error(), string(s)) {
			t.Errorf("the rejection does not mention scenario %q, so it does not tell the author which cells to fill:\n%v", s, err)
		}
	}
	if !strings.Contains(err.Error(), "SnapshotHandle") {
		t.Errorf("the rejection does not name the new field:\n%v", err)
	}

	// Fails closed: nothing can be judged against an incomplete table.
	if _, cerr := c.check(NativeImport, completeBinding()); cerr == nil {
		t.Fatal("check() returned a clean result against a matrix missing an expectation; a green run would mean nothing")
	}
}

// TestMatrix_NarrowedReflectionIsRejected pins the difference between "no violations" and "nothing
// inspected": a field set narrowed to empty (the classic way a reflective check goes quietly green) must be
// a failure, not a clean sheet.
func TestMatrix_NarrowedReflectionIsRejected(t *testing.T) {
	c := liveContract()
	c.fields = nil

	if err := c.validate(); err == nil {
		t.Fatal("an empty reflected field set was accepted; the matrix would inspect nothing and report success")
	}
	if _, err := c.check(DomainCapture, completeBinding()); err == nil {
		t.Fatal("check() succeeded with no fields to inspect")
	}
}

// TestMatrix_StaleExpectationForARemovedFieldIsRejected is the other direction of the same guard: it is
// what catches a reflection narrowed to a SUBSET (drop the struct fields, keep the string ones) as well as
// a rule left behind by a deleted field.
func TestMatrix_StaleExpectationForARemovedFieldIsRejected(t *testing.T) {
	c := liveContract()
	kept := make([]field, 0, len(c.fields))
	for _, f := range c.fields {
		if f.Name == "FsType" {
			continue
		}
		kept = append(kept, f)
	}
	c.fields = kept

	err := c.validate()
	if err == nil {
		t.Fatal("an expectation for a field the struct no longer has was accepted")
	}
	if !strings.Contains(err.Error(), "FsType") {
		t.Errorf("the rejection does not name the stale field:\n%v", err)
	}
}

// TestMatrix_MergingTheTwoImportCellsIsRejected pins the axis. Collapsing native import into one shared
// "import" cell is exactly the incompleteness that let volumeMode stay empty on every imported native-CSI
// leg, so the contract must refuse a table whose four routing combinations do not land on four cells.
func TestMatrix_MergingTheTwoImportCellsIsRejected(t *testing.T) {
	merged := liveContract()
	// One "import" cell for both import paths, exactly as the pre-fix mental model had it. The rule table is
	// cloned first: liveContract hands out the package-level matrix itself, and mutating it would leak into
	// every other test in this package.
	merged.axis = []Scenario{NativeCapture, DomainCapture, DomainImport}
	merged.classify = func(nativeCSI, importMode bool) Scenario {
		if importMode {
			return DomainImport
		}
		if nativeCSI {
			return NativeCapture
		}
		return DomainCapture
	}
	merged.rules = cloneRules(merged.rules)
	delete(merged.rules, NativeImport)

	err := merged.validate()
	if err == nil {
		t.Fatal("an axis that merges the two import paths into one cell was accepted; the native-CSI import leg would go unchecked")
	}
	if !strings.Contains(err.Error(), "four distinct cells") {
		t.Errorf("the rejection does not explain that the four data-leg paths must stay four cells:\n%v", err)
	}
}

// TestMatrix_MissingReasonForATolerantCellIsRejected: "legitimately empty" without a reason is how a lost
// field gets accepted forever.
func TestMatrix_MissingReasonForATolerantCellIsRejected(t *testing.T) {
	for _, expect := range []Expectation{MayBeEmpty, MustBeEmpty, MustBeSetUnlessBlock} {
		c := liveContract()
		c.rules = cloneRules(c.rules)
		c.rules[DomainImport]["FsType"] = Rule{Expect: expect, Source: "DataImport.status.data.fsType"}

		err := c.validate()
		if err == nil {
			t.Fatalf("%s with no Reason was accepted", expect)
		}
		if !strings.Contains(err.Error(), "needs a Reason") {
			t.Errorf("%s: the rejection does not ask for a reason:\n%v", expect, err)
		}
	}
}

// TestMatrix_MissingSourceIsRejected: the Source column is what forces an author to go looking for the
// value on each of the four paths — which is where the missing producer shows up.
func TestMatrix_MissingSourceIsRejected(t *testing.T) {
	c := liveContract()
	c.rules = cloneRules(c.rules)
	c.rules[NativeImport]["VolumeMode"] = Rule{Expect: MustBeSet}

	err := c.validate()
	if err == nil {
		t.Fatal("a cell with no Source was accepted")
	}
	if !strings.Contains(err.Error(), "no Source") {
		t.Errorf("the rejection does not name the missing source:\n%v", err)
	}
}

// TestCheck_CleanOnASettledBindingInEveryScenario walks the whole axis against a binding that carried
// everything, and prints how many cells each pass decided.
func TestCheck_CleanOnASettledBindingInEveryScenario(t *testing.T) {
	total := 0
	for _, s := range Scenarios() {
		for _, tc := range []struct {
			name    string
			binding *storagev1alpha1.SnapshotDataBinding
		}{
			{"filesystem", completeBinding()},
			{"block", blockBinding()},
		} {
			res, err := Check(s, tc.binding)
			if err != nil {
				t.Fatalf("[%s/%s] check: %v", s, tc.name, err)
			}
			if !res.OK() {
				t.Errorf("[%s/%s] settled binding reported as incomplete:\n%s", s, tc.name, res.Report())
			}
			if len(res.Skipped) != 0 {
				t.Errorf("[%s/%s] nothing should be undecidable on a settled binding:\n%s", s, tc.name, res.Report())
			}
			if res.Checked != len(FieldNames()) {
				t.Errorf("[%s/%s] decided %d cell(s), want all %d", s, tc.name, res.Checked, len(FieldNames()))
			}
			total += res.Checked
		}
	}
	if total == 0 {
		t.Fatal("nothing was inspected")
	}
	t.Logf("inspected %d cell(s) across %d scenario(s) x 2 volume mode(s)", total, len(Scenarios()))
}

// TestCheck_RedInBothDirections: an empty required field AND a populated not-applicable one. The second
// direction is the one that catches a value arriving from the wrong volume.
func TestCheck_RedInBothDirections(t *testing.T) {
	t.Run("empty required field", func(t *testing.T) {
		for _, s := range Scenarios() {
			b := completeBinding()
			b.VolumeMode = ""
			b.FsType = "" // keep the fsType cell undecidable rather than adding a second violation
			res, err := Check(s, b)
			if err != nil {
				t.Fatalf("[%s] check: %v", s, err)
			}
			if res.OK() {
				t.Fatalf("[%s] an empty volumeMode was accepted; storage-foundation fails export closed on it\n%s", s, res.Report())
			}
			if !strings.Contains(res.Report(), "volumeMode") {
				t.Errorf("[%s] the report does not name the empty field:\n%s", s, res.Report())
			}
		}
	})

	t.Run("populated not-applicable field", func(t *testing.T) {
		for _, s := range Scenarios() {
			b := blockBinding()
			b.FsType = "ext4" // a Block volume has no filesystem: this can only have come from another volume
			res, err := Check(s, b)
			if err != nil {
				t.Fatalf("[%s] check: %v", s, err)
			}
			if res.OK() {
				t.Fatalf("[%s] an fsType on a raw Block binding was accepted\n%s", s, res.Report())
			}
			if !strings.Contains(res.Report(), string(MustBeEmpty)) {
				t.Errorf("[%s] the report does not say the field does not apply:\n%s", s, res.Report())
			}
		}
	})
}

// TestCheck_UndecidableCellIsSkippedVisibly: a skip is reported and counted, never passed over quietly —
// and it does not count as a decided cell.
func TestCheck_UndecidableCellIsSkippedVisibly(t *testing.T) {
	b := completeBinding()
	b.VolumeMode = "" // fsType's expectation resolves against it, so that cell cannot be decided

	res, err := Check(NativeCapture, b)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].JSON != "fsType" {
		t.Fatalf("want exactly one skipped cell (fsType), got:\n%s", res.Report())
	}
	if res.Skipped[0].Reason == "" {
		t.Error("a skip without a reason is a silent pass")
	}
	if res.Checked != len(FieldNames())-1 {
		t.Errorf("decided %d cell(s), want %d (all but the skipped one)", res.Checked, len(FieldNames())-1)
	}
	if !strings.Contains(res.Report(), "skipped") {
		t.Errorf("the skip is not visible in the report:\n%s", res.Report())
	}
}

// TestCheck_MayBeEmptyToleratesEitherValue exercises the MayBeEmpty branch, which no live cell uses today:
// after this release five of the six fields are required on every path, and the sixth (fsType) is required on
// every path whose volume is not Block. The mechanism is covered so that the first cell to need it is not the
// first time it runs.
func TestCheck_MayBeEmptyToleratesEitherValue(t *testing.T) {
	c := liveContract()
	c.rules = cloneRules(c.rules)
	c.rules[DomainCapture]["StorageClassName"] = Rule{
		Expect: MayBeEmpty,
		Source: "the live source PVC.spec.storageClassName",
		Reason: "a statically provisioned source PVC has no StorageClass at all",
	}

	for _, class := range []string{"", "sc-a"} {
		b := completeBinding()
		b.StorageClassName = class
		res, err := c.check(DomainCapture, b)
		if err != nil {
			t.Fatalf("storageClassName=%q: %v", class, err)
		}
		if !res.OK() {
			t.Errorf("storageClassName=%q rejected by a MayBeEmpty cell:\n%s", class, res.Report())
		}
		if res.Checked != len(FieldNames()) {
			t.Errorf("storageClassName=%q: a tolerated cell must still count as inspected (%d of %d)", class, res.Checked, len(FieldNames()))
		}
	}
}

// TestCheck_AbsentStatusDataIsAnError: nothing published is not "nothing to check".
func TestCheck_AbsentStatusDataIsAnError(t *testing.T) {
	if _, err := Check(DomainImport, nil); err == nil {
		t.Fatal("a nil binding (status.data absent) was accepted as complete")
	}
	if _, err := Check(Scenario("import"), completeBinding()); err == nil {
		t.Fatal("an unknown scenario was accepted")
	}
}

// TestClassify_IsTheFullCrossProductOfTheRoutingDiscriminators keeps the axis and the router's two
// structural discriminators in step: four combinations, four cells, and the native-CSI import must NOT
// share a cell with the domain import (they are different code paths).
func TestClassify_IsTheFullCrossProductOfTheRoutingDiscriminators(t *testing.T) {
	want := map[Scenario][2]bool{
		NativeCapture: {true, false},
		NativeImport:  {true, true},
		DomainCapture: {false, false},
		DomainImport:  {false, true},
	}
	if len(want) != len(discriminatorCombinations()) {
		t.Fatalf("the expectation table has %d entries for %d discriminator combinations", len(want), len(discriminatorCombinations()))
	}
	seen := map[Scenario]bool{}
	for scenario, combo := range want {
		got := Classify(combo[0], combo[1])
		if got != scenario {
			t.Errorf("Classify(nativeCSI=%v, importMode=%v) = %q, want %q", combo[0], combo[1], got, scenario)
		}
		seen[got] = true
	}
	if len(seen) != len(discriminatorCombinations()) {
		t.Fatalf("the four routing combinations produced %d distinct scenario(s): %v", len(seen), seen)
	}
	if Classify(true, true) == Classify(false, true) {
		t.Fatal("native import and domain import share a cell; they are different code paths and merging them is what hid an empty volumeMode")
	}
	t.Logf("classified %d discriminator combination(s) into %d cell(s)", len(discriminatorCombinations()), len(seen))
}

// cloneRules copies the table two levels deep so a test can mutate one cell without touching the package
// level matrix (tests in one package share it).
func cloneRules(in map[Scenario]map[string]Rule) map[Scenario]map[string]Rule {
	out := make(map[Scenario]map[string]Rule, len(in))
	for s, rules := range in {
		cp := make(map[string]Rule, len(rules))
		for name, rule := range rules {
			cp[name] = rule
		}
		out[s] = cp
	}
	return out
}
