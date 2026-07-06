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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

func subtreeMirrorSnapshot(name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot"))
	obj.SetNamespace("ns1")
	obj.SetName(name)
	return obj
}

func subtreeMirrorContent(persisted bool, set bool) *unstructured.Unstructured {
	c := &unstructured.Unstructured{}
	c.SetGroupVersionKind(storagev1alpha1.SchemeGroupVersion.WithKind("SnapshotContent"))
	c.SetName("content")
	if set {
		_ = unstructured.SetNestedField(c.Object, persisted, "status", "subtreeManifestsPersisted")
	}
	return c
}

func newSubtreeMirrorController(t *testing.T, snapObj *unstructured.Unstructured) (*GenericSnapshotBinderController, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(snapObj).
		WithStatusSubresource(snapObj).
		Build()
	return &GenericSnapshotBinderController{Client: cl, APIReader: cl, Scheme: scheme}, cl
}

func subtreeMirrorLatch(t *testing.T, ctx context.Context, cl client.Client, name string) (bool, bool) {
	t.Helper()
	fresh := &unstructured.Unstructured{}
	fresh.SetGroupVersionKind(storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot"))
	if err := cl.Get(ctx, client.ObjectKey{Namespace: "ns1", Name: name}, fresh); err != nil {
		t.Fatalf("get snapshot: %v", err)
	}
	val, found, err := unstructured.NestedBool(fresh.Object, "status", "captureState", "commonController", "subtreeManifestsPersisted")
	if err != nil {
		t.Fatalf("read latch: %v", err)
	}
	return val, found
}

// TestMirrorSubtreeManifestsPersisted_MirrorsTrue verifies a persisted content latch is mirrored onto the
// snapshot's core-owned commonController.subtreeManifestsPersisted.
func TestMirrorSubtreeManifestsPersisted_MirrorsTrue(t *testing.T) {
	ctx := context.Background()
	snapObj := subtreeMirrorSnapshot("snap-a")
	r, cl := newSubtreeMirrorController(t, snapObj)

	if err := r.mirrorSubtreeManifestsPersistedFromContent(ctx, snapObj, subtreeMirrorContent(true, true)); err != nil {
		t.Fatalf("mirror: %v", err)
	}
	got, found := subtreeMirrorLatch(t, ctx, cl, "snap-a")
	if !found || !got {
		t.Fatalf("want commonController.subtreeManifestsPersisted=true, got found=%v value=%v", found, got)
	}
}

// TestMirrorSubtreeManifestsPersisted_FalseAndAbsentAreNoOps verifies that a content latch that is false
// or absent leaves the snapshot mirror unset (the gate reads absence as "not persisted yet").
func TestMirrorSubtreeManifestsPersisted_FalseAndAbsentAreNoOps(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name     string
		content  *unstructured.Unstructured
		snapName string
	}{
		{"false", subtreeMirrorContent(false, true), "snap-false"},
		{"absent", subtreeMirrorContent(false, false), "snap-absent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapObj := subtreeMirrorSnapshot(tc.snapName)
			r, cl := newSubtreeMirrorController(t, snapObj)
			if err := r.mirrorSubtreeManifestsPersistedFromContent(ctx, snapObj, tc.content); err != nil {
				t.Fatalf("mirror: %v", err)
			}
			if _, found := subtreeMirrorLatch(t, ctx, cl, tc.snapName); found {
				t.Fatalf("mirror must stay unset for a %s content latch", tc.name)
			}
		})
	}
}

// TestMirrorSubtreeManifestsPersisted_Monotonic verifies the mirror is monotonic: once set true, a later
// content latch reading false never flips it back (idempotent + true-only).
func TestMirrorSubtreeManifestsPersisted_Monotonic(t *testing.T) {
	ctx := context.Background()
	snapObj := subtreeMirrorSnapshot("snap-mono")
	r, cl := newSubtreeMirrorController(t, snapObj)

	if err := r.mirrorSubtreeManifestsPersistedFromContent(ctx, snapObj, subtreeMirrorContent(true, true)); err != nil {
		t.Fatalf("mirror true: %v", err)
	}
	// Re-read the live object so the in-memory copy carries the latch (as the reconcile would).
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot"))
	if err := cl.Get(ctx, client.ObjectKey{Namespace: "ns1", Name: "snap-mono"}, live); err != nil {
		t.Fatalf("get live: %v", err)
	}
	if err := r.mirrorSubtreeManifestsPersistedFromContent(ctx, live, subtreeMirrorContent(false, true)); err != nil {
		t.Fatalf("mirror false: %v", err)
	}
	got, found := subtreeMirrorLatch(t, ctx, cl, "snap-mono")
	if !found || !got {
		t.Fatalf("monotonic latch must remain true after a false content read, got found=%v value=%v", found, got)
	}
}
