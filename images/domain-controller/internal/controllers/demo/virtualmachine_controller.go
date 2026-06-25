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
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/domain-controller/internal/controllers/common"
	"github.com/deckhouse/state-snapshotter/images/domain-controller/pkg/config"
)

// DemoVirtualMachineReconciler materializes a demo Pod that mounts the PVC of the linked DemoVirtualDisk.
type DemoVirtualMachineReconciler struct {
	Client    client.Client
	APIReader client.Reader
	Config    *config.Options
}

func AddDemoVirtualMachineControllerToManager(mgr ctrl.Manager, cfg *config.Options) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&demov1alpha1.DemoVirtualMachine{}).
		Owns(&corev1.Pod{}).
		Watches(&demov1alpha1.DemoVirtualDisk{}, handler.EnqueueRequestsFromMapFunc(mapDemoVirtualDiskToVMs(mgr.GetClient()))).
		Complete(&DemoVirtualMachineReconciler{
			Client:    mgr.GetClient(),
			APIReader: mgr.GetAPIReader(),
			Config:    cfg,
		})
}

func mapDemoVirtualDiskToVMs(c client.Client) handler.MapFunc {
	return func(ctx context.Context, o client.Object) []reconcile.Request {
		disk, ok := o.(*demov1alpha1.DemoVirtualDisk)
		if !ok {
			return nil
		}
		list := &demov1alpha1.DemoVirtualMachineList{}
		if err := c.List(ctx, list, client.InNamespace(disk.Namespace)); err != nil {
			return nil
		}
		var out []reconcile.Request
		for i := range list.Items {
			vm := &list.Items[i]
			if vm.Spec.VirtualDiskName == disk.Name {
				out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: vm.Namespace, Name: vm.Name}})
			}
		}
		return out
	}
}

func demoVMPodName(vmUID types.UID) string {
	sum := sha256.Sum256([]byte("demo-vm-pod:" + string(vmUID)))
	return "demo-vm-" + hex.EncodeToString(sum[:8])
}

func (r *DemoVirtualMachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("demoVirtualMachine", req.NamespacedName)
	ctx = log.IntoContext(ctx, logger)

	vm := &demov1alpha1.DemoVirtualMachine{}
	if err := r.Client.Get(ctx, req.NamespacedName, vm); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if vm.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	diskName := vm.Spec.VirtualDiskName
	if diskName == "" {
		if err := patchDemoVirtualMachineStatus(ctx, r.Client, req.NamespacedName, demoPhaseReady, metav1.ConditionTrue, storagev1alpha1.ReasonCompleted, "manifest-only VM (no virtualDiskName)", nil); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	disk := &demov1alpha1.DemoVirtualDisk{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: vm.Namespace, Name: diskName}, disk); err != nil {
		if apierrors.IsNotFound(err) {
			if patchErr := patchDemoVirtualMachineStatus(ctx, r.Client, req.NamespacedName, demoPhaseFailed, metav1.ConditionFalse, demoReasonDiskNotReady, fmt.Sprintf("%s %q not found", controllercommon.KindDemoVirtualDisk, diskName), nil); patchErr != nil {
				return ctrl.Result{}, patchErr
			}
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	readyCond := meta.FindStatusCondition(disk.Status.Conditions, storagev1alpha1.ConditionReady)
	if readyCond == nil || readyCond.Status != metav1.ConditionTrue || disk.Status.Phase != demoPhaseReady {
		msg := fmt.Sprintf("waiting for %s %q to become Ready", controllercommon.KindDemoVirtualDisk, diskName)
		if readyCond != nil && readyCond.Message != "" {
			msg = readyCond.Message
		}
		if patchErr := patchDemoVirtualMachineStatus(ctx, r.Client, req.NamespacedName, demoPhasePending, metav1.ConditionFalse, demoReasonDiskNotReady, msg, nil); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{RequeueAfter: defaultDemoResourceRequeueAfter}, nil
	}

	pvcName := disk.Spec.PersistentVolumeClaimName
	if pvcName == "" || disk.Status.PersistentVolumeClaimRef == nil {
		if err := patchDemoVirtualMachineStatus(ctx, r.Client, req.NamespacedName, demoPhaseReady, metav1.ConditionTrue, storagev1alpha1.ReasonCompleted, "linked disk has no PVC (manifest-only)", nil); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	podName := demoVMPodName(vm.UID)
	podNN := types.NamespacedName{Namespace: vm.Namespace, Name: podName}
	pod := &corev1.Pod{}
	if err := r.Client.Get(ctx, podNN, pod); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		// The container must attach the backing PVC with the API that matches its volumeMode: a Block
		// PVC is a raw block device (volumeDevices), a Filesystem PVC is a mount (volumeMounts). Mixing
		// them makes the kubelet refuse the Pod ("volume has volumeMode Block, but is specified in
		// volumeMounts"). Read the real PVC mode (authoritative for both blank and restored disks)
		// instead of assuming Filesystem.
		pvc := &corev1.PersistentVolumeClaim{}
		if err := r.Client.Get(ctx, types.NamespacedName{Namespace: vm.Namespace, Name: pvcName}, pvc); err != nil {
			if apierrors.IsNotFound(err) {
				if patchErr := patchDemoVirtualMachineStatus(ctx, r.Client, req.NamespacedName, demoPhasePending, metav1.ConditionFalse, demoReasonPVCNotReady, fmt.Sprintf("waiting for PVC %q to exist", pvcName), nil); patchErr != nil {
					return ctrl.Result{}, patchErr
				}
				return ctrl.Result{RequeueAfter: defaultDemoResourceRequeueAfter}, nil
			}
			return ctrl.Result{}, err
		}
		blockMode := pvc.Spec.VolumeMode != nil && *pvc.Spec.VolumeMode == corev1.PersistentVolumeBlock

		pod = buildDemoVMPod(vm, disk, podName, pvcName, r.Config.DemoPodImage, blockMode)
		if createErr := r.Client.Create(ctx, pod); createErr != nil {
			return ctrl.Result{}, createErr
		}
		if patchErr := patchDemoVirtualMachineStatus(ctx, r.Client, req.NamespacedName, demoPhasePending, metav1.ConditionFalse, demoReasonPVCNotReady, fmt.Sprintf("waiting for Pod %q to become Ready", podName), &demov1alpha1.DemoObjectRef{Name: podName}); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{RequeueAfter: defaultDemoResourceRequeueAfter}, nil
	}

	if pod.Status.Phase != corev1.PodRunning && pod.Status.Phase != corev1.PodSucceeded {
		if patchErr := patchDemoVirtualMachineStatus(ctx, r.Client, req.NamespacedName, demoPhasePending, metav1.ConditionFalse, demoReasonPVCNotReady, fmt.Sprintf("waiting for Pod %q phase %q", podName, pod.Status.Phase), &demov1alpha1.DemoObjectRef{Name: podName}); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{RequeueAfter: defaultDemoResourceRequeueAfter}, nil
	}

	if err := patchDemoVirtualMachineStatus(ctx, r.Client, req.NamespacedName, demoPhaseReady, metav1.ConditionTrue, storagev1alpha1.ReasonCompleted, fmt.Sprintf("Pod %q is running", podName), &demov1alpha1.DemoObjectRef{Name: podName}); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// demoVMBlockDevicePath is where a Block-mode disk is exposed inside the demo Pod. It matches the
// device path the e2e block-data probes use so the same volume is addressable consistently.
const demoVMBlockDevicePath = "/dev/xvda"

func buildDemoVMPod(vm *demov1alpha1.DemoVirtualMachine, disk *demov1alpha1.DemoVirtualDisk, podName, pvcName, image string, block bool) *corev1.Pod {
	if image == "" {
		image = "busybox:1.36"
	}
	runAsNonRoot := true
	// runAsUser is required: PSA "restricted" only mandates runAsNonRoot, but the kubelet cannot verify a
	// non-root UID for an image that defaults to root (busybox), so it refuses to start the container with
	// CreateContainerConfigError. Pin an explicit non-zero UID so the Pod actually runs.
	runAsUser := int64(1000)
	allowPrivilegeEscalation := false
	seccompProfile := &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
	container := corev1.Container{
		Name:    "demo",
		Image:   image,
		Command: []string{"sleep"},
		Args:    []string{"infinity"},
		SecurityContext: &corev1.SecurityContext{
			RunAsNonRoot:             &runAsNonRoot,
			RunAsUser:                &runAsUser,
			AllowPrivilegeEscalation: &allowPrivilegeEscalation,
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
			},
			SeccompProfile: seccompProfile,
		},
	}
	// A Block PVC is attached as a raw block device (volumeDevices); a Filesystem PVC is attached as a
	// mount (volumeMounts). Using the wrong one makes the kubelet refuse the Pod.
	if block {
		container.VolumeDevices = []corev1.VolumeDevice{
			{Name: "data", DevicePath: demoVMBlockDevicePath},
		}
	} else {
		container.VolumeMounts = []corev1.VolumeMount{
			{Name: "data", MountPath: "/data"},
		}
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: vm.Namespace,
			Labels: map[string]string{
				demoLabelManagedBy: demoManagedByValue,
				demoLabelVMName:    vm.Name,
				demoLabelDiskName:  disk.Name,
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(vm, demov1alpha1.SchemeGroupVersion.WithKind(controllercommon.KindDemoVirtualMachine)),
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   &runAsNonRoot,
				RunAsUser:      &runAsUser,
				SeccompProfile: seccompProfile,
			},
			Containers: []corev1.Container{container},
			Volumes: []corev1.Volume{
				{
					Name: "data",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: pvcName,
						},
					},
				},
			},
		},
	}
}
