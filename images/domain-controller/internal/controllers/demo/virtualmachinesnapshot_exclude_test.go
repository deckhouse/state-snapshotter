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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/domain-controller/internal/controllers/common"
	"github.com/deckhouse/state-snapshotter/images/domain-controller/pkg/config"
)

// A VM-owned disk carrying the exclude veto label is dropped from the planned children and recorded as an
// excluded source ref instead; an unlabeled disk is planned as a child normally.
func TestPlanDemoVirtualMachineChildren_ExcludeVeto(t *testing.T) {
	ctx := context.Background()

	run := func(t *testing.T, vetoed bool) ([]string, []storagev1alpha1.ExcludedObjectRef) {
		t.Helper()
		diskLabels := map[string]string{}
		if vetoed {
			diskLabels[storagev1alpha1.ExcludeLabelKey] = ""
		}
		disk := &demov1alpha1.DemoVirtualDisk{
			ObjectMeta: metav1.ObjectMeta{Namespace: matNS, Name: "disk-vm", Labels: diskLabels},
			Spec:       demov1alpha1.DemoVirtualDiskSpec{PersistentVolumeClaimName: "data-pvc"},
		}
		vm := &demov1alpha1.DemoVirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Namespace: matNS, Name: "vm-1"},
			Spec:       demov1alpha1.DemoVirtualMachineSpec{VirtualDiskName: "disk-vm"},
		}
		vmSnap := &demov1alpha1.DemoVirtualMachineSnapshot{
			ObjectMeta: metav1.ObjectMeta{Namespace: matNS, Name: "vmsnap-1"},
		}
		cl := newMaterializationFakeClient(t, disk, vm)
		r := &DemoVirtualMachineSnapshotReconciler{Client: cl, APIReader: cl, Config: &config.Options{}}

		children, excluded, err := r.planDemoVirtualMachineChildren(ctx, vmSnap, vm)
		if err != nil {
			t.Fatalf("planDemoVirtualMachineChildren: %v", err)
		}
		names := make([]string, 0, len(children))
		for _, c := range children {
			names = append(names, c.Object.GetName())
		}
		return names, excluded
	}

	t.Run("unlabeled disk is planned as a child", func(t *testing.T) {
		children, excluded := run(t, false)
		if len(children) != 1 {
			t.Fatalf("children = %v, want exactly one child snapshot", children)
		}
		if len(excluded) != 0 {
			t.Fatalf("excluded = %+v, want none", excluded)
		}
	})

	t.Run("vetoed disk is dropped and recorded as excluded", func(t *testing.T) {
		children, excluded := run(t, true)
		if len(children) != 0 {
			t.Fatalf("children = %v, want none (disk vetoed)", children)
		}
		if len(excluded) != 1 {
			t.Fatalf("excluded = %+v, want exactly one excluded ref", excluded)
		}
		got := excluded[0]
		if got.Kind != controllercommon.KindDemoVirtualDisk || got.Name != "disk-vm" ||
			got.APIVersion != demov1alpha1.SchemeGroupVersion.String() {
			t.Fatalf("excluded[0] = %+v, want DemoVirtualDisk/disk-vm", got)
		}
	})
}

// A StaticBind VM snapshot must NOT run capture: the domain reconciler no-ops (the core binds it to a
// surviving SnapshotContent). No child snapshots are created and no domain capture phase is stamped.
func TestDemoVirtualMachineSnapshot_StaticBindSkipsCapture(t *testing.T) {
	ctx := context.Background()
	vmSnap := &demov1alpha1.DemoVirtualMachineSnapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: matNS, Name: "vmsnap-sb"},
		Spec: demov1alpha1.DemoVirtualMachineSnapshotSpec{
			Mode: storagev1alpha1.SnapshotModeStaticBind,
		},
	}
	cl := newMaterializationFakeClient(t, vmSnap)
	r := &DemoVirtualMachineSnapshotReconciler{Client: cl, APIReader: cl, Config: &config.Options{}}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(vmSnap)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	fresh := &demov1alpha1.DemoVirtualMachineSnapshot{}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: matNS, Name: "vmsnap-sb"}, fresh); err != nil {
		t.Fatalf("get vmsnap: %v", err)
	}
	// No live capture ran: the domain phase is never stamped in StaticBind mode.
	if fresh.Status.CaptureState != nil && fresh.Status.CaptureState.DomainSpecificController != nil &&
		fresh.Status.CaptureState.DomainSpecificController.Phase != "" {
		t.Fatalf("StaticBind must not stamp a domain phase, got %q",
			fresh.Status.CaptureState.DomainSpecificController.Phase)
	}
	children := &demov1alpha1.DemoVirtualDiskSnapshotList{}
	if err := cl.List(ctx, children, client.InNamespace(matNS)); err != nil {
		t.Fatalf("list disk snapshots: %v", err)
	}
	if len(children.Items) != 0 {
		t.Fatalf("StaticBind must not create child snapshots, got %d", len(children.Items))
	}
}
