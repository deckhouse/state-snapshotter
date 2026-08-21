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

package transform

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

func pvcManifest(name, namespace, uid string) unstructured.Unstructured {
	meta := map[string]interface{}{
		"name": name,
	}
	if namespace != "" {
		meta["namespace"] = namespace
	}
	if uid != "" {
		meta["uid"] = uid
	}
	return unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata":   meta,
	}}
}

func dataBindingRef(targetUID, pvcName, vscName string) PVCDataBinding {
	return PVCDataBinding{
		TargetUID: targetUID,
		Target: storagev1alpha1.ObjectRef{
			APIVersion: "v1",
			Kind:       "PersistentVolumeClaim",
			Namespace:  "default",
			Name:       pvcName,
			UID:        targetUID,
		},
		Artifact: storagev1alpha1.ObjectRef{
			APIVersion: "snapshot.storage.k8s.io/v1",
			Kind:       "VolumeSnapshotContent",
			Name:       vscName,
		},
	}
}

func TestFindDataBindingForPVC_UIDMatchPriority(t *testing.T) {
	pvc := pvcManifest("data-a", "default", "uid-a")
	pvc.SetUID("uid-a")
	bindings := []PVCDataBinding{
		dataBindingRef("uid-b", "data-a", "vsc-wrong"),
		dataBindingRef("uid-a", "data-a", "vsc-a"),
	}
	got, ok := FindDataBindingForPVC(pvc, bindings)
	if !ok || got.Artifact.Name != "vsc-a" {
		t.Fatalf("expected uid-a binding, got ok=%v ref=%#v", ok, got)
	}
}

func TestFindDataBindingForPVC_WrongUIDDoesNotMatchByName(t *testing.T) {
	pvc := pvcManifest("data-a", "default", "uid-a")
	pvc.SetUID("uid-a")
	bindings := []PVCDataBinding{dataBindingRef("uid-b", "data-a", "vsc-b")}
	if _, ok := FindDataBindingForPVC(pvc, bindings); ok {
		t.Fatal("expected no match when PVC UID conflicts with binding targetUID")
	}
}

func TestFindDataBindingForPVC_IdentityFallbackWhenUIDEmpty(t *testing.T) {
	pvc := pvcManifest("data-a", "default", "")
	bindings := []PVCDataBinding{{
		Target: storagev1alpha1.ObjectRef{
			APIVersion: "v1",
			Kind:       "PersistentVolumeClaim",
			Namespace:  "default",
			Name:       "data-a",
		},
		Artifact: storagev1alpha1.ObjectRef{Kind: "VolumeSnapshotContent", Name: "vsc-a"},
	}}
	got, ok := FindDataBindingForPVC(pvc, bindings)
	if !ok || got.Artifact.Name != "vsc-a" {
		t.Fatalf("expected identity fallback match, got ok=%v ref=%#v", ok, got)
	}
}

func TestFindDataBindingForPVC_MissingBinding(t *testing.T) {
	pvc := pvcManifest("missing", "default", "uid-missing")
	pvc.SetUID("uid-missing")
	if _, ok := FindDataBindingForPVC(pvc, nil); ok {
		t.Fatal("expected no match")
	}
}
