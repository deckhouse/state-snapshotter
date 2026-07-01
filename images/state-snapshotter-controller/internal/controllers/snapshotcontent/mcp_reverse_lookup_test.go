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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

func contentGVKForTest() schema.GroupVersionKind {
	return schema.GroupVersionKind{
		Group:   storagev1alpha1.SchemeGroupVersion.Group,
		Version: storagev1alpha1.SchemeGroupVersion.Version,
		Kind:    "SnapshotContent",
	}
}

// extractManifestCheckpointNameIndex projects status.manifestCheckpointName for the field index.
func TestExtractManifestCheckpointNameIndex(t *testing.T) {
	if got := extractManifestCheckpointNameIndex(commonContentWithStatus("c", "mcp-1")); len(got) != 1 || got[0] != "mcp-1" {
		t.Fatalf("with name: got %v, want [mcp-1]", got)
	}
	if got := extractManifestCheckpointNameIndex(commonContentWithStatus("c", "")); got != nil {
		t.Fatalf("empty name: got %v, want nil", got)
	}
	if got := extractManifestCheckpointNameIndex(&ssv1alpha1.ManifestCheckpoint{}); got != nil {
		t.Fatalf("non-unstructured: got %v, want nil", got)
	}
}

// mapManifestCheckpointToContent must resolve via ownerRef (path 1) when present, and fall back to the
// pre-adoption reverse lookup by status.manifestCheckpointName (path 2) when the MCP has no SnapshotContent
// ownerRef yet. An unknown name resolves to nothing (self-requeue backstops).
func TestMapManifestCheckpointToContent_DualPath(t *testing.T) {
	scheme := aggScheme(t)
	contentGVK := contentGVKForTest()
	indexObj := &unstructured.Unstructured{}
	indexObj.SetGroupVersionKind(contentGVK)

	content := commonContentWithStatus("owning-content", "mcp-1")
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(indexObj, indexKeyManifestCheckpointName, extractManifestCheckpointNameIndex).
		WithObjects(content).
		Build()
	r := &SnapshotContentController{
		Client:              cl,
		APIReader:           cl,
		GVKRegistry:         snapshot.NewGVKRegistry(),
		SnapshotContentGVKs: []schema.GroupVersionKind{contentGVK},
	}

	mcp := func(name string, refs []metav1.OwnerReference) *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		u.SetName(name)
		u.SetGroupVersionKind(unstructuredGVKForKind("ManifestCheckpoint"))
		if refs != nil {
			u.SetOwnerReferences(refs)
		}
		return u
	}
	ctrlTrue := true

	// path 1: ownerRef wins (distinct name proves we did not fall through to reverse lookup).
	owned := mcp("mcp-1", []metav1.OwnerReference{{
		APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
		Kind:       "SnapshotContent",
		Name:       "owner-via-ref",
		UID:        "u",
		Controller: &ctrlTrue,
	}})
	if reqs := r.mapManifestCheckpointToContent(context.Background(), owned); len(reqs) != 1 || reqs[0].Name != "owner-via-ref" {
		t.Fatalf("path1 (ownerRef): got %v, want one request for owner-via-ref", reqs)
	}

	// path 2: no ownerRef -> reverse lookup by deterministic name.
	if reqs := r.mapManifestCheckpointToContent(context.Background(), mcp("mcp-1", nil)); len(reqs) != 1 || reqs[0].Name != "owning-content" {
		t.Fatalf("path2 (reverse lookup): got %v, want one request for owning-content", reqs)
	}

	// path 2 miss: unknown checkpoint name -> nil (self-requeue backstops).
	if reqs := r.mapManifestCheckpointToContent(context.Background(), mcp("mcp-unknown", nil)); reqs != nil {
		t.Fatalf("path2 miss: got %v, want nil", reqs)
	}

	// nil object -> nil.
	if reqs := r.mapManifestCheckpointToContent(context.Background(), nil); reqs != nil {
		t.Fatalf("nil obj: got %v, want nil", reqs)
	}
}
