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

// CustomSnapshotDefinition registers a custom snapshot type provided by a platform module:
// it maps a source resource kind to the snapshot kind that materializes it.

// Registers a snapshot kind provided by another Deckhouse Platform module for a source resource kind.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=csd
// +kubebuilder:metadata:labels=module=state-snapshotter
// +kubebuilder:printcolumn:name="Weight",type=integer,JSONPath=`.spec.weight`
// +kubebuilder:printcolumn:name="Accepted",type=string,JSONPath=`.status.conditions[?(@.type=="Accepted")].status`
// +kubebuilder:printcolumn:name="AccessGranted",type=string,JSONPath=`.status.conditions[?(@.type=="AccessGranted")].status`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +crd-enricher:raw:properties.apiVersion.x-doc-skip=true
// +crd-enricher:raw:properties.kind.x-doc-skip=true
// +crd-enricher:raw:properties.metadata.x-doc-skip=true
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

// CustomSnapshotDefinitionSpec maps one source resource GVK to the snapshot GVK that materializes it.
// One CSD registers exactly one snapshot kind (flat schema): the snapshot apiVersion/kind live at the
// top level, the domain resource being snapshotted is referenced by Source.

// Maps one source resource kind to the snapshot kind that handles it.
// +k8s:deepcopy-gen=true
type CustomSnapshotDefinitionSpec struct {
	// Weight orders universal traversal across snapshot kinds by ascending value: lower weights run
	// first (earlier traversal wave), like FlowSchema.spec.matchingPrecedence and the Deckhouse-native
	// NodeGroupConfiguration.spec.weight. It is NOT a Kubernetes PriorityClass value (where a higher
	// number wins) — the ordering here is the opposite.

	// Traversal order for this snapshot kind. Lower values are processed earlier.
	// +kubebuilder:validation:Minimum=0
	Weight int32 `json:"weight,omitempty"`

	// APIVersion is the apiVersion of the snapshot resource that materializes Source.

	// API version of the snapshot resource.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	APIVersion string `json:"apiVersion"`

	// Kind is the kind of the snapshot resource that materializes Source.

	// Kind of the snapshot resource.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`

	// RequiresDataArtifact marks that this snapshot kind carries a volume data leg: the generic
	// controller must wait for the data artifact (capture, SnapshotContent.status.data.artifactRef) or the
	// matching DataImport (import) before it reports Ready. Manifest-only snapshot kinds (no volume
	// data) set false.

	// Shows whether this snapshot kind requires a captured volume-data artifact before it can become ready.
	RequiresDataArtifact bool `json:"requiresDataArtifact,omitempty"`

	// Source is the GVK of the domain resource being snapshotted.

	// Source Kubernetes resource kind handled by this definition.
	// +kubebuilder:validation:Required
	Source SnapshotGVKRef `json:"source"`
}

// Identifies a Kubernetes resource kind.
// +k8s:deepcopy-gen=true
type SnapshotGVKRef struct {
	// API version of the source resource.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	APIVersion string `json:"apiVersion"`
	// Kind of the source resource.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`
}

// Current acceptance, access, and readiness state of the definition.
// +k8s:deepcopy-gen=true
type CustomSnapshotDefinitionStatus struct {
	// Conditions carry three distinct signals, each with a single writer:
	//   - Accepted — the spec is valid and registrable (written by the CSD reconciler);
	//   - AccessGranted — core-side RBAC for this CSD's GVRs applied (the core controller SA, the
	//     DataExport SA, and the webhooks SA — the latter read-only on the source GVRs, for validating
	//     capture-request targets); written by the core 030-domain-rbac hook. The domain's own SA rights
	//     ship statically with the domain module and are NOT covered by this condition;
	//   - Ready — the aggregate Ready = Accepted && AccessGranted.
	// Accepted and Ready diverge when Accepted=True but AccessGranted=False (spec accepted, RBAC not yet
	// applied) → Ready=False; the printer columns surface all three so this is visible at a glance.

	// Current conditions. Accepted reflects definition validation, AccessGranted reflects required access, and Ready reflects overall readiness.
	// +crd-enricher:raw:items.description="Current condition."
	// +crd-enricher:raw:items.properties.lastTransitionTime.description="Time of the last condition status change."
	// +crd-enricher:raw:items.properties.message.description="Human-readable details about the condition."
	// +crd-enricher:raw:items.properties.observedGeneration.description="Object generation observed when this condition was set."
	// +crd-enricher:raw:items.properties.reason.description="Programmatic reason for the last condition transition."
	// +crd-enricher:raw:items.properties.status.description="Condition status."
	// +crd-enricher:raw:items.properties.type.description="Condition type."
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
