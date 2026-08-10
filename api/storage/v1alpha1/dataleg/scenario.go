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

// Package dataleg holds the scenario taxonomy of the SnapshotContent data leg and the completeness
// contract of SnapshotContent.status.data: for every field of SnapshotDataBinding, and for every
// scenario that publishes one, what a settled binding must carry.
//
// It lives in the api module because that is the only module both the controller
// (images/state-snapshotter-controller) and the e2e suite (e2e/) import, and the contract has to be ONE
// table — a second copy in either place is a table that silently stops matching the first. The package
// depends on nothing but the standard library and the api types.
//
// Why the contract needs a machine at all: every field of status.data except sourceRef/artifactRef is
// +optional and status.data carries no x-kubernetes-validations, so the apiserver cannot tell "legitimately
// empty" from "the controller lost it". Three fields were found empty that way, each by accident
// (storageClassName, fsType, volumeMode).
package dataleg

// Scenario is one cell of the scenario axis: one of the code paths that publish
// SnapshotContent.status.data. There are FOUR of them, not three, and the fourth is the whole reason this
// axis is written down: an IMPORTED native-CSI leaf does NOT take the DataImport branch. Its owner is a
// kind VolumeSnapshot, so the router hands it to the same bound-VSC projection that serves capture
// ("covers BOTH capture VS and import VS" — reconcileDataLegProjection). Collapsing the two import cells
// into one "import" cell is exactly the incompleteness that let volumeMode stay empty on every imported
// native-CSI leg: the mental model had one import path, the DataImport one, which does populate it.
type Scenario string

const (
	// NativeCapture: a capture VolumeSnapshot IS the volume capture; the projection builds the binding
	// from owner.status.{sourceRef,boundVolumeSnapshotContentName} and the enricher reads the volume
	// metadata off the live source PVC (and its bound PV).
	NativeCapture Scenario = "native capture"

	// NativeImport: an imported VolumeSnapshot, same bound-VSC projection as NativeCapture. There is no
	// live source PVC — status.sourceRef is rebuilt from the checkpoint manifest — so the volume metadata
	// comes from the DataImport that staged the bytes and is stamped after enrichment.
	NativeImport Scenario = "native import"

	// DomainCapture: a domain owner (demo disk, VM disk, ...) whose data leg is a VolumeCaptureRequest;
	// the binding comes from the VCR's dataRefs and the enricher reads the live source PVC.
	DomainCapture Scenario = "domain capture"

	// DomainImport: a generic imported leaf of a domain kind. No live VCR and no bound VSC on the owner:
	// the artifact and the volume metadata come from the reverse-looked-up DataImport.
	DomainImport Scenario = "domain import"
)

// Scenarios returns the whole axis in table order.
//
// It is the full cross product of the two discriminators the router uses — 2 x 2 = 4 — which is what
// makes the axis impossible to shrink quietly: Classify maps every combination onto a cell, and Validate
// requires the images of those four combinations to be four DISTINCT scenarios equal to this list. Merging
// two cells therefore fails the contract instead of just producing a smaller table.
func Scenarios() []Scenario {
	return []Scenario{NativeCapture, NativeImport, DomainCapture, DomainImport}
}

// Classify maps the two discriminators the data-leg router uses onto the cell that will handle the owner.
//
// nativeCSI is "the owning snapshot object is a CSI VolumeSnapshot" (kind VolumeSnapshot); importMode is
// "the owner declares spec.mode: Import". Both are STRUCTURAL by construction — a kind and a spec
// discriminator — never inferred from the emptiness of a value, which is the mistake that made an unset
// field indistinguishable from an import.
//
// This is the single definition of the routing rule. reconcileDataLegProjection switches on it (so the
// production route and this taxonomy cannot drift apart), and the e2e completeness assertion derives the
// same two booleans from the cluster objects. What each consumer does on its own is only EXTRACTING the
// two booleans from its own representation of the owner (unstructured owner vs. live cluster object); the
// mapping from booleans to cell is here and nowhere else.
func Classify(nativeCSI, importMode bool) Scenario {
	if nativeCSI {
		if importMode {
			return NativeImport
		}
		return NativeCapture
	}
	if importMode {
		return DomainImport
	}
	return DomainCapture
}

// discriminatorCombinations returns every (nativeCSI, importMode) pair, i.e. the domain of Classify. Used
// by Validate to prove the axis is the full cross product and that no two combinations share a cell.
func discriminatorCombinations() [][2]bool {
	return [][2]bool{{false, false}, {false, true}, {true, false}, {true, true}}
}
