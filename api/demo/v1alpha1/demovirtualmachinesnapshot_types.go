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
// DemoVirtualMachineSnapshot is an intermediate demo snapshot node (PR5b) under root Snapshot; disk snapshots may attach here.
type DemoVirtualMachineSnapshot struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DemoVirtualMachineSnapshotSpec   `json:"spec,omitempty"`
	Status DemoVirtualMachineSnapshotStatus `json:"status,omitempty"`
}

// DemoVirtualMachineSnapshotSpec defines the desired state of DemoVirtualMachineSnapshot. Exactly one of
// SourceRef (capture) or Source (import) is set.
// +k8s:deepcopy-gen=true
// +kubebuilder:validation:XValidation:rule="has(self.sourceRef) != has(self.source)",message="exactly one of spec.sourceRef (capture) or spec.source (import) must be set"
type DemoVirtualMachineSnapshotSpec struct {
	// SourceRef identifies the DemoVirtualMachine captured by this snapshot in CAPTURE mode. It is the
	// single source-of-truth for what the snapshot captures and is immutable once set. It is a pointer so
	// its presence unambiguously selects capture mode for the exactly-one-of rule; an IMPORT-mode snapshot
	// (spec.source.import) has no live source VM and omits it.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.sourceRef is immutable"
	SourceRef *SnapshotSourceRef `json:"sourceRef,omitempty"`

	// Source, when set, switches this VM snapshot into IMPORT mode (spec.source.import: {} only — no static
	// pre-provisioning on a demo leaf). In import mode the domain controller does NO capture planning (no
	// source-VM lookup, no children planning, no MCR): the common controller materializes it from the
	// uploaded manifests and child refs. Immutable once set.
	// +optional
	// +kubebuilder:validation:XValidation:rule="has(self.import) && !has(self.snapshotContentName)",message="only spec.source.import: {} is supported on a demo snapshot leaf"
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.source is immutable"
	Source *storagev1alpha1.SnapshotSource `json:"source,omitempty"`
}

// DemoVirtualMachineSnapshotStatus defines the observed state of DemoVirtualMachineSnapshot.
// +k8s:deepcopy-gen=true
type DemoVirtualMachineSnapshotStatus struct {
	// BoundSnapshotContentName is the cluster-scoped SnapshotContent name, once created.
	BoundSnapshotContentName string `json:"boundSnapshotContentName,omitempty"`

	// ManifestCaptureRequestName is the temporary MCR owned by this snapshot while own-scope capture runs.
	ManifestCaptureRequestName string `json:"manifestCaptureRequestName,omitempty"`

	// ManifestCaptured is set by the common controller once this snapshot's manifest capture has been
	// durably handed off to SnapshotContent (manifestCheckpointName published and the ManifestCheckpoint
	// owned by the content). It is a domain-only suppression signal: the domain controller reads it to
	// stop re-creating the MCR after the common controller deletes it, without ever reading SnapshotContent.
	ManifestCaptured bool `json:"manifestCaptured,omitempty"`

	// Conditions report readiness (e.g. Ready=True for generic parent children-readiness aggregation).
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
}

// IsImportMode reports whether this VM snapshot is an import target (spec.source.import set). Import
// nodes are materialized from an uploaded payload + child refs and MUST NOT trigger live capture or
// children planning (parity with Snapshot.IsImportMode).
func (s *DemoVirtualMachineSnapshot) IsImportMode() bool {
	return s != nil && s.Spec.Source != nil && s.Spec.Source.Import != nil
}

// +kubebuilder:object:root=true
type DemoVirtualMachineSnapshotList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DemoVirtualMachineSnapshot `json:"items"`
}
