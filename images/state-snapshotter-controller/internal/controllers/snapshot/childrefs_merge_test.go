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
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/common"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

func childRef(name string) storagev1alpha1.SnapshotChildRef {
	return storagev1alpha1.SnapshotChildRef{
		APIVersion: "demo.test/v1",
		Kind:       "DemoSnapshot",
		Name:       name,
	}
}

func TestMergeSnapshotChildRefs(t *testing.T) {
	keep := childRef("other")
	child := childRef("child")
	got := mergeSnapshotChildRefs([]storagev1alpha1.SnapshotChildRef{keep}, []storagev1alpha1.SnapshotChildRef{child})
	if len(got) != 2 {
		t.Fatalf("want 2 refs got %d %+v", len(got), got)
	}
	if got[0].Name != "child" || got[1].Name != "other" {
		t.Fatalf("want stable sort by name got %+v", got)
	}
	if !snapshotChildRefsEqualIgnoreOrder(got, []storagev1alpha1.SnapshotChildRef{keep, child}) {
		t.Fatalf("multiset mismatch %+v", got)
	}

	overwrite := mergeSnapshotChildRefs(
		[]storagev1alpha1.SnapshotChildRef{childRef("x")},
		[]storagev1alpha1.SnapshotChildRef{childRef("x")},
	)
	if len(overwrite) != 1 || overwrite[0].Name != "x" {
		t.Fatalf("same-key merge: %+v", overwrite)
	}
}

func TestPriorityLayerChildrenSnapshotReady(t *testing.T) {
	ctx := context.Background()
	readyChild := demoSnapshotChild("ready", []metav1.Condition{{
		Type:               snapshotpkg.ConditionChildrenSnapshotReady,
		Status:             metav1.ConditionTrue,
		Reason:             snapshotpkg.ReasonCompleted,
		ObservedGeneration: 1,
	}})
	pendingChild := demoSnapshotChild("pending", nil)
	failedChild := demoSnapshotChild("failed", []metav1.Condition{{
		Type:               snapshotpkg.ConditionChildrenSnapshotReady,
		Status:             metav1.ConditionFalse,
		Reason:             snapshotpkg.ReasonGraphPlanningFailed,
		Message:            "child graph failed",
		ObservedGeneration: 1,
	}})

	t.Run("all graph ready", func(t *testing.T) {
		r := &SnapshotReconciler{Client: fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(readyChild).Build()}
		ready, terminal, pending, err := r.priorityLayerChildrenSnapshotReady(ctx, "ns1", []storagev1alpha1.SnapshotChildRef{childRef("ready")})
		if err != nil || !ready || terminal != "" || len(pending) != 0 {
			t.Fatalf("want ready with no terminal/pending, got ready=%v terminal=%q pending=%v err=%v", ready, terminal, pending, err)
		}
	})

	t.Run("pending blocks lower priority without terminal message", func(t *testing.T) {
		r := &SnapshotReconciler{Client: fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(pendingChild).Build()}
		ready, terminal, pending, err := r.priorityLayerChildrenSnapshotReady(ctx, "ns1", []storagev1alpha1.SnapshotChildRef{childRef("pending")})
		if err != nil || ready || terminal != "" || len(pending) != 1 {
			t.Fatalf("want pending with no terminal message, got ready=%v terminal=%q pending=%v err=%v", ready, terminal, pending, err)
		}
		if !strings.Contains(pending[0], "no ChildrenSnapshotReady condition yet") {
			t.Fatalf("pending descriptor should explain missing ChildrenSnapshotReady, got %q", pending[0])
		}
	})

	t.Run("graph ready true without observedGeneration stays pending", func(t *testing.T) {
		child := demoSnapshotChildRawConditions("noobserved", 1, []map[string]interface{}{{
			"type":   snapshotpkg.ConditionChildrenSnapshotReady,
			"status": string(metav1.ConditionTrue),
			"reason": snapshotpkg.ReasonCompleted,
		}})
		r := &SnapshotReconciler{Client: fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(child).Build()}
		ready, terminal, pending, err := r.priorityLayerChildrenSnapshotReady(ctx, "ns1", []storagev1alpha1.SnapshotChildRef{childRef("noobserved")})
		if err != nil || ready || terminal != "" || len(pending) != 1 {
			t.Fatalf("want pending, got ready=%v terminal=%q pending=%v err=%v", ready, terminal, pending, err)
		}
		if !strings.Contains(pending[0], "without observedGeneration") {
			t.Fatalf("pending descriptor should flag missing observedGeneration, got %q", pending[0])
		}
	})

	t.Run("graph ready true with stale observedGeneration stays pending", func(t *testing.T) {
		child := demoSnapshotChildRawConditions("stale", 3, []map[string]interface{}{{
			"type":               snapshotpkg.ConditionChildrenSnapshotReady,
			"status":             string(metav1.ConditionTrue),
			"reason":             snapshotpkg.ReasonCompleted,
			"observedGeneration": int64(2),
		}})
		r := &SnapshotReconciler{Client: fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(child).Build()}
		ready, terminal, pending, err := r.priorityLayerChildrenSnapshotReady(ctx, "ns1", []storagev1alpha1.SnapshotChildRef{childRef("stale")})
		if err != nil || ready || terminal != "" || len(pending) != 1 {
			t.Fatalf("want pending, got ready=%v terminal=%q pending=%v err=%v", ready, terminal, pending, err)
		}
		if !strings.Contains(pending[0], "stale") {
			t.Fatalf("pending descriptor should flag stale observedGeneration, got %q", pending[0])
		}
	})

	t.Run("terminal graph failure returns message", func(t *testing.T) {
		r := &SnapshotReconciler{Client: fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(failedChild).Build()}
		ready, terminal, pending, err := r.priorityLayerChildrenSnapshotReady(ctx, "ns1", []storagev1alpha1.SnapshotChildRef{childRef("failed")})
		if err != nil || ready || len(pending) != 0 || !strings.Contains(terminal, "failed graph planning") {
			t.Fatalf("want terminal failure message, got ready=%v terminal=%q pending=%v err=%v", ready, terminal, pending, err)
		}
	})

	t.Run("ready-based terminal failure requires current observedGeneration", func(t *testing.T) {
		staleTerminal := demoSnapshotChildReadyTerminal("ready-stale", 3, 2, "ListFailed")
		r := &SnapshotReconciler{Client: fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(staleTerminal).Build()}
		ready, terminal, pending, err := r.priorityLayerChildrenSnapshotReady(ctx, "ns1", []storagev1alpha1.SnapshotChildRef{childRef("ready-stale")})
		if err != nil || ready || terminal != "" || len(pending) != 1 {
			t.Fatalf("stale terminal Ready must be pending, not terminal; got ready=%v terminal=%q pending=%v err=%v", ready, terminal, pending, err)
		}

		currentTerminal := demoSnapshotChildReadyTerminal("ready-current", 3, 3, "ListFailed")
		r = &SnapshotReconciler{Client: fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(currentTerminal).Build()}
		ready, terminal, pending, err = r.priorityLayerChildrenSnapshotReady(ctx, "ns1", []storagev1alpha1.SnapshotChildRef{childRef("ready-current")})
		if err != nil || ready || len(pending) != 0 || !strings.Contains(terminal, "failed") {
			t.Fatalf("current terminal Ready must be terminal; got ready=%v terminal=%q pending=%v err=%v", ready, terminal, pending, err)
		}
	})
}

// domainChildReady builds a bound child snapshot whose Ready condition reflects full readiness (Ready=True)
// or in-progress capture (Ready=False/Capturing). It is the fixture for the orphan-PVC final-wave gate
// (allDeclaredDomainChildSnapshotsReady), which keys on full Ready (via ClassifyGenericChildSnapshotReady),
// not on ChildrenSnapshotReady.
func domainChildReady(name string, ready bool) *unstructured.Unstructured {
	cond := metav1.Condition{Type: snapshotpkg.ConditionReady, Status: metav1.ConditionTrue, Reason: snapshotpkg.ReasonCompleted, ObservedGeneration: 1}
	if !ready {
		cond = metav1.Condition{Type: snapshotpkg.ConditionReady, Status: metav1.ConditionFalse, Reason: "Capturing", Message: "still capturing", ObservedGeneration: 1}
	}
	child := demoSnapshotChild(name, []metav1.Condition{cond})
	if err := unstructured.SetNestedField(child.Object, "content-"+name, "status", "boundSnapshotContentName"); err != nil {
		panic(err)
	}
	return child
}

func TestAllDeclaredDomainChildSnapshotsReady(t *testing.T) {
	ctx := context.Background()
	ns := "ns1"
	vsLeaf := storagev1alpha1.SnapshotChildRef{APIVersion: snapshotpkg.CSISnapshotAPIVersion, Kind: snapshotpkg.KindVolumeSnapshot, Name: "nss-vs-x"}

	t.Run("no domain children passes vacuously (VS visibility leaf skipped)", func(t *testing.T) {
		r := &SnapshotReconciler{Client: fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()}
		ready, pending, err := r.allDeclaredDomainChildSnapshotsReady(ctx, ns, []storagev1alpha1.SnapshotChildRef{vsLeaf})
		if err != nil || !ready || len(pending) != 0 {
			t.Fatalf("vacuous gate must be open, got ready=%v pending=%v err=%v", ready, pending, err)
		}
	})

	t.Run("domain child not yet Ready keeps gate closed", func(t *testing.T) {
		child := domainChildReady("pending", false)
		r := &SnapshotReconciler{Client: fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(child).Build()}
		ready, pending, err := r.allDeclaredDomainChildSnapshotsReady(ctx, ns, []storagev1alpha1.SnapshotChildRef{childRef("pending")})
		if err != nil || ready || len(pending) != 1 {
			t.Fatalf("closed gate expected, got ready=%v pending=%v err=%v", ready, pending, err)
		}
	})

	t.Run("missing domain child keeps gate closed", func(t *testing.T) {
		r := &SnapshotReconciler{Client: fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()}
		ready, pending, err := r.allDeclaredDomainChildSnapshotsReady(ctx, ns, []storagev1alpha1.SnapshotChildRef{childRef("absent")})
		if err != nil || ready || len(pending) != 1 {
			t.Fatalf("closed gate expected, got ready=%v pending=%v err=%v", ready, pending, err)
		}
		if !strings.Contains(pending[0], "not created yet") {
			t.Fatalf("pending descriptor should flag the missing child, got %q", pending[0])
		}
	})

	t.Run("all domain children Ready opens gate, VS leaf ignored", func(t *testing.T) {
		readyChild := domainChildReady("ready", true)
		r := &SnapshotReconciler{Client: fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(readyChild).Build()}
		refs := []storagev1alpha1.SnapshotChildRef{childRef("ready"), vsLeaf}
		ready, pending, err := r.allDeclaredDomainChildSnapshotsReady(ctx, ns, refs)
		if err != nil || !ready || len(pending) != 0 {
			t.Fatalf("open gate expected, got ready=%v pending=%v err=%v", ready, pending, err)
		}
	})

	t.Run("one ready one pending keeps gate closed", func(t *testing.T) {
		readyChild := domainChildReady("ready", true)
		pendingChild := domainChildReady("pending", false)
		r := &SnapshotReconciler{Client: fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(readyChild, pendingChild).Build()}
		refs := []storagev1alpha1.SnapshotChildRef{childRef("ready"), childRef("pending")}
		ready, pending, err := r.allDeclaredDomainChildSnapshotsReady(ctx, ns, refs)
		if err != nil || ready || len(pending) != 1 {
			t.Fatalf("closed gate expected with one pending, got ready=%v pending=%v err=%v", ready, pending, err)
		}
	})
}

// demoSnapshotChildReadyTerminal builds a bound child snapshot with a terminal Ready=False condition
// (one of usecase.ChildSnapshotTerminalReadyReasons), so a test can exercise the Ready-based terminal
// path of snapshotChildTerminalFailure with explicit generation vs observedGeneration.
func demoSnapshotChildReadyTerminal(name string, generation, observedGeneration int64, reason string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "demo.test/v1",
			"kind":       "DemoSnapshot",
			"metadata": map[string]interface{}{
				"name":       name,
				"namespace":  "ns1",
				"generation": generation,
			},
			"status": map[string]interface{}{
				"boundSnapshotContentName": "content-" + name,
				"conditions": []interface{}{
					map[string]interface{}{
						"type":               snapshotpkg.ConditionReady,
						"status":             string(metav1.ConditionFalse),
						"reason":             reason,
						"message":            "terminal failure",
						"observedGeneration": observedGeneration,
					},
				},
			},
		},
	}
}

func TestSnapshotCoverageCheckerUsesSpecSourceRef(t *testing.T) {
	ctx := context.Background()
	source := demoSourceObject("vm-1", "uid-a")
	identity := controllercommon.SnapshotSourceIdentity{
		APIVersion: "demo.test/v1",
		Kind:       "DemoSource",
		Namespace:  "ns1",
		Name:       "vm-1",
	}
	child := demoSnapshotChildWithSource("covered", identity)
	checker := newSnapshotCoverageChecker(
		fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(child).Build(),
		"ns1",
		[]storagev1alpha1.SnapshotChildRef{childRef("covered")},
	)
	covered, err := checker.IsCovered(ctx, source)
	if err != nil {
		t.Fatalf("IsCovered returned error: %v", err)
	}
	if !covered {
		t.Fatalf("expected source vm-1 to be covered")
	}

	// Coverage is name-only (spec.sourceRef carries no UID): a source recreated with the same name
	// but a different UID is still considered covered by the existing child snapshot.
	recreated := demoSourceObject("vm-1", "uid-b")
	covered, err = checker.IsCovered(ctx, recreated)
	if err != nil {
		t.Fatalf("IsCovered for recreated source returned error: %v", err)
	}
	if !covered {
		t.Fatalf("source recreated with the same name must remain covered (name-only coverage)")
	}
}

// TestEnsureParentOwnedChildSnapshotWritesSpecSourceRef pins the WRITE side of the source-of-truth
// flip: the planner must persist spec.sourceRef {apiVersion,kind,name} (derived from the live source)
// on the child it creates, and that written value must round-trip back through the coverage checker so
// the same source reads as covered. Without this, a regression that drops/mis-maps the spec block would
// still pass every coverage test (which fabricate the field independently).
func TestEnsureParentOwnedChildSnapshotWritesSpecSourceRef(t *testing.T) {
	ctx := context.Background()
	cl := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
	r := &SnapshotReconciler{Client: cl}

	nsSnap := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "root", UID: "root-uid"},
	}
	gvk := schema.GroupVersionKind{Group: "demo.test", Version: "v1", Kind: "DemoSnapshot"}
	source := demoSourceObject("vm-1", "uid-a")

	const childName = "nss-child-vm-1"
	if err := r.ensureParentOwnedChildSnapshot(ctx, nsSnap, childName, gvk, source); err != nil {
		t.Fatalf("ensureParentOwnedChildSnapshot: %v", err)
	}

	created := &unstructured.Unstructured{}
	created.SetGroupVersionKind(gvk)
	if err := cl.Get(ctx, client.ObjectKey{Namespace: "ns1", Name: childName}, created); err != nil {
		t.Fatalf("get created child: %v", err)
	}
	srcRef, found, err := unstructured.NestedStringMap(created.Object, "spec", "sourceRef")
	if err != nil || !found {
		t.Fatalf("created child must carry spec.sourceRef (found=%v err=%v)", found, err)
	}
	if srcRef["apiVersion"] != "demo.test/v1" || srcRef["kind"] != "DemoSource" || srcRef["name"] != "vm-1" {
		t.Fatalf("spec.sourceRef mismatch: %+v", srcRef)
	}

	// Round-trip: the coverage checker reading the just-created child must consider the source covered.
	checker := newSnapshotCoverageChecker(cl, "ns1", []storagev1alpha1.SnapshotChildRef{{
		APIVersion: "demo.test/v1",
		Kind:       "DemoSnapshot",
		Name:       childName,
	}})
	covered, err := checker.IsCovered(ctx, source)
	if err != nil {
		t.Fatalf("IsCovered: %v", err)
	}
	if !covered {
		t.Fatalf("source must be covered by the child the planner just created (write->read round-trip)")
	}
}

func TestSnapshotCoverageCheckerSkipsChildWithoutSourceRef(t *testing.T) {
	ctx := context.Background()
	child := demoSnapshotChild("missing-source-ref", nil)
	checker := newSnapshotCoverageChecker(
		fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(child).Build(),
		"ns1",
		[]storagev1alpha1.SnapshotChildRef{childRef("missing-source-ref")},
	)
	covered, err := checker.IsCovered(ctx, demoSourceObject("vm-1", "uid-a"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if covered {
		t.Fatalf("a child without spec.sourceRef must not contribute coverage")
	}
}

// TestCoverageRootsForNextWaveExcludesParentStatus locks the fix at its source: the next-wave
// coverage seed is derived ONLY from the refs planned in the current pass, never the parent's stale
// status. The signature alone forbids passing status; the test also asserts the seed is a copy so a
// later append (ObservePlannedSnapshot growing roots) cannot alias and mutate the planned slice.
func TestCoverageRootsForNextWaveExcludesParentStatus(t *testing.T) {
	planned := []storagev1alpha1.SnapshotChildRef{childRef("nss-child-vm")}
	got := coverageRootsForNextWave(planned)
	if len(got) != 1 || got[0].Name != "nss-child-vm" {
		t.Fatalf("seed must echo planned refs, got %+v", got)
	}
	got = append(got, childRef("nss-child-extra"))
	got[0].Name = "mutated"
	if len(planned) != 1 || planned[0].Name != "nss-child-vm" {
		t.Fatalf("seed must be an independent copy; planned mutated: %+v", planned)
	}
}

// TestParentStatusSeedWouldSelfCoverStandalone is the minimal regression: seeding the coverage
// checker with the parent's own generated standalone-disk ref makes that disk self-cover (the bug),
// while the fixed next-wave seed never includes status refs and therefore does not skip it.
func TestParentStatusSeedWouldSelfCoverStandalone(t *testing.T) {
	ctx := context.Background()
	diskStandaloneIdentity := demoSourceIdentity("disk-standalone")
	diskStandaloneSource := demoSourceObject("disk-standalone", "uid-disk-standalone")
	diskStandaloneChild := demoSnapshotChildWithSource("nss-child-disk-standalone", diskStandaloneIdentity)
	cl := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(diskStandaloneChild).Build()

	buggy := newSnapshotCoverageChecker(cl, "ns1", []storagev1alpha1.SnapshotChildRef{childRef("nss-child-disk-standalone")})
	covered, err := buggy.IsCovered(ctx, diskStandaloneSource)
	if err != nil {
		t.Fatalf("IsCovered (status-seeded): %v", err)
	}
	if !covered {
		t.Fatalf("precondition: status-seeded checker must self-cover the standalone disk (this is the bug)")
	}

	fixed := newSnapshotCoverageChecker(cl, "ns1", coverageRootsForNextWave(nil))
	covered, err = fixed.IsCovered(ctx, diskStandaloneSource)
	if err != nil {
		t.Fatalf("IsCovered (fixed seed): %v", err)
	}
	if covered {
		t.Fatalf("fixed next-wave seed must not self-cover the standalone disk")
	}
}

// TestRecomputeChildGraphKeepsLowerPriorityStandalone reconstructs a two-wave recompute
// (VM priority 100 -> Disk priority 10) using the production coverage checker and merge, and proves
// the deterministic idempotency invariant: on a repeated reconcile where the root status already
// carries both generated refs, the lower-priority standalone disk ref survives, the disk-vm covered
// by the VM subtree is not re-added (no duplicate), and the VM ref is kept.
func TestRecomputeChildGraphKeepsLowerPriorityStandalone(t *testing.T) {
	ctx := context.Background()

	vmIdentity := demoSourceIdentity("vm")
	diskVMIdentity := demoSourceIdentity("disk-vm")
	diskStandaloneIdentity := demoSourceIdentity("disk-standalone")

	diskVMSource := demoSourceObject("disk-vm", "uid-disk-vm")
	diskStandaloneSource := demoSourceObject("disk-standalone", "uid-disk-standalone")

	diskVMChildRef := childRef("nss-child-disk-vm")
	vmChild := demoSnapshotChildWithSourceAndChildren("nss-child-vm", vmIdentity, diskVMChildRef)
	diskVMChild := demoSnapshotChildWithSource("nss-child-disk-vm", diskVMIdentity)
	diskStandaloneChild := demoSnapshotChildWithSource("nss-child-disk-standalone", diskStandaloneIdentity)

	cl := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).
		WithObjects(vmChild, diskVMChild, diskStandaloneChild).Build()

	// Wave 1 (VM, priority 100): the highest-priority wave seeds from nil, so the VM source is always
	// re-planned and kept in the current pass.
	vmRef := childRef("nss-child-vm")
	desiredRefs := []storagev1alpha1.SnapshotChildRef{vmRef}

	// Wave 2 (Disk, priority 10): coverage seeded ONLY from this pass (the fix). disk-vm is covered by
	// the VM subtree; disk-standalone is not covered despite a stale status ref existing.
	coverage := newSnapshotCoverageChecker(cl, "ns1", coverageRootsForNextWave(desiredRefs))

	diskVMCovered, err := coverage.IsCovered(ctx, diskVMSource)
	if err != nil {
		t.Fatalf("IsCovered(disk-vm): %v", err)
	}
	if !diskVMCovered {
		t.Fatalf("disk-vm must be covered by the VM subtree (no duplicate direct child expected)")
	}
	diskStandaloneCovered, err := coverage.IsCovered(ctx, diskStandaloneSource)
	if err != nil {
		t.Fatalf("IsCovered(disk-standalone): %v", err)
	}
	if diskStandaloneCovered {
		t.Fatalf("disk-standalone must NOT be covered: a stale parent status ref must not self-cover it")
	}

	// disk-vm skipped (covered); disk-standalone re-planned this pass.
	desiredRefs = append(desiredRefs, childRef("nss-child-disk-standalone"))

	// Merge against the parent's previous status, which already carried both generated refs.
	currentStatus := []storagev1alpha1.SnapshotChildRef{vmRef, childRef("nss-child-disk-standalone")}
	merged := mergeSnapshotManagedChildRefs(currentStatus, desiredRefs)

	if countSnapshotChildRefName(merged, "nss-child-vm") != 1 {
		t.Fatalf("merged must keep exactly one VM ref: %+v", merged)
	}
	if countSnapshotChildRefName(merged, "nss-child-disk-standalone") != 1 {
		t.Fatalf("merged must keep exactly one standalone disk ref (regression): %+v", merged)
	}
	if countSnapshotChildRefName(merged, "nss-child-disk-vm") != 0 {
		t.Fatalf("merged must NOT add a duplicate direct ref for the VM-covered disk: %+v", merged)
	}
}

func TestChildGraphCaptureGate(t *testing.T) {
	t.Run("status changed requeues immediately (not RequeueAfter)", func(t *testing.T) {
		res, block := childGraphCaptureGate(true, true)
		if !block || !res.Requeue || res.RequeueAfter != 0 {
			t.Fatalf("changed graph must block with immediate Requeue:true; got block=%v res=%+v", block, res)
		}
	})

	t.Run("changed takes precedence over pending", func(t *testing.T) {
		res, block := childGraphCaptureGate(true, false)
		if !block || !res.Requeue || res.RequeueAfter != 0 {
			t.Fatalf("changed graph must requeue immediately even when pending; got block=%v res=%+v", block, res)
		}
	})

	t.Run("pending unchanged uses RequeueAfter polling fallback", func(t *testing.T) {
		res, block := childGraphCaptureGate(false, false)
		if !block {
			t.Fatalf("pending graph must block capture")
		}
		if res.Requeue {
			t.Fatalf("pending graph must not set Requeue:true (hot-loop); got %+v", res)
		}
		if res.RequeueAfter != snapshotChildGraphPollInterval {
			t.Fatalf("pending graph RequeueAfter = %v, want poll interval %v", res.RequeueAfter, snapshotChildGraphPollInterval)
		}
	})

	t.Run("ready unchanged graph proceeds to capture", func(t *testing.T) {
		res, block := childGraphCaptureGate(false, true)
		if block || res.Requeue || res.RequeueAfter != 0 {
			t.Fatalf("ready unchanged graph must proceed; got block=%v res=%+v", block, res)
		}
	})
}

func TestSummarizePendingChildrenCapsMessage(t *testing.T) {
	small := []string{"a", "b", "c"}
	if got := summarizePendingChildren(small); !strings.HasPrefix(got, "pending children: ") || strings.Contains(got, "first") {
		t.Fatalf("small list must not be capped, got %q", got)
	}

	large := make([]string, 0, 50)
	for i := 0; i < 50; i++ {
		large = append(large, fmt.Sprintf("child-%d", i))
	}
	got := summarizePendingChildren(large)
	if !strings.Contains(got, "first 20 of 50") {
		t.Fatalf("large list must report cap and total, got %q", got)
	}
	if strings.Contains(got, "child-20") {
		t.Fatalf("capped message must not include the 21st entry, got %q", got)
	}
	if !strings.Contains(got, "child-19") {
		t.Fatalf("capped message must include the first 20 entries, got %q", got)
	}
}

func demoSnapshotChild(name string, conditions []metav1.Condition) *unstructured.Unstructured {
	child := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "demo.test/v1",
			"kind":       "DemoSnapshot",
			"metadata": map[string]interface{}{
				"name":       name,
				"namespace":  "ns1",
				"generation": int64(1),
			},
		},
	}
	if len(conditions) > 0 {
		status := map[string]interface{}{}
		items := make([]interface{}, 0, len(conditions))
		for _, condition := range conditions {
			items = append(items, map[string]interface{}{
				"type":               condition.Type,
				"status":             string(condition.Status),
				"reason":             condition.Reason,
				"message":            condition.Message,
				"observedGeneration": condition.ObservedGeneration,
			})
		}
		status["conditions"] = items
		child.Object["status"] = status
	}
	return child
}

// demoSnapshotChildRawConditions builds a child snapshot with explicit raw condition maps, so a test
// can omit observedGeneration entirely (to exercise the strict ChildrenSnapshotReady contract) or set a stale
// value, which the typed helper cannot express.
func demoSnapshotChildRawConditions(name string, generation int64, conditions []map[string]interface{}) *unstructured.Unstructured {
	items := make([]interface{}, 0, len(conditions))
	for _, c := range conditions {
		items = append(items, c)
	}
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "demo.test/v1",
			"kind":       "DemoSnapshot",
			"metadata": map[string]interface{}{
				"name":       name,
				"namespace":  "ns1",
				"generation": generation,
			},
			"status": map[string]interface{}{
				"conditions": items,
			},
		},
	}
}

func demoSnapshotChildWithSource(name string, identity controllercommon.SnapshotSourceIdentity) *unstructured.Unstructured {
	child := demoSnapshotChild(name, nil)
	if err := unstructured.SetNestedStringMap(child.Object, map[string]string{
		"apiVersion": identity.APIVersion,
		"kind":       identity.Kind,
		"name":       identity.Name,
	}, "spec", "sourceRef"); err != nil {
		panic(err)
	}
	return child
}

// demoSnapshotChildWithSourceAndChildren builds a generated child snapshot carrying both its
// spec.sourceRef and a status.childrenSnapshotRefs list, so coverage-recompute scenarios can
// model a higher-priority subtree (e.g. VM snapshot owning the disk-vm snapshot).
func demoSnapshotChildWithSourceAndChildren(name string, identity controllercommon.SnapshotSourceIdentity, childRefs ...storagev1alpha1.SnapshotChildRef) *unstructured.Unstructured {
	child := demoSnapshotChildWithSource(name, identity)
	if len(childRefs) == 0 {
		return child
	}
	items := make([]interface{}, 0, len(childRefs))
	for _, r := range childRefs {
		items = append(items, map[string]interface{}{
			"apiVersion": r.APIVersion,
			"kind":       r.Kind,
			"name":       r.Name,
		})
	}
	if err := unstructured.SetNestedSlice(child.Object, items, "status", "childrenSnapshotRefs"); err != nil {
		panic(err)
	}
	return child
}

func demoSourceIdentity(name string) controllercommon.SnapshotSourceIdentity {
	return controllercommon.SnapshotSourceIdentity{
		APIVersion: "demo.test/v1",
		Kind:       "DemoSource",
		Namespace:  "ns1",
		Name:       name,
	}
}

func countSnapshotChildRefName(refs []storagev1alpha1.SnapshotChildRef, name string) int {
	n := 0
	for _, r := range refs {
		if r.Name == name {
			n++
		}
	}
	return n
}

func demoSourceObject(name, uid string) *unstructured.Unstructured {
	source := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "demo.test/v1",
			"kind":       "DemoSource",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": "ns1",
				"uid":       uid,
			},
		},
	}
	return source
}

func TestRemoveSnapshotChildRefsByKeys(t *testing.T) {
	existing := []storagev1alpha1.SnapshotChildRef{
		childRef("keep"),
		childRef("drop"),
	}
	remove := []storagev1alpha1.SnapshotChildRef{childRef("drop")}
	got := removeSnapshotChildRefsByKeys(existing, remove)
	if len(got) != 1 || got[0].Name != "keep" {
		t.Fatalf("got %+v", got)
	}
}

func TestRemoveSnapshotContentChildRefsByKeys(t *testing.T) {
	existing := []storagev1alpha1.SnapshotContentChildRef{{Name: "a"}, {Name: "b"}}
	got := removeSnapshotContentChildRefsByKeys(existing, []storagev1alpha1.SnapshotContentChildRef{{Name: "a"}})
	if len(got) != 1 || got[0].Name != "b" {
		t.Fatalf("got %+v", got)
	}
}

func TestMergeSnapshotContentChildRefs(t *testing.T) {
	existing := []storagev1alpha1.SnapshotContentChildRef{{Name: "x"}}
	upsert := []storagev1alpha1.SnapshotContentChildRef{{Name: "y"}}
	got := mergeSnapshotContentChildRefs(existing, upsert)
	if len(got) != 2 {
		t.Fatalf("want len 2 got %d", len(got))
	}
	got2 := mergeSnapshotContentChildRefs(got, []storagev1alpha1.SnapshotContentChildRef{{Name: "x"}})
	if len(got2) != 2 {
		t.Fatalf("re-upsert same name: want len 2 got %d", len(got2))
	}
}
