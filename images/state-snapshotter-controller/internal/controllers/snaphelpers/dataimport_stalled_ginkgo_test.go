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

package snaphelpers

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// The single Ginkgo entry point of this package. The table-driven tests around it use plain testing.
func TestSnaphelpersGinkgo(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "snaphelpers Suite")
}

// withImportPhase sets status.phase on a DataImport fixture. Kept separate from withImportStatus (which also
// publishes an artifactRef) because the cases below turn exactly on whether an artifact exists.
//
// The write error is discarded, as in the neighbouring fixtures: these helpers run while the table Entries
// are built (before any fail handler exists), and the only error a static map can produce is a type conflict
// on a path this fixture owns outright.
func withImportPhase(di *unstructured.Unstructured, phase string) *unstructured.Unstructured {
	_ = unstructured.SetNestedField(di.Object, phase, "status", "phase")
	return di
}

// withImportArtifact publishes the produced VolumeSnapshotContent the way storage-foundation does.
func withImportArtifact(di *unstructured.Unstructured, name string) *unstructured.Unstructured {
	_ = unstructured.SetNestedMap(di.Object, map[string]interface{}{
		"apiVersion": snapshot.CSISnapshotAPIVersion,
		"kind":       snapshot.KindVolumeSnapshotContent,
		"name":       name,
	}, "status", "data", "artifactRef")
	return di
}

// ImportStalledMessage turns a DataImport that will never deliver into the cause an operator reads on the
// snapshot side. Two properties are asserted, and both are the point of the function rather than decoration:
//
//   - the text is compared against the WHOLE expected literal, not against a second call of the function.
//     Comparing two calls would pass even if the message carried a timestamp or a counter (two calls land in
//     the same second, so the bytes match) — and such a message would rewrite the status of a settled object
//     on every reconcile pass. Only a fixed literal fails on that.
//   - a published artifact beats the phase. The import's job for the snapshot node is the artifact; once it
//     exists the phase the import happens to sit in says nothing about the node, so describing that node as
//     stuck would be wrong.
var _ = DescribeTable("ImportStalledMessage: the cause of an import that will not deliver",
	func(di *unstructured.Unstructured, want string) {
		Expect(ImportStalledMessage(di)).To(Equal(want))
	},
	Entry("an unresolved (or already reaped) DataImport yields no cause",
		nil, ""),
	Entry("Expired without an artifact names the import, its phase and the elapsed idle window",
		withImportPhase(realisticDataImport(snapshot.DataImportModePopulateData, scratchStorageParams("sc-import"), nil),
			snapshot.DataImportPhaseExpired),
		"DataImport team-a/di-1 is in phase Expired and produced no data artifact: "+
			"its spec.ttl idle window elapsed before the upload finished; "+
			"create a new DataImport for this node to retry"),
	Entry("Failed without an artifact names the import and its phase, and claims no idle window",
		withImportPhase(realisticDataImport(snapshot.DataImportModePopulateData, scratchStorageParams("sc-import"), nil),
			snapshot.DataImportPhaseFailed),
		"DataImport team-a/di-1 is in phase Failed and produced no data artifact; "+
			"create a new DataImport for this node to retry"),
	Entry("a published artifact wins over an Expired phase",
		withImportArtifact(withImportPhase(
			realisticDataImport(snapshot.DataImportModePopulateData, scratchStorageParams("sc-import"), nil),
			snapshot.DataImportPhaseExpired), "snapcontent-abc"),
		""),
	Entry("a published artifact wins over a Failed phase",
		withImportArtifact(withImportPhase(
			realisticDataImport(snapshot.DataImportModePopulateData, scratchStorageParams("sc-import"), nil),
			snapshot.DataImportPhaseFailed), "snapcontent-abc"),
		""),
	Entry("an import with no phase yet is merely converging, not stalled",
		realisticDataImport(snapshot.DataImportModePopulateData, scratchStorageParams("sc-import"), nil), ""),
	Entry("a Completed import is not stalled",
		withImportPhase(realisticDataImport(snapshot.DataImportModePopulateData, scratchStorageParams("sc-import"), nil),
			"Completed"),
		""),
	Entry("a CreatePVC import is not a snapshot node's data leg, so it yields no cause even when Expired",
		withImportPhase(realisticDataImport("CreatePVC", nil, nil), snapshot.DataImportPhaseExpired), ""),
)
