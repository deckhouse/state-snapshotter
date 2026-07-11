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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
)

// deletingNodeWithVSC seeds a common SnapshotContent that is being torn down (deletionTimestamp set, held by
// parent-protect) with a single VolumeSnapshotContent data ref, plus the bound VSC (Retain + bound-protection
// + our artifact-protect finalizer, as pinned during capture). It returns the built controller and client.
// The finalizers keep both objects observable after the fake client's finalizer-driven deletion.
func deletingNodeWithVSC(t *testing.T, extraInterceptors interceptor.Funcs) (*SnapshotContentController, client.WithWatch) {
	t.Helper()
	ctx := context.Background()
	scheme := harnessTestScheme(t)
	gvk := unifiedbootstrap.CommonSnapshotContentGVK()

	content := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "node",
			Finalizers: []string{snapshot.FinalizerParentProtect},
		},
	}
	vsc := vscWithDeletionPolicy("vsc-x", volumeSnapshotContentRetainPolicy)
	vsc.SetFinalizers([]string{vscBoundProtectionFinalizer, snapshot.FinalizerArtifactProtect})

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(content, vsc).
		WithStatusSubresource(&storagev1alpha1.SnapshotContent{}).
		WithInterceptorFuncs(extraInterceptors).Build()

	// Publish the single VSC data ref on the status subresource.
	seeded := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: "node"}, seeded); err != nil {
		t.Fatalf("get seeded content: %v", err)
	}
	seeded.Status.Data = &storagev1alpha1.SnapshotDataBinding{
		Artifact: storagev1alpha1.SnapshotDataArtifactRef{
			APIVersion: "snapshot.storage.k8s.io/v1",
			Kind:       "VolumeSnapshotContent",
			Name:       "vsc-x",
		},
	}
	if err := cl.Status().Update(ctx, seeded); err != nil {
		t.Fatalf("seed status.data: %v", err)
	}

	// Move the node into teardown (deletionTimestamp); the parent-protect finalizer keeps it present.
	if err := cl.Delete(ctx, seeded); err != nil {
		t.Fatalf("delete node to set deletionTimestamp: %v", err)
	}

	r := &SnapshotContentController{
		Client:      cl,
		APIReader:   cl,
		Scheme:      scheme,
		GVKRegistry: snapshot.NewGVKRegistry(),
		clusterGVKs: []schema.GroupVersionKind{gvk},
	}
	return r, cl
}

// A node in teardown reclaims its OWN VSC (flip Retain->Delete + stamp being-deleted) and then removes only
// its OWN parent-protect finalizer.
func TestReconcileDeletingNodeReclaimsOwnVSCAndRemovesOwnFinalizer(t *testing.T) {
	ctx := context.Background()
	r, cl := deletingNodeWithVSC(t, interceptor.Funcs{})

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: "node"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// The node's own parent-protect finalizer is gone (the node is then removed by the finalizer mechanism).
	node := &unstructured.Unstructured{}
	node.SetGroupVersionKind(unifiedbootstrap.CommonSnapshotContentGVK())
	if err := cl.Get(ctx, client.ObjectKey{Name: "node"}, node); err != nil {
		if !apierrors.IsNotFound(err) {
			t.Fatalf("get node: %v", err)
		}
	} else if snapshot.HasFinalizer(node, snapshot.FinalizerParentProtect) {
		t.Fatalf("node parent-protect finalizer should be removed after its own reclaim: %v", node.GetFinalizers())
	}

	// The node's own VSC was reclaimed: flipped to Delete and stamped being-deleted.
	vsc := &unstructured.Unstructured{}
	vsc.SetGroupVersionKind(volumeSnapshotContentGVK())
	if err := cl.Get(ctx, client.ObjectKey{Name: "vsc-x"}, vsc); err != nil {
		t.Fatalf("get vsc: %v", err)
	}
	if policy, _, _ := unstructured.NestedString(vsc.Object, "spec", "deletionPolicy"); policy != volumeSnapshotContentDeletePolicy {
		t.Fatalf("vsc deletionPolicy = %q, want Delete", policy)
	}
	if v := vsc.GetAnnotations()[volumeSnapshotBeingDeletedAnnotation]; v != volumeSnapshotBeingDeletedValue {
		t.Fatalf("vsc being-deleted annotation = %q, want %q", v, volumeSnapshotBeingDeletedValue)
	}
	// Our artifact-protect finalizer is gone (only the sidecar's bound-protection remains), so the VSC
	// deletion completes as soon as the sidecar finishes the physical reclaim.
	if snapshot.HasFinalizer(vsc, snapshot.FinalizerArtifactProtect) {
		t.Fatalf("vsc artifact-protect finalizer should be removed during teardown: %v", vsc.GetFinalizers())
	}
}

// If removing our artifact-protect finalizer from the node's own VSC fails, the node KEEPS parent-protect
// and the reconcile errors so it is retried. This content is the LAST writer able to strip artifact-protect:
// swallowing the failure (the pre-fix behavior) let the content proceed to its own finalizer removal and be
// GC'd, leaving the VSC wedged forever as a tombstone — deletionTimestamp set, our finalizer never removed.
func TestReconcileDeletingNodeKeepsFinalizerWhenArtifactFinalizerRemovalFails(t *testing.T) {
	ctx := context.Background()
	// Fail metadata updates on the VSC: the deletion path touches the VSC only via Patch (reclaim flip) and
	// Delete, so an Update interceptor targets exactly the artifact-protect finalizer removal.
	r, cl := deletingNodeWithVSC(t, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if obj.GetObjectKind().GroupVersionKind().Kind == kindVolumeSnapshotContent {
				return apierrors.NewServiceUnavailable("simulated VSC update failure")
			}
			return c.Update(ctx, obj, opts...)
		},
	})

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: "node"}}); err == nil {
		t.Fatalf("expected reconcile to return an error when the VSC artifact-protect removal fails")
	}

	node := &unstructured.Unstructured{}
	node.SetGroupVersionKind(unifiedbootstrap.CommonSnapshotContentGVK())
	if err := cl.Get(ctx, client.ObjectKey{Name: "node"}, node); err != nil {
		t.Fatalf("get node: %v", err)
	}
	if !snapshot.HasFinalizer(node, snapshot.FinalizerParentProtect) {
		t.Fatalf("node must RETAIN parent-protect when the artifact finalizer removal fails (retryable): %v", node.GetFinalizers())
	}

	// The VSC still carries our finalizer — the retried teardown remains the writer that will remove it.
	vsc := &unstructured.Unstructured{}
	vsc.SetGroupVersionKind(volumeSnapshotContentGVK())
	if err := cl.Get(ctx, client.ObjectKey{Name: "vsc-x"}, vsc); err != nil {
		t.Fatalf("get vsc: %v", err)
	}
	if !snapshot.HasFinalizer(vsc, snapshot.FinalizerArtifactProtect) {
		t.Fatalf("vsc must still carry artifact-protect (removal failed): %v", vsc.GetFinalizers())
	}
}

// If the node's own VSC reclaim fails, the node KEEPS its parent-protect finalizer and the reconcile returns
// an error so it is retried — the physical snapshot is never orphaned.
func TestReconcileDeletingNodeKeepsFinalizerWhenReclaimFails(t *testing.T) {
	ctx := context.Background()
	// Fail the VSC flip patch so reclaim cannot complete.
	r, cl := deletingNodeWithVSC(t, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if obj.GetObjectKind().GroupVersionKind().Kind == kindVolumeSnapshotContent {
				return apierrors.NewServiceUnavailable("simulated VSC patch failure")
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: "node"}}); err == nil {
		t.Fatalf("expected reconcile to return an error when VSC reclaim fails")
	}

	node := &unstructured.Unstructured{}
	node.SetGroupVersionKind(unifiedbootstrap.CommonSnapshotContentGVK())
	if err := cl.Get(ctx, client.ObjectKey{Name: "node"}, node); err != nil {
		t.Fatalf("get node: %v", err)
	}
	if !snapshot.HasFinalizer(node, snapshot.FinalizerParentProtect) {
		t.Fatalf("node must RETAIN parent-protect when its reclaim fails (retryable): %v", node.GetFinalizers())
	}
}
