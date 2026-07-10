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
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
)

// A live SnapshotContent whose bound Snapshot is already gone (status.boundSnapshotDeleted=true) must still
// carry parent-protect: it is a durable node released only by its own teardown. The pre-fix code stripped
// the finalizer on bound-Snapshot deletion, so a content left finalizer-less must be self-healed by the live
// reconcile. Only AFTER the finalizer is confirmed does the bound-deleted early return skip status
// aggregation against the gone owner (no requeue, no owner read).
func TestReconcileSelfHealsMissingFinalizerWhenBoundSnapshotDeleted(t *testing.T) {
	ctx := context.Background()
	scheme := harnessTestScheme(t)
	gvk := unifiedbootstrap.CommonSnapshotContentGVK()

	content := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "healme"}, // no finalizer (left stripped by the old code)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(content).
		WithStatusSubresource(&storagev1alpha1.SnapshotContent{}).Build()

	// Latch the recycle-bin flag on the status subresource so the read in Reconcile observes it.
	seeded := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: "healme"}, seeded); err != nil {
		t.Fatalf("get seeded content: %v", err)
	}
	seeded.Status.BoundSnapshotDeleted = true
	if err := cl.Status().Update(ctx, seeded); err != nil {
		t.Fatalf("seed boundSnapshotDeleted: %v", err)
	}

	r := &SnapshotContentController{
		Client:      cl,
		APIReader:   cl,
		Scheme:      scheme,
		GVKRegistry: snapshot.NewGVKRegistry(),
		clusterGVKs: []schema.GroupVersionKind{gvk},
	}

	// Pass 1: the finalizer is self-healed even though boundSnapshotDeleted=true, and the pass requeues.
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: "healme"}})
	if err != nil {
		t.Fatalf("reconcile (heal): %v", err)
	}
	if !res.Requeue {
		t.Fatalf("expected Requeue after self-healing the finalizer, got %+v", res)
	}
	fresh := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: "healme"}, fresh); err != nil {
		t.Fatalf("get content: %v", err)
	}
	if !snapshot.HasFinalizer(fresh, snapshot.FinalizerParentProtect) {
		t.Fatalf("parent-protect finalizer was not self-healed: %v", fresh.Finalizers)
	}

	// Pass 2: finalizer present + boundSnapshotDeleted=true -> early return, no stale-owner aggregation, no requeue.
	res, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: "healme"}})
	if err != nil {
		t.Fatalf("reconcile (early-return): %v", err)
	}
	if res.Requeue || res.RequeueAfter != 0 {
		t.Fatalf("expected no requeue on the bound-deleted early return, got %+v", res)
	}
}
