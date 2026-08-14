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
// +kubebuilder:metadata:labels=module=state-snapshotter
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Content",type=string,JSONPath=`.status.boundSnapshotContentName`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// Snapshot requests a namespace state/configuration snapshot.
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

// +k8s:deepcopy-gen=true
// SnapshotSpec is the capture/import mode selector and is fully immutable after creation. A
// snapshot is a one-shot artifact: manifests for the namespace subtree are captured exactly once, so the
// spec must never change. The spec-level transition rule (self == oldSelf) freezes the entire spec on any
// UPDATE while passing through CREATE; consequently metadata.generation never advances and there is
// no recapture (a new capture requires a new Snapshot).
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
// The rule below rejects the removed resourceSelector input. It exists because DELETING the field from the
// schema would NOT reject it: the apiserver prunes fields it does not know BEFORE validation runs, so a
// client still sending a selector would be answered "created" and silently get a snapshot of the whole
// namespace instead of the subset it asked for. Keeping the field declared and refusing it is the only way
// to turn that silent widening into an error. Verified against a live apiserver, not assumed.
// +kubebuilder:validation:XValidation:rule="!has(self.resourceSelector)",message="spec.resourceSelector was removed: a Snapshot captures its whole namespace. Drop the field (and update d8: the -l/--selector flag is gone); to keep objects out of a snapshot, label them state-snapshotter.deckhouse.io/exclude"
type SnapshotSpec struct {
	// Mode selects how this Snapshot obtains its content and is immutable (frozen by the spec-level rule):
	//   - Capture (default): dynamic namespace capture from the live cluster.
	//   - Import: the Snapshot is materialized from an uploaded payload (manifests-and-children-refs-upload)
	//     plus, for data leaves, a DataImport — the controller does NOT capture the live namespace.
	// +kubebuilder:default=Capture
	// +optional
	Mode SnapshotMode `json:"mode,omitempty"`

	// ResourceSelector is a REMOVED input that is NOT read by anything: it stays in the schema only so that
	// the spec-level rule above can refuse a request that carries it. A Snapshot captures its whole
	// namespace; objects are kept out with the ExcludeLabelKey label, not with a selector.
	//
	// Deprecated: never read and never set this field. It is a rejection stub kept for one deprecation
	// cycle, after which it is dropped from the schema together with the rule that refuses it.
	// +optional
	ResourceSelector *metav1.LabelSelector `json:"resourceSelector,omitempty"`
}

// IsImportMode reports whether this Snapshot is an import target (spec.mode == Import). Import-mode
// snapshots are materialized from an uploaded payload and MUST NOT trigger dynamic namespace capture.
func (s *Snapshot) IsImportMode() bool {
	return s != nil && s.Spec.Mode == SnapshotModeImport
}

// +k8s:deepcopy-gen=true
type SnapshotStatus struct {
	// BoundSnapshotContentName is the cluster-scoped name of the bound snapshot content object for this root.
	// The content kind is defined by the snapshot line (e.g. SnapshotContent), not by this field name.
	BoundSnapshotContentName string `json:"boundSnapshotContentName,omitempty"`

	// SourceRef is the provenance of what this root Snapshot captured. On the namespace-root Snapshot
	// it is the captured Namespace (kind=Namespace), written by the in-process namespace-domain via the SDK
	// (PublishSnapshotSource). It is a self-contained provenance block (read by d8-cli without joining spec
	// and a separate uid); it is NOT a restore directive — the import target namespace comes from
	// spec/targetNamespace, not from this field. (This is the status-side source reference, present on every
	// snapshot kind; the optional spec.sourceRef on domain snapshots is a distinct lighter ref.)
	// +optional
	SourceRef *SnapshotSourceObjectRef `json:"sourceRef,omitempty"`

	// CaptureState collects internal capture signals. On the namespace-root Snapshot the core-written
	// commonController.manifestCaptured is present (read by the RBAC hook); the root ALSO carries
	// domainSpecificController, written by the in-process namespace-domain (SDK): manifestCaptureRequestName
	// (the namespace MCR) and phase, plus the core-published excludedRefs aggregate input. There is no
	// volumeCaptureRequestName on the root (a namespace has no data leg of its own).
	// +optional
	CaptureState *CaptureStateStatus `json:"captureState,omitempty"`

	// ChildrenSnapshotRefs lists child snapshot objects (strict ref with apiVersion/kind/name)
	// in the N2b run tree. Generic reconcile resolves each child with one Get by ref GVK (no demo-kind
	// branching and no registry scan for child selection); it is not limited to Snapshot.
	// Child namespace is implicit and always equals parent Snapshot namespace.
	// Populated by domain controllers or merge helpers that own graph edges.
	// +optional
	ChildrenSnapshotRefs []SnapshotChildRef `json:"childrenSnapshotRefs,omitempty"`

	// ExcludedRefs is the TOP-LEVEL MIRROR of the bound SnapshotContent's durable excludedRefs aggregate
	// (the whole-subtree set of source objects vetoed out of this snapshot). It is written ONLY by the
	// core, exactly as it mirrors Ready from the bound content. It is a user-facing audit view — the
	// durable truth lives on the cluster-scoped SnapshotContent (which outlives this namespaced object).
	// +optional
	// +listType=atomic
	ExcludedRefs []ExcludedObjectRef `json:"excludedRefs,omitempty"`

	// Conditions represent the latest observations (Ready, Bound, Failed, etc.).
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
