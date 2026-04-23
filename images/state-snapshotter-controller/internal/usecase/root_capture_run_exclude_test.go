/*
Copyright 2025 Flant JSC

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
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
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
	if err := demov1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme demo: %v", err)
	}
	return s
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

	nscRoot := &storagev1alpha1.NamespaceSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "root-nsc"},
		Status: storagev1alpha1.NamespaceSnapshotContentStatus{
			ChildrenSnapshotContentRefs: []storagev1alpha1.NamespaceSnapshotContentChildRef{{Name: "child-nsc"}},
		},
	}
	nscChild := &storagev1alpha1.NamespaceSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "child-nsc"},
		Status: storagev1alpha1.NamespaceSnapshotContentStatus{
			ManifestCheckpointName: "mcp-child",
		},
	}
	meta.SetStatusCondition(&nscChild.Status.Conditions, metav1.Condition{
		Type: snapshot.ConditionReady, Status: metav1.ConditionTrue, Reason: "Completed",
	})

	childSnap := &storagev1alpha1.NamespaceSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "ch1", Namespace: "ns1"},
		Status: storagev1alpha1.NamespaceSnapshotStatus{
			BoundSnapshotContentName: "child-nsc",
		},
	}

	rootNS := &storagev1alpha1.NamespaceSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: "ns1"},
		Status: storagev1alpha1.NamespaceSnapshotStatus{
			ChildrenSnapshotRefs: []storagev1alpha1.NamespaceSnapshotChildRef{
				{Namespace: "ns1", Name: "ch1"},
			},
		},
	}

	nscOrphan := &storagev1alpha1.NamespaceSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "orphan-nsc"},
		Status: storagev1alpha1.NamespaceSnapshotContentStatus{
			ManifestCheckpointName: "mcp-orphan",
		},
	}
	meta.SetStatusCondition(&nscOrphan.Status.Conditions, metav1.Condition{
		Type: snapshot.ConditionReady, Status: metav1.ConditionTrue, Reason: "Completed",
	})

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		ch, mcpChild, chOrphan, mcpOrphan,
		nscRoot, nscChild, nscOrphan, childSnap, rootNS,
	).Build()
	arch := NewArchiveService(cl, cl, log)

	excl, err := collectRunSubtreeManifestExcludeKeys(ctx, arch, cl, rootNS, "root-nsc")
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
		t.Fatalf("object from NamespaceSnapshotContent not in run graph must not affect exclude (INV-S0)")
	}
}

func TestCollectRunSubtreeManifestExcludeKeys_ChildNotReachableFails(t *testing.T) {
	scheme := rootCaptureTestScheme(t)
	log, _ := logger.NewLogger("error")
	ctx := context.Background()

	nscRoot := &storagev1alpha1.NamespaceSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "root-nsc"},
		Status:     storagev1alpha1.NamespaceSnapshotContentStatus{},
	}
	disk := &demov1alpha1.DemoVirtualDiskSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "disk-a", Namespace: "ns1"},
		Status: demov1alpha1.DemoVirtualDiskSnapshotStatus{
			BoundSnapshotContentName: "missing-from-graph",
		},
	}
	rootNS := &storagev1alpha1.NamespaceSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: "ns1"},
		Status: storagev1alpha1.NamespaceSnapshotStatus{
			ChildrenSnapshotRefs: []storagev1alpha1.NamespaceSnapshotChildRef{
				{Namespace: "ns1", Name: "disk-a"},
			},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nscRoot, disk, rootNS).Build()
	arch := NewArchiveService(cl, cl, log)

	_, err := collectRunSubtreeManifestExcludeKeys(ctx, arch, cl, rootNS, "root-nsc")
	if err == nil {
		t.Fatal("expected error when child content is not linked from root NSC graph")
	}
	if !errors.Is(err, ErrRunGraphChildNotReachable) {
		t.Fatalf("expected ErrRunGraphChildNotReachable, got %v", err)
	}
}

func TestCollectRunSubtreeManifestExcludeKeys_MCPReadFailClosed(t *testing.T) {
	scheme := rootCaptureTestScheme(t)
	log, _ := logger.NewLogger("error")
	ctx := context.Background()

	nscRoot := &storagev1alpha1.NamespaceSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "root-nsc"},
		Status: storagev1alpha1.NamespaceSnapshotContentStatus{
			ChildrenSnapshotContentRefs: []storagev1alpha1.NamespaceSnapshotContentChildRef{{Name: "child-nsc"}},
		},
	}
	nscChild := &storagev1alpha1.NamespaceSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "child-nsc"},
		Status: storagev1alpha1.NamespaceSnapshotContentStatus{
			ManifestCheckpointName: "mcp-broken",
		},
	}
	meta.SetStatusCondition(&nscChild.Status.Conditions, metav1.Condition{
		Type: snapshot.ConditionReady, Status: metav1.ConditionTrue, Reason: "Completed",
	})
	disk := &demov1alpha1.DemoVirtualDiskSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "disk-a", Namespace: "ns1"},
		Status: demov1alpha1.DemoVirtualDiskSnapshotStatus{
			BoundSnapshotContentName: "child-nsc",
		},
	}
	rootNS := &storagev1alpha1.NamespaceSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: "ns1"},
		Status: storagev1alpha1.NamespaceSnapshotStatus{
			ChildrenSnapshotRefs: []storagev1alpha1.NamespaceSnapshotChildRef{
				{Namespace: "ns1", Name: "disk-a"},
			},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nscRoot, nscChild, disk, rootNS).Build()
	arch := NewArchiveService(cl, cl, log)

	_, err := collectRunSubtreeManifestExcludeKeys(ctx, arch, cl, rootNS, "root-nsc")
	if err == nil {
		t.Fatal("expected error when ManifestCheckpoint is missing / not readable")
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
