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

package snapshotcontent

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
)

func vscWithDeletionPolicy(name, policy string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotContent"})
	obj.SetName(name)
	if policy != "" {
		_ = unstructured.SetNestedField(obj.Object, policy, "spec", "deletionPolicy")
	}
	return obj
}

// The durable-artifact handoff MUST force the bound VSC to deletionPolicy=Retain (so a class-default
// Delete policy cannot drop the artifact when the per-run VolumeSnapshot/VCR is GC'd) AND re-parent the
// ownerReference to the SnapshotContent. This is the regression guard for the domain VCR path, which
// previously left the VSC at Delete.
func TestEnsureVolumeSnapshotContentsOwnedByContent_ForcesRetainAndOwner(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}

	vsc := vscWithDeletionPolicy("vsc-domain", "Delete")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vsc).Build()

	content := &storagev1alpha1.SnapshotContent{}
	content.SetName("demodiskc-1")
	content.SetUID(types.UID("content-uid-1"))

	bindings := []storagev1alpha1.SnapshotDataBinding{{
		TargetUID: "pvc-uid-1",
		Artifact:  storagev1alpha1.SnapshotDataArtifactRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: "vsc-domain"},
	}}

	if err := EnsureVolumeSnapshotContentsOwnedByContent(ctx, cl, content, bindings); err != nil {
		t.Fatalf("handoff: %v", err)
	}

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotContent"})
	if err := cl.Get(ctx, client.ObjectKey{Name: "vsc-domain"}, got); err != nil {
		t.Fatalf("get VSC: %v", err)
	}

	policy, _, _ := unstructured.NestedString(got.Object, "spec", "deletionPolicy")
	if policy != "Retain" {
		t.Fatalf("deletionPolicy = %q, want Retain", policy)
	}
	owners := got.GetOwnerReferences()
	if len(owners) != 1 || owners[0].Kind != "SnapshotContent" || owners[0].Name != "demodiskc-1" ||
		owners[0].Controller == nil || !*owners[0].Controller {
		t.Fatalf("ownerReferences = %+v, want single controller SnapshotContent/demodiskc-1", owners)
	}
}

// Idempotency: a VSC already owned by the content and already Retain stays Retain and singly-owned.
func TestEnsureVolumeSnapshotContentsOwnedByContent_StableWhenAlreadyRetainAndOwned(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}

	content := &storagev1alpha1.SnapshotContent{}
	content.SetName("demodiskc-1")
	content.SetUID(types.UID("content-uid-1"))

	ctrlTrue := true
	vsc := vscWithDeletionPolicy("vsc-domain", "Retain")
	vsc.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
		Kind:       "SnapshotContent",
		Name:       "demodiskc-1",
		UID:        types.UID("content-uid-1"),
		Controller: &ctrlTrue,
	}})
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vsc).Build()

	bindings := []storagev1alpha1.SnapshotDataBinding{{
		TargetUID: "pvc-uid-1",
		Artifact:  storagev1alpha1.SnapshotDataArtifactRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: "vsc-domain"},
	}}
	if err := EnsureVolumeSnapshotContentsOwnedByContent(ctx, cl, content, bindings); err != nil {
		t.Fatalf("handoff: %v", err)
	}

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotContent"})
	if err := cl.Get(ctx, client.ObjectKey{Name: "vsc-domain"}, got); err != nil {
		t.Fatalf("get VSC: %v", err)
	}
	if policy, _, _ := unstructured.NestedString(got.Object, "spec", "deletionPolicy"); policy != "Retain" {
		t.Fatalf("deletionPolicy = %q, want Retain", policy)
	}
	if owners := got.GetOwnerReferences(); len(owners) != 1 {
		t.Fatalf("ownerReferences = %+v, want exactly one", owners)
	}
}
