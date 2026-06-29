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
	"k8s.io/apimachinery/pkg/types"
)

// DeletionPolicy values for SnapshotContent.spec.deletionPolicy.
const (
	SnapshotContentDeletionPolicyRetain = "Retain"
	SnapshotContentDeletionPolicyDelete = "Delete"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=stsnapct
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Manifests",type=string,JSONPath=`.status.conditions[?(@.type=="ManifestsReady")].status`
// +kubebuilder:printcolumn:name="Volume",type=string,JSONPath=`.status.conditions[?(@.type=="VolumeReady")].status`
// +kubebuilder:printcolumn:name="Children",type=string,JSONPath=`.status.conditions[?(@.type=="ChildrenReady")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// SnapshotContent holds the result of a snapshot (shared carrier for multiple snapshot root kinds).
type SnapshotContent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="SnapshotContent spec is immutable"
	Spec   SnapshotContentSpec   `json:"spec,omitempty"`
	Status SnapshotContentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type SnapshotContentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SnapshotContent `json:"items"`
}

// +k8s:deepcopy-gen=true
type SnapshotContentSpec struct {
	// DeletionPolicy controls whether the controller may delete this SnapshotContent when the root snapshot is removed.
	// +kubebuilder:validation:Enum=Retain;Delete
	DeletionPolicy string `json:"deletionPolicy,omitempty"`

	// SnapshotRef is the required, immutable back-reference to the snapshot subject that owns this
	// content, mirroring VolumeSnapshotContent.spec.volumeSnapshotRef. It is set at creation time by
	// whichever controller binds the content via the snapshot's status.boundSnapshotContentName (a core
	// Snapshot, a domain XXXSnapshot, or a CSI VolumeSnapshot for orphan volume nodes), and it is the
	// anti-spoofing handshake: a consumer (static bind / restore) accepts a content only when this ref
	// points back at the very snapshot that referenced it, so a user cannot attach a foreign content by
	// pointing status.boundSnapshotContentName at it. The whole spec is immutable, so this ref cannot
	// change after creation.
	// +kubebuilder:validation:Required
	SnapshotRef *SnapshotSubjectRef `json:"snapshotRef"`
}

// +k8s:deepcopy-gen=true
type SnapshotSubjectRef struct {
	APIVersion string    `json:"apiVersion"`
	Kind       string    `json:"kind"`
	Name       string    `json:"name"`
	Namespace  string    `json:"namespace,omitempty"`
	UID        types.UID `json:"uid,omitempty"`
}

// SnapshotDataArtifactRef points to a durable data artifact produced by the data path.
// It MUST reference a final artifact such as VolumeSnapshotContent or equivalent.
// It MUST NOT reference execution requests such as VolumeCaptureRequest.
// +k8s:deepcopy-gen=true
type SnapshotDataArtifactRef struct {
	// +kubebuilder:validation:MinLength=1
	APIVersion string `json:"apiVersion"`
	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// SnapshotDataBinding associates the single PVC target of a logical snapshot node with its captured
// data artifact. Variant A (cardinality ≤1): a SnapshotContent carries at most ONE dataRef; multiple
// volumes are modeled as child volume nodes (each its own SnapshotContent), never as a list on one node.
// +k8s:deepcopy-gen=true
type SnapshotDataBinding struct {
	// TargetUID identifies the captured PersistentVolumeClaim (its UID) backing this node's data.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	TargetUID string `json:"targetUID"`

	// Target identifies the PVC (and related metadata) captured in MCP for this binding.
	Target SnapshotSubjectRef `json:"target"`

	// Artifact references the cluster-scoped durable data artifact (for example VolumeSnapshotContent).
	Artifact SnapshotDataArtifactRef `json:"artifact"`

	// VolumeMode records the source volume mode (Block or Filesystem). CSI snapshots are
	// mode-agnostic, so this is persisted here to drive the unified export (VolumeRestoreRequest)
	// and to recreate the PVC on import. Typed as a string to keep the api module dependency-free;
	// controllers convert it to corev1.PersistentVolumeMode.
	// +kubebuilder:validation:Enum=Block;Filesystem
	// +optional
	VolumeMode string `json:"volumeMode,omitempty"`

	// FsType records the source filesystem type (Filesystem volumes only).
	// +optional
	FsType string `json:"fsType,omitempty"`

	// AccessModes records the source PVC access modes (e.g. ReadWriteOnce, ReadWriteMany).
	// +optional
	AccessModes []string `json:"accessModes,omitempty"`

	// StorageClassName records the source StorageClass of the captured volume. Used by the
	// aggregated /index and by import StorageClass mapping.
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`

	// Size records the real allocated size of the captured volume, taken from the data artifact
	// (VolumeSnapshotContent.status.restoreSize). The snapshot outlives the source PVC, so the size MUST
	// be persisted here to recreate the volume on restore/export (the export VolumeRestoreRequest sizes
	// the target PVC from it). Stored as a resource.Quantity string (e.g. "10Gi") to keep the api module
	// dependency-free; controllers parse it via resource.ParseQuantity.
	// +optional
	Size string `json:"size,omitempty"`
}

// +k8s:deepcopy-gen=true
type SnapshotContentStatus struct {
	// ManifestCheckpointName is the cluster-scoped ManifestCheckpoint name once manifest capture has persisted.
	// +optional
	ManifestCheckpointName string `json:"manifestCheckpointName,omitempty"`

	// ChildrenSnapshotContentRefs lists direct child SnapshotContent objects in the snapshot tree.
	// +optional
	ChildrenSnapshotContentRefs []SnapshotContentChildRef `json:"childrenSnapshotContentRefs,omitempty"`

	// DataRef is the single PVC-target-to-data-artifact binding for this logical snapshot node.
	// Variant A (cardinality ≤1): a node carries at most one data artifact; multiple volumes are
	// represented as separate child volume nodes (childrenSnapshotContentRefs), never as a list here.
	// +optional
	DataRef *SnapshotDataBinding `json:"dataRef,omitempty"`

	// ResidualVolumeCapture latches completion of the final residual/orphan-PVC capture wave on a
	// namespace-root SnapshotContent. It is the gate signal the aggregator reads to hold the FIRST
	// Ready=True until the residual wave is done (fail-closed). It is written ONLY by the snapshot
	// reconciler (the sole owner of the namespace PVC scope), never by the aggregator: absence (or any
	// Phase != Complete) means "wave not finished yet". See ResidualVolumeCaptureStatus.
	// +optional
	ResidualVolumeCapture *ResidualVolumeCaptureStatus `json:"residualVolumeCapture,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ResidualVolumeCapturePhase values for SnapshotContentStatus.residualVolumeCapture.phase.
const (
	// ResidualVolumeCapturePhasePending is an explicit "wave not finished" marker. The reconciler is
	// not required to write it: an absent residualVolumeCapture is treated as Pending. It exists for
	// observability only; the gate reacts solely to Complete.
	ResidualVolumeCapturePhasePending = "Pending"
	// ResidualVolumeCapturePhaseComplete latches that the residual/orphan-PVC capture wave finished
	// (no orphan targets, or every orphan child node is linked and ready). The aggregator opens the
	// first Ready=True only when phase == Complete. Monotonic: it never reverts (point-in-time capture,
	// immutable spec — no recapture).
	ResidualVolumeCapturePhaseComplete = "Complete"
)

// ResidualVolumeCaptureStatus is the residual/orphan-PVC capture latch on a namespace-root
// SnapshotContent. Only the snapshot reconciler writes it (status field, like dataRef), and only the
// SnapshotContent aggregator reads it (locally, to gate the first Ready=True). It is NOT a condition:
// conditions on SnapshotContent are the aggregator's exclusive domain, so the "wave finished" signal
// that the reconciler owns is carried as a field and surfaced to users via the aggregate Ready reason.
// +k8s:deepcopy-gen=true
type ResidualVolumeCaptureStatus struct {
	// Phase is the latch state. The reconciler writes only Complete; the aggregator treats anything
	// other than Complete (including an absent residualVolumeCapture) as "wave not finished".
	// +kubebuilder:validation:Enum=Pending;Complete
	// +optional
	Phase string `json:"phase,omitempty"`

	// TargetUIDs records the captured orphan PVC UIDs at completion (empty when there were no orphan
	// targets). Diagnostic only; the gate does not read it.
	// +optional
	TargetUIDs []string `json:"targetUIDs,omitempty"`

	// CompletedAt records when the latch reached Complete. Diagnostic only.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// DataRefList returns status.dataRef as a slice of length 0 or 1. Variant A keeps cardinality ≤1 on a
// node, but the coverage/dedup/publish helpers stay generic over a slice; this bridge lets them iterate
// the single binding without each call site special-casing the nil pointer.
func (c *SnapshotContent) DataRefList() []SnapshotDataBinding {
	if c == nil || c.Status.DataRef == nil {
		return nil
	}
	return []SnapshotDataBinding{*c.Status.DataRef}
}
