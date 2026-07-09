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
// +kubebuilder:resource:shortName=demovmsnap
// +kubebuilder:metadata:labels=module=state-snapshotter
// DemoVirtualMachineSnapshot is an intermediate demo snapshot node (PR5b) under root Snapshot; disk snapshots may attach here.
type DemoVirtualMachineSnapshot struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DemoVirtualMachineSnapshotSpec   `json:"spec,omitempty"`
	Status DemoVirtualMachineSnapshotStatus `json:"status,omitempty"`
}

// DemoVirtualMachineSnapshotSpec defines the desired state of DemoVirtualMachineSnapshot. spec.mode
// selects the content source: Capture (sourceRef), Import (no source), or StaticBind (source.snapshotContentName).
// +k8s:deepcopy-gen=true
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
// +kubebuilder:validation:XValidation:rule="self.mode == 'Capture' ? has(self.sourceRef) : !has(self.sourceRef)",message="spec.sourceRef is required in Capture mode and forbidden otherwise"
// +kubebuilder:validation:XValidation:rule="self.mode == 'StaticBind' ? (has(self.source) && has(self.source.snapshotContentName)) : (!has(self.source) || !has(self.source.snapshotContentName))",message="spec.source.snapshotContentName is required when mode is StaticBind and forbidden otherwise"
type DemoVirtualMachineSnapshotSpec struct {
	// Mode selects how this VM snapshot obtains its content (Capture|Import|StaticBind), immutable.
	// Capture: capture the live source VM (spec.sourceRef) and plan children. Import: materialize from an
	// uploaded payload plus child refs (no live capture). StaticBind: bind an existing SnapshotContent
	// (spec.source.snapshotContentName) — restore from the retained-content "bin" (wave4B).
	// +kubebuilder:default=Capture
	// +optional
	Mode storagev1alpha1.SnapshotMode `json:"mode,omitempty"`

	// SourceRef identifies the DemoVirtualMachine captured by this snapshot in Capture mode. It is the
	// single source-of-truth for what the snapshot captures and is immutable once set. Required in Capture
	// mode and forbidden otherwise.
	// +optional
	SourceRef *SnapshotSourceRef `json:"sourceRef,omitempty"`

	// Source carries mode-specific source parameters. Required (snapshotContentName) only in StaticBind mode.
	// +optional
	Source *storagev1alpha1.SnapshotSource `json:"source,omitempty"`
}

// DemoVirtualMachineSnapshotStatus defines the observed state of DemoVirtualMachineSnapshot.
// +k8s:deepcopy-gen=true
type DemoVirtualMachineSnapshotStatus struct {
	// BoundSnapshotContentName is the cluster-scoped SnapshotContent name, once created.
	BoundSnapshotContentName string `json:"boundSnapshotContentName,omitempty"`

	// SnapshotSource is the full reference to the captured live source object (top-level, written by the
	// domain controller via PublishSnapshotSource). Self-contained for import-mode recreation.
	// +optional
	SnapshotSource *storagev1alpha1.SnapshotSourceObjectRef `json:"snapshotSource,omitempty"`

	// CaptureState collects internal capture signals. As a manifest-only aggregator, commonController
	// carries only manifestCaptured (no dataCaptured), and domainSpecificController carries the MCR ref +
	// phase (no volumeCaptureRequestName).
	// +optional
	CaptureState *storagev1alpha1.CaptureStateStatus `json:"captureState,omitempty"`

	// Conditions report readiness. Ready is the single user-facing condition, always derived by the core.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ChildrenSnapshotRefs lists child snapshot objects (e.g. DemoVirtualDiskSnapshot) under this VM snapshot.
	// +optional
	// +listType=map
	// +listMapKey=apiVersion
	// +listMapKey=kind
	// +listMapKey=name
	ChildrenSnapshotRefs []storagev1alpha1.SnapshotChildRef `json:"childrenSnapshotRefs,omitempty"`

	// ExcludedRefs is the TOP-LEVEL MIRROR of the bound SnapshotContent's durable excludedRefs aggregate
	// (source objects vetoed out of this VM snapshot's subtree — e.g. a disk labeled with the exclude
	// veto). Written ONLY by the core, exactly as it mirrors Ready.
	// +optional
	// +listType=atomic
	ExcludedRefs []storagev1alpha1.ExcludedObjectRef `json:"excludedRefs,omitempty"`
}

// IsImportMode reports whether this VM snapshot is an import target (spec.mode == Import). Import
// nodes are materialized from an uploaded payload + child refs and MUST NOT trigger live capture or
// children planning (parity with Snapshot.IsImportMode).
func (s *DemoVirtualMachineSnapshot) IsImportMode() bool {
	return s != nil && s.Spec.Mode == storagev1alpha1.SnapshotModeImport
}

// IsStaticBind reports whether this VM snapshot statically binds to pre-provisioned content
// (spec.mode == StaticBind).
func (s *DemoVirtualMachineSnapshot) IsStaticBind() bool {
	return s != nil && s.Spec.Mode == storagev1alpha1.SnapshotModeStaticBind
}

// +kubebuilder:object:root=true
type DemoVirtualMachineSnapshotList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DemoVirtualMachineSnapshot `json:"items"`
}
