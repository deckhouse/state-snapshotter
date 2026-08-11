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
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// Expectation is what a settled status.data must show for one field in one scenario.
//
// "Settled" means the data leg has published and latched: the artifact is bound and the projection has run
// with everything it reads available. The matrix deliberately does NOT describe a pass in flight, where
// every field may still be absent for a moment.
type Expectation string

const (
	// MustBeSet — required: the path MUST carry this value onto status.data.
	//
	// Read it as a PROPAGATION contract, not as a claim that every volume in the world has the value: the
	// judged binding is one whose source carried it (the unit fixtures construct exactly that, the e2e
	// suite provisions it). What it forbids is a path that DROPS a value its own source did attest —
	// which is what happened to fsType and volumeMode on both import cells and to storageClassName
	// before them. A cell whose source genuinely cannot produce the value is MayBeEmpty or MustBeEmpty
	// instead, with the reason written down.
	MustBeSet Expectation = "MustBeSet"

	// MayBeEmpty — legitimately empty: the value may be absent from a settled binding, and Reason says
	// why the absence is legitimate. It is the weakest expectation and it is never a shrug: an empty
	// value here must be indistinguishable-by-design from a lost one, otherwise use MustBeSet.
	MayBeEmpty Expectation = "MayBeEmpty"

	// MustBeEmpty — not applicable: the field does not apply to this cell and a NON-empty value is itself
	// a violation. This is the direction that catches a value arriving from the wrong volume: an fsType on
	// a raw block binding can only have come from a namesake stranger PVC or from a pre-fix revision.
	MustBeEmpty Expectation = "MustBeEmpty"

	// MustBeSetUnlessBlock — MustBeSet on a Filesystem volume, MustBeEmpty on a Block one. The single
	// mode-dependent expectation in the table (fsType: a raw block device carries no filesystem, and both
	// producers — the PV-reading enricher and storage-foundation on the import side — leave it empty
	// structurally, gated on the mode rather than on the value).
	//
	// LIMIT: this is a NAMED dependency, not a general predicate mechanism. It resolves against
	// volumeMode and nothing else, and it is undecidable while volumeMode itself is empty (reported as a
	// skip, not silently passed). A second conditional field would need a real predicate, not a third
	// enum value.
	MustBeSetUnlessBlock Expectation = "MustBeSetUnlessBlock"
)

// Rule is one cell of the matrix: the expectation for one field of SnapshotDataBinding in one scenario,
// plus where the value comes from on that path.
type Rule struct {
	// Expect is the expectation for a settled binding. Required.
	Expect Expectation

	// Source names the producer of the value on this path (which object and which field). Required for
	// EVERY cell, including the empty ones — where it says what would have produced a value, and that is
	// what forces an author adding a field to look for the value on all four paths instead of assuming a
	// single "import" path fills it. Filling this in per cell is what turns the table from a description
	// into a check.
	Source string

	// Reason justifies a non-MustBeSet expectation. Required for MayBeEmpty, MustBeEmpty and
	// MustBeSetUnlessBlock (which resolves to MustBeEmpty on Block), forbidden to be empty there by
	// Validate: an unexplained "legitimately empty" is how a lost field gets accepted forever.
	Reason string
}

// volumeModeField is the field MustBeSetUnlessBlock resolves against, and blockVolumeMode is the value
// that flips it. The mode vocabulary itself is owned by the schema marker on
// SnapshotDataBinding.VolumeMode (its kubebuilder Enum marker, Block;Filesystem); the api module has no
// k8s.io/api dependency, so corev1.PersistentVolumeBlock cannot be referenced here. Validate fails if the
// field disappears, so a rename cannot silently turn the conditional into a no-op.
const (
	volumeModeField = "VolumeMode"
	blockVolumeMode = "Block"
)

// matrix is the «scenario x field» table: what a settled SnapshotContent.status.data must carry.
//
// Keyed by Go field name of SnapshotDataBinding, because that is the identity reflection walks; the JSON
// name is reported alongside it for humans. Every scenario must map EVERY reflected field — Validate
// enforces both directions, so a new field on the struct has no rule and fails the contract until its
// author declares one per cell, and a rule left behind by a removed field fails too.
var matrix = map[Scenario]map[string]Rule{
	NativeCapture: {
		"SourceRef": {
			Expect: MustBeSet,
			Source: "owner.status.sourceRef — the captured PVC, published by the foundation domain reconciler at adoption",
		},
		"ArtifactRef": {
			Expect: MustBeSet,
			Source: "owner.status.boundVolumeSnapshotContentName (the VSC the fork bound); uid backfilled from the live VSC",
		},
		"VolumeMode": {
			Expect: MustBeSet,
			Source: "EnrichDataBindingsWithVolumeMetadata from the live source PVC.spec.volumeMode (nil means the Kubernetes default, Filesystem)",
		},
		"FsType": {
			Expect: MustBeSetUnlessBlock,
			Source: "EnrichDataBindingsWithVolumeMetadata from the bound PV.spec.csi.fsType, read only for a Filesystem volume",
			Reason: "a raw block volume carries no filesystem: the enricher skips the PV read entirely for volumeMode Block, so a value here could only have come from a different volume",
		},
		"StorageClassName": {
			Expect: MustBeSet,
			Source: "EnrichDataBindingsWithVolumeMetadata from the live source PVC.spec.storageClassName",
		},
		"Size": {
			Expect: MustBeSet,
			Source: "VolumeSnapshotContent.status.restoreSize of the durable artifact",
		},
	},

	NativeImport: {
		"SourceRef": {
			Expect: MustBeSet,
			Source: "owner.status.sourceRef, rebuilt by the import binder from the checkpoint manifest (the PVC it names does not exist in this cluster)",
		},
		"ArtifactRef": {
			Expect: MustBeSet,
			Source: "owner.status.boundVolumeSnapshotContentName, published by the import binder for the imported VSC",
		},
		"VolumeMode": {
			Expect: MustBeSet,
			Source: "DataImport.status.volumeMode (storage-foundation republishes the scratch PVC's mode), stamped onto the binding after enrichment",
		},
		"FsType": {
			Expect: MustBeSetUnlessBlock,
			Source: "DataImport.status.data.fsType — observed by storage-foundation on the scratch volume's PV while it still existed",
			Reason: "a raw block import has no filesystem, and storage-foundation publishes none for volumeMode Block",
		},
		"StorageClassName": {
			Expect: MustBeSet,
			Source: "DataImport.spec.storageParams.storageClassName — the class the bytes were staged into, stamped after enrichment",
		},
		"Size": {
			Expect: MustBeSet,
			Source: "VolumeSnapshotContent.status.restoreSize of the imported artifact",
		},
	},

	DomainCapture: {
		"SourceRef": {
			Expect: MustBeSet,
			Source: "the VolumeCaptureRequest's captured target (status.data dataRefs of the VCR)",
		},
		"ArtifactRef": {
			Expect: MustBeSet,
			Source: "the VolumeCaptureRequest's produced artifact (status.data.artifactRef of the VCR)",
		},
		"VolumeMode": {
			Expect: MustBeSet,
			Source: "EnrichDataBindingsWithVolumeMetadata from the live source PVC.spec.volumeMode",
		},
		"FsType": {
			Expect: MustBeSetUnlessBlock,
			Source: "EnrichDataBindingsWithVolumeMetadata from the bound PV.spec.csi.fsType, read only for a Filesystem volume",
			Reason: "a raw block volume carries no filesystem; the enricher skips the PV read for volumeMode Block",
		},
		"StorageClassName": {
			Expect: MustBeSet,
			Source: "EnrichDataBindingsWithVolumeMetadata from the live source PVC.spec.storageClassName",
		},
		"Size": {
			Expect: MustBeSet,
			Source: "VolumeSnapshotContent.status.restoreSize of the durable artifact",
		},
	},

	DomainImport: {
		"SourceRef": {
			Expect: MustBeSet,
			Source: "the imported leaf's own identity (apiVersion/kind/namespace/name/uid) — it has no source PVC in this cluster",
		},
		"ArtifactRef": {
			Expect: MustBeSet,
			Source: "DataImport.status.data.artifactRef of the reverse-looked-up DataImport",
		},
		"VolumeMode": {
			Expect: MustBeSet,
			Source: "DataImport.status.volumeMode, set on the binding by BuildImportDataBinding",
		},
		"FsType": {
			Expect: MustBeSetUnlessBlock,
			Source: "DataImport.status.data.fsType, set on the binding by BuildImportDataBinding",
			Reason: "a raw block import has no filesystem, and storage-foundation publishes none for volumeMode Block",
		},
		"StorageClassName": {
			Expect: MustBeSet,
			Source: "DataImport.spec.storageParams.storageClassName, stamped after enrichment (it lives in the spec, not the status, so BuildImportDataBinding does not carry it)",
		},
		"Size": {
			Expect: MustBeSet,
			Source: "VolumeSnapshotContent.status.restoreSize of the artifact the DataImport produced",
		},
	},
}

// field is one reflected field of SnapshotDataBinding.
type field struct {
	Name  string // Go field name, the matrix key
	JSON  string // JSON/wire name, for messages
	Index int
}

// fields walks SnapshotDataBinding by REFLECTION — never a hand-written list, which is the whole point:
// a field added to the struct appears here on its own and, having no rule, fails Validate until its author
// declares an expectation for each of the four cells.
//
// LIMITS, both deliberate:
//   - The walk is ONE level deep. sourceRef/artifactRef are judged as whole fields (populated == not the
//     zero struct); the required subfields inside them are enforced by the CRD schema (MinLength markers
//     on artifactRef.apiVersion/kind/name) and by the publish gates, not here.
//   - Nothing is filtered out — not unexported fields, not json:"-" ones. Filtering would be a silent
//     narrowing of the reflected set, and a narrowed set is exactly how a check goes green on an
//     unchecked field.
func fields() []field {
	t := reflect.TypeOf(storagev1alpha1.SnapshotDataBinding{})
	out := make([]field, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		out = append(out, field{Name: f.Name, JSON: jsonName(f), Index: i})
	}
	return out
}

// jsonName returns the wire name of a field: the json tag's name, or the Go name when there is no tag.
func jsonName(f reflect.StructField) string {
	tag, ok := f.Tag.Lookup("json")
	if !ok {
		return f.Name
	}
	name := strings.Split(tag, ",")[0]
	if name == "" {
		return f.Name
	}
	return name
}

// FieldNames returns the Go field names of SnapshotDataBinding in declaration order, as the matrix sees
// them. Exported so consumers can print what was covered.
func FieldNames() []string {
	fs := fields()
	names := make([]string, 0, len(fs))
	for _, f := range fs {
		names = append(names, f.Name)
	}
	return names
}

// Cells returns the size of the matrix — scenarios x fields — i.e. the number of expectations declared.
// Printed by the callers so that "no violations" cannot be confused with "nothing was inspected".
func Cells() int {
	return len(Scenarios()) * len(fields())
}

// Violation is one broken expectation.
type Violation struct {
	Scenario Scenario
	Field    string // Go field name
	JSON     string // wire name
	Expect   Expectation
	Detail   string
}

func (v Violation) String() string {
	return fmt.Sprintf("%s: status.data.%s (%s): %s", v.Scenario, v.JSON, v.Expect, v.Detail)
}

// Skip is a cell the matrix could not decide. It is reported and counted rather than passed over: a
// pass-by-default skip is a green result with nothing behind it.
type Skip struct {
	Scenario Scenario
	Field    string
	JSON     string
	Reason   string
}

func (s Skip) String() string {
	return fmt.Sprintf("%s: status.data.%s: skipped: %s", s.Scenario, s.JSON, s.Reason)
}

// Result is the outcome of checking one binding against one scenario's column.
type Result struct {
	Scenario   Scenario
	Checked    int // cells decided (violated or satisfied)
	Violations []Violation
	Skipped    []Skip
}

// OK reports whether the binding satisfied every decided expectation.
func (r Result) OK() bool { return len(r.Violations) == 0 }

// Report renders the result including the number of cells inspected, so a caller's log distinguishes
// "clean" from "vacuous".
func (r Result) Report() string {
	var b strings.Builder
	fmt.Fprintf(&b, "status.data completeness [%s]: %d field(s) checked, %d skipped, %d violation(s)",
		r.Scenario, r.Checked, len(r.Skipped), len(r.Violations))
	for _, s := range r.Skipped {
		fmt.Fprintf(&b, "\n  ~ %s", s)
	}
	for _, v := range r.Violations {
		fmt.Fprintf(&b, "\n  ! %s", v)
	}
	return b.String()
}

// contract is the matrix together with the field set and the axis it is indexed by. The live one is the
// only one production uses; the tests build MUTATED contracts (a cell removed, the field set narrowed to
// nothing, the two import cells merged) to prove each guard actually bites instead of merely existing.
type contract struct {
	fields   []field
	axis     []Scenario
	classify func(nativeCSI, importMode bool) Scenario
	rules    map[Scenario]map[string]Rule
}

// liveContract is the real thing: fields by reflection, the four-cell axis, the router's classifier and
// the declared matrix.
func liveContract() contract {
	return contract{fields: fields(), axis: Scenarios(), classify: Classify, rules: matrix}
}

// Check judges one published binding against one scenario's column of the matrix.
//
// It fails CLOSED on a broken matrix: Validate runs first, so an incomplete or unexplained table can never
// produce a clean Result. That is what keeps "0 violations" from meaning "0 expectations".
func Check(scenario Scenario, binding *storagev1alpha1.SnapshotDataBinding) (Result, error) {
	return liveContract().check(scenario, binding)
}

func (c contract) check(scenario Scenario, binding *storagev1alpha1.SnapshotDataBinding) (Result, error) {
	if err := c.validate(); err != nil {
		return Result{}, fmt.Errorf("status.data completeness matrix is not valid, refusing to judge anything against it: %w", err)
	}
	if binding == nil {
		return Result{}, fmt.Errorf("status.data completeness [%s]: no binding published (status.data is absent)", scenario)
	}
	rules, ok := c.rules[scenario]
	if !ok {
		return Result{}, fmt.Errorf("status.data completeness: unknown scenario %q (axis: %v)", scenario, c.axis)
	}

	res := Result{Scenario: scenario}
	v := reflect.ValueOf(*binding)
	for _, f := range c.fields {
		rule := rules[f.Name] // validate guarantees presence
		expect, reason, decidable := resolveExpectation(rule, binding)
		if !decidable {
			res.Skipped = append(res.Skipped, Skip{Scenario: scenario, Field: f.Name, JSON: f.JSON, Reason: reason})
			continue
		}
		res.Checked++
		empty := v.Field(f.Index).IsZero()
		switch expect {
		case MustBeSet:
			if empty {
				res.Violations = append(res.Violations, Violation{
					Scenario: scenario, Field: f.Name, JSON: f.JSON, Expect: expect,
					Detail: fmt.Sprintf("empty, but this path must carry it — expected value from %s", rule.Source),
				})
			}
		case MustBeEmpty:
			if !empty {
				res.Violations = append(res.Violations, Violation{
					Scenario: scenario, Field: f.Name, JSON: f.JSON, Expect: expect,
					Detail: fmt.Sprintf("set to %q, but the field does not apply here: %s", fmt.Sprint(v.Field(f.Index).Interface()), reason),
				})
			}
		case MayBeEmpty:
			// Either way is legitimate; the cell still counts as inspected.
		}
	}
	return res, nil
}

// resolveExpectation reduces a rule to a decidable expectation for this binding. MustBeSetUnlessBlock is
// the only rule that depends on the binding: it becomes MustBeEmpty on a Block volume, MustBeSet on any
// other mode, and undecidable while volumeMode is empty — that emptiness is already a violation of the
// volumeMode cell, and inventing an fsType verdict on top of it would report one defect twice while
// pretending to have checked something.
func resolveExpectation(rule Rule, binding *storagev1alpha1.SnapshotDataBinding) (expect Expectation, reason string, decidable bool) {
	if rule.Expect != MustBeSetUnlessBlock {
		return rule.Expect, rule.Reason, true
	}
	switch binding.VolumeMode {
	case "":
		return "", "volumeMode is empty, so it cannot be decided whether a filesystem applies (see the volumeMode cell)", false
	case blockVolumeMode:
		return MustBeEmpty, rule.Reason, true
	default:
		return MustBeSet, rule.Reason, true
	}
}

// Validate checks the matrix against the struct it describes, and the axis against the router's
// discriminators. It is the guard that keeps the table honest, so Check refuses to run without it.
//
// It reports EVERY problem it finds (joined), not the first: a table with three missing cells should not
// take three runs to fix.
func Validate() error { return liveContract().validate() }

func (c contract) validate() error {
	var problems []error

	fs := c.fields
	if len(fs) == 0 {
		// Reflection yielding nothing would make every Check trivially clean: 0 cells inspected, 0
		// violations. Fail instead.
		problems = append(problems, errors.New("reflection over SnapshotDataBinding yielded no fields; the matrix would inspect nothing"))
	}

	axis := c.axis
	seen := map[Scenario]bool{}
	for _, s := range axis {
		if seen[s] {
			problems = append(problems, fmt.Errorf("scenario axis lists %q twice", s))
		}
		seen[s] = true
	}

	// The axis must be exactly the image of the router's two discriminators, and the four combinations
	// must map to four DISTINCT cells. This is what makes merging two cells (notably the two import ones)
	// a failure rather than a quietly smaller table.
	byCell := map[Scenario][][2]bool{}
	for _, combo := range discriminatorCombinations() {
		cell := c.classify(combo[0], combo[1])
		byCell[cell] = append(byCell[cell], combo)
		if !seen[cell] {
			problems = append(problems, fmt.Errorf("Classify(nativeCSI=%v, importMode=%v) returns %q, which is not on the scenario axis", combo[0], combo[1], cell))
		}
	}
	for cell, combos := range byCell {
		if len(combos) > 1 {
			problems = append(problems, fmt.Errorf("scenario %q is shared by %d discriminator combinations %v; the four data-leg paths must stay four distinct cells", cell, len(combos), combos))
		}
	}
	for _, s := range axis {
		if len(byCell[s]) == 0 {
			problems = append(problems, fmt.Errorf("scenario %q is on the axis but no discriminator combination routes to it", s))
		}
	}

	// Table shape: exactly the axis as columns, exactly the reflected fields as rows.
	for _, s := range axis {
		if _, ok := c.rules[s]; !ok {
			problems = append(problems, fmt.Errorf("scenario %q has no column in the matrix", s))
		}
	}
	for s := range c.rules {
		if !seen[s] {
			problems = append(problems, fmt.Errorf("matrix has a column for %q, which is not on the scenario axis", s))
		}
	}

	known := map[string]bool{}
	for _, f := range fs {
		known[f.Name] = true
	}
	usesModeConditional := false
	for _, s := range axis {
		rules, ok := c.rules[s]
		if !ok {
			continue
		}
		for _, f := range fs {
			rule, ok := rules[f.Name]
			if !ok {
				problems = append(problems, fmt.Errorf("scenario %q has no expectation for SnapshotDataBinding.%s (status.data.%s): declare whether this path must carry it, may leave it empty, or cannot produce it at all", s, f.Name, f.JSON))
				continue
			}
			if rule.Expect == MustBeSetUnlessBlock {
				usesModeConditional = true
			}
			problems = append(problems, ruleProblems(s, f, rule)...)
		}
		for name := range rules {
			if !known[name] {
				problems = append(problems, fmt.Errorf("scenario %q declares an expectation for %q, which SnapshotDataBinding no longer has", s, name))
			}
		}
	}
	if usesModeConditional && !known[volumeModeField] {
		problems = append(problems, fmt.Errorf("a rule resolves against SnapshotDataBinding.%s, which no longer exists; the conditional would silently stop deciding", volumeModeField))
	}

	// Deterministic order so a failure reads the same twice.
	msgs := make([]string, 0, len(problems))
	for _, p := range problems {
		msgs = append(msgs, p.Error())
	}
	sort.Strings(msgs)
	sorted := make([]error, 0, len(msgs))
	for _, m := range msgs {
		sorted = append(sorted, errors.New(m))
	}
	return errors.Join(sorted...)
}

// ruleProblems validates one cell: a known expectation, a named source, and a reason wherever the
// expectation tolerates or demands emptiness.
func ruleProblems(s Scenario, f field, rule Rule) []error {
	var problems []error
	switch rule.Expect {
	case MustBeSet, MayBeEmpty, MustBeEmpty, MustBeSetUnlessBlock:
	default:
		problems = append(problems, fmt.Errorf("scenario %q, field %s: unknown expectation %q", s, f.Name, rule.Expect))
	}
	if strings.TrimSpace(rule.Source) == "" {
		problems = append(problems, fmt.Errorf("scenario %q, field %s: no Source — name the object and field this path takes the value from (or would take it from)", s, f.Name))
	}
	if rule.Expect != MustBeSet && strings.TrimSpace(rule.Reason) == "" {
		problems = append(problems, fmt.Errorf("scenario %q, field %s: expectation %s needs a Reason explaining why an empty value is legitimate here", s, f.Name, rule.Expect))
	}
	return problems
}

// Table renders the whole matrix, counts included, for a test log or a human reading the contract.
func Table() string {
	fs := fields()
	var b strings.Builder
	fmt.Fprintf(&b, "SnapshotContent.status.data completeness matrix: %d scenario(s) x %d field(s) = %d cell(s)",
		len(Scenarios()), len(fs), Cells())
	for _, s := range Scenarios() {
		fmt.Fprintf(&b, "\n  [%s]", s)
		for _, f := range fs {
			rule, ok := matrix[s][f.Name]
			if !ok {
				fmt.Fprintf(&b, "\n    %-18s <no expectation declared>", f.JSON)
				continue
			}
			fmt.Fprintf(&b, "\n    %-18s %-21s from %s", f.JSON, rule.Expect, rule.Source)
			if rule.Reason != "" {
				fmt.Fprintf(&b, "\n    %-18s %-21s because %s", "", "", rule.Reason)
			}
		}
	}
	return b.String()
}
