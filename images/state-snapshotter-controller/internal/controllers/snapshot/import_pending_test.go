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

package snapshot

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// readyCond returns the Ready condition from a status condition slice (nil if absent).
func readyCond(t *testing.T, conds []metav1.Condition) *metav1.Condition {
	t.Helper()
	return meta.FindStatusCondition(conds, snapshotpkg.ConditionReady)
}

func importSnapshot() *storagev1alpha1.Snapshot {
	return &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "imp", Namespace: "ns", UID: types.UID("imp-uid")},
		Spec:       storagev1alpha1.SnapshotSpec{Mode: storagev1alpha1.SnapshotModeImport},
	}
}

func TestReconcileImportPending_SetsPendingAndRequeues(t *testing.T) {
	ctx := context.Background()
	scheme := testVolumeCaptureScheme(t)
	snap := importSnapshot()
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(snap).
		WithStatusSubresource(&storagev1alpha1.Snapshot{}).Build()
	r := &SnapshotReconciler{Client: cl, APIReader: cl}

	res, err := r.reconcileImportPending(ctx, snap)
	if err != nil {
		t.Fatalf("reconcileImportPending: %v", err)
	}
	if res.RequeueAfter != importPendingRequeueInterval {
		t.Fatalf("want RequeueAfter=%v, got %#v", importPendingRequeueInterval, res)
	}

	got := &storagev1alpha1.Snapshot{}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "imp"}, got); err != nil {
		t.Fatal(err)
	}
	cond := readyCond(t, got.Status.Conditions)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != snapshotpkg.ReasonImportPending {
		t.Fatalf("want Ready=False/ImportPending, got %#v", cond)
	}
	// Import mode must NOT trigger any capture artifacts. The root MCR name is core-internal (no longer a
	// status field); capture materialization would show up as a bound content or a captureState block.
	if got.Status.BoundSnapshotContentName != "" || got.Status.CaptureState != nil {
		t.Fatalf("import snapshot must not capture: bound=%q captureState=%#v", got.Status.BoundSnapshotContentName, got.Status.CaptureState)
	}
}

func TestReconcileImportPending_Idempotent(t *testing.T) {
	ctx := context.Background()
	scheme := testVolumeCaptureScheme(t)
	snap := importSnapshot()
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(snap).
		WithStatusSubresource(&storagev1alpha1.Snapshot{}).Build()
	r := &SnapshotReconciler{Client: cl, APIReader: cl}

	if _, err := r.reconcileImportPending(ctx, snap); err != nil {
		t.Fatalf("first: %v", err)
	}
	after := &storagev1alpha1.Snapshot{}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "imp"}, after); err != nil {
		t.Fatal(err)
	}
	rv := after.ResourceVersion

	// Second call with the already-patched object must not write again (stable ResourceVersion).
	if _, err := r.reconcileImportPending(ctx, after); err != nil {
		t.Fatalf("second: %v", err)
	}
	again := &storagev1alpha1.Snapshot{}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "imp"}, again); err != nil {
		t.Fatal(err)
	}
	if again.ResourceVersion != rv {
		t.Fatalf("idempotent reconcile must not re-write status (rv %s -> %s)", rv, again.ResourceVersion)
	}
}
