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
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	vcctrl "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/volumecapture"
	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
)

func TestCollectSubtreeCoveredPVCUIDs_emptySubtree(t *testing.T) {
	t.Parallel()
	root := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "root"}}
	cl := fake.NewClientBuilder().WithScheme(testSubtreeScheme(t)).WithObjects(root).Build()
	got, err := CollectSubtreeCoveredPVCUIDs(context.Background(), cl, "ns", root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty set, got %v", got)
	}
}

func TestCollectSubtreeCoveredPVCUIDs_oneChildDataRef(t *testing.T) {
	t.Parallel()
	root := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "root"},
		Status: storagev1alpha1.SnapshotContentStatus{
			ChildrenSnapshotContentRefs: []storagev1alpha1.SnapshotContentChildRef{{Name: "child"}},
		},
	}
	child := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "child"},
		Status: storagev1alpha1.SnapshotContentStatus{
			Data: &storagev1alpha1.SnapshotDataBinding{
				Source: storagev1alpha1.SnapshotSubjectRef{
					UID:        "uid-a",
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "PersistentVolumeClaim",
					Name:       "pvc-a",
					Namespace:  "ns",
				},
			},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(testSubtreeScheme(t)).WithObjects(root, child).Build()
	got, err := CollectSubtreeCoveredPVCUIDs(context.Background(), cl, "ns", root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := got[types.UID("uid-a")]; !ok {
		t.Fatalf("expected uid-a in covered set, got %v", got)
	}
}

func TestCollectSubtreeCoveredPVCUIDs_twoChildrenDifferentUIDs(t *testing.T) {
	t.Parallel()
	root := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "root"},
		Status: storagev1alpha1.SnapshotContentStatus{
			ChildrenSnapshotContentRefs: []storagev1alpha1.SnapshotContentChildRef{{Name: "c1"}, {Name: "c2"}},
		},
	}
	c1 := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "c1"},
		Status: storagev1alpha1.SnapshotContentStatus{
			Data: &storagev1alpha1.SnapshotDataBinding{Source: storagev1alpha1.SnapshotSubjectRef{UID: "uid-1"}},
		},
	}
	c2 := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "c2"},
		Status: storagev1alpha1.SnapshotContentStatus{
			Data: &storagev1alpha1.SnapshotDataBinding{Source: storagev1alpha1.SnapshotSubjectRef{UID: "uid-2"}},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(testSubtreeScheme(t)).WithObjects(root, c1, c2).Build()
	got, err := CollectSubtreeCoveredPVCUIDs(context.Background(), cl, "ns", root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 UIDs, got %v", got)
	}
}

func TestCollectSubtreeCoveredPVCUIDs_duplicateUIDFailsClosed(t *testing.T) {
	t.Parallel()
	root := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "root"},
		Status: storagev1alpha1.SnapshotContentStatus{
			ChildrenSnapshotContentRefs: []storagev1alpha1.SnapshotContentChildRef{{Name: "c1"}, {Name: "c2"}},
		},
	}
	c1 := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "c1"},
		Status: storagev1alpha1.SnapshotContentStatus{
			Data: &storagev1alpha1.SnapshotDataBinding{Source: storagev1alpha1.SnapshotSubjectRef{UID: "dup"}},
		},
	}
	c2 := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "c2"},
		Status: storagev1alpha1.SnapshotContentStatus{
			Data: &storagev1alpha1.SnapshotDataBinding{Source: storagev1alpha1.SnapshotSubjectRef{UID: "dup"}},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(testSubtreeScheme(t)).WithObjects(root, c1, c2).Build()
	_, err := CollectSubtreeCoveredPVCUIDs(context.Background(), cl, "ns", root)
	if err == nil {
		t.Fatal("expected duplicate covered PVC UID error")
	}
	if !errors.Is(err, ErrDuplicateCoveredPVCUID) {
		t.Fatalf("expected ErrDuplicateCoveredPVCUID, got %v", err)
	}
}

func TestCollectSubtreeCoveredPVCUIDs_missingChildContent(t *testing.T) {
	t.Parallel()
	root := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "root"},
		Status: storagev1alpha1.SnapshotContentStatus{
			ChildrenSnapshotContentRefs: []storagev1alpha1.SnapshotContentChildRef{{Name: "missing"}},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(testSubtreeScheme(t)).WithObjects(root).Build()
	_, err := CollectSubtreeCoveredPVCUIDs(context.Background(), cl, "ns", root)
	if err == nil {
		t.Fatal("expected error for missing child SnapshotContent")
	}
}

func TestCollectSubtreeCoveredPVCUIDs_pendingVCRTargets(t *testing.T) {
	t.Parallel()
	root := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "root"},
		Status: storagev1alpha1.SnapshotContentStatus{
			ChildrenSnapshotContentRefs: []storagev1alpha1.SnapshotContentChildRef{{Name: "child"}},
		},
	}
	child := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "child", UID: types.UID("child-uid")},
	}
	vcr := vcctrl.NewVolumeCaptureRequestObject("ns", vcpkg.SnapshotContentVCRName(child.UID), metav1.OwnerReference{}, []vcpkg.Target{{
		UID: "uid-vcr", APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc-a", Namespace: "ns",
	}})
	cl := fake.NewClientBuilder().WithScheme(testSubtreeScheme(t)).WithObjects(root, child, vcr).Build()
	got, err := CollectSubtreeCoveredPVCUIDs(context.Background(), cl, "ns", root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := got[types.UID("uid-vcr")]; !ok {
		t.Fatalf("expected VCR target UID in covered set, got %v", got)
	}
}

func TestListOwnedPVCTargets_residualExcludesSubtreeCovered(t *testing.T) {
	t.Parallel()
	ns := "ns"
	pvcRoot := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "root-pvc", Namespace: ns, UID: types.UID("uid-root")}}
	pvcChild := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "child-pvc", Namespace: ns, UID: types.UID("uid-child")}}
	// wave7 root-content-free coverage: the covered child is discovered through the Snapshot child graph — a
	// child Snapshot node bound to its OWN content, whose data binding covers uid-child — NOT the root's
	// SnapshotContent subtree.
	childContent := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "child-content"},
		Status: storagev1alpha1.SnapshotContentStatus{
			Data: &storagev1alpha1.SnapshotDataBinding{Source: storagev1alpha1.SnapshotSubjectRef{UID: "uid-child"}},
		},
	}
	childSnap := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "child-snap", Namespace: ns},
		Status: storagev1alpha1.SnapshotStatus{
			BoundSnapshotContentName: "child-content",
		},
	}
	snap := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: ns},
		Status: storagev1alpha1.SnapshotStatus{
			ChildrenSnapshotRefs: []storagev1alpha1.SnapshotChildRef{
				{APIVersion: storagev1alpha1.SchemeGroupVersion.String(), Kind: "Snapshot", Name: "child-snap"},
			},
		},
	}
	root := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "root"}}
	cl := fake.NewClientBuilder().WithScheme(testSubtreeScheme(t)).WithObjects(pvcRoot, pvcChild, childContent, childSnap, snap, root).Build()
	got, err := ListOwnedPVCTargetsForLogicalContent(context.Background(), cl, snap, root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].UID != "uid-root" {
		t.Fatalf("expected only residual root PVC, got %#v", got)
	}
}

func testSubtreeScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	_ = storagev1alpha1.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	return s
}
