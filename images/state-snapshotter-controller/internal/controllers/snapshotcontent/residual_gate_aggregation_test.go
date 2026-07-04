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
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
)

// residualContentOpts builds a SnapshotContent unstructured tailored for residual-gate tests. By default
// it pre-latches subtreeManifestsPersisted=true so that the only Ready gate left to exercise is the residual one.
type residualContentOpts struct {
	name            string
	mcpName         string
	snapshotRefKind string // default "Snapshot"
	ownerNS         string
	ownerName       string
	residualPhase   string // "" => latch absent (pending)
	leaf            bool   // sets LabelChildVolumeNode
	readyTrue       bool   // pre-persist Ready=True (upgrade-guard)
	childRefs       []string
}

func residualGateContent(t *testing.T, o residualContentOpts) *unstructured.Unstructured {
	t.Helper()
	kind := o.snapshotRefKind
	if kind == "" {
		kind = "Snapshot"
	}
	c := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: o.name},
		Spec: storagev1alpha1.SnapshotContentSpec{
			SnapshotRef: &storagev1alpha1.SnapshotSubjectRef{
				APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
				Kind:       kind,
				Name:       o.ownerName,
				Namespace:  o.ownerNS,
			},
		},
		Status: storagev1alpha1.SnapshotContentStatus{
			ManifestCheckpointName: o.mcpName,
			// Pre-latch subtreeManifestsPersisted=true so the archive gate is satisfied and the residual gate is isolated.
			SubtreeManifestsPersisted: true,
		},
	}
	if o.leaf {
		c.Labels = map[string]string{snapshot.LabelChildVolumeNode: "true"}
	}
	for _, cn := range o.childRefs {
		c.Status.ChildrenSnapshotContentRefs = append(c.Status.ChildrenSnapshotContentRefs,
			storagev1alpha1.SnapshotContentChildRef{Name: cn})
	}
	if o.residualPhase != "" {
		c.Status.ResidualVolumeCapture = &storagev1alpha1.ResidualVolumeCaptureStatus{Phase: o.residualPhase}
	}
	if o.readyTrue {
		c.Status.Conditions = append(c.Status.Conditions, metav1.Condition{
			Type:               snapshot.ConditionReady,
			Status:             metav1.ConditionTrue,
			Reason:             snapshot.ReasonCompleted,
			Message:            "ready",
			LastTransitionTime: metav1.Now(),
		})
	}
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(c)
	if err != nil {
		t.Fatalf("to unstructured: %v", err)
	}
	obj := &unstructured.Unstructured{Object: raw}
	obj.SetGroupVersionKind(unifiedbootstrap.CommonSnapshotContentGVK())
	return obj
}

// Root content (spec.snapshotRef.kind=Snapshot) with the residual latch ABSENT holds the FIRST Ready=True
// at Ready=False/ResidualVolumeCapturePending, even though every other leg (incl. the archive latch) is ready.
func TestResidualGate_RootAbsentLatchHoldsReady(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	obj := residualGateContent(t, residualContentOpts{name: "root", mcpName: "mcp-ok", ownerNS: "ns1", ownerName: "owner"})
	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, obj)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if !plan.subtreeManifestsPersisted {
		t.Fatalf("precondition: subtree-persist latch must be true, got %v", plan.subtreeManifestsPersisted)
	}
	if plan.residualSweepStatus != metav1.ConditionFalse {
		t.Fatalf("residual gate must be False (latch absent), got %s", plan.residualSweepStatus)
	}
	if plan.readyStatus != metav1.ConditionFalse || plan.readyReason != snapshot.ReasonResidualVolumeCapturePending {
		t.Fatalf("ready=%s/%s, want False/%s", plan.readyStatus, plan.readyReason, snapshot.ReasonResidualVolumeCapturePending)
	}
}

// Root content with residual phase=Complete opens the gate -> Ready=True/Completed.
func TestResidualGate_RootCompleteLatchAllowsReady(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	obj := residualGateContent(t, residualContentOpts{
		name: "root", mcpName: "mcp-ok", ownerNS: "ns1", ownerName: "owner",
		residualPhase: storagev1alpha1.ResidualVolumeCapturePhaseComplete,
	})
	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, obj)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.residualSweepStatus != metav1.ConditionTrue {
		t.Fatalf("residual gate must be True (latch Complete), got %s", plan.residualSweepStatus)
	}
	if plan.readyStatus != metav1.ConditionTrue || plan.readyReason != snapshot.ReasonCompleted {
		t.Fatalf("ready=%s/%s, want True/%s", plan.readyStatus, plan.readyReason, snapshot.ReasonCompleted)
	}
}

// A leaf child-volume-node (LabelChildVolumeNode) is never gated, even with the latch absent
// (anti-deadlock: orphan child nodes have no residual wave of their own).
func TestResidualGate_LeafChildVolumeNodeNotGated(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	// kind=Snapshot on purpose: only the leaf label must open the gate here, so removing the leaf
	// short-circuit would flip this to gated and fail the test.
	obj := residualGateContent(t, residualContentOpts{
		name: "leaf", mcpName: "mcp-ok", ownerNS: "ns1", ownerName: "owner",
		snapshotRefKind: "Snapshot", leaf: true,
	})
	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, obj)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.residualSweepStatus != metav1.ConditionTrue {
		t.Fatalf("leaf must not be gated, got residual=%s", plan.residualSweepStatus)
	}
	if plan.readyStatus != metav1.ConditionTrue {
		t.Fatalf("leaf ready=%s/%s, want True", plan.readyStatus, plan.readyReason)
	}
}

// A non-root content (spec.snapshotRef.kind != Snapshot, e.g. a domain XxxxSnapshot) is never gated.
func TestResidualGate_NonRootKindNotGated(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	obj := residualGateContent(t, residualContentOpts{
		name: "domain-child", mcpName: "mcp-ok", ownerNS: "ns1", ownerName: "vd-snap",
		snapshotRefKind: "DemoVirtualDiskSnapshot",
	})
	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, obj)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.residualSweepStatus != metav1.ConditionTrue {
		t.Fatalf("non-root must not be gated, got residual=%s", plan.residualSweepStatus)
	}
	if plan.readyStatus != metav1.ConditionTrue {
		t.Fatalf("non-root ready=%s/%s, want True", plan.readyStatus, plan.readyReason)
	}
}

// A content whose spec.snapshotRef.kind is "Snapshot" but whose apiVersion is foreign is NOT a storage
// root and must not be gated (locks the apiVersion half of the root discriminator AND).
func TestResidualGate_ForeignApiVersionNotGated(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	// Build a content with kind=Snapshot but a foreign apiVersion on spec.snapshotRef.
	obj := residualGateContent(t, residualContentOpts{name: "foreign", mcpName: "mcp-ok", ownerNS: "ns1", ownerName: "owner"})
	if err := unstructured.SetNestedField(obj.Object, "example.com/v1", "spec", "snapshotRef", "apiVersion"); err != nil {
		t.Fatalf("set foreign apiVersion: %v", err)
	}
	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, obj)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.residualSweepStatus != metav1.ConditionTrue {
		t.Fatalf("foreign apiVersion must not be gated, got residual=%s", plan.residualSweepStatus)
	}
	if plan.readyStatus != metav1.ConditionTrue {
		t.Fatalf("foreign apiVersion ready=%s/%s, want True", plan.readyStatus, plan.readyReason)
	}
}

// An explicit phase=Pending (not just an absent latch) must hold the gate closed: the gate opens only on
// phase==Complete.
func TestResidualGate_ExplicitPendingPhaseHoldsReady(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	obj := residualGateContent(t, residualContentOpts{
		name: "root", mcpName: "mcp-ok", ownerNS: "ns1", ownerName: "owner",
		residualPhase: storagev1alpha1.ResidualVolumeCapturePhasePending,
	})
	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, obj)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.residualSweepStatus != metav1.ConditionFalse {
		t.Fatalf("explicit Pending must keep the gate closed, got residual=%s", plan.residualSweepStatus)
	}
	if plan.readyStatus != metav1.ConditionFalse || plan.readyReason != snapshot.ReasonResidualVolumeCapturePending {
		t.Fatalf("ready=%s/%s, want False/%s", plan.readyStatus, plan.readyReason, snapshot.ReasonResidualVolumeCapturePending)
	}
}

// Upgrade-guard / monotonicity: a root whose Ready is ALREADY persisted True must NOT be re-gated when the
// latch field is still absent (controller rollout onto pre-existing objects), so Ready stays True (no flap).
func TestResidualGate_UpgradeGuardKeepsAlreadyReadyRoot(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	obj := residualGateContent(t, residualContentOpts{
		name: "root", mcpName: "mcp-ok", ownerNS: "ns1", ownerName: "owner",
		readyTrue: true, // already Ready=True, latch absent
	})
	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, obj)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.residualSweepStatus != metav1.ConditionTrue {
		t.Fatalf("upgrade-guard: already-Ready root must not be gated, got residual=%s", plan.residualSweepStatus)
	}
	if plan.readyStatus != metav1.ConditionTrue {
		t.Fatalf("upgrade-guard: ready=%s/%s, want True (no True->False flap)", plan.readyStatus, plan.readyReason)
	}
}

// Priority: an ordinary pending leg (a pending child) outranks the residual gate (it is the lowest priority).
func TestResidualGate_PendingChildWinsOverResidualGate(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	pendingChild := contentWithReadyCond("child-pending", metav1.ConditionFalse, snapshot.ReasonArtifactNotReady, "vsc not readyToUse")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp, pendingChild).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	// Root with the residual latch ABSENT AND a pending child: the higher-priority children leg must win.
	obj := residualGateContent(t, residualContentOpts{
		name: "root", mcpName: "mcp-ok", ownerNS: "ns1", ownerName: "owner",
		childRefs: []string{"child-pending"},
	})
	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, obj)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.readyStatus != metav1.ConditionFalse || plan.readyReason != snapshot.ReasonChildrenPending {
		t.Fatalf("ready=%s/%s, want False/%s (children pending outranks residual gate)", plan.readyStatus, plan.readyReason, snapshot.ReasonChildrenPending)
	}
}
