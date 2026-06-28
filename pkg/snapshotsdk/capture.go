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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk/internal/children"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk/internal/conditions"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk/internal/manifest"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk/internal/patch"
)

// Planning drives the three idempotent, restart-safe planning legs of a snapshot: its child snapshots, its
// data-leg volume capture, and its manifest capture. Each method reconciles the cluster toward the desired
// intent and publishes the resulting names/refs into the snapshot status.
type Planning interface {
	// EnsureChildren makes the cluster match the desired set of child snapshots (create/adopt each under
	// this snapshot) and publishes the resulting refs as status.childrenSnapshotRefs. It performs
	// create/adopt + publication only and never deletes children (SDK v1 is delete-free). A nil or empty
	// desired set publishes empty refs: a child no longer desired becomes detached from the snapshot graph
	// but is left in the cluster for ownerRef GC / a future cleanup component to reclaim.
	EnsureChildren(ctx context.Context, t SnapshotAdapter, desired []ChildSpec) error

	// EnsureVolumeCapture ensures the data-leg capture request for the given PVC targets and publishes its
	// name. An empty target set is a manifest-only snapshot (no request, no published name). The operation
	// is suppressed once the core controller has stamped the data leg captured.
	EnsureVolumeCapture(ctx context.Context, t SnapshotAdapter, in VolumeCaptureSpec) error

	// EnsureManifestCapture ensures the per-snapshot ManifestCaptureRequest (the domain-chosen target set
	// plus any owned-PVC target discovered from the data leg) and publishes its name. The operation is
	// suppressed once the core controller has stamped the manifest leg captured.
	EnsureManifestCapture(ctx context.Context, t SnapshotAdapter, in ManifestCaptureSpec) error
}

// PlanningBarrier publishes the derived planning-complete signal the core controller waits on before it
// takes over snapshot content. It is the single place the legacy condition name is written.
type PlanningBarrier interface {
	// MarkPlanningReady declares all planning legs complete (planning barrier satisfied).
	MarkPlanningReady(ctx context.Context, t SnapshotAdapter, message string) error
	// MarkPlanningFailed declares planning blocked with a domain-chosen reason and underlying cause.
	MarkPlanningFailed(ctx context.Context, t SnapshotAdapter, reason Reason, cause error) error
}

// ReadinessFault publishes a Ready=False outcome for a snapshot whose source or required artifact is not
// (yet) usable.
type ReadinessFault interface {
	MarkNotReady(ctx context.Context, t SnapshotAdapter, in NotReadySpec) error
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
// TOCTOU-safe marker refreshes), and a data-leg provider (see NewStorageFoundationProvider).
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
	for _, d := range desired {
		objs = append(objs, d.Object)
	}
	newRefs, err := children.Reconcile(ctx, s.client, s.client.Scheme(), owner, objs)
	if err != nil {
		return err
	}
	return patch.Status(ctx, s.client, obj, func() bool {
		st := t.GetDomainCaptureState()
		if children.RefsEqualIgnoreOrder(st.ChildrenSnapshotRefs, newRefs) {
			return false
		}
		st.ChildrenSnapshotRefs = newRefs
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

	existing := &ssv1alpha1.ManifestCaptureRequest{}
	getErr := s.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: mcrName}, existing)
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		return getErr
	}
	if apierrors.IsNotFound(getErr) {
		ownedPVC, err := s.provider.OwnedPVCTarget(ctx, namespace, t.GetDomainCaptureState().VolumeCaptureRequestName)
		if err != nil {
			return err
		}
		base := append([]ssv1alpha1.ManifestTarget(nil), in.Targets...)
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
				Targets: manifest.Targets(base, ownedPVC, namespace),
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

func (s *sdk) MarkNotReady(ctx context.Context, t SnapshotAdapter, in NotReadySpec) error {
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
