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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
)

// csiVSGVK is the external-snapshotter VolumeSnapshot GVK used for the orphan visibility leaves.
var csiVSGVK = schema.GroupVersionKind{
	Group:   snapshot.CSISnapshotGroup,
	Version: snapshot.CSISnapshotVersion,
	Kind:    snapshot.KindVolumeSnapshot,
}

// orphanGateOpts builds a namespace-root SnapshotContent for orphan-link-gate tests. It pre-latches
// subtreeManifestsPersisted=true so the only remaining gate to exercise is the orphan-link one.
type orphanGateOpts struct {
	name            string
	mcpName         string
	snapshotRefKind string // default "Snapshot"
	ownerNS         string
	ownerName       string
	leaf            bool     // sets LabelChildVolumeNode
	readyTrue       bool     // pre-persist Ready=True (upgrade-guard)
	childRefs       []string // linked child content edges (status.childrenSnapshotContentRefs)
}

func orphanGateContent(t *testing.T, o orphanGateOpts) *unstructured.Unstructured {
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
			ManifestCheckpointName:    o.mcpName,
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

// ownerSnapshotWithVSLeaf builds the owning core Snapshot carrying a single orphan CSI VolumeSnapshot
// visibility leaf in status.childrenSnapshotRefs.
func ownerSnapshotWithVSLeaf(ns, name, vsName string) *storagev1alpha1.Snapshot {
	return &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Status: storagev1alpha1.SnapshotStatus{
			ChildrenSnapshotRefs: []storagev1alpha1.SnapshotChildRef{
				{APIVersion: snapshot.CSISnapshotAPIVersion, Kind: snapshot.KindVolumeSnapshot, Name: vsName},
			},
		},
	}
}

// orphanVS builds the CSI VolumeSnapshot unstructured whose UID keys the orphan child content name.
func orphanVS(ns, name string, uid types.UID) *unstructured.Unstructured {
	vs := &unstructured.Unstructured{}
	vs.SetGroupVersionKind(csiVSGVK)
	vs.SetNamespace(ns)
	vs.SetName(name)
	vs.SetUID(uid)
	return vs
}

// A root whose declared orphan VS leaf has NOT been linked into childrenSnapshotContentRefs is held at
// Ready=False/ChildrenLinkPending (fail-closed), even though every other leg (incl. the subtree-persist
// latch) is satisfied.
func TestOrphanLinkGate_UnlinkedOrphanHoldsReady(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	owner := ownerSnapshotWithVSLeaf("ns1", "owner", "vs-a")
	vs := orphanVS("ns1", "vs-a", types.UID("vs-a-uid"))
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp, owner, vs).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	obj := orphanGateContent(t, orphanGateOpts{name: "root", mcpName: "mcp-ok", ownerNS: "ns1", ownerName: "owner"})
	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, obj)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.childrenReady != metav1.ConditionFalse || plan.childrenReason != snapshot.ReasonChildrenLinkPending {
		t.Fatalf("childrenReady=%s/%s, want False/%s", plan.childrenReady, plan.childrenReason, snapshot.ReasonChildrenLinkPending)
	}
	if plan.readyStatus != metav1.ConditionFalse || plan.readyReason != snapshot.ReasonChildrenLinkPending {
		t.Fatalf("ready=%s/%s, want False/%s", plan.readyStatus, plan.readyReason, snapshot.ReasonChildrenLinkPending)
	}
}

// Once the orphan child content is linked into childrenSnapshotContentRefs (and Ready), the gate opens ->
// Ready=True/Completed.
func TestOrphanLinkGate_LinkedOrphanAllowsReady(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	owner := ownerSnapshotWithVSLeaf("ns1", "owner", "vs-a")
	vs := orphanVS("ns1", "vs-a", types.UID("vs-a-uid"))
	childName := ChildVolumeContentName(types.UID("vs-a-uid"))
	child := contentWithReadyCond(childName, metav1.ConditionTrue, snapshot.ReasonCompleted, "ready")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp, owner, vs, child).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	obj := orphanGateContent(t, orphanGateOpts{
		name: "root", mcpName: "mcp-ok", ownerNS: "ns1", ownerName: "owner",
		childRefs: []string{childName},
	})
	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, obj)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.readyStatus != metav1.ConditionTrue || plan.readyReason != snapshot.ReasonCompleted {
		t.Fatalf("ready=%s/%s, want True/%s", plan.readyStatus, plan.readyReason, snapshot.ReasonCompleted)
	}
}

// A leaf child-volume-node (LabelChildVolumeNode) has no orphan wave of its own and is never gated.
func TestOrphanLinkGate_LeafChildVolumeNodeNotGated(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	owner := ownerSnapshotWithVSLeaf("ns1", "owner", "vs-a")
	vs := orphanVS("ns1", "vs-a", types.UID("vs-a-uid"))
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp, owner, vs).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	obj := orphanGateContent(t, orphanGateOpts{
		name: "leaf", mcpName: "mcp-ok", ownerNS: "ns1", ownerName: "owner", leaf: true,
	})
	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, obj)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.readyStatus != metav1.ConditionTrue {
		t.Fatalf("leaf ready=%s/%s, want True", plan.readyStatus, plan.readyReason)
	}
}

// A non-root content (spec.snapshotRef.kind != Snapshot, e.g. a domain XxxxSnapshot) is never gated.
func TestOrphanLinkGate_NonRootKindNotGated(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	obj := orphanGateContent(t, orphanGateOpts{
		name: "domain-child", mcpName: "mcp-ok", ownerNS: "ns1", ownerName: "vd-snap",
		snapshotRefKind: "DemoVirtualDiskSnapshot",
	})
	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, obj)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.readyStatus != metav1.ConditionTrue {
		t.Fatalf("non-root ready=%s/%s, want True", plan.readyStatus, plan.readyReason)
	}
}

// An owner with only non-VS (domain) children declares no orphan wave -> the gate is vacuously open.
func TestOrphanLinkGate_NoVSLeavesNotGated(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	owner := &storagev1alpha1.Snapshot{ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "owner"}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp, owner).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	obj := orphanGateContent(t, orphanGateOpts{name: "root", mcpName: "mcp-ok", ownerNS: "ns1", ownerName: "owner"})
	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, obj)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.readyStatus != metav1.ConditionTrue {
		t.Fatalf("no-VS-leaves ready=%s/%s, want True", plan.readyStatus, plan.readyReason)
	}
}

// A declared VS leaf whose VolumeSnapshot object does not exist (reconstructed import/restore ref, no live
// orphan wave) is SKIPPED, not fail-closed -> the gate stays open.
func TestOrphanLinkGate_MissingVSSkipped(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	owner := ownerSnapshotWithVSLeaf("ns1", "owner", "vs-gone")
	// VolumeSnapshot vs-gone intentionally absent.
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp, owner).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	obj := orphanGateContent(t, orphanGateOpts{name: "root", mcpName: "mcp-ok", ownerNS: "ns1", ownerName: "owner"})
	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, obj)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.readyStatus != metav1.ConditionTrue {
		t.Fatalf("missing-VS ready=%s/%s, want True (skip reconstructed leaf)", plan.readyStatus, plan.readyReason)
	}
}

// Upgrade-guard / monotonicity: a root whose Ready is ALREADY True is not re-gated even with an unlinked
// orphan leaf, so Ready stays True (no True->False flap).
func TestOrphanLinkGate_UpgradeGuardKeepsAlreadyReadyRoot(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	owner := ownerSnapshotWithVSLeaf("ns1", "owner", "vs-a")
	vs := orphanVS("ns1", "vs-a", types.UID("vs-a-uid"))
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp, owner, vs).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	obj := orphanGateContent(t, orphanGateOpts{
		name: "root", mcpName: "mcp-ok", ownerNS: "ns1", ownerName: "owner", readyTrue: true,
	})
	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, obj)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.readyStatus != metav1.ConditionTrue {
		t.Fatalf("upgrade-guard ready=%s/%s, want True (no True->False flap)", plan.readyStatus, plan.readyReason)
	}
}

// Priority: a pending LINKED child outranks the orphan-link gate (children-pending wins over link-pending).
func TestOrphanLinkGate_PendingChildWinsOverLinkGate(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	owner := ownerSnapshotWithVSLeaf("ns1", "owner", "vs-a")
	vs := orphanVS("ns1", "vs-a", types.UID("vs-a-uid"))
	pendingChild := contentWithReadyCond("child-pending", metav1.ConditionFalse, snapshot.ReasonArtifactNotReady, "vsc not readyToUse")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp, owner, vs, pendingChild).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	obj := orphanGateContent(t, orphanGateOpts{
		name: "root", mcpName: "mcp-ok", ownerNS: "ns1", ownerName: "owner",
		childRefs: []string{"child-pending"},
	})
	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, obj)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.readyStatus != metav1.ConditionFalse || plan.readyReason != snapshot.ReasonChildrenPending {
		t.Fatalf("ready=%s/%s, want False/%s (pending child outranks orphan-link gate)", plan.readyStatus, plan.readyReason, snapshot.ReasonChildrenPending)
	}
}
