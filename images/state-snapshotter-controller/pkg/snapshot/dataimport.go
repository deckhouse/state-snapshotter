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

package snapshot

const (
	// DataImportModePopulateData is the storage-foundation DataImport spec.mode discriminator value for an
	// import that materializes the data leg of an ALREADY-EXISTING snapshot node: the bytes are staged into
	// a transient scratch volume (spec.storageParams) and captured into a durable VolumeSnapshotContent.
	//
	// It is the only mode that takes part in a snapshot tree — it is the only one carrying
	// spec.snapshotRef, which the leaf reverse-lookup matches on. The other mode (CreatePVC, also the CRD
	// default for an empty spec.mode) creates and keeps a PVC and never references a snapshot node, so
	// nothing on the snapshot side may read its spec.
	DataImportModePopulateData = "PopulateData"

	// DataImportPhaseExpired and DataImportPhaseFailed are the two storage-foundation DataImport
	// status.phase values that mean "this import will not produce a data artifact any more":
	//
	//   - Expired: the transfer server was torn down because its spec.ttl idle window elapsed;
	//   - Failed: the import gave up.
	//
	// Neither is terminal for the snapshot NODE: a client may create another DataImport for the same node,
	// so the snapshot side reports them as a cause inside a pending message and never as a terminal reason.
	// The third phase (Completed) needs no constant here — a completed import is recognised by the data
	// artifact it produced, not by its phase, and it is reaped soon after.
	DataImportPhaseExpired = "Expired"
	DataImportPhaseFailed  = "Failed"
)
