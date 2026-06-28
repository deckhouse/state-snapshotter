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
	"sigs.k8s.io/controller-runtime/pkg/log"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/domain-controller/internal/controllers/common"
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
	// before RBACReady=True is set on CSD.
	//
	// Content-free for SNAPSHOT reconcilers: NO SnapshotContent watch/informer here. The core
	// GenericSnapshotBinderController owns all SnapshotContent work for this DomainCaptureSnapshotKind
	// (creation/projection/Ready mirror) and provides the SnapshotContent -> demo Snapshot wake-up itself.
	// DemoVirtualDisk resource restore reads SnapshotContent via APIReader only (get RBAC, no informer).
	return ctrl.NewControllerManagedBy(mgr).
		For(&demov1alpha1.DemoVirtualDiskSnapshot{}).
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

	// Import mode (C5): spec.dataSource switches this disk snapshot off capture. The domain controller
	// does NO capture planning (no source-disk lookup, no MCR/VCR) — the live DemoVirtualDisk may be
	// absent on import. The common controller materializes the backing SnapshotContent from the uploaded
	// manifests and the data leg from DataImport.status.dataArtifactRef. Domain planning is trivially
	// complete for an import leaf.
	if s.Spec.DataSource != nil {
		return ctrl.Result{}, nil
	}

	adapter := demoVirtualDiskSnapshotAdapter{snap: s}
	sdk := r.capture()

	resolution := resolveDemoSnapshotSource(controllercommon.KindDemoVirtualDisk, s.Spec.SourceRef)
	if resolution.Reason != "" {
		return ctrl.Result{}, sdk.MarkNotReady(ctx, adapter, snapshotsdk.NotReadySpec{Reason: snapshotsdk.Reason(resolution.Reason), Message: resolution.Message})
	}
	source := &demov1alpha1.DemoVirtualDisk{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: resolution.Name}, source); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, sdk.MarkNotReady(ctx, adapter, snapshotsdk.NotReadySpec{
			Reason:  snapshotsdk.Reason(demoReasonSourceNotFound),
			Message: fmt.Sprintf("%s %q not found", controllercommon.KindDemoVirtualDisk, resolution.Name),
		})
	}

	// Data leg (D3): resolve the source disk's single PVC into the data-leg target (domain decision). A
	// missing PVC is an actionable, surfaced Ready=False (the PVC may still appear), not an endless raw
	// requeue. A disk without spec.persistentVolumeClaimName is manifest-only (no data leg).
	dataRef, terminalReason, terminalMessage, err := r.resolveDemoVirtualDiskDataRef(ctx, s, source)
	if err != nil {
		return ctrl.Result{}, err
	}
	if terminalReason != "" {
		if perr := sdk.MarkNotReady(ctx, adapter, snapshotsdk.NotReadySpec{
			Reason:  snapshotsdk.Reason(terminalReason),
			Message: terminalMessage,
			Requeue: true,
		}); perr != nil {
			return ctrl.Result{}, perr
		}
		return ctrl.Result{RequeueAfter: defaultDemoSnapshotRequeueAfter}, nil
	}

	if err := sdk.EnsureVolumeCapture(ctx, adapter, snapshotsdk.VolumeCaptureSpec{DataRef: dataRef}); err != nil {
		return ctrl.Result{}, err
	}

	if err := sdk.EnsureManifestCapture(ctx, adapter, snapshotsdk.ManifestCaptureSpec{
		Targets: []snapshotsdk.ManifestTarget{{
			APIVersion: demov1alpha1.SchemeGroupVersion.String(),
			Kind:       controllercommon.KindDemoVirtualDisk,
			Name:       source.Name,
		}},
	}); err != nil {
		return ctrl.Result{}, err
	}

	// Planning barrier: for a leaf disk "domain planning complete" means its own MCR/VCR are created and
	// published (it has no child snapshots). The common controller waits on this before taking over content.
	return ctrl.Result{}, sdk.MarkPlanningReady(ctx, adapter, "manifest capture request planned")
}

// resolveDemoVirtualDiskDataRef resolves the source disk's single PVC into the SDK data-leg target. It
// returns a nil ref for a manifest-only disk, or a non-empty terminalReason (ArtifactMissing) when the
// configured PVC is not yet present.
func (r *DemoVirtualDiskSnapshotReconciler) resolveDemoVirtualDiskDataRef(
	ctx context.Context,
	s *demov1alpha1.DemoVirtualDiskSnapshot,
	source *demov1alpha1.DemoVirtualDisk,
) (dataRef *snapshotsdk.Target, terminalReason string, terminalMessage string, err error) {
	pvcName := source.Spec.PersistentVolumeClaimName
	if pvcName == "" {
		return nil, "", "", nil
	}

	reader := demoReconcilerReader(r.APIReader, r.Client)
	pvc := &corev1.PersistentVolumeClaim{}
	if getErr := reader.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: pvcName}, pvc); getErr != nil {
		if apierrors.IsNotFound(getErr) {
			return nil, storagev1alpha1.ReasonArtifactMissing, fmt.Sprintf("PersistentVolumeClaim %q not found for disk data leg", pvcName), nil
		}
		return nil, "", "", getErr
	}

	return &snapshotsdk.Target{
		UID:        string(pvc.UID),
		APIVersion: corev1.SchemeGroupVersion.String(),
		Kind:       "PersistentVolumeClaim",
		Name:       pvc.Name,
		Namespace:  pvc.Namespace,
	}, "", "", nil
}
