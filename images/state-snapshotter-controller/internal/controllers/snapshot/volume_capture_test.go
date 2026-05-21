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
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	volumecapturectrl "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/volumecapture"
	volumecaptureuc "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase/volumecapture"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
)

func TestEnsureVolumeCaptureLeg_pendingDoesNotRequeue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testVolumeCaptureScheme(t)
	ns := "default"
	pvc := newPVC(ns, "pvc-a", "uid-a")
	snap := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "snap",
			Namespace: ns,
			Annotations: map[string]string{
				volumecaptureuc.AnnotationStubVolumeCapturePVCs: "pvc-a",
			},
		},
	}
	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "c1", UID: types.UID("content-uid")}}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pvc, snap, content).
		WithStatusSubresource(&storagev1alpha1.Snapshot{}, &storagev1alpha1.SnapshotContent{}).Build()
	r := &SnapshotReconciler{Client: cl, APIReader: cl}

	if err := r.ensureVolumeCaptureLeg(ctx, snap, content); err != nil {
		t.Fatalf("ensureVolumeCaptureLeg: %v", err)
	}
	res, err := r.reconcileVolumeCapturePublish(ctx, snap, content, false)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if res.Requeue || res.RequeueAfter > 0 {
		t.Fatalf("pending VCR must not requeue when allowRequeue=false (manifest leg may proceed), got %#v", res)
	}
}

func TestReconcileVolumeCapturePublish_wrongTargetIdentityNoPublish(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testVolumeCaptureScheme(t)
	ns := "default"
	pvc := newPVC(ns, "pvc-a", "uid-a")
	snap := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "snap",
			Namespace: ns,
			Annotations: map[string]string{volumecaptureuc.AnnotationStubVolumeCapturePVCs: "pvc-a"},
		},
	}
	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "c1", UID: types.UID("content-uid")}}
	vcr := readyVCR(ns, vcpkg.SnapshotContentVCRName(content.UID), []vcpkg.Target{
		{UID: "uid-a", APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc-a", Namespace: ns},
	}, []vcpkg.DataBinding{
		{TargetUID: "uid-a", Target: vcpkg.Target{UID: "uid-a", APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc-wrong", Namespace: ns},
			Artifact: vcpkg.ArtifactRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: "vsc-a"}},
	})

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pvc, snap, content, vcr).
		WithStatusSubresource(&storagev1alpha1.SnapshotContent{}).Build()
	r := &SnapshotReconciler{Client: cl, APIReader: cl}

	res, err := r.reconcileVolumeCapturePublish(ctx, snap, content, true)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if res.RequeueAfter == 0 && !res.Requeue {
		t.Fatalf("expected requeue for invalid target identity, got %#v", res)
	}
	got := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: content.Name}, got); err != nil {
		t.Fatalf("get content: %v", err)
	}
	if len(got.Status.DataRefs) != 0 {
		t.Fatalf("expected no published dataRefs, got %v", got.Status.DataRefs)
	}
}

func TestReconcileVolumeCapturePublish_incompleteDataRefsNoPublish(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testVolumeCaptureScheme(t)
	ns := "default"
	pvcA := newPVC(ns, "pvc-a", "uid-a")
	pvcB := newPVC(ns, "pvc-b", "uid-b")
	snap := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "snap",
			Namespace: ns,
			Annotations: map[string]string{
				volumecaptureuc.AnnotationStubVolumeCapturePVCs: "pvc-a,pvc-b",
			},
		},
		Status: storagev1alpha1.SnapshotStatus{VolumeCaptureRequestName: vcpkg.SnapshotContentVCRName("content-uid")},
	}
	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "c1", UID: types.UID("content-uid")}}
	vcr := readyVCR(ns, vcpkg.SnapshotContentVCRName(content.UID), []vcpkg.Target{
		{UID: "uid-a", APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc-a", Namespace: ns},
		{UID: "uid-b", APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc-b", Namespace: ns},
	}, []vcpkg.DataBinding{
		{TargetUID: "uid-a", Target: vcpkg.Target{UID: "uid-a", APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc-a", Namespace: ns},
			Artifact: vcpkg.ArtifactRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: "vsc-a"}},
	})

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pvcA, pvcB, snap, content, vcr).
		WithStatusSubresource(&storagev1alpha1.SnapshotContent{}).Build()
	r := &SnapshotReconciler{Client: cl, APIReader: cl}

	res, err := r.reconcileVolumeCapturePublish(ctx, snap, content, true)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if res.RequeueAfter == 0 && !res.Requeue {
		t.Fatalf("expected requeue for incomplete dataRefs, got %#v", res)
	}
	got := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: content.Name}, got); err != nil {
		t.Fatalf("get content: %v", err)
	}
	if len(got.Status.DataRefs) != 0 {
		t.Fatalf("expected no published dataRefs, got %v", got.Status.DataRefs)
	}
}

func TestReconcileVolumeCapturePublish_handoffBeforePublish(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testVolumeCaptureScheme(t)
	ns := "default"
	pvc := newPVC(ns, "pvc-a", "uid-a")
	snap := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "snap",
			Namespace:   ns,
			Annotations: map[string]string{volumecaptureuc.AnnotationStubVolumeCapturePVCs: "pvc-a"},
		},
	}
	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "c1", UID: types.UID("content-uid")}}
	vsc := volumeSnapshotContent("vsc-a", true)
	vcr := readyVCR(ns, vcpkg.SnapshotContentVCRName(content.UID), []vcpkg.Target{
		{UID: "uid-a", APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc-a", Namespace: ns},
	}, []vcpkg.DataBinding{
		{TargetUID: "uid-a", Target: vcpkg.Target{UID: "uid-a", APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc-a", Namespace: ns},
			Artifact: vcpkg.ArtifactRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: "vsc-a"}},
	})

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pvc, snap, content, vcr, vsc).
		WithStatusSubresource(&storagev1alpha1.Snapshot{}, &storagev1alpha1.SnapshotContent{}).Build()
	r := &SnapshotReconciler{Client: cl, APIReader: cl}

	if _, err := r.reconcileVolumeCapturePublish(ctx, snap, content, false); err != nil {
		t.Fatalf("publish: %v", err)
	}
	vscObj := &unstructured.Unstructured{}
	vscObj.SetGroupVersionKind(schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotContent"})
	if err := cl.Get(ctx, client.ObjectKey{Name: "vsc-a"}, vscObj); err != nil {
		t.Fatalf("get vsc: %v", err)
	}
	owned := false
	for _, ref := range vscObj.GetOwnerReferences() {
		if ref.Kind == "SnapshotContent" && ref.Name == content.Name {
			owned = true
		}
	}
	if !owned {
		t.Fatal("VSC must be owned by SnapshotContent before publish completes")
	}
	got := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: content.Name}, got); err != nil {
		t.Fatalf("get content: %v", err)
	}
	if len(got.Status.DataRefs) != 1 {
		t.Fatalf("expected published dataRefs after handoff, got %v", got.Status.DataRefs)
	}
}

func TestReconcileVolumeCaptureSteadyState_staleTargetUIDNotComplete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testVolumeCaptureScheme(t)
	ns := "default"
	snap := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: ns, UID: "snap-uid"},
		Status:     storagev1alpha1.SnapshotStatus{VolumeCaptureRequestName: "snap-vcr-content-uid"},
	}
	content := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "c1", UID: types.UID("content-uid")},
		Status: storagev1alpha1.SnapshotContentStatus{
			DataRefs: []storagev1alpha1.SnapshotDataBinding{{
				TargetUID: "wrong-uid",
				Artifact:  storagev1alpha1.SnapshotDataArtifactRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: "vsc"},
			}},
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
	if snap.Status.VolumeCaptureRequestName == "" {
		t.Fatal("volumeCaptureRequestName must not be cleared while coverage incomplete")
	}
}

func TestReconcileVolumeCapture_PublishTwoDataRefsAndCleanup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testVolumeCaptureScheme(t)
	ns := "default"
	pvcA := newPVC(ns, "pvc-a", "uid-a")
	pvcB := newPVC(ns, "pvc-b", "uid-b")
	snap := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "snap",
			Namespace:   ns,
			Annotations: map[string]string{volumecaptureuc.AnnotationStubVolumeCapturePVCs: "pvc-a,pvc-b"},
		},
	}
	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "c1", UID: types.UID("content-uid")}}
	vscA := volumeSnapshotContent("vsc-a", true)
	vscB := volumeSnapshotContent("vsc-b", true)
	vcrName := vcpkg.SnapshotContentVCRName(content.UID)
	vcr := readyVCR(ns, vcrName, []vcpkg.Target{
		{UID: "uid-a", APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc-a", Namespace: ns},
		{UID: "uid-b", APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc-b", Namespace: ns},
	}, []vcpkg.DataBinding{
		{TargetUID: "uid-a", Target: vcpkg.Target{UID: "uid-a", APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc-a", Namespace: ns},
			Artifact: vcpkg.ArtifactRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: "vsc-a"}},
		{TargetUID: "uid-b", Target: vcpkg.Target{UID: "uid-b", APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc-b", Namespace: ns},
			Artifact: vcpkg.ArtifactRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: "vsc-b"}},
	})

	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(pvcA, pvcB, snap, content, vcr, vscA, vscB).
		WithStatusSubresource(&storagev1alpha1.Snapshot{}, &storagev1alpha1.SnapshotContent{}).Build()
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
	if len(got.Status.DataRefs) != 2 {
		t.Fatalf("dataRefs len=%d want 2", len(got.Status.DataRefs))
	}
	gone := &unstructured.Unstructured{}
	gone.SetGroupVersionKind(vcpkg.VolumeCaptureRequestGVK)
	if err := cl.Get(ctx, types.NamespacedName{Namespace: ns, Name: vcrName}, gone); !apierrors.IsNotFound(err) {
		t.Fatalf("VCR should be deleted: %v", err)
	}
	fresh := &storagev1alpha1.Snapshot{}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: ns, Name: snap.Name}, fresh); err != nil {
		t.Fatalf("get snapshot: %v", err)
	}
	if fresh.Status.VolumeCaptureRequestName != "" {
		t.Fatalf("expected cleared volumeCaptureRequestName, got %q", fresh.Status.VolumeCaptureRequestName)
	}
}

func TestReconcileVolumeCapture_VCRFailedRetainsRequest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testVolumeCaptureScheme(t)
	ns := "default"
	pvc := newPVC(ns, "pvc-a", "uid-a")
	snap := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "snap",
			Namespace:   ns,
			Annotations: map[string]string{volumecaptureuc.AnnotationStubVolumeCapturePVCs: "pvc-a"},
		},
	}
	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "c1", UID: types.UID("content-uid")}}
	vcrName := vcpkg.SnapshotContentVCRName(content.UID)
	vcr := failedVCR(ns, vcrName, []vcpkg.Target{{UID: "uid-a", APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc-a", Namespace: ns}})

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pvc, snap, content, vcr).
		WithStatusSubresource(&storagev1alpha1.Snapshot{}).Build()
	r := &SnapshotReconciler{Client: cl, APIReader: cl}
	if err := r.ensureVolumeCaptureLeg(ctx, snap, content); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if _, err := r.reconcileVolumeCapturePublish(ctx, snap, content, true); err != nil {
		t.Fatalf("publish: %v", err)
	}
	fresh := &storagev1alpha1.Snapshot{}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: ns, Name: snap.Name}, fresh); err != nil {
		t.Fatalf("get snapshot: %v", err)
	}
	found := false
	for _, c := range fresh.Status.Conditions {
		if c.Type == snapshotpkg.ConditionReady && c.Reason == "VolumeCaptureFailed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("conditions=%v", fresh.Status.Conditions)
	}
	if fresh.Status.VolumeCaptureRequestName != vcrName {
		t.Fatalf("failed capture retains volumeCaptureRequestName for debug, got %q", fresh.Status.VolumeCaptureRequestName)
	}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: ns, Name: vcrName}, vcr); err != nil {
		t.Fatalf("VCR should be retained: %v", err)
	}
	got := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: content.Name}, got); err != nil {
		t.Fatalf("get content: %v", err)
	}
	if len(got.Status.DataRefs) != 0 {
		t.Fatalf("expected no dataRefs on failure, got %v", got.Status.DataRefs)
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
	byUID := make(map[string]vcpkg.Target, len(targets))
	for _, t := range targets {
		byUID[t.UID] = t
	}
	dataRefs := make([]interface{}, 0, len(refs))
	for _, r := range refs {
		tgt := r.Target
		if tgt.UID == "" {
			tgt = byUID[r.TargetUID]
		}
		dataRefs = append(dataRefs, map[string]interface{}{
			"targetUID": r.TargetUID,
			"target": map[string]interface{}{
				"uid": tgt.UID, "apiVersion": tgt.APIVersion, "kind": tgt.Kind,
				"name": tgt.Name, "namespace": tgt.Namespace,
			},
			"artifact": map[string]interface{}{
				"apiVersion": r.Artifact.APIVersion, "kind": r.Artifact.Kind, "name": r.Artifact.Name,
			},
		})
	}
	_ = unstructured.SetNestedSlice(obj.Object, dataRefs, "status", "dataRefs")
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

func volumeSnapshotContent(name string, ready bool) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotContent"})
	obj.SetName(name)
	_ = unstructured.SetNestedField(obj.Object, ready, "status", "readyToUse")
	return obj
}

func testVolumeCaptureScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = storagev1alpha1.AddToScheme(s)
	return s
}
