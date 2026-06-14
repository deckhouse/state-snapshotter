package snapshotcontent

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
)

// commonContentReadyWithMCPAndDataRefs builds a common SnapshotContent that already carries Ready=True
// conditions and references a ready MCP plus a single VSC data artifact. It models a node that was
// previously Ready=True; the aggregation recompute does not depend on the stored conditions.
func commonContentReadyWithMCPAndDataRefs(name, mcpName, vscName string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": storagev1alpha1.SchemeGroupVersion.String(),
		"kind":       "SnapshotContent",
		"metadata":   map[string]interface{}{"name": name},
		"status": map[string]interface{}{
			"manifestCheckpointName": mcpName,
			"dataRefs": []interface{}{
				map[string]interface{}{
					"targetUID": "pvc-1",
					"target": map[string]interface{}{
						"apiVersion": "v1", "kind": "PersistentVolumeClaim", "name": "pvc-1", "namespace": "default",
					},
					"artifact": map[string]interface{}{
						"apiVersion": volumeSnapshotContentAPIVersion,
						"kind":       kindVolumeSnapshotContent,
						"name":       vscName,
					},
				},
			},
			"conditions": []interface{}{
				map[string]interface{}{"type": snapshot.ConditionReady, "status": "True", "reason": snapshot.ReasonCompleted, "message": "manifest, data, and child content are ready"},
			},
		},
	}}
	obj.SetGroupVersionKind(unifiedbootstrap.CommonSnapshotContentGVK())
	return obj
}

// Phase 1 revalidation-without-watch: a SnapshotContent that was Ready=True must, on a later reconcile
// that observes the published data artifact missing, recompute VolumesReady=False / Ready=False with
// reason ArtifactMissing and the artifact kind/name in the message. No watch is involved. The manifest
// leg stays Ready=True (the failure is on the volume leg only).
func TestContentPlanAlreadyReadyThenArtifactMissing(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	// VSC "vsc-gone" intentionally not created -> missing published artifact.
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	content := commonContentReadyWithMCPAndDataRefs("c", "mcp-ok", "vsc-gone")
	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, content)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.manifestsReady != metav1.ConditionTrue {
		t.Fatalf("manifestsReady=%s, want True (manifest leg unaffected)", plan.manifestsReady)
	}
	if plan.volumesReady != metav1.ConditionFalse || !plan.volumesFailed {
		t.Fatalf("volumesReady=%s failed=%v, want False/terminal", plan.volumesReady, plan.volumesFailed)
	}
	if plan.readyStatus != metav1.ConditionFalse || plan.readyReason != snapshot.ReasonArtifactMissing {
		t.Fatalf("ready=%s/%s, want False/%s", plan.readyStatus, plan.readyReason, snapshot.ReasonArtifactMissing)
	}
	if !strings.Contains(plan.readyMessage, "vsc-gone") {
		t.Fatalf("ready message %q must name the missing artifact", plan.readyMessage)
	}
}

// Data leg surfaces DataCapturePending with a "<ready>/<total> ready" progress count.
func TestContentPlanDataCapturePendingProgress(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	vscReady := volumeSnapshotContentObject("vsc-ready", true)
	vscPending := volumeSnapshotContentObject("vsc-pending", false)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp, vscReady, vscPending).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	content := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": storagev1alpha1.SchemeGroupVersion.String(),
		"kind":       "SnapshotContent",
		"metadata":   map[string]interface{}{"name": "c"},
		"status": map[string]interface{}{
			"manifestCheckpointName": "mcp-ok",
			"dataRefs": []interface{}{
				dataRefEntry("pvc-1", "vsc-ready"),
				dataRefEntry("pvc-2", "vsc-pending"),
			},
		},
	}}
	content.SetGroupVersionKind(unifiedbootstrap.CommonSnapshotContentGVK())

	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, content)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.manifestsReady != metav1.ConditionTrue {
		t.Fatalf("manifestsReady=%s, want True (MCP ready)", plan.manifestsReady)
	}
	if plan.volumesReady != metav1.ConditionFalse || plan.volumesFailed {
		t.Fatalf("volumesReady=%s failed=%v, want False/non-terminal", plan.volumesReady, plan.volumesFailed)
	}
	if plan.readyStatus != metav1.ConditionFalse || plan.readyReason != snapshot.ReasonDataCapturePending {
		t.Fatalf("ready=%s/%s, want False/%s", plan.readyStatus, plan.readyReason, snapshot.ReasonDataCapturePending)
	}
	if !strings.Contains(plan.readyMessage, "1/2 ready") {
		t.Fatalf("ready message %q must carry progress count", plan.readyMessage)
	}
}

// Leaf-chain diagnostics: a 3-level tree (root -> vm-content -> disk-content) yields a root ChildrenFailed
// message that names the direct child (vm-content) and pins the original failed leaf (disk-content) with
// its original reason/message, in the canonical parseable form.
func TestChildrenFailedLeafChainThreeLevels(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	leafMsg := "VolumeSnapshotContent/snap-abc for target pvc-1 is missing"
	diskContent := contentWithReadyCond("disk-content", metav1.ConditionFalse, snapshot.ReasonArtifactMissing, leafMsg)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(diskContent).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	// vm-content aggregates the failed disk leaf.
	vm := parentContentWithChildRefs("vm-content", "disk-content")
	_, reason, vmMsg, err := r.validateCommonContentChildren(ctx, vm)
	if err != nil {
		t.Fatalf("validate vm-content: %v", err)
	}
	if reason != snapshot.ReasonChildrenFailed {
		t.Fatalf("vm-content reason=%s, want ChildrenFailed", reason)
	}
	// Persist vm-content Ready=False/ChildrenFailed with its cumulative message.
	if err := cl.Create(ctx, contentWithReadyCond("vm-content", metav1.ConditionFalse, reason, vmMsg)); err != nil {
		t.Fatalf("create vm-content: %v", err)
	}

	root := parentContentWithChildRefs("root", "vm-content")
	_, reason, rootMsg, err := r.validateCommonContentChildren(ctx, root)
	if err != nil {
		t.Fatalf("validate root: %v", err)
	}
	if reason != snapshot.ReasonChildrenFailed {
		t.Fatalf("root reason=%s, want ChildrenFailed", reason)
	}
	want := "child SnapshotContent vm-content failed: leaf=disk-content reason=ArtifactMissing message=" + leafMsg
	if rootMsg != want {
		t.Fatalf("root message mismatch:\n got: %q\nwant: %q", rootMsg, want)
	}
}

// ChildrenReady=True message must reflect the actual node state: a leaf with no children says
// "no child content"; a parent with all children ready says "<ready>/<total> child content ready"
// (not a generic ambiguous phrase).
func TestChildrenReadySuccessMessageReflectsState(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	childA := contentWithReadyCond("child-a", metav1.ConditionTrue, snapshot.ReasonCompleted, "ready")
	childB := contentWithReadyCond("child-b", metav1.ConditionTrue, snapshot.ReasonCompleted, "ready")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(childA, childB).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	leaf := parentContentWithChildRefs("leaf")
	ready, _, msg, err := r.validateCommonContentChildren(ctx, leaf)
	if err != nil {
		t.Fatalf("validate leaf: %v", err)
	}
	if !ready || msg != "no child content" {
		t.Fatalf("leaf children: ready=%v msg=%q, want true/%q", ready, msg, "no child content")
	}

	parent := parentContentWithChildRefs("parent", "child-a", "child-b")
	ready, _, msg, err = r.validateCommonContentChildren(ctx, parent)
	if err != nil {
		t.Fatalf("validate parent: %v", err)
	}
	if !ready || msg != "2/2 child content ready" {
		t.Fatalf("parent children: ready=%v msg=%q, want true/%q", ready, msg, "2/2 child content ready")
	}
}

func dataRefEntry(targetUID, vscName string) map[string]interface{} {
	return map[string]interface{}{
		"targetUID": targetUID,
		"target": map[string]interface{}{
			"apiVersion": "v1", "kind": "PersistentVolumeClaim", "name": targetUID, "namespace": "default",
		},
		"artifact": map[string]interface{}{
			"apiVersion": volumeSnapshotContentAPIVersion,
			"kind":       kindVolumeSnapshotContent,
			"name":       vscName,
		},
	}
}
