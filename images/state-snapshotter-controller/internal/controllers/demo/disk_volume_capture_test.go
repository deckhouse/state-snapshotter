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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/common"
	vcctrl "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/volumecapture"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
)

// The demo disk data leg is content-free (commit 2 content-ownership, D3): the domain controller only
// ensures the VolumeCaptureRequest (named by the disk snapshot UID, owned by the disk snapshot) and
// publishes its name; reading the VCR result, the VolumeSnapshotContent ownership handoff and dataRefs
// publication, and VCR deletion are all owned by GenericSnapshotBinderController and tested there.

const (
	dataLegNS      = "ns1"
	dataLegSnap    = "disk-snap"
	dataLegSnapUID = "disk-snap-uid"
	dataLegDisk    = "disk-vm"
	dataLegPVCName = "demo-pvc-disk"
	dataLegPVCUID  = "pvc-disk-uid"
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

func dataLegPVC() *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: dataLegNS, Name: dataLegPVCName, UID: types.UID(dataLegPVCUID)},
	}
}

// dataLegVCRName is the deterministic data-leg VCR name keyed by the disk snapshot UID (D3).
func dataLegVCRName() string { return vcpkg.SnapshotOwnedVCRName(types.UID(dataLegSnapUID)) }

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

// A manifest-only disk (no spec.persistentVolumeClaimName) publishes an empty VCR name and never creates
// a VolumeCaptureRequest.
func TestDiskDataLeg_NoPVC_NoVCR(t *testing.T) {
	cl := newDemoSourceRefFakeClient(t, dataLegDiskSnap())
	r := &DemoVirtualDiskSnapshotReconciler{Client: cl, APIReader: cl}

	vcrName, reason, _, err := r.ensureDemoVirtualDiskDataLeg(context.Background(), dataLegDiskSnap(), dataLegSource(""))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vcrName != "" || reason != "" {
		t.Fatalf("manifest-only disk: want empty vcrName/no-reason, got vcrName=%q reason=%q", vcrName, reason)
	}
	if _, ok := dataLegGetVCR(t, cl); ok {
		t.Fatalf("manifest-only disk must not create a VolumeCaptureRequest")
	}
}

// With a configured PVC and no VCR yet, the data leg creates the VCR (named by the disk snapshot UID,
// owned by the disk snapshot) and returns its name without a terminal condition.
func TestDiskDataLeg_CreatesVCR(t *testing.T) {
	cl := newDemoSourceRefFakeClient(t, dataLegDiskSnap(), dataLegPVC())
	r := &DemoVirtualDiskSnapshotReconciler{Client: cl, APIReader: cl}

	vcrName, reason, _, err := r.ensureDemoVirtualDiskDataLeg(context.Background(), dataLegDiskSnap(), dataLegSource(dataLegPVCName))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reason != "" {
		t.Fatalf("create VCR: want no terminal reason, got %q", reason)
	}
	if vcrName != dataLegVCRName() {
		t.Fatalf("want vcrName %q (keyed by snapshot UID), got %q", dataLegVCRName(), vcrName)
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

// A missing PVC is an actionable ArtifactMissing condition (config not yet present), not a raw error,
// and no VCR is created.
func TestDiskDataLeg_MissingPVCIsTerminal(t *testing.T) {
	cl := newDemoSourceRefFakeClient(t, dataLegDiskSnap())
	r := &DemoVirtualDiskSnapshotReconciler{Client: cl, APIReader: cl}

	vcrName, reason, _, err := r.ensureDemoVirtualDiskDataLeg(context.Background(), dataLegDiskSnap(), dataLegSource(dataLegPVCName))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reason != snapshotpkg.ReasonArtifactMissing {
		t.Fatalf("want reason %q, got %q", snapshotpkg.ReasonArtifactMissing, reason)
	}
	if vcrName != "" {
		t.Fatalf("missing PVC must not yield a VCR name, got %q", vcrName)
	}
	if _, ok := dataLegGetVCR(t, cl); ok {
		t.Fatalf("missing PVC must not create a VolumeCaptureRequest")
	}
}
