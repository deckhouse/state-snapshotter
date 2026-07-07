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

package usecase

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	volumecaptureuc "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase/volumecapture"
)

func TestListOwnedPVCTargets_duplicateSubtreePVCFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := "ns1"
	scheme := runtime.NewScheme()
	_ = storagev1alpha1.AddToScheme(scheme)

	// wave7 content-free coverage walks the SNAPSHOT child graph (status.childrenSnapshotRefs -> each
	// direct child's bound SnapshotContent), not the root content tree. Two domain children whose bound
	// contents claim the same PVC UID must fail closed with ErrDuplicateCoveredPVCUID before any residual
	// PVC listing. Passing a nil root content also exercises the "late Planned" pre-bind (content-free) path.
	childRef := func(name string) storagev1alpha1.SnapshotChildRef {
		return storagev1alpha1.SnapshotChildRef{APIVersion: storagev1alpha1.SchemeGroupVersion.String(), Kind: "Snapshot", Name: name}
	}
	rootNS := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: ns},
		Status:     storagev1alpha1.SnapshotStatus{ChildrenSnapshotRefs: []storagev1alpha1.SnapshotChildRef{childRef("child-a"), childRef("child-b")}},
	}
	childA := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Name: "child-a", Namespace: ns}, Status: storagev1alpha1.SnapshotStatus{BoundSnapshotContentName: "c1"}}
	childB := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Name: "child-b", Namespace: ns}, Status: storagev1alpha1.SnapshotStatus{BoundSnapshotContentName: "c2"}}
	c1 := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "c1"},
		Status:     storagev1alpha1.SnapshotContentStatus{Data: &storagev1alpha1.SnapshotDataBinding{Source: storagev1alpha1.SnapshotSubjectRef{UID: "dup"}}},
	}
	c2 := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "c2"},
		Status:     storagev1alpha1.SnapshotContentStatus{Data: &storagev1alpha1.SnapshotDataBinding{Source: storagev1alpha1.SnapshotSubjectRef{UID: "dup"}}},
	}
	cl := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(rootNS, childA, childB, c1, c2).Build()

	_, err := volumecaptureuc.ListOwnedPVCTargetsForLogicalContent(ctx, cl, rootNS, nil)
	if err == nil {
		t.Fatal("expected duplicate covered PVC error")
	}
	if !errors.Is(err, volumecaptureuc.ErrDuplicateCoveredPVCUID) {
		t.Fatalf("expected ErrDuplicateCoveredPVCUID, got %v", err)
	}
}
