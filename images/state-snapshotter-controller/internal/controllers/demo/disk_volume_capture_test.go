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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/common"
	vcctrl "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/volumecapture"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
)

const (
	dataLegNS      = "ns1"
	dataLegSnap    = "disk-snap"
	dataLegSnapUID = "disk-snap-uid"
	dataLegDisk    = "disk-vm"
	dataLegPVCName = "demo-pvc-disk"
	dataLegPVCUID  = "pvc-disk-uid"
	dataLegContent = "demodiskc-1"
	dataLegConUID  = "content-uid"
)

func dataLegDiskSnap() *demov1alpha1.DemoVirtualDiskSnapshot {
	return &demov1alpha1.DemoVirtualDiskSnapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: dataLegNS, Name: dataLegSnap, UID: types.UID(dataLegSnapUID)},
	}
}

func dataLegSource(pvcName string) *demov1alpha1.DemoVirtualDisk {
	return &demov1alpha1.DemoVirtualDisk{
		ObjectMeta: metav1.ObjectMeta{Namespace: dataLegNS, Name: dataLegDisk},
		Spec:       demov1alpha1.DemoVirtualDiskSpec{PersistentVolumeClaimName: pvcName},
	}
}

func dataLegSnapContent() *storagev1alpha1.SnapshotContent {
	return &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: dataLegContent, UID: types.UID(dataLegConUID)},
	}
}

func dataLegPVC() *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: dataLegNS, Name: dataLegPVCName, UID: types.UID(dataLegPVCUID)},
	}
}

func dataLegTarget() vcpkg.Target {
	return vcpkg.Target{
		UID:        dataLegPVCUID,
		APIVersion: corev1.SchemeGroupVersion.String(),
		Kind:       "PersistentVolumeClaim",
		Name:       dataLegPVCName,
		Namespace:  dataLegNS,
	}
}

func dataLegVCRName() string { return vcpkg.SnapshotContentVCRName(types.UID(dataLegConUID)) }

func dataLegReadyVCR(refs []vcpkg.DataBinding) *unstructured.Unstructured {
	obj := vcctrl.NewVolumeCaptureRequestObject(dataLegNS, dataLegVCRName(), metav1.OwnerReference{}, []vcpkg.Target{dataLegTarget()})
	_ = unstructured.SetNestedSlice(obj.Object, []interface{}{
		map[string]interface{}{
			"type":   vcpkg.ConditionTypeReady,
			"status": string(metav1.ConditionTrue),
			"reason": vcpkg.ConditionReasonCompleted,
		},
	}, "status", "conditions")
	dataRefs := make([]interface{}, 0, len(refs))
	for _, r := range refs {
		dataRefs = append(dataRefs, map[string]interface{}{
			"targetUID": r.TargetUID,
			"target": map[string]interface{}{
				"uid": r.Target.UID, "apiVersion": r.Target.APIVersion, "kind": r.Target.Kind,
				"name": r.Target.Name, "namespace": r.Target.Namespace,
			},
			"artifact": map[string]interface{}{
				"apiVersion": r.Artifact.APIVersion, "kind": r.Artifact.Kind, "name": r.Artifact.Name,
			},
		})
	}
	_ = unstructured.SetNestedSlice(obj.Object, dataRefs, "status", "dataRefs")
	return obj
}

func dataLegFailedVCR() *unstructured.Unstructured {
	obj := vcctrl.NewVolumeCaptureRequestObject(dataLegNS, dataLegVCRName(), metav1.OwnerReference{}, []vcpkg.Target{dataLegTarget()})
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

func dataLegVSC(name string, owner *storagev1alpha1.SnapshotContent) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotContent"})
	obj.SetName(name)
	_ = unstructured.SetNestedField(obj.Object, "Retain", "spec", "deletionPolicy")
	_ = unstructured.SetNestedField(obj.Object, true, "status", "readyToUse")
	if owner != nil {
		controller := true
		obj.SetOwnerReferences([]metav1.OwnerReference{{
			APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
			Kind:       "SnapshotContent",
			Name:       owner.Name,
			UID:        owner.UID,
			Controller: &controller,
		}})
	}
	return obj
}

func dataLegBinding(vscName string) vcpkg.DataBinding {
	return vcpkg.DataBinding{
		TargetUID: dataLegPVCUID,
		Target:    dataLegTarget(),
		Artifact:  vcpkg.ArtifactRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: vscName},
	}
}

func dataLegPublishedDataRef(vscName string) storagev1alpha1.SnapshotDataBinding {
	return storagev1alpha1.SnapshotDataBinding{
		TargetUID: dataLegPVCUID,
		Target: storagev1alpha1.SnapshotSubjectRef{
			APIVersion: corev1.SchemeGroupVersion.String(),
			Kind:       "PersistentVolumeClaim",
			Name:       dataLegPVCName,
			Namespace:  dataLegNS,
			UID:        types.UID(dataLegPVCUID),
		},
		Artifact: storagev1alpha1.SnapshotDataArtifactRef{
			APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: vscName,
		},
	}
}

func dataLegGetVCR(t *testing.T, cl client.Client) (*unstructured.Unstructured, bool) {
	t.Helper()
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(vcpkg.VolumeCaptureRequestGVK)
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: dataLegNS, Name: dataLegVCRName()}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false
		}
		t.Fatalf("get VCR: %v", err)
	}
	return obj, true
}

// A manifest-only disk (no spec.persistentVolumeClaimName) reports the data leg complete and never
// creates a VolumeCaptureRequest.
func TestDiskDataLeg_NoPVC_NoVCR(t *testing.T) {
	cl := newDemoSourceRefFakeClient(t, dataLegDiskSnap(), dataLegSnapContent())
	r := &DemoVirtualDiskSnapshotReconciler{Client: cl, APIReader: cl}

	complete, reason, _, err := r.reconcileDemoVirtualDiskDataLeg(context.Background(), dataLegDiskSnap(), dataLegSource(""), dataLegContent)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !complete || reason != "" {
		t.Fatalf("manifest-only disk: want complete/no-reason, got complete=%v reason=%q", complete, reason)
	}
	if _, ok := dataLegGetVCR(t, cl); ok {
		t.Fatalf("manifest-only disk must not create a VolumeCaptureRequest")
	}
}

// With a configured PVC and no VCR yet, the data leg creates a pending VCR (coverage) without a terminal
// condition and reports not-complete.
func TestDiskDataLeg_CreatesPendingVCR(t *testing.T) {
	cl := newDemoSourceRefFakeClient(t, dataLegDiskSnap(), dataLegSnapContent(), dataLegPVC())
	r := &DemoVirtualDiskSnapshotReconciler{Client: cl, APIReader: cl}

	complete, reason, _, err := r.reconcileDemoVirtualDiskDataLeg(context.Background(), dataLegDiskSnap(), dataLegSource(dataLegPVCName), dataLegContent)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if complete || reason != "" {
		t.Fatalf("pending create: want not-complete/no-reason, got complete=%v reason=%q", complete, reason)
	}
	vcr, ok := dataLegGetVCR(t, cl)
	if !ok {
		t.Fatalf("expected a VolumeCaptureRequest to be created")
	}
	targets, err := vcctrl.ParseVolumeCaptureTargets(vcr)
	if err != nil {
		t.Fatalf("parse VCR targets: %v", err)
	}
	if len(targets) != 1 || targets[0].UID != dataLegPVCUID || targets[0].Name != dataLegPVCName {
		t.Fatalf("VCR target mismatch: %#v", targets)
	}
	if !demoDiskVCRHasOwnerRef(vcr.GetOwnerReferences(), demoSnapshotOwnerReference(demov1alpha1.SchemeGroupVersion.String(), controllercommon.KindDemoVirtualDiskSnapshot, dataLegSnap, types.UID(dataLegSnapUID))) {
		t.Fatalf("VCR missing ownerRef to disk snapshot: %#v", vcr.GetOwnerReferences())
	}
}

// A Ready VCR with consistent dataRefs is handed off: the bound VSC is published into the disk content
// dataRefs[] and the data leg reports complete.
func TestDiskDataLeg_ReadyVCRPublishesDataRefs(t *testing.T) {
	content := dataLegSnapContent()
	vcr := dataLegReadyVCR([]vcpkg.DataBinding{dataLegBinding("vsc-1")})
	cl := newDemoSourceRefFakeClient(t, dataLegDiskSnap(), content, dataLegPVC(), vcr, dataLegVSC("vsc-1", nil))
	r := &DemoVirtualDiskSnapshotReconciler{Client: cl, APIReader: cl}

	complete, reason, _, err := r.reconcileDemoVirtualDiskDataLeg(context.Background(), dataLegDiskSnap(), dataLegSource(dataLegPVCName), dataLegContent)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !complete || reason != "" {
		t.Fatalf("publish: want complete/no-reason, got complete=%v reason=%q", complete, reason)
	}
	got := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: dataLegContent}, got); err != nil {
		t.Fatalf("get content: %v", err)
	}
	if len(got.Status.DataRefs) != 1 {
		t.Fatalf("expected 1 dataRef on disk content, got %d", len(got.Status.DataRefs))
	}
	if got.Status.DataRefs[0].TargetUID != dataLegPVCUID || got.Status.DataRefs[0].Artifact.Name != "vsc-1" {
		t.Fatalf("unexpected published dataRef: %#v", got.Status.DataRefs[0])
	}
}

// A failed VCR surfaces an actionable terminal VolumeCaptureFailed condition instead of an endless requeue.
func TestDiskDataLeg_FailedVCRIsTerminal(t *testing.T) {
	cl := newDemoSourceRefFakeClient(t, dataLegDiskSnap(), dataLegSnapContent(), dataLegPVC(), dataLegFailedVCR())
	r := &DemoVirtualDiskSnapshotReconciler{Client: cl, APIReader: cl}

	complete, reason, msg, err := r.reconcileDemoVirtualDiskDataLeg(context.Background(), dataLegDiskSnap(), dataLegSource(dataLegPVCName), dataLegContent)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if complete {
		t.Fatalf("failed VCR must not report complete")
	}
	if reason != snapshotpkg.ReasonVolumeCaptureFailed {
		t.Fatalf("want reason %q, got %q (msg=%q)", snapshotpkg.ReasonVolumeCaptureFailed, reason, msg)
	}
}

// A missing PVC is an actionable ArtifactMissing condition (config not yet present), not a raw error.
func TestDiskDataLeg_MissingPVCIsTerminal(t *testing.T) {
	cl := newDemoSourceRefFakeClient(t, dataLegDiskSnap(), dataLegSnapContent())
	r := &DemoVirtualDiskSnapshotReconciler{Client: cl, APIReader: cl}

	complete, reason, _, err := r.reconcileDemoVirtualDiskDataLeg(context.Background(), dataLegDiskSnap(), dataLegSource(dataLegPVCName), dataLegContent)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if complete {
		t.Fatalf("missing PVC must not report complete")
	}
	if reason != snapshotpkg.ReasonArtifactMissing {
		t.Fatalf("want reason %q, got %q", snapshotpkg.ReasonArtifactMissing, reason)
	}
}

// Once dataRefs cover the PVC and the bound VSC is owned by the content, the transient VCR is deleted.
func TestDiskDataLeg_SteadyStateDeletesVCR(t *testing.T) {
	content := dataLegSnapContent()
	content.Status.DataRefs = []storagev1alpha1.SnapshotDataBinding{dataLegPublishedDataRef("vsc-1")}
	vcr := dataLegReadyVCR([]vcpkg.DataBinding{dataLegBinding("vsc-1")})
	cl := newDemoSourceRefFakeClient(t, dataLegDiskSnap(), content, dataLegPVC(), vcr, dataLegVSC("vsc-1", content))
	r := &DemoVirtualDiskSnapshotReconciler{Client: cl, APIReader: cl}

	complete, reason, _, err := r.reconcileDemoVirtualDiskDataLeg(context.Background(), dataLegDiskSnap(), dataLegSource(dataLegPVCName), dataLegContent)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !complete || reason != "" {
		t.Fatalf("steady state: want complete/no-reason, got complete=%v reason=%q", complete, reason)
	}
	if _, ok := dataLegGetVCR(t, cl); ok {
		t.Fatalf("steady-state VCR should be deleted after durable handoff")
	}
}
