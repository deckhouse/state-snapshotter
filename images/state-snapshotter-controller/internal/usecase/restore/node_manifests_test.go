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
	"net/http"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
)

var demoVMSnapshotGVK = schema.GroupVersionKind{
	Group:   "demo.state-snapshotter.deckhouse.io",
	Version: "v1alpha1",
	Kind:    "DemoVirtualMachineSnapshot",
}

// subtreeFixture is the shared VM-rooted subtree used by the per-node manifests tests:
//
//	app (root)               cm "config"
//	└── vm-a-snap (VMSnap)    vm "vm-a"
//	    └── disk-a1-snap      disk "disk-a1" (+ pvc-a1)
//	disk-standalone-snap is a sibling of the VM under the root and must never appear in the VM subtree.
func subtreeFixture() treeNode {
	return treeNode{
		name:      "app",
		manifests: []map[string]interface{}{cmManifest("config")},
		children: []treeNode{
			{
				kind: "DemoVirtualMachineSnapshot", name: "vm-a-snap",
				manifests: []map[string]interface{}{vmManifest("vm-a", "disk-a1")},
				children: []treeNode{
					{
						kind: "DemoVirtualDiskSnapshot", name: "disk-a1-snap",
						manifests: []map[string]interface{}{diskManifest("disk-a1", "pvc-a1"), pvcManifestRaw("pvc-a1", "uid-a1")},
					},
				},
			},
			{
				kind: "DemoVirtualDiskSnapshot", name: "disk-standalone-snap",
				manifests: []map[string]interface{}{diskManifest("disk-standalone", "")},
			},
		},
	}
}

// TestBuildNodeManifestsForNode_DomainRoot proves the domain-rooted per-node manifests endpoint
// returns exactly one node's OWN manifests (not aggregated over the subtree), reachable for both the
// domain root node and a node below it, when the resolution is rooted at a domain snapshot CR.
func TestBuildNodeManifestsForNode_DomainRoot(t *testing.T) {
	svc := buildComplexService(t, subtreeFixture())
	ctx := context.Background()

	// The domain root's own manifests: just vm-a, never the child disk.
	out, err := svc.BuildNodeManifestsForNode(ctx, demoVMSnapshotGVK, "source-ns", "vm-a-snap",
		"DemoVirtualMachineSnapshot--source-ns--vm-a-snap")
	if err != nil {
		t.Fatalf("BuildNodeManifestsForNode(root): %v", err)
	}
	objs := decodeObjects(t, out)
	if !hasObj(objs, "DemoVirtualMachine", "vm-a") {
		t.Fatalf("VM node own manifests must contain vm-a, got %v", objs)
	}
	if hasObj(objs, "DemoVirtualDisk", "disk-a1") {
		t.Fatal("per-node manifests must be this node's OWN objects, not the child disk")
	}

	// A child node below the domain root, reached via ?node=<child id>: its own disk + covered PVC.
	childOut, err := svc.BuildNodeManifestsForNode(ctx, demoVMSnapshotGVK, "source-ns", "vm-a-snap",
		"DemoVirtualDiskSnapshot--source-ns--disk-a1-snap")
	if err != nil {
		t.Fatalf("BuildNodeManifestsForNode(child): %v", err)
	}
	childObjs := decodeObjects(t, childOut)
	if !hasObj(childObjs, "DemoVirtualDisk", "disk-a1") {
		t.Fatalf("child node own manifests must contain disk-a1, got %v", childObjs)
	}
	if hasObj(childObjs, "DemoVirtualMachine", "vm-a") {
		t.Fatal("child node manifests must not contain the parent VM")
	}
}

// TestBuildNodeManifestsForNode_UnknownNode proves a node id outside the resolved subtree fails as a
// typed 404 (e.g. a sibling subtree's node is not reachable from the VM root).
func TestBuildNodeManifestsForNode_UnknownNode(t *testing.T) {
	svc := buildComplexService(t, subtreeFixture())

	_, err := svc.BuildNodeManifestsForNode(context.Background(), demoVMSnapshotGVK, "source-ns", "vm-a-snap",
		"DemoVirtualDiskSnapshot--source-ns--disk-standalone-snap")
	if err == nil {
		t.Fatal("expected NotFound for a node outside the resolved subtree")
	}
	var st *usecase.AggregatedStatusError
	if !errors.As(err, &st) || st.HTTPStatus != http.StatusNotFound {
		t.Fatalf("expected 404 AggregatedStatusError, got %v", err)
	}
}

// TestBuildNodeManifestsForNode_EmptyNodeSelector proves an empty node selector is a typed 400.
func TestBuildNodeManifestsForNode_EmptyNodeSelector(t *testing.T) {
	svc := buildComplexService(t, subtreeFixture())

	_, err := svc.BuildNodeManifestsForNode(context.Background(), demoVMSnapshotGVK, "source-ns", "vm-a-snap", "")
	var st *usecase.AggregatedStatusError
	if !errors.As(err, &st) || st.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("expected 400 AggregatedStatusError, got %v", err)
	}
}
