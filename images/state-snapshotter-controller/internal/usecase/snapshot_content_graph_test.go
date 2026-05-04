/*
Copyright 2025 Flant JSC

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
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

func TestWalkSnapshotContentSubtree_Order(t *testing.T) {
	scheme := rootCaptureTestScheme(t)
	root := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "root"},
		Status: storagev1alpha1.SnapshotContentStatus{
			ChildrenSnapshotContentRefs: []storagev1alpha1.SnapshotContentChildRef{{Name: "b"}, {Name: "a"}},
		},
	}
	a := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "a"}}
	b := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "b"}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(root, b, a).Build()

	var visited []string
	err := WalkSnapshotContentSubtree(context.Background(), cl, "root", func(_ context.Context, content *storagev1alpha1.SnapshotContent) error {
		visited = append(visited, content.Name)
		return nil
	})
	if err != nil {
		t.Fatalf("walk failed: %v", err)
	}
	if want := []string{"root", "a", "b"}; !reflect.DeepEqual(visited, want) {
		t.Fatalf("visited = %v, want %v", visited, want)
	}
}

func TestWalkSnapshotContentSubtree_Cycle(t *testing.T) {
	scheme := rootCaptureTestScheme(t)
	a := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "a"},
		Status: storagev1alpha1.SnapshotContentStatus{
			ChildrenSnapshotContentRefs: []storagev1alpha1.SnapshotContentChildRef{{Name: "b"}},
		},
	}
	b := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "b"},
		Status: storagev1alpha1.SnapshotContentStatus{
			ChildrenSnapshotContentRefs: []storagev1alpha1.SnapshotContentChildRef{{Name: "a"}},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(a, b).Build()

	err := WalkSnapshotContentSubtree(context.Background(), cl, "a", func(context.Context, *storagev1alpha1.SnapshotContent) error {
		return nil
	})
	if !errors.Is(err, ErrSnapshotContentCycle) {
		t.Fatalf("expected ErrSnapshotContentCycle, got %v", err)
	}
}
