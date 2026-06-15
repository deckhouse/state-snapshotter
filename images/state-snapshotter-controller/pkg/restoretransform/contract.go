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

// Package restoretransform is the importable wire contract for the domain restore-transform API
// (ADR 2026-06-13, PoC transport). It is the single thing a domain controller needs to participate in
// restore over HTTP: request/response DTOs, the endpoint path, the contract version and the endpoint
// env name. It has zero dependencies and references no generic-restore or domain internals, so an
// external/separate domain module can import it without pulling implementation packages.
//
// Layering:
//   - pkg/restoretransform           — the importable wire contract (this package).
//   - internal/usecase/restore       — generic implementation detail (REST client, DomainRestoreTransformer
//     adapter, RestoreNode integration); the contract's consumer, not its owner.
//   - internal/controllers/demo      — PoC domain implementation (HTTP handler/server) of the contract.
//
// The generic compiler owns the object set, sanitize, ordering and dedup; the domain only proposes a
// transformed object + suppress intent for objects it owns. This is a PoC, NOT a production Kubernetes
// APIService.
package restoretransform

// Version is the envelope version. Evolution is additive; breaking changes bump it.
const Version = "v1alpha1"

// EndpointPath is the canonical HTTP path a domain controller exposes for the PoC transport:
//
//	POST /apis/restore.state-snapshotter.deckhouse.io/v1alpha1/transform
const EndpointPath = "/apis/restore.state-snapshotter.deckhouse.io/" + Version + "/transform"

// EnvEndpoint, when set to a full http(s) URL, makes the generic restore compiler delegate domain
// restore transforms to that endpoint instead of the in-process transformer. It is the generic REST
// client's configuration; the matching endpoint is owned and served by the domain controller.
const EnvEndpoint = "RESTORE_TRANSFORM_ENDPOINT"

// ObjectRef identifies a Kubernetes object by GVK + namespace/name (transport-neutral).
type ObjectRef struct {
	APIVersion string `json:"apiVersion,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name,omitempty"`
}

// NodeRef carries the run-tree node identity a domain transform needs: the snapshot CR that owns this
// node (e.g. a per-disk snapshot), so the domain can point a restored object at its snapshot.
type NodeRef struct {
	SnapshotRef ObjectRef `json:"snapshotRef"`
}

// RestoreContext carries source/target namespaces of the restore.
type RestoreContext struct {
	SourceNamespace string `json:"sourceNamespace,omitempty"`
	TargetNamespace string `json:"targetNamespace,omitempty"`
}

// Status is the machine-readable failure reason when allowed=false.
type Status struct {
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// ChildNode is one child node passed as read-only context: its owning snapshot CR and its already-
// compiled restore-ready objects. The domain MUST NOT mutate children; they are context only, so a
// parent domain object can reference its restored children.
type ChildNode struct {
	SnapshotRef ObjectRef                `json:"snapshotRef,omitempty"`
	Objects     []map[string]interface{} `json:"objects,omitempty"`
}

// Request is the per-object transform request. object is the already-sanitized unstructured content
// (its raw .Object map). children is read-only context (compiled child objects).
//
// uid is an opaque per-call correlation token: the domain MUST echo it back unchanged and MUST NOT
// parse or derive meaning from it. It carries no object identity (that is in targetRef).
type Request struct {
	UID            string                 `json:"uid"`
	TargetRef      ObjectRef              `json:"targetRef"`
	Node           NodeRef                `json:"node"`
	RestoreContext RestoreContext         `json:"restoreContext"`
	Object         map[string]interface{} `json:"object"`
	DataRefs       []ObjectRef            `json:"dataRefs,omitempty"`
	Children       []ChildNode            `json:"children,omitempty"`
}

// Response is the domain answer. For the PoC the domain returns a full transformed object (not a
// patch). suppressRefs declares objects of this node the domain recreates on restore (PoC:
// PersistentVolumeClaim only).
type Response struct {
	UID          string                 `json:"uid"`
	Handled      bool                   `json:"handled"`
	Allowed      bool                   `json:"allowed"`
	Object       map[string]interface{} `json:"object,omitempty"`
	SuppressRefs []ObjectRef            `json:"suppressRefs,omitempty"`
	Warnings     []string               `json:"warnings,omitempty"`
	Status       *Status                `json:"status,omitempty"`
}

// RequestEnvelope / ResponseEnvelope wrap the request/response under a single key, matching the
// AdmissionReview-like shape ({"request": {...}} / {"response": {...}}).
type RequestEnvelope struct {
	Request Request `json:"request"`
}

type ResponseEnvelope struct {
	Response Response `json:"response"`
}
