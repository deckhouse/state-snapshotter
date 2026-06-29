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

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
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

// DemoVirtualDiskSnapshotSpec defines the desired state of DemoVirtualDiskSnapshot. Exactly one of
// SourceRef (capture) or Source (import) is set.
// +k8s:deepcopy-gen=true
// +kubebuilder:validation:XValidation:rule="has(self.sourceRef) != has(self.source)",message="exactly one of spec.sourceRef (capture) or spec.source (import) must be set"
type DemoVirtualDiskSnapshotSpec struct {
	// SourceRef identifies the DemoVirtualDisk captured by this snapshot in CAPTURE mode. It is the
	// single source-of-truth for what the snapshot captures and is immutable once set. It is a pointer so
	// its presence unambiguously selects capture mode for the exactly-one-of rule; an IMPORT-mode snapshot
	// (spec.source.import) has no live source disk and omits it.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.sourceRef is immutable"
	SourceRef *SnapshotSourceRef `json:"sourceRef,omitempty"`

	// Source, when set, switches this disk snapshot into IMPORT mode. Only the import marker is allowed on
	// a demo snapshot leaf (no static pre-provisioning), so spec.source.import: {} is the sole valid form.
	// In import mode the domain controller does NO capture planning (no source-disk lookup, no MCR/VCR):
	// the common controller materializes the backing SnapshotContent from the uploaded manifests and the
	// data leg from the matching DataImport (reverse-lookup by DataImport.spec.targetRef). Immutable once set.
	// +optional
	// +kubebuilder:validation:XValidation:rule="has(self.import) && !has(self.snapshotContentName)",message="only spec.source.import: {} is supported on a demo snapshot leaf"
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.source is immutable"
	Source *storagev1alpha1.SnapshotSource `json:"source,omitempty"`
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

	// StorageClassName mirrors this data leaf's volume StorageClass for d8 export: the bound
	// SnapshotContent.status.dataRef.storageClassName on capture, or DataImport.spec.storageClassName on
	// import (the import dataRef carries no storageClassName). Populated by the common controller.
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`

	// Size mirrors the real allocated volume size (e.g. "10Gi"): the bound dataRef.size on capture, or the
	// VolumeSnapshotContent restoreSize on import. Populated by the common controller.
	// +optional
	Size string `json:"size,omitempty"`

	// VolumeMode mirrors the source volume mode (Filesystem or Block). Populated by the common controller.
	// +optional
	VolumeMode string `json:"volumeMode,omitempty"`

	// Conditions report readiness (e.g. Ready=True for generic parent children-readiness aggregation).
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// IsImportMode reports whether this disk snapshot is an import target (spec.source.import set). Import
// leaves are materialized from an uploaded payload plus the matching DataImport and MUST NOT trigger
// live capture (parity with Snapshot.IsImportMode).
func (s *DemoVirtualDiskSnapshot) IsImportMode() bool {
	return s != nil && s.Spec.Source != nil && s.Spec.Source.Import != nil
}

// +kubebuilder:object:root=true
type DemoVirtualDiskSnapshotList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DemoVirtualDiskSnapshot `json:"items"`
}
