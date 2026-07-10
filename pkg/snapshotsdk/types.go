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
// the tree, independently of spec.resourceSelector. Domain enumerators MUST partition their candidate
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
	// Phase is the domain lifecycle barrier (Planning|Planned|Finished|Failed).
	Phase Phase
	// Reason/Message carry the failure detail when Phase=Failed.
	Reason  string
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
}

// manifestCaptured reports whether the manifest leg is captured (nil latch => not captured).
func (c CoreCaptureState) manifestCaptured() bool {
	return c.ManifestCaptured != nil && *c.ManifestCaptured
}

// dataCaptured reports whether the data leg is captured (nil latch => not captured).
func (c CoreCaptureState) dataCaptured() bool { return c.DataCaptured != nil && *c.DataCaptured }

// AllLegsCaptured reports whether every declared (non-nil) leg is captured. It requires at least one
// declared leg: with no leg declared yet (both nil) capture has not started, so it returns false — this
// distinguishes "nothing to wait for" (never happens; the core always declares the manifest leg) from
// "not started yet". The core eager-initializes applicable legs to false, so the domain stays leg-agnostic.
func (c CoreCaptureState) AllLegsCaptured() bool {
	declared := false
	if c.ManifestCaptured != nil {
		declared = true
		if !*c.ManifestCaptured {
			return false
		}
	}
	if c.DataCaptured != nil {
		declared = true
		if !*c.DataCaptured {
			return false
		}
	}
	return declared
}

// ChildSpec is the child-builder seam: the domain constructs the fully-formed child snapshot object
// (kind, name, spec.sourceRef, labels) and hands it to the SDK, which owns adoption (owner reference),
// create-or-validate, and SnapshotChildRef derivation. The SDK never authors domain child spec fields.
type ChildSpec struct {
	// Object is the desired child snapshot object, built by the domain. The SDK derives its
	// SnapshotChildRef from the object's GVK and name and stamps the parent owner reference on it.
	Object client.Object
}

// FailSpec describes a Phase=Failed outcome the domain wants published (invalid source, missing
// artifact, …). It generalizes the various failure paths into one verb (Reject). The failure surfaces to
// users through the core-derived Ready (the core mirrors domain phase=Failed into Ready=False).
type FailSpec struct {
	// Reason is the machine-readable failure reason (domain-chosen), stored on
	// captureState.domainSpecificController.reason.
	Reason Reason
	// Message is an optional human-readable explanation.
	Message string
	// Cause, when set, is logged/returned so the manager can surface the underlying error.
	Cause error
	// Requeue asks the caller to requeue (for example, an artifact that may appear later). When false the
	// outcome is treated as terminal-until-spec-change and the SDK returns no error and no requeue intent.
	Requeue bool
}

// CaptureOutcome is the tri-state the SDK derives for the domain from the core's leg latches plus the
// terminal Ready reason. The domain switches its wait loop on it (Captured -> ConfirmConsistent,
// Failed -> Fail/Reject, Capturing -> wait).
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

// ChildCaptureState is the read-only view of one declared child snapshot the domain inspects (via
// ChildrenCaptureStates) to build its own Finished/wait/stop logic — for example a VM aggregator that
// confirms consistency only once every child disk's data leg is latched, and that stops waiting once a
// child has gone terminal (the core owns and bubbles that terminal; the domain must not re-drive it).
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
// most one data artifact (Variant A, cardinality ≤1, see api/storage/v1alpha1 SnapshotContent.dataRef):
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

// ManifestCaptureSpec is the domain's manifest-leg intent: the SET of base manifest targets (source
// object identities). A single-object domain (e.g. a disk/VM snapshot) passes exactly one target; an
// aggregator that captures many objects at once (e.g. the namespace-root Snapshot, whose manifest leg is
// the whole namespace target set) passes the full set. The SDK merges any owned-PVC targets discovered
// from the data-leg VolumeCaptureRequest, then deduplicates and sorts deterministically.
type ManifestCaptureSpec struct {
	// Targets are the base manifest capture targets. Non-empty for a manifest-capturing snapshot.
	Targets []ManifestTarget
}
