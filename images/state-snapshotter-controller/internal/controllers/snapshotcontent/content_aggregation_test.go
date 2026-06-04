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

// RequestsReady=True, no children -> Ready=True/Completed.
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
	if plan.requestsReady != metav1.ConditionTrue {
		t.Fatalf("requestsReady = %s, want True", plan.requestsReady)
	}
	if plan.childrenReady != metav1.ConditionTrue {
		t.Fatalf("childrenReady = %s, want True", plan.childrenReady)
	}
	if plan.readyStatus != metav1.ConditionTrue || plan.readyReason != snapshot.ReasonCompleted {
		t.Fatalf("ready = %s/%s, want True/%s", plan.readyStatus, plan.readyReason, snapshot.ReasonCompleted)
	}
}

// RequestsReady=False (pending, no MCP name), ChildrenReady=True -> Ready=False/ManifestCapturePending.
func TestContentPlanRequestsPending(t *testing.T) {
	ctx := context.Background()
	cl := fake.NewClientBuilder().WithScheme(aggScheme(t)).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, commonContentWithStatus("c", ""))
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.requestsReady != metav1.ConditionFalse || plan.requestsFailed {
		t.Fatalf("requestsReady=%s failed=%v, want False/non-terminal", plan.requestsReady, plan.requestsFailed)
	}
	if plan.childrenReady != metav1.ConditionTrue {
		t.Fatalf("childrenReady=%s, want True", plan.childrenReady)
	}
	if plan.readyStatus != metav1.ConditionFalse || plan.readyReason != snapshot.ReasonManifestCapturePending {
		t.Fatalf("ready=%s/%s, want False/%s", plan.readyStatus, plan.readyReason, snapshot.ReasonManifestCapturePending)
	}
}

// RequestsReady=True, ChildrenReady=False (pending child) -> Ready=False/ChildSnapshotPending.
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
	if plan.requestsReady != metav1.ConditionTrue {
		t.Fatalf("requestsReady=%s, want True", plan.requestsReady)
	}
	if plan.childrenReady != metav1.ConditionFalse || plan.childrenFailed {
		t.Fatalf("childrenReady=%s failed=%v, want False/non-terminal", plan.childrenReady, plan.childrenFailed)
	}
	if plan.readyStatus != metav1.ConditionFalse || plan.readyReason != snapshot.ReasonChildSnapshotPending {
		t.Fatalf("ready=%s/%s, want False/%s", plan.readyStatus, plan.readyReason, snapshot.ReasonChildSnapshotPending)
	}
}

// RequestsReady=True, ChildrenReady=False (terminal child) -> Ready=False/ChildSnapshotFailed.
func TestContentPlanChildrenFailed(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	brokenChild := contentWithReadyCond("child-broken", metav1.ConditionFalse, reasonManifestCheckpointFailed, "mcp-child missing")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp, brokenChild).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, commonContentWithStatus("c", "mcp-ok", "child-broken"))
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.childrenReady != metav1.ConditionFalse || !plan.childrenFailed {
		t.Fatalf("childrenReady=%s failed=%v, want False/terminal", plan.childrenReady, plan.childrenFailed)
	}
	if plan.readyStatus != metav1.ConditionFalse || plan.readyReason != snapshot.ReasonChildSnapshotFailed {
		t.Fatalf("ready=%s/%s, want False/%s", plan.readyStatus, plan.readyReason, snapshot.ReasonChildSnapshotFailed)
	}
	if !strings.Contains(plan.readyMessage, "child-broken") {
		t.Fatalf("ready message %q must name the failed child", plan.readyMessage)
	}
}

// Priority: RequestsFailed (terminal) wins over ChildrenFailed.
func TestContentPlanReadyPriorityRequestsFailedWins(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-bad", metav1.ConditionFalse, ssv1alpha1.ManifestCheckpointConditionReasonFailed, "capture failed")
	brokenChild := contentWithReadyCond("child-broken", metav1.ConditionFalse, reasonManifestCheckpointFailed, "mcp-child missing")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp, brokenChild).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, commonContentWithStatus("c", "mcp-bad", "child-broken"))
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.requestsReady != metav1.ConditionFalse || !plan.requestsFailed {
		t.Fatalf("requestsReady=%s failed=%v, want False/terminal", plan.requestsReady, plan.requestsFailed)
	}
	if plan.childrenReady != metav1.ConditionFalse || !plan.childrenFailed {
		t.Fatalf("childrenReady=%s failed=%v, want False/terminal", plan.childrenReady, plan.childrenFailed)
	}
	if plan.readyStatus != metav1.ConditionFalse || plan.readyReason != reasonManifestCheckpointFailed {
		t.Fatalf("ready=%s/%s, want False/%s (requests-failed wins)", plan.readyStatus, plan.readyReason, reasonManifestCheckpointFailed)
	}
}

// Priority: ChildrenFailed (terminal) wins over RequestsPending.
func TestContentPlanReadyPriorityChildrenFailedOverRequestsPending(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	brokenChild := contentWithReadyCond("child-broken", metav1.ConditionFalse, reasonManifestCheckpointFailed, "mcp-child missing")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(brokenChild).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	// No manifestCheckpointName -> requests pending (non-terminal); a terminal child must still surface.
	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, commonContentWithStatus("c", "", "child-broken"))
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.requestsReady != metav1.ConditionFalse || plan.requestsFailed {
		t.Fatalf("requestsReady=%s failed=%v, want False/non-terminal", plan.requestsReady, plan.requestsFailed)
	}
	if plan.readyStatus != metav1.ConditionFalse || plan.readyReason != snapshot.ReasonChildSnapshotFailed {
		t.Fatalf("ready=%s/%s, want False/%s (children-failed wins over requests-pending)", plan.readyStatus, plan.readyReason, snapshot.ReasonChildSnapshotFailed)
	}
}

// reconcileCommonSnapshotContentStatus publishes all three conditions (RequestsReady/ChildrenReady/Ready)
// gen-gated on a real status update.
func TestReconcileCommonStatusPublishesAllThreeConditions(t *testing.T) {
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
	for _, ct := range []string{snapshot.ConditionRequestsReady, snapshot.ConditionChildrenReady, snapshot.ConditionReady} {
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
