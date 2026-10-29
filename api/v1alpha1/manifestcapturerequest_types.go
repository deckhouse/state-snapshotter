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

// ManifestCaptureRequest asks for the state of the listed Kubernetes objects to be
// captured into a manifest checkpoint.

// Requests capture of the listed Kubernetes object manifests into a checkpoint.
// +kubebuilder:subresource:status
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=mcr
// +kubebuilder:metadata:labels=module=state-snapshotter
// +kubebuilder:printcolumn:name="Checkpoint",type=string,JSONPath=`.status.checkpointName`
// +crd-enricher:raw:properties.apiVersion.x-doc-skip=true
// +crd-enricher:raw:properties.kind.x-doc-skip=true
// +crd-enricher:raw:properties.metadata.x-doc-skip=true
type ManifestCaptureRequest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ManifestCaptureRequestSpec   `json:"spec"`
	Status ManifestCaptureRequestStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ManifestCaptureRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ManifestCaptureRequest `json:"items"`
}

// Defines the immutable set of object manifests to capture.
// +k8s:deepcopy-gen=true
// +kubebuilder:validation:XValidation:rule="has(self.targets) && size(self.targets) > 0",message="spec.targets must list at least one object to capture (at minimum the snapshotted object's own manifest)"
// +kubebuilder:validation:XValidation:rule="self.targets == oldSelf.targets",message="spec.targets is immutable: the capture plan is frozen once the ManifestCaptureRequest is created"
type ManifestCaptureRequestSpec struct {
	// Targets specifies the objects to capture. It MUST contain at least one target — at minimum the
	// snapshotted object's own manifest. A single-object domain snapshot passes its own source identity,
	// and the namespace-root aggregator always includes its own Namespace object, so a well-formed capture
	// is never empty. An empty (or omitted) target set is a contract violation: the SDK fails closed with
	// ErrEmptyManifest before creating the request, and the CRD rejects it via the spec-level CEL rule
	// above ("has(self.targets) && size(self.targets) > 0").
	// The set is the FROZEN point-in-time capture plan: it is IMMUTABLE once the request is created (the
	// spec-level CEL transition rule "self.targets == oldSelf.targets" rejects any change). The SDK creates
	// the request once and never patches it; a caller that recomputes a shifting set (e.g. the namespace
	// root over a live namespace) is silently frozen to the first plan.
	// All targets must be namespaced objects in the same namespace as the ManifestCaptureRequest, with a
	// single exception: the capture's own Namespace object (core v1 Namespace whose name equals the
	// ManifestCaptureRequest namespace) is the only allowed cluster-scoped target.

	// Objects whose manifests are captured into the checkpoint.
	//
	// All targets must be in the request namespace except the Namespace object for that namespace. The list is immutable and must contain at least one target.
	// +optional
	Targets []ManifestTarget `json:"targets,omitempty"`
}

// Reference to an object whose manifest is captured.
// +k8s:deepcopy-gen=true
type ManifestTarget struct {
	// APIVersion of the target object

	// API version of the target resource.
	APIVersion string `json:"apiVersion"`

	// Kind of the target object
	// Cluster-scoped resources (Node, PersistentVolume, ClusterRole, etc.) are NOT allowed, with a single
	// exception: the core v1 Namespace whose name equals the ManifestCaptureRequest namespace (the capture's
	// own Namespace object).

	// Kind of the target resource.
	Kind string `json:"kind"`

	// Name of the target object

	// Name of the target object.
	Name string `json:"name"`
	// Namespace is not specified, it's implied to be the same as ManifestCaptureRequest namespace
}

// Current checkpoint and completion state of the request.
// +k8s:deepcopy-gen=true
type ManifestCaptureRequestStatus struct {
	// CheckpointName is the name of the cluster-scoped ManifestCheckpoint

	// Name of the ManifestCheckpoint created for this request.
	CheckpointName string `json:"checkpointName,omitempty"`

	// CompletionTimestamp is when the request was completed

	// Time when processing finished.
	CompletionTimestamp *metav1.Time `json:"completionTimestamp,omitempty"`

	// Conditions represent the latest available observations of the request state
	// Only Ready condition is used - it is set to True on success or False on final failure
	// Ready condition is set only in final state (terminal success or terminal failure)

	// Current request conditions.
	// +crd-enricher:raw:items.description="Current condition."
	// +crd-enricher:raw:items.properties.lastTransitionTime.description="Time of the last condition status change."
	// +crd-enricher:raw:items.properties.message.description="Human-readable details about the condition."
	// +crd-enricher:raw:items.properties.observedGeneration.description="Object generation observed when this condition was set."
	// +crd-enricher:raw:items.properties.reason.description="Programmatic reason for the last condition transition."
	// +crd-enricher:raw:items.properties.status.description="Condition status."
	// +crd-enricher:raw:items.properties.type.description="Condition type."
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
