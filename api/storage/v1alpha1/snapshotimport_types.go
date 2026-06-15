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

// SnapshotImport condition types.
const (
	// SnapshotImportConditionIndexReceived is True once the uploaded index has been parsed.
	SnapshotImportConditionIndexReceived = "IndexReceived"
	// SnapshotImportConditionManifestsReceived is True once the uploaded whole-tree manifests
	// archive has been fully received and validated.
	SnapshotImportConditionManifestsReceived = "ManifestsReceived"
	// SnapshotImportConditionUploadsPrepared is True once target StorageClasses are resolved
	// and per-data upload endpoints have been published.
	SnapshotImportConditionUploadsPrepared = "UploadsPrepared"
	// SnapshotImportConditionCaptured is True once every populated PVC has been captured into a
	// durable VolumeSnapshotContent (via VolumeCaptureRequest).
	SnapshotImportConditionCaptured = "Captured"
	// SnapshotImportConditionReady is True once the snapshot tree has been pre-provisioned.
	SnapshotImportConditionReady = "Ready"
)

// SnapshotImport condition reasons.
const (
	// SnapshotImportReasonStorageClassMappingRequired marks UploadsPrepared=False when one or more
	// source StorageClasses cannot be resolved in the target cluster and need spec.storageClassMapping.
	SnapshotImportReasonStorageClassMappingRequired = "StorageClassMappingRequired"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=snapimp
// +kubebuilder:printcolumn:name="Snapshot",type=string,JSONPath=`.spec.snapshotName`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// SnapshotImport orchestrates uploading (importing) a whole Snapshot hierarchy.
//
// It is a namespaced, user-facing resource. The controller publishes upload endpoints for the
// index, the whole-tree manifests and per-data-snapshot volume data; it populates a PVC per data
// snapshot via DataImport, captures each PVC into a durable VolumeSnapshotContent via
// VolumeCaptureRequest, then pre-provisions the cluster-scoped SnapshotContent tree and the
// statically-bound snapshot CRs. All intermediate objects live in this namespace.
type SnapshotImport struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SnapshotImportSpec   `json:"spec,omitempty"`
	Status SnapshotImportStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type SnapshotImportList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SnapshotImport `json:"items"`
}

// +k8s:deepcopy-gen=true
type SnapshotImportSpec struct {
	// SnapshotName is the desired name of the root Snapshot to (re)create on import (same namespace).
	// +kubebuilder:validation:MinLength=1
	SnapshotName string `json:"snapshotName"`

	// StorageClassMapping optionally remaps source StorageClass names (from the index) to target
	// StorageClass names. Sources not present in the map are looked up by identity in the cluster.
	// +optional
	StorageClassMapping map[string]string `json:"storageClassMapping,omitempty"`

	// Publish exposes upload endpoints outside the cluster (Ingress/Route) when true.
	// +optional
	Publish bool `json:"publish,omitempty"`
}

// +k8s:deepcopy-gen=true
type SnapshotImportStatus struct {
	// ObservedGeneration is the spec generation last reconciled into this status.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// IndexUploadURL is where the client uploads the hierarchy index (available immediately).
	// +optional
	IndexUploadURL string `json:"indexUploadURL,omitempty"`

	// ManifestsUploadURL is where the client uploads the whole-tree manifests archive.
	// +optional
	ManifestsUploadURL string `json:"manifestsUploadURL,omitempty"`

	// DataSnapshots lists per-data-snapshot upload endpoints, prepared once the index is received
	// and all target StorageClasses are resolved.
	// +listType=map
	// +listMapKey=snapshotID
	// +optional
	DataSnapshots []SnapshotImportDataEntry `json:"dataSnapshots,omitempty"`

	// Conditions represent the latest observations (IndexReceived, UploadsPrepared, Captured, Ready).
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// SnapshotImportDataEntry is one data snapshot's upload endpoint and capture progress.
// +k8s:deepcopy-gen=true
type SnapshotImportDataEntry struct {
	// SnapshotID is the stable archive identifier "<kind>--<namespace>--<name>" from the index.
	// +kubebuilder:validation:MinLength=1
	SnapshotID string `json:"snapshotID"`

	// UploadURL is the endpoint to upload this snapshot's volume data (set when UploadReady).
	// +optional
	UploadURL string `json:"uploadURL,omitempty"`

	// UploadCA is the base64-encoded PEM CA bundle to trust when uploading to the internal UploadURL.
	// It is empty for a published (externally-trusted) endpoint.
	// +optional
	UploadCA string `json:"uploadCA,omitempty"`

	// UploadReady indicates the populating PVC + importer endpoint are ready to receive data.
	// +optional
	UploadReady bool `json:"uploadReady,omitempty"`

	// Uploaded indicates the client signalled completion of this data upload.
	// +optional
	Uploaded bool `json:"uploaded,omitempty"`

	// CapturedSnapshotContentName is the durable VolumeSnapshotContent captured from the populated
	// PVC (via VolumeCaptureRequest), referenced from the recreated SnapshotContent.dataRefs[].
	// +optional
	CapturedSnapshotContentName string `json:"capturedSnapshotContentName,omitempty"`
}
