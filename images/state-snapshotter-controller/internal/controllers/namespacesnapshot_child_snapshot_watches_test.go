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

package controllers

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

func TestFindParentsReferencingChildSnapshot_MatchesGVKAndName(t *testing.T) {
	ctx := context.Background()
	gvk := schema.GroupVersionKind{Group: "demo.test", Version: "v1", Kind: "DemoSnap"}
	child := &unstructured.Unstructured{}
	child.SetGroupVersionKind(gvk)
	child.SetNamespace("ns-a")
	child.SetName("snap-1")

	parent := &storagev1alpha1.NamespaceSnapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns-a", Name: "parent"},
		Status: storagev1alpha1.NamespaceSnapshotStatus{
			ChildrenSnapshotRefs: []storagev1alpha1.NamespaceSnapshotChildRef{
				{
					APIVersion: "demo.test/v1",
					Kind:       "DemoSnap",
					Namespace:  "ns-a",
					Name:       "snap-1",
				},
			},
		},
	}
	sc := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(sc); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(sc).WithObjects(parent).Build()
	reqs := findParentsReferencingChildSnapshot(ctx, cl, child)
	if len(reqs) != 1 || reqs[0].Namespace != "ns-a" || reqs[0].Name != "parent" {
		t.Fatalf("got %+v", reqs)
	}
}

// Namespace-local run tree: a NamespaceSnapshot in another namespace must not be returned even if
// status.childrenSnapshotRefs would match the same GVK/name with a cross-namespace ref (invalid graph).
func TestFindParentsReferencingChildSnapshot_OnlySameNamespaceAsChild(t *testing.T) {
	ctx := context.Background()
	gvk := schema.GroupVersionKind{Group: "demo.test", Version: "v1", Kind: "DemoSnap"}
	child := &unstructured.Unstructured{}
	child.SetGroupVersionKind(gvk)
	child.SetNamespace("ns-a")
	child.SetName("snap-1")

	parentSame := &storagev1alpha1.NamespaceSnapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns-a", Name: "root"},
		Status: storagev1alpha1.NamespaceSnapshotStatus{
			ChildrenSnapshotRefs: []storagev1alpha1.NamespaceSnapshotChildRef{
				{APIVersion: "demo.test/v1", Kind: "DemoSnap", Namespace: "", Name: "snap-1"},
			},
		},
	}
	parentOther := &storagev1alpha1.NamespaceSnapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns-b", Name: "other-root"},
		Status: storagev1alpha1.NamespaceSnapshotStatus{
			ChildrenSnapshotRefs: []storagev1alpha1.NamespaceSnapshotChildRef{
				{APIVersion: "demo.test/v1", Kind: "DemoSnap", Namespace: "ns-a", Name: "snap-1"},
			},
		},
	}
	sc := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(sc); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(sc).WithObjects(parentSame, parentOther).Build()
	reqs := findParentsReferencingChildSnapshot(ctx, cl, child)
	if len(reqs) != 1 || reqs[0].Namespace != "ns-a" || reqs[0].Name != "root" {
		t.Fatalf("expected only parent in ns-a, got %+v", reqs)
	}
}

func TestFindParentsReferencingChildSnapshot_SameNameDifferentGVKNoFalsePositive(t *testing.T) {
	ctx := context.Background()
	gvkChild := schema.GroupVersionKind{Group: "demo.test", Version: "v1", Kind: "KindB"}
	child := &unstructured.Unstructured{}
	child.SetGroupVersionKind(gvkChild)
	child.SetNamespace("ns1")
	child.SetName("x")

	parent := &storagev1alpha1.NamespaceSnapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "parent"},
		Status: storagev1alpha1.NamespaceSnapshotStatus{
			ChildrenSnapshotRefs: []storagev1alpha1.NamespaceSnapshotChildRef{
				{
					APIVersion: "demo.test/v1",
					Kind:       "KindA",
					Namespace:  "ns1",
					Name:       "x",
				},
			},
		},
	}
	sc := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(sc); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(sc).WithObjects(parent).Build()
	reqs := findParentsReferencingChildSnapshot(ctx, cl, child)
	if len(reqs) != 0 {
		t.Fatalf("expected no parent, got %+v", reqs)
	}
}
