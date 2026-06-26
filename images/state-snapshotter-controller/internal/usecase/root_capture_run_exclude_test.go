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

package usecase

import (
	"context"
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

func rootCaptureTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := ssv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	if err := storagev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme storage: %v", err)
	}
	return s
}

func fixtureSnapshotUnstructured(boundContent string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "generic.state-snapshotter.test",
		Version: "v1",
		Kind:    "FixtureDomainSnapshot",
	})
	u.SetNamespace("ns1")
	u.SetName("disk-a")
	_ = unstructured.SetNestedField(u.Object, boundContent, "status", "boundSnapshotContentName")
	return u
}

func fixtureContent(name, mcpName string, children ...string) *storagev1alpha1.SnapshotContent {
	refs := make([]storagev1alpha1.SnapshotContentChildRef, 0, len(children))
	for _, child := range children {
		refs = append(refs, storagev1alpha1.SnapshotContentChildRef{Name: child})
	}
	c := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: storagev1alpha1.SnapshotContentStatus{
			ManifestCheckpointName:      mcpName,
			ChildrenSnapshotContentRefs: refs,
		},
	}
	// Direct children of the root must be ManifestsArchived=True for the root-capture wave barrier
	// (requireContentManifestsArchived) to proceed. Fixture direct children represent a fully archived
	// subtree.
	meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{
		Type:   snapshot.ConditionManifestsArchived,
		Status: metav1.ConditionTrue,
		Reason: snapshot.ReasonManifestsArchived,
	})
	return c
}

func TestCollectRunSubtreeManifestExcludeKeys_ChildContentMCPContributes(t *testing.T) {
	scheme := rootCaptureTestScheme(t)
	log, _ := logger.NewLogger("error")
	ctx := context.Background()
	d1, c1 := aggManifestEncodeChunk([]map[string]interface{}{
		{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]interface{}{"name": "demo-owned", "namespace": "ns1"}},
	})
	ch := aggManifestCreateChunk("ch-dedicated", "mcp-dedicated", d1, c1)
	mcpDedicated := aggManifestReadyMCP("mcp-dedicated", "ns1", []ssv1alpha1.ChunkInfo{{Name: ch.Name, Index: 0, Checksum: c1}}, 1)

	rootContentObj := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "root-content"},
		Status: storagev1alpha1.SnapshotContentStatus{
			ChildrenSnapshotContentRefs: []storagev1alpha1.SnapshotContentChildRef{{Name: "disk-content"}},
		},
	}
	disk := fixtureSnapshotUnstructured("disk-content")
	diskContent := fixtureContent("disk-content", "mcp-dedicated")
	rootNS := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: "ns1"},
		Status: storagev1alpha1.SnapshotStatus{
			ChildrenSnapshotRefs: []storagev1alpha1.SnapshotChildRef{{
				APIVersion: "generic.state-snapshotter.test/v1",
				Kind:       "FixtureDomainSnapshot",
				Name:       "disk-a",
			}},
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ch, mcpDedicated, rootContentObj, disk, diskContent, rootNS).Build()
	arch := NewArchiveService(cl, cl, log)

	excl, err := collectRunSubtreeManifestExcludeKeys(ctx, arch, cl, rootNS, "root-content")
	if err != nil {
		t.Fatalf("collectRunSubtreeManifestExcludeKeys: %v", err)
	}
	k := namespacemanifest.ManifestTargetDedupKey("ns1", namespacemanifest.ManifestTarget{
		APIVersion: "v1", Kind: "ConfigMap", Name: "demo-owned",
	})
	if _, ok := excl[k]; !ok {
		t.Fatalf("expected dedicated content MCP object in exclude set, got %#v", excl)
	}
}

func TestCollectRunSubtreeManifestExcludeKeys_ChildContentWithoutMCPPends(t *testing.T) {
	scheme := rootCaptureTestScheme(t)
	log, _ := logger.NewLogger("error")
	ctx := context.Background()
	rootContentObj := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "root-content"},
		Status: storagev1alpha1.SnapshotContentStatus{
			ChildrenSnapshotContentRefs: []storagev1alpha1.SnapshotContentChildRef{{Name: "disk-content"}},
		},
	}
	disk := fixtureSnapshotUnstructured("disk-content")
	diskContent := fixtureContent("disk-content", "")
	rootNS := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: "ns1"},
		Status: storagev1alpha1.SnapshotStatus{
			ChildrenSnapshotRefs: []storagev1alpha1.SnapshotChildRef{{
				APIVersion: "generic.state-snapshotter.test/v1",
				Kind:       "FixtureDomainSnapshot",
				Name:       "disk-a",
			}},
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(rootContentObj, disk, diskContent, rootNS).Build()
	arch := NewArchiveService(cl, cl, log)

	_, err := collectRunSubtreeManifestExcludeKeys(ctx, arch, cl, rootNS, "root-content")
	if err == nil {
		t.Fatal("expected pending error when dedicated content has no manifestCheckpointName")
	}
	if !errors.Is(err, ErrSubtreeManifestCapturePending) {
		t.Fatalf("expected ErrSubtreeManifestCapturePending, got %v", err)
	}
}

func TestCollectRunSubtreeManifestExcludeKeys_ChildContentMCPNotReadyPends(t *testing.T) {
	scheme := rootCaptureTestScheme(t)
	log, _ := logger.NewLogger("error")
	ctx := context.Background()
	rootContentObj := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "root-content"},
		Status: storagev1alpha1.SnapshotContentStatus{
			ChildrenSnapshotContentRefs: []storagev1alpha1.SnapshotContentChildRef{{Name: "disk-content"}},
		},
	}
	disk := fixtureSnapshotUnstructured("disk-content")
	diskContent := fixtureContent("disk-content", "mcp-pending")
	mcpPending := &ssv1alpha1.ManifestCheckpoint{ObjectMeta: metav1.ObjectMeta{Name: "mcp-pending"}}
	rootNS := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: "ns1"},
		Status: storagev1alpha1.SnapshotStatus{
			ChildrenSnapshotRefs: []storagev1alpha1.SnapshotChildRef{{
				APIVersion: "generic.state-snapshotter.test/v1",
				Kind:       "FixtureDomainSnapshot",
				Name:       "disk-a",
			}},
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(rootContentObj, disk, diskContent, mcpPending, rootNS).Build()
	arch := NewArchiveService(cl, cl, log)

	_, err := collectRunSubtreeManifestExcludeKeys(ctx, arch, cl, rootNS, "root-content")
	if err == nil {
		t.Fatal("expected pending error when dedicated content ManifestCheckpoint is not Ready")
	}
	if !errors.Is(err, ErrSubtreeManifestCapturePending) {
		t.Fatalf("expected ErrSubtreeManifestCapturePending, got %v", err)
	}
}

func TestCollectRunSubtreeManifestExcludeKeys_ExcludesOnlyDescendantMCP(t *testing.T) {
	scheme := rootCaptureTestScheme(t)
	log, _ := logger.NewLogger("error")
	ctx := context.Background()
	d1, c1 := aggManifestEncodeChunk([]map[string]interface{}{
		{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]interface{}{"name": "covered", "namespace": "ns1"}},
	})
	ch := aggManifestCreateChunk("ch-child", "mcp-child", d1, c1)
	mcpChild := aggManifestReadyMCP("mcp-child", "ns1", []ssv1alpha1.ChunkInfo{{Name: ch.Name, Index: 0, Checksum: c1}}, 1)

	dOrphan, cOrphan := aggManifestEncodeChunk([]map[string]interface{}{
		{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]interface{}{"name": "orphan-cm", "namespace": "ns1"}},
	})
	chOrphan := aggManifestCreateChunk("ch-orphan", "mcp-orphan", dOrphan, cOrphan)
	mcpOrphan := aggManifestReadyMCP("mcp-orphan", "ns1", []ssv1alpha1.ChunkInfo{{Name: chOrphan.Name, Index: 0, Checksum: cOrphan}}, 1)

	rootContentObj := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "root-content"},
		Status: storagev1alpha1.SnapshotContentStatus{
			ChildrenSnapshotContentRefs: []storagev1alpha1.SnapshotContentChildRef{{Name: "child-content"}},
		},
	}
	childContentObj := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "child-content"},
		Status: storagev1alpha1.SnapshotContentStatus{
			ManifestCheckpointName: "mcp-child",
		},
	}
	meta.SetStatusCondition(&childContentObj.Status.Conditions, metav1.Condition{
		Type: snapshot.ConditionReady, Status: metav1.ConditionTrue, Reason: "Completed",
	})
	// Direct child of the root: must be ManifestsArchived=True for the wave barrier to proceed.
	meta.SetStatusCondition(&childContentObj.Status.Conditions, metav1.Condition{
		Type: snapshot.ConditionManifestsArchived, Status: metav1.ConditionTrue, Reason: snapshot.ReasonManifestsArchived,
	})

	childSnap := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "ch1", Namespace: "ns1"},
		Status: storagev1alpha1.SnapshotStatus{
			BoundSnapshotContentName: "child-content",
		},
	}

	rootNS := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: "ns1"},
		Status: storagev1alpha1.SnapshotStatus{
			ChildrenSnapshotRefs: []storagev1alpha1.SnapshotChildRef{
				{
					APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
					Kind:       "Snapshot",
					Name:       "ch1",
				},
			},
		},
	}

	orphanContentObj := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "orphan-content"},
		Status: storagev1alpha1.SnapshotContentStatus{
			ManifestCheckpointName: "mcp-orphan",
		},
	}
	meta.SetStatusCondition(&orphanContentObj.Status.Conditions, metav1.Condition{
		Type: snapshot.ConditionReady, Status: metav1.ConditionTrue, Reason: "Completed",
	})

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		ch, mcpChild, chOrphan, mcpOrphan,
		rootContentObj, childContentObj, orphanContentObj, childSnap, rootNS,
	).Build()
	arch := NewArchiveService(cl, cl, log)

	excl, err := collectRunSubtreeManifestExcludeKeys(ctx, arch, cl, rootNS, "root-content")
	if err != nil {
		t.Fatalf("collectRunSubtreeManifestExcludeKeys: %v", err)
	}
	k := namespacemanifest.ManifestTargetDedupKey("ns1", namespacemanifest.ManifestTarget{
		APIVersion: "v1", Kind: "ConfigMap", Name: "covered",
	})
	if _, ok := excl[k]; !ok {
		t.Fatalf("expected covered ConfigMap in exclude set, got %#v", excl)
	}
	orphanKey := namespacemanifest.ManifestTargetDedupKey("ns1", namespacemanifest.ManifestTarget{
		APIVersion: "v1", Kind: "ConfigMap", Name: "orphan-cm",
	})
	if _, ok := excl[orphanKey]; ok {
		t.Fatalf("object from SnapshotContent not in run graph must not affect exclude (INV-S0)")
	}
}

// Grandchild dedup (the 409 scenario, healthy state): root -> child (archived, links grandchild) ->
// grandchild whose MCP captures a leaf object (disk-vm). When the direct child is ManifestsArchived=True
// (subtree fully linked), the content-graph walk reaches the grandchild and the leaf object is added to
// the root exclude set, so the root MCR cannot double-capture it.
func TestCollectRunSubtreeManifestExcludeKeys_GrandchildLeafExcluded(t *testing.T) {
	scheme := rootCaptureTestScheme(t)
	log, _ := logger.NewLogger("error")
	ctx := context.Background()

	dChild, cChild := aggManifestEncodeChunk([]map[string]interface{}{
		{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]interface{}{"name": "child-cm", "namespace": "ns1"}},
	})
	chChild := aggManifestCreateChunk("ch-child", "mcp-child", dChild, cChild)
	mcpChild := aggManifestReadyMCP("mcp-child", "ns1", []ssv1alpha1.ChunkInfo{{Name: chChild.Name, Index: 0, Checksum: cChild}}, 1)

	dGC, cGC := aggManifestEncodeChunk([]map[string]interface{}{
		{"apiVersion": "demo.state-snapshotter.deckhouse.io/v1alpha1", "kind": "DemoVirtualDisk", "metadata": map[string]interface{}{"name": "disk-vm", "namespace": "ns1"}},
	})
	chGC := aggManifestCreateChunk("ch-gc", "mcp-gc", dGC, cGC)
	mcpGC := aggManifestReadyMCP("mcp-gc", "ns1", []ssv1alpha1.ChunkInfo{{Name: chGC.Name, Index: 0, Checksum: cGC}}, 1)

	rootContentObj := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "root-content"},
		Status: storagev1alpha1.SnapshotContentStatus{
			ChildrenSnapshotContentRefs: []storagev1alpha1.SnapshotContentChildRef{{Name: "child-content"}},
		},
	}
	// child-content is a direct child: archived (fixtureContent sets ManifestsArchived=True) and links the
	// grandchild edge.
	childContent := fixtureContent("child-content", "mcp-child", "grandchild-content")
	grandchildContent := fixtureContent("grandchild-content", "mcp-gc")

	childSnap := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "ch1", Namespace: "ns1"},
		Status:     storagev1alpha1.SnapshotStatus{BoundSnapshotContentName: "child-content"},
	}
	rootNS := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: "ns1"},
		Status: storagev1alpha1.SnapshotStatus{
			ChildrenSnapshotRefs: []storagev1alpha1.SnapshotChildRef{{
				APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
				Kind:       "Snapshot",
				Name:       "ch1",
			}},
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		chChild, mcpChild, chGC, mcpGC,
		rootContentObj, childContent, grandchildContent, childSnap, rootNS,
	).Build()
	arch := NewArchiveService(cl, cl, log)

	excl, err := collectRunSubtreeManifestExcludeKeys(ctx, arch, cl, rootNS, "root-content")
	if err != nil {
		t.Fatalf("collectRunSubtreeManifestExcludeKeys: %v", err)
	}
	gcKey := namespacemanifest.ManifestTargetDedupKey("ns1", namespacemanifest.ManifestTarget{
		APIVersion: "demo.state-snapshotter.deckhouse.io/v1alpha1", Kind: "DemoVirtualDisk", Name: "disk-vm",
	})
	if _, ok := excl[gcKey]; !ok {
		t.Fatalf("grandchild leaf object disk-vm must be in the root exclude set, got %#v", excl)
	}
	childKey := namespacemanifest.ManifestTargetDedupKey("ns1", namespacemanifest.ManifestTarget{
		APIVersion: "v1", Kind: "ConfigMap", Name: "child-cm",
	})
	if _, ok := excl[childKey]; !ok {
		t.Fatalf("direct child object child-cm must be in the root exclude set, got %#v", excl)
	}
}

func TestCollectRunSubtreeManifestExcludeKeys_ChildNotReachableFails(t *testing.T) {
	scheme := rootCaptureTestScheme(t)
	log, _ := logger.NewLogger("error")
	ctx := context.Background()
	rootContentObj := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "root-content"},
		Status:     storagev1alpha1.SnapshotContentStatus{},
	}
	disk := fixtureSnapshotUnstructured("missing-from-graph")
	rootNS := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: "ns1"},
		Status: storagev1alpha1.SnapshotStatus{
			ChildrenSnapshotRefs: []storagev1alpha1.SnapshotChildRef{
				{
					APIVersion: "generic.state-snapshotter.test/v1",
					Kind:       "FixtureDomainSnapshot",
					Name:       "disk-a",
				},
			},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(rootContentObj, disk, rootNS).Build()
	arch := NewArchiveService(cl, cl, log)

	_, err := collectRunSubtreeManifestExcludeKeys(ctx, arch, cl, rootNS, "root-content")
	if err == nil {
		t.Fatal("expected error when child content is not linked from root SnapshotContent graph")
	}
	if !errors.Is(err, ErrRunGraphChildNotReachable) {
		t.Fatalf("expected ErrRunGraphChildNotReachable, got %v", err)
	}
}

func TestCollectRunSubtreeManifestExcludeKeys_MCPReadFailClosed(t *testing.T) {
	scheme := rootCaptureTestScheme(t)
	log, _ := logger.NewLogger("error")
	ctx := context.Background()
	rootContentObj := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "root-content"},
		Status: storagev1alpha1.SnapshotContentStatus{
			ChildrenSnapshotContentRefs: []storagev1alpha1.SnapshotContentChildRef{{Name: "child-content"}},
		},
	}
	childContentObj := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "child-content"},
		Status: storagev1alpha1.SnapshotContentStatus{
			ManifestCheckpointName: "mcp-broken",
		},
	}
	meta.SetStatusCondition(&childContentObj.Status.Conditions, metav1.Condition{
		Type: snapshot.ConditionReady, Status: metav1.ConditionTrue, Reason: "Completed",
	})
	disk := fixtureSnapshotUnstructured("child-content")
	rootNS := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: "ns1"},
		Status: storagev1alpha1.SnapshotStatus{
			ChildrenSnapshotRefs: []storagev1alpha1.SnapshotChildRef{
				{
					APIVersion: "generic.state-snapshotter.test/v1",
					Kind:       "FixtureDomainSnapshot",
					Name:       "disk-a",
				},
			},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(rootContentObj, childContentObj, disk, rootNS).Build()
	arch := NewArchiveService(cl, cl, log)

	_, err := collectRunSubtreeManifestExcludeKeys(ctx, arch, cl, rootNS, "root-content")
	if err == nil {
		t.Fatal("expected error when ManifestCheckpoint is missing / not readable")
	}
}

// Wave barrier: a reachable direct child whose ManifestCheckpoint is Ready but whose subtree latch
// (ManifestsArchived) is not yet True must hold root capture as pending, never build the root MCR. This
// is the guard against the 409 duplicate race: while a descendant subtree is not fully archived/linked,
// the root must not capture (its exclude set could miss not-yet-linked descendant manifests).
func TestCollectRunSubtreeManifestExcludeKeys_DirectChildNotArchivedPends(t *testing.T) {
	scheme := rootCaptureTestScheme(t)
	log, _ := logger.NewLogger("error")
	ctx := context.Background()
	d1, c1 := aggManifestEncodeChunk([]map[string]interface{}{
		{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]interface{}{"name": "demo-owned", "namespace": "ns1"}},
	})
	ch := aggManifestCreateChunk("ch-d", "mcp-d", d1, c1)
	mcp := aggManifestReadyMCP("mcp-d", "ns1", []ssv1alpha1.ChunkInfo{{Name: ch.Name, Index: 0, Checksum: c1}}, 1)

	rootContentObj := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "root-content"},
		Status: storagev1alpha1.SnapshotContentStatus{
			ChildrenSnapshotContentRefs: []storagev1alpha1.SnapshotContentChildRef{{Name: "disk-content"}},
		},
	}
	// disk-content has a Ready MCP but NO ManifestsArchived=True condition (subtree not fully archived).
	diskContent := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "disk-content"},
		Status:     storagev1alpha1.SnapshotContentStatus{ManifestCheckpointName: "mcp-d"},
	}
	disk := fixtureSnapshotUnstructured("disk-content")
	rootNS := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: "ns1"},
		Status: storagev1alpha1.SnapshotStatus{
			ChildrenSnapshotRefs: []storagev1alpha1.SnapshotChildRef{{
				APIVersion: "generic.state-snapshotter.test/v1",
				Kind:       "FixtureDomainSnapshot",
				Name:       "disk-a",
			}},
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ch, mcp, rootContentObj, diskContent, disk, rootNS).Build()
	arch := NewArchiveService(cl, cl, log)

	_, err := collectRunSubtreeManifestExcludeKeys(ctx, arch, cl, rootNS, "root-content")
	if !errors.Is(err, ErrSubtreeManifestCapturePending) {
		t.Fatalf("expected ErrSubtreeManifestCapturePending while a direct child is not ManifestsArchived, got %v", err)
	}
}

// Wave barrier: a direct child terminally ManifestsArchived=False/ManifestsArchiveFailed makes root
// capture terminally fail (the subtree can never be archived), not merely pend.
func TestCollectRunSubtreeManifestExcludeKeys_DirectChildArchiveFailedFails(t *testing.T) {
	scheme := rootCaptureTestScheme(t)
	log, _ := logger.NewLogger("error")
	ctx := context.Background()
	d1, c1 := aggManifestEncodeChunk([]map[string]interface{}{
		{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]interface{}{"name": "demo-owned", "namespace": "ns1"}},
	})
	ch := aggManifestCreateChunk("ch-d", "mcp-d", d1, c1)
	mcp := aggManifestReadyMCP("mcp-d", "ns1", []ssv1alpha1.ChunkInfo{{Name: ch.Name, Index: 0, Checksum: c1}}, 1)

	rootContentObj := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "root-content"},
		Status: storagev1alpha1.SnapshotContentStatus{
			ChildrenSnapshotContentRefs: []storagev1alpha1.SnapshotContentChildRef{{Name: "disk-content"}},
		},
	}
	diskContent := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "disk-content"},
		Status:     storagev1alpha1.SnapshotContentStatus{ManifestCheckpointName: "mcp-d"},
	}
	meta.SetStatusCondition(&diskContent.Status.Conditions, metav1.Condition{
		Type:    snapshot.ConditionManifestsArchived,
		Status:  metav1.ConditionFalse,
		Reason:  snapshot.ReasonManifestsArchiveFailed,
		Message: "descendant capture failed",
	})
	disk := fixtureSnapshotUnstructured("disk-content")
	rootNS := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: "ns1"},
		Status: storagev1alpha1.SnapshotStatus{
			ChildrenSnapshotRefs: []storagev1alpha1.SnapshotChildRef{{
				APIVersion: "generic.state-snapshotter.test/v1",
				Kind:       "FixtureDomainSnapshot",
				Name:       "disk-a",
			}},
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ch, mcp, rootContentObj, diskContent, disk, rootNS).Build()
	arch := NewArchiveService(cl, cl, log)

	_, err := collectRunSubtreeManifestExcludeKeys(ctx, arch, cl, rootNS, "root-content")
	if !errors.Is(err, ErrSubtreeManifestCaptureFailed) {
		t.Fatalf("expected ErrSubtreeManifestCaptureFailed for a terminally archive-failed direct child, got %v", err)
	}
}

func TestFilterManifestTargets_RemovesExcludedKeys(t *testing.T) {
	base := []namespacemanifest.ManifestTarget{
		{APIVersion: "v1", Kind: "ConfigMap", Name: "keep"},
		{APIVersion: "v1", Kind: "ConfigMap", Name: "drop"},
	}
	excl := map[string]struct{}{
		namespacemanifest.ManifestTargetDedupKey("ns1", namespacemanifest.ManifestTarget{
			APIVersion: "v1", Kind: "ConfigMap", Name: "drop",
		}): {},
	}
	out := namespacemanifest.FilterManifestTargets(base, excl, "ns1")
	if len(out) != 1 || out[0].Name != "keep" {
		t.Fatalf("unexpected filter result: %#v", out)
	}
}
