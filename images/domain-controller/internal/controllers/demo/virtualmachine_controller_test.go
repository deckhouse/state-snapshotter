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
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/domain-controller/pkg/config"
)

func TestDemoVirtualMachineCreatesPod(t *testing.T) {
	size := resource.MustParse("1Gi")
	disk := &demov1alpha1.DemoVirtualDisk{
		ObjectMeta: metav1.ObjectMeta{Namespace: matNS, Name: "disk-vm", UID: types.UID("disk-vm-uid")},
		Spec: demov1alpha1.DemoVirtualDiskSpec{
			PersistentVolumeClaimName: "data-pvc",
			Size:                      &size,
			StorageClassName:          "local-thin",
		},
		Status: demov1alpha1.DemoVirtualDiskStatus{
			Phase: demoPhaseReady,
			Conditions: []metav1.Condition{{
				Type:   storagev1alpha1.ConditionReady,
				Status: metav1.ConditionTrue,
				Reason: storagev1alpha1.ReasonCompleted,
			}},
			PersistentVolumeClaimRef: &demov1alpha1.DemoObjectRef{Name: "data-pvc"},
		},
	}
	vm := &demov1alpha1.DemoVirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Namespace: matNS, Name: "vm-1", UID: types.UID("vm-1-uid")},
		Spec:       demov1alpha1.DemoVirtualMachineSpec{VirtualDiskName: "disk-vm"},
	}
	cl := newMaterializationFakeClient(t, disk, vm)
	r := &DemoVirtualMachineReconciler{Client: cl, APIReader: cl, Config: &config.Options{DemoPodImage: "busybox:1.36"}}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(vm)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	podName := demoVMPodName(vm.UID)
	pod := &corev1.Pod{}
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: matNS, Name: podName}, pod); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if pod.Spec.Containers[0].Image != "busybox:1.36" {
		t.Fatalf("image = %q", pod.Spec.Containers[0].Image)
	}
	sc := pod.Spec.Containers[0].SecurityContext
	if sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Fatal("expected PSA-compatible runAsNonRoot on container")
	}
	if sc.RunAsUser == nil || *sc.RunAsUser == 0 {
		t.Fatal("expected explicit non-zero runAsUser on container (kubelet cannot verify non-root for busybox otherwise)")
	}
}

func TestDemoVirtualMachineReconcileIsIdempotent(t *testing.T) {
	disk := &demov1alpha1.DemoVirtualDisk{
		ObjectMeta: metav1.ObjectMeta{Namespace: matNS, Name: "disk-vm"},
		Spec:       demov1alpha1.DemoVirtualDiskSpec{PersistentVolumeClaimName: "data-pvc"},
		Status: demov1alpha1.DemoVirtualDiskStatus{
			Phase: demoPhaseReady,
			Conditions: []metav1.Condition{{
				Type:   storagev1alpha1.ConditionReady,
				Status: metav1.ConditionTrue,
			}},
			PersistentVolumeClaimRef: &demov1alpha1.DemoObjectRef{Name: "data-pvc"},
		},
	}
	vm := &demov1alpha1.DemoVirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Namespace: matNS, Name: "vm-1", UID: types.UID("vm-idem-uid")},
		Spec:       demov1alpha1.DemoVirtualMachineSpec{VirtualDiskName: "disk-vm"},
	}
	pod := buildDemoVMPod(vm, disk, demoVMPodName(vm.UID), "data-pvc", "busybox:1.36")
	pod.Status.Phase = corev1.PodRunning
	cl := newMaterializationFakeClient(t, disk, vm, pod)
	r := &DemoVirtualMachineReconciler{Client: cl, APIReader: cl, Config: &config.Options{}}

	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(vm)}); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	pods := &corev1.PodList{}
	if err := cl.List(context.Background(), pods, client.InNamespace(matNS)); err != nil {
		t.Fatalf("list pods: %v", err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("pod count = %d, want 1", len(pods.Items))
	}
	freshVM := &demov1alpha1.DemoVirtualMachine{}
	if err := cl.Get(context.Background(), client.ObjectKeyFromObject(vm), freshVM); err != nil {
		t.Fatalf("get vm: %v", err)
	}
	if freshVM.Status.Phase != demoPhaseReady {
		t.Fatalf("vm phase = %q", freshVM.Status.Phase)
	}
	cond := meta.FindStatusCondition(freshVM.Status.Conditions, storagev1alpha1.ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("ready condition = %#v", cond)
	}
}
