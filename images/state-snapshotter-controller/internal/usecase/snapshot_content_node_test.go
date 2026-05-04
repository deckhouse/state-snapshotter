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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

func TestSnapshotContentNodeFromSnapshotContent(t *testing.T) {
	sc := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "content"},
		Status: storagev1alpha1.SnapshotContentStatus{
			ManifestCheckpointName: "mcp",
			ChildrenSnapshotContentRefs: []storagev1alpha1.SnapshotContentChildRef{
				{Name: "child"},
			},
		},
	}

	node := snapshotContentNodeFromSnapshotContent(sc)
	if node.GVK != SnapshotContentGVK() || node.Name != "content" || node.ManifestCheckpointName != "mcp" {
		t.Fatalf("unexpected node: %#v", node)
	}
	if len(node.Children) != 1 || node.Children[0].Name != "child" {
		t.Fatalf("unexpected children: %#v", node.Children)
	}
}

func TestSnapshotContentNodeFromNamespaceSnapshotContent(t *testing.T) {
	nsc := &storagev1alpha1.NamespaceSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "root-content"},
		Status: storagev1alpha1.NamespaceSnapshotContentStatus{
			ManifestCheckpointName: "mcp-root",
			ChildrenSnapshotContentRefs: []storagev1alpha1.NamespaceSnapshotContentChildRef{
				{Name: "child-content"},
			},
		},
	}

	node := snapshotContentNodeFromNamespaceSnapshotContent(nsc)
	if node.GVK != NamespaceSnapshotContentGVK() || node.Name != "root-content" || node.ManifestCheckpointName != "mcp-root" {
		t.Fatalf("unexpected node: %#v", node)
	}
	if len(node.Children) != 1 || node.Children[0].Name != "child-content" {
		t.Fatalf("unexpected children: %#v", node.Children)
	}
}

func TestSnapshotContentNodeFromUnstructured(t *testing.T) {
	gvk := schema.GroupVersionKind{
		Group:   "demo.state-snapshotter.deckhouse.io",
		Version: "v1alpha1",
		Kind:    "DemoVirtualMachineSnapshotContent",
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetName("vm-content")
	u.Object["status"] = map[string]interface{}{
		"manifestCheckpointName": "mcp-vm",
		"childrenSnapshotContentRefs": []interface{}{
			map[string]interface{}{"name": "disk-content"},
		},
	}

	node, err := snapshotContentNodeFromUnstructured(u)
	if err != nil {
		t.Fatalf("adapter failed: %v", err)
	}
	if node.GVK != gvk || node.Name != "vm-content" || node.ManifestCheckpointName != "mcp-vm" {
		t.Fatalf("unexpected node: %#v", node)
	}
	if len(node.Children) != 1 || node.Children[0].Name != "disk-content" {
		t.Fatalf("unexpected children: %#v", node.Children)
	}
}
