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
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk/internal/children"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk/internal/manifest"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk/internal/patch"
)

// Planning drives the three idempotent, restart-safe planning legs of a snapshot: its child snapshots, its
// data-leg volume capture, and its manifest capture. Each method reconciles the cluster toward the desired
// intent and publishes the resulting names/refs into the snapshot status.
type Planning interface {
	// EnsureChildren creates/adopts the desired child snapshots (each under this snapshot) and ADDITIVELY
	// publishes their refs into status.childrenSnapshotRefs — a union, never a replace (wave5): the freshly
	// derived refs are UNIONED into the currently published set, so refs contributed by a co-writer of the
	// same field that this pass does not itself enumerate (the namespace root's orphan VolumeSnapshot wave,
	// §6.2) are preserved. It performs create/adopt + publication only and never deletes children (SDK v1
	// is delete-free): a nil or empty desired set therefore publishes NO new refs and leaves the currently
	// published set intact. A child no longer desired is simply not re-added by its emitter and is left in
	// the cluster for ownerRef GC / a future cleanup component to reclaim.
	//
	// The declared set is FROZEN once the node declares barrier 1: at phase>=Planned (and at the terminal
	// Failed) EnsureChildren rejects any GROWTH of the set (or change of the excluded set) with
	// ErrChildrenSetFrozen — fail-closed and BEFORE any child CR is created, so a rejected call has no side
	// effects. An idempotent re-publish of the same set (desired ⊆ published, excluded unchanged) stays a
	// no-op at any phase. The freeze mirrors the immutable SnapshotContent.childrenSnapshotContentRefs (a
	// late-added edge would be rejected by that CEL and wedge the node at ChildrenLinkPending); the
	// recommended domain reaction to the error is sdk.Fail(GraphPlanningFailed).
	//
	// excluded is the domain's DIRECT exclusion vetoes at this node — the source objects it dropped (via
	// the exclude label) while enumerating children, obtained from PartitionExcluded. It is published in
	// the same status patch as the kept children, into
	// status.captureState.domainSpecificController.excludedRefs (the INPUT the core aggregates). The SDK
	// does NOT compute the veto (it sees built child specs, not source labels): the domain partitions and
	// hands both halves here. Pass nil when nothing is excluded; the wire value is normalized to [].
	EnsureChildren(ctx context.Context, t SnapshotAdapter, desired []ChildSpec, excluded []ExcludedObjectRef) error

	// EnsureVolumeCapture ensures the data-leg capture request for the snapshot's single PVC (VolumeCaptureSpec.DataRef)
	// and publishes its name. A nil DataRef is a manifest-only snapshot (no request, no published name). The
	// operation is suppressed once the core controller has stamped the data leg captured. It depends ONLY on
	// VolumeCaptureSpec and never reads the manifest leg.
	EnsureVolumeCapture(ctx context.Context, t SnapshotAdapter, in VolumeCaptureSpec) error

	// EnsureManifestCapture ensures the per-snapshot ManifestCaptureRequest from the domain's declared
	// target SET (ManifestCaptureSpec.Targets) and publishes its name. It depends ONLY on ManifestCaptureSpec:
	// the SDK never reads the data-leg VCR to derive or inject targets, so EnsureManifestCapture and
	// EnsureVolumeCapture are two independent declarations whose call order does not affect the result. A
	// domain that wants a PVC's YAML captured lists that PVC in Targets explicitly. The operation is
	// suppressed once the core controller has stamped the manifest leg captured.
	EnsureManifestCapture(ctx context.Context, t SnapshotAdapter, in ManifestCaptureSpec) error
}

// CaptureBarrier publishes the domain lifecycle phase the core controller reads to sequence its work
// (barrier 1 = Planned, barrier 2 = Finished). The SDK writes only
// status.captureState.domainSpecificController.phase; it never writes the Ready condition.
type CaptureBarrier interface {
	// MarkPlanned declares barrier 1 satisfied: all objects created and refs published (children + MCR/VCR).
	// Sets phase=Planned.
	MarkPlanned(ctx context.Context, t SnapshotAdapter) error
	// ConfirmConsistent declares barrier 2 satisfied: the domain finished its side including consistency
	// actions (e.g. fs unfreeze). Sets phase=Finished.
	ConfirmConsistent(ctx context.Context, t SnapshotAdapter) error
}

// CaptureFault records a domain failure (phase=Failed + reason/message). The failure surfaces to users
// through the core-derived Ready (the core mirrors phase=Failed into Ready=False). Fail is the quick
// form (reason + underlying cause); Reject is the structured form (FailSpec).
type CaptureFault interface {
	Fail(ctx context.Context, t SnapshotAdapter, reason Reason, cause error) error
	Reject(ctx context.Context, t SnapshotAdapter, in FailSpec) error
}

// CaptureProgress publishes a NON-terminal, domain-owned diagnostic into
// status.captureState.domainSpecificController.message. It is the observable companion to a fail-closed
// requeue (a planning leg that is retrying, e.g. an unreadable namespace manifest plan): the message says
// WHY the leg is stuck, in the domain-owned status field the ADR status model keeps in every phase, so an
// operator sees it in `kubectl get snapshot -o yaml` instead of only in controller logs.
type CaptureProgress interface {
	// ReportProgress writes ONLY the domain message, preserving the current phase and reason — it never
	// advances/regresses the lifecycle phase and never writes the core-owned Ready condition (so it does
	// not violate the Ready writer discipline). It is idempotent (no status write when the message is
	// unchanged); an empty message clears a prior diagnostic. Unlike Fail/Reject this is non-terminal: the
	// caller keeps requeuing, this only makes the wait observable.
	ReportProgress(ctx context.Context, t SnapshotAdapter, message string) error
}

// SourcePublisher publishes the captured live source's full reference into the top-level
// status.sourceRef. It is used by import-mode recreation (d8-cli reads it as a single block). Only
// domain snapshots that capture a live source publish it; a nil/zero source is a no-op.
type SourcePublisher interface {
	PublishSnapshotSource(ctx context.Context, t SnapshotAdapter, src SnapshotSource) error
}

// ManifestExclude is the reusable exclude-ordering capability (wave5 §6.3) any aggregator uses to build
// its own manifest MCR as EnsureManifestCapture(base − exclude): the exclude set is everything its
// descendant snapshots already captured. It is optional — only aggregators that own a manifest leg
// spanning objects their children also capture need it (the namespace-root Snapshot; a VM whose disk
// children capture part of its objects). It requires a subresource REST client (WithSubresourceREST).
type ManifestExclude interface {
	// SubtreeManifestIdentities returns the union of object identities captured across this snapshot's
	// DIRECT children subtrees — the exclude set for the aggregator's own manifest MCR. It resolves each
	// child's bound SnapshotContent (child.status.boundSnapshotContentName) and calls the
	// snapshotcontents/<name>/subtree-manifest-identities subresource, unioning (de-duplicating) the
	// results. It is FAIL-CLOSED: if any subtree is not fully persisted (a 409 from the subresource) or a
	// child has not bound its content yet, it returns ErrSubtreeIdentitiesPending and the caller requeues
	// — a partial exclude is never returned. A node with no children returns an empty set.
	SubtreeManifestIdentities(ctx context.Context, t SnapshotAdapter) ([]SubtreeManifestIdentity, error)
}

// CaptureInspection exposes read-only condition views the domain uses to build its own Finished/wait/stop
// logic (Variant A): the core is the SOLE writer of the terminal Ready on both the SnapshotContent and its
// owning snapshot, and it bubbles a failed leg up the content tree as ChildrenFailed. The domain never
// turns a core-owned leg failure into a terminal itself — it only READS these to time its consistency
// actions and to stop requeuing once the core has surfaced a terminal outcome. A snapshot's OWN Ready is
// read directly off the adapter (ReadyStatus/ReadyReason/ReadyMessage); this capability adds the children.
type CaptureInspection interface {
	// ChildrenCaptureStates resolves the snapshot's declared child snapshot refs
	// (status.childrenSnapshotRefs) and returns, for each, its Ready condition (status/reason/message) and
	// whether all its declared capture legs are latched captured (status.captureState.commonController).
	// Children are read as unstructured objects by the ref GVK, so it works across any domain child kind
	// without the SDK importing the concrete types. A child not found yet is reported with an empty Ready
	// (status "") and AllLegsCaptured=false so the caller treats it as still-pending.
	ChildrenCaptureStates(ctx context.Context, t SnapshotAdapter) ([]ChildCaptureState, error)
}

// CaptureSDK is the capture-side protocol facade a domain snapshot controller drives. It hides all
// Kubernetes transport (capture requests, owner references, optimistic-locked status patches, the
// lifecycle phase) behind a small set of intent verbs.
type CaptureSDK interface {
	Planning
	CaptureBarrier
	CaptureFault
	CaptureProgress
	SourcePublisher
	ManifestExclude
	CaptureInspection
}

// CoreCaptureOutcome derives the tri-state the domain switches its wait loop on, from the core's leg
// latches (captureState.commonController) plus the terminal Ready reason. Failed is checked first (a
// terminal Ready reason wins over success latches, which are success-only and never express failure).
func CoreCaptureOutcome(t SnapshotAdapter) CaptureOutcomeResult {
	reason := t.ReadyReason()
	if storagev1alpha1.IsReasonTerminal(reason) {
		return CaptureOutcomeResult{Outcome: CaptureOutcomeFailed, Reason: reason, Message: t.ReadyMessage()}
	}
	if t.CoreCaptureState().AllLegsCaptured() {
		return CaptureOutcomeResult{Outcome: CaptureOutcomeCaptured}
	}
	return CaptureOutcomeResult{Outcome: CaptureOutcomeCapturing}
}

// Option configures optional SDK dependencies passed to New.
type Option func(*sdk)

// WithSubresourceREST wires the aggregated-subresource REST client (typically a discovery REST client)
// the SDK uses for the ManifestExclude capability (SubtreeManifestIdentities). Domains that do not use
// that capability may omit it; SubtreeManifestIdentities then returns a configuration error.
func WithSubresourceREST(r rest.Interface) Option {
	return func(s *sdk) { s.subresourceREST = r }
}

// New returns a CaptureSDK bound to a client (for writes and cached reads), an API reader (for live,
// TOCTOU-safe marker refreshes), and a data-leg provider (see NewStorageFoundationProvider). Optional
// dependencies (e.g. the subresource REST client for ManifestExclude) are supplied via Options.
func New(c client.Client, apiReader client.Reader, provider VolumeCaptureProvider, opts ...Option) CaptureSDK {
	s := &sdk{client: c, apiReader: apiReader, provider: provider}
	for _, o := range opts {
		o(s)
	}
	return s
}

type sdk struct {
	client          client.Client
	apiReader       client.Reader
	provider        VolumeCaptureProvider
	subresourceREST rest.Interface
}

// ErrChildrenSetFrozen is returned by EnsureChildren when a domain tries to GROW the declared child set
// (or change the excluded set) after the node has frozen its plan (phase>=Planned, including the terminal
// Failed). The declared child set is the snapshot's point-in-time membership: once barrier 1 (Planned) is
// declared it is immutable, mirroring the frozen SnapshotContent.status.childrenSnapshotContentRefs. The
// guard is fail-closed and side-effect-free (it rejects BEFORE any child CR is created), so a violating
// domain gets a clean error — recommended reaction sdk.Fail(GraphPlanningFailed) — instead of wedging the
// node in ChildrenLinkPending forever (the immutable content-ref CEL would reject the new edge). Callers
// match it with errors.Is(err, ErrChildrenSetFrozen).
var ErrChildrenSetFrozen = errors.New("snapshotsdk: children set is frozen (phase>=Planned): EnsureChildren cannot grow the declared child set")

func (s *sdk) EnsureChildren(ctx context.Context, t SnapshotAdapter, desired []ChildSpec, excluded []ExcludedObjectRef) error {
	obj := t.Object()

	// Fail-closed freeze pre-check, run BEFORE children.Reconcile creates/adopts anything. Reconcile
	// writes to the cluster first and publishes refs later, so a reject AFTER it ran would leave a freshly
	// created child CR orphaned. The refs the specs WOULD publish are derivable without a cluster write
	// (GVK + name), and the authoritative phase/published-set come from an uncached re-read (apiReader,
	// the same TOCTOU-safe pattern as EnsureVolumeCapture) so a stale cache cannot let a post-Planned
	// growth slip through. desired ⊆ published with an unchanged excluded set is NOT growth: it falls
	// through to the harmless idempotent no-op / ownerRef repair below.
	desiredRefs, err := s.childRefsForSpecs(desired)
	if err != nil {
		return err
	}
	newExcluded := normalizeExcludedRefs(excluded)
	if err := s.refresh(ctx, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if st := t.GetDomainCaptureState(); childrenSetFrozen(st.Phase) {
		grew := !children.RefsEqualIgnoreOrder(st.ChildrenSnapshotRefs, children.UnionRefs(st.ChildrenSnapshotRefs, desiredRefs))
		excludedChanged := !excludedRefsEqualIgnoreOrder(st.ExcludedRefs, newExcluded)
		if grew || excludedChanged {
			return fmt.Errorf("%w: node %s/%s at phase %q; desired refs %v vs published %v, desired excluded %v vs published %v",
				ErrChildrenSetFrozen, obj.GetNamespace(), obj.GetName(), st.Phase,
				desiredRefs, st.ChildrenSnapshotRefs, newExcluded, st.ExcludedRefs)
		}
	}

	owner, err := s.ownerRef(t)
	if err != nil {
		return err
	}
	objs := make([]client.Object, 0, len(desired))
	for _, d := range desired {
		objs = append(objs, d.Object)
	}
	newRefs, err := children.Reconcile(ctx, s.client, s.client.Scheme(), owner, objs)
	if err != nil {
		return err
	}
	return patch.Status(ctx, s.client, obj, func() bool {
		st := t.GetDomainCaptureState()
		// Additive publication: union the freshly planned refs into the currently published set instead
		// of overwriting it. patch.Status re-reads the live object before every attempt, so st here holds
		// FRESH refs — the union therefore preserves refs published by a co-writer of the same field (the
		// root's orphan VolumeSnapshot wave, §6.2) that this planning pass does not enumerate.
		mergedRefs := children.UnionRefs(st.ChildrenSnapshotRefs, newRefs)
		refsGrew := !children.RefsEqualIgnoreOrder(st.ChildrenSnapshotRefs, mergedRefs)
		excludedChanged := !excludedRefsEqualIgnoreOrder(st.ExcludedRefs, newExcluded)
		// TOCTOU belt: the node may have advanced into a frozen phase between the pre-check's authoritative
		// read and this retry re-read. patch.Status cannot surface an error from the closure, so instead of
		// persisting a frozen-set growth it fail-closed DROPS the write here (the pre-check already returned
		// ErrChildrenSetFrozen for the non-racing case). A genuine no-op (nothing grew, no excluded change)
		// short-circuits below regardless of phase, so an idempotent post-Planned re-reconcile is unaffected.
		if childrenSetFrozen(st.Phase) && (refsGrew || excludedChanged) {
			return false
		}
		if !refsGrew && !excludedChanged {
			return false
		}
		st.ChildrenSnapshotRefs = mergedRefs
		st.ExcludedRefs = newExcluded
		t.SetDomainCaptureState(st)
		return true
	})
}

// childRefsForSpecs derives the SnapshotChildRefs the given specs WOULD publish, without any cluster write
// — the same (apiVersion, kind, name) derivation children.Reconcile performs — so the freeze pre-check can
// detect declared-set growth before Reconcile creates/adopts anything.
func (s *sdk) childRefsForSpecs(desired []ChildSpec) ([]storagev1alpha1.SnapshotChildRef, error) {
	refs := make([]storagev1alpha1.SnapshotChildRef, 0, len(desired))
	for _, d := range desired {
		gvk, err := apiutil.GVKForObject(d.Object, s.client.Scheme())
		if err != nil {
			return nil, err
		}
		refs = append(refs, storagev1alpha1.SnapshotChildRef{
			APIVersion: gvk.GroupVersion().String(),
			Kind:       gvk.Kind,
			Name:       d.Object.GetName(),
		})
	}
	return refs, nil
}

func (s *sdk) EnsureVolumeCapture(ctx context.Context, t SnapshotAdapter, in VolumeCaptureSpec) error {
	if in.DataRef == nil {
		return nil
	}
	obj := t.Object()
	if t.CoreCaptureState().dataCaptured() {
		return nil
	}
	namespace := obj.GetNamespace()
	name := s.provider.VCRName(obj.GetUID())

	// Cached existence probe first (a live data-leg VCR always carries its PVC target, so a non-nil result
	// means the request still exists). While the data leg is in flight the VCR is present in cache, so we
	// converge without any uncached read and keep the domain's in-flight poll off the API server. Only
	// when the VCR is absent is the state ambiguous ("not created yet" vs "captured, then deleted by the
	// binder"): only then do we pay the authoritative uncached read to consult the leg latch and avoid
	// re-creating a captured request (the binder sets dataCaptured=true before deleting the VCR).
	existingTarget, err := s.provider.OwnedPVCTarget(ctx, namespace, name)
	if err != nil {
		return err
	}
	if existingTarget == nil {
		if err := s.refresh(ctx, obj); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		if t.CoreCaptureState().dataCaptured() {
			return nil
		}
	}
	owner, err := s.ownerRef(t)
	if err != nil {
		return err
	}
	if err := s.provider.EnsureVCR(ctx, namespace, name, owner, *in.DataRef); err != nil {
		return err
	}
	return patch.Status(ctx, s.client, obj, func() bool {
		st := t.GetDomainCaptureState()
		if st.VolumeCaptureRequestName == name {
			return false
		}
		st.VolumeCaptureRequestName = name
		t.SetDomainCaptureState(st)
		return true
	})
}

func (s *sdk) EnsureManifestCapture(ctx context.Context, t SnapshotAdapter, in ManifestCaptureSpec) error {
	obj := t.Object()
	if t.CoreCaptureState().manifestCaptured() {
		return nil
	}
	namespace := obj.GetNamespace()
	mcrName := manifest.RequestName(obj.GetUID())

	// Cached existence probe first. While the manifest leg is in flight the MCR still lives in the informer
	// cache, so we converge without any uncached read — this keeps the domain's in-flight poll
	// (RequeueAfter every few hundred ms) off the API server. Only when the cache shows the MCR absent is
	// the state ambiguous ("not created yet" vs "captured, then deleted by the binder"): only then do we
	// pay the authoritative uncached read to consult the leg latch and avoid re-creating a captured
	// request (the binder sets manifestCaptured=true before deleting the MCR).
	existing := &ssv1alpha1.ManifestCaptureRequest{}
	getErr := s.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: mcrName}, existing)
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		return getErr
	}
	if apierrors.IsNotFound(getErr) {
		if err := s.refresh(ctx, obj); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		if t.CoreCaptureState().manifestCaptured() {
			return nil
		}
		owner, err := s.ownerRef(t)
		if err != nil {
			return err
		}
		mcr := &ssv1alpha1.ManifestCaptureRequest{
			ObjectMeta: metav1.ObjectMeta{
				Name:            mcrName,
				Namespace:       namespace,
				OwnerReferences: []metav1.OwnerReference{owner},
			},
			Spec: ssv1alpha1.ManifestCaptureRequestSpec{
				// The manifest leg is built solely from the domain's declared targets — the SDK never reads
				// the data-leg VCR to derive or inject targets. EnsureManifestCapture and EnsureVolumeCapture
				// are therefore order-independent (see ManifestCaptureSpec / VolumeCaptureSpec).
				Targets: manifest.Targets(in.Targets),
			},
		}
		if err := s.client.Create(ctx, mcr); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}
	return patch.Status(ctx, s.client, obj, func() bool {
		st := t.GetDomainCaptureState()
		if st.ManifestCaptureRequestName == mcrName {
			return false
		}
		st.ManifestCaptureRequestName = mcrName
		t.SetDomainCaptureState(st)
		return true
	})
}

func (s *sdk) PublishSnapshotSource(ctx context.Context, t SnapshotAdapter, src SnapshotSource) error {
	if src.APIVersion == "" && src.Kind == "" && src.Name == "" {
		return nil
	}
	obj := t.Object()
	return patch.Status(ctx, s.client, obj, func() bool {
		cur := t.GetSnapshotSource()
		if cur != nil && *cur == src {
			return false
		}
		v := src
		t.SetSnapshotSource(&v)
		return true
	})
}

func (s *sdk) MarkPlanned(ctx context.Context, t SnapshotAdapter) error {
	return s.setPhase(ctx, t, storagev1alpha1.SnapshotCapturePhasePlanned, "", "")
}

func (s *sdk) ConfirmConsistent(ctx context.Context, t SnapshotAdapter) error {
	return s.setPhase(ctx, t, storagev1alpha1.SnapshotCapturePhaseFinished, "", "")
}

func (s *sdk) Fail(ctx context.Context, t SnapshotAdapter, reason Reason, cause error) error {
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	return s.setPhase(ctx, t, storagev1alpha1.SnapshotCapturePhaseFailed, string(reason), message)
}

func (s *sdk) Reject(ctx context.Context, t SnapshotAdapter, in FailSpec) error {
	message := in.Message
	if message == "" && in.Cause != nil {
		message = in.Cause.Error()
	}
	return s.setPhase(ctx, t, storagev1alpha1.SnapshotCapturePhaseFailed, string(in.Reason), message)
}

// ReportProgress patches ONLY status.captureState.domainSpecificController.message, leaving the phase and
// reason untouched — a non-terminal, observable-only write (see CaptureProgress). It intentionally does
// NOT go through setPhase (which is the phase-transition path with the monotonic barrier guard): a
// diagnostic is unordered and must never disturb the lifecycle phase or clear a real failure reason.
func (s *sdk) ReportProgress(ctx context.Context, t SnapshotAdapter, message string) error {
	obj := t.Object()
	return patch.Status(ctx, s.client, obj, func() bool {
		st := t.GetDomainCaptureState()
		// Failed is terminal: a non-terminal progress note must never overwrite the terminal
		// reason/message (ReportProgress is the Pending-only diagnostic channel).
		if st.Phase == storagev1alpha1.SnapshotCapturePhaseFailed {
			return false
		}
		if st.Message == message {
			return false
		}
		st.Message = message
		t.SetDomainCaptureState(st)
		return true
	})
}

// setPhase patches status.captureState.domainSpecificController.phase (+ reason/message) via the adapter.
func (s *sdk) setPhase(ctx context.Context, t SnapshotAdapter, phase storagev1alpha1.SnapshotCapturePhase, reason, message string) error {
	obj := t.Object()
	return patch.Status(ctx, s.client, obj, func() bool {
		st := t.GetDomainCaptureState()
		// Monotonic barrier guard. Domain controllers call MarkPlanned on every reconcile (before
		// switching on the capture outcome), so without this a snapshot that already advanced to Finished
		// would be dragged back to Planned. That regression makes each reconcile emit two status writes
		// (Planned then Finished) and, because the domain watches its own object, the pair re-triggers the
		// reconcile — a self-sustaining phase write storm (Planned<->Finished) that starves the core
		// binder's optimistic-lock Ready mirror and wedges the snapshot tree at Ready=False/ContentMissing.
		if !phaseCanAdvance(st.Phase, phase) {
			return false
		}
		if st.Phase == phase && st.Reason == reason && st.Message == message {
			return false
		}
		st.Phase = phase
		st.Reason = reason
		st.Message = message
		t.SetDomainCaptureState(st)
		return true
	})
}

// phaseRank orders the forward capture barriers. Unknown/empty ranks 0 so the first real phase always
// advances; Failed is intentionally absent (handled out-of-band in phaseCanAdvance).
func phaseRank(p storagev1alpha1.SnapshotCapturePhase) int {
	switch p {
	case storagev1alpha1.SnapshotCapturePhasePlanning:
		return 1
	case storagev1alpha1.SnapshotCapturePhasePlanned:
		return 2
	case storagev1alpha1.SnapshotCapturePhaseFinished:
		return 3
	default:
		return 0
	}
}

// phaseCanAdvance reports whether a from->to phase transition is allowed. Two rules:
//
//   - the forward chain Planning<Planned<Finished must never regress — this is what stops MarkPlanned from
//     dragging a Finished snapshot back to Planned;
//   - Failed is a TERMINAL SINK: once a capture fails it can never leave Failed. A snapshot is a
//     point-in-time capture with an immutable spec, so it never re-plans (the rest of the system already
//     treats Failed as terminal — see childrenSetFrozen and ownerDomainCaptureAtLeastPlanned). Making it a
//     sink here also kills the phase write storm where a domain's unconditional per-reconcile MarkPlanned
//     dragged Failed->Planned and the terminal outcome immediately re-Failed it (flapping the mirrored
//     Ready). Only Fail/Reject enter Failed; a NON-terminal, recoverable "waiting for X" state must NOT use
//     them — it stays in its current phase and surfaces the reason via ReportProgress (message-only), the
//     way a Pod stays Pending with a diagnostic instead of moving to a terminal phase.
//
// A Failed->Failed transition stays allowed so an idempotent re-assert (or a refined terminal
// reason/message) is a harmless no-op/refresh via setPhase's equality check.
func phaseCanAdvance(from, to storagev1alpha1.SnapshotCapturePhase) bool {
	if from == storagev1alpha1.SnapshotCapturePhaseFailed {
		return to == storagev1alpha1.SnapshotCapturePhaseFailed
	}
	if to == storagev1alpha1.SnapshotCapturePhaseFailed {
		return true
	}
	return phaseRank(to) >= phaseRank(from)
}

// childrenSetFrozen reports whether the declared child set is frozen at this phase: barrier 1 (Planned)
// and beyond (Finished), plus the terminal Failed. After the freeze EnsureChildren rejects any growth of
// the declared set or change of the excluded set (see ErrChildrenSetFrozen). Planned and Finished rank
// at/above Planned; Failed is terminal (a failed snapshot never re-plans) and, because phaseRank leaves it
// at 0, is matched explicitly here — the same out-of-band treatment phaseCanAdvance gives it.
func childrenSetFrozen(p storagev1alpha1.SnapshotCapturePhase) bool {
	if p == storagev1alpha1.SnapshotCapturePhaseFailed {
		return true
	}
	return phaseRank(p) >= phaseRank(storagev1alpha1.SnapshotCapturePhasePlanned)
}

func (s *sdk) ownerRef(t SnapshotAdapter) (metav1.OwnerReference, error) {
	obj := t.Object()
	gvk, err := apiutil.GVKForObject(obj, s.client.Scheme())
	if err != nil {
		return metav1.OwnerReference{}, err
	}
	controller := true
	return metav1.OwnerReference{
		APIVersion: gvk.GroupVersion().String(),
		Kind:       gvk.Kind,
		Name:       obj.GetName(),
		UID:        obj.GetUID(),
		Controller: &controller,
	}, nil
}

func (s *sdk) refresh(ctx context.Context, obj client.Object) error {
	return s.apiReader.Get(ctx, client.ObjectKeyFromObject(obj), obj)
}
