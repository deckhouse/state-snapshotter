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
	"k8s.io/apimachinery/pkg/labels"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=snap
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Content",type=string,JSONPath=`.status.boundSnapshotContentName`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
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
// SnapshotSpec is the capture/import mode selector and is fully immutable after creation. A snapshot
// is a one-shot artifact: manifests for the namespace subtree are captured exactly once, so the spec
// must never change. The spec-level transition rule (self == oldSelf) freezes the entire spec on any
// UPDATE while passing through CREATE; consequently metadata.generation never advances and there is
// no recapture (a new capture requires a new Snapshot). The narrower spec.source rules below are now
// subsumed by this rule but kept for clearer, field-scoped admission messages.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.source) == has(oldSelf.source)",message="spec.source cannot be added or removed after creation"
type SnapshotSpec struct {
	// SnapshotClassName optionally selects class/policy (aligned with unified snapshot model; resolution is N2+).
	SnapshotClassName string `json:"snapshotClassName,omitempty"`

	// Source optionally selects how this Snapshot obtains its content instead of dynamic capture.
	// When omitted, the controller performs dynamic namespace capture (default behaviour).
	// When set, exactly one member selects the mode (CEL one-of) and the whole source is immutable:
	//   - import: the Snapshot is materialized from an uploaded payload (manifests-and-children-refs-upload)
	//     plus, for data leaves, a DataImport — the controller does NOT capture the live namespace.
	//   - snapshotContentName: CSI-like static binding to existing pre-provisioned content; the controller
	//     does not create MCR/VCR and validates that the referenced SnapshotContent points back at this
	//     Snapshot via spec.snapshotRef.
	// +optional
	// +kubebuilder:validation:XValidation:rule="has(self.import) != has(self.snapshotContentName)",message="exactly one of spec.source.import or spec.source.snapshotContentName must be set"
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.source is immutable"
	Source *SnapshotSource `json:"source,omitempty"`

	// ResourceSelector optionally restricts which namespace resources are captured. It is applied to the
	// dynamic-capture legs: namespace manifests, top-level/standalone domain resources expanded via
	// CustomSnapshotDefinition, and PVCs (volume data leg). It is layered (ANDed) on top of the built-in
	// capture exclusions and can only narrow the capture, never force-capture controller/own machinery.
	//
	// Standard Kubernetes label selector: matchLabels and matchExpressions are ANDed together, so a single
	// selector can both include and exclude - e.g. matchLabels {app: myapp} together with a NotIn/DoesNotExist
	// matchExpression. Because everything is ANDed, OR semantics cannot be expressed in one selector.
	// When omitted, all resources are captured (no filtering).
	//
	// Ignored for spec.source modes (import / snapshotContentName): those do not list the live namespace.
	// +optional
	ResourceSelector *metav1.LabelSelector `json:"resourceSelector,omitempty"`
}

// SnapshotSource selects how a Snapshot obtains its content, mirroring VolumeSnapshot.spec.source.
// It is a CEL one-of: exactly one of Import or SnapshotContentName is set (enforced on SnapshotSpec.Source).
// +k8s:deepcopy-gen=true
type SnapshotSource struct {
	// Import, when present, marks this Snapshot as an import target. It is an empty marker object —
	// its mere presence selects import mode (content is materialized from an uploaded payload, not
	// captured from the live namespace). Kept as a struct (not a bool) to mirror VolumeSnapshot-style
	// source members and to allow future import options without another union rewrite.
	// +optional
	Import *SnapshotImportSource `json:"import,omitempty"`

	// SnapshotContentName binds this Snapshot to an existing cluster-scoped SnapshotContent
	// (static pre-provisioning, analogous to volumeSnapshotContentName).
	// +optional
	// +kubebuilder:validation:MinLength=1
	SnapshotContentName string `json:"snapshotContentName,omitempty"`
}

// SnapshotImportSource is an empty marker: its presence in spec.source selects import mode.
// +k8s:deepcopy-gen=true
type SnapshotImportSource struct{}

// IsImportMode reports whether this Snapshot is an import target (spec.source.import set). Import-mode
// snapshots are materialized from an uploaded payload and MUST NOT trigger dynamic namespace capture.
func (s *Snapshot) IsImportMode() bool {
	return s != nil && s.Spec.Source != nil && s.Spec.Source.Import != nil
}

// IsStaticBind reports whether this Snapshot statically binds to pre-provisioned content
// (spec.source.snapshotContentName set).
func (s *Snapshot) IsStaticBind() bool {
	return s != nil && s.Spec.Source != nil && s.Spec.Source.SnapshotContentName != ""
}

// ResolveResourceSelector converts spec.resourceSelector into a labels.Selector used by the dynamic
// capture legs. A nil selector means "no filtering" and resolves to labels.Everything(); note this
// differs from metav1.LabelSelectorAsSelector(nil), which returns labels.Nothing() (matches nothing).
// A non-nil but malformed selector is unlikely (the field is typed and the CRD schema validates most of
// it at admission), but the conversion can still fail, so the error is returned rather than swallowed.
func (s *Snapshot) ResolveResourceSelector() (labels.Selector, error) {
	if s == nil || s.Spec.ResourceSelector == nil {
		return labels.Everything(), nil
	}
	return metav1.LabelSelectorAsSelector(s.Spec.ResourceSelector)
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
