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
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// dataImportListGVK is the SVDM DataImportList resource. State-snapshotter reads DataImport cross-service
// via the dynamic/unstructured client, so it takes no Go-module dependency on SVDM.
var dataImportListGVK = schema.GroupVersionKind{Group: "storage-foundation.deckhouse.io", Version: "v1alpha1", Kind: "DataImportList"}

// FindDataImportForLeaf reverse-looks-up the DataImport that materializes the data leg for an import-mode
// snapshot leaf. The leaf↔DataImport link is single-directional: only a PopulateData DataImport's
// spec.snapshotRef points at the leaf (apiVersion/kind/name; namespace implicit = leaf namespace), so the
// binder lists DataImports in the leaf namespace and matches snapshotRef against the leaf identity. Matching
// is by GroupKind — the leaf's own GVK carries its group and kind, and snapshotRef.apiVersion carries the
// referenced group/version, so no RESTMapping is needed. CreatePVC DataImports carry no snapshotRef and
// never match. It is the single source of the list+match+fail-closed semantics shared by the generic binder
// (domain data leaves) and the VolumeSnapshot import binder (F2).
//
// Outcomes:
//   - di != nil: exactly one DataImport targets the leaf;
//   - di == nil, terminalReason == "": no DataImport targets the leaf yet (pending — d8 may not have
//     created it; poll);
//   - terminalReason != "": more than one DataImport targets the same leaf (ambiguous, fail-closed);
//   - err != nil: a transient API (List) failure.
func FindDataImportForLeaf(ctx context.Context, c client.Client, leaf *unstructured.Unstructured) (di *unstructured.Unstructured, terminalReason, terminalMessage string, err error) {
	gvk := leaf.GetObjectKind().GroupVersionKind()
	leafGroup := gvk.Group
	leafKind := gvk.Kind
	leafName := leaf.GetName()

	// Defensive: a leaf with no Kind would match any DataImport whose snapshotRef.kind is absent (all-empty
	// equality, fail-open). Production callers always set the leaf GVK, so this is unreachable, but guard
	// it explicitly so the invariant cannot regress silently.
	if leafKind == "" {
		return nil, "", "", nil
	}

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(dataImportListGVK)
	if lErr := c.List(ctx, list, client.InNamespace(leaf.GetNamespace())); lErr != nil {
		return nil, "", "", lErr
	}

	var match *unstructured.Unstructured
	count := 0
	for i := range list.Items {
		item := &list.Items[i]
		apiVersion, _, _ := unstructured.NestedString(item.Object, "spec", "snapshotRef", "apiVersion")
		k, _, _ := unstructured.NestedString(item.Object, "spec", "snapshotRef", "kind")
		n, _, _ := unstructured.NestedString(item.Object, "spec", "snapshotRef", "name")
		// snapshotRef.apiVersion is "group/version" (or "version" for the core group); the leaf identity is
		// keyed by group only, so parse the group out and ignore the version.
		g := schema.FromAPIVersionAndKind(apiVersion, k).Group
		if g == leafGroup && k == leafKind && n == leafName {
			count++
			match = item
		}
	}
	switch count {
	case 0:
		// Help diagnose a producer/consumer version skew: if there are candidate DataImports in the
		// namespace but none matched by GroupKind, the producer may still be writing the legacy
		// spec.targetRef instead of the PopulateData spec.snapshotRef. A leaf with zero candidates is
		// the normal not-yet-created (pending) case and stays quiet.
		if len(list.Items) > 0 {
			log.FromContext(ctx).V(1).Info("no DataImport matched leaf by GroupKind",
				"leafGroup", leafGroup, "leafKind", leafKind, "leafName", leafName,
				"namespace", leaf.GetNamespace(), "candidates", len(list.Items))
		}
		return nil, "", "", nil
	case 1:
		return match, "", "", nil
	default:
		return nil, snapshot.ReasonDataImportAmbiguous, fmt.Sprintf(
			"found %d DataImports targeting %s %s/%s; exactly one is required",
			count, leafKind, leaf.GetNamespace(), leafName), nil
	}
}

// ImportStorageClassName returns a PopulateData DataImport's scratch StorageClass —
// spec.storageParams.storageClassName — which is the authoritative StorageClass mapping for an imported
// leaf: it is the class the imported bytes were actually staged into before being captured into the
// durable VolumeSnapshotContent. This function is the SINGLE place in the codebase that knows the path;
// the aggregator publishes the value into SnapshotContent.status.data and the leaf mirrors copy the
// content verbatim.
//
// There is NO top-level spec.storageClassName on DataImport — neither in the storage-foundation Go types
// nor in its CRD (which prunes unknown fields), and no mutating webhook lifts the nested value up.
// Reading that non-existent path is exactly the defect this helper replaces: it silently yielded "", so
// every imported leaf published an empty status.data.storageClassName and the
// import -> d8 snapshot download -> d8 snapshot import round-trip broke on archive validation.
//
// The read is gated on spec.mode and yields "" for anything other than PopulateData (an empty mode is the
// CRD default CreatePVC, so it yields "" too). A CreatePVC DataImport cannot reach a snapshot leaf — it
// carries no spec.snapshotRef, so FindDataImportForLeaf can never return one — and its
// spec.pvcTemplate.spec.storageClassName describes a PVC the import CREATES AND KEEPS, not a captured
// snapshot's volume: it must not be read here. Gating on the CRD's own discriminator keeps this
// fail-closed on our side instead of resting on a CEL rule owned by another repository.
//
// nil-safe: an unresolved DataImport (nil) yields "".
func ImportStorageClassName(di *unstructured.Unstructured) string {
	if !isPopulateDataImport(di) {
		return ""
	}
	storageClassName, _, _ := unstructured.NestedString(di.Object, "spec", "storageParams", "storageClassName")
	return storageClassName
}

// ImportVolumeMode returns the volumeMode of the volume the imported bytes were staged onto —
// DataImport.status.volumeMode, which storage-foundation republishes from the scratch PVC it provisioned out
// of the uploaded manifest. It is the authoritative import-side volumeMode: the produced
// VolumeSnapshotContent records no mode (CSI snapshots are mode-agnostic), and the imported leaf's
// status.sourceRef names a PVC that exists only in the checkpoint, so there is no live object to read it off.
//
// Downstream fails CLOSED on an empty value rather than defaulting to Filesystem (guessing would restore a
// Block source as a filesystem and serve garbage), so an imported leg that never receives this field never
// exports.
//
// Like ImportStorageClassName, the read is gated on spec.mode: a CreatePVC DataImport also publishes
// status.volumeMode (from the PVC it creates AND KEEPS — see its own handlePVCImportStatus), and that PVC is
// not a captured snapshot's volume. Gating on the CRD's own discriminator keeps this fail-closed on our side
// instead of resting on the reverse-lookup happening to return PopulateData imports only.
//
// nil-safe: an unresolved DataImport (nil) yields "".
func ImportVolumeMode(di *unstructured.Unstructured) string {
	if !isPopulateDataImport(di) {
		return ""
	}
	volumeMode, _, _ := unstructured.NestedString(di.Object, "status", "volumeMode")
	return volumeMode
}

// ImportFsType returns the filesystem the imported bytes were ACTUALLY written onto —
// DataImport.status.data.fsType, which storage-foundation observes on the scratch volume's
// PersistentVolume (spec.csi.fsType) while that volume still exists and destroys right after capture.
//
// This is the only surviving record of the value: the durable VolumeSnapshotContent carries no filesystem
// type, and it must not be re-derived from the target StorageClass parameters, which can be edited or the
// class recreated after the volume was provisioned. state-snapshotter itself cannot observe it at all — it
// only joins the import once the artifact exists, by which time the scratch volume is gone.
//
// Empty means "not known", never "default": a Block import has no filesystem, and a driver may record none
// on the PV. Consumers must not substitute a guess.
//
// Mode-gated and nil-safe for the same reasons as ImportVolumeMode (status.data is written by PopulateData
// only, so the gate is a guard rather than a filter).
func ImportFsType(di *unstructured.Unstructured) string {
	if !isPopulateDataImport(di) {
		return ""
	}
	fsType, _, _ := unstructured.NestedString(di.Object, "status", "data", "fsType")
	return fsType
}

// isPopulateDataImport reports whether the DataImport is the PopulateData one that materializes a snapshot
// node's data leg. An empty spec.mode is the CRD default CreatePVC, so it is not PopulateData. nil-safe.
func isPopulateDataImport(di *unstructured.Unstructured) bool {
	if di == nil {
		return false
	}
	mode, _, _ := unstructured.NestedString(di.Object, "spec", "mode")
	return mode == snapshot.DataImportModePopulateData
}
