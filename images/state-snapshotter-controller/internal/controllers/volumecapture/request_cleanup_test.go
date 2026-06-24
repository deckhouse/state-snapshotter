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

package volumecapture

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
)

func TestVolumeSnapshotContentOwnedByContent_rejectsEmptyOwnerUID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	content := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "content-1", UID: types.UID("content-uid")},
	}
	vsc := &unstructured.Unstructured{}
	vsc.SetGroupVersionKind(schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotContent"})
	vsc.SetName("vsc-a")
	vsc.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
		Kind:       "SnapshotContent",
		Name:       content.Name,
		UID:        "",
	}})

	scheme := runtime.NewScheme()
	_ = storagev1alpha1.AddToScheme(scheme)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vsc).Build()

	ok, err := VolumeSnapshotContentOwnedByContent(ctx, cl, "vsc-a", content)
	if err != nil {
		t.Fatalf("VolumeSnapshotContentOwnedByContent: %v", err)
	}
	if ok {
		t.Fatal("empty ownerRef UID must not count as owned by SnapshotContent")
	}
}

func TestVolumeCaptureRequestSafeToDelete_emptyOwnerUIDNotSafe(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := "default"
	contentUID := types.UID("content-uid")
	content := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "content-1", UID: contentUID},
		Status: storagev1alpha1.SnapshotContentStatus{
			DataRef: &storagev1alpha1.SnapshotDataBinding{
				TargetUID: "uid-a",
				Artifact: storagev1alpha1.SnapshotDataArtifactRef{
					APIVersion: "snapshot.storage.k8s.io/v1",
					Kind:       "VolumeSnapshotContent",
					Name:       "vsc-a",
				},
			},
		},
	}
	vcr := &unstructured.Unstructured{}
	vcr.SetGroupVersionKind(vcpkg.VolumeCaptureRequestGVK)
	vcr.SetName(vcpkg.SnapshotContentVCRName(contentUID))
	vcr.SetNamespace(ns)
	_ = unstructured.SetNestedSlice(vcr.Object, []interface{}{
		map[string]interface{}{
			"targetUID": "uid-a",
			"artifact": map[string]interface{}{
				"apiVersion": "snapshot.storage.k8s.io/v1",
				"kind":       "VolumeSnapshotContent",
				"name":       "vsc-a",
			},
		},
	}, "status", "dataRefs")

	vsc := &unstructured.Unstructured{}
	vsc.SetGroupVersionKind(schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotContent"})
	vsc.SetName("vsc-a")
	vsc.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
		Kind:       "SnapshotContent",
		Name:       content.Name,
	}})

	scheme := runtime.NewScheme()
	_ = storagev1alpha1.AddToScheme(scheme)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(content, vcr, vsc).Build()

	safe, err := VolumeCaptureRequestSafeToDeleteWithHandoff(ctx, cl, client.ObjectKey{Namespace: ns, Name: vcr.GetName()}, content.Name)
	if err != nil {
		t.Fatalf("VolumeCaptureRequestSafeToDeleteWithHandoff: %v", err)
	}
	if safe {
		t.Fatal("VCR must not be safe to delete when VSC ownerRef UID is empty")
	}
}
