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
	"k8s.io/apimachinery/pkg/api/meta"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	snapshotcontentctrl "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/snapshotcontent"
	volumecapturectrl "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/volumecapture"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
)

func TestEnsureVolumeCaptureLeg_pendingDoesNotRequeue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testVolumeCaptureScheme(t)
	ns := "default"
	snap := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: ns, UID: "snap-uid"},
	}
	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "c1", UID: types.UID("content-uid")}}

	objs := append(orphanCaptureFixtures(ns, "pvc-a", "uid-a"), snap, content)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&storagev1alpha1.Snapshot{}, &storagev1alpha1.SnapshotContent{}).Build()
	r := &SnapshotReconciler{Client: cl, APIReader: cl}

	if err := r.ensureVolumeCaptureLeg(ctx, snap, content); err != nil {
		t.Fatalf("ensureVolumeCaptureLeg: %v", err)
	}
	vsName := orphanPVCVolumeSnapshotName(snap.UID, vcpkg.Target{UID: "uid-a", APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc-a", Namespace: ns})
	vs := &unstructured.Unstructured{}
	vs.SetGroupVersionKind(csiVolumeSnapshotGVK)
	if err := cl.Get(ctx, client.ObjectKey{Namespace: ns, Name: vsName}, vs); err != nil {
		t.Fatalf("expected root orphan PVC VolumeSnapshot, got %v", err)
	}
	if className, _, _ := unstructured.NestedString(vs.Object, "spec", "volumeSnapshotClassName"); className != testVSClassName {
		t.Fatalf("VS must be created with the resolved volumeSnapshotClassName %q, got %q", testVSClassName, className)
	}
	vcr := &unstructured.Unstructured{}
	vcr.SetGroupVersionKind(vcpkg.VolumeCaptureRequestGVK)
	if err := cl.Get(ctx, client.ObjectKey{Namespace: ns, Name: vcpkg.SnapshotContentVCRName(content.UID)}, vcr); !apierrors.IsNotFound(err) {
		t.Fatalf("root orphan PVC path must not create VCR, got %v", err)
	}
	res, err := r.reconcileVolumeCapturePublish(ctx, snap, content, false)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if res.Requeue || res.RequeueAfter > 0 {
		t.Fatalf("pending VolumeSnapshot must not requeue when allowRequeue=false (manifest leg may proceed), got %#v", res)
	}
}

func TestReconcileVolumeCapturePublish_missingVolumeSnapshotNoPublish(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testVolumeCaptureScheme(t)
	ns := "default"
	pvc := newPVC(ns, "pvc-a", "uid-a")
	snap := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: ns, UID: "snap-uid"},
	}
	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "c1", UID: types.UID("content-uid")}}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pvc, snap, content).
		WithStatusSubresource(&storagev1alpha1.SnapshotContent{}).Build()
	r := &SnapshotReconciler{Client: cl, APIReader: cl}

	res, err := r.reconcileVolumeCapturePublish(ctx, snap, content, true)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if res.RequeueAfter == 0 && !res.Requeue {
		t.Fatalf("expected requeue for missing VolumeSnapshot, got %#v", res)
	}
	got := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: content.Name}, got); err != nil {
		t.Fatalf("get content: %v", err)
	}
	if got.Status.Data != nil {
		t.Fatalf("expected no published dataRef on the root aggregator, got %v", got.Status.Data)
	}
}

func TestReconcileVolumeCapturePublish_incompleteVolumeSnapshotsNoPublish(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testVolumeCaptureScheme(t)
	ns := "default"
	pvcA := newPVC(ns, "pvc-a", "uid-a")
	pvcB := newPVC(ns, "pvc-b", "uid-b")
	snap := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: ns, UID: "snap-uid"},
	}
	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "c1", UID: types.UID("content-uid")}}
	vscA := volumeSnapshotContent("vsc-a", true)
	vsA := readyVolumeSnapshot(ns, orphanPVCVolumeSnapshotName(snap.UID, vcpkg.Target{UID: "uid-a", APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc-a", Namespace: ns}), "pvc-a", "vsc-a", snap)

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pvcA, pvcB, snap, content, vsA, vscA).
		WithStatusSubresource(&storagev1alpha1.SnapshotContent{}, csiVolumeSnapshotStatusStub()).Build()
	r := &SnapshotReconciler{Client: cl, APIReader: cl}

	res, err := r.reconcileVolumeCapturePublish(ctx, snap, content, true)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if res.RequeueAfter == 0 && !res.Requeue {
		t.Fatalf("expected requeue while one orphan PVC VolumeSnapshot is missing, got %#v", res)
	}
	got := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: content.Name}, got); err != nil {
		t.Fatalf("get content: %v", err)
	}
	if got.Status.Data != nil {
		t.Fatalf("expected no published dataRef on the root aggregator, got %v", got.Status.Data)
	}
}

// TestReconcileVolumeCapturePublish_orphanVolumeSnapshotHandoffBeforePublish verifies the Variant A
// orphan handoff: the bound VSC is re-owned by the standalone CHILD volume node (not the root), the
// child carries the single dataRef, the orphan VolumeSnapshot becomes a handle to that child
// (status.boundSnapshotContentName), and the root aggregator stays dataRef-less.
func TestReconcileVolumeCapturePublish_orphanVolumeSnapshotHandoffBeforePublish(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testVolumeCaptureScheme(t)
	ns := "default"
	target := pvcTarget(ns, "pvc-a", "uid-a")
	pvc := newPVC(ns, "pvc-a", "uid-a")
	snap := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: ns, UID: "snap-uid"},
	}
	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "c1", UID: types.UID("content-uid")}}
	vsc := volumeSnapshotContent("vsc-a", true)
	vsName := orphanPVCVolumeSnapshotName(snap.UID, target)
	vs := readyVolumeSnapshot(ns, vsName, "pvc-a", "vsc-a", snap)

	objs := append(seedOrphanChildManifest(ns, snap.UID, target), pvc, snap, content, vs, vsc)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&storagev1alpha1.Snapshot{}, &storagev1alpha1.SnapshotContent{}, csiVolumeSnapshotStatusStub()).Build()
	r := &SnapshotReconciler{Client: cl, APIReader: cl}

	if _, err := r.reconcileVolumeCapturePublish(ctx, snap, content, false); err != nil {
		t.Fatalf("publish: %v", err)
	}
	childName := orphanChildContentName(snap.UID, target)
	vscObj := &unstructured.Unstructured{}
	vscObj.SetGroupVersionKind(schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotContent"})
	if err := cl.Get(ctx, client.ObjectKey{Name: "vsc-a"}, vscObj); err != nil {
		t.Fatalf("get vsc: %v", err)
	}
	owned := false
	for _, ref := range vscObj.GetOwnerReferences() {
		if ref.Kind == "SnapshotContent" && ref.Name == childName {
			owned = true
		}
	}
	if !owned {
		t.Fatalf("VSC must be owned by the child volume node %q, got %#v", childName, vscObj.GetOwnerReferences())
	}

	// The root aggregator carries no dataRef; the single dataRef lives on the child volume node.
	root := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: content.Name}, root); err != nil {
		t.Fatalf("get content: %v", err)
	}
	if root.Status.Data != nil {
		t.Fatalf("root aggregator must not carry a dataRef, got %#v", root.Status.Data)
	}
	childRef := orphanChildDataRef(t, cl, snap.UID, target)
	if childRef == nil || childRef.Artifact.Name != "vsc-a" {
		t.Fatalf("expected child volume node dataRef -> vsc-a, got %#v", childRef)
	}

	// The orphan VolumeSnapshot is now a handle to the child content (INV-ORPHAN4).
	gotVS := &unstructured.Unstructured{}
	gotVS.SetGroupVersionKind(csiVolumeSnapshotGVK)
	if err := cl.Get(ctx, client.ObjectKey{Namespace: ns, Name: vsName}, gotVS); err != nil {
		t.Fatalf("get vs: %v", err)
	}
	if bound, _, _ := unstructured.NestedString(gotVS.Object, "status", "boundSnapshotContentName"); bound != childName {
		t.Fatalf("orphan VS boundSnapshotContentName = %q, want %q", bound, childName)
	}
}

func TestReconcileVolumeCaptureSteadyState_staleTargetUIDNotComplete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testVolumeCaptureScheme(t)
	ns := "default"
	snap := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: ns, UID: "snap-uid"},
	}
	content := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "c1", UID: types.UID("content-uid")},
		Status: storagev1alpha1.SnapshotContentStatus{
			Data: &storagev1alpha1.SnapshotDataBinding{
				Source:   storagev1alpha1.SnapshotSubjectRef{UID: "wrong-uid"},
				Artifact: storagev1alpha1.SnapshotDataArtifactRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: "vsc"},
			},
		},
	}
	targets := []vcpkg.Target{{UID: "uid-a", APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc-a", Namespace: ns}}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(snap, content).
		WithStatusSubresource(&storagev1alpha1.Snapshot{}).Build()
	r := &SnapshotReconciler{Client: cl, APIReader: cl}
	vcrKey := types.NamespacedName{Namespace: ns, Name: vcpkg.SnapshotContentVCRName(content.UID)}

	done, _, err := r.reconcileVolumeCaptureSteadyState(ctx, snap, content, vcrKey, targets)
	if err != nil {
		t.Fatalf("steady: %v", err)
	}
	if done {
		t.Fatal("stale dataRefs must not satisfy steady-state")
	}
}

// TestReconcileVolumeCapture_PublishTwoDataRefsAndCleanup verifies that two loose/orphan PVCs each
// become their own standalone child volume node (Variant A: one dataRef per content), the root
// aggregator stays dataRef-less, both children are linked under the root, and the stale root VCR left
// by a prior VCR-based run is deleted once every child node is ready.
func TestReconcileVolumeCapture_PublishTwoDataRefsAndCleanup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testVolumeCaptureScheme(t)
	ns := "default"
	targetA := pvcTarget(ns, "pvc-a", "uid-a")
	targetB := pvcTarget(ns, "pvc-b", "uid-b")
	pvcA := newPVC(ns, "pvc-a", "uid-a")
	pvcB := newPVC(ns, "pvc-b", "uid-b")
	snap := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: ns, UID: "snap-uid"},
	}
	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "c1", UID: types.UID("content-uid")}}
	vscA := volumeSnapshotContent("vsc-a", true)
	vscB := volumeSnapshotContent("vsc-b", true)
	vcrName := vcpkg.SnapshotContentVCRName(content.UID)
	staleVCR := readyVCR(ns, vcrName, nil, nil)
	vsA := readyVolumeSnapshot(ns, orphanPVCVolumeSnapshotName(snap.UID, targetA), "pvc-a", "vsc-a", snap)
	vsB := readyVolumeSnapshot(ns, orphanPVCVolumeSnapshotName(snap.UID, targetB), "pvc-b", "vsc-b", snap)

	objs := []client.Object{pvcA, pvcB, snap, content, staleVCR, vsA, vsB, vscA, vscB}
	objs = append(objs, seedOrphanChildManifest(ns, snap.UID, targetA)...)
	objs = append(objs, seedOrphanChildManifest(ns, snap.UID, targetB)...)
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&storagev1alpha1.Snapshot{}, &storagev1alpha1.SnapshotContent{}, csiVolumeSnapshotStatusStub()).Build()
	r := &SnapshotReconciler{Client: cl, APIReader: cl}

	if err := r.ensureVolumeCaptureLeg(ctx, snap, content); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if _, err := r.reconcileVolumeCapturePublish(ctx, snap, content, true); err != nil {
		t.Fatalf("publish: %v", err)
	}

	got := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: content.Name}, got); err != nil {
		t.Fatalf("get content: %v", err)
	}
	if got.Status.Data != nil {
		t.Fatalf("root aggregator must not carry a dataRef, got %#v", got.Status.Data)
	}
	if len(got.Status.ChildrenSnapshotContentRefs) != 2 {
		t.Fatalf("expected 2 child volume nodes linked under root, got %#v", got.Status.ChildrenSnapshotContentRefs)
	}
	for _, target := range []vcpkg.Target{targetA, targetB} {
		ref := orphanChildDataRef(t, cl, snap.UID, target)
		if ref == nil || string(ref.Source.UID) != target.UID {
			t.Fatalf("child volume node for %s missing its dataRef, got %#v", target.Name, ref)
		}
	}
	gone := &unstructured.Unstructured{}
	gone.SetGroupVersionKind(vcpkg.VolumeCaptureRequestGVK)
	if err := cl.Get(ctx, types.NamespacedName{Namespace: ns, Name: vcrName}, gone); !apierrors.IsNotFound(err) {
		t.Fatalf("stale VCR should be deleted: %v", err)
	}
}

func TestReconcileVolumeCapture_RootIgnoresAndDeletesStaleVCR(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testVolumeCaptureScheme(t)
	ns := "default"
	pvc := newPVC(ns, "pvc-a", "uid-a")
	snap := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: ns, UID: "snap-uid"},
	}
	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "c1", UID: types.UID("content-uid")}}
	target := pvcTarget(ns, "pvc-a", "uid-a")
	vcrName := vcpkg.SnapshotContentVCRName(content.UID)
	vcr := failedVCR(ns, vcrName, []vcpkg.Target{target})
	vsc := volumeSnapshotContent("vsc-a", true)
	vs := readyVolumeSnapshot(ns, orphanPVCVolumeSnapshotName(snap.UID, target), "pvc-a", "vsc-a", snap)

	objs := append(seedOrphanChildManifest(ns, snap.UID, target), pvc, snap, content, vcr, vs, vsc)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&storagev1alpha1.Snapshot{}, &storagev1alpha1.SnapshotContent{}, csiVolumeSnapshotStatusStub()).Build()
	r := &SnapshotReconciler{Client: cl, APIReader: cl}
	if err := r.ensureVolumeCaptureLeg(ctx, snap, content); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if _, err := r.reconcileVolumeCapturePublish(ctx, snap, content, true); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: ns, Name: vcrName}, vcr); !apierrors.IsNotFound(err) {
		t.Fatalf("stale VCR should be deleted, got %v", err)
	}
	got := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: content.Name}, got); err != nil {
		t.Fatalf("get content: %v", err)
	}
	if got.Status.Data != nil {
		t.Fatalf("root aggregator must not carry a dataRef, got %#v", got.Status.Data)
	}
	if ref := orphanChildDataRef(t, cl, snap.UID, target); ref == nil || ref.Artifact.Name != "vsc-a" {
		t.Fatalf("expected child volume node dataRef -> vsc-a, got %#v", ref)
	}
}

func readyVCR(ns, name string, targets []vcpkg.Target, refs []vcpkg.DataBinding) *unstructured.Unstructured {
	obj := volumecapturectrl.NewVolumeCaptureRequestObject(ns, name, metav1.OwnerReference{}, targets)
	_ = unstructured.SetNestedSlice(obj.Object, []interface{}{
		map[string]interface{}{
			"type":   vcpkg.ConditionTypeReady,
			"status": string(metav1.ConditionTrue),
			"reason": vcpkg.ConditionReasonCompleted,
		},
	}, "status", "conditions")
	// A snapshot node binds at most one data artifact, so status.data is a single object carrying only the
	// artifact; the captured PVC identity comes from spec.target (set by NewVolumeCaptureRequestObject).
	if len(refs) > 0 {
		r := refs[0]
		_ = unstructured.SetNestedMap(obj.Object, map[string]interface{}{
			"artifact": map[string]interface{}{
				"apiVersion": r.Artifact.APIVersion, "kind": r.Artifact.Kind, "name": r.Artifact.Name,
			},
		}, "status", "data")
	}
	return obj
}

func failedVCR(ns, name string, targets []vcpkg.Target) *unstructured.Unstructured {
	obj := volumecapturectrl.NewVolumeCaptureRequestObject(ns, name, metav1.OwnerReference{}, targets)
	_ = unstructured.SetNestedSlice(obj.Object, []interface{}{
		map[string]interface{}{
			"type":    vcpkg.ConditionTypeReady,
			"status":  string(metav1.ConditionFalse),
			"reason":  "SnapshotCreationFailed",
			"message": "csi failed",
		},
	}, "status", "conditions")
	return obj
}

func newPVC(ns, name, uid string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID(uid)}}
}

// Fixtures for the orphan-PVC VolumeSnapshotClass resolution path
// (PVC -> StorageClass annotation -> VolumeSnapshotClass, validated against PV CSI driver).
const (
	testSCName      = "sc-a"
	testVSClassName = "vsc-class-a"
	testCSIDriver   = "csi.example.com"
)

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

// orphanCaptureFixtures returns the PVC/StorageClass/PV/VolumeSnapshotClass objects that let the orphan
// VS create path resolve a valid, driver-matching VolumeSnapshotClass for the given PVC.
func orphanCaptureFixtures(ns, pvcName, uid string) []client.Object {
	pvName := "pv-" + pvcName
	return []client.Object{
		boundPVC(ns, pvcName, uid, testSCName, pvName),
		storageClassWithVSClass(testSCName, testVSClassName),
		csiPV(pvName, testCSIDriver),
		volumeSnapshotClass(testVSClassName, testCSIDriver),
	}
}

func volumeSnapshotContent(name string, ready bool) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotContent"})
	obj.SetName(name)
	_ = unstructured.SetNestedField(obj.Object, "Retain", "spec", "deletionPolicy")
	_ = unstructured.SetNestedField(obj.Object, ready, "status", "readyToUse")
	return obj
}

func readyVolumeSnapshot(ns, name, pvcName, vscName string, owner *storagev1alpha1.Snapshot) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "snapshot.storage.k8s.io/v1",
		"kind":       "VolumeSnapshot",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": ns,
		},
		"spec": map[string]interface{}{
			"source": map[string]interface{}{
				"persistentVolumeClaimName": pvcName,
			},
		},
		"status": map[string]interface{}{
			"boundVolumeSnapshotContentName": vscName,
			"readyToUse":                     true,
		},
	}}
	if owner != nil {
		obj.SetOwnerReferences([]metav1.OwnerReference{volumeSnapshotOwnerReferenceForSnapshot(owner)})
	}
	// Deterministic UID so the child volume-node SnapshotContent and per-orphan MCR names (keyed by the
	// orphan VS UID under the unified wave4C scheme) are stable and distinct per orphan in these tests.
	obj.SetUID(orphanVSTestUID(name))
	obj.SetGroupVersionKind(csiVolumeSnapshotGVK)
	return obj
}

// orphanVSTestUID is the deterministic UID stamped on orphan VolumeSnapshots created by test fixtures,
// mirroring what a live apiserver would assign. It keys the child content / per-orphan MCR names in
// assertions (unified wave4C scheme, see api/names).
func orphanVSTestUID(vsName string) types.UID {
	return types.UID("vsuid-" + vsName)
}

// unboundVolumeSnapshot builds a VolumeSnapshot that is not yet bound (no boundVolumeSnapshotContentName),
// with the given spec.volumeSnapshotClassName (empty string omits the field).
func unboundVolumeSnapshot(ns, name, pvcName, className string, owner *storagev1alpha1.Snapshot) *unstructured.Unstructured {
	spec := map[string]interface{}{
		"source": map[string]interface{}{
			"persistentVolumeClaimName": pvcName,
		},
	}
	if className != "" {
		spec["volumeSnapshotClassName"] = className
	}
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": snapshotpkg.CSISnapshotAPIVersion,
		"kind":       snapshotpkg.KindVolumeSnapshot,
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": ns,
		},
		"spec": spec,
		"status": map[string]interface{}{
			"readyToUse": false,
		},
	}}
	if owner != nil {
		obj.SetOwnerReferences([]metav1.OwnerReference{volumeSnapshotOwnerReferenceForSnapshot(owner)})
	}
	obj.SetGroupVersionKind(csiVolumeSnapshotGVK)
	return obj
}

func volumeSnapshotContentWithPolicy(name string, ready bool, policy string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(csiVolumeSnapshotContentGVK)
	obj.SetName(name)
	_ = unstructured.SetNestedField(obj.Object, policy, "spec", "deletionPolicy")
	_ = unstructured.SetNestedField(obj.Object, ready, "status", "readyToUse")
	return obj
}

func erroredVolumeSnapshot(ns, name, pvcName, errMsg string, owner *storagev1alpha1.Snapshot) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": snapshotpkg.CSISnapshotAPIVersion,
		"kind":       snapshotpkg.KindVolumeSnapshot,
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": ns,
		},
		"spec": map[string]interface{}{
			"source": map[string]interface{}{
				"persistentVolumeClaimName": pvcName,
			},
		},
		"status": map[string]interface{}{
			"readyToUse": false,
			"error": map[string]interface{}{
				"message": errMsg,
			},
		},
	}}
	if owner != nil {
		obj.SetOwnerReferences([]metav1.OwnerReference{volumeSnapshotOwnerReferenceForSnapshot(owner)})
	}
	obj.SetGroupVersionKind(csiVolumeSnapshotGVK)
	return obj
}

func pvcTarget(ns, name, uid string) vcpkg.Target {
	return vcpkg.Target{UID: uid, APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: name, Namespace: ns}
}

func TestEnsureVolumeCaptureLeg_RecordsVolumeSnapshotVisibilityLeaf(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testVolumeCaptureScheme(t)
	ns := "default"
	snap := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: ns, UID: "snap-uid"}}
	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "c1", UID: types.UID("content-uid")}}

	objs := append(orphanCaptureFixtures(ns, "pvc-a", "uid-a"), snap, content)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&storagev1alpha1.Snapshot{}, &storagev1alpha1.SnapshotContent{}).Build()
	r := &SnapshotReconciler{Client: cl, APIReader: cl}

	if err := r.ensureVolumeCaptureLeg(ctx, snap, content); err != nil {
		t.Fatalf("ensureVolumeCaptureLeg: %v", err)
	}
	vsName := orphanPVCVolumeSnapshotName(snap.UID, pvcTarget(ns, "pvc-a", "uid-a"))
	fresh := &storagev1alpha1.Snapshot{}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: ns, Name: snap.Name}, fresh); err != nil {
		t.Fatalf("get snapshot: %v", err)
	}
	if len(fresh.Status.ChildrenSnapshotRefs) != 1 {
		t.Fatalf("expected one VS visibility leaf, got %#v", fresh.Status.ChildrenSnapshotRefs)
	}
	ref := fresh.Status.ChildrenSnapshotRefs[0]
	if !snapshotpkg.IsVolumeSnapshotVisibilityLeaf(ref) || ref.Name != vsName {
		t.Fatalf("expected VS visibility leaf %q, got %#v", vsName, ref)
	}
	vcr := &unstructured.Unstructured{}
	vcr.SetGroupVersionKind(vcpkg.VolumeCaptureRequestGVK)
	if err := cl.Get(ctx, client.ObjectKey{Namespace: ns, Name: vcpkg.SnapshotContentVCRName(content.UID)}, vcr); !apierrors.IsNotFound(err) {
		t.Fatalf("orphan PVC path must not create VCR, got %v", err)
	}
}

func TestReconcileVolumeCapturePublish_DeletePolicyPatchedToRetain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testVolumeCaptureScheme(t)
	ns := "default"
	target := pvcTarget(ns, "pvc-a", "uid-a")
	pvc := newPVC(ns, "pvc-a", "uid-a")
	snap := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: ns, UID: "snap-uid"}}
	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "c1", UID: types.UID("content-uid")}}
	vsc := volumeSnapshotContentWithPolicy("vsc-a", true, "Delete")
	vs := readyVolumeSnapshot(ns, orphanPVCVolumeSnapshotName(snap.UID, target), "pvc-a", "vsc-a", snap)

	objs := append(seedOrphanChildManifest(ns, snap.UID, target), pvc, snap, content, vs, vsc)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&storagev1alpha1.Snapshot{}, &storagev1alpha1.SnapshotContent{}, csiVolumeSnapshotStatusStub()).Build()
	r := &SnapshotReconciler{Client: cl, APIReader: cl}

	res, err := r.reconcileVolumeCapturePublish(ctx, snap, content, true)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if res.Requeue || res.RequeueAfter > 0 {
		t.Fatalf("Delete VSC patched to Retain should publish without requeue, got %#v", res)
	}
	gotVSC := &unstructured.Unstructured{}
	gotVSC.SetGroupVersionKind(csiVolumeSnapshotContentGVK)
	if err := cl.Get(ctx, client.ObjectKey{Name: "vsc-a"}, gotVSC); err != nil {
		t.Fatalf("get vsc: %v", err)
	}
	policy, _, _ := unstructured.NestedString(gotVSC.Object, "spec", "deletionPolicy")
	if policy != "Retain" {
		t.Fatalf("VSC deletionPolicy must be patched to Retain, got %q", policy)
	}
	got := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: content.Name}, got); err != nil {
		t.Fatalf("get content: %v", err)
	}
	if got.Status.Data != nil {
		t.Fatalf("root aggregator must not carry a dataRef, got %#v", got.Status.Data)
	}
	if ref := orphanChildDataRef(t, cl, snap.UID, target); ref == nil || ref.Artifact.Name != "vsc-a" {
		t.Fatalf("expected child volume node dataRef -> vsc-a, got %#v", ref)
	}
}

func TestReconcileVolumeCapturePublish_RetainPatchImpossibleIsTerminal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testVolumeCaptureScheme(t)
	ns := "default"
	pvc := newPVC(ns, "pvc-a", "uid-a")
	snap := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: ns, UID: "snap-uid"}}
	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "c1", UID: types.UID("content-uid")}}
	vsc := volumeSnapshotContentWithPolicy("vsc-a", true, "Delete")
	vs := readyVolumeSnapshot(ns, orphanPVCVolumeSnapshotName(snap.UID, pvcTarget(ns, "pvc-a", "uid-a")), "pvc-a", "vsc-a", snap)

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pvc, snap, content, vs, vsc).
		WithStatusSubresource(&storagev1alpha1.Snapshot{}, &storagev1alpha1.SnapshotContent{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if u, ok := obj.(*unstructured.Unstructured); ok && u.GetKind() == snapshotpkg.KindVolumeSnapshotContent {
					return apierrors.NewInvalid(csiVolumeSnapshotContentGVK.GroupKind(), u.GetName(), nil)
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	r := &SnapshotReconciler{Client: cl, APIReader: cl}

	res, err := r.reconcileVolumeCapturePublish(ctx, snap, content, true)
	if err != nil {
		t.Fatalf("publish must not return raw error on terminal policy failure: %v", err)
	}
	if res.Requeue || res.RequeueAfter > 0 {
		t.Fatalf("terminal policy failure must not requeue, got %#v", res)
	}
	fresh := &storagev1alpha1.Snapshot{}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: ns, Name: snap.Name}, fresh); err != nil {
		t.Fatalf("get snapshot: %v", err)
	}
	cond := meta.FindStatusCondition(fresh.Status.Conditions, snapshotpkg.ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != snapshotpkg.ReasonDataArtifactInvalid {
		t.Fatalf("expected Ready=False/%s, got %#v", snapshotpkg.ReasonDataArtifactInvalid, cond)
	}
	got := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: content.Name}, got); err != nil {
		t.Fatalf("get content: %v", err)
	}
	if got.Status.Data != nil {
		t.Fatalf("no dataRef must be published on terminal failure, got %v", got.Status.Data)
	}
}

func TestReconcileVolumeCapturePublish_VolumeSnapshotStatusErrorIsTerminal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testVolumeCaptureScheme(t)
	ns := "default"
	pvc := newPVC(ns, "pvc-a", "uid-a")
	snap := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: ns, UID: "snap-uid"}}
	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "c1", UID: types.UID("content-uid")}}
	vs := erroredVolumeSnapshot(ns, orphanPVCVolumeSnapshotName(snap.UID, pvcTarget(ns, "pvc-a", "uid-a")), "pvc-a", "csi snapshot failed", snap)

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pvc, snap, content, vs).
		WithStatusSubresource(&storagev1alpha1.Snapshot{}, &storagev1alpha1.SnapshotContent{}).Build()
	r := &SnapshotReconciler{Client: cl, APIReader: cl}

	res, err := r.reconcileVolumeCapturePublish(ctx, snap, content, true)
	if err != nil {
		t.Fatalf("publish must not return raw error on CSI failure: %v", err)
	}
	if res.Requeue || res.RequeueAfter > 0 {
		t.Fatalf("terminal CSI failure must not requeue, got %#v", res)
	}
	fresh := &storagev1alpha1.Snapshot{}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: ns, Name: snap.Name}, fresh); err != nil {
		t.Fatalf("get snapshot: %v", err)
	}
	cond := meta.FindStatusCondition(fresh.Status.Conditions, snapshotpkg.ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != snapshotpkg.ReasonVolumeCaptureFailed {
		t.Fatalf("expected Ready=False/%s, got %#v", snapshotpkg.ReasonVolumeCaptureFailed, cond)
	}
	if !strings.Contains(cond.Message, "csi snapshot failed") {
		t.Fatalf("condition message must surface the CSI error, got %q", cond.Message)
	}
}

// TestEnsureOrphanPVCVolumeSnapshots_DurableVSNotPruned verifies the orphan VolumeSnapshot is durable:
// the visibility-leaf list on the Snapshot still tracks the current desired target set (domain children
// preserved, new leaf added, a leaf no longer desired dropped from the list), but the VolumeSnapshot
// OBJECT is NOT deleted mid-life — it is retained for the life of the Snapshot and removed only by
// ownerRef GC. Orphan capture is now sequenced after the domain wave, so "became covered -> prune" churn
// no longer happens; the durable object is the snapshot of the PVC.
func TestEnsureOrphanPVCVolumeSnapshots_DurableVSNotPruned(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testVolumeCaptureScheme(t)
	ns := "default"

	staleTarget := pvcTarget(ns, "pvc-old", "uid-old")
	staleName := orphanPVCVolumeSnapshotName("snap-uid", staleTarget)
	domainChild := storagev1alpha1.SnapshotChildRef{APIVersion: storagev1alpha1.SchemeGroupVersion.String(), Kind: "Snapshot", Name: "nss-child-domain"}

	snap := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: ns, UID: "snap-uid"},
		Status: storagev1alpha1.SnapshotStatus{
			ChildrenSnapshotRefs: []storagev1alpha1.SnapshotChildRef{
				domainChild,
				{APIVersion: snapshotpkg.CSISnapshotAPIVersion, Kind: snapshotpkg.KindVolumeSnapshot, Name: staleName},
			},
		},
	}
	staleVS := readyVolumeSnapshot(ns, staleName, "pvc-old", "vsc-old", snap)
	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "c1", UID: types.UID("content-uid")}}

	objs := append(orphanCaptureFixtures(ns, "pvc-a", "uid-a"), snap, staleVS, content)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&storagev1alpha1.Snapshot{}, &storagev1alpha1.SnapshotContent{}).Build()
	r := &SnapshotReconciler{Client: cl, APIReader: cl}

	newTarget := pvcTarget(ns, "pvc-a", "uid-a")
	if err := r.ensureOrphanPVCVolumeSnapshots(ctx, snap, content, []vcpkg.Target{newTarget}); err != nil {
		t.Fatalf("ensureOrphanPVCVolumeSnapshots: %v", err)
	}

	newName := orphanPVCVolumeSnapshotName(snap.UID, newTarget)
	fresh := &storagev1alpha1.Snapshot{}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: ns, Name: snap.Name}, fresh); err != nil {
		t.Fatalf("get snapshot: %v", err)
	}
	gotNames := map[string]bool{}
	for _, ref := range fresh.Status.ChildrenSnapshotRefs {
		gotNames[ref.Name] = true
	}
	if !gotNames["nss-child-domain"] {
		t.Fatalf("domain child ref must be preserved, got %#v", fresh.Status.ChildrenSnapshotRefs)
	}
	if !gotNames[newName] {
		t.Fatalf("new VS leaf %q must be present, got %#v", newName, fresh.Status.ChildrenSnapshotRefs)
	}
	if gotNames[staleName] {
		t.Fatalf("non-desired VS leaf %q must be dropped from the visibility list, got %#v", staleName, fresh.Status.ChildrenSnapshotRefs)
	}

	// The durable VS object must NOT be deleted: it is the snapshot of the PVC and lives for the Snapshot's
	// lifetime (ownerRef GC only). This is the core behavior change vs the old covered-driven pruning.
	keptVS := &unstructured.Unstructured{}
	keptVS.SetGroupVersionKind(csiVolumeSnapshotGVK)
	if err := cl.Get(ctx, client.ObjectKey{Namespace: ns, Name: staleName}, keptVS); err != nil {
		t.Fatalf("durable orphan VS object %q must be retained (not pruned), got err=%v", staleName, err)
	}
}

func TestEnsureOrphanPVCVolumeSnapshots_DriverMismatchIsTerminal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testVolumeCaptureScheme(t)
	ns := "default"
	snap := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: ns, UID: "snap-uid"}}
	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "c1", UID: types.UID("content-uid")}}

	pvName := "pv-pvc-a"
	pvc := boundPVC(ns, "pvc-a", "uid-a", testSCName, pvName)
	sc := storageClassWithVSClass(testSCName, testVSClassName)
	pv := csiPV(pvName, "csi.other.com")
	// VolumeSnapshotClass driver does not match the PV CSI driver.
	vsClass := volumeSnapshotClass(testVSClassName, testCSIDriver)

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pvc, sc, pv, vsClass, snap, content).
		WithStatusSubresource(&storagev1alpha1.Snapshot{}, &storagev1alpha1.SnapshotContent{}).Build()
	r := &SnapshotReconciler{Client: cl, APIReader: cl}

	target := pvcTarget(ns, "pvc-a", "uid-a")
	if err := r.ensureOrphanPVCVolumeSnapshots(ctx, snap, content, []vcpkg.Target{target}); err != nil {
		t.Fatalf("driver mismatch must be terminal, not a raw error: %v", err)
	}
	vs := &unstructured.Unstructured{}
	vs.SetGroupVersionKind(csiVolumeSnapshotGVK)
	if err := cl.Get(ctx, client.ObjectKey{Namespace: ns, Name: orphanPVCVolumeSnapshotName(snap.UID, target)}, vs); !apierrors.IsNotFound(err) {
		t.Fatalf("no VolumeSnapshot must be created on driver mismatch, got %v", err)
	}
	fresh := &storagev1alpha1.Snapshot{}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: ns, Name: snap.Name}, fresh); err != nil {
		t.Fatalf("get snapshot: %v", err)
	}
	cond := meta.FindStatusCondition(fresh.Status.Conditions, snapshotpkg.ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != snapshotpkg.ReasonVolumeCaptureFailed {
		t.Fatalf("expected Ready=False/%s, got %#v", snapshotpkg.ReasonVolumeCaptureFailed, cond)
	}
	if !strings.Contains(cond.Message, "driver") {
		t.Fatalf("condition message must explain the driver mismatch, got %q", cond.Message)
	}
}

func TestEnsureOrphanPVCVolumeSnapshots_MissingAnnotationIsTerminal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testVolumeCaptureScheme(t)
	ns := "default"
	snap := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: ns, UID: "snap-uid"}}
	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "c1", UID: types.UID("content-uid")}}

	pvName := "pv-pvc-a"
	pvc := boundPVC(ns, "pvc-a", "uid-a", testSCName, pvName)
	// StorageClass without the volumesnapshotclass annotation.
	sc := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: testSCName}}
	pv := csiPV(pvName, testCSIDriver)

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pvc, sc, pv, snap, content).
		WithStatusSubresource(&storagev1alpha1.Snapshot{}, &storagev1alpha1.SnapshotContent{}).Build()
	r := &SnapshotReconciler{Client: cl, APIReader: cl}

	target := pvcTarget(ns, "pvc-a", "uid-a")
	if err := r.ensureOrphanPVCVolumeSnapshots(ctx, snap, content, []vcpkg.Target{target}); err != nil {
		t.Fatalf("missing annotation must be terminal, not a raw error: %v", err)
	}
	fresh := &storagev1alpha1.Snapshot{}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: ns, Name: snap.Name}, fresh); err != nil {
		t.Fatalf("get snapshot: %v", err)
	}
	cond := meta.FindStatusCondition(fresh.Status.Conditions, snapshotpkg.ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != snapshotpkg.ReasonVolumeCaptureFailed {
		t.Fatalf("expected Ready=False/%s, got %#v", snapshotpkg.ReasonVolumeCaptureFailed, cond)
	}
}

func TestEnsureOrphanPVCVolumeSnapshots_ExistingUnboundWrongClassIsTerminal(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		existingClass string
	}{
		{name: "mismatched class", existingClass: "some-other-class"},
		{name: "legacy no class", existingClass: ""},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			scheme := testVolumeCaptureScheme(t)
			ns := "default"
			snap := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: ns, UID: "snap-uid"}}
			content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "c1", UID: types.UID("content-uid")}}

			target := pvcTarget(ns, "pvc-a", "uid-a")
			vsName := orphanPVCVolumeSnapshotName(snap.UID, target)
			existingVS := unboundVolumeSnapshot(ns, vsName, "pvc-a", tc.existingClass, snap)

			objs := append(orphanCaptureFixtures(ns, "pvc-a", "uid-a"), snap, content, existingVS)
			cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
				WithStatusSubresource(&storagev1alpha1.Snapshot{}, &storagev1alpha1.SnapshotContent{}).Build()
			r := &SnapshotReconciler{Client: cl, APIReader: cl}

			if err := r.ensureOrphanPVCVolumeSnapshots(ctx, snap, content, []vcpkg.Target{target}); err != nil {
				t.Fatalf("class mismatch on an unbound VS must be terminal, not a raw error: %v", err)
			}
			fresh := &storagev1alpha1.Snapshot{}
			if err := cl.Get(ctx, client.ObjectKey{Namespace: ns, Name: snap.Name}, fresh); err != nil {
				t.Fatalf("get snapshot: %v", err)
			}
			cond := meta.FindStatusCondition(fresh.Status.Conditions, snapshotpkg.ConditionReady)
			if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != snapshotpkg.ReasonVolumeCaptureFailed {
				t.Fatalf("expected Ready=False/%s, got %#v", snapshotpkg.ReasonVolumeCaptureFailed, cond)
			}
		})
	}
}

func TestEnsureOrphanPVCVolumeSnapshots_ExistingBoundLegacyNoClassAccepted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testVolumeCaptureScheme(t)
	ns := "default"
	snap := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: ns, UID: "snap-uid"}}
	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "c1", UID: types.UID("content-uid")}}

	target := pvcTarget(ns, "pvc-a", "uid-a")
	vsName := orphanPVCVolumeSnapshotName(snap.UID, target)
	// Bound VS without volumeSnapshotClassName (legacy): the durable VSC already exists, so the class is
	// moot and must not flip the snapshot to terminal. readyVolumeSnapshot omits volumeSnapshotClassName.
	boundLegacyVS := readyVolumeSnapshot(ns, vsName, "pvc-a", "vsc-a", snap)

	// No StorageClass/PV/VolumeSnapshotClass fixtures on purpose: a bound VS must not trigger resolution.
	pvc := newPVC(ns, "pvc-a", "uid-a")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pvc, snap, content, boundLegacyVS).
		WithStatusSubresource(&storagev1alpha1.Snapshot{}, &storagev1alpha1.SnapshotContent{}).Build()
	r := &SnapshotReconciler{Client: cl, APIReader: cl}

	if err := r.ensureOrphanPVCVolumeSnapshots(ctx, snap, content, []vcpkg.Target{target}); err != nil {
		t.Fatalf("bound legacy VS must be accepted without resolution: %v", err)
	}
	fresh := &storagev1alpha1.Snapshot{}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: ns, Name: snap.Name}, fresh); err != nil {
		t.Fatalf("get snapshot: %v", err)
	}
	if cond := meta.FindStatusCondition(fresh.Status.Conditions, snapshotpkg.ConditionReady); cond != nil && cond.Status == metav1.ConditionFalse {
		t.Fatalf("bound legacy VS must not produce a terminal Ready=False, got %#v", cond)
	}
}

func TestReconcileVolumeCapturePublish_ZeroOrphanTargetsClearsStaleVCR(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testVolumeCaptureScheme(t)
	ns := "default"
	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "c1", UID: types.UID("content-uid")}}
	vcrName := vcpkg.SnapshotContentVCRName(content.UID)
	snap := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: ns, UID: "snap-uid"},
	}
	// No PVCs in the namespace => zero orphan targets, but a stale VCR from a previous run still exists.
	staleVCR := readyVCR(ns, vcrName, nil, nil)

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(snap, content, staleVCR).
		WithStatusSubresource(&storagev1alpha1.Snapshot{}, &storagev1alpha1.SnapshotContent{}).Build()
	r := &SnapshotReconciler{Client: cl, APIReader: cl}

	if _, err := r.reconcileVolumeCapturePublish(ctx, snap, content, true); err != nil {
		t.Fatalf("publish: %v", err)
	}
	gone := &unstructured.Unstructured{}
	gone.SetGroupVersionKind(vcpkg.VolumeCaptureRequestGVK)
	if err := cl.Get(ctx, types.NamespacedName{Namespace: ns, Name: vcrName}, gone); !apierrors.IsNotFound(err) {
		t.Fatalf("stale VCR must be deleted on zero-target residual publish, got %v", err)
	}
}

func TestOrphanPVCVolumeSnapshotClass_UnboundPVCIsTransient(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testVolumeCaptureScheme(t)
	ns := "default"
	// PVC has a StorageClass with a valid annotation but is not bound to a PV yet.
	pvc := newPVC(ns, "pvc-a", "uid-a")
	scName := testSCName
	pvc.Spec.StorageClassName = &scName
	sc := storageClassWithVSClass(testSCName, testVSClassName)

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pvc, sc).Build()
	r := &SnapshotReconciler{Client: cl, APIReader: cl}

	className, reason, _, err := r.orphanPVCVolumeSnapshotClass(ctx, pvcTarget(ns, "pvc-a", "uid-a"))
	if err == nil {
		t.Fatal("unbound PVC must be transient (returns an error to requeue), got nil")
	}
	if reason != "" || className != "" {
		t.Fatalf("unbound PVC must not be terminal, got reason=%q class=%q", reason, className)
	}
}

// TestEnsureOrphanVolumeSnapshotsPrePlanned_ContentFreeCreatesVSAndLeaf verifies the "late Planned"
// pre-barrier wave: with no domain children (gate passes vacuously) it computes residual targets and
// creates the orphan VolumeSnapshot + publishes the visibility leaf onto childrenSnapshotRefs WITHOUT a
// bound root SnapshotContent (content-free), so the full child set is enumerated before MarkPlanned.
func TestEnsureOrphanVolumeSnapshotsPrePlanned_ContentFreeCreatesVSAndLeaf(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testVolumeCaptureScheme(t)
	ns := "default"
	snap := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: ns, UID: "snap-uid"}}

	// No SnapshotContent object in the fixtures: the wave must not need it.
	objs := orphanCaptureFixtures(ns, "pvc-a", "uid-a")
	objs = append(objs, snap)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&storagev1alpha1.Snapshot{}).Build()
	r := &SnapshotReconciler{Client: cl, APIReader: cl}

	res, err := r.ensureOrphanVolumeSnapshotsPrePlanned(ctx, snap)
	if err != nil {
		t.Fatalf("ensureOrphanVolumeSnapshotsPrePlanned: %v", err)
	}
	if res.Requeue || res.RequeueAfter > 0 {
		t.Fatalf("content-free wave with a resolvable target must not requeue, got %#v", res)
	}
	vsName := orphanPVCVolumeSnapshotName(snap.UID, pvcTarget(ns, "pvc-a", "uid-a"))
	vs := &unstructured.Unstructured{}
	vs.SetGroupVersionKind(csiVolumeSnapshotGVK)
	if err := cl.Get(ctx, client.ObjectKey{Namespace: ns, Name: vsName}, vs); err != nil {
		t.Fatalf("expected orphan VolumeSnapshot created content-free, got %v", err)
	}
	fresh := &storagev1alpha1.Snapshot{}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: ns, Name: snap.Name}, fresh); err != nil {
		t.Fatalf("get snapshot: %v", err)
	}
	if len(fresh.Status.ChildrenSnapshotRefs) != 1 || fresh.Status.ChildrenSnapshotRefs[0].Name != vsName {
		t.Fatalf("expected VS visibility leaf %q published before Planned, got %#v", vsName, fresh.Status.ChildrenSnapshotRefs)
	}
}

// TestEnsureOrphanVolumeSnapshotsPrePlanned_DefersUntilDomainChildrenReady verifies the wave is gated on
// domain children being Ready (so subtree-covered PVC coverage is complete): a not-yet-Ready domain child
// keeps the wave closed (requeue) and no orphan VolumeSnapshot is created.
func TestEnsureOrphanVolumeSnapshotsPrePlanned_DefersUntilDomainChildrenReady(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testVolumeCaptureScheme(t)
	ns := "default"
	childRef := storagev1alpha1.SnapshotChildRef{APIVersion: storagev1alpha1.SchemeGroupVersion.String(), Kind: "Snapshot", Name: "nss-child-domain"}
	child := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Name: "nss-child-domain", Namespace: ns, UID: "child-uid"}}
	snap := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: ns, UID: "snap-uid"},
		Status:     storagev1alpha1.SnapshotStatus{ChildrenSnapshotRefs: []storagev1alpha1.SnapshotChildRef{childRef}},
	}

	objs := orphanCaptureFixtures(ns, "pvc-a", "uid-a")
	objs = append(objs, snap, child)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&storagev1alpha1.Snapshot{}).Build()
	r := &SnapshotReconciler{Client: cl, APIReader: cl}

	res, err := r.ensureOrphanVolumeSnapshotsPrePlanned(ctx, snap)
	if err != nil {
		t.Fatalf("ensureOrphanVolumeSnapshotsPrePlanned: %v", err)
	}
	if !res.Requeue && res.RequeueAfter == 0 {
		t.Fatalf("wave must defer (requeue) until domain children are Ready, got %#v", res)
	}
	vsName := orphanPVCVolumeSnapshotName(snap.UID, pvcTarget(ns, "pvc-a", "uid-a"))
	vs := &unstructured.Unstructured{}
	vs.SetGroupVersionKind(csiVolumeSnapshotGVK)
	if err := cl.Get(ctx, client.ObjectKey{Namespace: ns, Name: vsName}, vs); !apierrors.IsNotFound(err) {
		t.Fatalf("no orphan VolumeSnapshot must be created while the gate is closed, got %v", err)
	}
}

func TestHasNonVisibilitySnapshotChildren(t *testing.T) {
	t.Parallel()
	leaf := storagev1alpha1.SnapshotChildRef{APIVersion: snapshotpkg.CSISnapshotAPIVersion, Kind: snapshotpkg.KindVolumeSnapshot, Name: "nss-vs-x"}
	domain := storagev1alpha1.SnapshotChildRef{APIVersion: storagev1alpha1.SchemeGroupVersion.String(), Kind: "Snapshot", Name: "nss-child-y"}
	if hasNonVisibilitySnapshotChildren(nil) {
		t.Fatal("nil refs must not count as having children")
	}
	if hasNonVisibilitySnapshotChildren([]storagev1alpha1.SnapshotChildRef{leaf}) {
		t.Fatal("only VS visibility leaves must not count as real children")
	}
	if !hasNonVisibilitySnapshotChildren([]storagev1alpha1.SnapshotChildRef{leaf, domain}) {
		t.Fatal("a domain child must count as a real child")
	}
}

func testVolumeCaptureScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = storagev1.AddToScheme(s)
	_ = storagev1alpha1.AddToScheme(s)
	// Variant A orphan capture fans each loose PVC out into a child volume node with its own per-orphan
	// ManifestCaptureRequest + ManifestCheckpoint, so the typed state-snapshotter API must be registered.
	_ = ssv1alpha1.AddToScheme(s)
	return s
}

// orphanChildContentName is the deterministic child volume-node SnapshotContent name for a captured PVC,
// keyed by the orphan VolumeSnapshot UID (unified wave4C scheme).
func orphanChildContentName(snapUID types.UID, target vcpkg.Target) string {
	vsUID := orphanVSTestUID(orphanPVCVolumeSnapshotName(snapUID, target))
	return snapshotcontentctrl.ChildVolumeContentName(vsUID)
}

// csiVolumeSnapshotStatusStub registers the CSI VolumeSnapshot as a status-subresource type on the fake
// client so the Variant A orphan handoff (bindOrphanVSToChildContent writes status.boundSnapshotContentName
// via Status().Patch) can persist.
func csiVolumeSnapshotStatusStub() client.Object {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(csiVolumeSnapshotGVK)
	return u
}

// seedOrphanChildManifest pre-creates the per-orphan ManifestCaptureRequest (with a fixed UID so the
// derived ManifestCheckpoint name is deterministic) and a Ready ManifestCheckpoint for a captured PVC,
// standing in for the manifest-capture controllers that do not run in these unit tests. With these in
// place, ensureOrphanVolumeChildManifestCheckpoint finds the MCR, publishes the MCP name onto the child
// volume node, and observes it Ready — letting the orphan publish path reach completion.
func seedOrphanChildManifest(ns string, snapUID types.UID, target vcpkg.Target) []client.Object {
	vsUID := orphanVSTestUID(orphanPVCVolumeSnapshotName(snapUID, target))
	mcrName := namespacemanifest.SnapshotVolumeMCRName(vsUID)
	mcrUID := types.UID("mcr-uid-" + target.UID)
	mcr := &ssv1alpha1.ManifestCaptureRequest{
		ObjectMeta: metav1.ObjectMeta{Name: mcrName, Namespace: ns, UID: mcrUID},
		Spec: ssv1alpha1.ManifestCaptureRequestSpec{
			Targets: []ssv1alpha1.ManifestTarget{{APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: target.Name}},
		},
	}
	mcp := &ssv1alpha1.ManifestCheckpoint{
		ObjectMeta: metav1.ObjectMeta{Name: namespacemanifest.GenerateManifestCheckpointNameFromUID(mcrUID)},
	}
	meta.SetStatusCondition(&mcp.Status.Conditions, metav1.Condition{
		Type: ssv1alpha1.ManifestCheckpointConditionTypeReady, Status: metav1.ConditionTrue,
		Reason: ssv1alpha1.ManifestCheckpointConditionReasonCompleted,
	})
	return []client.Object{mcr, mcp}
}

// orphanChildDataRef reads the single published dataRef of the child volume node for a captured PVC.
func orphanChildDataRef(t *testing.T, cl client.Client, snapUID types.UID, target vcpkg.Target) *storagev1alpha1.SnapshotDataBinding {
	t.Helper()
	child := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: orphanChildContentName(snapUID, target)}, child); err != nil {
		t.Fatalf("get orphan child content for %s: %v", target.Name, err)
	}
	return child.Status.Data
}
