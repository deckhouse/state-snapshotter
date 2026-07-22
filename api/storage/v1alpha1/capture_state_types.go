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
	"k8s.io/apimachinery/pkg/types"
)

// SnapshotCapturePhase is the domain-owned lifecycle of a capture node, carried on
// status.captureState.domainSpecificController.phase. It is the internal contract between the domain
// snapshot controller and the core planner; users never read it (they read the aggregated Ready
// condition). One enum replaces the former PlanningReady/Consistent conditions and the standalone
// failure field.
//
// +kubebuilder:validation:Enum=Planning;Planned;Finished;Failed
type SnapshotCapturePhase string

const (
	// SnapshotCapturePhasePlanning: the domain controller is creating objects/refs (children, MCR/VCR).
	SnapshotCapturePhasePlanning SnapshotCapturePhase = "Planning"
	// SnapshotCapturePhasePlanned: barrier 1 — all objects created and refs published (children + MCR/VCR).
	// The core planner expands the graph and the binder takes over the content.
	SnapshotCapturePhasePlanned SnapshotCapturePhase = "Planned"
	// SnapshotCapturePhaseFinished: barrier 2 — the domain finished its side, including consistency
	// actions (e.g. fs unfreeze). The core finalizes the aggregate Ready.
	SnapshotCapturePhaseFinished SnapshotCapturePhase = "Finished"
	// SnapshotCapturePhaseFailed: the domain hit a terminal error; reason/message carry the detail and
	// the core bubbles it into the user-facing Ready.
	SnapshotCapturePhaseFailed SnapshotCapturePhase = "Failed"
)

// CaptureStateStatus is the umbrella for internal capture signals on a snapshot object. It has two
// sub-structures split strictly by writer, so each is patched independently by exactly one controller
// (nobody replaces the whole captureState): commonController is written by the core, domainSpecificController
// by the domain (via the SDK). It is present only in Capture mode (absent on Import).
// +k8s:deepcopy-gen=true
type CaptureStateStatus struct {
	// CommonController holds the core-written capture-leg success latches. Single writer: core.
	// +optional
	CommonController *CommonControllerCaptureState `json:"commonController,omitempty"`

	// DomainSpecificController holds the domain-written planning refs and lifecycle. Single writer: domain (SDK).
	// +optional
	DomainSpecificController *DomainSpecificControllerCaptureState `json:"domainSpecificController,omitempty"`
}

// CommonControllerCaptureState is the core-written half of captureState: per-leg success latches. The
// core eagerly initializes the applicable legs to false (the field's presence declares the leg; nil
// means "no such leg"), then monotonically flips them to true as each leg is captured. Success-only:
// a capture failure is NOT written here — it surfaces as a terminal Ready reason (IsReasonTerminal).
// There is no rollup field; "all legs captured" is computed by the SDK over the declared legs.
// It also carries the non-leg ChildSubtreesManifestsPersisted children-only latch (a manifest-exclude
// pre-gate), see below.
// +k8s:deepcopy-gen=true
type CommonControllerCaptureState struct {
	// ManifestCaptured is the manifest-leg success latch (declared on every capture node). nil = no leg;
	// false = declared, not captured yet; true = captured. On the root Snapshot the RBAC hook reads it.
	// +optional
	ManifestCaptured *bool `json:"manifestCaptured,omitempty"`

	// DataCaptured is the data-leg success latch, declared only where a data line exists (absent = nil on
	// nodes without data, e.g. the namespace root or a manifest-only aggregate). The parent reads a
	// child's dataCaptured to time freeze/unfreeze.
	// +optional
	DataCaptured *bool `json:"dataCaptured,omitempty"`

	// ChildSubtreesManifestsPersisted records that the subtrees of ALL declared direct children of this node
	// are fully persisted — every descendant node beneath each direct child has durably archived its
	// manifests. It deliberately EXCLUDES this node's OWN manifests (those are tracked by ManifestCaptured):
	// "the whole node is persisted" decomposes as ManifestCaptured && ChildSubtreesManifestsPersisted.
	// Because it does not depend on this node's own manifest leg, it can flip true BEFORE this node creates
	// its own MCR — which is exactly what makes it usable as a manifest-exclude pre-gate. A childless node is
	// vacuously persisted (true). It is NOT a capture leg: it is NOT part of CoreCaptureOutcome, and it is
	// distinct from ManifestCaptured (which the root RBAC hook reads). Its purpose is a cheap namespaced
	// pre-gate for the SDK manifest-exclude computation (SubtreeManifestIdentities): an aggregator reads its
	// OWN latch to decide when to attempt building its own MCR (base - exclude) without hitting the
	// subtree-manifest-identities subresource while descendants are still capturing. The gate is best-effort:
	// that subresource is fail-closed (409 while any subtree MCP is not Ready), so correctness holds even
	// when the latch is absent. Core-computed from the direct children's content latches (declared-vs-linked
	// fail-closed; a leaf is vacuously true). The core eager-declares it false at capture barrier 1 and
	// monotonically flips it to true once the children's subtrees persist (a childless node latches true on
	// the first pass) — it is deliberately NOT left nil past barrier 1, since a nil field disables the SDK
	// pre-gate (nil = pre-gate off). Written sideways onto the snapshot. nil = not yet computed / no bound
	// content, or an adapter that does not map it (SDK pre-gate off, backward compatible). Monotonic (false -> true).
	// +optional
	ChildSubtreesManifestsPersisted *bool `json:"childSubtreesManifestsPersisted,omitempty"`

	// SubtreePlanned is a core-computed monotonic recursive latch: true once THIS node reached capture
	// barrier 1 (domainSpecificController.phase >= Planned) AND every DIRECT child's own SubtreePlanned is
	// true — i.e. the whole subtree has finished planning (objects + refs published, not necessarily
	// captured/Ready). It is snapshot-native and main-written (the aggregator computes it by resolving the
	// owner's direct children from status.childrenSnapshotRefs and reading each child's latch, then writes
	// it SIDEWAYS onto this snapshot; content has no phase, so this cannot live on the SnapshotContent).
	// The root's orphan/residual-PVC wave gates on its direct children's SubtreePlanned so an orphan PVC is
	// evaluated only once every declared subtree's coverage is fully computable (no premature root exclude).
	// It is NOT a capture leg (not part of CoreCaptureOutcome). Domains only READ it. nil = subtree not
	// planned yet. Monotonic (nil/false -> true; the spec is immutable, so no recapture flips it back).
	// +optional
	SubtreePlanned *bool `json:"subtreePlanned,omitempty"`

	// ChildrenSettled is a core-computed monotonic latch: true once EVERY DIRECT child of THIS node has gone
	// terminal — terminal meaning captured-OK OR failed. Formally childrenSettled(n) = for every direct child
	// c: isTerminal(c), where isTerminal(c) = c.Ready==True OR c.phase in {Finished,Failed} OR
	// IsReasonTerminal(c.Ready). It is, in effect, ChildrenReady with a terminal child FAILURE also counted as
	// "settled": a completeness signal ORTHOGONAL to success, not a success aggregate. (Ready cannot serve
	// this role — its terminal False collapses "one child failed, others still in flight" and "one failed, all
	// done" into an indistinguishable False, and a parent's own Ready=True is derived only AFTER its domain
	// reaches phase=Finished, a circular dependency for a consumer that must act BEFORE finishing.)
	//
	// It is snapshot-native and main-written, computed by resolving the owner's DIRECT children from
	// status.childrenSnapshotRefs and reading each child's Ready condition AND domain phase, then written
	// SIDEWAYS onto this snapshot (like SubtreePlanned; a SnapshotContent has no phase, so this cannot live on
	// the content). The child's domain phase=Failed is read DIRECTLY (not inferred from a terminal Ready
	// reason): the core bubbles a domain's free-form phase=Failed reason VERBATIM onto the child Ready, and a
	// free-form reason is not in TerminalReadyReasons, so IsReasonTerminal alone would miss a domain failure.
	//
	// It uses the same fail-closed-until-frozen machinery as ChildrenReady: the owner's direct-child set is
	// frozen at capture barrier 1 (phase >= Planned), and a declared-but-not-yet-created child is treated as
	// not-terminal, so the latch never flips true over an incomplete child set. It is NOT a capture leg (not
	// part of CoreCaptureOutcome and not counted by AllLegsCaptured). Domains only READ it — e.g. a domain
	// gates a consistency action (fs unfreeze) on childrenSettled==true, which must fire even when a child
	// data snapshot failed; a child that HANGS (never terminal) is covered by a domain-side deadline, not by
	// this latch. nil = no direct children (leaf) or not computed yet. Monotonic (nil/false -> true; the spec
	// is immutable, so no recapture flips it back).
	// +optional
	ChildrenSettled *bool `json:"childrenSettled,omitempty"`
}

// DomainSpecificControllerCaptureState is the domain-written half of captureState: execution-request
// handles plus the lifecycle phase and failure detail. Single writer: the domain controller via the SDK.
// +k8s:deepcopy-gen=true
type DomainSpecificControllerCaptureState struct {
	// ManifestCaptureRequestName is the temporary MCR owned by the domain node while own-scope capture runs.
	// +optional
	ManifestCaptureRequestName string `json:"manifestCaptureRequestName,omitempty"`

	// VolumeCaptureRequestName is the temporary VCR owned by a data-leaf domain node while the data leg runs.
	// +optional
	VolumeCaptureRequestName string `json:"volumeCaptureRequestName,omitempty"`

	// Phase is the domain lifecycle barrier (Planning|Planned|Finished|Failed).
	// +optional
	Phase SnapshotCapturePhase `json:"phase,omitempty"`

	// Reason is a short, machine-readable reason for Phase=Failed (free-form domain string).
	// +optional
	Reason string `json:"reason,omitempty"`

	// Message is a human-readable detail for Phase=Failed.
	// +optional
	Message string `json:"message,omitempty"`

	// ExcludedRefs are the domain's DIRECT exclusion vetoes at this node: the source objects it dropped
	// (via the exclude label) while enumerating its children. It is the transient INPUT the core reads and
	// folds into the durable SnapshotContent.status.excludedRefs aggregate; the domain never writes the
	// aggregate or the top-level mirror (both are core-owned).
	//
	// Written WITHOUT omitempty: an empty list ([]) means "domain planned, nothing excluded" and MUST be
	// distinguishable from "domain has not planned yet" (domainSpecificController absent). A data-leaf
	// (e.g. a disk) never enumerates children, so it always writes []. Absent in Import
	// (no live capture happens).
	// +optional
	// +listType=atomic
	ExcludedRefs []ExcludedObjectRef `json:"excludedRefs"`
}

// SnapshotSourceObjectRef is the full reference to the live source object a snapshot captured, carried
// on top-level status.sourceRef. It is written by the domain controller (PublishSnapshotSource) and
// is self-contained for import-mode recreation (d8-cli reads it as a single block, without joining
// spec.sourceRef and a separate uid). On the namespace-root Snapshot it references the captured Namespace
// (kind=Namespace), written by the in-process namespace-domain.
// +k8s:deepcopy-gen=true
type SnapshotSourceObjectRef struct {
	// +kubebuilder:validation:MinLength=1
	APIVersion string `json:"apiVersion"`
	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// Namespace of the source object (namespaced sources only).
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// UID of the captured live source object (best-effort; used by d8-cli for import-mode recreation).
	// +optional
	UID types.UID `json:"uid,omitempty"`
}
