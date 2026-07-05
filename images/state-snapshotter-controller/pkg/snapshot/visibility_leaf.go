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

import storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"

const (
	// CSISnapshotGroup is the external-snapshotter API group.
	CSISnapshotGroup = "snapshot.storage.k8s.io"
	// CSISnapshotVersion is the external-snapshotter API version we interact with.
	CSISnapshotVersion = "v1"
	// CSISnapshotAPIVersion is the external-snapshotter group/version (apiVersion string).
	CSISnapshotAPIVersion = CSISnapshotGroup + "/" + CSISnapshotVersion
	// KindVolumeSnapshot is the CSI VolumeSnapshot kind.
	KindVolumeSnapshot = "VolumeSnapshot"
	// KindVolumeSnapshotContent is the CSI VolumeSnapshotContent kind.
	KindVolumeSnapshotContent = "VolumeSnapshotContent"
	// KindVolumeSnapshotClass is the CSI VolumeSnapshotClass kind.
	KindVolumeSnapshotClass = "VolumeSnapshotClass"

	// AnnotationStorageClassVolumeSnapshotClass is the StorageClass annotation that names the
	// VolumeSnapshotClass to use for volumes provisioned by that StorageClass. The orphan-PVC data leg
	// resolves the class through this annotation (PVC -> StorageClass -> annotation), mirroring the VCR path.
	// This is an external CSI contract owned by the SDS modules (NOT the state-snapshotter API group): the
	// SDS modules write the key on the StorageClass and storage-foundation reads it (ADR
	// 2025-06-24-auto-add-volumesnapshotclass). It MUST stay on storage.deckhouse.io.
	AnnotationStorageClassVolumeSnapshotClass = "storage.deckhouse.io/volumesnapshotclass"

	// LabelChildVolumeNode marks a SnapshotContent created as a standalone child volume node for a
	// root-residual/orphan PVC (Variant A). It is the authoritative signal that distinguishes the orphan
	// capture itself from a real domain subtree child: subtree PVC-coverage must skip these nodes (the
	// orphan PVC must stay in the root residual scope so the same PVC is not double-handled),
	// while the manifest-checkpoint subtree exclude still removes the PVC manifest from the root MCP.
	// Under the unified wave4C naming scheme content names are opaque (keyed by the orphan VolumeSnapshot
	// UID; see api/names), so classification cannot rely on the name — this explicit label, set at
	// creation by EnsureVolumeChildContent, is the sole marker.
	LabelChildVolumeNode = "state-snapshotter.deckhouse.io/child-volume-node"
)

// IsVolumeSnapshotVisibilityLeaf reports whether a Snapshot-level child ref is a CSI VolumeSnapshot
// leaf used only for root orphan-PVC visibility/lifecycle. These refs do not have a backing
// SnapshotContent and must be skipped by content-child publication, subtree exclude, and terminal
// child-Snapshot failure scans.
func IsVolumeSnapshotVisibilityLeaf(ref storagev1alpha1.SnapshotChildRef) bool {
	return ref.APIVersion == CSISnapshotAPIVersion && ref.Kind == KindVolumeSnapshot
}

// IsChildVolumeNodeContent reports whether a SnapshotContent is a standalone child volume node created
// for a root-residual/orphan PVC (Variant A), identified by the LabelChildVolumeNode marker.
func IsChildVolumeNodeContent(content *storagev1alpha1.SnapshotContent) bool {
	if content == nil {
		return false
	}
	return content.Labels[LabelChildVolumeNode] == "true"
}
