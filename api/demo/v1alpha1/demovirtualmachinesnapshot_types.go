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

// IsImportMode reports whether this DemoVirtualMachineSnapshot is an import target (spec.import set).
// Import-mode VM snapshots are materialised from uploaded child refs and must not trigger live-VM capture.
func (s *DemoVirtualMachineSnapshot) IsImportMode() bool {
	return s != nil && s.Spec.Import != nil
}

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

// DemoVirtualMachineSnapshotSpec defines the desired state of DemoVirtualMachineSnapshot.
// +k8s:deepcopy-gen=true
type DemoVirtualMachineSnapshotSpec struct {
	// SourceRef identifies the DemoVirtualMachine captured by this snapshot. It is the single
	// source-of-truth for what the snapshot captures (both manually-created and root-planned
	// snapshots) and is immutable once set. On import it still carries the original VM identity
	// (the live VM may be absent); spec.import is what switches the node into import mode.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.sourceRef is immutable"
	SourceRef SnapshotSourceRef `json:"sourceRef"`

	// Import, when present, marks this DemoVirtualMachineSnapshot as an import target. It is an
	// empty marker object — its mere presence selects import mode: the domain controller skips
	// live-VM capture and children planning; child disk-snapshot CRs are created by the CLI with
	// ownerRefs and children refs arrive via the per-CR manifests-and-children-refs-upload payload.
	// Immutable once set (the marker cannot be added or removed after creation).
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.import is immutable"
	Import *storagev1alpha1.SnapshotImportSource `json:"import,omitempty"`
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

// +kubebuilder:object:root=true
type DemoVirtualMachineSnapshotList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DemoVirtualMachineSnapshot `json:"items"`
}
