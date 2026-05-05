package controllers

import (
	"context"
	"testing"

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

func TestSnapshotContentControllerCascadeRemoveFinalizersFromCommonChildren(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}

	child := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "child-content",
			Finalizers: []string{snapshot.FinalizerParentProtect},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(child).Build()
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
					map[string]interface{}{"name": "child-content"},
				},
			},
		},
	}
	rootObj.SetGroupVersionKind(unifiedbootstrap.CommonSnapshotContentGVK())
	contentLike, err := snapshot.ExtractSnapshotContentLike(rootObj)
	if err != nil {
		t.Fatalf("extract content like: %v", err)
	}

	if err := r.cascadeRemoveFinalizersFromChildren(ctx, contentLike, rootObj); err != nil {
		t.Fatalf("cascade finalizers: %v", err)
	}

	freshChild := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: "child-content"}, freshChild); err != nil {
		t.Fatalf("get child: %v", err)
	}
	for _, finalizer := range freshChild.Finalizers {
		if finalizer == snapshot.FinalizerParentProtect {
			t.Fatalf("child parent-protect finalizer was not removed: %v", freshChild.Finalizers)
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

func TestEnsureChildSnapshotContentOwnedByParentHandoffPreservesUnrelatedRefs(t *testing.T) {
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

	if err := r.ensureChildSnapshotContentOwnedByParent(ctx, "child-content", parent); err != nil {
		t.Fatalf("handoff child content ownerRef: %v", err)
	}
	fresh := &storagev1alpha1.SnapshotContent{}
	if err := cl.Get(ctx, client.ObjectKey{Name: "child-content"}, fresh); err != nil {
		t.Fatalf("get child content: %v", err)
	}
	assertHasOwnerRef(t, fresh.OwnerReferences, storagev1alpha1.SchemeGroupVersion.String(), "SnapshotContent", "parent-content", true)
	assertHasOwnerRef(t, fresh.OwnerReferences, unrelated.APIVersion, unrelated.Kind, unrelated.Name, false)
	assertNoOwnerRef(t, fresh.OwnerReferences, "demo.test/v1", "DemoVirtualDiskSnapshot", "child-snapshot")
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
				{APIVersion: DeckhouseAPIVersion, Kind: KindObjectKeeper, Name: "ret-mcr", Controller: boolPtr(true)},
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
	assertNoOwnerRef(t, fresh.OwnerReferences, DeckhouseAPIVersion, KindObjectKeeper, "ret-mcr")
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
