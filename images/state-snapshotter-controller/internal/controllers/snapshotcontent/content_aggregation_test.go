package snapshotcontent

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
)

func aggScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}
	if err := ssv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add ss scheme: %v", err)
	}
	return scheme
}

// commonContentWithStatus builds a common (cluster-scoped) SnapshotContent with an optional
// manifestCheckpointName and optional child content refs.
func commonContentWithStatus(name, mcpName string, childNames ...string) *unstructured.Unstructured {
	status := map[string]interface{}{}
	if mcpName != "" {
		status["manifestCheckpointName"] = mcpName
	}
	if len(childNames) > 0 {
		refs := make([]interface{}, 0, len(childNames))
		for _, cn := range childNames {
			refs = append(refs, map[string]interface{}{"name": cn})
		}
		status["childrenSnapshotContentRefs"] = refs
	}
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": storagev1alpha1.SchemeGroupVersion.String(),
		"kind":       "SnapshotContent",
		"metadata":   map[string]interface{}{"name": name},
		"status":     status,
	}}
	obj.SetGroupVersionKind(unifiedbootstrap.CommonSnapshotContentGVK())
	return obj
}

func manifestCheckpointWithReady(name string, status metav1.ConditionStatus, reason, message string) *ssv1alpha1.ManifestCheckpoint {
	mcp := &ssv1alpha1.ManifestCheckpoint{}
	mcp.Name = name
	meta.SetStatusCondition(&mcp.Status.Conditions, metav1.Condition{
		Type:    ssv1alpha1.ManifestCheckpointConditionTypeReady,
		Status:  status,
		Reason:  reason,
		Message: message,
	})
	return mcp
}

// MCP ready, no data refs, no children -> ManifestsReady=True, VolumesReady=True, Ready=True/Completed.
func TestContentPlanAllReadyNoChildren(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, commonContentWithStatus("c", "mcp-ok"))
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.manifestsReady != metav1.ConditionTrue {
		t.Fatalf("manifestsReady = %s, want True", plan.manifestsReady)
	}
	if plan.volumesReady != metav1.ConditionTrue {
		t.Fatalf("volumesReady = %s, want True (no data refs)", plan.volumesReady)
	}
	if plan.childrenReady != metav1.ConditionTrue {
		t.Fatalf("childrenReady = %s, want True", plan.childrenReady)
	}
	if plan.readyStatus != metav1.ConditionTrue || plan.readyReason != snapshot.ReasonCompleted {
		t.Fatalf("ready = %s/%s, want True/%s", plan.readyStatus, plan.readyReason, snapshot.ReasonCompleted)
	}
}

// ManifestsReady=False (pending, no MCP name), ChildrenReady=True -> Ready=False/ManifestCapturePending.
// While the manifest leg is not ready the volume leg is Unknown/ManifestCapturePending (not evaluated).
func TestContentPlanManifestsPending(t *testing.T) {
	ctx := context.Background()
	cl := fake.NewClientBuilder().WithScheme(aggScheme(t)).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, commonContentWithStatus("c", ""))
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.manifestsReady != metav1.ConditionFalse || plan.manifestsFailed {
		t.Fatalf("manifestsReady=%s failed=%v, want False/non-terminal", plan.manifestsReady, plan.manifestsFailed)
	}
	if plan.volumesReady != metav1.ConditionUnknown || plan.volumesReason != snapshot.ReasonManifestCapturePending {
		t.Fatalf("volumesReady=%s/%s, want Unknown/%s", plan.volumesReady, plan.volumesReason, snapshot.ReasonManifestCapturePending)
	}
	if plan.childrenReady != metav1.ConditionTrue {
		t.Fatalf("childrenReady=%s, want True", plan.childrenReady)
	}
	if plan.readyStatus != metav1.ConditionFalse || plan.readyReason != snapshot.ReasonManifestCapturePending {
		t.Fatalf("ready=%s/%s, want False/%s", plan.readyStatus, plan.readyReason, snapshot.ReasonManifestCapturePending)
	}
}

// ManifestsReady=True, VolumesReady=True, ChildrenReady=False (pending child) -> Ready=False/ChildrenPending.
func TestContentPlanChildrenPending(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	pendingChild := contentWithReadyCond("child-pending", metav1.ConditionFalse, snapshot.ReasonArtifactNotReady, "vsc not readyToUse")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp, pendingChild).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, commonContentWithStatus("c", "mcp-ok", "child-pending"))
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.manifestsReady != metav1.ConditionTrue {
		t.Fatalf("manifestsReady=%s, want True", plan.manifestsReady)
	}
	if plan.volumesReady != metav1.ConditionTrue {
		t.Fatalf("volumesReady=%s, want True", plan.volumesReady)
	}
	if plan.childrenReady != metav1.ConditionFalse || plan.childrenFailed {
		t.Fatalf("childrenReady=%s failed=%v, want False/non-terminal", plan.childrenReady, plan.childrenFailed)
	}
	if plan.readyStatus != metav1.ConditionFalse || plan.readyReason != snapshot.ReasonChildrenPending {
		t.Fatalf("ready=%s/%s, want False/%s", plan.readyStatus, plan.readyReason, snapshot.ReasonChildrenPending)
	}
}

// ManifestsReady=True, VolumesReady=True, ChildrenReady=False (terminal child) -> Ready=False/ChildrenFailed.
func TestContentPlanChildrenFailed(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	brokenChild := contentWithReadyCond("child-broken", metav1.ConditionFalse, snapshot.ReasonManifestCheckpointFailed, "mcp-child missing")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp, brokenChild).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, commonContentWithStatus("c", "mcp-ok", "child-broken"))
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.childrenReady != metav1.ConditionFalse || !plan.childrenFailed {
		t.Fatalf("childrenReady=%s failed=%v, want False/terminal", plan.childrenReady, plan.childrenFailed)
	}
	if plan.readyStatus != metav1.ConditionFalse || plan.readyReason != snapshot.ReasonChildrenFailed {
		t.Fatalf("ready=%s/%s, want False/%s", plan.readyStatus, plan.readyReason, snapshot.ReasonChildrenFailed)
	}
	if !strings.Contains(plan.readyMessage, "child-broken") {
		t.Fatalf("ready message %q must name the failed child", plan.readyMessage)
	}
}

// Priority: a terminal manifest failure wins over ChildrenFailed.
func TestContentPlanReadyPriorityManifestsFailedWins(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-bad", metav1.ConditionFalse, ssv1alpha1.ManifestCheckpointConditionReasonFailed, "capture failed")
	brokenChild := contentWithReadyCond("child-broken", metav1.ConditionFalse, snapshot.ReasonManifestCheckpointFailed, "mcp-child missing")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp, brokenChild).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, commonContentWithStatus("c", "mcp-bad", "child-broken"))
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.manifestsReady != metav1.ConditionFalse || !plan.manifestsFailed {
		t.Fatalf("manifestsReady=%s failed=%v, want False/terminal", plan.manifestsReady, plan.manifestsFailed)
	}
	if plan.childrenReady != metav1.ConditionFalse || !plan.childrenFailed {
		t.Fatalf("childrenReady=%s failed=%v, want False/terminal", plan.childrenReady, plan.childrenFailed)
	}
	if plan.readyStatus != metav1.ConditionFalse || plan.readyReason != snapshot.ReasonManifestCheckpointFailed {
		t.Fatalf("ready=%s/%s, want False/%s (manifests-failed wins)", plan.readyStatus, plan.readyReason, snapshot.ReasonManifestCheckpointFailed)
	}
}

// Priority: ChildrenFailed (terminal) wins over a pending manifest leg.
func TestContentPlanReadyPriorityChildrenFailedOverManifestsPending(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	brokenChild := contentWithReadyCond("child-broken", metav1.ConditionFalse, snapshot.ReasonManifestCheckpointFailed, "mcp-child missing")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(brokenChild).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	// No manifestCheckpointName -> manifest leg pending (non-terminal); a terminal child must still surface.
	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, commonContentWithStatus("c", "", "child-broken"))
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.manifestsReady != metav1.ConditionFalse || plan.manifestsFailed {
		t.Fatalf("manifestsReady=%s failed=%v, want False/non-terminal", plan.manifestsReady, plan.manifestsFailed)
	}
	if plan.readyStatus != metav1.ConditionFalse || plan.readyReason != snapshot.ReasonChildrenFailed {
		t.Fatalf("ready=%s/%s, want False/%s (children-failed wins over manifests-pending)", plan.readyStatus, plan.readyReason, snapshot.ReasonChildrenFailed)
	}
}

// reconcileCommonSnapshotContentStatus publishes all conditions (ManifestsReady/VolumesReady/ChildrenReady/Ready)
// gen-gated on a real status update.
func TestReconcileCommonStatusPublishesAllConditions(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	content := commonContentWithStatus("agg-content", "mcp-ok")
	content.SetGeneration(7)
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mcp, content).
		WithStatusSubresource(content).
		Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	ready, err := r.reconcileCommonSnapshotContentStatus(ctx, content)
	if err != nil {
		t.Fatalf("reconcile status: %v", err)
	}
	if !ready {
		t.Fatalf("expected ready=true")
	}

	fresh := &unstructured.Unstructured{}
	fresh.SetGroupVersionKind(unifiedbootstrap.CommonSnapshotContentGVK())
	if err := cl.Get(ctx, client.ObjectKey{Name: "agg-content"}, fresh); err != nil {
		t.Fatalf("get content: %v", err)
	}
	contentLike, err := snapshot.ExtractSnapshotContentLike(fresh)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	for _, ct := range []string{snapshot.ConditionManifestsReady, snapshot.ConditionVolumesReady, snapshot.ConditionChildrenReady, snapshot.ConditionReady} {
		cond := snapshot.GetCondition(contentLike, ct)
		if cond == nil {
			t.Fatalf("condition %s missing", ct)
		}
		if cond.Status != metav1.ConditionTrue {
			t.Fatalf("condition %s = %s, want True", ct, cond.Status)
		}
		if cond.ObservedGeneration != 7 {
			t.Fatalf("condition %s observedGeneration=%d, want 7", ct, cond.ObservedGeneration)
		}
	}
}

// A stale legacy RequestsReady condition left on a SnapshotContent must be pruned on reconcile so the
// object converges to the ManifestsReady/VolumesReady model (RequestsReady is never written anymore).
func TestReconcileCommonStatusPrunesLegacyRequestsReady(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	content := commonContentWithStatus("legacy-content", "mcp-ok")
	content.SetGeneration(3)
	// Seed a stale RequestsReady condition that no current code path writes.
	if err := unstructured.SetNestedSlice(content.Object, []interface{}{
		map[string]interface{}{
			"type": "RequestsReady", "status": "True", "reason": "Completed",
			"message": "legacy", "lastTransitionTime": "2020-01-01T00:00:00Z",
		},
	}, "status", "conditions"); err != nil {
		t.Fatalf("seed conditions: %v", err)
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mcp, content).
		WithStatusSubresource(content).
		Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	if _, err := r.reconcileCommonSnapshotContentStatus(ctx, content); err != nil {
		t.Fatalf("reconcile status: %v", err)
	}

	fresh := &unstructured.Unstructured{}
	fresh.SetGroupVersionKind(unifiedbootstrap.CommonSnapshotContentGVK())
	if err := cl.Get(ctx, client.ObjectKey{Name: "legacy-content"}, fresh); err != nil {
		t.Fatalf("get content: %v", err)
	}
	contentLike, err := snapshot.ExtractSnapshotContentLike(fresh)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if c := snapshot.GetCondition(contentLike, "RequestsReady"); c != nil {
		t.Fatalf("legacy RequestsReady condition must be pruned, got %#v", c)
	}
	for _, ct := range []string{snapshot.ConditionManifestsReady, snapshot.ConditionVolumesReady, snapshot.ConditionChildrenReady, snapshot.ConditionReady} {
		if snapshot.GetCondition(contentLike, ct) == nil {
			t.Fatalf("condition %s must exist after reconcile", ct)
		}
	}
}
