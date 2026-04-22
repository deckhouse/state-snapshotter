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
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

func graphTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	return s
}

func graphTestSchemeWithDemo(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := graphTestScheme(t)
	if err := demov1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme demo: %v", err)
	}
	return s
}

func TestWalkNamespaceSnapshotContentSubtree_Order(t *testing.T) {
	scheme := graphTestScheme(t)
	// child-b before child-a by name; DFS should visit root, then a, then b (sorted by Name)
	childA := &storagev1alpha1.NamespaceSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "child-a"},
		Status:     storagev1alpha1.NamespaceSnapshotContentStatus{},
	}
	childB := &storagev1alpha1.NamespaceSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "child-b"},
		Status:     storagev1alpha1.NamespaceSnapshotContentStatus{},
	}
	root := &storagev1alpha1.NamespaceSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "root"},
		Status: storagev1alpha1.NamespaceSnapshotContentStatus{
			ChildrenSnapshotContentRefs: []storagev1alpha1.NamespaceSnapshotContentChildRef{
				{Name: "child-b"},
				{Name: "child-a"},
			},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(root, childA, childB).Build()

	var order []string
	err := WalkNamespaceSnapshotContentSubtree(context.Background(), cl, "root", func(_ context.Context, nsc *storagev1alpha1.NamespaceSnapshotContent) error {
		order = append(order, nsc.Name)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"root", "child-a", "child-b"}
	if len(order) != len(want) {
		t.Fatalf("order=%v", order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("at %d: got %q want %q (full %v)", i, order[i], want[i], order)
		}
	}
}

func TestWalkNamespaceSnapshotContentSubtree_Cycle(t *testing.T) {
	scheme := graphTestScheme(t)
	a := &storagev1alpha1.NamespaceSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "nsc-a"},
		Status: storagev1alpha1.NamespaceSnapshotContentStatus{
			ChildrenSnapshotContentRefs: []storagev1alpha1.NamespaceSnapshotContentChildRef{{Name: "nsc-b"}},
		},
	}
	b := &storagev1alpha1.NamespaceSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "nsc-b"},
		Status: storagev1alpha1.NamespaceSnapshotContentStatus{
			ChildrenSnapshotContentRefs: []storagev1alpha1.NamespaceSnapshotContentChildRef{{Name: "nsc-a"}},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(a, b).Build()
	err := WalkNamespaceSnapshotContentSubtree(context.Background(), cl, "nsc-a", func(context.Context, *storagev1alpha1.NamespaceSnapshotContent) error {
		return nil
	})
	if !errors.Is(err, ErrNamespaceSnapshotContentCycle) {
		t.Fatalf("got %v", err)
	}
}

func TestWalkNamespaceSnapshotContentSubtreeWithDemoLeaves_VisitsDemoLeaf(t *testing.T) {
	scheme := graphTestSchemeWithDemo(t)
	demoLeaf := &demov1alpha1.DemoVirtualDiskSnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "diskc-leaf"}}
	childNSC := &storagev1alpha1.NamespaceSnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "nsc-child"}}
	root := &storagev1alpha1.NamespaceSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "root"},
		Status: storagev1alpha1.NamespaceSnapshotContentStatus{
			ChildrenSnapshotContentRefs: []storagev1alpha1.NamespaceSnapshotContentChildRef{
				{Name: "nsc-child"},
				{Name: "diskc-leaf"},
			},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(root, childNSC, demoLeaf).Build()

	var nscNames []string
	var demoNames []string
	err := WalkNamespaceSnapshotContentSubtreeWithDemoLeaves(context.Background(), cl, "root",
		func(_ context.Context, nsc *storagev1alpha1.NamespaceSnapshotContent) error {
			nscNames = append(nscNames, nsc.Name)
			return nil
		},
		func(_ context.Context, d *demov1alpha1.DemoVirtualDiskSnapshotContent) error {
			demoNames = append(demoNames, d.Name)
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	// Children sorted by Name: diskc-leaf before nsc-child; demo is a leaf, then NSC subtree.
	wantNSC := []string{"root", "nsc-child"}
	if !slices.Equal(nscNames, wantNSC) {
		t.Fatalf("nsc order: got %v want %v", nscNames, wantNSC)
	}
	wantDemo := []string{"diskc-leaf"}
	if !slices.Equal(demoNames, wantDemo) {
		t.Fatalf("demo leaves: got %v want %v", demoNames, wantDemo)
	}
}

func TestWalkNamespaceSnapshotContentSubtree_SkipsDemoLeavesWithoutCallback(t *testing.T) {
	scheme := graphTestSchemeWithDemo(t)
	demoLeaf := &demov1alpha1.DemoVirtualDiskSnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "diskc-leaf"}}
	childNSC := &storagev1alpha1.NamespaceSnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "nsc-child"}}
	root := &storagev1alpha1.NamespaceSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "root"},
		Status: storagev1alpha1.NamespaceSnapshotContentStatus{
			ChildrenSnapshotContentRefs: []storagev1alpha1.NamespaceSnapshotContentChildRef{
				{Name: "diskc-leaf"},
				{Name: "nsc-child"},
			},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(root, childNSC, demoLeaf).Build()

	var nscNames []string
	err := WalkNamespaceSnapshotContentSubtree(context.Background(), cl, "root", func(_ context.Context, nsc *storagev1alpha1.NamespaceSnapshotContent) error {
		nscNames = append(nscNames, nsc.Name)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"root", "nsc-child"}
	if !slices.Equal(nscNames, want) {
		t.Fatalf("nsc order: got %v want %v", nscNames, want)
	}
}
