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

package genericbinder

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

func TestMapParentContentToChildSnapshotsAcceptsTypedAndUnstructuredContent(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	childGVK := storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot")
	child := func(name, parentName, parentUID string) *storagev1alpha1.Snapshot {
		return &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns1",
			Name:      name,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
				Kind:       "Snapshot",
				Name:       parentName,
				UID:        types.UID(parentUID),
			}},
		}}
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(
			child("waiting", "parent", "parent-uid"),
			child("other-parent", "someone-else", "other-uid"),
			child("stale-uid", "parent", "old-uid"),
		).
		Build()
	r := &GenericSnapshotBinderController{Client: cl, Scheme: scheme}
	mapFn := r.mapParentContentToChildSnapshots(childGVK)

	typed := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "parent-content"},
		Spec: storagev1alpha1.SnapshotContentSpec{
			SnapshotRef: &storagev1alpha1.SnapshotSubjectRef{
				APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
				Kind:       "Snapshot",
				Name:       "parent",
				Namespace:  "ns1",
				UID:        types.UID("parent-uid"),
			},
		},
	}
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(typed)
	if err != nil {
		t.Fatal(err)
	}
	unstructuredContent := &unstructured.Unstructured{Object: raw}
	unstructuredContent.SetGroupVersionKind(storagev1alpha1.SchemeGroupVersion.WithKind("SnapshotContent"))

	for name, obj := range map[string]client.Object{"typed": typed, "unstructured": unstructuredContent} {
		reqs := mapFn(context.Background(), obj)
		if len(reqs) != 1 || reqs[0].Namespace != "ns1" || reqs[0].Name != "waiting" {
			t.Errorf("%s content: got %v, want only ns1/waiting", name, reqs)
		}
	}

	if reqs := mapFn(context.Background(), &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "no-ref"}}); len(reqs) != 0 {
		t.Errorf("content without snapshotRef: got %v, want none", reqs)
	}
}
