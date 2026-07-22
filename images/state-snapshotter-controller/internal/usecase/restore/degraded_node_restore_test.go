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

package restore

import (
	"context"
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// degradedRootSnapshotObj builds root Snapshot "snap"/source-ns bound to "root-content" whose own Ready
// condition is False with the given reason, plus optional child refs. It is the not-ready counterpart of
// rootSnapshotObj, used to drive the scope=node degraded-relax gate (ensureSnapshotReadyOrDegraded).
func degradedRootSnapshotObj(reason string, children []storagev1alpha1.SnapshotChildRef) *storagev1alpha1.Snapshot {
	s := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "source-ns"},
		Status:     storagev1alpha1.SnapshotStatus{BoundSnapshotContentName: "root-content", ChildrenSnapshotRefs: children},
	}
	meta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{Type: snapshot.ConditionReady, Status: metav1.ConditionFalse, Reason: reason})
	return s
}

// rootContentWithData builds the Ready root SnapshotContent "root-content" carrying the anti-spoofing
// back-reference to root Snapshot "snap" (source-ns) plus a single dataRef, so the resolved node exposes
// both an MCP ("mcp-root") and a DataBinding leg. It lets the degraded-root case prove the node still
// carries its own restore legs.
func rootContentWithData() *storagev1alpha1.SnapshotContent {
	c := readySnapshotContent("root-content", "mcp-root", &storagev1alpha1.SnapshotDataBinding{
		SourceRef:   storagev1alpha1.SnapshotSubjectRef{APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "root-pvc", Namespace: "source-ns", UID: "uid-root"},
		ArtifactRef: storagev1alpha1.SnapshotDataArtifactRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: "vsc-root"},
	})
	c.Spec.SnapshotRef = &storagev1alpha1.SnapshotSubjectRef{
		APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
		Kind:       "Snapshot",
		Namespace:  "source-ns",
		Name:       "snap",
	}
	return c
}

// Case 1: a soft-degraded root (Ready=False / ChildSnapshotDeleted, a DegradedReadyReasons member) is
// accepted at scope=node. The declared child is intentionally NOT seeded, so a subtree walk would fail —
// proving node-only never reads children — and the resolved node still carries its own MCP + DataBinding.
func TestResolveRestoreNodeOnlyRoot_DegradedRootChildSnapshotDeletedSucceeds(t *testing.T) {
	scheme := restoreTreeScheme()
	root := degradedRootSnapshotObj(storagev1alpha1.ReasonChildSnapshotDeleted, []storagev1alpha1.SnapshotChildRef{
		{APIVersion: demoGroupV, Kind: "DemoVirtualDiskSnapshot", Name: "disk-snap"},
	})
	rootContent := rootContentWithData()
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(root, rootContent).Build()

	node, err := NewResolver(cl).ResolveRestoreNodeOnlyRoot(context.Background(), "source-ns", "snap")
	if err != nil {
		t.Fatalf("degraded root at scope=node must succeed, got %v", err)
	}
	if node.ManifestCheckpointName != "mcp-root" {
		t.Fatalf("root MCP = %q, want mcp-root", node.ManifestCheckpointName)
	}
	if len(node.Children) != 0 {
		t.Fatalf("scope=node must not read children, got %d", len(node.Children))
	}
	if len(node.DataBindings) != 1 || node.DataBindings[0].Artifact.Name != "vsc-root" {
		t.Fatalf("degraded root node must still carry its own DataBindings, got %#v", node.DataBindings)
	}
}

// Case 2 (subtree regression): the SAME degraded root fails closed under a subtree walk. scope=subtree
// (and the default) keeps the strict Ready=True gate — a degraded snapshot is not compiled as a whole.
func TestResolveRestoreTree_DegradedRootChildSnapshotDeletedFailsClosed(t *testing.T) {
	scheme := restoreTreeScheme()
	root := degradedRootSnapshotObj(storagev1alpha1.ReasonChildSnapshotDeleted, nil)
	rootContent := rootContentWithData()
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(root, rootContent).Build()

	_, err := NewResolver(cl).ResolveRestoreTree(context.Background(), "source-ns", "snap")
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("degraded root at scope=subtree must fail closed with ErrNotReady, got %v", err)
	}
}

// Case 3: a Ready=False root with a PROCESSING reason (DataCapturePending, not in DegradedReadyReasons)
// is NOT relaxed even at scope=node — a mid-capture node must never be compiled.
func TestResolveRestoreNodeOnlyRoot_ProcessingReasonFailsClosed(t *testing.T) {
	scheme := restoreTreeScheme()
	root := degradedRootSnapshotObj(snapshot.ReasonDataCapturePending, nil)
	rootContent := rootContentWithData()
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(root, rootContent).Build()

	_, err := NewResolver(cl).ResolveRestoreNodeOnlyRoot(context.Background(), "source-ns", "snap")
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("processing-reason root at scope=node must fail closed with ErrNotReady, got %v", err)
	}
}

// Case 4: a Ready=False root with a TERMINAL reason (ChildSnapshotLost) is NOT relaxed even at
// scope=node — the terminal set is disjoint from DegradedReadyReasons and stays fail-closed.
func TestResolveRestoreNodeOnlyRoot_TerminalReasonFailsClosed(t *testing.T) {
	scheme := restoreTreeScheme()
	root := degradedRootSnapshotObj(storagev1alpha1.ReasonChildSnapshotLost, nil)
	rootContent := rootContentWithData()
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(root, rootContent).Build()

	_, err := NewResolver(cl).ResolveRestoreNodeOnlyRoot(context.Background(), "source-ns", "snap")
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("terminal-reason root at scope=node must fail closed with ErrNotReady, got %v", err)
	}
}

// Case 5: the degraded-relax touches ONLY the snapshot node's own Ready condition — the bound
// SnapshotContent readiness gate still applies. A soft-degraded root whose bound content is NOT Ready
// fails closed with ErrNotReady.
func TestResolveRestoreNodeOnlyRoot_DegradedRootBoundContentNotReady(t *testing.T) {
	scheme := restoreTreeScheme()
	root := degradedRootSnapshotObj(storagev1alpha1.ReasonChildSnapshotDeleted, nil)
	rootContent := rootBoundContent("root-content", "mcp-root", "snap")
	meta.SetStatusCondition(&rootContent.Status.Conditions, metav1.Condition{Type: snapshot.ConditionReady, Status: metav1.ConditionFalse, Reason: snapshot.ReasonDataCapturePending})
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(root, rootContent).Build()

	_, err := NewResolver(cl).ResolveRestoreNodeOnlyRoot(context.Background(), "source-ns", "snap")
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("degraded root with a not-Ready bound content must fail closed with ErrNotReady, got %v", err)
	}
}

// Case 6 (Service level): a degraded root at scope=node compiles its own manifests through the full
// restore-safe + object-filter path and returns exactly the requested object, namespace-rewritten.
func TestScopeNode_DegradedRootFilterReturnsObject(t *testing.T) {
	svc := newRootNodeServiceReady(t, []map[string]interface{}{cm("app-config"), cm("other")}, nil,
		metav1.Condition{Type: snapshot.ConditionReady, Status: metav1.ConditionFalse, Reason: storagev1alpha1.ReasonChildSnapshotDeleted})

	out, err := svc.BuildManifestsWithDataRestoration(context.Background(), Options{
		SnapshotName: "snap", SnapshotNamespace: "source-ns", TargetNamespace: "restore-ns",
		Scope: ScopeNode, FilterKind: "ConfigMap", FilterName: "app-config",
	})
	if err != nil {
		t.Fatalf("degraded root scope=node filter build: %v", err)
	}
	objs := decodeObjects(t, out)
	if len(objs) != 1 {
		t.Fatalf("filter on a degraded root must return exactly one object, got %d", len(objs))
	}
	u := unstructured.Unstructured{Object: objs[0]}
	if u.GetKind() != "ConfigMap" || u.GetName() != "app-config" {
		t.Fatalf("filter returned %s/%s, want ConfigMap/app-config", u.GetKind(), u.GetName())
	}
	if u.GetNamespace() != "restore-ns" {
		t.Fatalf("filtered object namespace = %q, want restore-ns", u.GetNamespace())
	}
}
