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

package ownerref

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func desiredOwner() metav1.OwnerReference {
	controller := true
	return metav1.OwnerReference{APIVersion: "demo/v1", Kind: "Parent", Name: "p", UID: "p-uid", Controller: &controller}
}

func TestEnsurePreservesUnrelatedAndAddsDesired(t *testing.T) {
	unrelated := metav1.OwnerReference{APIVersion: "example.io/v1", Kind: "AuditAnchor", Name: "audit"}
	obj := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns", OwnerReferences: []metav1.OwnerReference{unrelated}}}

	if err := Ensure(obj, desiredOwner()); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	refs := obj.GetOwnerReferences()
	if len(refs) != 2 {
		t.Fatalf("expected 2 ownerRefs, got %#v", refs)
	}
	if !hasRef(refs, "example.io/v1", "AuditAnchor", "audit") || !hasRef(refs, "demo/v1", "Parent", "p") {
		t.Fatalf("ownerRefs missing expected entries: %#v", refs)
	}
}

func TestEnsureIsIdempotent(t *testing.T) {
	obj := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns", OwnerReferences: []metav1.OwnerReference{desiredOwner()}}}

	if err := Ensure(obj, desiredOwner()); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if refs := obj.GetOwnerReferences(); len(refs) != 1 {
		t.Fatalf("expected ownerRefs unchanged (1), got %#v", refs)
	}
}

func TestEnsureFailsClosedOnDifferentParentSameKind(t *testing.T) {
	other := metav1.OwnerReference{APIVersion: "demo/v1", Kind: "Parent", Name: "other"}
	obj := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns", OwnerReferences: []metav1.OwnerReference{other}}}

	if err := Ensure(obj, desiredOwner()); err == nil {
		t.Fatal("expected conflict error for a different parent of the same kind")
	}
	// The object must be left untouched on conflict.
	if refs := obj.GetOwnerReferences(); len(refs) != 1 || refs[0].Name != "other" {
		t.Fatalf("object mutated on conflict: %#v", refs)
	}
}

func TestEnsureFailsClosedOnForeignController(t *testing.T) {
	controller := true
	foreign := metav1.OwnerReference{APIVersion: "other/v1", Kind: "Boss", Name: "boss", Controller: &controller}
	obj := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns", OwnerReferences: []metav1.OwnerReference{foreign}}}

	if err := Ensure(obj, desiredOwner()); err == nil {
		t.Fatal("expected conflict error for a foreign controller ownerRef")
	}
}

func hasRef(refs []metav1.OwnerReference, apiVersion, kind, name string) bool {
	for _, r := range refs {
		if r.APIVersion == apiVersion && r.Kind == kind && r.Name == name {
			return true
		}
	}
	return false
}
