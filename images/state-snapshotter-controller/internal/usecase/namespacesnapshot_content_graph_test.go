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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

func graphTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	return s
}

func registryGenericLeafPair(t *testing.T) *snapshot.GVKRegistry {
	t.Helper()
	r := snapshot.NewGVKRegistry()
	if err := r.RegisterSnapshotContentMapping(
		"GenericLeafSnapshot", "generic.state-snapshotter.test/v1",
		"GenericLeafSnapshotContent", "generic.state-snapshotter.test/v1",
	); err != nil {
		t.Fatalf("register pair: %v", err)
	}
	return r
}

func unstructuredDedicatedContent(name string, childNames ...string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "generic.state-snapshotter.test",
		Version: "v1",
		Kind:    "GenericLeafSnapshotContent",
	})
	u.SetName(name)
	if len(childNames) > 0 {
		refs := make([]interface{}, 0, len(childNames))
		for _, n := range childNames {
			refs = append(refs, map[string]interface{}{"name": n})
		}
		_ = unstructured.SetNestedSlice(u.Object, refs, "status", "childrenSnapshotContentRefs")
	}
	return u
}

func registryAcmeNestedPair(t *testing.T) *snapshot.GVKRegistry {
	t.Helper()
	r := snapshot.NewGVKRegistry()
	if err := r.RegisterSnapshotContentMapping(
		"AcmeMachineSnapshot", "acme.test/v1",
		"AcmeMachineSnapshotContent", "acme.test/v1",
	); err != nil {
		t.Fatalf("register machine: %v", err)
	}
	if err := r.RegisterSnapshotContentMapping(
		"AcmeDiskSnapshot", "acme.test/v1",
		"AcmeDiskSnapshotContent", "acme.test/v1",
	); err != nil {
		t.Fatalf("register disk: %v", err)
	}
	return r
}

func unstructuredAcmeMachineContent(name string, diskChild string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "acme.test", Version: "v1", Kind: "AcmeMachineSnapshotContent"})
	u.SetName(name)
	refs := []interface{}{map[string]interface{}{"name": diskChild}}
	_ = unstructured.SetNestedSlice(u.Object, refs, "status", "childrenSnapshotContentRefs")
	return u
}

func unstructuredAcmeDiskContent(name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "acme.test", Version: "v1", Kind: "AcmeDiskSnapshotContent"})
	u.SetName(name)
	return u
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

func TestWalkNamespaceSnapshotContentSubtreeWithRegistry_VisitsDedicatedLeaf(t *testing.T) {
	scheme := graphTestScheme(t)
	reg := registryGenericLeafPair(t)
	dedicatedLeaf := unstructuredDedicatedContent("diskc-leaf")
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
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(root, childNSC, dedicatedLeaf).Build()

	var nscNames []string
	var dedicatedNames []string
	hooks := &DedicatedContentVisitHooks{
		Visit: func(_ context.Context, gvk schema.GroupVersionKind, contentName string, _ *unstructured.Unstructured, _ bool) error {
			if gvk.Kind == "GenericLeafSnapshotContent" {
				dedicatedNames = append(dedicatedNames, contentName)
			}
			return nil
		},
	}
	err := WalkNamespaceSnapshotContentSubtreeWithRegistry(context.Background(), cl, "root",
		func(_ context.Context, nsc *storagev1alpha1.NamespaceSnapshotContent) error {
			nscNames = append(nscNames, nsc.Name)
			return nil
		},
		reg, hooks,
	)
	if err != nil {
		t.Fatal(err)
	}
	wantNSC := []string{"root", "nsc-child"}
	if !slices.Equal(nscNames, wantNSC) {
		t.Fatalf("nsc order: got %v want %v", nscNames, wantNSC)
	}
	wantDedicated := []string{"diskc-leaf"}
	if !slices.Equal(dedicatedNames, wantDedicated) {
		t.Fatalf("dedicated leaves: got %v want %v", dedicatedNames, wantDedicated)
	}
}

func TestWalkNamespaceSnapshotContentSubtree_ErrorsOnDedicatedRefWithoutRegistry(t *testing.T) {
	scheme := graphTestScheme(t)
	dedicatedLeaf := unstructuredDedicatedContent("diskc-leaf")
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
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(root, childNSC, dedicatedLeaf).Build()

	err := WalkNamespaceSnapshotContentSubtree(context.Background(), cl, "root", func(_ context.Context, _ *storagev1alpha1.NamespaceSnapshotContent) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected error for heterogeneous childrenSnapshotContentRefs without GVK registry")
	}
}

func TestWalkNamespaceSnapshotContentSubtreeWithRegistry_NoHooksStillWalksDedicatedThenNSC(t *testing.T) {
	scheme := graphTestScheme(t)
	reg := registryGenericLeafPair(t)
	dedicatedLeaf := unstructuredDedicatedContent("diskc-leaf")
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
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(root, childNSC, dedicatedLeaf).Build()

	var nscNames []string
	err := WalkNamespaceSnapshotContentSubtreeWithRegistry(context.Background(), cl, "root",
		func(_ context.Context, nsc *storagev1alpha1.NamespaceSnapshotContent) error {
			nscNames = append(nscNames, nsc.Name)
			return nil
		},
		reg, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"root", "nsc-child"}
	if !slices.Equal(nscNames, want) {
		t.Fatalf("nsc order: got %v want %v", nscNames, want)
	}
}

func TestWalkNamespaceSnapshotContentSubtreeWithRegistry_NestedDedicatedContent(t *testing.T) {
	scheme := graphTestScheme(t)
	reg := registryAcmeNestedPair(t)
	diskLeaf := unstructuredAcmeDiskContent("diskc-under-vm")
	vmContent := unstructuredAcmeMachineContent("vmc-parent", "diskc-under-vm")
	root := &storagev1alpha1.NamespaceSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "root"},
		Status: storagev1alpha1.NamespaceSnapshotContentStatus{
			ChildrenSnapshotContentRefs: []storagev1alpha1.NamespaceSnapshotContentChildRef{
				{Name: "vmc-parent"},
			},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(root, vmContent, diskLeaf).Build()

	var machineNames []string
	var diskNames []string
	hooks := &DedicatedContentVisitHooks{
		Visit: func(_ context.Context, gvk schema.GroupVersionKind, contentName string, _ *unstructured.Unstructured, _ bool) error {
			switch gvk.Kind {
			case "AcmeMachineSnapshotContent":
				machineNames = append(machineNames, contentName)
			case "AcmeDiskSnapshotContent":
				diskNames = append(diskNames, contentName)
			}
			return nil
		},
	}
	err := WalkNamespaceSnapshotContentSubtreeWithRegistry(context.Background(), cl, "root",
		func(_ context.Context, _ *storagev1alpha1.NamespaceSnapshotContent) error {
			return nil
		},
		reg, hooks,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(machineNames, []string{"vmc-parent"}) {
		t.Fatalf("machine content visits: got %v", machineNames)
	}
	if !slices.Equal(diskNames, []string{"diskc-under-vm"}) {
		t.Fatalf("disk content visits: got %v", diskNames)
	}
}
