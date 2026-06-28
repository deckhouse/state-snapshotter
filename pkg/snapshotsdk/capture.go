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
type Planning interface {
	// EnsureChildren create/adopts the desired set of child snapshots under this snapshot and publishes
	// the resulting refs as status.childrenSnapshotRefs. It performs create/adopt + publication only and
	// never deletes children (SDK v1 is delete-free).
	//
	// The published child set becomes the authoritative, immutable snapshot topology once the planning
	// barrier is committed (ChildrenSnapshotReady=True, written by MarkPlanningReady). Before commit the
	// desired set may still converge to newly observed domain state; after commit it must match the
	// published set by canonical identity (set equality on (apiVersion, kind, name), not count) — this
	// also locks a committed EMPTY topology (a leaf can never grow a child). A differing set after commit
	// is terminal topology drift: EnsureChildren fails closed with ErrTopologyDrift, creating nothing,
	// deleting nothing, leaving the published refs untouched. The caller surfaces it via MarkPlanningFailed
	// with ReasonTopologyDrift. It also fails (non-drift error) on a duplicate desired child.
	EnsureChildren(ctx context.Context, t SnapshotAdapter, desired []ChildSpec) error

	// EnsureVolumeCapture ensures the volume capture request for the snapshot's single data-ref PVC and
	// publishes its name. A nil DataRef is a manifest-only snapshot: the SDK ensures no request and, being
	// delete-free, never clears a name it published earlier (so a published VCR survives a later nil). Unlike
	// the child topology, the SDK does NOT enforce data-slot immutability or fail closed on a nil after a
	// published VCR — keeping the slot stable is a domain convention (immutable spec.sourceRef), see
	// VolumeCaptureSpec. The operation is suppressed once the core controller has stamped the data captured.
	EnsureVolumeCapture(ctx context.Context, t SnapshotAdapter, in VolumeCaptureSpec) error

	// EnsureManifestCapture ensures the per-snapshot ManifestCaptureRequest (the domain-chosen target set
	// plus any owned-PVC target discovered from the data capture) and publishes its name. On the first call it
	// creates the request; on later calls, if the request already exists, the desired target set must
	// match the request's targets by canonical identity (set equality on (apiVersion, kind, name), order
	// and duplicates ignored). A differing set is terminal manifest drift and EnsureManifestCapture fails
	// closed with ErrManifestDrift — it does not update/patch/delete the request and leaves status
	// untouched. The caller surfaces it via MarkPlanningFailed with ReasonManifestDrift. The operation is
	// suppressed once the core controller has stamped the manifest captured.
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

// New returns a CaptureSDK bound to a client (for writes and cached reads), an API reader (for live,
// TOCTOU-safe marker refreshes), and a volume-capture provider (see NewStorageFoundationProvider).
func New(c client.Client, apiReader client.Reader, provider VolumeCaptureProvider) CaptureSDK {
	return &sdk{client: c, apiReader: apiReader, provider: provider}
}

type sdk struct {
	client    client.Client
	apiReader client.Reader
	provider  VolumeCaptureProvider
}

func (s *sdk) EnsureChildren(ctx context.Context, t SnapshotAdapter, desired []ChildSpec) error {
	obj := t.Object()
	owner, err := s.ownerRef(t)
	if err != nil {
		return err
	}
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

	// Topology is immutable once the planning barrier is committed. The commit marker is the durable
	// ChildrenSnapshotReady=True condition (published by MarkPlanningReady), NOT "refs are non-empty" —
	// otherwise a committed EMPTY topology (a leaf) could later grow a child unnoticed. After commit, the
	// desired set must match the published set exactly (set equality on the canonical (apiVersion,kind,
	// name) key, NOT count); a drifted set — e.g. a restart where discovery sees different children — is
	// terminal: fail closed without creating, deleting, or republishing anything. Before commit, the set
	// may still converge to newly observed desired (Р25).
	published := t.GetDomainCaptureState().ChildrenSnapshotRefs
	committed := conditions.IsTrue(t.GetConditions(), storagev1alpha1.ConditionChildrenSnapshotReady)
	if committed && !children.RefsEqualIgnoreOrder(published, desiredRefs) {
		return ErrTopologyDrift
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
	obj := t.Object()
	if !t.CoreCaptureState().DataCaptured {
		if err := s.refresh(ctx, obj); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
	}
	if t.CoreCaptureState().DataCaptured {
		return nil
	}
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
	obj := t.Object()
	if !t.CoreCaptureState().ManifestCaptured {
		if err := s.refresh(ctx, obj); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
	}
	if t.CoreCaptureState().ManifestCaptured {
		return nil
	}
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

func (s *sdk) refresh(ctx context.Context, obj client.Object) error {
	return s.apiReader.Get(ctx, client.ObjectKeyFromObject(obj), obj)
}
