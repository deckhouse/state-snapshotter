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

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

func patchDemoVirtualDiskStatus(
	ctx context.Context,
	c client.Client,
	nn types.NamespacedName,
	phase string,
	readyStatus metav1.ConditionStatus,
	reason, message string,
	pvcRef *demov1alpha1.DemoObjectRef,
	capacity map[string]resource.Quantity,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		disk := &demov1alpha1.DemoVirtualDisk{}
		if err := c.Get(ctx, nn, disk); err != nil {
			return err
		}
		base := disk.DeepCopy()
		disk.Status.Phase = phase
		meta.SetStatusCondition(&disk.Status.Conditions, metav1.Condition{
			Type:               storagev1alpha1.ConditionReady,
			Status:             readyStatus,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: disk.Generation,
		})
		disk.Status.PersistentVolumeClaimRef = pvcRef
		if capacity != nil {
			disk.Status.Capacity = capacity
		}
		return c.Status().Patch(ctx, disk, client.MergeFrom(base))
	})
}

func patchDemoVirtualMachineStatus(
	ctx context.Context,
	c client.Client,
	nn types.NamespacedName,
	phase string,
	readyStatus metav1.ConditionStatus,
	reason, message string,
	podRef *demov1alpha1.DemoObjectRef,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		vm := &demov1alpha1.DemoVirtualMachine{}
		if err := c.Get(ctx, nn, vm); err != nil {
			return err
		}
		base := vm.DeepCopy()
		vm.Status.Phase = phase
		meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{
			Type:               storagev1alpha1.ConditionReady,
			Status:             readyStatus,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: vm.Generation,
		})
		vm.Status.PodRef = podRef
		return c.Status().Patch(ctx, vm, client.MergeFrom(base))
	})
}
