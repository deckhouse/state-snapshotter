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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/domain-controller/internal/controllers/snaphelpers"
	"github.com/deckhouse/state-snapshotter/images/domain-controller/pkg/config"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk"
)

// DemoVirtualDiskSnapshotReconciler owns demo disk DOMAIN planning only: sourceRef validation, the
// per-snapshot manifest-capture request (MCR), the data-leg volume-capture request (VCR), and the
// planning barrier. All Kubernetes transport (capture requests, owner references, optimistic-locked status
// patches, the barrier condition) is delegated to the snapshot SDK (pkg/snapshotsdk). The domain decides
// only what its source is and which PVC makes up its data leg; it never touches the cluster-scoped
// SnapshotContent (owned by GenericSnapshotBinderController).
type DemoVirtualDiskSnapshotReconciler struct {
	Client    client.Client
	APIReader client.Reader
	Config    *config.Options
}

func AddDemoVirtualDiskSnapshotControllerToManager(mgr ctrl.Manager, cfg *config.Options) error {
	// RBAC is not generated from kubebuilder markers in this module.
	// Static controller RBAC is defined in templates/controller/rbac-for-us.yaml.
	// Domain/custom RBAC is granted externally by Deckhouse RBAC controller/hook
	// before AccessGranted=True is set on CSD.
	//
	// Content-free for SNAPSHOT reconcilers: NO SnapshotContent watch/informer here. The core
	// GenericSnapshotBinderController owns all SnapshotContent work for this DomainCaptureSnapshotKind
	// (creation/projection/Ready mirror) and provides the SnapshotContent -> demo Snapshot wake-up itself.
	// DemoVirtualDisk resource restore reads SnapshotContent via APIReader only (get RBAC, no informer).
	return ctrl.NewControllerManagedBy(mgr).
		For(&demov1alpha1.DemoVirtualDiskSnapshot{}).
		// Independent disk snapshots (one per disk, several per set) fan out under a multi-set burst; a
		// single worker serializes their planning + capture-request creation. Each reconcile writes only
		// its own snapshot's status and keeps no shared mutable state, so parallel workers are safe.
		WithOptions(controller.Options{MaxConcurrentReconciles: 4}).
		Complete(&DemoVirtualDiskSnapshotReconciler{
			Client:    mgr.GetClient(),
			APIReader: mgr.GetAPIReader(),
			Config:    cfg,
		})
}

func (r *DemoVirtualDiskSnapshotReconciler) capture() snapshotsdk.CaptureSDK {
	return snapshotsdk.New(r.Client, r.APIReader, snapshotsdk.NewStorageFoundationProvider(r.Client))
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

	// Import mode (C5): spec.mode: Import switches this disk snapshot off capture. The domain controller
	// does NO capture planning (no source-disk lookup, no MCR/VCR) — the live DemoVirtualDisk may be
	// absent on import. The common controller materializes the backing SnapshotContent from the uploaded
	// manifests (reconstructed ManifestCheckpoint) and the data leg from the matching DataImport
	// (reverse-lookup by spec.snapshotRef). Domain planning is trivially complete for an import leaf.
	if s.IsImportMode() {
		return ctrl.Result{}, nil
	}

	adapter := demoVirtualDiskSnapshotAdapter{snap: s}
	sdk := r.capture()

	resolution := resolveDemoSnapshotSource(controllercommon.KindDemoVirtualDisk, s.Spec.SourceRef)
	if resolution.Reason != "" {
		return ctrl.Result{}, sdk.Reject(ctx, adapter, snapshotsdk.FailSpec{Reason: snapshotsdk.Reason(resolution.Reason), Message: resolution.Message})
	}
	source := &demov1alpha1.DemoVirtualDisk{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: resolution.Name}, source); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		// Recoverable, NOT terminal: the captured source may still appear. Surface it as a Pending
		// diagnostic (message-only, phase preserved) and requeue — Fail/Reject would move to the terminal
		// Failed SINK the SDK never leaves, so a source that shows up later could never be captured. This is
		// the pod model: stay Pending with a "waiting for X" note instead of failing.
		if perr := sdk.ReportProgress(ctx, adapter, fmt.Sprintf("waiting for %s %q to exist", controllercommon.KindDemoVirtualDisk, resolution.Name)); perr != nil {
			return ctrl.Result{}, perr
		}
		return ctrl.Result{RequeueAfter: defaultDemoSnapshotRequeueAfter}, nil
	}

	// Publish the captured live source's full reference (top-level status.sourceRef). d8-cli reads it
	// as a self-contained block to rebuild the import-mode source. Not part of the readiness formula.
	if err := sdk.PublishSnapshotSource(ctx, adapter, snapshotsdk.SnapshotSource{
		APIVersion: demov1alpha1.SchemeGroupVersion.String(),
		Kind:       controllercommon.KindDemoVirtualDisk,
		Name:       source.Name,
		Namespace:  source.Namespace,
		UID:        source.UID,
	}); err != nil {
		return ctrl.Result{}, err
	}

	// Data leg (D3): resolve the source disk's single PVC into the data-leg target (domain decision). A
	// missing PVC is recoverable (the PVC may still appear), NOT terminal: surface it as a Pending
	// diagnostic and requeue rather than entering the terminal Failed sink. A disk without
	// spec.persistentVolumeClaimName is manifest-only (no data leg).
	dataRef, pendingMessage, err := r.resolveDemoVirtualDiskDataRef(ctx, s, source)
	if err != nil {
		return ctrl.Result{}, err
	}
	if pendingMessage != "" {
		if perr := sdk.ReportProgress(ctx, adapter, pendingMessage); perr != nil {
			return ctrl.Result{}, perr
		}
		return ctrl.Result{RequeueAfter: defaultDemoSnapshotRequeueAfter}, nil
	}

	// All source/data preconditions resolved: clear any stale "waiting for X" diagnostic left by a prior
	// reconcile so a recovered snapshot does not keep showing an obsolete Pending note.
	if err := clearDemoProgress(ctx, sdk, adapter); err != nil {
		return ctrl.Result{}, err
	}

	if err := sdk.EnsureVolumeCapture(ctx, adapter, snapshotsdk.VolumeCaptureSpec{DataRef: dataRef}); err != nil {
		return ctrl.Result{}, err
	}

	if err := sdk.EnsureManifestCapture(ctx, adapter, snapshotsdk.ManifestCaptureSpec{Targets: []snapshotsdk.ManifestTarget{{
		APIVersion: demov1alpha1.SchemeGroupVersion.String(),
		Kind:       controllercommon.KindDemoVirtualDisk,
		Name:       source.Name,
	}}}); err != nil {
		return ctrl.Result{}, err
	}

	// Barrier 1 (Planned): for a leaf disk "planning complete" means its own MCR/VCR are created and
	// published (it has no child snapshots). The common controller waits on phase>=Planned before taking
	// over the SnapshotContent.
	if err := sdk.MarkPlanned(ctx, adapter); err != nil {
		return ctrl.Result{}, err
	}

	// Barrier 2 (Finished): switch on the SDK-derived capture outcome. The core flips the commonController
	// leg latches as it captures. A disk is a data-leaf: it confirms consistency immediately once all its
	// declared legs (manifest + data) are captured — there is no freeze/unfreeze showcase to time on a
	// single volume.
	switch outcome := snapshotsdk.CoreCaptureOutcome(adapter); outcome.Outcome {
	case snapshotsdk.CaptureOutcomeCaptured:
		return ctrl.Result{}, sdk.ConfirmConsistent(ctx, adapter)
	case snapshotsdk.CaptureOutcomeFailed:
		// Core-owned terminal (a failed data/manifest leg) is surfaced by the CORE on the Ready condition
		// — the core makes the SnapshotContent itself terminal (VolumeCaptureFailed) and mirrors it here
		// (Variant A). The domain does NOT re-drive it into phase=Failed via Reject: turning a core-owned
		// leg failure into a terminal is the core's job. Nothing left for the domain to do, so stop (the
		// core owns the durable terminal state); requeuing would only spin.
		return ctrl.Result{}, nil
	default:
		// Capturing: wait for the core to finish. The status watch wakes us on each leg latch flip; poll as
		// a fallback in case a signal is missed.
		return ctrl.Result{RequeueAfter: defaultDemoSnapshotRequeueAfter}, nil
	}
}

// resolveDemoVirtualDiskDataRef resolves the source disk's single PVC into the SDK data-leg target. It
// returns a nil ref for a manifest-only disk, or a non-empty pendingMessage when the configured PVC is not
// yet present. A missing PVC is recoverable (Pending), not terminal — the caller surfaces the message via
// ReportProgress and requeues instead of failing.
func (r *DemoVirtualDiskSnapshotReconciler) resolveDemoVirtualDiskDataRef(
	ctx context.Context,
	s *demov1alpha1.DemoVirtualDiskSnapshot,
	source *demov1alpha1.DemoVirtualDisk,
) (dataRef *snapshotsdk.Target, pendingMessage string, err error) {
	pvcName := source.Spec.PersistentVolumeClaimName
	if pvcName == "" {
		return nil, "", nil
	}

	reader := demoReconcilerReader(r.APIReader, r.Client)
	pvc := &corev1.PersistentVolumeClaim{}
	if getErr := reader.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: pvcName}, pvc); getErr != nil {
		if apierrors.IsNotFound(getErr) {
			return nil, fmt.Sprintf("waiting for PersistentVolumeClaim %q (disk data leg) to exist", pvcName), nil
		}
		return nil, "", getErr
	}

	return &snapshotsdk.Target{
		UID:        string(pvc.UID),
		APIVersion: corev1.SchemeGroupVersion.String(),
		Kind:       "PersistentVolumeClaim",
		Name:       pvc.Name,
		Namespace:  pvc.Namespace,
	}, "", nil
}

// clearDemoProgress clears a stale non-terminal Pending diagnostic
// (captureState.domainSpecificController.message) once the domain's "waiting for X" precondition resolves,
// so a recovered snapshot stops showing an obsolete note. It is a no-op when the message is already empty;
// ReportProgress additionally refuses to touch a terminal (Failed) object, so this never disturbs a real
// failure reason/message.
func clearDemoProgress(ctx context.Context, sdk snapshotsdk.CaptureSDK, adapter snapshotsdk.SnapshotAdapter) error {
	if adapter.GetDomainCaptureState().Message == "" {
		return nil
	}
	return sdk.ReportProgress(ctx, adapter, "")
}
