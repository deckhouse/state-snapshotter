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

package namespacemanifest

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
)

func TestBuildManifestCaptureTargets_EmptyNamespaceHasNoTargets(t *testing.T) {
	listKinds := make(map[schema.GroupVersionResource]string, len(n2aNamespacedGVR))
	for _, gvr := range n2aNamespacedGVR {
		listKinds[gvr] = gvr.Resource + "List"
	}
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(k8sruntime.NewScheme(), listKinds)

	targets, err := BuildManifestCaptureTargets(context.Background(), dyn, "ns1")
	if err != nil {
		t.Fatalf("BuildManifestCaptureTargets: %v", err)
	}
	if len(targets) != 0 {
		t.Fatalf("expected no targets in empty namespace, got %#v", targets)
	}
}

func TestBuildManifestCaptureTargets_PVCCapturedVolumeSnapshotExcluded(t *testing.T) {
	volumeSnapshotsGVR := schema.GroupVersionResource{Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshots"}
	listKinds := make(map[schema.GroupVersionResource]string, len(n2aNamespacedGVR)+1)
	for _, gvr := range n2aNamespacedGVR {
		listKinds[gvr] = gvr.Resource + "List"
	}
	listKinds[volumeSnapshotsGVR] = "VolumeSnapshotList"

	pvc := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata":   map[string]interface{}{"name": "pvc-a", "namespace": "ns1"},
	}}
	vs := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "snapshot.storage.k8s.io/v1",
		"kind":       "VolumeSnapshot",
		"metadata":   map[string]interface{}{"name": "nss-vs-a", "namespace": "ns1"},
	}}
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(k8sruntime.NewScheme(), listKinds, pvc, vs)

	targets, err := BuildManifestCaptureTargets(context.Background(), dyn, "ns1")
	if err != nil {
		t.Fatalf("BuildManifestCaptureTargets: %v", err)
	}
	var pvcFound bool
	for _, target := range targets {
		if target.Kind == "VolumeSnapshot" {
			t.Fatalf("VolumeSnapshot must never enter manifest inventory, got %#v", target)
		}
		if target.APIVersion == "v1" && target.Kind == "PersistentVolumeClaim" && target.Name == "pvc-a" {
			pvcFound = true
		}
	}
	if !pvcFound {
		t.Fatalf("PVC manifest must remain in inventory, got %#v", targets)
	}
}

func TestBuildManifestCaptureTargets_SkipsControllerOwnedDependents(t *testing.T) {
	listKinds := make(map[schema.GroupVersionResource]string, len(n2aNamespacedGVR))
	for _, gvr := range n2aNamespacedGVR {
		listKinds[gvr] = gvr.Resource + "List"
	}

	// Backing Pod owned by a controller (mirrors the demo VM's Pod): must be skipped.
	ownedPod := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]interface{}{
			"name":      "demo-vm-pod",
			"namespace": "ns1",
			"ownerReferences": []interface{}{map[string]interface{}{
				"apiVersion": "demo.state-snapshotter.deckhouse.io/v1alpha1",
				"kind":       "DemoVirtualMachine",
				"name":       "vm",
				"uid":        "vm-uid",
				"controller": true,
			}},
		},
	}}
	// Standalone Pod created directly by a user (no controller owner): must be kept.
	standalonePod := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   map[string]interface{}{"name": "standalone-pod", "namespace": "ns1"},
	}}

	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(k8sruntime.NewScheme(), listKinds, ownedPod, standalonePod)

	targets, err := BuildManifestCaptureTargets(context.Background(), dyn, "ns1")
	if err != nil {
		t.Fatalf("BuildManifestCaptureTargets: %v", err)
	}
	var standaloneFound bool
	for _, target := range targets {
		if target.Kind == "Pod" && target.Name == "demo-vm-pod" {
			t.Fatalf("controller-owned dependent Pod must be skipped, got %#v", target)
		}
		if target.Kind == "Pod" && target.Name == "standalone-pod" {
			standaloneFound = true
		}
	}
	if !standaloneFound {
		t.Fatalf("standalone Pod (no controller owner) must be kept, got %#v", targets)
	}
}

func TestIsForbiddenManifestTargetRejectsVolumeSnapshot(t *testing.T) {
	if !isForbiddenManifestTarget("snapshot.storage.k8s.io/v1", "VolumeSnapshot") {
		t.Fatal("VolumeSnapshot must never be captured into ManifestCheckpoint inventory")
	}
	if isForbiddenManifestTarget("v1", "PersistentVolumeClaim") {
		t.Fatal("PVC manifest must remain eligible for ManifestCheckpoint inventory")
	}
}
