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

// SnapshotContent holds the result of a snapshot (shared carrier for multiple snapshot root kinds).
//
// The spec is immutable EXCEPT for the snapshotRef back-reference, which may be re-pointed onto a freshly
// re-created snapshot subject when recovering a content from the recycle bin. That single carve-out is
// gated on the recycle-bin latch status.boundSnapshotDeleted: while the owning Snapshot is alive the ref is
// frozen (the anti-spoofing handshake), and it becomes re-pointable only after the parent was deleted and
// this cluster-scoped content survives in the TTL bin. It is the escape hatch that keeps recovery possible
// (the automated restore flow is not yet defined; recovery is done by manual intervention).
// deletionPolicy stays immutable in all cases. The rules live on the root object (not the spec field) so
// CEL can read both self.spec and self.status.
//
// A published artifact is set-once: once this content names a ManifestCheckpoint, a data binding or its
// complete child set, that identity is the durable answer to "what does this snapshot consist of" and no
// later writer may replace it. The rules on those fields refuse a CHANGE; they cannot refuse a REMOVAL,
// because a transition rule does not run at all when the field is absent from the new object — which is
// precisely what a writer whose model has no such field produces when it replaces the whole status. Each
// axis therefore has a companion rule here, on the ROOT: only the root can compare presence across the
// update, and only the root survives `status: null` (a rule anchored under status disappears with it).

// SnapshotContent stores the captured content of one node of a snapshot tree.
// It links the snapshot node to captured manifests and, when present, captured volume data.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=stsnapct
// +kubebuilder:metadata:labels=module=state-snapshotter
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Manifests",type=string,JSONPath=`.status.conditions[?(@.type=="ManifestsReady")].status`
// +kubebuilder:printcolumn:name="Data",type=string,JSONPath=`.status.conditions[?(@.type=="DataReady")].status`
// +kubebuilder:printcolumn:name="Children",type=string,JSONPath=`.status.conditions[?(@.type=="ChildrenReady")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.spec.snapshotRef == oldSelf.spec.snapshotRef || (has(self.status) && has(self.status.boundSnapshotDeleted) && self.status.boundSnapshotDeleted)",message="SnapshotContent spec.snapshotRef is immutable until the bound Snapshot is deleted (recycle-bin restore)"
// +kubebuilder:validation:XValidation:rule="has(self.spec.deletionPolicy) == has(oldSelf.spec.deletionPolicy) && (!has(self.spec.deletionPolicy) || self.spec.deletionPolicy == oldSelf.spec.deletionPolicy)",message="SnapshotContent spec.deletionPolicy is immutable"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.manifestCheckpointName) || (has(self.status) && has(self.status.manifestCheckpointName))",message="SnapshotContent status.manifestCheckpointName is set-once: a published ManifestCheckpoint name cannot be removed"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.data) || (has(self.status) && has(self.status.data))",message="SnapshotContent status.data is set-once: a published data binding cannot be removed"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.childrenSnapshotContentRefs) || (has(self.status) && has(self.status.childrenSnapshotContentRefs))",message="SnapshotContent status.childrenSnapshotContentRefs is frozen once set: a published child set cannot be removed"
// +crd-enricher:raw:properties.apiVersion.x-doc-skip=true
// +crd-enricher:raw:properties.kind.x-doc-skip=true
// +crd-enricher:raw:properties.metadata.x-doc-skip=true
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

// Desired ownership and retention settings for the stored snapshot content.
// +k8s:deepcopy-gen=true
type SnapshotContentSpec struct {
	// DeletionPolicy controls whether the controller may delete this SnapshotContent when the root snapshot is removed.

	// Controls whether the controller deletes the content when its snapshot tree is removed.
	// +kubebuilder:validation:Enum=Retain;Delete
	DeletionPolicy string `json:"deletionPolicy,omitempty"`

	// SnapshotRef is the required back-reference to the snapshot subject that owns this content, mirroring
	// VolumeSnapshotContent.spec.volumeSnapshotRef. It is set at creation time by whichever controller binds
	// the content via the snapshot's status.boundSnapshotContentName (a core Snapshot, a domain XXXSnapshot,
	// or a CSI VolumeSnapshot for orphan volume nodes), and it is the anti-spoofing handshake: the restore
	// path accepts a content only when this ref points back at the very snapshot that referenced it, so a
	// user cannot attach a foreign content by pointing status.boundSnapshotContentName at it. It is
	// immutable while the owning Snapshot is alive; it may be re-pointed onto a freshly re-created subject
	// only once status.boundSnapshotDeleted latched true (see the object-level XValidation rules). The
	// anti-spoofing check is not weakened by this: recovery proceeds by re-pointing the ref onto the new
	// subject's identity, not by bypassing the handshake.

	// Reference to the snapshot resource that owns this content.
	//
	// The reference is normally immutable while the snapshot exists. It can be updated when retained content is rebound to a recreated snapshot after the previous owner was deleted.
	// +kubebuilder:validation:Required
	SnapshotRef *SnapshotSubjectRef `json:"snapshotRef"`
}

// Reference to a namespaced resource.
// +k8s:deepcopy-gen=true
type SnapshotSubjectRef struct {
	// API version of the referenced resource.
	APIVersion string `json:"apiVersion"`
	// Kind of the referenced resource.
	Kind string `json:"kind"`
	// Name of the referenced object.
	Name string `json:"name"`
	// Namespace of the referenced object.
	Namespace string `json:"namespace,omitempty"`
	// UID of the referenced object.
	UID types.UID `json:"uid,omitempty"`
}

// SnapshotDataArtifactRef points to a durable data artifact produced by the data path.
// It MUST reference a final artifact such as VolumeSnapshotContent or equivalent.
// It MUST NOT reference execution requests such as VolumeCaptureRequest.

// Reference to the artifact that stores captured volume data.
// +k8s:deepcopy-gen=true
type SnapshotDataArtifactRef struct {
	// API version of the data artifact resource.
	// +kubebuilder:validation:MinLength=1
	APIVersion string `json:"apiVersion"`
	// Kind of the data artifact resource.
	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`
	// Name of the data artifact object.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// UID is the durable data artifact UID (for example the VolumeSnapshotContent UID). It makes the
	// artifact reference self-contained, symmetric with sourceRef.uid. Optional: the artifact may be
	// referenced before its UID is known, so producers fill it best-effort.

	// UID of the data artifact object.
	// +optional
	UID types.UID `json:"uid,omitempty"`
}

// SnapshotDataBinding associates the single PVC source of a logical snapshot node with its captured data
// artifact. Cardinality is ≤1 by design: a SnapshotContent carries at most ONE data binding; multiple
// volumes are modeled as child volume nodes (each its own SnapshotContent), never as a list on one node.
// It is self-contained ({sourceRef, artifactRef, volume metadata}) so the core can mirror it verbatim onto
// the namespaced snapshot's top-level status.data (see the status-source descriptor).
//
// Which volume properties belong here is a deliberate, narrow criterion: a property is carried only if
// export/import needs it, OR if it is NOT preserved in the captured PVC manifest and is needed for
// validation. Everything else is read back from the captured manifest on restore and would only be a second,
// divergable copy. spec.accessModes is the worked example of what does NOT qualify: it round-trips through the
// manifest untouched, and the transient export volume does not inherit the source modes (the provisioner
// treats them as optional and defaults to ReadWriteOnce), so it was dropped before this schema was released.

// Describes the source volume and the artifact that stores its captured data.
// +k8s:deepcopy-gen=true
type SnapshotDataBinding struct {
	// SourceRef identifies the captured PersistentVolumeClaim (apiVersion/kind/name/namespace + uid) backing
	// this node's data. Its uid is the single volume identity — it replaces the former standalone targetUID.
	// This is the backing-PVC reference (status.data.sourceRef); it is distinct from the top-level
	// status.sourceRef (SnapshotSourceObjectRef), which references the captured live domain object.

	// Reference to the captured PersistentVolumeClaim that owns this node’s volume data.
	SourceRef SnapshotSubjectRef `json:"sourceRef"`

	// ArtifactRef references the cluster-scoped durable data artifact (for example VolumeSnapshotContent).

	// Reference to the artifact that stores the captured volume data.
	ArtifactRef SnapshotDataArtifactRef `json:"artifactRef"`

	// VolumeMode records the source volume mode (Block or Filesystem). CSI snapshots are
	// mode-agnostic, so this is persisted here to drive the unified export (VolumeRestoreRequest)
	// and to recreate the PVC on import. Typed as a string to keep the api module dependency-free;
	// controllers convert it to corev1.PersistentVolumeMode.

	// Source volume mode.
	// +kubebuilder:validation:Enum=Block;Filesystem
	// +optional
	VolumeMode string `json:"volumeMode,omitempty"`

	// FsType records the source filesystem type (Filesystem volumes only).

	// Source filesystem type for Filesystem volumes.
	// +optional
	FsType string `json:"fsType,omitempty"`

	// StorageClassName records the source StorageClass of the captured volume. Used by the
	// aggregated /index and by import StorageClass mapping.

	// Source StorageClass name.
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`

	// Size records the real allocated size of the captured volume, taken from the data artifact
	// (VolumeSnapshotContent.status.restoreSize). The snapshot outlives the source PVC, so the size MUST
	// be persisted here to recreate the volume on restore/export (the export VolumeRestoreRequest sizes
	// the target PVC from it). Stored as a resource.Quantity string (e.g. "10Gi") to keep the api module
	// dependency-free; controllers parse it via resource.ParseQuantity.

	// Allocated size of the captured volume as a Kubernetes quantity.
	// +optional
	Size string `json:"size,omitempty"`
}

// Current binding, stored data, and readiness state of SnapshotContent.
// +k8s:deepcopy-gen=true
type SnapshotContentStatus struct {
	// ManifestCheckpointName is the cluster-scoped ManifestCheckpoint name once manifest capture has persisted.
	//
	// It is SET-ONCE. The checkpoint it names holds the manifests of the very moment this content's data was
	// captured, so re-pointing it at a checkpoint produced by a later capture would silently pair one
	// snapshot's data with another's manifests. The name is therefore a durable pointer, not a projection of
	// whatever ManifestCaptureRequest exists right now: the first publish passes, every later change is
	// rejected, and the root-level companion rule refuses its removal.

	// Name of the ManifestCheckpoint that stores this node’s captured manifests.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="SnapshotContent status.manifestCheckpointName is set-once: the published ManifestCheckpoint name cannot be replaced"
	ManifestCheckpointName string `json:"manifestCheckpointName,omitempty"`

	// ChildrenSnapshotContentRefs lists direct child SnapshotContent objects in the snapshot tree.
	//
	// It carries the FROZEN expected child set of the node, written in ONE transition by the SOLE writer,
	// the aggregator (SnapshotContentController): the aggregator projects it from the owning snapshot's
	// frozen status.childrenSnapshotRefs (set-once at phase=Planned) and publishes the COMPLETE set only
	// once every declared child snapshot has materialized+bound its content (all-or-nothing; see
	// PublishSnapshotContentChildrenFromSnapshotRefs). Until then nothing is written, so the field
	// transitions empty -> complete in a single patch and never grows incrementally. ChildrenReady is a pure
	// recompute against it (no flap); a failed child stays a node, so the edge is preserved.
	//
	// The XValidation transition rule makes this immutability an API-level guarantee: once the set
	// is non-empty it is frozen — no add, remove, reorder, or replace. The empty->set transition is the only
	// allowed change (oldSelf.size()==0). The single-writer all-or-nothing publish is what makes this safe:
	// the rule would wedge an incremental (append) writer, so it became enforceable only once the aggregator
	// was the sole writer of child edges.
	// self==oldSelf is an O(n) element comparison; MaxItems + the element name MaxLength bound the CEL
	// estimated cost so the apiserver accepts the CRD (an unbounded list/name would exceed the cost budget —
	// this is why the O(1) size-monotonic predecessor rule capped nothing). MaxItems=8192 is a generous
	// per-node direct-child ceiling (far beyond any realistic fan-out) chosen to keep the cost well under
	// budget. Not marked Required: a volume LEAF legitimately has no children (empty/omitted).

	// Direct child SnapshotContent objects.
	// +optional
	// +kubebuilder:validation:MaxItems=8192
	// +kubebuilder:validation:XValidation:rule="oldSelf.size() == 0 || self == oldSelf",message="childrenSnapshotContentRefs is frozen once set: the complete child SnapshotContent set is immutable (no add, remove, reorder, or replace)"
	ChildrenSnapshotContentRefs []SnapshotContentChildRef `json:"childrenSnapshotContentRefs,omitempty"`

	// Data is the single PVC-source-to-data-artifact binding for this logical snapshot node.
	// Cardinality ≤1 by design: a node carries at most one data artifact; multiple volumes are
	// represented as separate child volume nodes (childrenSnapshotContentRefs), never as a list here.
	// It is the durable, self-contained {sourceRef, artifactRef, volume metadata} block the core mirrors
	// onto the namespaced snapshot's top-level status.data.
	//
	// Its IDENTITY is set-once, and identity is narrower than the block: the two refs' apiVersion, kind and
	// name (plus sourceRef.namespace) say WHICH volume was captured into WHICH artifact, and a rewrite there
	// would move the snapshot onto a different artifact — while size, the volume metadata, and a uid that was
	// not known at first publish are enrichment that still has to be able to land. Hence one rule per axis
	// rather than `self == oldSelf` on the whole block. The uid rules are one-directional on purpose: an
	// absent uid may be filled in later, a published one can be neither changed nor dropped. Removal of the
	// whole block is refused by the root-level companion rule.
	//
	// These rules are here and not on SnapshotSubjectRef / SnapshotDataArtifactRef, because the very same
	// SnapshotSubjectRef type is spec.snapshotRef, which MUST stay re-pointable for the recycle-bin restore.
	//
	// sourceRef.namespace is spelled __namespace__ because `namespace` is a CEL reserved word: Kubernetes
	// exposes such a property under its escaped name, and the plain spelling does not resolve to this string
	// field at all. It does not fail as a typo would — the apiserver refuses the whole CRD with "estimated
	// rule cost exceeds budget by factor of more than 100x", because an unresolved selection is costed as an
	// arbitrarily large value. Do not "simplify" the escaping away; the integration test that asserts a
	// namespace rewrite is rejected is what keeps this honest.

	// Captured volume data for this node, when present.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self.artifactRef.apiVersion == oldSelf.artifactRef.apiVersion && self.artifactRef.kind == oldSelf.artifactRef.kind && self.artifactRef.name == oldSelf.artifactRef.name",message="SnapshotContent status.data.artifactRef identity (apiVersion, kind, name) is set-once: a published data artifact cannot be replaced"
	// +kubebuilder:validation:XValidation:rule="!has(oldSelf.artifactRef.uid) || (has(self.artifactRef.uid) && self.artifactRef.uid == oldSelf.artifactRef.uid)",message="SnapshotContent status.data.artifactRef.uid is set-once: it may be filled in after the first publish, but not changed or removed"
	// +kubebuilder:validation:XValidation:rule="self.sourceRef.apiVersion == oldSelf.sourceRef.apiVersion && self.sourceRef.kind == oldSelf.sourceRef.kind && self.sourceRef.name == oldSelf.sourceRef.name",message="SnapshotContent status.data.sourceRef identity (apiVersion, kind, name) is set-once: the captured source of a published data binding cannot be replaced"
	// +kubebuilder:validation:XValidation:rule="!has(oldSelf.sourceRef.__namespace__) || (has(self.sourceRef.__namespace__) && self.sourceRef.__namespace__ == oldSelf.sourceRef.__namespace__)",message="SnapshotContent status.data.sourceRef.namespace is part of the set-once identity of a published data binding: it may not be changed or removed"
	// +kubebuilder:validation:XValidation:rule="!has(oldSelf.sourceRef.uid) || (has(self.sourceRef.uid) && self.sourceRef.uid == oldSelf.sourceRef.uid)",message="SnapshotContent status.data.sourceRef.uid is set-once: it may be filled in after the first publish, but not changed or removed"
	Data *SnapshotDataBinding `json:"data,omitempty"`

	// BoundSnapshotDeleted is a one-shot internal latch set by the binder when the bound namespaced
	// Snapshot is deleted while this cluster-scoped SnapshotContent survives (the recycle bin). Once true,
	// the SnapshotContent controller no longer re-adds the parent-protect finalizer (the snapshot is gone)
	// and GC may proceed. Monotonic (false -> true only); it replaces the former
	// snapshot.deckhouse.io/parent-deleted annotation.

	// Shows whether the bound namespaced snapshot has been deleted while retained content still exists.
	// +optional
	BoundSnapshotDeleted bool `json:"boundSnapshotDeleted,omitempty"`

	// SubtreeManifestsPersisted is a core-internal monotonic recursive latch (true once this node's own
	// ManifestCheckpoint is Ready AND every declared child SnapshotContent has subtreeManifestsPersisted=true,
	// fail-closed). This SnapshotContent field is the durable truth; user-facing objects do not carry this
	// top-level field but DO carry a core-written children-only latch at
	// captureState.commonController.childSubtreesManifestsPersisted (see CommonControllerCaptureState) used as
	// the manifest-exclude pre-gate. It serves purposes not
	// reducible to per-node manifestCaptured: (1) gate the FIRST Ready=True against declared-but-unlinked
	// children, (2) drive the wave-barrier exclude-set of an aggregator MCR (subtree completeness + linkage
	// => no 409 double-capture; identities served by the subtree-manifest-identities subresource),
	// (3) monotonicity (never re-opens after the first Ready). It never expresses failure — a terminal
	// manifest failure surfaces via the Ready reason (IsReasonTerminal).

	// Shows whether manifests for this node and all declared child subtrees have been stored.
	// +optional
	SubtreeManifestsPersisted bool `json:"subtreeManifestsPersisted,omitempty"`

	// CaptureState optionally carries core-written suppression leaves for a domain reader; on a
	// core-owned SnapshotContent aggregator this is normally unset.

	// Capture progress exposed in status during Capture mode.
	// +optional
	CaptureState *CaptureStateStatus `json:"captureState,omitempty"`

	// ExcludedRefs is the DURABLE AGGREGATE of source objects excluded from this content node's subtree
	// (this node's own direct exclusions UNION the direct exclusions of all descendants; on the root, PLUS
	// the explicit top-level drops). It is written ONLY by the core (single aggregator) and is the TRUTH:
	// being on the cluster-scoped SnapshotContent, it outlives deletion of the namespaced Snapshot (the
	// recycle bin) and is what the top-level status.excludedRefs mirrors. It is an aggregate rather
	// than direct edges (like childrenSnapshotContentRefs) because an excluded object is non-navigable: no
	// snapshot node is created for it, so per-node reconstruction is impossible after the fact.

	// Source objects excluded from this content subtree.
	// +optional
	// +listType=atomic
	ExcludedRefs []ExcludedObjectRef `json:"excludedRefs,omitempty"`
	// Current SnapshotContent conditions.
	// +crd-enricher:raw:items.description="Current condition."
	// +crd-enricher:raw:items.properties.lastTransitionTime.description="Time of the last condition status change."
	// +crd-enricher:raw:items.properties.message.description="Human-readable details about the condition."
	// +crd-enricher:raw:items.properties.observedGeneration.description="Object generation observed when this condition was set."
	// +crd-enricher:raw:items.properties.reason.description="Programmatic reason for the last condition transition."
	// +crd-enricher:raw:items.properties.status.description="Condition status."
	// +crd-enricher:raw:items.properties.type.description="Condition type."
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// DataList returns status.data as a slice of length 0 or 1. The design keeps cardinality ≤1 on a
// node, but the coverage/dedup/publish helpers stay generic over a slice; this bridge lets them iterate
// the single binding without each call site special-casing the nil pointer.
func (c *SnapshotContent) DataList() []SnapshotDataBinding {
	if c == nil || c.Status.Data == nil {
		return nil
	}
	return []SnapshotDataBinding{*c.Status.Data}
}
