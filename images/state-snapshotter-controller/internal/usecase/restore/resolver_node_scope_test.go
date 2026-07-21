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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// TestResolveRestoreNodeOnlyRoot_IgnoresNotReadyChild proves scope=node does not read the run-tree
// children at all: the root declares a child in childrenSnapshotRefs that is NOT even seeded in the
// client (a Get on it would 409/404 in the subtree walk), yet node-only resolution succeeds and returns
// no children. This is the core backward-incompatible-with-subtree behavior of scope=node.
func TestResolveRestoreNodeOnlyRoot_IgnoresNotReadyChild(t *testing.T) {
	scheme := restoreTreeScheme()
	root := rootSnapshotObj("snap", "root-content", []storagev1alpha1.SnapshotChildRef{
		{APIVersion: demoGroupV, Kind: "DemoVirtualDiskSnapshot", Name: "disk-snap"},
	})
	rootContent := rootBoundContent("root-content", "mcp-root", "snap")
	// The child disk-snap object is intentionally absent from the client: node-only must not Get it.
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(root, rootContent).Build()

	node, err := NewResolver(cl).ResolveRestoreNodeOnlyRoot(context.Background(), "source-ns", "snap")
	if err != nil {
		t.Fatalf("ResolveRestoreNodeOnlyRoot: %v", err)
	}
	if node.ManifestCheckpointName != "mcp-root" {
		t.Fatalf("root MCP = %q, want mcp-root", node.ManifestCheckpointName)
	}
	if len(node.Children) != 0 {
		t.Fatalf("scope=node must not resolve children, got %d", len(node.Children))
	}
}

// TestResolveRestoreNodeOnlyRoot_SkipsResolvableChildren proves scope=node skips children even when they
// ARE fully resolvable: the same node returns children under a subtree walk but none under node-only.
func TestResolveRestoreNodeOnlyRoot_SkipsResolvableChildren(t *testing.T) {
	scheme := restoreTreeScheme()
	root := rootSnapshotObj("snap", "root-content", []storagev1alpha1.SnapshotChildRef{
		{APIVersion: demoGroupV, Kind: "DemoVirtualDiskSnapshot", Name: "disk-snap"},
	})
	rootContent := rootBoundContent("root-content", "mcp-root", "snap")
	diskSnap := demoDiskSnapshotObj("disk-snap", "disk-content")
	diskContent := diskSnapBoundContent("disk-content", "mcp-disk", "disk-snap")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(root, rootContent, diskSnap, diskContent).Build()

	r := NewResolver(cl)
	subtree, err := r.ResolveRestoreTree(context.Background(), "source-ns", "snap")
	if err != nil {
		t.Fatalf("ResolveRestoreTree (subtree regression guard): %v", err)
	}
	if len(subtree.Children) != 1 {
		t.Fatalf("subtree must resolve the child, got %d", len(subtree.Children))
	}

	nodeOnly, err := r.ResolveRestoreNodeOnlyRoot(context.Background(), "source-ns", "snap")
	if err != nil {
		t.Fatalf("ResolveRestoreNodeOnlyRoot: %v", err)
	}
	if len(nodeOnly.Children) != 0 {
		t.Fatalf("scope=node must skip the resolvable child, got %d", len(nodeOnly.Children))
	}
}

// TestResolveRestoreNodeOnlyRoot_NodeNotReady proves the addressed node's own Ready gate still applies at
// scope=node: a root Snapshot without a Ready=True condition fails closed with ErrNotReady.
func TestResolveRestoreNodeOnlyRoot_NodeNotReady(t *testing.T) {
	scheme := restoreTreeScheme()
	root := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "source-ns"},
		Status:     storagev1alpha1.SnapshotStatus{BoundSnapshotContentName: "root-content"},
	}
	rootContent := rootBoundContent("root-content", "mcp-root", "snap")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(root, rootContent).Build()

	_, err := NewResolver(cl).ResolveRestoreNodeOnlyRoot(context.Background(), "source-ns", "snap")
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("expected ErrNotReady for a not-ready node, got %v", err)
	}
}

// TestResolveRestoreNodeOnlyRoot_BackRefMismatchForbidden proves the anti-spoofing back-ref check still
// runs for the user-addressed root at scope=node: a bound content whose spec.snapshotRef points at a
// different subject is a 403 (Forbidden), exactly as in the subtree path.
func TestResolveRestoreNodeOnlyRoot_BackRefMismatchForbidden(t *testing.T) {
	scheme := restoreTreeScheme()
	root := rootSnapshotObj("snap", "root-content", nil)
	rootContent := rootBoundContent("root-content", "mcp-root", "other-snap")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(root, rootContent).Build()

	_, err := NewResolver(cl).ResolveRestoreNodeOnlyRoot(context.Background(), "source-ns", "snap")
	if !apierrors.IsForbidden(err) {
		t.Fatalf("expected Forbidden (403) for a back-ref mismatch, got %v", err)
	}
}

// TestResolveRestoreNodeOnlyRoot_EmptyMCPContractViolation proves the non-empty MCP invariant still holds
// at scope=node: a bound (Ready, back-ref-correct) content carrying an empty manifestCheckpointName fails
// closed with ErrContractViolation.
func TestResolveRestoreNodeOnlyRoot_EmptyMCPContractViolation(t *testing.T) {
	scheme := restoreTreeScheme()
	root := rootSnapshotObj("snap", "root-content", nil)
	rootContent := rootBoundContent("root-content", "", "snap")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(root, rootContent).Build()

	_, err := NewResolver(cl).ResolveRestoreNodeOnlyRoot(context.Background(), "source-ns", "snap")
	if !errors.Is(err, ErrContractViolation) {
		t.Fatalf("expected ErrContractViolation for an empty MCP, got %v", err)
	}
}
