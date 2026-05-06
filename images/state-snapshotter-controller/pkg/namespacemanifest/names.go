/*
Copyright 2025 Flant JSC

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
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"k8s.io/apimachinery/pkg/types"
)

// CheckpointNamePrefix is the prefix for ManifestCheckpoint names and related chunk names (must match manifest line).
const CheckpointNamePrefix = "mcp-"

// AnnotationBoundSnapshotContent on ManifestCaptureRequest: ManifestCheckpoint ownerRef targets this SnapshotContent.
const AnnotationBoundSnapshotContent = "state-snapshotter.deckhouse.io/bound-snapshot-content"

// SnapshotMCRName returns the deterministic ManifestCaptureRequest name for a Snapshot root (design §4.7).
func SnapshotMCRName(uid types.UID) string {
	return fmt.Sprintf("snap-%s", uid)
}

// SnapshotRootObjectKeeperName is the cluster-scoped ObjectKeeper name for root retention (FollowObjectWithTTL on Snapshot; see controller config).
// Root ObjectKeeper name intentionally stays stable for one retained root run. Execution request ObjectKeepers are UID-aware
// to avoid stale request-name reuse conflicts.
func SnapshotRootObjectKeeperName(namespace, snapshotName string) string {
	return fmt.Sprintf("ret-snap-%s-%s", namespace, snapshotName)
}

// ManifestCaptureRequestObjectKeeperName returns the generic execution ObjectKeeper name for a ManifestCaptureRequest.
// The name includes a hash of the MCR UID so a recreated request with the same namespace/name cannot collide with a stale OK.
func ManifestCaptureRequestObjectKeeperName(namespace, name string, uid types.UID) string {
	hash := sha256.Sum256([]byte(namespace + "|" + name + "|" + string(uid)))
	return "ret-mcr-" + hex.EncodeToString(hash[:8])
}

// GenerateManifestCheckpointNameFromUID returns the deterministic ManifestCheckpoint name for a ManifestCaptureRequest UID.
// Must stay in sync with ManifestCheckpointController (single SSOT).
func GenerateManifestCheckpointNameFromUID(mcrUID types.UID) string {
	hash := sha256.Sum256([]byte(mcrUID))
	id := hex.EncodeToString(hash[:8])
	return CheckpointNamePrefix + id
}
