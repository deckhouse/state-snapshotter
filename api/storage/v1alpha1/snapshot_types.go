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
	"k8s.io/apimachinery/pkg/selection"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=snap
// +kubebuilder:metadata:labels=module=state-snapshotter
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
type SnapshotSpec struct {
	// Mode selects how this Snapshot obtains its content and is immutable (frozen by the spec-level rule):
	//   - Capture (default): dynamic namespace capture from the live cluster.
	//   - Import: the Snapshot is materialized from an uploaded payload (manifests-and-children-refs-upload)
	//     plus, for data leaves, a DataImport — the controller does NOT capture the live namespace.
	// +kubebuilder:default=Capture
	// +optional
	Mode SnapshotMode `json:"mode,omitempty"`

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
	// Ignored for non-Capture modes (Import): those do not list the live namespace.
	// +optional
	ResourceSelector *metav1.LabelSelector `json:"resourceSelector,omitempty"`
}

// IsImportMode reports whether this Snapshot is an import target (spec.mode == Import). Import-mode
// snapshots are materialized from an uploaded payload and MUST NOT trigger dynamic namespace capture.
func (s *Snapshot) IsImportMode() bool {
	return s != nil && s.Spec.Mode == SnapshotModeImport
}

// ResolveResourceSelector converts spec.resourceSelector into a labels.Selector used by the dynamic
// capture legs, with the absolute exclude veto ALWAYS ANDed on top. The veto (ExcludeLabelKey
// DoesNotExist) is independent of the user selector, so a nil/empty resourceSelector no longer resolves
// to labels.Everything() — it resolves to "everything except objects carrying the exclude label". This
// single fold makes every core leg that resolves through this method honor the veto with one edit.
//
// A non-nil but malformed user selector is unlikely (the field is typed and the CRD schema validates
// most of it at admission), but the conversion can still fail, so the error is returned rather than
// swallowed.
func (s *Snapshot) ResolveResourceSelector() (labels.Selector, error) {
	excludeReq, err := labels.NewRequirement(ExcludeLabelKey, selection.DoesNotExist, nil)
	if err != nil {
		return nil, err
	}
	base := labels.Everything()
	if s != nil && s.Spec.ResourceSelector != nil {
		base, err = metav1.LabelSelectorAsSelector(s.Spec.ResourceSelector)
		if err != nil {
			return nil, err
		}
	}
	return base.Add(*excludeReq), nil
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
