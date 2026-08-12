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
)
