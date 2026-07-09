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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
)

// Swap E: reconcileCommonSnapshotContentStatus writes conditions via a condition-only MergeFrom patch
// instead of a full Status().Update. This test pins the two guarantees of that change:
//
//  1. CLOBBER-SAFETY: a sibling status field written by the snapshot reconciler (boundSnapshotDeleted) AFTER
//     the aggregator's read survives the aggregator's write. A full Status().Update would either clobber the
//     field (if its resourceVersion still matched) or 409 on the bumped resourceVersion; the condition-only
//     MergeFrom touches only status.conditions and sends no resourceVersion.
//  2. NO-409-DOWNGRADE (parity): because no resourceVersion is sent, the concurrent sibling write does not
//     turn the aggregator pass into a conflict -> the pass still reports the computed Ready (the old
//     Status().Update path would have hit IsConflict and returned ready=false = an extra requeue).
//
// It also asserts routing: the write goes through Status().Patch, never Status().Update.
func TestConditionsMergeFromPatchPreservesWriterStatusFields(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	child := readyArchivedChildContent("child-1")
	parent := commonContentWithStatus("parent", "mcp-ok", "child-1")
	base := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(parent, child, mcp).
		WithStatusSubresource(&storagev1alpha1.SnapshotContent{}).
		Build()

	// Aggregator reads its working copy of the parent (before the concurrent writer runs).
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(unifiedbootstrap.CommonSnapshotContentGVK())
	if err := base.Get(ctx, client.ObjectKey{Name: "parent"}, obj); err != nil {
		t.Fatalf("aggregator read: %v", err)
	}

	// Concurrent writer (the snapshot reconciler) latches a SIBLING status field and bumps the
	// resourceVersion AFTER the aggregator's read.
	writer := &unstructured.Unstructured{}
	writer.SetGroupVersionKind(unifiedbootstrap.CommonSnapshotContentGVK())
	if err := base.Get(ctx, client.ObjectKey{Name: "parent"}, writer); err != nil {
		t.Fatalf("writer read: %v", err)
	}
	if err := unstructured.SetNestedField(writer.Object, true, "status", "boundSnapshotDeleted"); err != nil {
		t.Fatalf("set writer field: %v", err)
	}
	if err := base.Status().Update(ctx, writer); err != nil {
		t.Fatalf("writer status update: %v", err)
	}

	// Aggregator writes conditions from its now-stale obj via the controller (condition-only MergeFrom).
	counters := newSplitCounters()
	cacheCl, apiReader := counters.splitClients(base, nil)
	r := &SnapshotContentController{Client: cacheCl, APIReader: apiReader, Scheme: scheme, GVKRegistry: snapshot.NewGVKRegistry()}

	ready, err := r.ReconcileCommonSnapshotContentStatus(ctx, obj)
	if err != nil {
		// The old full Status().Update path would surface the stale-resourceVersion 409 here.
		t.Fatalf("aggregator write: %v (condition-only MergeFrom must not conflict on a concurrent sibling write)", err)
	}
	if !ready {
		t.Fatalf("ready=false: a concurrent sibling write must not downgrade the aggregator pass (no 409 requeue)")
	}

	// Routing: the write is a condition-only Patch, not a full Update.
	if counters.statusPatch == 0 {
		t.Fatalf("aggregator did not Status().Patch conditions (statusPatch=0)")
	}
	if counters.statusUpdate != 0 {
		t.Fatalf("aggregator used Status().Update (%d); want condition-only MergeFrom Patch", counters.statusUpdate)
	}

	// The store carries BOTH the writer's latch field AND the aggregator's conditions.
	fresh := &unstructured.Unstructured{}
	fresh.SetGroupVersionKind(unifiedbootstrap.CommonSnapshotContentGVK())
	if err := base.Get(ctx, client.ObjectKey{Name: "parent"}, fresh); err != nil {
		t.Fatalf("post-write read: %v", err)
	}
	boundSnapshotDeleted, _, _ := unstructured.NestedBool(fresh.Object, "status", "boundSnapshotDeleted")
	if !boundSnapshotDeleted {
		t.Fatalf("writer field status.boundSnapshotDeleted clobbered: got %v, want true", boundSnapshotDeleted)
	}
	contentLike, err := snapshot.ExtractSnapshotContentLike(fresh)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if c := snapshot.GetCondition(contentLike, snapshot.ConditionReady); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("aggregator Ready condition missing/not True after MergeFrom: %#v", c)
	}
}

// Swap E parity: with no competing writer, the condition-only MergeFrom still converges to a stable Ready
// with exactly one status write on the first pass and none afterward (no extra reconciles vs the old full
// Status().Update). Guards against the patch firing on a no-op (which would spin the self-requeue loop).
func TestConditionsMergeFromConvergesWithSingleWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, parent, counters := newAggregatorHarness(t, nil)

	res, err := driveToSteady(counters, func() (bool, error) {
		return r.ReconcileCommonSnapshotContentStatus(ctx, parent)
	}, 10)
	if err != nil {
		t.Fatalf("drive to steady: %v", err)
	}
	if res.passes != 2 || res.requeues != 0 {
		t.Fatalf("passes=%d requeues=%d, want 2/0 (ready immediately, stable)", res.passes, res.requeues)
	}
	if res.statusWrites != 1 {
		t.Fatalf("statusWrites=%d, want 1 (one condition patch, then no-op)", res.statusWrites)
	}
	if counters.statusUpdate != 0 {
		t.Fatalf("aggregator used Status().Update (%d); want condition-only MergeFrom Patch", counters.statusUpdate)
	}
	if counters.statusPatch != 1 {
		t.Fatalf("statusPatch=%d, want exactly 1", counters.statusPatch)
	}
}
