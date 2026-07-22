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

// Package names is the single source of truth for the state-snapshotter object-name scheme (wave4C).
//
// Every generated name is derived from cluster-local UIDs via a truncated sha256 hex hash, giving a stable,
// deterministic, DNS-1123-safe name. Names are intentionally OPAQUE: object connectivity is carried by
// refs/ownerRefs (child->parent ownerRef with the parent's live UID, parent->children via childRefs, and
// node->content via boundSnapshotContentName), never by parsing a name. Nothing should reverse-derive a
// name to resolve an object.
//
// This package deliberately depends on stdlib + apimachinery only (no controller-runtime, no SDK) so the
// core imports it directly and the SDK re-exports it for domain controllers — one definition, zero
// duplicates. d8-cli does NOT import this package: on import it replays the names recorded in the archive
// rather than regenerating them.
package names

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"

	"k8s.io/apimachinery/pkg/types"
)

// hashPrefixLen constants: h8 = 8 hex chars (4 bytes), h16 = 16 hex chars (8 bytes) of sha256.
const (
	h8Len  = 8
	h16Len = 16
)

// Name prefixes for the wave4C object-name scheme. They are the single source of truth reused by both the
// generators below and by recognizers (e.g. the delete-protection backfill classifier, which — as a
// migration-only mechanism — may key on the fact that a legacy object carries one of our deterministic
// names). Admission never parses names; these are for provenance classification only.
const (
	PrefixChildSnapshot   = "nss-snap-"
	PrefixContent         = "nss-content-"
	PrefixManifestCapture = "nss-mcr-"
	PrefixManifestCP      = "nss-mcp-"
	PrefixOrphanVS        = "nss-vs-"
	PrefixVolumeCapture   = "nss-vcr-"
	PrefixObjectKeeper    = "nss-ok-"
	PrefixImportKeeper    = "nss-import-ok-"
)

// h8 returns the first 8 hex chars of sha256(s). Used for the parent/root discriminator segment, where a
// shorter hash suffices to disambiguate the (few) parents a source can sit under.
func h8(s string) string {
	return hashHex(s, h8Len)
}

// h16 returns the first 16 hex chars of sha256(s). Used for the primary identity segment.
func h16(s string) string {
	return hashHex(s, h16Len)
}

func hashHex(s string, n int) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:n]
}

// ChildSnapshotName is the name of a snapshot CR node (root-owned child or domain sub-child):
// nss-snap-<h8(parentSnapshotUID)>-<h16(sourceUID)>. The parent segment (h8) lets the same source object
// captured under two different parents (a DAG) resolve to distinct names; the source segment (h16) is the
// primary identity. childSnapshotGVK is intentionally NOT part of the key — it is derived from the source
// GVK via the CustomSnapshotDefinition.
func ChildSnapshotName(parentSnapshotUID, sourceUID types.UID) string {
	return PrefixChildSnapshot + h8(string(parentSnapshotUID)) + "-" + h16(string(sourceUID))
}

// ContentName is the name of the SnapshotContent bound to a snapshot object: nss-content-<h16(snapshotUID)>.
// One SnapshotContent per owning snapshot object (1:1), keyed by that owner's UID.
func ContentName(snapshotUID types.UID) string {
	return PrefixContent + h16(string(snapshotUID))
}

// ManifestCaptureRequestName is the MCR name: nss-mcr-<h16(snapshotUID)>. For an orphan per-PVC MCR the
// caller passes the orphan VolumeSnapshot's UID (so each orphan PVC gets its own MCR name, no collision).
func ManifestCaptureRequestName(snapshotUID types.UID) string {
	return PrefixManifestCapture + h16(string(snapshotUID))
}

// ManifestCheckpointName is the MCP name: nss-mcp-<h16(mcrUID)>.
func ManifestCheckpointName(mcrUID types.UID) string {
	return PrefixManifestCP + h16(string(mcrUID))
}

// ChunkName is the manifest chunk name: nss-mcp-<h16(mcpUID)>-<index>.
func ChunkName(mcpUID types.UID, index int) string {
	return PrefixManifestCP + h16(string(mcpUID)) + "-" + strconv.Itoa(index)
}

// OrphanVolumeSnapshotName is the orphan/residual-PVC VolumeSnapshot name:
// nss-vs-<h8(rootUID)>-<h16(pvcUID)>.
func OrphanVolumeSnapshotName(rootUID, pvcUID types.UID) string {
	return PrefixOrphanVS + h8(string(rootUID)) + "-" + h16(string(pvcUID))
}

// VolumeCaptureRequestName is the VCR name: nss-vcr-<h16(snapshotUID)>.
func VolumeCaptureRequestName(snapshotUID types.UID) string {
	return PrefixVolumeCapture + h16(string(snapshotUID))
}

// ObjectKeeperName is the ObjectKeeper name for the tracked object (Snapshot/MCR/VCR):
// nss-ok-<h16(objUID)>.
func ObjectKeeperName(objUID types.UID) string {
	return PrefixObjectKeeper + h16(string(objUID))
}

// ImportManifestCheckpointObjectKeeperName is the dedicated ObjectKeeper name that anchors the
// reconstructed (import) ManifestCheckpoint until it is handed off to its SnapshotContent:
// nss-import-ok-<h16(snapshotUID)>. Keyed by the import snapshot UID but with a DISTINCT prefix from
// ObjectKeeperName so it never collides with the same snapshot's root ObjectKeeper (which is
// ObjectKeeperName(snapshotUID), i.e. keyed by the same UID). See content-single-writer design §10.1.
func ImportManifestCheckpointObjectKeeperName(snapshotUID types.UID) string {
	return PrefixImportKeeper + h16(string(snapshotUID))
}
