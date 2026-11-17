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

// Package snapshotarchive is the reference implementation of the snapshot archive descriptor:
// the per-node snapshot.yaml file of an exported snapshot tree, its canonical form, and the three
// SHA-256 digests every node carries.
//
// Every client that writes or reads a snapshot archive (the d8 client, a backup product) uses this
// package so that the same tree yields byte-identical descriptors and digests everywhere.
//
// # What the package covers
//
//   - The descriptor model: [Descriptor] with [NodeChecksum], [SourceObjectRef], [VolumeInfo] and
//     [VolumeObjectRef], and the format version constants.
//   - The canonical form of a descriptor and its YAML/JSON encoding: [MarshalDescriptor],
//     [UnmarshalDescriptor], [Seal].
//   - The digest algorithms: the content checksum ([NewFileHash], [ComputeFileDigest],
//     [ComputeContentChecksum]), the direct-child commitment ([ComputeChildrenChecksum],
//     [EmptyChildrenChecksum]) and the metadata checksum ([ComputeMetadataChecksum]).
//   - Validation of one descriptor ([ValidateEnvelope], [ValidateDescriptor]) and of a whole
//     descriptor tree held in memory ([ValidateTree]), plus sealing such a tree bottom up
//     ([SealTree]).
//
// # What the package does not cover
//
// Anything about where the bytes live: the directory layout, which files of a node directory are
// covered by the content checksum, transport, tar and compression, zip packing, file locking,
// resumable downloads, atomic writes and every OS-specific concern. The caller selects the covered
// files (see "Computing the digests" in docs/BACKUP_INTEGRATION.md), hashes them with
// [NewFileHash] or [ComputeFileDigest], and passes the per-file digests to
// [ComputeContentChecksum]. The package does no I/O of its own.
//
// # Compatibility contract
//
// The format is versioned by [Descriptor.FormatVersion]. Writers always stamp
// [FormatVersionCurrent]; readers accept [FormatVersionAuthenticatedChildren] and
// [FormatVersionPayloadSizes], accept [FormatVersionLegacy] only under
// [ReadOptions.AllowUnauthenticatedLegacy], and refuse every other version, a future one included.
//
// The canonical form and the digest algorithms are part of the format. Any change that alters the
// bytes produced for an existing input — the canonical JSON field order, a field's omission rule,
// string escaping, the per-file digest framing, the child commitment framing, the sort order, the
// digest algorithm — is a format version change: it needs a new FormatVersion constant, and the
// previous versions must keep being read with their original rules. Adding a descriptor field is
// a format version change too, because the field enters the metadata checksum.
//
// The golden vectors in the snapshotarchivetest package pin the current rules. They must never be
// regenerated to make a change pass: a vector that stops matching means the change breaks every
// archive already written.
package snapshotarchive
