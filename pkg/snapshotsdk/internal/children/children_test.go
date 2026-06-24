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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

func childCM(name string) client.Object {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: childNS}}
}

func TestReconcileCreatesAndDerivesRefs(t *testing.T) {
	scheme := testScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()

	refs, err := Reconcile(context.Background(), cl, scheme, childNS, owner(), []client.Object{childCM("a")}, nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
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

func TestReconcileGarbageCollectsOrphans(t *testing.T) {
	scheme := testScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: childNS, OwnerReferences: []metav1.OwnerReference{owner()}}}).
		Build()

	previous := []storagev1alpha1.SnapshotChildRef{{APIVersion: "v1", Kind: "ConfigMap", Name: "old"}}
	refs, err := Reconcile(context.Background(), cl, scheme, childNS, owner(), nil, previous)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("expected no refs for empty desired, got %#v", refs)
	}
	err = cl.Get(context.Background(), client.ObjectKey{Namespace: childNS, Name: "old"}, &corev1.ConfigMap{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected orphan 'old' deleted, got err=%v", err)
	}
}

func TestReconcileFailsClosedOnConflictingOwner(t *testing.T) {
	scheme := testScheme(t)
	conflicting := metav1.OwnerReference{APIVersion: "demo/v1", Kind: "Parent", Name: "other"}
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: childNS, OwnerReferences: []metav1.OwnerReference{conflicting}}}).
		Build()

	if _, err := Reconcile(context.Background(), cl, scheme, childNS, owner(), []client.Object{childCM("a")}, nil); err == nil {
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

func TestReconcileAdoptsUnownedChild(t *testing.T) {
	scheme := testScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: childNS}}).
		Build()

	if _, err := Reconcile(context.Background(), cl, scheme, childNS, owner(), []client.Object{childCM("a")}, nil); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := &corev1.ConfigMap{}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: childNS, Name: "a"}, got); err != nil {
		t.Fatalf("get child: %v", err)
	}
	if len(got.OwnerReferences) != 1 || got.OwnerReferences[0].Name != "p" {
		t.Fatalf("expected child adopted by parent, got %#v", got.OwnerReferences)
	}
}
