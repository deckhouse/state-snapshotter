package snapshotcontent

import (
	"context"
	goerrors "errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// --- Revalidation semantics (Phase 2a): MCP state surfaced only through RequestsReady ---

// manifestCheckpointName set, MCP NotFound -> pending (legitimate publish-before-create window).
func TestContentPlanMCPNotFoundPending(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).Build() // MCP "mcp-gone" not created
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	content := commonContentWithStatus("c", "mcp-gone")
	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, content)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.requestsReady != metav1.ConditionFalse || plan.requestsFailed {
		t.Fatalf("requestsReady=%s failed=%v, want False/non-terminal", plan.requestsReady, plan.requestsFailed)
	}
	if plan.readyStatus != metav1.ConditionFalse || plan.readyReason != snapshot.ReasonManifestCapturePending {
		t.Fatalf("ready=%s/%s, want False/%s", plan.readyStatus, plan.readyReason, snapshot.ReasonManifestCapturePending)
	}
	if !strings.Contains(plan.readyMessage, "mcp-gone") || !strings.Contains(plan.readyMessage, "to become Ready") {
		t.Fatalf("ready message %q must name the MCP and say it must become Ready", plan.readyMessage)
	}
}

// MCP exists, Ready=False with terminal Failed reason -> ManifestCheckpointFailed (terminal), MCP message kept.
func TestContentPlanMCPFailedTerminal(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-bad", metav1.ConditionFalse, ssv1alpha1.ManifestCheckpointConditionReasonFailed, "capture exploded")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	content := commonContentWithStatus("c", "mcp-bad")
	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, content)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.requestsReady != metav1.ConditionFalse || !plan.requestsFailed {
		t.Fatalf("requestsReady=%s failed=%v, want False/terminal", plan.requestsReady, plan.requestsFailed)
	}
	if plan.readyStatus != metav1.ConditionFalse || plan.readyReason != snapshot.ReasonManifestCheckpointFailed {
		t.Fatalf("ready=%s/%s, want False/%s", plan.readyStatus, plan.readyReason, snapshot.ReasonManifestCheckpointFailed)
	}
	if !strings.Contains(plan.readyMessage, "capture exploded") {
		t.Fatalf("ready message %q must carry the original MCP message", plan.readyMessage)
	}
}

// MCP exists, Ready=False without a terminal reason -> pending (not terminal).
func TestContentPlanMCPNonTerminalFalsePending(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-wip", metav1.ConditionFalse, "Capturing", "still working")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	content := commonContentWithStatus("c", "mcp-wip")
	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, content)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.requestsFailed {
		t.Fatalf("non-terminal MCP Ready=False must not be terminal")
	}
	if plan.readyReason != snapshot.ReasonManifestCapturePending {
		t.Fatalf("ready reason=%s, want %s", plan.readyReason, snapshot.ReasonManifestCapturePending)
	}
}

// MCP Ready=True but a chunk named in MCP.status.chunks[] is missing -> terminal ManifestCheckpointFailed,
// message names the MCP and the missing chunk. Validated by exact GET (no list/watch).
func TestContentPlanChunkMissingTerminal(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-chunks", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	mcp.Status.Chunks = []ssv1alpha1.ChunkInfo{{Name: "chunk-0", Index: 0}, {Name: "chunk-1", Index: 1}}
	chunk0 := &ssv1alpha1.ManifestCheckpointContentChunk{ObjectMeta: metav1.ObjectMeta{Name: "chunk-0"}}
	// chunk-1 intentionally not created -> missing.
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp, chunk0).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	content := commonContentWithStatus("c", "mcp-chunks")
	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, content)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.requestsReady != metav1.ConditionFalse || !plan.requestsFailed {
		t.Fatalf("requestsReady=%s failed=%v, want False/terminal", plan.requestsReady, plan.requestsFailed)
	}
	if plan.readyReason != snapshot.ReasonManifestCheckpointFailed {
		t.Fatalf("ready reason=%s, want %s", plan.readyReason, snapshot.ReasonManifestCheckpointFailed)
	}
	if !strings.Contains(plan.readyMessage, "mcp-chunks") || !strings.Contains(plan.readyMessage, "chunk-1") {
		t.Fatalf("ready message %q must name the MCP and the missing chunk", plan.readyMessage)
	}
}

// MCP Ready=True and all chunks present -> requests ready (no children -> Ready=True).
func TestContentPlanChunkPresentReady(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-chunks-ok", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	mcp.Status.Chunks = []ssv1alpha1.ChunkInfo{{Name: "ok-chunk-0", Index: 0}}
	chunk0 := &ssv1alpha1.ManifestCheckpointContentChunk{ObjectMeta: metav1.ObjectMeta{Name: "ok-chunk-0"}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp, chunk0).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	content := commonContentWithStatus("c", "mcp-chunks-ok")
	plan, err := r.buildCommonSnapshotContentStatusPlan(ctx, content)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.readyStatus != metav1.ConditionTrue || plan.readyReason != snapshot.ReasonCompleted {
		t.Fatalf("ready=%s/%s, want True/%s", plan.readyStatus, plan.readyReason, snapshot.ReasonCompleted)
	}
}

// MCP Ready=True but a chunk GET fails transiently (non-NotFound): must surface as a reconcile
// error (requeue) and never publish a terminal ManifestCheckpointFailed from a transient blip.
func TestContentPlanChunkTransientErrorRequeues(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	mcp := manifestCheckpointWithReady("mcp-chunks-flaky", metav1.ConditionTrue, ssv1alpha1.ManifestCheckpointConditionReasonCompleted, "ok")
	mcp.Status.Chunks = []ssv1alpha1.ChunkInfo{{Name: "flaky-chunk-0", Index: 0}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcp).
		WithInterceptorFuncs(interceptor.Funcs{
			// Only the chunk existence check uses metadata-only GET; fail exactly that, transiently.
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*metav1.PartialObjectMetadata); ok {
					return goerrors.New("apiserver temporarily unavailable")
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	content := commonContentWithStatus("c", "mcp-chunks-flaky")
	if _, err := r.buildCommonSnapshotContentStatusPlan(ctx, content); err == nil {
		t.Fatalf("transient chunk GET error must propagate as a reconcile error (requeue), got nil")
	} else if strings.Contains(err.Error(), "missing chunk") {
		t.Fatalf("transient error must not be reported as a missing chunk: %v", err)
	}
}

// --- Wake-up mapping: artifact ownerRef -> owning SnapshotContent, enqueue only ---

func TestMapArtifactToOwningSnapshotContentWithOwnerRef(t *testing.T) {
	for _, kind := range []string{"ManifestCheckpoint", "VolumeSnapshotContent"} {
		art := &unstructured.Unstructured{}
		art.SetName("artifact-1")
		art.SetGroupVersionKind(unstructuredGVKForKind(kind))
		ctrlTrue := true
		art.SetOwnerReferences([]metav1.OwnerReference{{
			APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
			Kind:       "SnapshotContent",
			Name:       "owning-content",
			UID:        "uid-1",
			Controller: &ctrlTrue,
		}})
		reqs := mapArtifactToOwningSnapshotContent(context.Background(), art)
		if len(reqs) != 1 || reqs[0].Name != "owning-content" {
			t.Fatalf("kind=%s: got %v, want one request for owning-content", kind, reqs)
		}
	}
}

// No SnapshotContent ownerRef (none at all, or only a foreign owner) must never route, for either
// artifact kind. Covers MCP/VSC with no ownerRef and MCP/VSC with a foreign-only ownerRef.
func TestMapArtifactToOwningSnapshotContentNoOwnerRef(t *testing.T) {
	foreign := []metav1.OwnerReference{{APIVersion: "example.com/v1", Kind: "Foo", Name: "foo-1"}}
	cases := []struct {
		name string
		kind string
		refs []metav1.OwnerReference
	}{
		{"mcp-no-owner", "ManifestCheckpoint", nil},
		{"mcp-foreign-owner", "ManifestCheckpoint", foreign},
		{"vsc-no-owner", "VolumeSnapshotContent", nil},
		{"vsc-foreign-owner", "VolumeSnapshotContent", foreign},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			art := &unstructured.Unstructured{}
			art.SetName(tc.name)
			art.SetGroupVersionKind(unstructuredGVKForKind(tc.kind))
			if tc.refs != nil {
				art.SetOwnerReferences(tc.refs)
			}
			if reqs := mapArtifactToOwningSnapshotContent(context.Background(), art); reqs != nil {
				t.Fatalf("expected nil (no SnapshotContent ownerRef), got %v", reqs)
			}
		})
	}
}

// --- VSC ownerRef self-healing from status.dataRefs[] ---

func TestSelfHealVSCOwnerRefAddsAndPreservesForeign(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	vsc := volumeSnapshotContentObject("vsc-1", true)
	// Pre-existing foreign, non-controller ownerRef that must be preserved.
	vsc.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: "example.com/v1", Kind: "Foo", Name: "foo-1", UID: "foo-uid",
	}})
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vsc).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	content := commonContentReadyWithMCPAndDataRefs("owning-content", "mcp-ok", "vsc-1")
	content.SetUID(types.UID("content-uid"))
	r.selfHealDataArtifactOwnerRefs(ctx, content)

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(unstructuredGVKForKind("VolumeSnapshotContent"))
	if err := cl.Get(ctx, client.ObjectKey{Name: "vsc-1"}, got); err != nil {
		t.Fatalf("get vsc: %v", err)
	}
	var foundContent, foundForeign bool
	for _, ref := range got.GetOwnerReferences() {
		if ref.Kind == "SnapshotContent" && ref.Name == "owning-content" {
			foundContent = true
		}
		if ref.Kind == "Foo" && ref.Name == "foo-1" {
			foundForeign = true
		}
	}
	if !foundContent {
		t.Fatalf("self-heal must add SnapshotContent ownerRef; refs=%v", got.GetOwnerReferences())
	}
	if !foundForeign {
		t.Fatalf("self-heal must preserve foreign non-controller ownerRef; refs=%v", got.GetOwnerReferences())
	}
}

// VSC already carries the correct SnapshotContent controller ownerRef: self-heal must be a no-op
// (no Patch), so steady-state reconciles do not churn the artifact.
func TestSelfHealVSCAlreadyOwnedNoUpdate(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	vsc := volumeSnapshotContentObject("vsc-owned", true)
	ctrlTrue := true
	vsc.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
		Kind:       "SnapshotContent",
		Name:       "owning-content",
		UID:        types.UID("content-uid"),
		Controller: &ctrlTrue,
	}})
	var patchCalls int
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vsc).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				patchCalls++
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	content := commonContentReadyWithMCPAndDataRefs("owning-content", "mcp-ok", "vsc-owned")
	content.SetUID(types.UID("content-uid"))
	r.selfHealDataArtifactOwnerRefs(ctx, content)

	if patchCalls != 0 {
		t.Fatalf("self-heal must not patch an already-correctly-owned VSC; patchCalls=%d", patchCalls)
	}
}

func TestSelfHealVSCMissingIsNoop(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).Build() // vsc-gone not created
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	content := commonContentReadyWithMCPAndDataRefs("c", "mcp-ok", "vsc-gone")
	// Must not panic / error; missing artifact is left to data readiness.
	r.selfHealDataArtifactOwnerRefs(ctx, content)
}

func TestSelfHealVSCDeletingNotPatched(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	vsc := volumeSnapshotContentObject("vsc-del", true)
	vsc.SetFinalizers([]string{"keep/for-test"})
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vsc).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	// Mark deleting (finalizer keeps the object around with a deletionTimestamp).
	if err := cl.Delete(ctx, vsc); err != nil {
		t.Fatalf("delete vsc: %v", err)
	}

	content := commonContentReadyWithMCPAndDataRefs("owning-content", "mcp-ok", "vsc-del")
	content.SetUID(types.UID("content-uid"))
	r.selfHealDataArtifactOwnerRefs(ctx, content)

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(unstructuredGVKForKind("VolumeSnapshotContent"))
	if err := cl.Get(ctx, client.ObjectKey{Name: "vsc-del"}, got); err != nil {
		t.Fatalf("get vsc: %v", err)
	}
	for _, ref := range got.GetOwnerReferences() {
		if ref.Kind == "SnapshotContent" {
			t.Fatalf("self-heal must not patch a deleting VSC; refs=%v", got.GetOwnerReferences())
		}
	}
}

// --- Propagation: terminal vs pending child classification, with sibling isolation ---

// A leaf RequestsReady=False/ArtifactMissing must propagate to the parent as ChildrenFailed (terminal),
// and only the failed child drives the failure (the ready sibling is untouched).
func TestPropagationArtifactMissingToChildrenFailedSiblingReady(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	failed := contentWithReadyCond("disk-content", metav1.ConditionFalse, snapshot.ReasonArtifactMissing,
		"data artifact VolumeSnapshotContent/snap-x for target pvc-1 is missing")
	readySibling := contentWithReadyCond("logs-content", metav1.ConditionTrue, snapshot.ReasonCompleted, "ready")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(failed, readySibling).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	parent := parentContentWithChildRefs("vm-content", "disk-content", "logs-content")
	ready, reason, msg, err := r.validateCommonContentChildren(ctx, parent)
	if err != nil {
		t.Fatalf("validate children: %v", err)
	}
	if ready || reason != snapshot.ReasonChildrenFailed {
		t.Fatalf("got ready=%v reason=%s, want false/ChildrenFailed", ready, reason)
	}
	if !strings.Contains(msg, "leaf=disk-content") || !strings.Contains(msg, "reason=ArtifactMissing") {
		t.Fatalf("message %q must pin the failed leaf and its reason", msg)
	}
	if strings.Contains(msg, "logs-content") {
		t.Fatalf("ready sibling must not appear in failure message %q", msg)
	}
}

// A leaf RequestsReady=False/DataCapturePending is non-terminal and must propagate as ChildrenPending,
// not ChildrenFailed (a transient child must not fail the tree).
func TestPropagationDataCapturePendingToChildrenPending(t *testing.T) {
	ctx := context.Background()
	scheme := aggScheme(t)
	pending := contentWithReadyCond("disk-content", metav1.ConditionFalse, snapshot.ReasonDataCapturePending,
		"waiting for volume snapshot artifacts: 0/1 ready; pending: vsc-1")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pending).Build()
	r := &SnapshotContentController{Client: cl, APIReader: cl, GVKRegistry: snapshot.NewGVKRegistry()}

	parent := parentContentWithChildRefs("vm-content", "disk-content")
	ready, reason, _, err := r.validateCommonContentChildren(ctx, parent)
	if err != nil {
		t.Fatalf("validate children: %v", err)
	}
	if ready || reason != snapshot.ReasonChildrenPending {
		t.Fatalf("got ready=%v reason=%s, want false/ChildrenPending", ready, reason)
	}
}

func unstructuredGVKForKind(kind string) schema.GroupVersionKind {
	switch kind {
	case "ManifestCheckpoint":
		return schema.GroupVersionKind{Group: ssv1alpha1.SchemeGroupVersion.Group, Version: ssv1alpha1.SchemeGroupVersion.Version, Kind: kind}
	default:
		return schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: kind}
	}
}
