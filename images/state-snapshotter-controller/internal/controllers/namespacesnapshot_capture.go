/*
Copyright 2025 Flant JSC

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

package controllers

import (
	"context"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	deckhousev1alpha1 "github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

const labelNamespaceSnapshotUID = "state-snapshotter.deckhouse.io/namespace-snapshot-uid"

// errManifestCapturePlanDrift is returned when an existing MCR for this NamespaceSnapshot has a different
// target set than the current namespace listing. MCR spec is treated as immutable for N2a (no silent Update).
var errManifestCapturePlanDrift = errors.New("manifest capture plan drift")

// reconcileCaptureN2a drives manifest capture via internal MCR→MCP after NamespaceSnapshotContent is bound (design N2a).
func (r *NamespaceSnapshotReconciler) reconcileCaptureN2a(
	ctx context.Context,
	nsSnap *storagev1alpha1.NamespaceSnapshot,
	content *storagev1alpha1.NamespaceSnapshotContent,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if r.Dynamic == nil {
		return ctrl.Result{}, fmt.Errorf("namespace snapshot reconciler: Dynamic client is nil")
	}
	if r.APIReader == nil {
		return ctrl.Result{}, fmt.Errorf("namespace snapshot reconciler: APIReader is nil")
	}

	_, res, err := r.ensureNamespaceSnapshotRootObjectKeeper(ctx, nsSnap, content)
	if err != nil {
		return ctrl.Result{}, err
	}
	if res.RequeueAfter > 0 || res.Requeue {
		return res, nil
	}

	targets, err := namespacemanifest.BuildManifestCaptureTargets(ctx, r.Dynamic, nsSnap.Namespace)
	if err != nil {
		return r.failCapture(ctx, nsSnap, content, "ListFailed", fmt.Sprintf("build capture targets: %v", err))
	}
	if len(targets) == 0 {
		return r.failCapture(ctx, nsSnap, content, "NoCaptureTargets", "namespace has no resources matching the N2a allowlist (see design §4.5)")
	}

	mcr, res, err := r.ensureManifestCaptureRequest(ctx, nsSnap, targets)
	if err != nil {
		if errors.Is(err, errManifestCapturePlanDrift) {
			return r.failCapture(ctx, nsSnap, content, "CapturePlanDrift", err.Error())
		}
		return ctrl.Result{}, err
	}
	if res.RequeueAfter > 0 || res.Requeue {
		return res, nil
	}

	mcpName := namespacemanifest.GenerateManifestCheckpointNameFromUID(mcr.UID)
	mcp := &ssv1alpha1.ManifestCheckpoint{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: mcpName}, mcp); err != nil {
		if apierrors.IsNotFound(err) {
			meta.SetStatusCondition(&nsSnap.Status.Conditions, metav1.Condition{
				Type:               snapshot.ConditionReady,
				Status:             metav1.ConditionFalse,
				Reason:             "ManifestCheckpointPending",
				Message:            fmt.Sprintf("waiting for ManifestCheckpoint %q", mcpName),
				ObservedGeneration: nsSnap.Generation,
			})
			if err := r.Client.Status().Update(ctx, nsSnap); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: 500 * time.Millisecond}, nil
		}
		return ctrl.Result{}, err
	}

	readyCond := meta.FindStatusCondition(mcp.Status.Conditions, ssv1alpha1.ManifestCheckpointConditionTypeReady)
	if readyCond == nil || readyCond.Status != metav1.ConditionTrue {
		reason := "ManifestCheckpointNotReady"
		msg := "ManifestCheckpoint not ready"
		if readyCond != nil {
			reason = readyCond.Reason
			msg = readyCond.Message
		}
		if readyCond != nil && readyCond.Status == metav1.ConditionFalse &&
			(readyCond.Reason == ssv1alpha1.ManifestCheckpointConditionReasonFailed) {
			return r.failCapture(ctx, nsSnap, content, "ManifestCheckpointFailed", msg)
		}
		meta.SetStatusCondition(&nsSnap.Status.Conditions, metav1.Condition{
			Type:               snapshot.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            msg,
			ObservedGeneration: nsSnap.Generation,
		})
		if err := r.Client.Status().Update(ctx, nsSnap); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 500 * time.Millisecond}, nil
	}

	if readyCond.Reason != ssv1alpha1.ManifestCheckpointConditionReasonCompleted {
		// Ready=True with unexpected reason — still treat as success if True (defensive).
		logger.Info("ManifestCheckpoint Ready=True with non-Completed reason", "reason", readyCond.Reason, "mcp", mcpName)
	}

	// Persist checkpoint name on content (N2a) and mirror success on NSC.status.conditions.
	contentKey := client.ObjectKey{Name: content.Name}
	if err := r.Client.Get(ctx, contentKey, content); err != nil {
		return ctrl.Result{}, err
	}
	if content.Status.ManifestCheckpointName != mcpName {
		content.Status.ManifestCheckpointName = mcpName
	}
	meta.SetStatusCondition(&content.Status.Conditions, metav1.Condition{
		Type:               snapshot.ConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             snapshot.ReasonCompleted,
		Message:            fmt.Sprintf("manifest capture persisted (ManifestCheckpoint %s)", mcpName),
		ObservedGeneration: content.Generation,
	})
	if err := r.Client.Status().Update(ctx, content); err != nil {
		return ctrl.Result{}, err
	}

	nsSnap.Status.ObservedGeneration = nsSnap.Generation
	meta.SetStatusCondition(&nsSnap.Status.Conditions, metav1.Condition{
		Type:               snapshot.ConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             snapshot.ReasonCompleted,
		Message:            fmt.Sprintf("manifest capture complete (ManifestCheckpoint %s)", mcpName),
		ObservedGeneration: nsSnap.Generation,
	})
	if err := r.Client.Status().Update(ctx, nsSnap); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *NamespaceSnapshotReconciler) failCapture(ctx context.Context, nsSnap *storagev1alpha1.NamespaceSnapshot, content *storagev1alpha1.NamespaceSnapshotContent, reason, msg string) (ctrl.Result, error) {
	nsSnap.Status.ObservedGeneration = nsSnap.Generation
	meta.SetStatusCondition(&nsSnap.Status.Conditions, metav1.Condition{
		Type:               snapshot.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: nsSnap.Generation,
	})
	if err := r.Client.Status().Update(ctx, nsSnap); err != nil {
		return ctrl.Result{}, err
	}
	if content != nil && content.Name != "" {
		fresh := &storagev1alpha1.NamespaceSnapshotContent{}
		if err := r.Client.Get(ctx, client.ObjectKey{Name: content.Name}, fresh); err != nil {
			return ctrl.Result{}, err
		}
		meta.SetStatusCondition(&fresh.Status.Conditions, metav1.Condition{
			Type:               snapshot.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            msg,
			ObservedGeneration: fresh.Generation,
		})
		if err := r.Client.Status().Update(ctx, fresh); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

// ensureNamespaceSnapshotRootObjectKeeper creates a cluster-scoped ObjectKeeper in FollowObject mode that
// follows NamespaceSnapshotContent by UID. N2a implementation note (aligned with design §4.3.2): this is a
// lifecycle helper tied to NSC existence, not FollowObjectWithTTL retention yet — when NSC is deleted, OK is
// removed with it; TTL-based retention for persisted results is a follow-up (see namespace-snapshot-controller.md §4.3.2).
func (r *NamespaceSnapshotReconciler) ensureNamespaceSnapshotRootObjectKeeper(ctx context.Context, nsSnap *storagev1alpha1.NamespaceSnapshot, content *storagev1alpha1.NamespaceSnapshotContent) (*deckhousev1alpha1.ObjectKeeper, ctrl.Result, error) {
	logger := log.FromContext(ctx)
	name := namespacemanifest.NamespaceSnapshotRootObjectKeeperName(nsSnap.Namespace, nsSnap.Name)
	ok := &deckhousev1alpha1.ObjectKeeper{}
	err := r.Client.Get(ctx, client.ObjectKey{Name: name}, ok)
	switch {
	case apierrors.IsNotFound(err):
		ok = &deckhousev1alpha1.ObjectKeeper{
			TypeMeta: metav1.TypeMeta{
				APIVersion: DeckhouseAPIVersion,
				Kind:       KindObjectKeeper,
			},
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: deckhousev1alpha1.ObjectKeeperSpec{
				Mode: "FollowObject",
				FollowObjectRef: &deckhousev1alpha1.FollowObjectRef{
					APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
					Kind:       "NamespaceSnapshotContent",
					Name:       content.Name,
					UID:        string(content.UID),
				},
			},
		}
		if err := r.Client.Create(ctx, ok); err != nil {
			return nil, ctrl.Result{}, fmt.Errorf("create ObjectKeeper %s: %w", name, err)
		}
		if err := r.APIReader.Get(ctx, client.ObjectKey{Name: name}, ok); err != nil {
			return nil, ctrl.Result{RequeueAfter: 200 * time.Millisecond}, nil
		}
		logger.Info("created root ObjectKeeper for NamespaceSnapshot", "objectKeeper", name)
		return ok, ctrl.Result{}, nil
	case err != nil:
		return nil, ctrl.Result{}, err
	default:
		if ok.Spec.FollowObjectRef == nil || ok.Spec.FollowObjectRef.UID != string(content.UID) {
			return nil, ctrl.Result{}, fmt.Errorf("ObjectKeeper %s does not follow this NamespaceSnapshotContent", name)
		}
		return ok, ctrl.Result{}, nil
	}
}

func (r *NamespaceSnapshotReconciler) ensureManifestCaptureRequest(ctx context.Context, nsSnap *storagev1alpha1.NamespaceSnapshot, targets []namespacemanifest.ManifestTarget) (*ssv1alpha1.ManifestCaptureRequest, ctrl.Result, error) {
	name := namespacemanifest.NamespaceSnapshotMCRName(nsSnap.UID)
	key := types.NamespacedName{Namespace: nsSnap.Namespace, Name: name}

	specTargets := make([]ssv1alpha1.ManifestTarget, 0, len(targets))
	for _, t := range targets {
		specTargets = append(specTargets, ssv1alpha1.ManifestTarget{
			APIVersion: t.APIVersion,
			Kind:       t.Kind,
			Name:       t.Name,
		})
	}

	existing := &ssv1alpha1.ManifestCaptureRequest{}
	err := r.Client.Get(ctx, key, existing)
	switch {
	case apierrors.IsNotFound(err):
		mcr := &ssv1alpha1.ManifestCaptureRequest{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: nsSnap.Namespace,
				Labels: map[string]string{
					labelNamespaceSnapshotUID: string(nsSnap.UID),
				},
			},
			Spec: ssv1alpha1.ManifestCaptureRequestSpec{Targets: specTargets},
		}
		if err := r.Client.Create(ctx, mcr); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return r.ensureManifestCaptureRequest(ctx, nsSnap, targets)
			}
			return nil, ctrl.Result{}, err
		}
		created := &ssv1alpha1.ManifestCaptureRequest{}
		if err := r.Client.Get(ctx, key, created); err != nil {
			return nil, ctrl.Result{RequeueAfter: 200 * time.Millisecond}, nil
		}
		return created, ctrl.Result{}, nil
	case err != nil:
		return nil, ctrl.Result{}, err
	default:
		if existing.Labels == nil || existing.Labels[labelNamespaceSnapshotUID] != string(nsSnap.UID) {
			return nil, ctrl.Result{}, fmt.Errorf("ManifestCaptureRequest %s exists but is not owned by this NamespaceSnapshot (stale or manual)", key.String())
		}
		if !manifestTargetsEqual(existing.Spec.Targets, specTargets) {
			return nil, ctrl.Result{}, fmt.Errorf("%w: ManifestCaptureRequest %s spec.targets differ from current resolved N2a targets; delete the MCR to retry with a fresh plan", errManifestCapturePlanDrift, key.String())
		}
		return existing, ctrl.Result{}, nil
	}
}

func manifestTargetsEqual(a, b []ssv1alpha1.ManifestTarget) bool {
	if len(a) != len(b) {
		return false
	}
	// Order may differ; compare as sets.
	am := make(map[string]struct{}, len(a))
	for _, t := range a {
		am[fmt.Sprintf("%s|%s|%s", t.APIVersion, t.Kind, t.Name)] = struct{}{}
	}
	for _, t := range b {
		k := fmt.Sprintf("%s|%s|%s", t.APIVersion, t.Kind, t.Name)
		if _, ok := am[k]; !ok {
			return false
		}
		delete(am, k)
	}
	return len(am) == 0
}
