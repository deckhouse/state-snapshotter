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
	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/snaphelpers"
	deckhousev1alpha1 "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/deckhouseio/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
)

// Parent teardown must ENSURE parent-protect on every live direct child and NEVER remove it: each child is a
// durable node that must run its own deletion handler once ownerRef GC reaches it. This is the corrected
// contract (the pre-fix code stripped the child finalizer here, letting a deeper descendant skip its
// handler). A child that already carries the finalizer keeps it (idempotent); a child missing it gains it.
func TestSnapshotContentControllerEnsuresChildFinalizersForCascade(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}

	childWith := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "child-with-finalizer",
			Finalizers: []string{snapshot.FinalizerParentProtect},
		},
	}
	childWithout := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Name: "child-without-finalizer",
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(childWith, childWithout).Build()
	r := &SnapshotContentController{
		Client:      cl,
		APIReader:   cl,
		GVKRegistry: snapshot.NewGVKRegistry(),
	}

	rootObj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": storagev1alpha1.SchemeGroupVersion.String(),
			"kind":       "SnapshotContent",
			"metadata": map[string]interface{}{
				"name": "root-content",
			},
			"status": map[string]interface{}{
				"childrenSnapshotContentRefs": []interface{}{
					map[string]interface{}{"name": "child-with-finalizer"},
					map[string]interface{}{"name": "child-without-finalizer"},
				},
			},
		},
	}
	rootObj.SetGroupVersionKind(unifiedbootstrap.CommonSnapshotContentGVK())
	contentLike, err := snapshot.ExtractSnapshotContentLike(rootObj)
	if err != nil {
		t.Fatalf("extract content like: %v", err)
	}

	if err := r.ensureFinalizersOnChildrenForCascade(ctx, contentLike, rootObj); err != nil {
		t.Fatalf("ensure child finalizers: %v", err)
	}

	for _, name := range []string{"child-with-finalizer", "child-without-finalizer"} {
		freshChild := &storagev1alpha1.SnapshotContent{}
		if err := cl.Get(ctx, client.ObjectKey{Name: name}, freshChild); err != nil {
			t.Fatalf("get child %s: %v", name, err)
		}
		if !snapshot.HasFinalizer(freshChild, snapshot.FinalizerParentProtect) {
			t.Fatalf("child %s must retain/gain parent-protect after cascade ensure: %v", name, freshChild.Finalizers)
		}
	}
}

func TestBuildCommonSnapshotContentStatusPlanUsesPersistedRefsOnly(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}

	content := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": storagev1alpha1.SchemeGroupVersion.String(),
		"kind":       "SnapshotContent",
		"metadata": map[string]interface{}{
			"name": "content",
		},
	}}
	content.SetGroupVersionKind(unifiedbootstrap.CommonSnapshotContentGVK())

	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, content)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.readyStatus != metav1.ConditionFalse || plan.readyReason != snapshot.ReasonManifestCapturePending {
		t.Fatalf("expected content pending on missing persisted manifest ref, got status=%s reason=%s", plan.readyStatus, plan.readyReason)
	}
}

func TestEnsureChildSnapshotContentOwnedByParentDoesNotStealConflictingOwner(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}
	child := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Name: "child-content",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
				Kind:       "SnapshotContent",
				Name:       "other-parent",
				UID:        "other-uid",
			}},
		},
	}
	parent := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": storagev1alpha1.SchemeGroupVersion.String(),
		"kind":       "SnapshotContent",
		"metadata": map[string]interface{}{
			"name": "parent-content",
			"uid":  "parent-uid",
		},
	}}
	parent.SetGroupVersionKind(unifiedbootstrap.CommonSnapshotContentGVK())
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(child).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	if err := r.ensureChildSnapshotContentOwnedByParent(ctx, "child-content", parent); err == nil {
		t.Fatal("expected conflicting child content ownerRef to fail closed")
	}
	fresh := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: "child-content"}, fresh); err != nil {
		t.Fatalf("get child content: %v", err)
	}
	if got := fresh.OwnerReferences[0].Name; got != "other-parent" {
		t.Fatalf("child content ownerRef was stolen: got owner %q", got)
	}
}

func TestEnsureChildSnapshotContentOwnedByParentRejectsSnapshotOwner(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}
	unrelated := metav1.OwnerReference{APIVersion: "example.io/v1", Kind: "AuditAnchor", Name: "audit"}
	child := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Name: "child-content",
			OwnerReferences: []metav1.OwnerReference{
				{APIVersion: "demo.test/v1", Kind: "DemoVirtualDiskSnapshot", Name: "child-snapshot", Controller: boolPtr(true)},
				unrelated,
			},
		},
	}
	parent := snapshotContentUnstructuredForOwnerTest("parent-content", "parent-uid")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(child).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	if err := r.ensureChildSnapshotContentOwnedByParent(ctx, "child-content", parent); err == nil {
		t.Fatal("expected child content ownerRef to Snapshot to fail closed")
	}
	fresh := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: "child-content"}, fresh); err != nil {
		t.Fatalf("get child content: %v", err)
	}
	assertHasOwnerRef(t, fresh.OwnerReferences, "demo.test/v1", "DemoVirtualDiskSnapshot", "child-snapshot", true)
	assertHasOwnerRef(t, fresh.OwnerReferences, unrelated.APIVersion, unrelated.Kind, unrelated.Name, false)
}

func TestEnsureChildSnapshotContentOwnedByParentPreservesUnrelatedRefs(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}
	unrelated := metav1.OwnerReference{APIVersion: "example.io/v1", Kind: "AuditAnchor", Name: "audit"}
	child := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "child-content",
			OwnerReferences: []metav1.OwnerReference{unrelated},
		},
	}
	parent := snapshotContentUnstructuredForOwnerTest("parent-content", "parent-uid")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(child).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	if err := r.ensureChildSnapshotContentOwnedByParent(ctx, "child-content", parent); err != nil {
		t.Fatalf("set child content ownerRef: %v", err)
	}
	fresh := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: "child-content"}, fresh); err != nil {
		t.Fatalf("get child content: %v", err)
	}
	assertHasOwnerRef(t, fresh.OwnerReferences, storagev1alpha1.SchemeGroupVersion.String(), "SnapshotContent", "parent-content", true)
	assertHasOwnerRef(t, fresh.OwnerReferences, unrelated.APIVersion, unrelated.Kind, unrelated.Name, false)
}

func TestEnsureManifestCheckpointOwnedByContentDoesNotStealConflictingContentOwner(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := ssv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add snapshotter scheme: %v", err)
	}
	mcp := &ssv1alpha1.ManifestCheckpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name: "mcp",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
				Kind:       "SnapshotContent",
				Name:       "other-content",
			}},
		},
	}
	content := snapshotContentUnstructuredForOwnerTest("content", "content-uid")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	if err := r.ensureManifestCheckpointOwnedByContent(ctx, "mcp", content); err == nil {
		t.Fatal("expected conflicting MCP SnapshotContent ownerRef to fail closed")
	}
	fresh := &ssv1alpha1.ManifestCheckpoint{}
	if err := cl.Get(ctx, client.ObjectKey{Name: "mcp"}, fresh); err != nil {
		t.Fatalf("get MCP: %v", err)
	}
	assertHasOwnerRef(t, fresh.OwnerReferences, storagev1alpha1.SchemeGroupVersion.String(), "SnapshotContent", "other-content", false)
	assertNoOwnerRef(t, fresh.OwnerReferences, storagev1alpha1.SchemeGroupVersion.String(), "SnapshotContent", "content")
}

func TestEnsureManifestCheckpointOwnedByContentHandoffFromObjectKeeper(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := ssv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add snapshotter scheme: %v", err)
	}
	unrelated := metav1.OwnerReference{APIVersion: "example.io/v1", Kind: "AuditAnchor", Name: "audit"}
	mcp := &ssv1alpha1.ManifestCheckpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name: "mcp",
			OwnerReferences: []metav1.OwnerReference{
				{APIVersion: controllercommon.DeckhouseAPIVersion, Kind: controllercommon.KindObjectKeeper, Name: "ret-mcr", Controller: boolPtr(true)},
				unrelated,
			},
		},
	}
	content := snapshotContentUnstructuredForOwnerTest("content", "content-uid")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	if err := r.ensureManifestCheckpointOwnedByContent(ctx, "mcp", content); err != nil {
		t.Fatalf("handoff MCP ownerRef: %v", err)
	}
	fresh := &ssv1alpha1.ManifestCheckpoint{}
	if err := cl.Get(ctx, client.ObjectKey{Name: "mcp"}, fresh); err != nil {
		t.Fatalf("get MCP: %v", err)
	}
	assertHasOwnerRef(t, fresh.OwnerReferences, storagev1alpha1.SchemeGroupVersion.String(), "SnapshotContent", "content", true)
	assertHasOwnerRef(t, fresh.OwnerReferences, unrelated.APIVersion, unrelated.Kind, unrelated.Name, false)
	assertNoOwnerRef(t, fresh.OwnerReferences, controllercommon.DeckhouseAPIVersion, controllercommon.KindObjectKeeper, "ret-mcr")
}

// TestEnsureManifestCheckpointOwnedByContentKeepsExecutionObjectKeeper pins the post-bind-first handoff:
// the MCP handoff replaces the (capture) MCR execution ObjectKeeper's controller ref with the
// SnapshotContent, while the ObjectKeeper OBJECT is left untouched (the aggregator no longer deletes any
// keeper — the import ObjectKeeper backstop was removed; the execution keeper is GC'd with its MCR).
func TestEnsureManifestCheckpointOwnedByContentKeepsExecutionObjectKeeper(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := ssv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add snapshotter scheme: %v", err)
	}
	if err := deckhousev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add deckhouse scheme: %v", err)
	}
	const execOKName = "nss-ok-capture"
	execOK := &deckhousev1alpha1.ObjectKeeper{ObjectMeta: metav1.ObjectMeta{Name: execOKName}}
	mcp := &ssv1alpha1.ManifestCheckpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name: "mcp",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: controllercommon.DeckhouseAPIVersion, Kind: controllercommon.KindObjectKeeper, Name: execOKName, Controller: boolPtr(true),
			}},
		},
	}
	content := snapshotContentUnstructuredForOwnerTest("content", "content-uid")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp, execOK).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	if err := r.ensureManifestCheckpointOwnedByContent(ctx, "mcp", content); err != nil {
		t.Fatalf("handoff MCP ownerRef: %v", err)
	}
	fresh := &ssv1alpha1.ManifestCheckpoint{}
	if err := cl.Get(ctx, client.ObjectKey{Name: "mcp"}, fresh); err != nil {
		t.Fatalf("get MCP: %v", err)
	}
	assertHasOwnerRef(t, fresh.OwnerReferences, storagev1alpha1.SchemeGroupVersion.String(), "SnapshotContent", "content", true)
	assertNoOwnerRef(t, fresh.OwnerReferences, controllercommon.DeckhouseAPIVersion, controllercommon.KindObjectKeeper, execOKName)
	if err := cl.Get(ctx, client.ObjectKey{Name: execOKName}, &deckhousev1alpha1.ObjectKeeper{}); err != nil {
		t.Fatalf("execution ObjectKeeper object must survive the handoff, got err=%v", err)
	}
}

func snapshotContentUnstructuredForOwnerTest(name, uid string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": storagev1alpha1.SchemeGroupVersion.String(),
		"kind":       "SnapshotContent",
		"metadata": map[string]interface{}{
			"name": name,
			"uid":  uid,
		},
	}}
	obj.SetGroupVersionKind(unifiedbootstrap.CommonSnapshotContentGVK())
	return obj
}

func boolPtr(v bool) *bool {
	return &v
}

func assertHasOwnerRef(t *testing.T, refs []metav1.OwnerReference, apiVersion, kind, name string, controller bool) {
	t.Helper()
	for _, ref := range refs {
		if ref.APIVersion == apiVersion && ref.Kind == kind && ref.Name == name {
			gotController := ref.Controller != nil && *ref.Controller
			if gotController != controller {
				t.Fatalf("ownerRef %s/%s/%s controller=%v, want %v", apiVersion, kind, name, gotController, controller)
			}
			return
		}
	}
	t.Fatalf("ownerRef %s/%s/%s not found in %#v", apiVersion, kind, name, refs)
}

func assertNoOwnerRef(t *testing.T, refs []metav1.OwnerReference, apiVersion, kind, name string) {
	t.Helper()
	for _, ref := range refs {
		if ref.APIVersion == apiVersion && ref.Kind == kind && ref.Name == name {
			t.Fatalf("ownerRef %s/%s/%s unexpectedly found in %#v", apiVersion, kind, name, refs)
		}
	}
}

// contentWithReadyCond builds a typed SnapshotContent carrying a single Ready condition. It is read
// back by validateCommonContentChildren as a child SnapshotContent (CommonSnapshotContentGVK).
func contentWithReadyCond(name string, status metav1.ConditionStatus, reason, message string) *storagev1alpha1.SnapshotContent {
	c := &storagev1alpha1.SnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: name}}
	meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{
		Type:    snapshot.ConditionReady,
		Status:  status,
		Reason:  reason,
		Message: message,
	})
	return c
}

// parentContentWithChildRefs builds a parent SnapshotContent (unstructured) whose
// status.childrenSnapshotContentRefs point at the given child names.
func parentContentWithChildRefs(name string, childNames ...string) *unstructured.Unstructured {
	refs := make([]interface{}, 0, len(childNames))
	for _, cn := range childNames {
		refs = append(refs, map[string]interface{}{"name": cn})
	}
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": storagev1alpha1.SchemeGroupVersion.String(),
		"kind":       "SnapshotContent",
		"metadata":   map[string]interface{}{"name": name},
		"status": map[string]interface{}{
			"childrenSnapshotContentRefs": refs,
		},
	}}
	obj.SetGroupVersionKind(unifiedbootstrap.CommonSnapshotContentGVK())
	return obj
}

func assertChildReadyUnchanged(t *testing.T, cl client.Client, name string) {
	t.Helper()
	fresh := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: name}, fresh); err != nil {
		t.Fatalf("get sibling %s: %v", name, err)
	}
	cond := meta.FindStatusCondition(fresh.Status.Conditions, snapshot.ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("sibling %s Ready changed: %#v", name, fresh.Status.Conditions)
	}
}

// A: terminal leaf failure propagates up the ancestor chain (depth >= 2) as ChildrenFailed,
// the root message keeps the failed leaf name + original reason, and an unaffected sibling stays Ready=True.
func TestValidateCommonContentChildrenPropagatesTerminalLeafFailureAcrossDepth(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}

	leaf := contentWithReadyCond("leaf-broken", metav1.ConditionFalse, snapshot.ReasonManifestCheckpointFailed, "ManifestCheckpoint mcp-leaf not found")
	childOK := contentWithReadyCond("child-ok", metav1.ConditionTrue, snapshot.ReasonCompleted, "manifest, data, and child content are ready")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(leaf, childOK).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	// Depth 1: child-a aggregates the broken leaf and must surface a terminal ChildrenFailed
	// that names the leaf and preserves its original reason.
	childA := parentContentWithChildRefs("child-a", "leaf-broken")
	ready, reason, msg, err := r.validateCommonContentChildren(ctx, childA)
	if err != nil {
		t.Fatalf("validate child-a children: %v", err)
	}
	if ready || reason != snapshot.ReasonChildrenFailed {
		t.Fatalf("child-a expected ready=false reason=%s, got ready=%v reason=%s", snapshot.ReasonChildrenFailed, ready, reason)
	}
	if !strings.Contains(msg, "leaf-broken") || !strings.Contains(msg, snapshot.ReasonManifestCheckpointFailed) {
		t.Fatalf("child-a message must name failed leaf + reason, got %q", msg)
	}

	// Persist child-a's computed Ready=False/ChildrenFailed (cumulative message) and aggregate at root.
	if err := cl.Create(ctx, contentWithReadyCond("child-a", metav1.ConditionFalse, reason, msg)); err != nil {
		t.Fatalf("create child-a content: %v", err)
	}
	root := parentContentWithChildRefs("root", "child-a", "child-ok")
	ready, reason, msg, err = r.validateCommonContentChildren(ctx, root)
	if err != nil {
		t.Fatalf("validate root children: %v", err)
	}
	if ready || reason != snapshot.ReasonChildrenFailed {
		t.Fatalf("root expected ready=false reason=%s, got ready=%v reason=%s", snapshot.ReasonChildrenFailed, ready, reason)
	}
	for _, want := range []string{"child-a", "leaf-broken", snapshot.ReasonManifestCheckpointFailed} {
		if !strings.Contains(msg, want) {
			t.Fatalf("root message %q must contain %q (path/ID to failed leaf)", msg, want)
		}
	}

	assertChildReadyUnchanged(t, cl, "child-ok")
}

// B: a missing data artifact on the leaf (ArtifactMissing) is terminal and propagates as ChildrenFailed.
func TestValidateCommonContentChildrenTreatsMissingDataArtifactAsTerminal(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}

	leaf := contentWithReadyCond("leaf-missing-vsc", metav1.ConditionFalse, snapshot.ReasonArtifactMissing, "data artifact(s) missing: vsc-leaf")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(leaf).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	parent := parentContentWithChildRefs("parent", "leaf-missing-vsc")
	ready, reason, msg, err := r.validateCommonContentChildren(ctx, parent)
	if err != nil {
		t.Fatalf("validate children: %v", err)
	}
	if ready || reason != snapshot.ReasonChildrenFailed {
		t.Fatalf("expected ready=false reason=%s, got ready=%v reason=%s", snapshot.ReasonChildrenFailed, ready, reason)
	}
	for _, want := range []string{"leaf-missing-vsc", snapshot.ReasonArtifactMissing, "vsc-leaf"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message %q must contain %q", msg, want)
		}
	}
}

// C: a broken manifest leaf (ManifestCheckpointFailed) is terminal and wins over a sibling that is only
// pending (ArtifactNotReady), regardless of ref order; a non-terminal child alone yields ChildrenPending;
// the Ready sibling is never mutated.
func TestValidateCommonContentChildrenClassifiesTerminalVsPending(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}

	leafBroken := contentWithReadyCond("leaf-manifest-broken", metav1.ConditionFalse, snapshot.ReasonManifestCheckpointFailed, "MCP mcp-x Ready=False")
	pendingChild := contentWithReadyCond("child-pending", metav1.ConditionFalse, snapshot.ReasonArtifactNotReady, "VolumeSnapshotContent vsc-x is not readyToUse")
	siblingOK := contentWithReadyCond("sibling-ok", metav1.ConditionTrue, snapshot.ReasonCompleted, "ready")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(leafBroken, pendingChild, siblingOK).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	// Pending ref first proves terminal failure wins over pending irrespective of iteration order.
	parent := parentContentWithChildRefs("parent", "child-pending", "leaf-manifest-broken", "sibling-ok")
	ready, reason, msg, err := r.validateCommonContentChildren(ctx, parent)
	if err != nil {
		t.Fatalf("validate children: %v", err)
	}
	if ready || reason != snapshot.ReasonChildrenFailed {
		t.Fatalf("expected terminal ready=false reason=%s, got ready=%v reason=%s", snapshot.ReasonChildrenFailed, ready, reason)
	}
	if !strings.Contains(msg, "leaf-manifest-broken") || !strings.Contains(msg, snapshot.ReasonManifestCheckpointFailed) {
		t.Fatalf("message %q must name the terminal child + reason", msg)
	}
	assertChildReadyUnchanged(t, cl, "sibling-ok")

	// Non-terminal child alone must remain pending, not failed.
	pendingParent := parentContentWithChildRefs("pending-parent", "child-pending")
	ready, reason, msg, err = r.validateCommonContentChildren(ctx, pendingParent)
	if err != nil {
		t.Fatalf("validate pending children: %v", err)
	}
	if ready || reason != snapshot.ReasonChildrenPending {
		t.Fatalf("expected ready=false reason=%s, got ready=%v reason=%s", snapshot.ReasonChildrenPending, ready, reason)
	}
	if !strings.Contains(msg, "0/1 ready") || !strings.Contains(msg, "child-pending") {
		t.Fatalf("pending message %q must carry progress count and pending child name", msg)
	}
}
