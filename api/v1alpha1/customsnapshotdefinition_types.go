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
// +kubebuilder:resource:scope=Cluster,shortName=csd
// +kubebuilder:metadata:labels=module=state-snapshotter
// +kubebuilder:printcolumn:name="Weight",type=integer,JSONPath=`.spec.weight`
// +kubebuilder:printcolumn:name="Accepted",type=string,JSONPath=`.status.conditions[?(@.type=="Accepted")].status`
// +kubebuilder:printcolumn:name="AccessGranted",type=string,JSONPath=`.status.conditions[?(@.type=="AccessGranted")].status`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// CustomSnapshotDefinition registers custom snapshot types for platform modules.
// See ADR: snapshot-rework/2026-01-23-unified-snapshots-registry.md
type CustomSnapshotDefinition struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CustomSnapshotDefinitionSpec   `json:"spec,omitempty"`
	Status CustomSnapshotDefinitionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type CustomSnapshotDefinitionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CustomSnapshotDefinition `json:"items"`
}

// +k8s:deepcopy-gen=true
// CustomSnapshotDefinitionSpec maps one source resource GVK to the snapshot GVK that materializes it.
// One CSD registers exactly one snapshot kind (flat schema): the snapshot apiVersion/kind live at the
// top level, the domain resource being snapshotted is referenced by Source.
type CustomSnapshotDefinitionSpec struct {
	// Weight orders universal traversal across snapshot kinds by ascending value: lower weights run
	// first (earlier traversal wave), like FlowSchema.spec.matchingPrecedence and the Deckhouse-native
	// NodeGroupConfiguration.spec.weight. It is NOT a Kubernetes PriorityClass value (where a higher
	// number wins) — the ordering here is the opposite.
	// +kubebuilder:validation:Minimum=0
	Weight int32 `json:"weight,omitempty"`
	// APIVersion is the apiVersion of the snapshot resource that materializes Source.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	APIVersion string `json:"apiVersion"`
	// Kind is the kind of the snapshot resource that materializes Source.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`
	// RequiresDataArtifact marks that this snapshot kind carries a volume data leg: the generic
	// controller must wait for the data artifact (capture, SnapshotContent.status.data.artifactRef) or the
	// matching DataImport (import) before it reports Ready. Manifest-only snapshot kinds (no volume
	// data) set false.
	RequiresDataArtifact bool `json:"requiresDataArtifact,omitempty"`
	// Source is the GVK of the domain resource being snapshotted.
	// +kubebuilder:validation:Required
	Source SnapshotGVKRef `json:"source"`
}

// +k8s:deepcopy-gen=true
type SnapshotGVKRef struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	APIVersion string `json:"apiVersion"`
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`
}

// +k8s:deepcopy-gen=true
type CustomSnapshotDefinitionStatus struct {
	// Conditions carry three distinct signals, each with a single writer:
	//   - Accepted — the spec is valid and registrable (written by the CSD reconciler);
	//   - AccessGranted — core-side RBAC for this CSD's GVRs applied (core SA + DataExport SA); written by
	//     the core 030-domain-rbac hook. The domain's own SA rights ship statically with the domain module
	//     and are NOT covered by this condition;
	//   - Ready — the aggregate Ready = Accepted && AccessGranted.
	// Accepted and Ready diverge when Accepted=True but AccessGranted=False (spec accepted, RBAC not yet
	// applied) → Ready=False; the printer columns surface all three so this is visible at a glance.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
