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

// Snapshot requests a namespace state/configuration snapshot.
//
// The binding to a SnapshotContent is set-once (see status.boundSnapshotContentName). The field rule there
// refuses a change; it cannot refuse a removal, because a transition rule does not run when the field is
// absent from the new object — what a writer whose model has no such field produces when it replaces the
// whole status. The companion that does refuse it is on the ROOT: only the root can compare presence across
// the update, and only the root survives `status: null`.

// Snapshot captures or imports the state of one namespace as an immutable snapshot tree.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=snap
// +kubebuilder:metadata:labels=module=state-snapshotter
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Content",type=string,JSONPath=`.status.boundSnapshotContentName`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.boundSnapshotContentName) || (has(self.status) && has(self.status.boundSnapshotContentName))",message="Snapshot status.boundSnapshotContentName is set-once: the bound SnapshotContent name cannot be removed"
// +crd-enricher:raw:properties.apiVersion.x-doc-skip=true
// +crd-enricher:raw:properties.kind.x-doc-skip=true
// +crd-enricher:raw:properties.metadata.x-doc-skip=true
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

// SnapshotMode selects how a snapshot object (root Snapshot or a domain XxxxSnapshot) obtains its
// content. The two values are mutually exclusive content sources: a live cluster (Capture) or an
// uploaded payload (Import). It replaces the former spec.source.import marker.
// +kubebuilder:validation:Enum=Capture;Import
type SnapshotMode string

const (
	// SnapshotModeCapture: dynamic capture from the live cluster (default).
	SnapshotModeCapture SnapshotMode = "Capture"
	// SnapshotModeImport: materialize from an uploaded payload (+ DataImport for data leaves); no live capture.
	SnapshotModeImport SnapshotMode = "Import"
)

// SnapshotSpec is the capture/import mode selector and is fully immutable after creation. A
// snapshot is a one-shot artifact: manifests for the namespace subtree are captured exactly once, so the
// spec must never change. The spec-level transition rule (self == oldSelf) freezes the entire spec on any
// UPDATE while passing through CREATE; consequently metadata.generation never advances and there is
// no recapture (a new capture requires a new Snapshot).
//
// The rule below rejects the removed resourceSelector input. It exists because DELETING the field from the
// schema would NOT reject it: the apiserver prunes fields it does not know BEFORE validation runs, so a
// client still sending a selector would be answered "created" and silently get a snapshot of the whole
// namespace instead of the subset it asked for. Keeping the field declared and refusing it is the only way
// to turn that silent widening into an error. Verified against a live apiserver, not assumed.

// Defines how snapshot content is obtained.
// +k8s:deepcopy-gen=true
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
// +kubebuilder:validation:XValidation:rule="!has(self.resourceSelector)",message="spec.resourceSelector was removed: a Snapshot captures its whole namespace. Drop the field (and update d8: the -l/--selector flag is gone); to keep objects out of a snapshot, label them state-snapshotter.deckhouse.io/exclude"
type SnapshotSpec struct {
	// Mode selects how this Snapshot obtains its content and is immutable (frozen by the spec-level rule):
	//   - Capture (default): dynamic namespace capture from the live cluster.
	//   - Import: the Snapshot is materialized from an uploaded payload (manifests-and-children-refs-upload)
	//     plus, for data leaves, a DataImport — the controller does NOT capture the live namespace.

	// Selects whether content is captured from the live namespace or imported.
	//
	// Capture reads live namespace state. Import waits for content supplied through the upload API and does not capture live state.
	// +kubebuilder:default=Capture
	// +optional
	Mode SnapshotMode `json:"mode,omitempty"`

	// ResourceSelector is a REMOVED input that is NOT read by anything: it stays in the schema only so that
	// the spec-level rule above can refuse a request that carries it. A Snapshot captures its whole
	// namespace; objects are kept out with the ExcludeLabelKey label, not with a selector.
	//
	// Deprecated: never read and never set this field. It is a rejection stub kept for one deprecation
	// cycle, after which it is dropped from the schema together with the rule that refuses it.

	// Removed field. A Snapshot with this field is rejected.
	// To exclude objects from capture, use the `state-snapshotter.deckhouse.io/exclude` label.
	//
	// Deprecated: never read and never set this field.
	// +optional
	// +crd-enricher:raw:x-doc-skip=true
	ResourceSelector *metav1.LabelSelector `json:"resourceSelector,omitempty"`
}

// IsImportMode reports whether this Snapshot is an import target (spec.mode == Import). Import-mode
// snapshots are materialized from an uploaded payload and MUST NOT trigger dynamic namespace capture.
func (s *Snapshot) IsImportMode() bool {
	return s != nil && s.Spec.Mode == SnapshotModeImport
}

// Current binding, tree, and readiness state of the Snapshot.
// +k8s:deepcopy-gen=true
type SnapshotStatus struct {
	// BoundSnapshotContentName is the cluster-scoped name of the bound snapshot content object for this root.
	// The content kind is defined by the snapshot line (e.g. SnapshotContent), not by this field name.
	//
	// It is SET-ONCE: this snapshot's content is the artifact it consists of, and re-pointing the binding
	// would hand the snapshot somebody else's data. The name is derived from this object's own name and uid,
	// so the binder recomputes the same value and its re-publish after a status replace is not a change. A
	// content recovered from the recycle bin is adopted by a NEWLY created Snapshot, whose status starts
	// empty — so the first publish passes and the carve-out lives on the content's spec.snapshotRef, not
	// here. The root-level companion rule refuses removal.

	// Name of the SnapshotContent bound to this Snapshot.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="Snapshot status.boundSnapshotContentName is set-once: the bound SnapshotContent name cannot be replaced"
	BoundSnapshotContentName string `json:"boundSnapshotContentName,omitempty"`

	// SourceRef is the provenance of what this root Snapshot captured. On the namespace-root Snapshot
	// it is the captured Namespace (kind=Namespace), written by the in-process namespace-domain via the SDK
	// (PublishSnapshotSource). It is a self-contained provenance block (read by d8-cli without joining spec
	// and a separate uid); it is NOT a restore directive — an imported snapshot lives in the namespace it
	// was created in, and what is restored from it is addressed there, never at this field. (This is the status-side source reference, present on every
	// snapshot kind; the optional spec.sourceRef on domain snapshots is a distinct lighter ref.)

	// Reference to the source object whose state this snapshot node represents.
	// +optional
	SourceRef *SnapshotSourceObjectRef `json:"sourceRef,omitempty"`

	// CaptureState collects internal capture signals. On the namespace-root Snapshot the core-written
	// commonController.manifestCaptured is present (read by the RBAC hook); the root ALSO carries
	// domainSpecificController, written by the in-process namespace-domain (SDK): manifestCaptureRequestName
	// (the namespace MCR) and phase, plus the core-published excludedRefs aggregate input. There is no
	// volumeCaptureRequestName on the root (a namespace has no data leg of its own).

	// Capture progress exposed in status during Capture mode.
	// +optional
	CaptureState *CaptureStateStatus `json:"captureState,omitempty"`

	// ChildrenSnapshotRefs lists child snapshot objects (strict ref with apiVersion/kind/name)
	// in the N2b run tree. Generic reconcile resolves each child with one Get by ref GVK (no demo-kind
	// branching and no registry scan for child selection); it is not limited to Snapshot.
	// Child namespace is implicit and always equals parent Snapshot namespace.
	// Populated by domain controllers or merge helpers that own graph edges.

	// Direct child snapshot nodes in the same namespace.
	// +optional
	ChildrenSnapshotRefs []SnapshotChildRef `json:"childrenSnapshotRefs,omitempty"`

	// ExcludedRefs is the TOP-LEVEL MIRROR of the bound SnapshotContent's durable excludedRefs aggregate
	// (the whole-subtree set of source objects vetoed out of this snapshot). It is written ONLY by the
	// core, exactly as it mirrors Ready from the bound content. It is a user-facing audit view — the
	// durable truth lives on the cluster-scoped SnapshotContent (which outlives this namespaced object).

	// Source objects excluded from this snapshot subtree.
	// +optional
	// +listType=atomic
	ExcludedRefs []ExcludedObjectRef `json:"excludedRefs,omitempty"`

	// Conditions represent the latest observations (Ready, Bound, Failed, etc.).

	// Current Snapshot conditions.
	// +crd-enricher:raw:items.description="Current condition."
	// +crd-enricher:raw:items.properties.lastTransitionTime.description="Time of the last condition status change."
	// +crd-enricher:raw:items.properties.message.description="Human-readable details about the condition."
	// +crd-enricher:raw:items.properties.observedGeneration.description="Object generation observed when this condition was set."
	// +crd-enricher:raw:items.properties.reason.description="Programmatic reason for the last condition transition."
	// +crd-enricher:raw:items.properties.status.description="Condition status."
	// +crd-enricher:raw:items.properties.type.description="Condition type."
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
