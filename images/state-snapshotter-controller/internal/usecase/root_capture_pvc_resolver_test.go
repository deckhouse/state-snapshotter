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

	rootNS := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: ns}}
	rootContent := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "root-content"},
		Status: storagev1alpha1.SnapshotContentStatus{
			ChildrenSnapshotContentRefs: []storagev1alpha1.SnapshotContentChildRef{{Name: "c1"}, {Name: "c2"}},
		},
	}
	c1 := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "c1"},
		Status:     storagev1alpha1.SnapshotContentStatus{DataRef: &storagev1alpha1.SnapshotDataBinding{TargetUID: "dup"}},
	}
	c2 := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "c2"},
		Status:     storagev1alpha1.SnapshotContentStatus{DataRef: &storagev1alpha1.SnapshotDataBinding{TargetUID: "dup"}},
	}
	cl := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(rootNS, rootContent, c1, c2).Build()

	_, err := volumecaptureuc.ListOwnedPVCTargetsForLogicalContent(ctx, cl, rootNS, rootContent)
	if err == nil {
		t.Fatal("expected duplicate covered PVC error")
	}
	if !errors.Is(err, volumecaptureuc.ErrDuplicateCoveredPVCUID) {
		t.Fatalf("expected ErrDuplicateCoveredPVCUID, got %v", err)
	}
}
