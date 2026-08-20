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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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

// contentControllerRef is the ownerReference the handoff installs, i.e. what an already handed-off VSC
// carries.
func contentControllerRef(content *storagev1alpha1.SnapshotContent) metav1.OwnerReference {
	ctrlTrue := true
	return metav1.OwnerReference{
		APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
		Kind:       "SnapshotContent",
		Name:       content.Name,
		UID:        content.UID,
		Controller: &ctrlTrue,
	}
}

func getVSC(ctx context.Context, t *testing.T, cl client.Client, name string) *unstructured.Unstructured {
	t.Helper()
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotContent"})
	if err := cl.Get(ctx, client.ObjectKey{Name: name}, got); err != nil {
		t.Fatalf("get VSC %s: %v", name, err)
	}
	return got
}

// The durable-artifact handoff MUST force the bound VSC to deletionPolicy=Retain (so a class-default
// Delete policy cannot drop the artifact when the per-run VolumeSnapshot/VCR is GC'd) and re-parent the
// ownerReference to the SnapshotContent. This is the regression guard for the domain VCR path, which
// previously left the VSC at Delete.
//
// Both land in ONE patch, which the test asserts by counting patches. The VSC is cluster-scoped, so the
// handoff must leave it WITHOUT the delete-protection marker: its lifecycle is held by the owning
// SnapshotContent, not by admission.
func TestEnsureVolumeSnapshotContentsOwnedByContent_ForcesRetainAndOwner(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}

	vsc := vscWithDeletionPolicy("vsc-domain", "Delete")
	var patchCalls int
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vsc).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			patchCalls++
			return c.Patch(ctx, obj, patch, opts...)
		},
	}).Build()

	content := &storagev1alpha1.SnapshotContent{}
	content.SetName("demodiskc-1")
	content.SetUID(types.UID("content-uid-1"))

	bindings := []storagev1alpha1.SnapshotDataBinding{{
		SourceRef:   storagev1alpha1.SnapshotSubjectRef{UID: "pvc-uid-1"},
		ArtifactRef: storagev1alpha1.SnapshotDataArtifactRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: "vsc-domain"},
	}}

	if err := EnsureVolumeSnapshotContentsOwnedByContent(ctx, cl, content, bindings); err != nil {
		t.Fatalf("handoff: %v", err)
	}
	if patchCalls != 1 {
		t.Fatalf("handoff must deliver owner and Retain in one patch; patchCalls=%d", patchCalls)
	}

	got := getVSC(ctx, t, cl, "vsc-domain")

	policy, _, _ := unstructured.NestedString(got.Object, "spec", "deletionPolicy")
	if policy != "Retain" {
		t.Fatalf("deletionPolicy = %q, want Retain", policy)
	}
	owners := got.GetOwnerReferences()
	if len(owners) != 1 || owners[0].Kind != "SnapshotContent" || owners[0].Name != "demodiskc-1" ||
		owners[0].Controller == nil || !*owners[0].Controller {
		t.Fatalf("ownerReferences = %+v, want single controller SnapshotContent/demodiskc-1", owners)
	}
	if storagev1alpha1.IsDeleteProtected(got) {
		t.Fatalf("handed-off VSC must stay unmarked; labels = %v", got.GetLabels())
	}
}

// An artifact that OWNS the content is left completely alone: it belongs to a snapshot this module did not
// create, and the content merely wraps it. Two things would go wrong otherwise, and the test pins both —
// the user's Delete policy would be silently changed into Retain, and the two cluster-scoped objects would
// end up owning each other, a cycle garbage collection never resolves.
func TestEnsureVolumeSnapshotContentsOwnedByContent_LeavesTheArtifactThatOwnsTheContent(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}

	vsc := vscWithDeletionPolicy("vsc-user", "Delete")
	vsc.SetUID(types.UID("vsc-user-uid"))
	var patchCalls int
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vsc).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			patchCalls++
			return c.Patch(ctx, obj, patch, opts...)
		},
	}).Build()

	ctrlTrue := true
	content := &storagev1alpha1.SnapshotContent{}
	content.SetName("user-vs-content")
	content.SetUID(types.UID("content-uid-user"))
	content.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: "snapshot.storage.k8s.io/v1",
		Kind:       "VolumeSnapshotContent",
		Name:       "vsc-user",
		UID:        types.UID("vsc-user-uid"),
		Controller: &ctrlTrue,
	}})

	bindings := []storagev1alpha1.SnapshotDataBinding{{
		SourceRef:   storagev1alpha1.SnapshotSubjectRef{UID: "pvc-uid-user"},
		ArtifactRef: storagev1alpha1.SnapshotDataArtifactRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: "vsc-user"},
	}}

	if err := EnsureVolumeSnapshotContentsOwnedByContent(ctx, cl, content, bindings); err != nil {
		t.Fatalf("handoff: %v", err)
	}
	if patchCalls != 0 {
		t.Fatalf("the artifact that owns the content must not be written; patchCalls=%d", patchCalls)
	}

	got := getVSC(ctx, t, cl, "vsc-user")
	if policy, _, _ := unstructured.NestedString(got.Object, "spec", "deletionPolicy"); policy != "Delete" {
		t.Fatalf("deletionPolicy = %q, want the user's own Delete", policy)
	}
	if owners := got.GetOwnerReferences(); len(owners) != 0 {
		t.Fatalf("ownerReferences = %+v, want none: owning it back would close a cycle", owners)
	}
}

// Idempotency: a VSC that already carries everything the handoff delivers — Retain and the content owner —
// is not written at all.
func TestEnsureVolumeSnapshotContentsOwnedByContent_StableWhenAlreadyHandedOff(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}

	content := &storagev1alpha1.SnapshotContent{}
	content.SetName("demodiskc-1")
	content.SetUID(types.UID("content-uid-1"))

	vsc := vscWithDeletionPolicy("vsc-domain", "Retain")
	vsc.SetOwnerReferences([]metav1.OwnerReference{contentControllerRef(content)})
	var patchCalls int
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vsc).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			patchCalls++
			return c.Patch(ctx, obj, patch, opts...)
		},
	}).Build()

	bindings := []storagev1alpha1.SnapshotDataBinding{{
		SourceRef:   storagev1alpha1.SnapshotSubjectRef{UID: "pvc-uid-1"},
		ArtifactRef: storagev1alpha1.SnapshotDataArtifactRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: "vsc-domain"},
	}}
	if err := EnsureVolumeSnapshotContentsOwnedByContent(ctx, cl, content, bindings); err != nil {
		t.Fatalf("handoff: %v", err)
	}
	if patchCalls != 0 {
		t.Fatalf("a conforming VSC must not be written; patchCalls=%d", patchCalls)
	}

	got := getVSC(ctx, t, cl, "vsc-domain")
	if policy, _, _ := unstructured.NestedString(got.Object, "spec", "deletionPolicy"); policy != "Retain" {
		t.Fatalf("deletionPolicy = %q, want Retain", policy)
	}
	if owners := got.GetOwnerReferences(); len(owners) != 1 {
		t.Fatalf("ownerReferences = %+v, want exactly one", owners)
	}
}

// A VSC that is being deleted MUST NOT be patched, even though its ownerRef is wrong, its deletionPolicy is
// not Retain and it carries no marker: the handoff must skip it (data readiness reports ArtifactMissing).
// Protecting an object that is already going away would only race its finalizers.
func TestEnsureVolumeSnapshotContentsOwnedByContent_SkipsDeletingVSC(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}

	vsc := vscWithDeletionPolicy("vsc-deleting", "Delete")
	// Deleting: a finalizer is required by the fake client to keep the object present with a
	// non-zero deletionTimestamp.
	vsc.SetFinalizers([]string{"keep/for-test"})
	now := metav1.NewTime(time.Now())
	vsc.SetDeletionTimestamp(&now)

	var patchCalls int
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vsc).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			patchCalls++
			return c.Patch(ctx, obj, patch, opts...)
		},
	}).Build()

	content := &storagev1alpha1.SnapshotContent{}
	content.SetName("demodiskc-1")
	content.SetUID(types.UID("content-uid-1"))
	bindings := []storagev1alpha1.SnapshotDataBinding{{
		SourceRef:   storagev1alpha1.SnapshotSubjectRef{UID: "pvc-uid-1"},
		ArtifactRef: storagev1alpha1.SnapshotDataArtifactRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: "vsc-deleting"},
	}}

	if err := EnsureVolumeSnapshotContentsOwnedByContent(ctx, cl, content, bindings); err != nil {
		t.Fatalf("handoff on deleting VSC must not error: %v", err)
	}
	if patchCalls != 0 {
		t.Fatalf("deleting VSC must not be patched; patchCalls=%d", patchCalls)
	}

	got := getVSC(ctx, t, cl, "vsc-deleting")
	if policy, _, _ := unstructured.NestedString(got.Object, "spec", "deletionPolicy"); policy != "Delete" {
		t.Fatalf("deleting VSC deletionPolicy = %q, want unchanged Delete", policy)
	}
	if storagev1alpha1.IsDeleteProtected(got) {
		t.Fatalf("deleting VSC must not be marked delete-protected; labels=%v", got.GetLabels())
	}
	for _, ref := range got.GetOwnerReferences() {
		if ref.Kind == "SnapshotContent" {
			t.Fatalf("deleting VSC must not get a SnapshotContent ownerRef; refs=%+v", got.GetOwnerReferences())
		}
	}
}

// dataRefs[] may reference artifact kinds other than VolumeSnapshotContent; the handoff MUST skip them
// and never attempt to GET/Patch them as a VSC.
func TestEnsureVolumeSnapshotContentsOwnedByContent_IgnoresNonVSCArtifacts(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}

	var getCalls, patchCalls int
	cl := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			getCalls++
			return c.Get(ctx, key, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			patchCalls++
			return c.Patch(ctx, obj, patch, opts...)
		},
	}).Build()

	content := &storagev1alpha1.SnapshotContent{}
	content.SetName("demodiskc-1")
	content.SetUID(types.UID("content-uid-1"))
	bindings := []storagev1alpha1.SnapshotDataBinding{
		{SourceRef: storagev1alpha1.SnapshotSubjectRef{UID: "u1"}, ArtifactRef: storagev1alpha1.SnapshotDataArtifactRef{APIVersion: "backup.example.io/v1", Kind: "BackupSnapshot", Name: "b-1"}},
		{SourceRef: storagev1alpha1.SnapshotSubjectRef{UID: "u2"}, ArtifactRef: storagev1alpha1.SnapshotDataArtifactRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: ""}},
	}

	if err := EnsureVolumeSnapshotContentsOwnedByContent(ctx, cl, content, bindings); err != nil {
		t.Fatalf("handoff with non-VSC artifacts must not error: %v", err)
	}
	if getCalls != 0 || patchCalls != 0 {
		t.Fatalf("non-VSC (and empty-name) artifacts must be skipped without API calls; getCalls=%d patchCalls=%d", getCalls, patchCalls)
	}
}
