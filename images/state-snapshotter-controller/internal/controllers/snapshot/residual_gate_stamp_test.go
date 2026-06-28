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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	deckhousev1alpha1 "github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
)

func residualPhase(content *storagev1alpha1.SnapshotContent) string {
	if content.Status.ResidualVolumeCapture == nil {
		return ""
	}
	return content.Status.ResidualVolumeCapture.Phase
}

// TestReconcileVolumeCapturePublish_ZeroOrphanTargetsStampsResidualComplete: the capture root with no
// orphan PVCs reaches the zero-targets done branch and latches the residual gate Complete.
func TestReconcileVolumeCapturePublish_ZeroOrphanTargetsStampsResidualComplete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testVolumeCaptureScheme(t)
	ns := "default"
	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "c1", UID: types.UID("content-uid")}}
	snap := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: ns, UID: "snap-uid"}}

	// No PVCs in the namespace => zero orphan targets => the wave is done with no orphans.
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(snap, content).
		WithStatusSubresource(&storagev1alpha1.Snapshot{}, &storagev1alpha1.SnapshotContent{}).Build()
	r := &SnapshotReconciler{Client: cl, APIReader: cl}

	if _, err := r.reconcileVolumeCapturePublish(ctx, snap, content, true); err != nil {
		t.Fatalf("publish: %v", err)
	}

	got := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: "c1"}, got); err != nil {
		t.Fatalf("get content: %v", err)
	}
	if residualPhase(got) != storagev1alpha1.ResidualVolumeCapturePhaseComplete {
		t.Fatalf("zero-targets capture must latch residual Complete, got %q (%#v)", residualPhase(got), got.Status.ResidualVolumeCapture)
	}
	if len(got.Status.ResidualVolumeCapture.TargetUIDs) != 0 {
		t.Fatalf("zero-targets must record no orphan UIDs, got %#v", got.Status.ResidualVolumeCapture.TargetUIDs)
	}
}

// TestReconcileOrphanPVCVolumeSnapshotPublish_NotAllReadyDoesNotStamp: while an orphan child volume node
// is not yet ready, the publish requeues and must NOT latch the residual gate (the gate opens only once
// the data is ready).
func TestReconcileOrphanPVCVolumeSnapshotPublish_NotAllReadyDoesNotStamp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testVolumeCaptureScheme(t)
	ns := "default"
	content := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "c1", UID: types.UID("content-uid")}}
	snap := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: ns, UID: "snap-uid"}}

	target := pvcTarget(ns, "pvc-a", "uid-a")
	vsName := orphanPVCVolumeSnapshotName(snap.UID, target)
	// Unbound VolumeSnapshot => orphanPVCVolumeSnapshotBinding returns not-ready (not failed).
	unboundVS := unboundVolumeSnapshot(ns, vsName, "pvc-a", testVSClassName, snap)

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(snap, content, unboundVS).
		WithStatusSubresource(&storagev1alpha1.Snapshot{}, &storagev1alpha1.SnapshotContent{}).Build()
	r := &SnapshotReconciler{Client: cl, APIReader: cl}

	res, err := r.reconcileOrphanPVCVolumeSnapshotPublish(ctx, snap, content, []vcpkg.Target{target}, true)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if !res.Requeue && res.RequeueAfter == 0 {
		t.Fatalf("a not-ready orphan child must requeue, got %#v", res)
	}

	got := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: "c1"}, got); err != nil {
		t.Fatalf("get content: %v", err)
	}
	if got.Status.ResidualVolumeCapture != nil {
		t.Fatalf("residual latch must NOT be stamped while orphan children are not ready, got %#v", got.Status.ResidualVolumeCapture)
	}
}

// TestReconcileImport_StampsResidualCompleteAtSteadyState: an import-mode root has no residual wave, so
// once the content graph is published the reconciler latches the residual gate Complete.
func TestReconcileImport_StampsResidualCompleteAtSteadyState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testVolumeCaptureScheme(t)
	snap := importSnapshot()
	expectedName := snapshotContentName(snap)
	snap.Status.BoundSnapshotContentName = expectedName // already bound => go straight to steady state

	mcpName := usecase.ReconstructedManifestCheckpointName(snap.UID, "")
	mcp := &ssv1alpha1.ManifestCheckpoint{ObjectMeta: metav1.ObjectMeta{Name: mcpName}}
	content := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: expectedName},
		Spec:       desiredImportSnapshotContentSpec(snap),
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(snap, content, mcp).
		WithStatusSubresource(&storagev1alpha1.Snapshot{}, &storagev1alpha1.SnapshotContent{}).Build()
	r := &SnapshotReconciler{Client: cl, APIReader: cl}

	if _, err := r.reconcileImport(ctx, snap, &deckhousev1alpha1.ObjectKeeper{}); err != nil {
		t.Fatalf("reconcileImport: %v", err)
	}

	got := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: expectedName}, got); err != nil {
		t.Fatalf("get content: %v", err)
	}
	if residualPhase(got) != storagev1alpha1.ResidualVolumeCapturePhaseComplete {
		t.Fatalf("import steady-state must latch residual Complete, got %q (%#v)", residualPhase(got), got.Status.ResidualVolumeCapture)
	}
}

// TestReconcileStaticBind_StampsResidualCompleteAtSteadyState: a statically-bound content has no residual
// wave of its own, so the steady-state path latches the residual gate Complete (idempotent).
func TestReconcileStaticBind_StampsResidualCompleteAtSteadyState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testVolumeCaptureScheme(t)
	gv := storagev1alpha1.SchemeGroupVersion.String()
	snap := staticBindSnapshot()
	snap.Status.BoundSnapshotContentName = "c-pre" // already bound => go straight to steady state
	content := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "c-pre"},
		Spec: storagev1alpha1.SnapshotContentSpec{
			SnapshotRef: &storagev1alpha1.SnapshotSubjectRef{APIVersion: gv, Kind: "Snapshot", Name: "snap", Namespace: "ns", UID: types.UID("snap-uid")},
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(snap, content).
		WithStatusSubresource(&storagev1alpha1.Snapshot{}, &storagev1alpha1.SnapshotContent{}).Build()
	r := &SnapshotReconciler{Client: cl, APIReader: cl}

	if _, err := r.reconcileStaticBind(ctx, snap); err != nil {
		t.Fatalf("reconcileStaticBind: %v", err)
	}

	got := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: "c-pre"}, got); err != nil {
		t.Fatalf("get content: %v", err)
	}
	if residualPhase(got) != storagev1alpha1.ResidualVolumeCapturePhaseComplete {
		t.Fatalf("static-bind steady-state must latch residual Complete, got %q (%#v)", residualPhase(got), got.Status.ResidualVolumeCapture)
	}
}
