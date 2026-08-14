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

package snapshotcontent

// This file is the «scenario x field» completeness check applied to the REAL publishers: it drives all four
// data-leg paths to a settled status.data and judges what each one actually published against the matrix in
// api/storage/v1alpha1/dataleg. The matrix's own shape (a rule per reflected field per cell, reasons, the
// four-cell axis) is checked in that package; here it meets the controller.
//
// Why the whole axis in one test: each of the three defects this contract exists for (storageClassName, then
// fsType and volumeMode) occupied one or two cells of it, never the whole table, and each was found by
// accident. A per-path assertion of the fields somebody remembered is what was already in place while they
// were empty.

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/api/storage/v1alpha1/dataleg"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// maxDataLegPasses bounds the drive. A settled leg publishes on the first pass and latches on the second;
// the bound is generous, and exhausting it is a FAILURE rather than a retry budget — a leg that keeps
// requeueing is the wedge shape (an expectation that can never be satisfied because nothing publishes it).
const maxDataLegPasses = 5

// dataLegCell is one cell of the scenario axis wired to the fixture that reproduces it.
type dataLegCell struct {
	scenario dataleg.Scenario
	// nativeCSI/importMode are the two structural discriminators the router feeds to dataleg.Classify. They
	// are asserted to produce this cell, so a fixture and its column cannot drift apart.
	nativeCSI  bool
	importMode bool
	// wantSourceKind and wantClass are cell-DISTINGUISHING facts, not extra field assertions: the two
	// capture cells take the class off the live source PVC while the two import cells take it off the
	// DataImport, and an imported domain leaf binds its own identity as the source where a native one binds
	// the captured PVC. If the router ever sent one cell down another cell's branch — the exact mistake that
	// merging the two import paths would encode — the values would swap and these would fail.
	wantSourceKind string
	wantClass      string
	// build wires the fixture. volumeMode/fsType are what this cell's SOURCE attests (a live PVC and its PV
	// for capture, the DataImport for import), which is what MustBeSet is a contract about: the path must
	// carry through what its source had.
	build func(t *testing.T, volumeMode, fsType string) (*SnapshotContentController, client.Client, *unstructured.Unstructured)
}

func dataLegCells() []dataLegCell {
	return []dataLegCell{
		{
			scenario: dataleg.NativeCapture, nativeCSI: true, importMode: false,
			wantSourceKind: "PersistentVolumeClaim", wantClass: "sc-a",
			build: func(t *testing.T, volumeMode, _ string) (*SnapshotContentController, client.Client, *unstructured.Unstructured) {
				// The fixture counts DataImport lists; this test does not assert on that count (whether the
				// capture path skips the lookup STRUCTURALLY has its own spec), it only needs a live counter.
				var dataImportLists int32
				r, cl := projBoundVSCFixture(t, &dataImportLists,
					projContentTyped(), projVSCWithRestoreSize(), completenessSourcePVC(volumeMode), projSourcePV())
				return r, cl, projBoundVSCOwner(false)
			},
		},
		{
			// The cell the whole four-way split exists for: an imported native-CSI leaf shares the bound-VSC
			// projection with capture, so it has no live source PVC to enrich from and every volume field has
			// to come from the DataImport.
			scenario: dataleg.NativeImport, nativeCSI: true, importMode: true,
			wantSourceKind: "PersistentVolumeClaim", wantClass: importScratchStorageClass,
			build: func(t *testing.T, volumeMode, fsType string) (*SnapshotContentController, client.Client, *unstructured.Unstructured) {
				var dataImportLists int32
				r, cl := projBoundVSCFixture(t, &dataImportLists,
					projContentTyped(), projVSCWithRestoreSize(), projDataImportForVSWithVolumeData(volumeMode, fsType))
				return r, cl, projBoundVSCOwner(true)
			},
		},
		{
			scenario: dataleg.DomainCapture, nativeCSI: false, importMode: false,
			wantSourceKind: "PersistentVolumeClaim", wantClass: "sc-a",
			build: func(t *testing.T, volumeMode, _ string) (*SnapshotContentController, client.Client, *unstructured.Unstructured) {
				cl := fake.NewClientBuilder().
					WithScheme(projScheme(t)).
					WithStatusSubresource(&storagev1alpha1.SnapshotContent{}).
					WithObjects(projContentTyped(), projReadyVCR(), projVSCWithRestoreSize(),
						completenessSourcePVC(volumeMode), projSourcePV()).
					Build()
				r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}
				return r, cl, completenessDomainCaptureOwner()
			},
		},
		{
			scenario: dataleg.DomainImport, nativeCSI: false, importMode: true,
			wantSourceKind: importLeafKind, wantClass: importScratchStorageClass,
			build: func(t *testing.T, volumeMode, fsType string) (*SnapshotContentController, client.Client, *unstructured.Unstructured) {
				r, cl := completenessDomainImportFixture(t, volumeMode, fsType)
				return r, cl, importOwnerLeaf()
			},
		},
	}
}

// TestDataLegProjection_PublishesCompleteStatusDataInEveryScenario drives every cell of the axis in both
// volume modes and judges the published binding against the matrix. Both directions are exercised by the
// modes: a Filesystem volume must carry an fsType, a Block one must NOT (there is no filesystem on a raw
// device, so a value could only have come from another volume).
func TestDataLegProjection_PublishesCompleteStatusDataInEveryScenario(t *testing.T) {
	if err := dataleg.Validate(); err != nil {
		t.Fatalf("the status.data completeness matrix is not valid, so nothing below would mean anything:\n%v", err)
	}
	cells := dataLegCells()
	if len(cells) != len(dataleg.Scenarios()) {
		t.Fatalf("this test drives %d cell(s) but the axis has %d; every scenario must be driven or the matrix is describing a path nobody runs",
			len(cells), len(dataleg.Scenarios()))
	}

	modes := []struct {
		name       string
		volumeMode string
		fsType     string
	}{
		// fsType is what the cell's source attests: the import fixtures publish it verbatim, the capture
		// fixtures read their own PV (projTestPVFsType) — either way the matrix only asks whether the path
		// carried a filesystem through, not which one.
		{"filesystem", string(corev1.PersistentVolumeFilesystem), importObservedFsType},
		{"block", "Block", ""},
	}

	inspected, drives := 0, 0
	for _, cell := range cells {
		for _, mode := range modes {
			t.Run(string(cell.scenario)+"/"+mode.name, func(t *testing.T) {
				if got := dataleg.Classify(cell.nativeCSI, cell.importMode); got != cell.scenario {
					t.Fatalf("the fixture claims cell %q, but the router's discriminators (nativeCSI=%v, importMode=%v) classify it as %q",
						cell.scenario, cell.nativeCSI, cell.importMode, got)
				}

				r, cl, owner := cell.build(t, mode.volumeMode, mode.fsType)
				passes := driveDataLegToSettled(t, r, owner)

				binding := projContentData(t, cl)
				if binding.SourceRef.Kind != cell.wantSourceKind {
					t.Errorf("published sourceRef.kind = %q, want %q: this cell was published by another cell's branch",
						binding.SourceRef.Kind, cell.wantSourceKind)
				}
				if binding.StorageClassName != cell.wantClass {
					t.Errorf("published storageClassName = %q, want %q: the value came from the wrong producer for this cell",
						binding.StorageClassName, cell.wantClass)
				}

				res, err := dataleg.Check(cell.scenario, &binding)
				if err != nil {
					t.Fatalf("check published status.data: %v", err)
				}
				if !res.OK() {
					t.Fatalf("the %s path published an incomplete status.data after %d pass(es):\n%s\npublished: %#v",
						cell.scenario, passes, res.Report(), binding)
				}
				if len(res.Skipped) != 0 {
					t.Fatalf("a settled binding left cells undecidable, which means a field it depends on is missing:\n%s", res.Report())
				}
				if res.Checked != len(dataleg.FieldNames()) {
					t.Fatalf("decided %d cell(s), want all %d fields of SnapshotDataBinding", res.Checked, len(dataleg.FieldNames()))
				}
				t.Logf("settled in %d pass(es); %s", passes, res.Report())
				inspected += res.Checked
				drives++
			})
		}
	}

	if drives == 0 || inspected == 0 {
		t.Fatal("no data leg was driven and nothing was inspected; a green result here would mean nothing")
	}
	t.Logf("drove %d data leg(s) (%d scenario(s) x %d volume mode(s)) and inspected %d field expectation(s) of the %d declared",
		drives, len(cells), len(modes), inspected, dataleg.Cells())
}

// driveDataLegToSettled runs the projection until it stops asking for a requeue, i.e. until the leg has
// published and latched. A terminal reason or an exhausted bound fails the test: neither is a settled leg.
func driveDataLegToSettled(t *testing.T, r *SnapshotContentController, owner *unstructured.Unstructured) int {
	t.Helper()
	ctx := context.Background()
	for pass := 1; pass <= maxDataLegPasses; pass++ {
		requeue, termReason, termMessage, err := r.reconcileDataLegProjection(ctx, projContentObj(), owner, projTestNS, true)
		if err != nil {
			t.Fatalf("pass %d: reconcileDataLegProjection: %v", pass, err)
		}
		if termReason != "" {
			t.Fatalf("pass %d: the leg went terminal (%s: %s)", pass, termReason, termMessage)
		}
		if !requeue {
			return pass
		}
	}
	t.Fatalf("the data leg still asked for a requeue after %d passes; it never settled (an expectation nothing publishes re-publishes forever)", maxDataLegPasses)
	return 0
}

// completenessSourcePVC is the live source PVC of a capture cell in the requested volume mode. It keeps the
// identity of projSourcePVC (namespace/name/UID) so both the VolumeSnapshot's status.sourceRef and the VCR
// target still point at it, and it stays bound to projSourcePV so a Filesystem claim yields an fsType.
func completenessSourcePVC(volumeMode string) *corev1.PersistentVolumeClaim {
	pvc := projSourcePVCOnPV()
	mode := corev1.PersistentVolumeMode(volumeMode)
	pvc.Spec.VolumeMode = &mode
	return pvc
}

// completenessDomainCaptureOwner is a domain owner whose data leg is a VolumeCaptureRequest.
func completenessDomainCaptureOwner() *unstructured.Unstructured {
	owner := &unstructured.Unstructured{}
	owner.SetGroupVersionKind(schema.GroupVersionKind{Group: "sds-unified-snapshots-poc.deckhouse.io", Version: "v1alpha1", Kind: "DemoVirtualDiskSnapshot"})
	owner.SetNamespace(projTestNS)
	owner.SetName("disk-snap")
	_ = unstructured.SetNestedField(owner.Object, projTestVCRName, "status", "captureState", "domainSpecificController", "volumeCaptureRequestName")
	return owner
}

// completenessDomainImportFixture is newImportProjectionFixture with the DataImport's attested volume
// metadata under the test's control (the shared fixture attests Filesystem + ext4, and a second DataImport
// cannot just be added alongside it: two of them are a cardinality fault that withholds the publish).
func completenessDomainImportFixture(t *testing.T, volumeMode, fsType string) (*SnapshotContentController, client.Client) {
	t.Helper()
	di := importDataImportForLeafWithVolumeData(projTestVSCName, volumeMode, fsType)

	scheme := projScheme(t)
	scheme.AddKnownTypeWithName(dataImportListGVK, &unstructured.UnstructuredList{})
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&storagev1alpha1.SnapshotContent{}).
		WithObjects(projContentTyped(), projVSCWithRestoreSize(), di).
		Build()
	reg := snapshot.NewGVKRegistry()
	reg.MarkRequiresDataArtifact(importLeafKind, true)
	return &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: reg}, cl
}
