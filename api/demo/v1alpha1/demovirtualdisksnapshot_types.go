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

// DemoVirtualDiskSnapshotSpec defines the desired state of DemoVirtualDiskSnapshot. spec.mode selects
// the content source: Capture (sourceRef), Import (no source), or StaticBind (source.snapshotContentName).
// +k8s:deepcopy-gen=true
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
// +kubebuilder:validation:XValidation:rule="self.mode == 'Capture' ? has(self.sourceRef) : !has(self.sourceRef)",message="spec.sourceRef is required in Capture mode and forbidden otherwise"
// +kubebuilder:validation:XValidation:rule="self.mode == 'StaticBind' ? (has(self.source) && has(self.source.snapshotContentName)) : (!has(self.source) || !has(self.source.snapshotContentName))",message="spec.source.snapshotContentName is required when mode is StaticBind and forbidden otherwise"
type DemoVirtualDiskSnapshotSpec struct {
	// Mode selects how this disk snapshot obtains its content (Capture|Import|StaticBind), immutable.
	// Capture: capture the live source disk (spec.sourceRef). Import: materialize from an uploaded payload
	// plus the matching DataImport (no live capture). StaticBind: bind an existing SnapshotContent
	// (spec.source.snapshotContentName) — used for restore from the retained-content "bin" (wave4B).
	// +kubebuilder:default=Capture
	// +optional
	Mode storagev1alpha1.SnapshotMode `json:"mode,omitempty"`

	// SourceRef identifies the DemoVirtualDisk captured by this snapshot in Capture mode. It is the
	// single source-of-truth for what the snapshot captures and is immutable once set. Required in Capture
	// mode and forbidden otherwise.
	// +optional
	SourceRef *SnapshotSourceRef `json:"sourceRef,omitempty"`

	// Source carries mode-specific source parameters. Required (snapshotContentName) only in StaticBind mode.
	// +optional
	Source *storagev1alpha1.SnapshotSource `json:"source,omitempty"`
}

// DemoVirtualDiskSnapshotStatus defines the observed state of DemoVirtualDiskSnapshot.
// +k8s:deepcopy-gen=true
type DemoVirtualDiskSnapshotStatus struct {
	// BoundSnapshotContentName is the cluster-scoped SnapshotContent name, once created.
	BoundSnapshotContentName string `json:"boundSnapshotContentName,omitempty"`

	// SnapshotSource is the full reference to the captured live source object (top-level, written by the
	// domain controller via PublishSnapshotSource). Self-contained for import-mode recreation.
	// +optional
	SnapshotSource *storagev1alpha1.SnapshotSourceObjectRef `json:"snapshotSource,omitempty"`

	// CaptureState collects internal capture signals: commonController (core-written leg latches
	// manifestCaptured/dataCaptured) and domainSpecificController (domain-written MCR/VCR refs + phase).
	// +optional
	CaptureState *storagev1alpha1.CaptureStateStatus `json:"captureState,omitempty"`

	// ChildrenSnapshotRefs is empty on a data-leaf disk snapshot (kept for uniformity across snapshot kinds).
	// +optional
	// +listType=map
	// +listMapKey=apiVersion
	// +listMapKey=kind
	// +listMapKey=name
	ChildrenSnapshotRefs []storagev1alpha1.SnapshotChildRef `json:"childrenSnapshotRefs,omitempty"`

	// ExcludedRefs is the TOP-LEVEL MIRROR of the bound SnapshotContent's durable excludedRefs aggregate,
	// written ONLY by the core (as it mirrors Ready). A data-leaf disk has no children, so this is
	// normally empty; it is kept for uniformity across snapshot kinds.
	// +optional
	// +listType=atomic
	ExcludedRefs []storagev1alpha1.ExcludedObjectRef `json:"excludedRefs,omitempty"`

	// Data is the self-contained, top-level captured-volume descriptor for this data leaf: the
	// {source, artifact, volumeMode, fsType, accessModes, storageClassName, size} block the core mirrors
	// verbatim from the bound SnapshotContent.status.data (mirrorLeafDataFromContent). It makes the
	// namespaced snapshot self-sufficient for d8 export/restore without reading the cluster-scoped
	// SnapshotContent. It replaces the former flat status.storageClassName/size/volumeMode mirrors.
	// Populated by the common controller once the bound content has a published data binding.
	// +optional
	Data *storagev1alpha1.SnapshotDataBinding `json:"data,omitempty"`

	// Conditions report readiness. Ready is the single user-facing condition, always derived by the core.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// IsImportMode reports whether this disk snapshot is an import target (spec.mode == Import). Import
// leaves are materialized from an uploaded payload plus the matching DataImport and MUST NOT trigger
// live capture (parity with Snapshot.IsImportMode).
func (s *DemoVirtualDiskSnapshot) IsImportMode() bool {
	return s != nil && s.Spec.Mode == storagev1alpha1.SnapshotModeImport
}

// IsStaticBind reports whether this disk snapshot statically binds to pre-provisioned content
// (spec.mode == StaticBind).
func (s *DemoVirtualDiskSnapshot) IsStaticBind() bool {
	return s != nil && s.Spec.Mode == storagev1alpha1.SnapshotModeStaticBind
}

// +kubebuilder:object:root=true
type DemoVirtualDiskSnapshotList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DemoVirtualDiskSnapshot `json:"items"`
}
