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

package controllers

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

func TestMapSnapshotContentToSnapshot_AllowsClusterSnapshotWithoutNamespace(t *testing.T) {
	registry := snapshot.NewGVKRegistry()
	if err := registry.RegisterSnapshotGVK("ClusterSnapshot", "storage.deckhouse.io/v1alpha1"); err != nil {
		t.Fatalf("register snapshot gvk: %v", err)
	}
	if err := registry.RegisterSnapshotContentGVK("ClusterSnapshotContent", "storage.deckhouse.io/v1alpha1"); err != nil {
		t.Fatalf("register content gvk: %v", err)
	}

	controller := &GenericSnapshotBinderController{
		GVKRegistry:  registry,
		SnapshotGVKs: []schema.GroupVersionKind{{Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "ClusterSnapshot"}},
	}

	contentObj := &unstructured.Unstructured{}
	contentObj.Object = map[string]interface{}{
		"apiVersion": "storage.deckhouse.io/v1alpha1",
		"kind":       "ClusterSnapshotContent",
		"spec": map[string]interface{}{
			"snapshotRef": map[string]interface{}{
				"kind": "ClusterSnapshot",
				"name": "root-snapshot",
				// namespace intentionally omitted
			},
		},
	}
	contentObj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "storage.deckhouse.io",
		Version: "v1alpha1",
		Kind:    "ClusterSnapshotContent",
	})
	contentObj.SetName("content-1")

	reqs := controller.mapSnapshotContentToSnapshot(context.Background(), contentObj)
	if len(reqs) != 1 {
		t.Fatalf("expected 1 request, got %d", len(reqs))
	}
	expected := types.NamespacedName{Name: "root-snapshot", Namespace: ""}
	if reqs[0].NamespacedName != expected {
		t.Fatalf("unexpected request: %+v (expected %+v)", reqs[0].NamespacedName, expected)
	}
}

func TestMapSnapshotContentToSnapshot_MissingSnapshotRefKindSkips(t *testing.T) {
	registry := snapshot.NewGVKRegistry()
	controller := &GenericSnapshotBinderController{
		GVKRegistry: registry,
	}

	contentObj := &unstructured.Unstructured{}
	contentObj.Object = map[string]interface{}{
		"apiVersion": "storage.deckhouse.io/v1alpha1",
		"kind":       "SnapshotContent",
		"spec": map[string]interface{}{
			"snapshotRef": map[string]interface{}{
				"name":      "snapshot-1",
				"namespace": "default",
			},
		},
	}
	contentObj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "storage.deckhouse.io",
		Version: "v1alpha1",
		Kind:    "SnapshotContent",
	})
	contentObj.SetName("content-1")

	reqs := controller.mapSnapshotContentToSnapshot(context.Background(), contentObj)
	if len(reqs) != 0 {
		t.Fatalf("expected no requests, got %d", len(reqs))
	}
}
