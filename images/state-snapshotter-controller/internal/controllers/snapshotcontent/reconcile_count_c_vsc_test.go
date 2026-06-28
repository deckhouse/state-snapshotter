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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

var vscGVK = schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: kindVolumeSnapshotContent}

// Swap C (data_readiness.go checkVolumeSnapshotContentReadiness): the VolumeSnapshotContent readiness read
// (deletionTimestamp + status.readyToUse) now uses the cached r.Client instead of the uncached APIReader.
// The VSC is already under the artifact wake-up watch and is read via the cache elsewhere
// (datarefs_publish.go), so the swap forces no new watch. These subtests prove the read moved to the cache
// and that the fail-closed classification is preserved: a NotFound stays ArtifactMissing (no error), and a
// non-NotFound transient/CRD-absent error still propagates as a reconcile error (requeue).
func TestVolumeSnapshotContentReadFromCache(t *testing.T) {
	t.Parallel()

	t.Run("ready VSC read via cache", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		scheme := harnessTestScheme(t)
		vsc := volumeSnapshotContentObject("vsc-ok", true)
		base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vsc).Build()

		counters := newSplitCounters()
		cache, apiReader := counters.splitClients(base, nil)
		r := &SnapshotContentController{Client: cache, APIReader: apiReader, Scheme: scheme, GVKRegistry: snapshot.NewGVKRegistry()}

		ready, reason, _, err := r.checkVolumeSnapshotContentReadiness(ctx, "vsc-ok")
		if err != nil {
			t.Fatalf("check readiness: %v", err)
		}
		if !ready {
			t.Fatalf("ready=%v reason=%s, want ready=true for readyToUse VSC", ready, reason)
		}
		if n := counters.getCount(roleClient, vscGVK); n < 1 {
			t.Fatalf("VSC GET via Client = %d, want >=1 after swap", n)
		}
		if n := counters.getCount(roleAPIReader, vscGVK); n != 0 {
			t.Fatalf("VSC GET via APIReader = %d, want 0 after swap", n)
		}
	})

	t.Run("NotFound stays ArtifactMissing (fail-closed, no error)", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		scheme := harnessTestScheme(t)
		base := fake.NewClientBuilder().WithScheme(scheme).Build() // vsc-gone not created

		counters := newSplitCounters()
		cache, apiReader := counters.splitClients(base, nil)
		r := &SnapshotContentController{Client: cache, APIReader: apiReader, Scheme: scheme, GVKRegistry: snapshot.NewGVKRegistry()}

		ready, reason, _, err := r.checkVolumeSnapshotContentReadiness(ctx, "vsc-gone")
		if err != nil {
			t.Fatalf("NotFound must not be a reconcile error, got %v", err)
		}
		if ready || reason != snapshot.ReasonArtifactMissing {
			t.Fatalf("got ready=%v reason=%s, want false/ArtifactMissing", ready, reason)
		}
		if n := counters.getCount(roleClient, vscGVK); n < 1 {
			t.Fatalf("VSC GET via Client = %d, want >=1 after swap", n)
		}
		if n := counters.getCount(roleAPIReader, vscGVK); n != 0 {
			t.Fatalf("VSC GET via APIReader = %d, want 0 after swap", n)
		}
	})

	t.Run("non-NotFound error propagates as requeue (CRD/informer absent model)", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		scheme := harnessTestScheme(t)
		// Model a cached Get against a type whose informer/CRD is not available: a non-NotFound error.
		base := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if u, ok := obj.(*unstructured.Unstructured); ok && u.GroupVersionKind().Kind == kindVolumeSnapshotContent {
					return apierrors.NewServiceUnavailable("VolumeSnapshotContent informer not available")
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()

		counters := newSplitCounters()
		cache, apiReader := counters.splitClients(base, nil)
		r := &SnapshotContentController{Client: cache, APIReader: apiReader, Scheme: scheme, GVKRegistry: snapshot.NewGVKRegistry()}

		ready, _, _, err := r.checkVolumeSnapshotContentReadiness(ctx, "vsc-any")
		if err == nil {
			t.Fatalf("a non-NotFound cached read error must propagate as a reconcile error (requeue), got nil")
		}
		if ready {
			t.Fatalf("ready must be false on a read error")
		}
		if n := counters.getCount(roleClient, vscGVK); n < 1 {
			t.Fatalf("VSC GET via Client = %d, want >=1 after swap", n)
		}
		if n := counters.getCount(roleAPIReader, vscGVK); n != 0 {
			t.Fatalf("VSC GET via APIReader = %d, want 0 after swap", n)
		}
	})
}
