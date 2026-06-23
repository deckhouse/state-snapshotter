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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrl "sigs.k8s.io/controller-runtime"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/domain-controller/internal/controllers/common"
	"github.com/deckhouse/state-snapshotter/images/domain-controller/pkg/config"
)

const matNS = "ns-mat"

func TestDemoVirtualDiskScratchCreatesPVC(t *testing.T) {
	size := resource.MustParse("1Gi")
	disk := &demov1alpha1.DemoVirtualDisk{
		ObjectMeta: metav1.ObjectMeta{Namespace: matNS, Name: "disk-scratch", UID: types.UID("disk-scratch-uid")},
		Spec: demov1alpha1.DemoVirtualDiskSpec{
			PersistentVolumeClaimName: "scratch-pvc",
			Size:                      &size,
			StorageClassName:          "local-thin",
			VolumeMode:                string(corev1.PersistentVolumeFilesystem),
		},
	}
	cl := newMaterializationFakeClient(t, disk)
	r := &DemoVirtualDiskReconciler{Client: cl, APIReader: cl, Config: &config.Options{}}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(disk)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	pvc := &corev1.PersistentVolumeClaim{}
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: matNS, Name: "scratch-pvc"}, pvc); err != nil {
		t.Fatalf("get pvc: %v", err)
	}
	if !diskHasControllerOwner(pvc, disk) {
		t.Fatal("expected disk controller owner on scratch PVC")
	}
}

func TestDemoVirtualDiskRestoreRejectsCrossNamespace(t *testing.T) {
	disk := &demov1alpha1.DemoVirtualDisk{
		ObjectMeta: metav1.ObjectMeta{Namespace: matNS, Name: "disk-restore", UID: types.UID("disk-restore-uid")},
		Spec: demov1alpha1.DemoVirtualDiskSpec{
			PersistentVolumeClaimName: "restored-pvc",
			DataSource: &demov1alpha1.DemoVirtualDiskDataSource{
				Kind: controllercommon.KindDemoVirtualDiskSnapshot,
				Name: "src-snap",
			},
		},
	}
	snap := &demov1alpha1.DemoVirtualDiskSnapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: matNS, Name: "src-snap"},
		Status: demov1alpha1.DemoVirtualDiskSnapshotStatus{
			BoundSnapshotContentName: "content-1",
		},
	}
	content := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "content-1"},
		Spec: storagev1alpha1.SnapshotContentSpec{
			SnapshotRef: &storagev1alpha1.SnapshotSubjectRef{
				Namespace: matNS,
				Name:      "src-snap",
			},
		},
		Status: storagev1alpha1.SnapshotContentStatus{
			DataRef: &storagev1alpha1.SnapshotDataBinding{
				Target: storagev1alpha1.SnapshotSubjectRef{Namespace: "other-ns", Name: "orig-pvc"},
				Artifact: storagev1alpha1.SnapshotDataArtifactRef{
					Kind: vscKind,
					Name: "vsc-1",
				},
				VolumeMode:       string(corev1.PersistentVolumeFilesystem),
				StorageClassName: "local-thin",
			},
		},
	}
	cl := newMaterializationFakeClient(t, disk, snap, content)
	r := &DemoVirtualDiskReconciler{Client: cl, APIReader: cl}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(disk)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	fresh := &demov1alpha1.DemoVirtualDisk{}
	if err := cl.Get(context.Background(), client.ObjectKeyFromObject(disk), fresh); err != nil {
		t.Fatalf("get disk: %v", err)
	}
	if fresh.Status.Phase != demoPhaseFailed {
		t.Fatalf("phase = %q, want Failed", fresh.Status.Phase)
	}
	cond := meta.FindStatusCondition(fresh.Status.Conditions, storagev1alpha1.ConditionReady)
	if cond == nil || cond.Reason != demoReasonRestoreDenied {
		t.Fatalf("condition = %#v, want RestoreDenied", cond)
	}
}

func TestDemoVirtualDiskRestoreVRRHasNoSize(t *testing.T) {
	disk := &demov1alpha1.DemoVirtualDisk{
		ObjectMeta: metav1.ObjectMeta{Namespace: matNS, Name: "disk-vrr", UID: types.UID("disk-vrr-uid")},
		Spec: demov1alpha1.DemoVirtualDiskSpec{
			PersistentVolumeClaimName: "restored-pvc",
			DataSource: &demov1alpha1.DemoVirtualDiskDataSource{
				Kind: controllercommon.KindDemoVirtualDiskSnapshot,
				Name: "src-snap",
			},
		},
	}
	snap := &demov1alpha1.DemoVirtualDiskSnapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: matNS, Name: "src-snap"},
		Status: demov1alpha1.DemoVirtualDiskSnapshotStatus{
			BoundSnapshotContentName: "content-1",
		},
	}
	content := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "content-1"},
		Spec: storagev1alpha1.SnapshotContentSpec{
			SnapshotRef: &storagev1alpha1.SnapshotSubjectRef{Namespace: matNS, Name: "src-snap"},
		},
		Status: storagev1alpha1.SnapshotContentStatus{
			DataRef: &storagev1alpha1.SnapshotDataBinding{
				Target: storagev1alpha1.SnapshotSubjectRef{Namespace: matNS, Name: "orig-pvc"},
				Artifact: storagev1alpha1.SnapshotDataArtifactRef{
					Kind: vscKind,
					Name: "vsc-1",
				},
				VolumeMode:       string(corev1.PersistentVolumeFilesystem),
				StorageClassName: "local-thin",
			},
		},
	}
	cl := newMaterializationFakeClient(t, disk, snap, content)
	r := &DemoVirtualDiskReconciler{Client: cl, APIReader: cl}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(disk)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	vrr := &unstructured.Unstructured{}
	vrr.SetGroupVersionKind(schema.GroupVersionKind{Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: vrrKind})
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: matNS, Name: demoDiskVRRName(disk.UID)}, vrr); err != nil {
		t.Fatalf("get vrr: %v", err)
	}
	if _, ok, _ := unstructured.NestedString(vrr.Object, "spec", "size"); ok {
		t.Fatal("VRR spec must not contain size")
	}
}

func TestAdoptDemoDiskPVCDropsKeeperController(t *testing.T) {
	disk := &demov1alpha1.DemoVirtualDisk{
		ObjectMeta: metav1.ObjectMeta{Namespace: matNS, Name: "disk-adopt", UID: types.UID("disk-adopt-uid")},
	}
	keeperController := true
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: matNS,
			Name:      "restored-pvc",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: objectKeeperAPIVersion,
				Kind:       objectKeeperKind,
				Name:       "keeper-1",
				UID:        types.UID("keeper-uid"),
				Controller: &keeperController,
			}},
		},
	}
	cl := newMaterializationFakeClient(t, disk, pvc)
	if err := adoptDemoDiskPVC(context.Background(), cl, disk, pvc); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	fresh := &corev1.PersistentVolumeClaim{}
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: matNS, Name: "restored-pvc"}, fresh); err != nil {
		t.Fatalf("get pvc: %v", err)
	}
	if keeperStillControlsPVC(fresh) {
		t.Fatal("keeper must not remain controller after adoption")
	}
	if !diskHasControllerOwner(fresh, disk) {
		t.Fatal("disk must become controller owner after adoption")
	}
}

func TestResolveDemoDiskRestoreRejectsCloneKind(t *testing.T) {
	disk := &demov1alpha1.DemoVirtualDisk{
		ObjectMeta: metav1.ObjectMeta{Namespace: matNS, Name: "disk-clone"},
		Spec: demov1alpha1.DemoVirtualDiskSpec{
			DataSource: &demov1alpha1.DemoVirtualDiskDataSource{
				Kind: controllercommon.KindDemoVirtualDisk,
				Name: "other-disk",
			},
		},
	}
	res, err := resolveDemoDiskRestore(context.Background(), newMaterializationFakeClient(t), disk)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !res.Failed || res.Reason != demoReasonInvalidDataSource {
		t.Fatalf("resolution = %#v, want InvalidDataSource failure", res)
	}
}

func TestDemoDiskVRRNameIsIdempotent(t *testing.T) {
	uid := types.UID("stable-uid")
	a := demoDiskVRRName(uid)
	b := demoDiskVRRName(uid)
	if a != b || a == "" {
		t.Fatalf("vrr names differ: %q vs %q", a, b)
	}
}
