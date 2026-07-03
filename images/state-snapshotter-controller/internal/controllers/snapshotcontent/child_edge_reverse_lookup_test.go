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
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// extractChildContentNamesIndex projects every status.childrenSnapshotContentRefs[].name for the field index.
func TestExtractChildContentNamesIndex(t *testing.T) {
	got := extractChildContentNamesIndex(commonContentWithStatus("parent", "", "child-a", "child-b"))
	if len(got) != 2 || got[0] != "child-a" || got[1] != "child-b" {
		t.Fatalf("with edges: got %v, want [child-a child-b]", got)
	}
	if got := extractChildContentNamesIndex(commonContentWithStatus("leaf", "")); got != nil {
		t.Fatalf("no edges: got %v, want nil", got)
	}
	if got := extractChildContentNamesIndex(&ssv1alpha1.ManifestCheckpoint{}); got != nil {
		t.Fatalf("non-unstructured: got %v, want nil", got)
	}
}

// mapChildContentToParentContentsByEdge must enqueue every parent whose published
// status.childrenSnapshotContentRefs includes the changed child's name (forward-edge reverse lookup, C-2).
// A child that no parent references, and a nil/empty object, resolve to nothing.
func TestMapChildContentToParentContentsByEdge(t *testing.T) {
	scheme := aggScheme(t)
	contentGVK := contentGVKForTest()
	indexObj := &unstructured.Unstructured{}
	indexObj.SetGroupVersionKind(contentGVK)

	// root -> [vm], vm -> [leaf]; a two-level tree so we exercise a middle parent.
	root := commonContentWithStatus("root", "", "vm")
	vm := commonContentWithStatus("vm", "", "leaf")
	leaf := commonContentWithStatus("leaf", "")

	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(indexObj, indexKeyChildContentName, extractChildContentNamesIndex).
		WithObjects(root, vm, leaf).
		Build()
	r := &SnapshotContentController{
		Client:              cl,
		APIReader:           cl,
		GVKRegistry:         snapshot.NewGVKRegistry(),
		SnapshotContentGVKs: []schema.GroupVersionKind{contentGVK},
	}

	child := func(name string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		u.SetName(name)
		u.SetGroupVersionKind(contentGVK)
		return u
	}

	// leaf change -> wakes vm (its only parent by edge).
	if reqs := r.mapChildContentToParentContentsByEdge(context.Background(), child("leaf")); len(reqs) != 1 || reqs[0].Name != "vm" {
		t.Fatalf("leaf: got %v, want one request for vm", reqs)
	}

	// vm change -> wakes root.
	if reqs := r.mapChildContentToParentContentsByEdge(context.Background(), child("vm")); len(reqs) != 1 || reqs[0].Name != "root" {
		t.Fatalf("vm: got %v, want one request for root", reqs)
	}

	// root change -> no parent references it -> nil (self-requeue backstops).
	if reqs := r.mapChildContentToParentContentsByEdge(context.Background(), child("root")); reqs != nil {
		t.Fatalf("root: got %v, want nil", reqs)
	}

	// unknown child -> nil.
	if reqs := r.mapChildContentToParentContentsByEdge(context.Background(), child("nope")); reqs != nil {
		t.Fatalf("unknown child: got %v, want nil", reqs)
	}

	// nil object -> nil.
	if reqs := r.mapChildContentToParentContentsByEdge(context.Background(), nil); reqs != nil {
		t.Fatalf("nil obj: got %v, want nil", reqs)
	}
}
