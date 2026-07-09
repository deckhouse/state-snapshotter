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
// +kubebuilder:metadata:labels=module=state-snapshotter
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Manifests",type=string,JSONPath=`.status.conditions[?(@.type=="ManifestsReady")].status`
// +kubebuilder:printcolumn:name="Volume",type=string,JSONPath=`.status.conditions[?(@.type=="VolumeReady")].status`
// +kubebuilder:printcolumn:name="Children",type=string,JSONPath=`.status.conditions[?(@.type=="ChildrenReady")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// SnapshotContent holds the result of a snapshot (shared carrier for multiple snapshot root kinds).
//
// The spec is immutable EXCEPT for the snapshotRef back-reference, which may be re-pointed onto a freshly
// re-created snapshot subject when recovering a content from the recycle bin. That single carve-out is
// gated on the recycle-bin latch status.parentDeleted: while the owning Snapshot is alive the ref is
// frozen (the anti-spoofing handshake), and it becomes re-pointable only after the parent was deleted and
// this cluster-scoped content survives in the TTL bin. It is the escape hatch that keeps recovery possible
// (the automated restore flow is not yet defined; recovery is done by manual intervention).
// deletionPolicy stays immutable in all cases. The rules live on the root object (not the spec field) so
// CEL can read both self.spec and self.status.
// +kubebuilder:validation:XValidation:rule="self.spec.snapshotRef == oldSelf.spec.snapshotRef || (has(self.status) && has(self.status.parentDeleted) && self.status.parentDeleted)",message="SnapshotContent spec.snapshotRef is immutable until the parent Snapshot is deleted (recycle-bin restore)"
// +kubebuilder:validation:XValidation:rule="has(self.spec.deletionPolicy) == has(oldSelf.spec.deletionPolicy) && (!has(self.spec.deletionPolicy) || self.spec.deletionPolicy == oldSelf.spec.deletionPolicy)",message="SnapshotContent spec.deletionPolicy is immutable"
type SnapshotContent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

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

	// SnapshotRef is the required back-reference to the snapshot subject that owns this content, mirroring
	// VolumeSnapshotContent.spec.volumeSnapshotRef. It is set at creation time by whichever controller binds
	// the content via the snapshot's status.boundSnapshotContentName (a core Snapshot, a domain XXXSnapshot,
	// or a CSI VolumeSnapshot for orphan volume nodes), and it is the anti-spoofing handshake: the restore
	// path accepts a content only when this ref points back at the very snapshot that referenced it, so a
	// user cannot attach a foreign content by pointing status.boundSnapshotContentName at it. It is
	// immutable while the owning Snapshot is alive; it may be re-pointed onto a freshly re-created subject
	// only once status.parentDeleted latched true (see the object-level XValidation rules). The
	// anti-spoofing check is not weakened by this: recovery proceeds by re-pointing the ref onto the new
	// subject's identity, not by bypassing the handshake.
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
	// UID is the durable data artifact UID (for example the VolumeSnapshotContent UID). It makes the
	// artifact reference self-contained, symmetric with source.uid. Optional: the artifact may be
	// referenced before its UID is known, so producers fill it best-effort.
	// +optional
	UID types.UID `json:"uid,omitempty"`
}

// SnapshotDataBinding associates the single PVC source of a logical snapshot node with its captured data
// artifact. Variant A (cardinality ≤1): a SnapshotContent carries at most ONE data binding; multiple
// volumes are modeled as child volume nodes (each its own SnapshotContent), never as a list on one node.
// It is self-contained ({source, artifact, volume metadata}) so the core can mirror it verbatim onto the
// namespaced snapshot's top-level status.data (see the status-source descriptor).
// +k8s:deepcopy-gen=true
type SnapshotDataBinding struct {
	// Source identifies the captured PersistentVolumeClaim (apiVersion/kind/name/namespace + uid) backing
	// this node's data. Its uid is the single volume identity — it replaces the former standalone targetUID.
	Source SnapshotSubjectRef `json:"source"`

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
	//
	// It carries the FROZEN expected child set of the node. As of the content-single-writer design (Block 4,
	// INV-CONTENT-CHILDREN-2) it is written in ONE transition by the SOLE writer, the aggregator
	// (SnapshotContentController): the aggregator projects it from the owning snapshot's frozen
	// status.childrenSnapshotRefs (set-once at phase=Planned) and publishes the COMPLETE set only once every
	// declared child snapshot has materialized+bound its content (all-or-nothing; see
	// PublishSnapshotContentChildrenFromSnapshotRefs). Until then nothing is written, so the field
	// transitions empty -> complete in a single patch and never grows incrementally. ChildrenReady is a pure
	// recompute against it (no flap); a failed child stays a node (E3 degradation, edge preserved).
	//
	// The XValidation transition rule makes this immutability an API-level guarantee (Option A): once the set
	// is non-empty it is frozen — no add, remove, reorder, or replace. The empty->set transition is the only
	// allowed change (oldSelf.size()==0). The single-writer all-or-nothing publish is what makes this safe:
	// it would be a wedge to enable this rule against an incremental (append) writer, which is why it lands
	// only after the orphan special-path dismantling (Block 3d) unified the aggregator as the sole edge
	// writer. self==oldSelf is an O(n) element comparison; MaxItems + the element name MaxLength bound the CEL
	// estimated cost so the apiserver accepts the CRD (an unbounded list/name would exceed the cost budget —
	// this is why the O(1) size-monotonic predecessor rule capped nothing). MaxItems=8192 is a generous
	// per-node direct-child ceiling (far beyond any realistic fan-out) chosen to keep the cost well under
	// budget. Not marked Required: a volume LEAF legitimately has no children (empty/omitted).
	// +optional
	// +kubebuilder:validation:MaxItems=8192
	// +kubebuilder:validation:XValidation:rule="oldSelf.size() == 0 || self == oldSelf",message="childrenSnapshotContentRefs is frozen once set: the complete child SnapshotContent set is immutable (no add, remove, reorder, or replace)"
	ChildrenSnapshotContentRefs []SnapshotContentChildRef `json:"childrenSnapshotContentRefs,omitempty"`

	// Data is the single PVC-source-to-data-artifact binding for this logical snapshot node.
	// Variant A (cardinality ≤1): a node carries at most one data artifact; multiple volumes are
	// represented as separate child volume nodes (childrenSnapshotContentRefs), never as a list here.
	// It is the durable, self-contained {source, artifact, volume metadata} block the core mirrors onto
	// the namespaced snapshot's top-level status.data.
	// +optional
	Data *SnapshotDataBinding `json:"data,omitempty"`

	// ParentDeleted is a one-shot internal latch set by the binder when the parent Snapshot is deleted
	// while this cluster-scoped SnapshotContent survives. Once true, the SnapshotContent controller no
	// longer re-adds the parent-protect finalizer (the parent is gone) and GC may proceed. Monotonic
	// (false -> true only); it replaces the former snapshot.deckhouse.io/parent-deleted annotation.
	// +optional
	ParentDeleted bool `json:"parentDeleted,omitempty"`

	// SubtreeManifestsPersisted is a core-internal monotonic recursive latch (true once this node's own
	// ManifestCheckpoint is Ready AND every declared child SnapshotContent has subtreeManifestsPersisted=true,
	// fail-closed). This SnapshotContent field is the durable truth; user-facing objects do not carry this
	// top-level field but DO carry a core-written mirror at captureState.commonController.subtreeManifestsPersisted
	// (see CommonControllerCaptureState) used as the manifest-exclude pre-gate. It serves purposes not
	// reducible to per-node manifestCaptured: (1) gate the FIRST Ready=True against declared-but-unlinked
	// children, (2) drive the wave-barrier exclude-set of an aggregator MCR (subtree completeness + linkage
	// => no 409 double-capture; identities served by the subtree-manifest-identities subresource),
	// (3) monotonicity (never re-opens after the first Ready). It never expresses failure — a terminal
	// manifest failure surfaces via the Ready reason (IsReasonTerminal).
	// +optional
	SubtreeManifestsPersisted bool `json:"subtreeManifestsPersisted,omitempty"`

	// CaptureState optionally carries core-written suppression leaves for a domain reader; on a
	// core-owned SnapshotContent aggregator this is normally unset.
	// +optional
	CaptureState *CaptureStateStatus `json:"captureState,omitempty"`

	// ExcludedRefs is the DURABLE AGGREGATE of source objects excluded from this content node's subtree
	// (this node's own direct exclusions UNION the direct exclusions of all descendants; on the root, PLUS
	// the explicit top-level drops). It is written ONLY by the core (single aggregator) and is the TRUTH:
	// being on the cluster-scoped SnapshotContent, it outlives deletion of the namespaced Snapshot (the
	// recycle bin, wave4B) and is what the top-level status.excludedRefs mirrors. It is an aggregate rather
	// than direct edges (like childrenSnapshotContentRefs) because an excluded object is non-navigable: no
	// snapshot node is created for it, so per-node reconstruction is impossible after the fact.
	// +optional
	// +listType=atomic
	ExcludedRefs []ExcludedObjectRef `json:"excludedRefs,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// DataList returns status.data as a slice of length 0 or 1. Variant A keeps cardinality ≤1 on a
// node, but the coverage/dedup/publish helpers stay generic over a slice; this bridge lets them iterate
// the single binding without each call site special-casing the nil pointer.
func (c *SnapshotContent) DataList() []SnapshotDataBinding {
	if c == nil || c.Status.Data == nil {
		return nil
	}
	return []SnapshotDataBinding{*c.Status.Data}
}
