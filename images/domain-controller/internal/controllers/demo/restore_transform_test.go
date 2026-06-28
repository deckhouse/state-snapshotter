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

package demo

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/domain-controller/internal/controllers/common"
	domainsdk "github.com/deckhouse/state-snapshotter/pkg/snapshotsdk/transform"
)

func demoDisk(name, pvcName string) unstructured.Unstructured {
	obj := map[string]interface{}{
		"apiVersion": demov1alpha1.SchemeGroupVersion.String(),
		"kind":       controllercommon.KindDemoVirtualDisk,
		"metadata":   map[string]interface{}{"name": name, "namespace": "source-ns"},
		"spec":       map[string]interface{}{},
	}
	if pvcName != "" {
		obj["spec"].(map[string]interface{})["persistentVolumeClaimName"] = pvcName
	}
	return unstructured.Unstructured{Object: obj}
}

func TestRestoreTransformer_CoveredPVCNames(t *testing.T) {
	disk := demoDisk("disk-a", "disk-a-pvc")
	covered := RestoreTransformer{}.CoveredPVCNames(nil, []unstructured.Unstructured{disk})
	if _, ok := covered["disk-a-pvc"]; !ok {
		t.Fatalf("expected disk-a-pvc to be covered, got %v", covered)
	}
}

func TestRestoreTransformer_SetsDiskDataSourceUnderDiskSnapshotNode(t *testing.T) {
	disk := demoDisk("disk-a", "disk-a-pvc")
	node := &domainsdk.RestoreNode{SnapshotRef: storagev1alpha1.ObjectRef{
		APIVersion: demov1alpha1.SchemeGroupVersion.String(),
		Kind:       controllercommon.KindDemoVirtualDiskSnapshot,
		Name:       "disk-a-snap",
		Namespace:  "source-ns",
	}}

	handled, err := RestoreTransformer{}.TransformObject(node, &disk, nil)
	if err != nil {
		t.Fatalf("TransformObject: %v", err)
	}
	if !handled {
		t.Fatal("expected DemoVirtualDisk to be handled")
	}
	name, _, _ := unstructured.NestedString(disk.Object, "spec", "dataSource", "name")
	if name != "disk-a-snap" {
		t.Fatalf("dataSource.name = %q, want disk-a-snap", name)
	}
	kind, _, _ := unstructured.NestedString(disk.Object, "spec", "dataSource", "kind")
	if kind != controllercommon.KindDemoVirtualDiskSnapshot {
		t.Fatalf("dataSource.kind = %q, want %s", kind, controllercommon.KindDemoVirtualDiskSnapshot)
	}
}

func TestRestoreTransformer_IgnoresNonDiskAndNonDiskSnapshotNodes(t *testing.T) {
	// Non-disk object is ignored.
	cm := unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]interface{}{"name": "cfg", "namespace": "source-ns"},
	}}
	node := &domainsdk.RestoreNode{SnapshotRef: storagev1alpha1.ObjectRef{Kind: controllercommon.KindDemoVirtualDiskSnapshot, Name: "s"}}
	if handled, _ := (RestoreTransformer{}).TransformObject(node, &cm, nil); handled {
		t.Fatal("ConfigMap must not be handled by the demo transformer")
	}

	// Disk under a non-disk-snapshot node is left untouched (no restore source to point at).
	disk := demoDisk("disk-a", "disk-a-pvc")
	vmNode := &domainsdk.RestoreNode{SnapshotRef: storagev1alpha1.ObjectRef{Kind: controllercommon.KindDemoVirtualMachineSnapshot, Name: "vm-snap"}}
	handled, err := RestoreTransformer{}.TransformObject(vmNode, &disk, nil)
	if err != nil {
		t.Fatalf("TransformObject: %v", err)
	}
	if handled {
		t.Fatal("disk under non-disk-snapshot node must not be handled")
	}
	if _, found, _ := unstructured.NestedMap(disk.Object, "spec", "dataSource"); found {
		t.Fatal("disk under non-disk-snapshot node must not get a dataSource")
	}
}
