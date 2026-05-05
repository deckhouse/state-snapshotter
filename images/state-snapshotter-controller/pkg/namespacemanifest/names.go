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
// Namespace disambiguates same snapshot name in different namespaces. If the root is recreated with the same name,
// the controller deletes a stale OK that still follows the previous SnapshotContent UID before creating one for the new generation.
func SnapshotRootObjectKeeperName(namespace, snapshotName string) string {
	return fmt.Sprintf("ret-snap-%s-%s", namespace, snapshotName)
}

// GenerateManifestCheckpointNameFromUID returns the deterministic ManifestCheckpoint name for a ManifestCaptureRequest UID.
// Must stay in sync with ManifestCheckpointController (single SSOT).
func GenerateManifestCheckpointNameFromUID(mcrUID types.UID) string {
	hash := sha256.Sum256([]byte(mcrUID))
	id := hex.EncodeToString(hash[:8])
	return CheckpointNamePrefix + id
}
