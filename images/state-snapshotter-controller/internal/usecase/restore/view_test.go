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
	"encoding/json"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

func decodeView(t *testing.T, raw []byte) *SnapshotView {
	t.Helper()
	v := &SnapshotView{}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("unmarshal view: %v", err)
	}
	return v
}

func viewChild(n SnapshotViewNode, kind, name string) *SnapshotViewNode {
	for i := range n.Children {
		if n.Children[i].Kind == kind && n.Children[i].Name == name {
			return &n.Children[i]
		}
	}
	return nil
}

// TestBuildView_RootTree proves the root Snapshot view is a nested tree carrying every node's GVK and
// name (presentation fields only), reachable from the namespaced root Snapshot.
func TestBuildView_RootTree(t *testing.T) {
	svc := buildComplexService(t, subtreeFixture())

	raw, err := svc.BuildView(context.Background(), Options{SnapshotName: "app", SnapshotNamespace: "source-ns"})
	if err != nil {
		t.Fatalf("BuildView: %v", err)
	}
	view := decodeView(t, raw)
	if view.Version != SnapshotViewVersion {
		t.Fatalf("view version = %q, want %q", view.Version, SnapshotViewVersion)
	}
	if view.Root.Kind != "Snapshot" || view.Root.Name != "app" {
		t.Fatalf("root = %s/%s, want Snapshot/app", view.Root.Kind, view.Root.Name)
	}
	vm := viewChild(view.Root, "DemoVirtualMachineSnapshot", "vm-a-snap")
	if vm == nil {
		t.Fatalf("root must list the VM child, children=%+v", view.Root.Children)
	}
	if viewChild(view.Root, "DemoVirtualDiskSnapshot", "disk-standalone-snap") == nil {
		t.Fatalf("root must list the standalone disk child, children=%+v", view.Root.Children)
	}
	if viewChild(*vm, "DemoVirtualDiskSnapshot", "disk-a1-snap") == nil {
		t.Fatalf("VM must list its disk child, children=%+v", vm.Children)
	}
}

// TestBuildViewForNode_Subtree proves the domain-rooted view contains only the selected subtree (the
// sibling standalone disk under the root must not appear).
func TestBuildViewForNode_Subtree(t *testing.T) {
	svc := buildComplexService(t, subtreeFixture())

	raw, err := svc.BuildViewForNode(context.Background(), demoVMSnapshotGVK, Options{SnapshotName: "vm-a-snap", SnapshotNamespace: "source-ns"})
	if err != nil {
		t.Fatalf("BuildViewForNode: %v", err)
	}
	view := decodeView(t, raw)
	if view.Root.Kind != "DemoVirtualMachineSnapshot" || view.Root.Name != "vm-a-snap" {
		t.Fatalf("subtree root = %s/%s, want DemoVirtualMachineSnapshot/vm-a-snap", view.Root.Kind, view.Root.Name)
	}
	if viewChild(view.Root, "DemoVirtualDiskSnapshot", "disk-a1-snap") == nil {
		t.Fatalf("subtree root must list its disk child, children=%+v", view.Root.Children)
	}
	if viewChild(view.Root, "DemoVirtualDiskSnapshot", "disk-standalone-snap") != nil {
		t.Fatal("subtree view must not include the sibling standalone disk")
	}
}

// TestBuildView_DataNode proves a data node surfaces hasData + volumeMode + the size read from the
// source VolumeSnapshotContent restoreSize.
func TestBuildView_DataNode(t *testing.T) {
	scheme := restoreTreeScheme()
	vscGVK := schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotContent"}
	scheme.AddKnownTypeWithName(vscGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: vscGVK.Group, Version: vscGVK.Version, Kind: "VolumeSnapshotContentList"}, &unstructured.UnstructuredList{})

	root := rootSnapshotObj("vroot", "root-content", nil)
	content := readySnapshotContent("root-content", "mcp-root", []storagev1alpha1.SnapshotDataBinding{{
		Target:     storagev1alpha1.SnapshotSubjectRef{APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc", Namespace: "source-ns"},
		Artifact:   storagev1alpha1.SnapshotDataArtifactRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: "vsc-1"},
		VolumeMode: "Block",
	}})
	vsc := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "snapshot.storage.k8s.io/v1",
		"kind":       "VolumeSnapshotContent",
		"metadata":   map[string]interface{}{"name": "vsc-1"},
		"status":     map[string]interface{}{"restoreSize": int64(12345)},
	}}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(root, content, vsc).Build()
	log, _ := logger.NewLogger("error")
	svc := NewService(cl, usecase.NewArchiveService(cl, cl, log), &diskRestoreStub{}, &vmRestoreStub{})

	raw, err := svc.BuildView(context.Background(), Options{SnapshotName: "vroot", SnapshotNamespace: "source-ns"})
	if err != nil {
		t.Fatalf("BuildView: %v", err)
	}
	view := decodeView(t, raw)
	if !view.Root.HasData || view.Root.VolumeMode != "Block" {
		t.Fatalf("root data fields not set: %+v", view.Root)
	}
	if view.Root.SizeBytes != 12345 {
		t.Fatalf("size = %d, want 12345 (from VSC restoreSize)", view.Root.SizeBytes)
	}
}
