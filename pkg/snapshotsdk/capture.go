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
	// EnsureChildren makes the cluster match the desired set of child snapshots (create/adopt each under
	// this snapshot) and publishes the resulting refs as status.childrenSnapshotRefs. Publication is
	// ADDITIVE (wave5): the freshly derived refs are UNIONED into the currently published set, never
	// replacing it — so refs contributed by a co-writer of the same field that this pass does not itself
	// enumerate (the namespace root's orphan VolumeSnapshot wave, §6.2) are preserved. It performs
	// create/adopt + publication only and never deletes children (SDK v1 is delete-free): a nil or empty
	// desired set therefore publishes NO new refs and leaves the currently published set intact. A child
	// no longer desired is simply not re-added by its emitter and is left in the cluster for ownerRef GC /
	// a future cleanup component to reclaim.
	//
	// excluded is the domain's DIRECT exclusion vetoes at this node — the source objects it dropped (via
	// the exclude label) while enumerating children, obtained from PartitionExcluded. It is published in
	// the same status patch as the kept children, into
	// status.captureState.domainSpecificController.excludedRefs (the INPUT the core aggregates). The SDK
	// does NOT compute the veto (it sees built child specs, not source labels): the domain partitions and
	// hands both halves here. Pass nil when nothing is excluded; the wire value is normalized to [].
	EnsureChildren(ctx context.Context, t SnapshotAdapter, desired []ChildSpec, excluded []ExcludedObjectRef) error

	// EnsureVolumeCapture ensures the data-leg capture request for the given PVC targets and publishes its
	// name. An empty target set is a manifest-only snapshot (no request, no published name). The operation
	// is suppressed once the core controller has stamped the data leg captured.
	EnsureVolumeCapture(ctx context.Context, t SnapshotAdapter, in VolumeCaptureSpec) error

	// EnsureManifestCapture ensures the per-snapshot ManifestCaptureRequest (the base target SET plus any
	// owned-PVC targets discovered from the data leg) and publishes its name. The operation is suppressed
	// once the core controller has stamped the manifest leg captured.
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

// SourcePublisher publishes the captured live source's full reference into the top-level
// status.snapshotSource. It is used by import-mode recreation (d8-cli reads it as a single block). Only
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

// CaptureSDK is the capture-side protocol facade a domain snapshot controller drives. It hides all
// Kubernetes transport (capture requests, owner references, optimistic-locked status patches, the
// lifecycle phase) behind a small set of intent verbs.
type CaptureSDK interface {
	Planning
	CaptureBarrier
	CaptureFault
	SourcePublisher
	ManifestExclude
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

func (s *sdk) EnsureChildren(ctx context.Context, t SnapshotAdapter, desired []ChildSpec, excluded []ExcludedObjectRef) error {
	obj := t.Object()
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
	newExcluded := normalizeExcludedRefs(excluded)
	return patch.Status(ctx, s.client, obj, func() bool {
		st := t.GetDomainCaptureState()
		// Additive publication: union the freshly planned refs into the currently published set instead
		// of overwriting it. patch.Status re-reads the live object before every attempt, so st here holds
		// FRESH refs — the union therefore preserves refs published by a co-writer of the same field (the
		// root's orphan VolumeSnapshot wave, §6.2) that this planning pass does not enumerate.
		mergedRefs := children.UnionRefs(st.ChildrenSnapshotRefs, newRefs)
		if children.RefsEqualIgnoreOrder(st.ChildrenSnapshotRefs, mergedRefs) &&
			excludedRefsEqualIgnoreOrder(st.ExcludedRefs, newExcluded) {
			return false
		}
		st.ChildrenSnapshotRefs = mergedRefs
		st.ExcludedRefs = newExcluded
		t.SetDomainCaptureState(st)
		return true
	})
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
		ownedPVC, err := s.provider.OwnedPVCTarget(ctx, namespace, t.GetDomainCaptureState().VolumeCaptureRequestName)
		if err != nil {
			return err
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
				Targets: manifest.Targets(in.Targets, ownedPVC, namespace),
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

// phaseCanAdvance reports whether a from->to phase transition is allowed. The forward chain
// (Planning<Planned<Finished) must never regress — this is what stops MarkPlanned from dragging a
// Finished snapshot back to Planned. Failed stays orthogonal: it may be entered from any phase (surface a
// late error) and left again on a subsequent successful reconcile, preserving pre-guard failure behavior.
func phaseCanAdvance(from, to storagev1alpha1.SnapshotCapturePhase) bool {
	if from == storagev1alpha1.SnapshotCapturePhaseFailed || to == storagev1alpha1.SnapshotCapturePhaseFailed {
		return true
	}
	return phaseRank(to) >= phaseRank(from)
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
