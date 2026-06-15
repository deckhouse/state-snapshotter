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

// SnapshotExport condition types.
const (
	// SnapshotExportConditionReady is True once index, manifests and all data endpoints are published.
	SnapshotExportConditionReady = "Ready"
	// SnapshotExportConditionDataReady is True once every data leaf has a serving download endpoint.
	SnapshotExportConditionDataReady = "DataReady"
)

// SnapshotExport condition reasons.
const (
	// SnapshotExportReasonInvalidSpec marks Ready=False when the spec is invalid (e.g. empty snapshotRef).
	SnapshotExportReasonInvalidSpec = "InvalidSpec"
	// SnapshotExportReasonSnapshotNotReady marks Ready=False while the source snapshot tree is not
	// resolvable yet (snapshot not Ready / bound content missing). Non-terminal (requeued).
	SnapshotExportReasonSnapshotNotReady = "SnapshotNotReady"
	// SnapshotExportReasonVolumeModeUnknown marks Ready=False when a data leaf has no captured
	// volumeMode; defaulting would risk a wrong-mode (Block vs Filesystem) restore, so it fails closed.
	SnapshotExportReasonVolumeModeUnknown = "VolumeModeUnknown"
	// SnapshotExportReasonDataExportFailed marks Ready=False when a leaf's VolumeRestoreRequest or
	// DataExport reports a failure. The failure detail is surfaced in the condition message.
	SnapshotExportReasonDataExportFailed = "DataExportFailed"
	// SnapshotExportReasonDataPending marks Ready/DataReady=False while leaves are still converging.
	SnapshotExportReasonDataPending = "DataPending"
	// SnapshotExportReasonAllDataReady marks DataReady=True once every leaf serves a download endpoint.
	SnapshotExportReasonAllDataReady = "AllDataReady"
	// SnapshotExportReasonPublished marks Ready=True once index, manifests and all data are published.
	SnapshotExportReasonPublished = "Published"
)

// LocalSnapshotRef references a root Snapshot in the same namespace as the referrer.
// +k8s:deepcopy-gen=true
type LocalSnapshotRef struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=snapexp
// +kubebuilder:printcolumn:name="Snapshot",type=string,JSONPath=`.spec.snapshotRef.name`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// SnapshotExport orchestrates downloading (exporting) a whole Snapshot hierarchy.
//
// It is a namespaced, user-facing resource. The controller walks the bound SnapshotContent
// tree, restores each data leaf to a PVC via VolumeRestoreRequest, exports each PVC via a
// per-leaf DataExport, and publishes one index URL, one whole-tree manifests URL and a
// per-data-snapshot download URL in status. All intermediate objects live in this namespace.
type SnapshotExport struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SnapshotExportSpec   `json:"spec,omitempty"`
	Status SnapshotExportStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type SnapshotExportList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SnapshotExport `json:"items"`
}

// +k8s:deepcopy-gen=true
type SnapshotExportSpec struct {
	// SnapshotRef references the root Snapshot (same namespace) to export.
	SnapshotRef LocalSnapshotRef `json:"snapshotRef"`

	// Publish exposes the endpoints outside the cluster (Ingress/Route) when true.
	// When false (default), endpoints are only reachable in-cluster.
	// +optional
	Publish bool `json:"publish,omitempty"`
}

// +k8s:deepcopy-gen=true
type SnapshotExportStatus struct {
	// ObservedGeneration is the spec generation last reconciled into this status.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// IndexURL serves the hierarchy index (snapshot tree + per-snapshot data metadata).
	// +optional
	IndexURL string `json:"indexURL,omitempty"`

	// ManifestsURL serves the whole-tree manifests archive (proxied aggregated /manifests).
	// +optional
	ManifestsURL string `json:"manifestsURL,omitempty"`

	// DataSnapshots lists per-data-snapshot export endpoints.
	// +listType=map
	// +listMapKey=snapshotID
	// +optional
	DataSnapshots []SnapshotExportDataEntry `json:"dataSnapshots,omitempty"`

	// Conditions represent the latest observations (Ready, DataReady).
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// SnapshotExportDataEntry is one data leaf's export endpoint.
// +k8s:deepcopy-gen=true
type SnapshotExportDataEntry struct {
	// SnapshotID is the stable archive identifier "<kind>--<namespace>--<name>".
	// +kubebuilder:validation:MinLength=1
	SnapshotID string `json:"snapshotID"`

	// DataURL is the endpoint to download this snapshot's volume data.
	// +optional
	DataURL string `json:"dataURL,omitempty"`

	// DataCA is the base64-encoded PEM CA bundle to trust when downloading from the internal DataURL.
	// It is empty for a published (externally-trusted) endpoint.
	// +optional
	DataCA string `json:"dataCA,omitempty"`

	// Ready indicates the data endpoint is serving (restored PVC bound + DataExport ready).
	// +optional
	Ready bool `json:"ready,omitempty"`
}
