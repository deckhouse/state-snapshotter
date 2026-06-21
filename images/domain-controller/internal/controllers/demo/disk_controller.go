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

package demo

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/domain-controller/internal/controllers/common"
	"github.com/deckhouse/state-snapshotter/images/domain-controller/pkg/config"
)

// DemoVirtualDiskSnapshotReconciler owns demo disk DOMAIN planning only: sourceRef validation, the
// per-snapshot manifest-capture request (MCR), the data-leg volume-capture request (VCR), and the
// ChildrenSnapshotReady planning barrier. It publishes results into demo.status and never touches the
// cluster-scoped SnapshotContent (created/owned/projected/mirrored by GenericSnapshotBinderController).
// It is content-free: re-creation of MCR/VCR is suppressed by the common controller's domain-only markers
// (status.manifestCaptured / status.dataCaptured), so this controller never reads SnapshotContent.
type DemoVirtualDiskSnapshotReconciler struct {
	Client    client.Client
	APIReader client.Reader
	Config    *config.Options
}

func AddDemoVirtualDiskSnapshotControllerToManager(mgr ctrl.Manager, cfg *config.Options) error {
	// RBAC is not generated from kubebuilder markers in this module.
	// Static controller RBAC is defined in templates/controller/rbac-for-us.yaml.
	// Domain/custom RBAC is granted externally by Deckhouse RBAC controller/hook
	// before RBACReady=True is set on CSD.
	if err := registerDemoDiskBoundContentFieldIndex(context.Background(), mgr.GetFieldIndexer()); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&demov1alpha1.DemoVirtualDiskSnapshot{}).
		// SnapshotContent -> bound demo Snapshot wake-up so the common controller's projection/marker
		// writes re-trigger domain reconcile promptly; enqueue-only.
		Watches(&storagev1alpha1.SnapshotContent{}, handler.EnqueueRequestsFromMapFunc(mapContentToBoundDemoDiskSnapshots(mgr.GetClient()))).
		Complete(&DemoVirtualDiskSnapshotReconciler{
			Client:    mgr.GetClient(),
			APIReader: mgr.GetAPIReader(),
			Config:    cfg,
		})
}

func (r *DemoVirtualDiskSnapshotReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("demoVirtualDiskSnapshot", req.NamespacedName)
	ctx = log.IntoContext(ctx, logger)

	s := &demov1alpha1.DemoVirtualDiskSnapshot{}
	if err := r.Client.Get(ctx, req.NamespacedName, s); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Deletion is handled by higher-level lifecycle (no finalizers here). Materialization-only.
	if s.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	resolution := resolveDemoSnapshotSource(controllercommon.KindDemoVirtualDisk, s.Spec.SourceRef)
	if resolution.Reason != "" {
		if patchErr := patchDemoVirtualDiskSnapshotNotReady(ctx, r.Client, req.NamespacedName, resolution.Reason, resolution.Message); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{}, nil
	}
	sourceName := resolution.Name
	source := &demov1alpha1.DemoVirtualDisk{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: sourceName}, source); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if err := patchDemoVirtualDiskSnapshotNotReady(ctx, r.Client, req.NamespacedName, demoReasonSourceNotFound, fmt.Sprintf("%s %q not found", controllercommon.KindDemoVirtualDisk, sourceName)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Stale-cache guard (TOCTOU): the suppression markers (status.dataCaptured / status.manifestCaptured)
	// are set by the common controller via a live write immediately BEFORE it deletes the VCR/MCR. If this
	// reconcile started from a stale informer cache (marker still false) it would re-create a request the
	// common controller already cleaned up — leaking a duplicate VolumeSnapshot/VolumeSnapshotContent.
	// Refresh the markers from a live read before planning. Once both are set the cached gate below
	// short-circuits, so steady state pays no extra live read.
	if !s.Status.DataCaptured || !s.Status.ManifestCaptured {
		live := &demov1alpha1.DemoVirtualDiskSnapshot{}
		if err := r.APIReader.Get(ctx, req.NamespacedName, live); err != nil {
			if apierrors.IsNotFound(err) {
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, err
		}
		s.Status.DataCaptured = live.Status.DataCaptured
		s.Status.ManifestCaptured = live.Status.ManifestCaptured
	}

	// Data leg (D3): ensure the data-leg VCR (named by the disk snapshot UID, owned by the disk snapshot)
	// and publish its name. The common controller reads the VCR, enriches + hands off the bound
	// VolumeSnapshotContent into SnapshotContent.status.dataRefs, then deletes the VCR and sets
	// status.dataCaptured to suppress re-creation here. A manifest-only disk has no data leg.
	var vcrName string
	if !s.Status.DataCaptured {
		name, terminalReason, terminalMessage, err := r.ensureDemoVirtualDiskDataLeg(ctx, s, source)
		if err != nil {
			return ctrl.Result{}, err
		}
		if terminalReason != "" {
			if perr := patchDemoVirtualDiskSnapshotNotReady(ctx, r.Client, req.NamespacedName, terminalReason, terminalMessage); perr != nil {
				return ctrl.Result{}, perr
			}
			// PVC may still appear (creation race); keep polling.
			return ctrl.Result{RequeueAfter: defaultDemoSnapshotRequeueAfter}, nil
		}
		vcrName = name
		if vcrName != "" {
			if err := patchDemoVirtualDiskSnapshotVolumeCaptureRequestName(ctx, r.Client, req.NamespacedName, vcrName); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	// Manifest leg: ensure the per-snapshot MCR (owned by the disk snapshot, manifest targets derived from
	// the data-leg VCR per D3) and publish its name. Suppressed once status.manifestCaptured is set.
	if !s.Status.ManifestCaptured {
		mcr, err := ensureDemoSnapshotManifestCaptureRequest(
			ctx,
			r.Client,
			s.Namespace,
			s.Name,
			controllercommon.KindDemoVirtualDiskSnapshot,
			demov1alpha1.SchemeGroupVersion.String(),
			controllercommon.KindDemoVirtualDisk,
			source.Name,
			demoSnapshotOwnerReference(demov1alpha1.SchemeGroupVersion.String(), controllercommon.KindDemoVirtualDiskSnapshot, s.Name, s.UID),
			vcrName,
		)
		if err != nil {
			return ctrl.Result{}, err
		}
		if err := patchDemoVirtualDiskSnapshotManifestCaptureRequestName(ctx, r.Client, req.NamespacedName, mcr.Name); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Planning barrier: for a leaf disk "domain planning complete" means its own MCR/VCR are created and
	// published (it has no child snapshots). The common controller waits on this before taking over content.
	if err := patchDemoVirtualDiskSnapshotChildrenSnapshotReady(ctx, r.Client, req.NamespacedName, metav1.ConditionTrue, storagev1alpha1.ReasonCompleted, "manifest capture request planned"); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func patchDemoVirtualDiskSnapshotChildrenSnapshotReady(
	ctx context.Context,
	c client.Client,
	diskKey types.NamespacedName,
	status metav1.ConditionStatus,
	reason string,
	message string,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		o := &demov1alpha1.DemoVirtualDiskSnapshot{}
		if err := c.Get(ctx, diskKey, o); err != nil {
			return err
		}
		if rc := meta.FindStatusCondition(o.Status.Conditions, storagev1alpha1.ConditionChildrenSnapshotReady); rc != nil &&
			rc.Status == status && rc.Reason == reason && rc.Message == message && rc.ObservedGeneration == o.Generation {
			return nil
		}
		base := o.DeepCopy()
		meta.SetStatusCondition(&o.Status.Conditions, metav1.Condition{
			Type:               storagev1alpha1.ConditionChildrenSnapshotReady,
			Status:             status,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: o.Generation,
		})
		// D4a: optimistic-lock merge patch so co-writing conditions (core writes Ready) never
		// silently clobbers this owner's entry — a concurrent write yields 409 → RetryOnConflict re-reads.
		return c.Status().Patch(ctx, o, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	})
}

func patchDemoVirtualDiskSnapshotManifestCaptureRequestName(
	ctx context.Context,
	c client.Client,
	diskKey types.NamespacedName,
	mcrName string,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		o := &demov1alpha1.DemoVirtualDiskSnapshot{}
		if err := c.Get(ctx, diskKey, o); err != nil {
			return err
		}
		if o.Status.ManifestCaptureRequestName == mcrName {
			return nil
		}
		base := o.DeepCopy()
		o.Status.ManifestCaptureRequestName = mcrName
		return c.Status().Patch(ctx, o, client.MergeFrom(base))
	})
}

func patchDemoVirtualDiskSnapshotVolumeCaptureRequestName(
	ctx context.Context,
	c client.Client,
	diskKey types.NamespacedName,
	vcrName string,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		o := &demov1alpha1.DemoVirtualDiskSnapshot{}
		if err := c.Get(ctx, diskKey, o); err != nil {
			return err
		}
		if o.Status.VolumeCaptureRequestName == vcrName {
			return nil
		}
		base := o.DeepCopy()
		o.Status.VolumeCaptureRequestName = vcrName
		return c.Status().Patch(ctx, o, client.MergeFrom(base))
	})
}

func patchDemoVirtualDiskSnapshotNotReady(
	ctx context.Context,
	c client.Client,
	diskKey types.NamespacedName,
	reason string,
	message string,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		o := &demov1alpha1.DemoVirtualDiskSnapshot{}
		if err := c.Get(ctx, diskKey, o); err != nil {
			return err
		}
		if rc := meta.FindStatusCondition(o.Status.Conditions, storagev1alpha1.ConditionReady); rc != nil &&
			rc.Status == metav1.ConditionFalse && rc.Reason == reason && rc.Message == message && rc.ObservedGeneration == o.Generation {
			return nil
		}
		base := o.DeepCopy()
		meta.SetStatusCondition(&o.Status.Conditions, metav1.Condition{
			Type:               storagev1alpha1.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: o.Generation,
		})
		// D4a: optimistic-lock merge patch so this early validation Ready=False and the common controller's
		// Ready mirror co-own the same conditions array safely (409 → RetryOnConflict re-reads).
		return c.Status().Patch(ctx, o, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	})
}
