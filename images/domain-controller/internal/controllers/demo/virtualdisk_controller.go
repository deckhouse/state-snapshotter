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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/domain-controller/internal/controllers/common"
	"github.com/deckhouse/state-snapshotter/images/domain-controller/pkg/config"
)

// DemoVirtualDiskReconciler materializes the backing PVC for a DemoVirtualDisk (scratch or restored from
// a DemoVirtualDiskSnapshot). Restore resolves SnapshotContent.status.dataRef via APIReader (uncached)
// and creates a storage-foundation VolumeRestoreRequest directly; snapshot reconcilers stay content-free.
type DemoVirtualDiskReconciler struct {
	Client    client.Client
	APIReader client.Reader
	Config    *config.Options
}

func AddDemoVirtualDiskControllerToManager(mgr ctrl.Manager, cfg *config.Options) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&demov1alpha1.DemoVirtualDisk{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Complete(&DemoVirtualDiskReconciler{
			Client:    mgr.GetClient(),
			APIReader: mgr.GetAPIReader(),
			Config:    cfg,
		})
}

func (r *DemoVirtualDiskReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("demoVirtualDisk", req.NamespacedName)
	ctx = log.IntoContext(ctx, logger)

	disk := &demov1alpha1.DemoVirtualDisk{}
	if err := r.Client.Get(ctx, req.NamespacedName, disk); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if disk.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	pvcName := disk.Spec.PersistentVolumeClaimName
	if pvcName == "" {
		if err := patchDemoVirtualDiskStatus(ctx, r.Client, req.NamespacedName, demoPhaseReady, metav1.ConditionTrue, storagev1alpha1.ReasonCompleted, "manifest-only disk (no PVC)", nil, nil); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if disk.Spec.DataSource != nil {
		return r.reconcileRestoreDisk(ctx, req.NamespacedName, disk, pvcName)
	}
	return r.reconcileScratchDisk(ctx, req.NamespacedName, disk, pvcName)
}

func (r *DemoVirtualDiskReconciler) reconcileScratchDisk(ctx context.Context, nn types.NamespacedName, disk *demov1alpha1.DemoVirtualDisk, pvcName string) (ctrl.Result, error) {
	if disk.Spec.Size == nil || disk.Spec.Size.IsZero() {
		if err := patchDemoVirtualDiskStatus(ctx, r.Client, nn, demoPhaseFailed, metav1.ConditionFalse, demoReasonMissingSize, "spec.size is required for scratch disks", nil, nil); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}
	if disk.Spec.StorageClassName == "" {
		if err := patchDemoVirtualDiskStatus(ctx, r.Client, nn, demoPhaseFailed, metav1.ConditionFalse, demoReasonMissingStorageClass, "spec.storageClassName is required for scratch disks", nil, nil); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}
	volumeMode, err := normalizeDemoVolumeMode(disk.Spec.VolumeMode)
	if err != nil {
		if patchErr := patchDemoVirtualDiskStatus(ctx, r.Client, nn, demoPhaseFailed, metav1.ConditionFalse, demoReasonInvalidRestoreSpec, err.Error(), nil, nil); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{}, nil
	}

	pvc := &corev1.PersistentVolumeClaim{}
	pvcNN := types.NamespacedName{Namespace: disk.Namespace, Name: pvcName}
	getErr := r.Client.Get(ctx, pvcNN, pvc)
	if getErr != nil {
		if !apierrors.IsNotFound(getErr) {
			return ctrl.Result{}, getErr
		}
		pvc = buildScratchPVC(disk, pvcName, volumeMode)
		if createErr := r.Client.Create(ctx, pvc); createErr != nil {
			return ctrl.Result{}, createErr
		}
		if err := patchDemoVirtualDiskStatus(ctx, r.Client, nn, demoPhasePending, metav1.ConditionFalse, demoReasonPVCNotReady, fmt.Sprintf("waiting for PVC %q to bind", pvcName), &demov1alpha1.DemoObjectRef{Name: pvcName}, nil); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: defaultDemoResourceRequeueAfter}, nil
	}

	if !diskHasControllerOwner(pvc, disk) {
		if adoptErr := adoptDemoDiskPVC(ctx, r.Client, disk, pvc); adoptErr != nil {
			return ctrl.Result{}, adoptErr
		}
	}

	return r.publishDiskReadyFromPVC(ctx, nn, disk, pvc, pvcName)
}

func buildScratchPVC(disk *demov1alpha1.DemoVirtualDisk, pvcName string, volumeMode corev1.PersistentVolumeMode) *corev1.PersistentVolumeClaim {
	sc := disk.Spec.StorageClassName
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: disk.Namespace,
			Labels: map[string]string{
				demoLabelManagedBy: demoManagedByValue,
				demoLabelDiskName:  disk.Name,
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(disk, demov1alpha1.SchemeGroupVersion.WithKind(controllercommon.KindDemoVirtualDisk)),
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: *disk.Spec.Size,
				},
			},
			StorageClassName: &sc,
			VolumeMode:       &volumeMode,
		},
	}
	return pvc
}

func (r *DemoVirtualDiskReconciler) reconcileRestoreDisk(ctx context.Context, nn types.NamespacedName, disk *demov1alpha1.DemoVirtualDisk, pvcName string) (ctrl.Result, error) {
	resolution, err := resolveDemoDiskRestore(ctx, r.APIReader, disk)
	if err != nil {
		return ctrl.Result{}, err
	}
	if resolution.Failed {
		if patchErr := patchDemoVirtualDiskStatus(ctx, r.Client, nn, demoPhaseFailed, metav1.ConditionFalse, resolution.Reason, resolution.Message, nil, nil); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{}, nil
	}
	if !resolution.Ready {
		if patchErr := patchDemoVirtualDiskStatus(ctx, r.Client, nn, demoPhasePending, metav1.ConditionFalse, resolution.Reason, resolution.Message, &demov1alpha1.DemoObjectRef{Name: pvcName}, nil); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{RequeueAfter: defaultDemoResourceRequeueAfter}, nil
	}

	if err := ensureDemoDiskVRR(ctx, r.Client, disk, resolution); err != nil {
		return ctrl.Result{}, err
	}

	pvc := &corev1.PersistentVolumeClaim{}
	pvcNN := types.NamespacedName{Namespace: disk.Namespace, Name: pvcName}
	if err := r.Client.Get(ctx, pvcNN, pvc); err != nil {
		if apierrors.IsNotFound(err) {
			if patchErr := patchDemoVirtualDiskStatus(ctx, r.Client, nn, demoPhasePending, metav1.ConditionFalse, demoReasonRestorePending, fmt.Sprintf("waiting for PVC %q from VolumeRestoreRequest", pvcName), &demov1alpha1.DemoObjectRef{Name: pvcName}, nil); patchErr != nil {
				return ctrl.Result{}, patchErr
			}
			return ctrl.Result{RequeueAfter: defaultDemoResourceRequeueAfter}, nil
		}
		return ctrl.Result{}, err
	}

	if keeperStillControlsPVC(pvc) || !diskHasControllerOwner(pvc, disk) {
		if adoptErr := adoptDemoDiskPVC(ctx, r.Client, disk, pvc); adoptErr != nil {
			return ctrl.Result{}, adoptErr
		}
		if err := r.Client.Get(ctx, pvcNN, pvc); err != nil {
			return ctrl.Result{}, err
		}
	}

	if !diskHasControllerOwner(pvc, disk) {
		if patchErr := patchDemoVirtualDiskStatus(ctx, r.Client, nn, demoPhasePending, metav1.ConditionFalse, demoReasonPVCNotReady, fmt.Sprintf("waiting to adopt PVC %q", pvcName), &demov1alpha1.DemoObjectRef{Name: pvcName}, nil); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{RequeueAfter: defaultDemoResourceRequeueAfter}, nil
	}

	if err := deleteDemoDiskVRR(ctx, r.Client, disk); err != nil {
		return ctrl.Result{}, err
	}

	return r.publishDiskReadyFromPVC(ctx, nn, disk, pvc, pvcName)
}

func (r *DemoVirtualDiskReconciler) publishDiskReadyFromPVC(
	ctx context.Context,
	nn types.NamespacedName,
	disk *demov1alpha1.DemoVirtualDisk,
	pvc *corev1.PersistentVolumeClaim,
	pvcName string,
) (ctrl.Result, error) {
	if !pvcIsBound(pvc) {
		if err := patchDemoVirtualDiskStatus(ctx, r.Client, nn, demoPhasePending, metav1.ConditionFalse, demoReasonPVCNotReady, fmt.Sprintf("waiting for PVC %q to bind", pvcName), &demov1alpha1.DemoObjectRef{Name: pvcName}, nil); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: defaultDemoResourceRequeueAfter}, nil
	}
	capacity := pvcCapacityCopy(pvc)
	if err := patchDemoVirtualDiskStatus(ctx, r.Client, nn, demoPhaseReady, metav1.ConditionTrue, storagev1alpha1.ReasonCompleted, fmt.Sprintf("PVC %q is ready", pvcName), &demov1alpha1.DemoObjectRef{Name: pvcName}, capacity); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}
