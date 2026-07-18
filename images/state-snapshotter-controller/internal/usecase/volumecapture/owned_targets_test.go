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

package volumecapture

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	vcctrl "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/volumecapture"
	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
)

func TestListOwnedPVCTargetsForSnapshotContent_doesNotListNamespacePVCs(t *testing.T) {
	t.Parallel()
	ns := "ns"
	pvcA := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "pvc-a", Namespace: ns, UID: "uid-a"}}
	pvcB := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "pvc-b", Namespace: ns, UID: "uid-b"}}
	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "domain-content", UID: "content-uid"}}
	cl := fakeclient.NewClientBuilder().WithScheme(testSubtreeScheme(t)).WithObjects(pvcA, pvcB, content).Build()

	got, err := ListOwnedPVCTargetsForSnapshotContent(context.Background(), cl, ns, content)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("domain leaf without dataRefs/VCR must not own all namespace PVCs, got %#v", got)
	}
}

func TestListOwnedPVCTargetsForSnapshotContent_pendingVCRTargets(t *testing.T) {
	t.Parallel()
	ns := "ns"
	pvcA := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "pvc-a", Namespace: ns, UID: "uid-a"}}
	pvcB := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "pvc-b", Namespace: ns, UID: "uid-b"}}
	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "domain-content", UID: "content-uid"}}
	vcr := vcctrl.NewVolumeCaptureRequestObject(ns, vcpkg.SnapshotContentVCRName(content.UID), metav1.OwnerReference{}, []vcpkg.Target{{
		UID:        "uid-a",
		APIVersion: corev1.SchemeGroupVersion.String(),
		Kind:       "PersistentVolumeClaim",
		Name:       "pvc-a",
		Namespace:  ns,
	}})
	cl := fakeclient.NewClientBuilder().WithScheme(testSubtreeScheme(t)).WithObjects(pvcA, pvcB, content, vcr).Build()

	got, err := ListOwnedPVCTargetsForSnapshotContent(context.Background(), cl, ns, content)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].UID != "uid-a" {
		t.Fatalf("expected only pending VCR pvc-a, got %#v", got)
	}
}

func TestCollectSubtreeCoveredPVCUIDs_staleUIDDoesNotCoverUnrelatedPVC(t *testing.T) {
	t.Parallel()
	ns := "ns"
	root := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "root"},
		Status: storagev1alpha1.SnapshotContentStatus{
			ChildrenSnapshotContentRefs: []storagev1alpha1.SnapshotContentChildRef{{Name: "child"}},
		},
	}
	child := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "child"},
		Status: storagev1alpha1.SnapshotContentStatus{
			Data: &storagev1alpha1.SnapshotDataBinding{SourceRef: storagev1alpha1.SnapshotSubjectRef{UID: "stale-uid-not-in-cluster"}},
		},
	}
	pvcA := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "pvc-a", Namespace: ns, UID: "uid-a"}}
	pvcB := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "pvc-b", Namespace: ns, UID: "uid-b"}}
	snap := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Name: "root-snap", Namespace: ns}}
	cl := fakeclient.NewClientBuilder().WithScheme(testSubtreeScheme(t)).WithObjects(root, child, pvcA, pvcB, snap).Build()
	covered, err := CollectSubtreeCoveredPVCUIDs(context.Background(), cl, ns, root, allKindsDataBearing)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := covered["stale-uid-not-in-cluster"]; !ok {
		t.Fatalf("expected stale uid in covered set, got %v", covered)
	}

	got, err := ListOwnedPVCTargetsForLogicalContent(context.Background(), cl, snap, root, allKindsDataBearing)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("stale child dataRef uid must not remove real namespace PVCs from root residual, got %#v", got)
	}
}

func TestIsResidualRootPVCCaptureScope(t *testing.T) {
	t.Parallel()
	rootSnap := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: "ns"}}
	rootContent := &storagev1alpha1.SnapshotContent{
		Status: storagev1alpha1.SnapshotContentStatus{
			ChildrenSnapshotContentRefs: []storagev1alpha1.SnapshotContentChildRef{{Name: "c1"}},
		},
	}
	if !IsResidualRootPVCCaptureScope(rootSnap, rootContent) {
		t.Fatal("expected residual root scope with children")
	}
	domainContent := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "disk"}}
	if IsResidualRootPVCCaptureScope(&storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Namespace: "ns"}}, domainContent) {
		t.Fatal("domain-only entry must not be residual root scope")
	}
}
