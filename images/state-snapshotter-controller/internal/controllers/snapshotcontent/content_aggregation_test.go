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

// contentWithSnapshotRef builds a common SnapshotContent unstructured carrying spec.snapshotRef (the
// binding-subject back-reference used by the declared-children fail-closed check) plus optional
// manifestCheckpointName and published child content edges.
func contentWithSnapshotRef(name, mcpName, ownerName string, childContentNames ...string) *unstructured.Unstructured {
	status := map[string]interface{}{}
	if mcpName != "" {
		status["manifestCheckpointName"] = mcpName
	}
	if len(childContentNames) > 0 {
		refs := make([]interface{}, 0, len(childContentNames))
		for _, cn := range childContentNames {
			refs = append(refs, map[string]interface{}{"name": cn})
		}
		status["childrenSnapshotContentRefs"] = refs
	}
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": storagev1alpha1.SchemeGroupVersion.String(),
		"kind":       "SnapshotContent",
		"metadata":   map[string]interface{}{"name": name},
		"spec": map[string]interface{}{
			"snapshotRef": map[string]interface{}{
				"apiVersion": storagev1alpha1.SchemeGroupVersion.String(),
				"kind":       "Snapshot",
				"name":       ownerName,
				"namespace":  "ns1",
			},
		},
		"status": status,
	}}
	obj.SetGroupVersionKind(unifiedbootstrap.CommonSnapshotContentGVK())
	return obj
}

// ownerSnapshotWithChildren builds a typed Snapshot that declares the given child Snapshots in
// status.childrenSnapshotRefs (strict storage Snapshot refs).
func ownerSnapshotWithChildren(name string, childSnapshotNames ...string) *storagev1alpha1.Snapshot {
	refs := make([]storagev1alpha1.SnapshotChildRef, 0, len(childSnapshotNames))
	for _, cn := range childSnapshotNames {
		refs = append(refs, storagev1alpha1.SnapshotChildRef{
			APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
			Kind:       "Snapshot",
			Name:       cn,
		})
	}
	return &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: name},
		Status:     storagev1alpha1.SnapshotStatus{ChildrenSnapshotRefs: refs},
	}
}

// boundChildSnapshot builds a typed child Snapshot already bound to a content name.
func boundChildSnapshot(boundContentName string) *storagev1alpha1.Snapshot {
	return &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "child-snap"},
		Status:     storagev1alpha1.SnapshotStatus{BoundSnapshotContentName: boundContentName},
	}
}

// contentWithSubtreeManifestsPersisted builds a typed SnapshotContent carrying the core-internal
// status.subtreeManifestsPersisted latch (the successor of the former ManifestsArchived condition),
// read back as a child content under CommonSnapshotContentGVK.
func contentWithSubtreeManifestsPersisted(name string, persisted bool) *storagev1alpha1.SnapshotContent {
	return &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     storagev1alpha1.SnapshotContentStatus{SubtreeManifestsPersisted: persisted},
	}
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

// MCP ready, no data refs, no children -> ManifestsReady=True, DataReady=True, Ready=True/Completed.
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
	if plan.volumeReady != metav1.ConditionTrue {
		t.Fatalf("volumeReady = %s, want True (no data refs)", plan.volumeReady)
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
	if plan.volumeReady != metav1.ConditionUnknown || plan.volumeReason != snapshot.ReasonManifestCapturePending {
		t.Fatalf("volumeReady=%s/%s, want Unknown/%s", plan.volumeReady, plan.volumeReason, snapshot.ReasonManifestCapturePending)
	}
	if plan.childrenReady != metav1.ConditionTrue {
		t.Fatalf("childrenReady=%s, want True", plan.childrenReady)
	}
	if plan.readyStatus != metav1.ConditionFalse || plan.readyReason != snapshot.ReasonManifestCapturePending {
		t.Fatalf("ready=%s/%s, want False/%s", plan.readyStatus, plan.readyReason, snapshot.ReasonManifestCapturePending)
	}
}

// ManifestsReady=True, DataReady=True, ChildrenReady=False (pending child) -> Ready=False/ChildrenPending.
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
	if plan.volumeReady != metav1.ConditionTrue {
		t.Fatalf("volumeReady=%s, want True", plan.volumeReady)
	}
	if plan.childrenReady != metav1.ConditionFalse || plan.childrenFailed {
		t.Fatalf("childrenReady=%s failed=%v, want False/non-terminal", plan.childrenReady, plan.childrenFailed)
	}
	if plan.readyStatus != metav1.ConditionFalse || plan.readyReason != snapshot.ReasonChildrenPending {
		t.Fatalf("ready=%s/%s, want False/%s", plan.readyStatus, plan.readyReason, snapshot.ReasonChildrenPending)
	}
}

// ManifestsReady=True, DataReady=True, ChildrenReady=False (terminal child) -> Ready=False/ChildrenFailed.
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

// subtreeManifestsPersisted must NOT latch True while the owning snapshot declares a child that is not yet
// linked into status.childrenSnapshotContentRefs, even when this node's own manifest leg is ready. This is
// the fail-closed guard against premature subtree-latch (root cause of the 409 duplicate root capture).
func TestComputeSubtreeManifestsPersisted_DeclaredButUnlinkedChildPends(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	owner := ownerSnapshotWithChildren("owner", "child-snap")
	childSnap := boundChildSnapshot("child-content")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp, owner, childSnap).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	// Own MCP ready, but the declared child (child-snap -> child-content) is not in childrenSnapshotContentRefs.
	parent := contentWithSnapshotRef("parent-content", "mcp-ok", "owner")
	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, parent)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.manifestsReady != metav1.ConditionTrue {
		t.Fatalf("manifestsReady=%s, want True (precondition)", plan.manifestsReady)
	}
	if plan.subtreeManifestsPersisted {
		t.Fatalf("subtreeManifestsPersisted must NOT be true while a declared child is unlinked")
	}
}

// Once the declared child is both linked into childrenSnapshotContentRefs AND its own
// subtreeManifestsPersisted latch is true, the node latches subtreeManifestsPersisted=true.
func TestComputeSubtreeManifestsPersisted_DeclaredChildLinkedAndPersistedLatches(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	owner := ownerSnapshotWithChildren("owner", "child-snap")
	childSnap := boundChildSnapshot("child-content")
	childContent := contentWithSubtreeManifestsPersisted("child-content", true)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp, owner, childSnap, childContent).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	parent := contentWithSnapshotRef("parent-content", "mcp-ok", "owner", "child-content")
	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, parent)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if !plan.subtreeManifestsPersisted {
		t.Fatalf("subtreeManifestsPersisted=%v, want true", plan.subtreeManifestsPersisted)
	}
}

// A leaf node (owning snapshot declares no children) latches subtreeManifestsPersisted=true from its own
// MCP, even with spec.snapshotRef set (no regression for the common no-children case).
func TestComputeSubtreeManifestsPersisted_LeafLatchesWithSnapshotRef(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	owner := ownerSnapshotWithChildren("owner") // no declared children
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp, owner).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	parent := contentWithSnapshotRef("leaf-content", "mcp-ok", "owner")
	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, parent)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if !plan.subtreeManifestsPersisted {
		t.Fatalf("leaf subtreeManifestsPersisted=%v, want true", plan.subtreeManifestsPersisted)
	}
}

// reconcileCommonSnapshotContentStatus publishes all conditions (ManifestsReady/DataReady/ChildrenReady/Ready)
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

	ready, err := r.reconcileCommonSnapshotContentStatus(ctx, content, false, "", "", nil)
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
	for _, ct := range []string{snapshot.ConditionManifestsReady, snapshot.ConditionDataReady, snapshot.ConditionChildrenReady, snapshot.ConditionReady} {
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

	// The former ManifestsArchived condition was replaced by the core-internal status.subtreeManifestsPersisted
	// bool latch; it must be persisted true here and must NOT resurface as a condition.
	if persisted, _, _ := unstructured.NestedBool(fresh.Object, "status", "subtreeManifestsPersisted"); !persisted {
		t.Fatalf("status.subtreeManifestsPersisted = false, want true")
	}
	if cond := snapshot.GetCondition(contentLike, "ManifestsArchived"); cond != nil {
		t.Fatalf("legacy ManifestsArchived condition must no longer be published, got %#v", cond)
	}
}

// Ready must stay False while the subtree-persist latch is still Capturing (here: a linked, Ready child
// whose OWN subtree is not yet persisted), EVEN THOUGH the live legs (ManifestsReady / DataReady /
// ChildrenReady) are all satisfied. The subtree-persist gate is the lowest-priority Ready gate, so the
// first Ready=True is blocked until the whole subtree's manifests are archived. The resulting !ready is
// what keeps the Reconcile loop requeuing until the archive wave converges.
func TestReconcileCommonStatusNotReadyWhileArchivePending(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	owner := ownerSnapshotWithChildren("owner", "child-snap")
	childSnap := boundChildSnapshot("child-content")
	// The declared child IS linked into childrenSnapshotContentRefs and IS Ready (so the ChildrenReady
	// read barrier is satisfied — see validateCommonContentChildren), but the child's own subtree is not yet
	// persisted (subtreeManifestsPersisted=false). Own MCP ready + linked+Ready child => own legs and
	// ChildrenReady are True, yet the subtree-persist latch is still Capturing -> the (lowest-priority)
	// archive gate must hold Ready=False on SubtreeManifestCapturePending. This isolates the archive gate
	// from the read barrier.
	childContent := contentWithReadyCond("child-content", metav1.ConditionTrue, snapshot.ReasonCompleted, "ready")
	parent := contentWithSnapshotRef("parent-content", "mcp-ok", "owner", "child-content")
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mcp, owner, childSnap, childContent, parent).
		WithStatusSubresource(parent).
		Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	ready, err := r.reconcileCommonSnapshotContentStatus(ctx, parent, false, "", "", nil)
	if err != nil {
		t.Fatalf("reconcile status: %v", err)
	}
	if ready {
		t.Fatalf("expected ready=false while a declared child is unlinked (archive wave still pending)")
	}

	fresh := &unstructured.Unstructured{}
	fresh.SetGroupVersionKind(unifiedbootstrap.CommonSnapshotContentGVK())
	if err := cl.Get(ctx, client.ObjectKey{Name: "parent-content"}, fresh); err != nil {
		t.Fatalf("get content: %v", err)
	}
	contentLike, err := snapshot.ExtractSnapshotContentLike(fresh)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	readyCond := snapshot.GetCondition(contentLike, snapshot.ConditionReady)
	if readyCond == nil || readyCond.Status != metav1.ConditionFalse {
		t.Fatalf("Ready = %#v, want False", readyCond)
	}
	if readyCond.Reason != snapshot.ReasonSubtreeManifestCapturePending {
		t.Fatalf("Ready reason = %q, want %q (subtree-persist gate)", readyCond.Reason, snapshot.ReasonSubtreeManifestCapturePending)
	}
}

// A terminal child-content failure must propagate up the tree as a ChildrenFailed: a child whose subtree
// can never be captured makes the parent terminally failed too. The subtree-persist pending reason is
// transient (pending), not terminal. This guards the terminal-reason set against a future Ready-priority
// change that could let a transient child surface as a terminal failure on Ready.
func TestTerminalChildContentFailureClassification(t *testing.T) {
	terminal := []string{
		snapshot.ReasonManifestCheckpointFailed,
		snapshot.ReasonDataArtifactInvalid,
		snapshot.ReasonDataArtifactNotSupported,
		snapshot.ReasonArtifactMissing,
		snapshot.ReasonVolumeCaptureFailed,
		snapshot.ReasonDomainCaptureFailed,
		snapshot.ReasonChildrenFailed,
	}
	for _, reason := range terminal {
		if !isTerminalChildContentFailure(reason) {
			t.Fatalf("isTerminalChildContentFailure(%q) = false, want true", reason)
		}
	}
	if isTerminalChildContentFailure(snapshot.ReasonSubtreeManifestCapturePending) {
		t.Fatalf("isTerminalChildContentFailure(%q) = true, want false (transient)", snapshot.ReasonSubtreeManifestCapturePending)
	}
}

// Premature-Ready gate (content-single-writer §4 Slice 3 / §11.4): reconcileDataLegProjection is the single
// writer of status.data and publishes it via a SEPARATE status patch, so during a pass where the data leg is
// still converging (just published, or the VCR/VSC not ready) the in-memory content has an empty
// status.dataRefs that resolveDataReadiness treats as volume N/A (DataReady=True). For a content that HAS
// an expected data leg this must NOT let Ready escalate to True (and mirrorReadyToOwnerSnapshot propagate it)
// before the bound VolumeSnapshotContent's readyToUse is validated. reconcileCommonSnapshotContentStatus
// honours the dataLegPending signal by downgrading the stale-empty volume leg to a non-terminal
// DataCapturePending and re-deriving Ready; with the signal off the same content is manifest-only and stays
// Ready=True (N/A). This pins the Bugbot-flagged same-pass premature-Ready regression closed.
func TestReconcileCommonStatus_DataLegPendingGatesPrematureReady(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")

	build := func(name string) (*SnapshotContentController, *unstructured.Unstructured, client.Client) {
		content := commonContentWithStatus(name, "mcp-ok")
		cl := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(mcp.DeepCopy(), content).WithStatusSubresource(content).Build()
		return &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}, content, cl
	}

	condOf := func(t *testing.T, cl client.Client, name, condType string) *metav1.Condition {
		t.Helper()
		fresh := &unstructured.Unstructured{}
		fresh.SetGroupVersionKind(unifiedbootstrap.CommonSnapshotContentGVK())
		if err := cl.Get(ctx, client.ObjectKey{Name: name}, fresh); err != nil {
			t.Fatalf("get content: %v", err)
		}
		contentLike, err := snapshot.ExtractSnapshotContentLike(fresh)
		if err != nil {
			t.Fatalf("extract: %v", err)
		}
		return snapshot.GetCondition(contentLike, condType)
	}

	t.Run("dataLegPending downgrades the stale-empty volume leg and blocks Ready", func(t *testing.T) {
		r, content, cl := build("dlp-pending")
		ready, err := r.reconcileCommonSnapshotContentStatus(ctx, content, true, "", "", nil)
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if ready {
			t.Fatalf("Ready must be False while the data leg is still pending (empty status.data must not read as volume N/A)")
		}
		if vol := condOf(t, cl, "dlp-pending", snapshot.ConditionDataReady); vol == nil || vol.Status != metav1.ConditionFalse || vol.Reason != snapshot.ReasonDataCapturePending {
			t.Fatalf("DataReady = %#v, want False/%s", vol, snapshot.ReasonDataCapturePending)
		}
		if rd := condOf(t, cl, "dlp-pending", snapshot.ConditionReady); rd == nil || rd.Status != metav1.ConditionFalse || rd.Reason != snapshot.ReasonDataCapturePending {
			t.Fatalf("Ready = %#v, want False/%s", rd, snapshot.ReasonDataCapturePending)
		}
	})

	t.Run("no data leg keeps Ready=True (manifest-only, empty dataRefs are N/A)", func(t *testing.T) {
		r, content, cl := build("dlp-absent")
		ready, err := r.reconcileCommonSnapshotContentStatus(ctx, content, false, "", "", nil)
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if !ready {
			t.Fatalf("manifest-only content (no data leg) must stay Ready=True")
		}
		if vol := condOf(t, cl, "dlp-absent", snapshot.ConditionDataReady); vol == nil || vol.Status != metav1.ConditionTrue {
			t.Fatalf("DataReady = %#v, want True (N/A, no data refs)", vol)
		}
	})

	// vcr-watch-core-terminal (decision D2): a terminal data-leg reason (failed VCR / Variant-A fault)
	// makes the CONTENT itself terminal: DataReady=False/VolumeCaptureFailed and Ready=False/VolumeCaptureFailed
	// (terminal beats the pending downgrade). This is what lets the failure propagate up as ChildrenFailed.
	t.Run("dataLegTerminal makes the content terminal (VolumeCaptureFailed beats pending)", func(t *testing.T) {
		r, content, cl := build("dlp-terminal")
		ready, err := r.reconcileCommonSnapshotContentStatus(ctx, content, true, snapshot.ReasonVolumeCaptureFailed, "data-leg volume capture failed: csi failed", nil)
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if ready {
			t.Fatalf("a terminal data leg must not be Ready")
		}
		if vol := condOf(t, cl, "dlp-terminal", snapshot.ConditionDataReady); vol == nil || vol.Status != metav1.ConditionFalse || vol.Reason != snapshot.ReasonVolumeCaptureFailed {
			t.Fatalf("DataReady = %#v, want False/%s", vol, snapshot.ReasonVolumeCaptureFailed)
		}
		if rd := condOf(t, cl, "dlp-terminal", snapshot.ConditionReady); rd == nil || rd.Status != metav1.ConditionFalse || rd.Reason != snapshot.ReasonVolumeCaptureFailed {
			t.Fatalf("Ready = %#v, want False/%s", rd, snapshot.ReasonVolumeCaptureFailed)
		}
	})
}
