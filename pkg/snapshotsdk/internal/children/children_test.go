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

package children

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

const childNS = "ns"

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1: %v", err)
	}
	return scheme
}

func owner() metav1.OwnerReference {
	controller := true
	return metav1.OwnerReference{APIVersion: "demo/v1", Kind: "Parent", Name: "p", UID: "p-uid", Controller: &controller}
}

func childCM() client.Object {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: childNS}}
}

func TestEnsureAllCreatesChildrenAndDeriveRefs(t *testing.T) {
	scheme := testScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()

	desired := []client.Object{childCM()}
	if err := EnsureAll(context.Background(), cl, owner(), desired); err != nil {
		t.Fatalf("ensure all: %v", err)
	}
	refs, err := DeriveRefs(scheme, desired)
	if err != nil {
		t.Fatalf("derive refs: %v", err)
	}
	if len(refs) != 1 || refs[0].Kind != "ConfigMap" || refs[0].Name != "a" || refs[0].APIVersion != "v1" {
		t.Fatalf("unexpected refs: %#v", refs)
	}
	got := &corev1.ConfigMap{}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: childNS, Name: "a"}, got); err != nil {
		t.Fatalf("expected child created: %v", err)
	}
	if len(got.OwnerReferences) != 1 || got.OwnerReferences[0].Name != "p" {
		t.Fatalf("expected child owned by parent, got %#v", got.OwnerReferences)
	}
}

func TestEnsureAllIsDeleteFree(t *testing.T) {
	scheme := testScheme(t)
	// 'old' was a previously created child; the new desired set no longer references it.
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: childNS, OwnerReferences: []metav1.OwnerReference{owner()}}}).
		Build()

	if err := EnsureAll(context.Background(), cl, owner(), nil); err != nil {
		t.Fatalf("ensure all: %v", err)
	}
	refs, err := DeriveRefs(scheme, nil)
	if err != nil {
		t.Fatalf("derive refs: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("expected empty refs for empty desired, got %#v", refs)
	}
	// SDK v1 is delete-free: the old child is detached from the published graph but NOT deleted.
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: childNS, Name: "old"}, &corev1.ConfigMap{}); err != nil {
		t.Fatalf("old child must not be deleted by SDK v1, got err=%v", err)
	}
}

func TestEnsureAllDoesNotMutateCallerTemplate(t *testing.T) {
	scheme := testScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()

	template := childCM()
	if err := EnsureAll(context.Background(), cl, owner(), []client.Object{template}); err != nil {
		t.Fatalf("ensure all: %v", err)
	}
	// The caller-owned template must be left pristine: no owner refs stamped, no resourceVersion/UID from Create.
	tmpl := template.(*corev1.ConfigMap)
	if len(tmpl.OwnerReferences) != 0 {
		t.Fatalf("template must not receive owner refs, got %#v", tmpl.OwnerReferences)
	}
	if tmpl.ResourceVersion != "" || tmpl.UID != "" {
		t.Fatalf("template must not be mutated by Create, got rv=%q uid=%q", tmpl.ResourceVersion, tmpl.UID)
	}
}

func TestEnsureAllFailsClosedOnConflictingOwner(t *testing.T) {
	scheme := testScheme(t)
	conflicting := metav1.OwnerReference{APIVersion: "demo/v1", Kind: "Parent", Name: "other"}
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: childNS, OwnerReferences: []metav1.OwnerReference{conflicting}}}).
		Build()

	if err := EnsureAll(context.Background(), cl, owner(), []client.Object{childCM()}); err == nil {
		t.Fatal("expected conflict error when adopting a child owned by another parent")
	}
	got := &corev1.ConfigMap{}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: childNS, Name: "a"}, got); err != nil {
		t.Fatalf("get child: %v", err)
	}
	if len(got.OwnerReferences) != 1 || got.OwnerReferences[0].Name != "other" {
		t.Fatalf("child must be left untouched on conflict, got %#v", got.OwnerReferences)
	}
}

func TestEnsureAllAdoptsUnownedChild(t *testing.T) {
	scheme := testScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: childNS}}).
		Build()

	if err := EnsureAll(context.Background(), cl, owner(), []client.Object{childCM()}); err != nil {
		t.Fatalf("ensure all: %v", err)
	}
	got := &corev1.ConfigMap{}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: childNS, Name: "a"}, got); err != nil {
		t.Fatalf("get child: %v", err)
	}
	if len(got.OwnerReferences) != 1 || got.OwnerReferences[0].Name != "p" {
		t.Fatalf("expected child adopted by parent, got %#v", got.OwnerReferences)
	}
}

// TestRefsEqualIgnoreOrderSetSemantics guards the topology-drift comparison: it must be set equality on the
// canonical (apiVersion,kind,name) key, never a length check. The [A,B] vs [A,C] case (equal count,
// different member) is the one a naive len() comparison would wrongly accept.
func TestRefsEqualIgnoreOrderSetSemantics(t *testing.T) {
	a := ref("a")
	b := ref("b")
	c := ref("c")
	cases := []struct {
		name        string
		left, right []storagev1alpha1.SnapshotChildRef
		wantEqual   bool
	}{
		{name: "same set, same order", left: []storagev1alpha1.SnapshotChildRef{a, b}, right: []storagev1alpha1.SnapshotChildRef{a, b}, wantEqual: true},
		{name: "same set, different order", left: []storagev1alpha1.SnapshotChildRef{a, b}, right: []storagev1alpha1.SnapshotChildRef{b, a}, wantEqual: true},
		{name: "shrunk set (count differs)", left: []storagev1alpha1.SnapshotChildRef{a, b}, right: []storagev1alpha1.SnapshotChildRef{a}, wantEqual: false},
		{name: "same count, different member", left: []storagev1alpha1.SnapshotChildRef{a, b}, right: []storagev1alpha1.SnapshotChildRef{a, c}, wantEqual: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RefsEqualIgnoreOrder(tc.left, tc.right); got != tc.wantEqual {
				t.Fatalf("RefsEqualIgnoreOrder(%v, %v) = %v, want %v", tc.left, tc.right, got, tc.wantEqual)
			}
		})
	}
}

func ref(name string) storagev1alpha1.SnapshotChildRef {
	return storagev1alpha1.SnapshotChildRef{APIVersion: "v1", Kind: "ConfigMap", Name: name}
}
