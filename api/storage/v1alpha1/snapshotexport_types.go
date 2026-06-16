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
	// SnapshotExportReasonExpired marks Ready=False (terminal, latched) once every data leaf's child
	// DataExport has idled out past spec.ttl: the controller frees the heavy children (restored PVC,
	// DataExport, VRR) and leaves the SnapshotExport as a tombstone for the user/CLI to delete.
	SnapshotExportReasonExpired = "Expired"
)

// SnapshotReference is a typed reference to a snapshot object by GroupVersionKind and name, within
// the referrer's namespace. APIVersion and Kind are optional on input: an empty apiVersion/kind is
// interpreted by the controller as the namespaced root Snapshot
// (storage.deckhouse.io/v1alpha1, kind Snapshot), so a bare {name} keeps exporting a whole Snapshot.
// +k8s:deepcopy-gen=true
type SnapshotReference struct {
	// APIVersion of the referenced snapshot (e.g. "storage.deckhouse.io/v1alpha1"). Empty defaults to
	// the namespaced root Snapshot apiVersion.
	// +optional
	APIVersion string `json:"apiVersion,omitempty"`

	// Kind of the referenced snapshot (e.g. "Snapshot", "DemoVirtualMachineSnapshot"). Empty defaults
	// to "Snapshot".
	// +optional
	Kind string `json:"kind,omitempty"`

	// Name of the referenced snapshot.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=snapexp
// +kubebuilder:printcolumn:name="Snapshot",type=string,JSONPath=`.spec.snapshotRef.name`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="TTL",type=string,JSONPath=`.spec.ttl`,priority=1
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
	// SnapshotRef is a typed reference to the snapshot to export (same namespace). It may be the
	// namespaced root Snapshot (the default when kind/apiVersion are empty) or any domain snapshot CR
	// (e.g. a DemoVirtualMachineSnapshot / DemoVirtualDiskSnapshot), in which case the export covers
	// that node and its subtree only.
	SnapshotRef SnapshotReference `json:"snapshotRef"`

	// TTL is the idle time-to-live for the export's data endpoints, propagated verbatim to each child
	// DataExport. The countdown is reset by active downloads (enforced in the SVDM pod). Once every
	// data leaf's DataExport idles out, the export becomes terminal (Ready=False, reason=Expired) and
	// its heavy children are freed. Required. Format: <h><m><s>, e.g. "2h45m".
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?h)?([0-9]+(\.[0-9]+)?m)?([0-9]+s)?$`
	TTL string `json:"ttl"`

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

	// IndexURL serves the opaque hierarchy index blob. Clients download it verbatim and MUST NOT
	// parse it: every field a client needs to drive the download is mirrored per node in Snapshots.
	// +optional
	IndexURL string `json:"indexURL,omitempty"`

	// Snapshots is the flat, per-node export view: one entry per snapshot in the exported (sub)tree,
	// carrying that node's own manifests URL and, for data nodes, the volume metadata and download
	// endpoint. It replaces the former data-only dataSnapshots list.
	// +listType=map
	// +listMapKey=snapshotID
	// +optional
	Snapshots []SnapshotExportSnapshotEntry `json:"snapshots,omitempty"`

	// Conditions represent the latest observations (Ready, DataReady).
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// SnapshotExportSnapshotEntry is one snapshot node's export view: its own manifests URL plus, for a
// data node, the volume metadata and data download endpoint. A client follows these URLs without
// parsing the index blob.
// +k8s:deepcopy-gen=true
type SnapshotExportSnapshotEntry struct {
	// SnapshotID is the stable archive identifier "<kind>--<namespace>--<name>".
	// +kubebuilder:validation:MinLength=1
	SnapshotID string `json:"snapshotID"`

	// ManifestsURL serves this single node's own manifests (the per-node ?node= aggregated endpoint).
	// +optional
	ManifestsURL string `json:"manifestsURL,omitempty"`

	// HasData is true when this node carries a data volume (DataURL is then populated once ready).
	// +optional
	HasData bool `json:"hasData,omitempty"`

	// VolumeMode is the data volume mode (Block or Filesystem); it selects the data endpoint and the
	// on-disk layout. Empty for dataless nodes.
	// +optional
	VolumeMode string `json:"volumeMode,omitempty"`

	// StorageClassName is the source volume's StorageClass (informational for the client). Empty for
	// dataless nodes.
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`

	// FsType is the source volume filesystem type, when known. Empty for dataless/Block nodes.
	// +optional
	FsType string `json:"fsType,omitempty"`

	// AccessModes are the source volume access modes, when known.
	// +optional
	AccessModes []string `json:"accessModes,omitempty"`

	// Size is the data volume size in bytes (source VolumeSnapshotContent restoreSize); 0 if unknown.
	// +optional
	Size int64 `json:"size,omitempty"`

	// DataURL is the endpoint to download this node's volume data (data nodes only).
	// +optional
	DataURL string `json:"dataURL,omitempty"`

	// DataCA is the base64-encoded PEM CA bundle to trust when downloading from the internal DataURL.
	// It is empty for a published (externally-trusted) endpoint.
	// +optional
	DataCA string `json:"dataCA,omitempty"`

	// Ready indicates the data endpoint is serving (restored PVC bound + DataExport ready). Dataless
	// nodes report Ready=false and do not gate the export's overall readiness.
	// +optional
	Ready bool `json:"ready,omitempty"`
}
