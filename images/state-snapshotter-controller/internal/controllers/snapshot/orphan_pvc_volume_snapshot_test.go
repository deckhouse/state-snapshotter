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

package snapshot

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
)

// testVolumeCaptureScheme is the shared scheme for the snapshot-package tests that exercise orphan/residual
// PVC VolumeSnapshot capture (PVC/StorageClass/PV/VolumeSnapshotClass + the state-snapshotter typed API).
func testVolumeCaptureScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = storagev1.AddToScheme(s)
	_ = storagev1alpha1.AddToScheme(s)
	_ = ssv1alpha1.AddToScheme(s)
	return s
}

// Fixtures for the orphan-PVC VolumeSnapshotClass resolution path
// (PVC -> StorageClass annotation -> VolumeSnapshotClass, validated against the bound PV CSI driver).
const (
	testSCName      = "sc-a"
	testVSClassName = "vsc-class-a"
	testCSIDriver   = "csi.example.com"
)

func newPVC(ns, name, uid string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID(uid)}}
}

func boundPVC(ns, name, uid, scName, pvName string) *corev1.PersistentVolumeClaim {
	pvc := newPVC(ns, name, uid)
	pvc.Spec.StorageClassName = &scName
	pvc.Spec.VolumeName = pvName
	return pvc
}

func storageClassWithVSClass(name, vscClassName string) *storagev1.StorageClass {
	return &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: map[string]string{snapshotpkg.AnnotationStorageClassVolumeSnapshotClass: vscClassName},
		},
	}
}

func csiPV(name, driver string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: driver, VolumeHandle: name + "-handle"},
			},
		},
	}
}

func volumeSnapshotClass(name, driver string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(csiVolumeSnapshotClassGVK)
	obj.SetName(name)
	_ = unstructured.SetNestedField(obj.Object, driver, "driver")
	return obj
}

func pvcTarget(ns, name, uid string) vcpkg.Target {
	return vcpkg.Target{UID: uid, APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: name, Namespace: ns}
}

// orphanPVCVolumeSnapshotClass resolves the VolumeSnapshotClass for a residual/orphan PVC and validates its
// driver against the bound PV CSI driver. The full orphan-child declaration flow (EnsureChildren) is covered
// by the n5_pr7 envtest integration; here we unit-test the deterministic class resolver + terminal/transient
// classification (content-single-writer design §11.6).
func TestOrphanPVCVolumeSnapshotClass_HappyPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := "default"
	pvName := "pv-pvc-a"
	cl := fake.NewClientBuilder().WithScheme(testVolumeCaptureScheme(t)).WithObjects(
		boundPVC(ns, "pvc-a", "uid-a", testSCName, pvName),
		storageClassWithVSClass(testSCName, testVSClassName),
		csiPV(pvName, testCSIDriver),
		volumeSnapshotClass(testVSClassName, testCSIDriver),
	).Build()
	r := &SnapshotReconciler{Client: cl, APIReader: cl}

	className, reason, _, err := r.orphanPVCVolumeSnapshotClass(ctx, pvcTarget(ns, "pvc-a", "uid-a"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reason != "" {
		t.Fatalf("happy path must not be terminal, got reason=%q", reason)
	}
	if className != testVSClassName {
		t.Fatalf("className=%q, want %q", className, testVSClassName)
	}
}

func TestOrphanPVCVolumeSnapshotClass_DriverMismatchIsTerminal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := "default"
	pvName := "pv-pvc-a"
	cl := fake.NewClientBuilder().WithScheme(testVolumeCaptureScheme(t)).WithObjects(
		boundPVC(ns, "pvc-a", "uid-a", testSCName, pvName),
		storageClassWithVSClass(testSCName, testVSClassName),
		csiPV(pvName, "csi.other.com"),
		volumeSnapshotClass(testVSClassName, testCSIDriver),
	).Build()
	r := &SnapshotReconciler{Client: cl, APIReader: cl}

	className, reason, msg, err := r.orphanPVCVolumeSnapshotClass(ctx, pvcTarget(ns, "pvc-a", "uid-a"))
	if err != nil {
		t.Fatalf("driver mismatch must be terminal, not a raw error: %v", err)
	}
	if reason != snapshotpkg.ReasonVolumeCaptureFailed || className != "" {
		t.Fatalf("expected terminal %s/empty class, got reason=%q class=%q", snapshotpkg.ReasonVolumeCaptureFailed, reason, className)
	}
	if !strings.Contains(msg, "driver") {
		t.Fatalf("terminal message must explain the driver mismatch, got %q", msg)
	}
}

func TestOrphanPVCVolumeSnapshotClass_MissingAnnotationIsTerminal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := "default"
	pvName := "pv-pvc-a"
	cl := fake.NewClientBuilder().WithScheme(testVolumeCaptureScheme(t)).WithObjects(
		boundPVC(ns, "pvc-a", "uid-a", testSCName, pvName),
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: testSCName}},
		csiPV(pvName, testCSIDriver),
	).Build()
	r := &SnapshotReconciler{Client: cl, APIReader: cl}

	className, reason, _, err := r.orphanPVCVolumeSnapshotClass(ctx, pvcTarget(ns, "pvc-a", "uid-a"))
	if err != nil {
		t.Fatalf("missing annotation must be terminal, not a raw error: %v", err)
	}
	if reason != snapshotpkg.ReasonVolumeCaptureFailed || className != "" {
		t.Fatalf("expected terminal %s/empty class, got reason=%q class=%q", snapshotpkg.ReasonVolumeCaptureFailed, reason, className)
	}
}

// existingOrphanVS builds a pre-existing CSI VolumeSnapshot at the deterministic orphan name with the given
// source PVC + class, for the pre-adoption spec-mismatch guard tests.
func existingOrphanVS(ns, name, srcPVC, className string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(csiVolumeSnapshotGVK)
	obj.SetNamespace(ns)
	obj.SetName(name)
	if srcPVC != "" {
		_ = unstructured.SetNestedField(obj.Object, srcPVC, "spec", "source", "persistentVolumeClaimName")
	}
	if className != "" {
		_ = unstructured.SetNestedField(obj.Object, className, "spec", "volumeSnapshotClassName")
	}
	return obj
}

// boundOrphanVS marks a pre-existing orphan VolumeSnapshot as bound to a durable content, so the guard's
// once-bound class relaxation can be exercised.
func boundOrphanVS(vs *unstructured.Unstructured, boundContent string) *unstructured.Unstructured {
	_ = unstructured.SetNestedField(vs.Object, boundContent, "status", "boundVolumeSnapshotContentName")
	return vs
}

func TestOrphanPVCVolumeSnapshotSpecMismatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := "default"
	const name, pvc, class = "orphan-vs", "pvc-a", "vsc-a"

	t.Run("absent object is not a mismatch", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(testVolumeCaptureScheme(t)).Build()
		r := &SnapshotReconciler{Client: cl, APIReader: cl}
		reason, _, err := r.orphanPVCVolumeSnapshotSpecMismatch(ctx, ns, name, pvc, class)
		if err != nil || reason != "" {
			t.Fatalf("absent object must pass, got reason=%q err=%v", reason, err)
		}
	})

	t.Run("matching source+class is not a mismatch", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(testVolumeCaptureScheme(t)).
			WithObjects(existingOrphanVS(ns, name, pvc, class)).Build()
		r := &SnapshotReconciler{Client: cl, APIReader: cl}
		reason, _, err := r.orphanPVCVolumeSnapshotSpecMismatch(ctx, ns, name, pvc, class)
		if err != nil || reason != "" {
			t.Fatalf("matching object must pass, got reason=%q err=%v", reason, err)
		}
	})

	t.Run("wrong source PVC is terminal", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(testVolumeCaptureScheme(t)).
			WithObjects(existingOrphanVS(ns, name, "other-pvc", class)).Build()
		r := &SnapshotReconciler{Client: cl, APIReader: cl}
		reason, _, err := r.orphanPVCVolumeSnapshotSpecMismatch(ctx, ns, name, pvc, class)
		if err != nil || reason != snapshotpkg.ReasonVolumeCaptureFailed {
			t.Fatalf("wrong source must be terminal %s, got reason=%q err=%v", snapshotpkg.ReasonVolumeCaptureFailed, reason, err)
		}
	})

	t.Run("empty-source shell is terminal", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(testVolumeCaptureScheme(t)).
			WithObjects(existingOrphanVS(ns, name, "", "")).Build()
		r := &SnapshotReconciler{Client: cl, APIReader: cl}
		reason, _, err := r.orphanPVCVolumeSnapshotSpecMismatch(ctx, ns, name, pvc, class)
		if err != nil || reason != snapshotpkg.ReasonVolumeCaptureFailed {
			t.Fatalf("empty-source shell must be terminal, got reason=%q err=%v", reason, err)
		}
	})

	t.Run("wrong class on an UNBOUND handle is terminal", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(testVolumeCaptureScheme(t)).
			WithObjects(existingOrphanVS(ns, name, pvc, "other-class")).Build()
		r := &SnapshotReconciler{Client: cl, APIReader: cl}
		reason, _, err := r.orphanPVCVolumeSnapshotSpecMismatch(ctx, ns, name, pvc, class)
		if err != nil || reason != snapshotpkg.ReasonVolumeCaptureFailed {
			t.Fatalf("wrong class must be terminal, got reason=%q err=%v", reason, err)
		}
	})

	t.Run("wrong class on a BOUND handle is tolerated", func(t *testing.T) {
		// A durable snapshot already exists (bound content): the class has served its sole purpose at
		// creation and re-resolving it (e.g. the StorageClass annotation changed) MUST NOT flip an
		// already-captured snapshot to terminal. Only the source PVC still matters once bound.
		cl := fake.NewClientBuilder().WithScheme(testVolumeCaptureScheme(t)).
			WithObjects(boundOrphanVS(existingOrphanVS(ns, name, pvc, "other-class"), "content-x")).Build()
		r := &SnapshotReconciler{Client: cl, APIReader: cl}
		reason, _, err := r.orphanPVCVolumeSnapshotSpecMismatch(ctx, ns, name, pvc, class)
		if err != nil || reason != "" {
			t.Fatalf("bound handle with drifted class must be tolerated, got reason=%q err=%v", reason, err)
		}
	})

	t.Run("wrong source on a BOUND handle is still terminal", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(testVolumeCaptureScheme(t)).
			WithObjects(boundOrphanVS(existingOrphanVS(ns, name, "other-pvc", class), "content-x")).Build()
		r := &SnapshotReconciler{Client: cl, APIReader: cl}
		reason, _, err := r.orphanPVCVolumeSnapshotSpecMismatch(ctx, ns, name, pvc, class)
		if err != nil || reason != snapshotpkg.ReasonVolumeCaptureFailed {
			t.Fatalf("bound handle capturing the wrong PVC must stay terminal, got reason=%q err=%v", reason, err)
		}
	})
}

func TestOrphanPVCVolumeSnapshotClass_UnboundPVCIsTransient(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := "default"
	// PVC has a StorageClass with a valid annotation but is not bound to a PV yet.
	pvc := newPVC(ns, "pvc-a", "uid-a")
	scName := testSCName
	pvc.Spec.StorageClassName = &scName
	cl := fake.NewClientBuilder().WithScheme(testVolumeCaptureScheme(t)).WithObjects(
		pvc, storageClassWithVSClass(testSCName, testVSClassName),
	).Build()
	r := &SnapshotReconciler{Client: cl, APIReader: cl}

	className, reason, _, err := r.orphanPVCVolumeSnapshotClass(ctx, pvcTarget(ns, "pvc-a", "uid-a"))
	if err == nil {
		t.Fatal("unbound PVC must be transient (returns an error to requeue), got nil")
	}
	if reason != "" || className != "" {
		t.Fatalf("unbound PVC must not be terminal, got reason=%q class=%q", reason, className)
	}
}
