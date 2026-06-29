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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// The demo disk data capture is content-free (D3): the domain controller only resolves the source disk's PVC
// into a capture target; the SDK turns it into the VolumeCaptureRequest (named by the disk snapshot UID,
// owned by the disk snapshot) and the common controller owns reading the VCR result, the
// VolumeSnapshotContent ownership handoff and dataRefs publication, and VCR deletion. These unit tests
// therefore assert only the domain target-resolution decision.

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

// A manifest-only disk (no spec.persistentVolumeClaimName) yields no data-capture ref and no terminal reason,
// so the SDK ensures no VolumeCaptureRequest.
func TestDiskDataRef_NoPVC(t *testing.T) {
	cl := newDemoSourceRefFakeClient(t, dataLegDiskSnap())
	r := &DemoVirtualDiskSnapshotReconciler{Client: cl, APIReader: cl}

	dataRef, reason, _, err := r.resolveDemoVirtualDiskDataRef(context.Background(), dataLegDiskSnap(), dataLegSource(""))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dataRef != nil || reason != "" {
		t.Fatalf("manifest-only disk: want nil ref/no-reason, got dataRef=%#v reason=%q", dataRef, reason)
	}
}

// With a configured and present PVC the data capture resolves a single PVC capture ref.
func TestDiskDataRef_ResolvesPVCTarget(t *testing.T) {
	cl := newDemoSourceRefFakeClient(t, dataLegDiskSnap(), dataLegPVC())
	r := &DemoVirtualDiskSnapshotReconciler{Client: cl, APIReader: cl}

	dataRef, reason, _, err := r.resolveDemoVirtualDiskDataRef(context.Background(), dataLegDiskSnap(), dataLegSource(dataLegPVCName))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reason != "" {
		t.Fatalf("present PVC: want no terminal reason, got %q", reason)
	}
	if dataRef == nil || dataRef.UID != dataLegPVCUID || dataRef.Name != dataLegPVCName || dataRef.Kind != "PersistentVolumeClaim" {
		t.Fatalf("unexpected data-capture ref: %#v", dataRef)
	}
}

// A missing PVC is an actionable ArtifactMissing terminal reason (config not yet present), not a raw
// error, and yields no ref.
func TestDiskDataRef_MissingPVCIsTerminal(t *testing.T) {
	cl := newDemoSourceRefFakeClient(t, dataLegDiskSnap())
	r := &DemoVirtualDiskSnapshotReconciler{Client: cl, APIReader: cl}

	dataRef, reason, _, err := r.resolveDemoVirtualDiskDataRef(context.Background(), dataLegDiskSnap(), dataLegSource(dataLegPVCName))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reason != storagev1alpha1.ReasonArtifactMissing {
		t.Fatalf("want reason %q, got %q", storagev1alpha1.ReasonArtifactMissing, reason)
	}
	if dataRef != nil {
		t.Fatalf("missing PVC must not yield a ref, got %#v", dataRef)
	}
}
