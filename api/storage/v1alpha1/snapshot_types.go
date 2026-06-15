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
// +kubebuilder:resource:scope=Namespaced,shortName=snap
// +kubebuilder:printcolumn:name="Content",type=string,JSONPath=`.status.boundSnapshotContentName`
// Snapshot requests a namespace state/configuration snapshot (MVP: design snapshot-controller.md).
type Snapshot struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SnapshotSpec   `json:"spec,omitempty"`
	Status SnapshotStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type SnapshotList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Snapshot `json:"items"`
}

// +k8s:deepcopy-gen=true
type SnapshotSpec struct {
	// SnapshotClassName optionally selects class/policy (aligned with unified snapshot model; resolution is N2+).
	SnapshotClassName string `json:"snapshotClassName,omitempty"`

	// Source optionally binds this Snapshot to pre-provisioned content (CSI-like static binding).
	// When omitted, the controller performs dynamic namespace capture (default behaviour).
	// When set, the controller takes the static-bind path: it does not create MCR/VCR and
	// validates that the referenced SnapshotContent points back at this Snapshot via spec.snapshotRef.
	// +optional
	Source *SnapshotSource `json:"source,omitempty"`
}

// SnapshotSource selects an existing data source for pre-provisioning, mirroring
// VolumeSnapshot.spec.source. Currently only static binding to an existing
// SnapshotContent is supported.
// +k8s:deepcopy-gen=true
type SnapshotSource struct {
	// SnapshotContentName binds this Snapshot to an existing cluster-scoped SnapshotContent
	// (static pre-provisioning, analogous to volumeSnapshotContentName). It is required when
	// source is set: presence of spec.source is the signal to take the static-bind path, so an
	// empty content name would flip the snapshot into static mode with nothing to bind to.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	SnapshotContentName string `json:"snapshotContentName"`
}

// +k8s:deepcopy-gen=true
type SnapshotStatus struct {
	// ObservedGeneration is the metadata.generation the controller last reconciled into this status.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// BoundSnapshotContentName is the cluster-scoped name of the bound snapshot content object for this root.
	// The content kind is defined by the snapshot line (e.g. SnapshotContent), not by this field name.
	BoundSnapshotContentName string `json:"boundSnapshotContentName,omitempty"`

	// ManifestCaptureRequestName is the temporary MCR owned by this snapshot while own-scope capture runs.
	// Snapshot controllers use it as execution state and clear it after the result is published to SnapshotContent.
	ManifestCaptureRequestName string `json:"manifestCaptureRequestName,omitempty"`

	// VolumeCaptureRequestName is the temporary bulk VCR owned by this snapshot while the volume leg runs.
	// Cleared after VCR.status.dataRefs[] is published to bound SnapshotContent and artifacts are handed off.
	VolumeCaptureRequestName string `json:"volumeCaptureRequestName,omitempty"`

	// ChildrenSnapshotRefs lists child snapshot objects (strict ref with apiVersion/kind/name)
	// in the N2b run tree. Generic reconcile resolves each child with one Get by ref GVK (no demo-kind
	// branching and no registry scan for child selection); it is not limited to Snapshot.
	// Child namespace is implicit and always equals parent Snapshot namespace.
	// Populated by domain controllers or merge helpers that own graph edges.
	// +optional
	ChildrenSnapshotRefs []SnapshotChildRef `json:"childrenSnapshotRefs,omitempty"`

	// Conditions represent the latest observations (Ready, Bound, Failed, etc.).
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
