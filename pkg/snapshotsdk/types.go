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

package snapshotsdk

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk/internal/storagefoundation"
)

// SourceRef identifies the namespace-local source object a snapshot captures. It mirrors the generic
// spec.sourceRef contract; the namespace is implicit (the snapshot's namespace).
type SourceRef struct {
	APIVersion string
	Kind       string
	Name       string
}

// SnapshotChildRef identifies one child snapshot CR in the snapshot run tree. It is the durable record the
// SDK publishes (the set of children currently attached to the snapshot graph). This is the shared api
// contract type, re-exported so domain controllers and adapters reference a single definition.
type SnapshotChildRef = storagev1alpha1.SnapshotChildRef

// ExcludedObjectRef identifies one source object excluded from a snapshot — the shadow of SnapshotChildRef,
// but pointing at the vetoed SOURCE object rather than at a child snapshot. Re-exported api type so domain
// controllers and adapters reference a single definition.
type ExcludedObjectRef = storagev1alpha1.ExcludedObjectRef

// ExcludeLabelKey is the absolute snapshot veto label (re-exported from the api module — one source of
// truth). Any object carrying this key (value ignored) is excluded from every snapshot, at every level of
// the tree. Domain enumerators MUST partition their candidate
// source objects with PartitionExcluded: build children from kept, record excluded into
// DomainCaptureState.ExcludedRefs (published to status.captureState.domainSpecificController.excludedRefs).
const ExcludeLabelKey = storagev1alpha1.ExcludeLabelKey

// Target is the single PVC capture target of a snapshot's data leg. The domain resolves its own PVC
// (including readiness/ArtifactMissing decisions) and hands the SDK the result; the SDK turns it into the
// storage-foundation VolumeCaptureRequest.
type Target = storagefoundation.Target

// Reason is a stable, machine-readable condition reason published by the SDK on behalf of the domain. The
// domain chooses the reason (for example "InvalidSourceRef", "SourceNotFound", "ArtifactMissing"); the SDK
// never invents domain semantics.
type Reason string

// Phase is the domain-owned capture lifecycle carried on
// status.captureState.domainSpecificController.phase. Re-exported from the api module so domain
// controllers and adapters reference one definition.
type Phase = storagev1alpha1.SnapshotCapturePhase

// Phase values (re-exported).
const (
	PhasePlanning = storagev1alpha1.SnapshotCapturePhasePlanning
	PhasePlanned  = storagev1alpha1.SnapshotCapturePhasePlanned
	PhaseFinished = storagev1alpha1.SnapshotCapturePhaseFinished
	PhaseFailed   = storagev1alpha1.SnapshotCapturePhaseFailed
)

// SnapshotSource is the full reference to the captured live source object, published by the SDK into the
// top-level status.sourceRef. It is self-contained for import-mode recreation. Re-exported api type.
type SnapshotSource = storagev1alpha1.SnapshotSourceObjectRef

// DomainCaptureState is the durable, domain-owned planning result the SDK publishes into snapshot status
// under status.captureState.domainSpecificController: the names of the manifest- and volume-capture
// requests it created, the lifecycle phase/reason/message, and the top-level set of child snapshot refs.
// Adapters map it to and from the concrete snapshot status fields.
type DomainCaptureState struct {
	ManifestCaptureRequestName string
	VolumeCaptureRequestName   string
	ChildrenSnapshotRefs       []SnapshotChildRef
	// ExcludedRefs are this node's DIRECT exclusion vetoes: the source objects the domain dropped (via the
	// exclude label) while enumerating its children. The SDK publishes them into
	// status.captureState.domainSpecificController.excludedRefs as the INPUT the core folds into the durable
	// SnapshotContent aggregate. The domain provides them through EnsureChildren (alongside the kept
	// children); the SDK/adapter guarantees a non-nil slice on the wire (empty [] = "nothing excluded",
	// which a leaf always writes). The domain never authors the durable aggregate or the top-level mirror.
	ExcludedRefs []ExcludedObjectRef
	// Phase is the domain lifecycle barrier (Planning|Planned|Finished|Failed). Planned freezes
	// child/excluded membership and enables core projection. Regular domains publish MCR/VCR before it;
	// the current subtree-gated aggregator flow may publish its own MCR afterwards, once descendant
	// manifests are persisted and the complete exclusion set is available.
	Phase Phase
	// Reason is a machine-readable detail (progress codes while Planning; failure reason when Failed).
	Reason string
	// Message is a human-readable detail in any phase (progress or failure).
	Message string
}

// CoreCaptureState is the read-only, core-owned handoff (captureState.commonController leg latches) the
// SDK consults to suppress re-creating capture requests and to compute CoreCaptureOutcome. It is never
// written by the SDK. Each leg is a *bool success latch: nil = no such leg on this node; false = leg
// declared but not captured yet; true = captured.
type CoreCaptureState struct {
	// ManifestCaptured is the manifest-leg latch (declared on every capture node).
	ManifestCaptured *bool
	// DataCaptured is the data-leg latch (declared only where a data line exists).
	DataCaptured *bool
	// DataLegExpected says whether this KIND has a data line at all, which is the one thing the latches
	// cannot say for themselves. A nil DataCaptured is two different facts depending on it: on a
	// manifest-only node it means "there is no data leg here, and never will be", and on a data node it
	// means "the core has not declared it yet" — the same absence, opposite conclusions about whether the
	// capture is finished. The adapter knows which kind it maps and is the only one that can say.
	//
	// It defaults to false, which reads every nil as "no such leg" — the behaviour every adapter had before
	// this field existed. A domain whose kind carries a data line MUST set it; leaving it false there lets a
	// half-declared status pass for a finished capture, and a finished capture is never recomputed.
	DataLegExpected bool
	// ChildrenSettled is the core-computed "every direct child has gone terminal (captured-OK or failed)"
	// latch (captureState.commonController.childrenSettled). It is NOT a capture leg: it does NOT participate
	// in AllLegsCaptured or CoreCaptureOutcome (the subtree-scoped latches are orthogonal to this node's own
	// legs). It is the completeness signal a domain reads — orthogonal to success — to time a barrier-2 action
	// (e.g. fs unfreeze) that must fire even when a child data snapshot failed. nil = no direct children (leaf)
	// or not computed yet; true once every direct child is terminal.
	ChildrenSettled *bool
	// ChildSubtreesManifestsPersisted is the core-computed "the subtrees of ALL declared direct children are
	// fully persisted (every descendant durably archived its manifests)" latch
	// (captureState.commonController.childSubtreesManifestsPersisted). It excludes this node's own manifests,
	// so it can be true before this node creates its own MCR; a childless node is vacuously true. It is NOT a
	// capture leg: it does NOT participate in AllLegsCaptured or CoreCaptureOutcome. The SDK reads it as the
	// built-in pre-gate of SubtreeManifestIdentities (a non-nil false short-circuits to
	// ErrSubtreeIdentitiesPending without any REST call). nil = the adapter does not map it (pre-gate off,
	// backward compatible) or not computed yet.
	ChildSubtreesManifestsPersisted *bool
}

// manifestCaptured reports whether the manifest leg is latched captured. A nil latch is NOT captured, but
// it is not "not captured yet" either — it is "the core has not declared this leg", which no caller may
// answer by creating a request. Suppression therefore reads manifestLegOpen, never this; this one is for
// the outcome the domain waits on (AllLegsCaptured / CoreCaptureOutcome), where an undeclared leg is
// correctly not a captured one.
func (c CoreCaptureState) manifestCaptured() bool {
	return c.ManifestCaptured != nil && *c.ManifestCaptured
}

// dataCaptured reports whether the data leg is latched captured. Same caveat as manifestCaptured: a nil
// latch means "no such leg on this node, or not declared yet", not "go ahead and capture".
func (c CoreCaptureState) dataCaptured() bool { return c.DataCaptured != nil && *c.DataCaptured }

// manifestLegOpen reports whether a manifest capture request may be created: the core declared the leg
// (non-nil) and has not latched it captured. A nil latch is closed — an undeclared leg is one the core is
// about to declare false, or one whose captureState was replaced by a writer without the field and which
// the core will restore from the published artifact. Creating in that window is exactly the recapture this
// three-valued latch exists to prevent.
func (c CoreCaptureState) manifestLegOpen() bool {
	return c.ManifestCaptured != nil && !*c.ManifestCaptured
}

// dataLegOpen reports whether a volume capture request may be created. Symmetrical to manifestLegOpen, and
// a nil latch is closed for a second reason too: a node with no data line at all carries nil forever, so
// "wait for the declaration" is also the correct answer for it.
func (c CoreCaptureState) dataLegOpen() bool {
	return c.DataCaptured != nil && !*c.DataCaptured
}

// childrenSettled reports whether every direct child has gone terminal (captured-OK or failed). A nil latch
// (no direct children, or not computed yet) reads as false. It is deliberately NOT consulted by
// AllLegsCaptured: childrenSettled is a subtree completeness signal, not a leg of THIS node's capture.
func (c CoreCaptureState) childrenSettled() bool {
	return c.ChildrenSettled != nil && *c.ChildrenSettled
}

// AllLegsCaptured reports whether this node's capture is complete: every leg this kind has, latched
// captured. It is what a domain waits on before its terminal phase, and a terminal phase is never
// recomputed — so every honest doubt here has to answer "not complete".
//
// Both legs are required to be DECLARED, not merely non-false, and an absent latch is never read as a leg
// that does not exist:
//
//   - the manifest leg exists on every capture node, so a nil latch only ever means the core has not
//     written it yet;
//   - the data leg exists where DataLegExpected says it does, and there a nil latch means the same thing.
//     Where it does not, nil is the steady state of a manifest-only node and says nothing is owed.
//
// Both readings are load-bearing and both were wrong before. Reading a nil manifest latch as "nothing to
// wait for" deadlocked a snapshot outright: the domain went terminal in the window before the core declared
// the leg, so it never created the capture request the core then asked for, and the content sat at
// ManifestCapturePending with no writer left to move it. Reading a nil data latch the same way on a data
// node ends the capture one leg early — the core declares both legs in one write, but nothing guarantees an
// apiserver or a foreign writer keeps both halves of it, and half of that write is exactly this state.
//
// It answers only about THIS node's own legs: the subtree-scoped latches (childrenSettled,
// childSubtreesManifestsPersisted) are deliberately not consulted.
func (c CoreCaptureState) AllLegsCaptured() bool {
	if !c.manifestCaptured() {
		return false
	}
	if c.DataLegExpected {
		return c.dataCaptured()
	}
	return c.DataCaptured == nil || *c.DataCaptured
}

// ChildSpec is the child-builder seam: the domain constructs the fully-formed child snapshot object
// (kind, name, spec.sourceRef, labels) and hands it to the SDK, which owns the tree membership (owner
// reference and markers), create-or-validate, and SnapshotChildRef derivation. The SDK never authors domain
// child spec fields.
type ChildSpec struct {
	// Object is the desired child snapshot object, built by the domain. The SDK derives its
	// SnapshotChildRef from the object's GVK and name and stamps the parent owner reference on it.
	Object client.Object
}

// CaptureLegDecision is the three-valued answer to "what should this domain do about a capture leg right
// now". It exists because the leg latch itself is three-valued (nil / false / true) and collapsing it to a
// bool loses the one distinction that matters for a set-once artifact: "the core has not declared this leg
// yet" is not the same as "the leg is already established". A domain that reads the first as the second
// creates a second capture request — which, for a snapshot whose status was replaced by a writer without
// extension fields, means capturing again from a cluster that has moved on.
type CaptureLegDecision int

const (
	// CaptureLegWaitDeclaration: the core has not declared the leg yet (nil latch). Do NOT create a request
	// and do NOT treat the leg as done — wait for the declaration. This is also what a snapshot whose
	// captureState was wiped looks like until the core restores it from the published artifact.
	CaptureLegWaitDeclaration CaptureLegDecision = iota
	// CaptureLegPlan: the core declared the leg and nothing has been captured or planned yet (latch false,
	// no request name published). This is the only state in which a request may be created.
	CaptureLegPlan
	// CaptureLegSuppressed: the leg is already established — the request name is published (the
	// point-in-time plan is frozen) or the latch is captured. Never create; a caller that skips expensive
	// target computation on this is correct.
	CaptureLegSuppressed
)

// CaptureOutcome is the tri-state the SDK derives for the domain from the core's leg latches plus the
// terminal Ready reason. The domain switches its wait loop on it: Captured -> publish phase=Finished via
// DomainCaptureStatus; Failed -> stop (the core owns and bubbles the terminal Ready, but the domain must
// still compensate its consistency action, e.g. fs unfreeze); Capturing -> wait.
type CaptureOutcome int

const (
	// CaptureOutcomeCapturing: the core is still capturing (some declared leg not captured, no terminal Ready).
	CaptureOutcomeCapturing CaptureOutcome = iota
	// CaptureOutcomeCaptured: every declared leg is captured and Ready is not terminal.
	CaptureOutcomeCaptured
	// CaptureOutcomeFailed: the core surfaced a terminal Ready reason (own manifests/volumes or child failure).
	CaptureOutcomeFailed
)

// CaptureOutcomeResult carries the tri-state plus, for Failed, the terminal Ready reason/message.
type CaptureOutcomeResult struct {
	Outcome CaptureOutcome
	Reason  string
	Message string
}

// ChildCaptureState is the read-only per-child view the domain inspects via ChildrenCaptureStates —
// diagnostics/inspection only. It MUST NOT be used to gate a consistency action: looping on each child's
// terminal Ready reason hangs forever on a child that fails with a domain free-form reason (not in
// TerminalReadyReasons). Gate the consistency action on this node's own CoreCaptureState().ChildrenSettled
// / CoreCaptureOutcome (or a domain freeze-deadline) instead — see CaptureOutcome and ChildrenSettled.
type ChildCaptureState struct {
	// Ref is the child snapshot's published ref (status.childrenSnapshotRefs entry).
	Ref SnapshotChildRef
	// ReadyStatus / ReadyReason / ReadyMessage are the child's core-written status.conditions[Ready].
	// ReadyStatus is empty ("") when the child has no Ready condition yet (or is not found).
	ReadyStatus  metav1.ConditionStatus
	ReadyReason  string
	ReadyMessage string
	// AllLegsCaptured reports whether every declared (non-nil) capture leg of the child is latched captured
	// (status.captureState.commonController). It is false for a not-yet-found or not-yet-latched child.
	AllLegsCaptured bool
}

// VolumeCaptureSpec is the domain's data-leg intent: the single PVC to capture. A snapshot node binds at
// most one data artifact (cardinality ≤1, see api/storage/v1alpha1 SnapshotContent.dataRef):
// multiple volumes are modeled as child snapshot nodes, never as several data refs on one node. A nil
// DataRef means the snapshot is manifest-only — the SDK ensures no VolumeCaptureRequest and publishes no
// name.
type VolumeCaptureSpec struct {
	// DataRef is the snapshot's single data-leg PVC, or nil for a manifest-only snapshot.
	DataRef *Target
}

// ManifestTarget is one manifest capture target — a source object identity (apiVersion/kind/name; the
// namespace is implicit, equal to the request's namespace). Re-exported api type so domain controllers,
// adapters, and the SDK reference a single definition.
type ManifestTarget = ssv1alpha1.ManifestTarget

// ManifestCaptureSpec is the domain's manifest-leg intent: the COMPLETE set of manifest targets (source
// object identities) whose YAML this snapshot captures. A single-object domain (e.g. a disk/VM snapshot)
// passes its own object; an aggregator (e.g. the namespace-root Snapshot) passes the full namespace target
// set. The set is authoritative and self-contained: the SDK captures EXACTLY these targets (deduplicated
// and sorted deterministically) and never derives or injects targets from the data leg. If a snapshot also
// captures a PVC's data (VolumeCaptureSpec.DataRef) and wants that PVC's YAML restored, the domain lists
// the PVC here explicitly — the manifest and data legs are two independent declarations.
type ManifestCaptureSpec struct {
	// Targets are the manifest capture targets. Non-empty for a manifest-capturing snapshot.
	Targets []ManifestTarget
}
