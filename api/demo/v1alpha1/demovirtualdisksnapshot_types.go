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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=demovdsnap
// DemoVirtualDiskSnapshot is a minimal demo snapshot node (PR5a). Wires into root Snapshot via children*Refs.
type DemoVirtualDiskSnapshot struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DemoVirtualDiskSnapshotSpec   `json:"spec,omitempty"`
	Status DemoVirtualDiskSnapshotStatus `json:"status,omitempty"`
}

// DemoVirtualDiskSnapshotSpec defines the desired state of DemoVirtualDiskSnapshot.
// +k8s:deepcopy-gen=true
type DemoVirtualDiskSnapshotSpec struct {
	// SourceRef identifies the DemoVirtualDisk captured by this snapshot. It is the single
	// source-of-truth for what the snapshot captures (both manually-created and root-planned
	// snapshots) and is immutable once set. On import it still carries the original disk identity
	// (the live disk may be absent); spec.dataSource is what switches the node into import mode.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.sourceRef is immutable"
	SourceRef SnapshotSourceRef `json:"sourceRef"`

	// DataSource, when set, switches this disk snapshot into IMPORT mode: instead of capturing the live
	// DemoVirtualDisk (no MCR/VCR planning), the data leg is materialized by the common controller from
	// the referenced DataImport's produced artifact (DataImport.status.dataArtifactRef). The domain
	// controller skips capture planning for import-mode snapshots; manifests + children arrive via the
	// per-CR manifests-and-children-refs-upload payload. Immutable once set (C5; demo-domain reference
	// for the domain leaf spec.dataSource->DataImport import trigger).
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.dataSource is immutable"
	DataSource *SnapshotDataImportRef `json:"dataSource,omitempty"`
}

// SnapshotDataImportRef references the namespace-local DataImport (storage.deckhouse.io DataImport)
// whose status.dataArtifactRef provides this snapshot node's data artifact on import. The DataImport is
// resolved in the snapshot's own namespace; apiGroup/kind are fixed (storage.deckhouse.io / DataImport).
// +k8s:deepcopy-gen=true
type SnapshotDataImportRef struct {
	// Name is the DataImport object name in the same namespace as this snapshot.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// DemoVirtualDiskSnapshotStatus defines the observed state of DemoVirtualDiskSnapshot.
// +k8s:deepcopy-gen=true
type DemoVirtualDiskSnapshotStatus struct {
	// BoundSnapshotContentName is the cluster-scoped SnapshotContent name, once created.
	BoundSnapshotContentName string `json:"boundSnapshotContentName,omitempty"`

	// ManifestCaptureRequestName is the temporary MCR owned by this snapshot while own-scope capture runs.
	ManifestCaptureRequestName string `json:"manifestCaptureRequestName,omitempty"`

	// VolumeCaptureRequestName is the temporary VCR owned by this disk snapshot while data-leg capture runs.
	// The common controller reads this VCR's result to enrich and publish SnapshotContent.status.dataRefs;
	// the domain controller never touches SnapshotContent itself.
	VolumeCaptureRequestName string `json:"volumeCaptureRequestName,omitempty"`

	// ManifestCaptured is set by the common controller once this snapshot's manifest capture has been
	// durably handed off to SnapshotContent (manifestCheckpointName published and the ManifestCheckpoint
	// owned by the content). It is a domain-only suppression signal: the domain controller reads it to
	// stop re-creating the MCR after the common controller deletes it, without ever reading SnapshotContent.
	ManifestCaptured bool `json:"manifestCaptured,omitempty"`

	// DataCaptured is set by the common controller once this disk snapshot's data leg has been durably
	// handed off to SnapshotContent (dataRefs published and the VolumeSnapshotContent owned by the content).
	// Domain-only suppression signal: the domain controller reads it to stop re-creating the VCR after the
	// common controller deletes it. Always considered captured for a manifest-only disk (no data leg).
	DataCaptured bool `json:"dataCaptured,omitempty"`

	// Conditions report readiness (e.g. Ready=True for generic parent children-readiness aggregation).
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
type DemoVirtualDiskSnapshotList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DemoVirtualDiskSnapshot `json:"items"`
}
