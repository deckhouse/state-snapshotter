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
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk/internal/children"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk/internal/conditions"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk/internal/manifest"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk/internal/patch"
)

// Planning drives the three idempotent, restart-safe parts of a snapshot's planning: its child snapshots,
// its volume (PVC) data capture, and its manifest capture. Each method reconciles the cluster toward the
// desired intent and publishes the resulting names/refs into the snapshot status.
//
// Lifecycle model (all three methods): the planning barrier (ChildrenSnapshotReady=True, written by
// MarkPlanningReady) is the final commit point of the planning phase; an individual planning artifact
// becomes immutable the moment it is published, even before the barrier. This yields three states:
//   - State 1 (nothing published, barrier not committed): converge — create/reuse freely.
//   - State 2 (published, barrier not committed): fail closed if the desired artifact diverges from the
//     published one, so a restart with non-deterministic discovery cannot silently rewrite planning intent.
//   - State 3 (barrier committed): the SDK is INERT — every Ensure* returns nil immediately, creating,
//     reusing, and validating nothing. Ownership has passed to the core controller.
//
// The SDK is delete-free throughout and consults no execution-phase signal (it does not wait on the core
// controller); suppression after the barrier is the domain-owned condition alone.
type Planning interface {
	// EnsureChildren create/adopts the desired child snapshots and publishes their refs as
	// status.childrenSnapshotRefs (create/adopt + publication only; never deletes). Per the lifecycle
	// model: inert once the barrier is committed; before commit, a published non-empty set that diverges
	// from desired (set equality on (apiVersion,kind,name), not count) is terminal topology drift —
	// ErrTopologyDrift, creating/deleting nothing — surfaced via MarkPlanningFailed(ReasonTopologyDrift).
	// An empty published set still converges. A duplicate desired child is a non-drift error.
	EnsureChildren(ctx context.Context, t SnapshotAdapter, desired []ChildSpec) error

	// EnsureVolumeCapture ensures the volume capture request for the snapshot's single data-ref PVC and
	// publishes its name. A nil DataRef is a manifest-only snapshot (no request, delete-free: a previously
	// published name is never cleared). Per the lifecycle model: inert once the barrier is committed;
	// before commit it create-or-reuses the VCR (a missing VCR is recreated), while an existing VCR
	// targeting a different PVC fails closed (per-artifact immutability of the data capture).
	EnsureVolumeCapture(ctx context.Context, t SnapshotAdapter, in VolumeCaptureSpec) error

	// EnsureManifestCapture ensures the per-snapshot ManifestCaptureRequest (the domain-chosen target set
	// plus any owned-PVC target discovered from the data capture) and publishes its name. Per the lifecycle
	// model: inert once the barrier is committed; before commit it creates the request when absent and,
	// when the request already exists, requires the desired target set to match its targets by canonical
	// identity (set equality on (apiVersion,kind,name), order and duplicates ignored) — a differing set is
	// terminal ErrManifestDrift (no update/patch/delete), surfaced via MarkPlanningFailed(ReasonManifestDrift).
	//
	// Cardinality invariant: the FINAL target set (domain targets plus any owned-PVC augmentation) must be
	// non-empty. If it is empty, EnsureManifestCapture fails closed with ErrEmptyManifest before touching
	// the cluster (no Get/Create, no status patch). The SDK does not inject the source object — supplying at
	// least one manifest target is the domain's responsibility.
	EnsureManifestCapture(ctx context.Context, t SnapshotAdapter, in ManifestCaptureSpec) error
}

// PlanningBarrier publishes the derived planning-complete signal the core controller waits on before it
// takes over snapshot content. It is the single place the legacy condition name is written.
type PlanningBarrier interface {
	// MarkPlanningReady declares the snapshot's planning complete — manifest capture, data capture, and
	// child snapshot planning are all done (planning barrier satisfied).
	MarkPlanningReady(ctx context.Context, t SnapshotAdapter, message string) error
	// MarkPlanningFailed declares planning blocked with a domain-chosen reason and underlying cause.
	MarkPlanningFailed(ctx context.Context, t SnapshotAdapter, reason Reason, cause error) error
}

// ReadinessFault publishes a Ready=False outcome for a snapshot whose source or required artifact is not
// (yet) usable.
type ReadinessFault interface {
	MarkNotReady(ctx context.Context, t SnapshotAdapter, in NotReadyStatus) error
}

// CaptureSDK is the capture-side protocol facade a domain snapshot controller drives. It hides all
// Kubernetes transport (capture requests, owner references, optimistic-locked status patches, the planning
// barrier condition) behind a small set of intent verbs.
type CaptureSDK interface {
	Planning
	PlanningBarrier
	ReadinessFault
}

// New returns a CaptureSDK bound to a client (for writes and cached reads) and a volume-capture provider
// (see NewStorageFoundationProvider). Suppression is driven by the planning barrier condition read from
// the adapter's in-memory state, so no separate API reader is needed.
func New(c client.Client, provider VolumeCaptureProvider) CaptureSDK {
	return &sdk{client: c, provider: provider}
}

type sdk struct {
	client   client.Client
	provider VolumeCaptureProvider
}

func (s *sdk) EnsureChildren(ctx context.Context, t SnapshotAdapter, desired []ChildSpec) error {
	// After the planning barrier is committed the planning phase is immutable and the SDK is inert:
	// ownership has passed to the core controller, so EnsureChildren neither creates, adopts, nor
	// validates anything. A post-commit divergence is an invalid state the SDK does not repair or report.
	if conditions.IsTrue(t.GetConditions(), storagev1alpha1.ConditionChildrenSnapshotReady) {
		return nil
	}
	obj := t.Object()
	objs := make([]client.Object, 0, len(desired))
	for i, d := range desired {
		if d.Object == nil {
			return fmt.Errorf("snapshotsdk: desired[%d].Object is nil; ChildSpec.Object must be a domain-built child object", i)
		}
		objs = append(objs, d.Object)
	}
	desiredRefs, err := children.DeriveRefs(s.client.Scheme(), objs)
	if err != nil {
		return err
	}

	// Per-artifact immutability: a planning artifact becomes immutable the moment it is published, even
	// before the barrier, so a restart whose discovery yields a different set cannot silently rewrite
	// already-published planning intent. A published, non-empty child set that diverges from desired (set
	// equality on the canonical (apiVersion,kind,name) key, NOT count) is terminal topology drift — fail
	// closed without creating, deleting, or republishing anything. An empty published set is State 1
	// (nothing published yet): the set may still converge to newly observed desired (R25).
	published := t.GetDomainCaptureState().ChildrenSnapshotRefs
	if len(published) > 0 && !children.RefsEqualIgnoreOrder(published, desiredRefs) {
		return ErrTopologyDrift
	}

	owner, err := s.ownerRef(t)
	if err != nil {
		return err
	}
	if err := children.EnsureAll(ctx, s.client, owner, objs); err != nil {
		return err
	}
	return patch.Status(ctx, s.client, obj, func() bool {
		st := t.GetDomainCaptureState()
		if children.RefsEqualIgnoreOrder(st.ChildrenSnapshotRefs, desiredRefs) {
			return false
		}
		st.ChildrenSnapshotRefs = desiredRefs
		t.SetDomainCaptureState(st)
		return true
	})
}

func (s *sdk) EnsureVolumeCapture(ctx context.Context, t SnapshotAdapter, in VolumeCaptureSpec) error {
	if in.DataRef == nil {
		return nil
	}
	// Inert after the planning barrier (see EnsureChildren). Before the barrier this create-or-reuses the
	// VCR; a missing VCR is (re)created, while an existing VCR targeting a different PVC fails closed inside
	// EnsureVCR (per-artifact immutability for the data capture).
	if conditions.IsTrue(t.GetConditions(), storagev1alpha1.ConditionChildrenSnapshotReady) {
		return nil
	}
	obj := t.Object()
	owner, err := s.ownerRef(t)
	if err != nil {
		return err
	}
	name := s.provider.VCRName(obj.GetUID())
	if err := s.provider.EnsureVCR(ctx, obj.GetNamespace(), name, owner, *in.DataRef); err != nil {
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
	// Inert after the planning barrier (see EnsureChildren).
	if conditions.IsTrue(t.GetConditions(), storagev1alpha1.ConditionChildrenSnapshotReady) {
		return nil
	}
	obj := t.Object()
	gvk, err := apiutil.GVKForObject(obj, s.client.Scheme())
	if err != nil {
		return err
	}
	namespace := obj.GetNamespace()
	mcrName := manifest.RequestName(gvk.Kind, namespace, obj.GetName())

	// Build the FULL desired target set exactly as on create — domain targets plus the owned-PVC target
	// derived from the data-capture VCR — so the drift comparison below is apples-to-apples (the augmented
	// PVC target must be part of desired, otherwise an existing MCR that legitimately carries it would
	// be a false-positive drift).
	ownedPVC, err := s.provider.OwnedPVCTarget(ctx, namespace, t.GetDomainCaptureState().VolumeCaptureRequestName)
	if err != nil {
		return err
	}
	base := append([]ssv1alpha1.ManifestTarget(nil), in.Targets...)
	desiredTargets := manifest.Targets(base, ownedPVC, namespace)

	// Manifest capture cardinality invariant: the final target set (domain targets + owned-PVC augmentation)
	// must be non-empty. Fail closed BEFORE any cluster mutation (no MCR Get/Create, no status patch) so an
	// empty manifest capture can never be silently published as a valid request.
	if len(desiredTargets) == 0 {
		return ErrEmptyManifest
	}

	existing := &ssv1alpha1.ManifestCaptureRequest{}
	getErr := s.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: mcrName}, existing)
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		return getErr
	}
	if apierrors.IsNotFound(getErr) {
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
				Targets: desiredTargets,
			},
		}
		if err := s.client.Create(ctx, mcr); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	} else if !manifest.TargetsEqualIgnoreOrder(existing.Spec.Targets, desiredTargets) {
		// Fail-closed manifest capture: the published MCR's targets diverge from the desired set. Do not
		// update/patch/delete the request and do not touch status — surface terminal drift.
		return ErrManifestDrift
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

func (s *sdk) MarkPlanningReady(ctx context.Context, t SnapshotAdapter, message string) error {
	return s.patchCondition(ctx, t, storagev1alpha1.ConditionChildrenSnapshotReady, metav1.ConditionTrue, storagev1alpha1.ReasonCompleted, message)
}

func (s *sdk) MarkPlanningFailed(ctx context.Context, t SnapshotAdapter, reason Reason, cause error) error {
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	return s.patchCondition(ctx, t, storagev1alpha1.ConditionChildrenSnapshotReady, metav1.ConditionFalse, string(reason), message)
}

func (s *sdk) MarkNotReady(ctx context.Context, t SnapshotAdapter, in NotReadyStatus) error {
	message := in.Message
	if message == "" && in.Cause != nil {
		message = in.Cause.Error()
	}
	return s.patchCondition(ctx, t, storagev1alpha1.ConditionReady, metav1.ConditionFalse, string(in.Reason), message)
}

func (s *sdk) patchCondition(ctx context.Context, t SnapshotAdapter, condType string, status metav1.ConditionStatus, reason, message string) error {
	return patch.Condition(ctx, s.client, t.Object(), t.GetConditions, t.SetConditions,
		func(conds []metav1.Condition, observedGeneration int64) ([]metav1.Condition, bool) {
			if conditions.Equal(conds, condType, status, reason, message, observedGeneration) {
				return conds, false
			}
			return conditions.Upsert(conds, metav1.Condition{
				Type:               condType,
				Status:             status,
				Reason:             reason,
				Message:            message,
				ObservedGeneration: observedGeneration,
			}), true
		})
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
