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

package namespacemanifest

import (
	"k8s.io/apimachinery/pkg/types"

	"github.com/deckhouse/state-snapshotter/api/names"
)

// CheckpointNamePrefix is the prefix for ManifestCheckpoint names and related chunk names (unified wave4C
// scheme, see api/names). Chunk names are recorded in status and read back from there, so this prefix is
// not reverse-parsed by consumers.
const CheckpointNamePrefix = "nss-mcp-"

// SnapshotMCRName returns the deterministic ManifestCaptureRequest name for a Snapshot root, keyed by the
// owning snapshot UID (unified wave4C scheme, see api/names).
func SnapshotMCRName(uid types.UID) string {
	return names.ManifestCaptureRequestName(uid)
}

// SnapshotVolumeMCRName returns the deterministic per-orphan-PVC ManifestCaptureRequest name for a child
// volume node (Variant A): each loose/orphan PVC becomes its own snapshot node whose PVC manifest is
// captured in its own ManifestCheckpoint. Keyed by the orphan VolumeSnapshot UID (the per-PVC leaf
// identity, unified wave4C scheme), so it is stable across reconciles and distinct from the root MCR.
func SnapshotVolumeMCRName(orphanVSUID types.UID) string {
	return names.ManifestCaptureRequestName(orphanVSUID)
}

// ManifestCaptureRequestObjectKeeperName returns the execution ObjectKeeper name for a ManifestCaptureRequest,
// keyed by the MCR UID (unified wave4C scheme, see api/names). A recreated request with the same
// namespace/name gets a distinct UID and therefore a distinct OK, so a stale OK cannot collide.
func ManifestCaptureRequestObjectKeeperName(uid types.UID) string {
	return names.ObjectKeeperName(uid)
}

// GenerateManifestCheckpointNameFromUID returns the deterministic ManifestCheckpoint name for a
// ManifestCaptureRequest UID (unified wave4C scheme, see api/names). Single SSOT with the checkpoint
// controller.
func GenerateManifestCheckpointNameFromUID(mcrUID types.UID) string {
	return names.ManifestCheckpointName(mcrUID)
}
