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
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// captureOwnerWithPhase builds a Capture-mode owner Snapshot carrying the given
// status.captureState.domainSpecificController.phase (empty means pre-Planned, before the domain wrote it).
func captureOwnerWithPhase(t *testing.T, phase string) *unstructured.Unstructured {
	t.Helper()
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": storagev1alpha1.SchemeGroupVersion.String(),
		"kind":       "Snapshot",
		"metadata":   map[string]interface{}{"name": "owner", "namespace": "ns1"},
	}}
	if phase != "" {
		if err := unstructured.SetNestedField(obj.Object, phase, "status", "captureState", "domainSpecificController", "phase"); err != nil {
			t.Fatalf("set phase: %v", err)
		}
	}
	return obj
}

// ownerWithMode builds an owner Snapshot with the given spec.mode and no capture phase.
func ownerWithMode(mode storagev1alpha1.SnapshotMode) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": storagev1alpha1.SchemeGroupVersion.String(),
		"kind":       "Snapshot",
		"metadata":   map[string]interface{}{"name": "owner", "namespace": "ns1"},
		"spec":       map[string]interface{}{"mode": string(mode)},
	}}
}

// TestOwnerChildSetFrozen pins the gate decision: a Capture owner's declared child set is frozen only at
// phase >= Planned (Planned/Finished) or terminal Failed; Import/StaticBind owners (no capture phase) are
// frozen from the start because they write childrenSnapshotRefs atomically.
func TestOwnerChildSetFrozen(t *testing.T) {
	tests := []struct {
		name  string
		owner *unstructured.Unstructured
		want  bool
	}{
		{"capture pre-Planned (empty phase)", captureOwnerWithPhase(t, ""), false},
		{"capture Planning", captureOwnerWithPhase(t, string(storagev1alpha1.SnapshotCapturePhasePlanning)), false},
		{"capture Planned", captureOwnerWithPhase(t, string(storagev1alpha1.SnapshotCapturePhasePlanned)), true},
		{"capture Finished", captureOwnerWithPhase(t, string(storagev1alpha1.SnapshotCapturePhaseFinished)), true},
		{"capture Failed", captureOwnerWithPhase(t, string(storagev1alpha1.SnapshotCapturePhaseFailed)), true},
		{"import mode (no phase)", ownerWithMode(storagev1alpha1.SnapshotModeImport), true},
		{"static bind mode (no phase)", ownerWithMode(storagev1alpha1.SnapshotModeStaticBind), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ownerChildSetFrozen(tc.owner); got != tc.want {
				t.Fatalf("ownerChildSetFrozen = %v, want %v", got, tc.want)
			}
		})
	}
}

// planGateFixture builds a reconciler whose client holds a parent SnapshotContent "root-content" plus two
// bound child Snapshots (each pointing at a present child SnapshotContent), so a publish PAST the gate would
// freeze the parent edge set at exactly two children.
func planGateFixture(t *testing.T) (*SnapshotContentController, client.Client) {
	t.Helper()
	scheme := aggScheme(t)
	parent := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "root-content", UID: "root-uid"}}
	childSnapA := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "csnap-a"},
		Status:     storagev1alpha1.SnapshotStatus{BoundSnapshotContentName: "ccontent-a"},
	}
	childSnapB := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "csnap-b"},
		Status:     storagev1alpha1.SnapshotStatus{BoundSnapshotContentName: "ccontent-b"},
	}
	childContentA := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "ccontent-a"}}
	childContentB := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "ccontent-b"}}
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(parent, childSnapA, childSnapB, childContentA, childContentB).
		WithStatusSubresource(&storagev1alpha1.SnapshotContent{}, &storagev1alpha1.Snapshot{}).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}
	return r, cl
}

// ownerWithChildRefs builds an owner Snapshot declaring the given child Snapshots in
// status.childrenSnapshotRefs, with an optional spec.mode and capture phase.
func ownerWithChildRefs(phase string, mode storagev1alpha1.SnapshotMode, childSnapshotNames ...string) *unstructured.Unstructured {
	refs := make([]interface{}, 0, len(childSnapshotNames))
	for _, n := range childSnapshotNames {
		refs = append(refs, map[string]interface{}{
			"apiVersion": storagev1alpha1.SchemeGroupVersion.String(),
			"kind":       "Snapshot",
			"name":       n,
		})
	}
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": storagev1alpha1.SchemeGroupVersion.String(),
		"kind":       "Snapshot",
		"metadata":   map[string]interface{}{"name": "owner", "namespace": "ns1"},
		"status":     map[string]interface{}{"childrenSnapshotRefs": refs},
	}}
	if mode != "" {
		_ = unstructured.SetNestedField(obj.Object, string(mode), "spec", "mode")
	}
	if phase != "" {
		_ = unstructured.SetNestedField(obj.Object, phase, "status", "captureState", "domainSpecificController", "phase")
	}
	return obj
}

func parentEdgeCount(t *testing.T, cl client.Client) int {
	t.Helper()
	got := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: "root-content"}, got); err != nil {
		t.Fatalf("get parent content: %v", err)
	}
	return len(got.Status.ChildrenSnapshotContentRefs)
}

// TestReconcileChildContentEdgesDoesNotFreezePrePlanned is the direct regression guard for the
// ChildrenLinkPending wedge: a Capture owner whose declared child set is still growing (phase pre-Planned)
// MUST NOT have its content edge set frozen, even when every currently-declared child is bound and
// resolvable. Freezing here on a partial set is what stranded the later-declared orphan VolumeSnapshot.
func TestReconcileChildContentEdgesDoesNotFreezePrePlanned(t *testing.T) {
	ctx := context.Background()
	r, cl := planGateFixture(t)
	content := commonContentWithStatus("root-content", "")
	owner := ownerWithChildRefs("", "", "csnap-a", "csnap-b") // Capture, pre-Planned (empty phase)

	requeue, err := r.reconcileChildContentEdges(ctx, content, owner, "ns1", true)
	if err != nil {
		t.Fatalf("reconcileChildContentEdges: %v", err)
	}
	if !requeue {
		t.Fatal("a pre-Planned Capture owner must requeue (gate), not freeze the edge set")
	}
	if n := parentEdgeCount(t, cl); n != 0 {
		t.Fatalf("no edge must be frozen while the owner is pre-Planned, got %d", n)
	}
}

// TestReconcileChildContentEdgesFreezesAtPlanned asserts that once the Capture owner reaches Planned (its
// declared child set is frozen), the aggregator projects the COMPLETE edge set.
func TestReconcileChildContentEdgesFreezesAtPlanned(t *testing.T) {
	ctx := context.Background()
	r, cl := planGateFixture(t)
	content := commonContentWithStatus("root-content", "")
	owner := ownerWithChildRefs(string(storagev1alpha1.SnapshotCapturePhasePlanned), "", "csnap-a", "csnap-b")

	if _, err := r.reconcileChildContentEdges(ctx, content, owner, "ns1", true); err != nil {
		t.Fatalf("reconcileChildContentEdges: %v", err)
	}
	if n := parentEdgeCount(t, cl); n != 2 {
		t.Fatalf("a Planned owner must freeze the full 2-child edge set, got %d", n)
	}
}

// TestReconcileChildContentEdgesImportProjectsWithoutPhase asserts import owners (no capture phase) still
// project their atomically-written child set.
func TestReconcileChildContentEdgesImportProjectsWithoutPhase(t *testing.T) {
	ctx := context.Background()
	r, cl := planGateFixture(t)
	content := commonContentWithStatus("root-content", "")
	owner := ownerWithChildRefs("", storagev1alpha1.SnapshotModeImport, "csnap-a", "csnap-b")

	if _, err := r.reconcileChildContentEdges(ctx, content, owner, "ns1", true); err != nil {
		t.Fatalf("reconcileChildContentEdges: %v", err)
	}
	if n := parentEdgeCount(t, cl); n != 2 {
		t.Fatalf("an import owner must project the full 2-child edge set, got %d", n)
	}
}
