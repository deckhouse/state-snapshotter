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
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
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

func dataBindingRef(targetUID, pvcName, vscName string) snapshot.DataBindingRef {
	return snapshot.DataBindingRef{
		TargetUID: targetUID,
		Target: snapshot.ObjectRef{
			APIVersion: "v1",
			Kind:       "PersistentVolumeClaim",
			Namespace:  "default",
			Name:       pvcName,
			UID:        targetUID,
		},
		Artifact: snapshot.ObjectRef{
			APIVersion: "snapshot.storage.k8s.io/v1",
			Kind:       "VolumeSnapshotContent",
			Name:       vscName,
		},
	}
}

func TestFindDataBindingForPVC_UIDMatchPriority(t *testing.T) {
	pvc := pvcManifest("data-a", "default", "uid-a")
	pvc.SetUID("uid-a")
	bindings := []snapshot.DataBindingRef{
		dataBindingRef("uid-b", "data-a", "vsc-wrong"),
		dataBindingRef("uid-a", "data-a", "vsc-a"),
	}
	got, ok := findDataBindingForPVC(pvc, bindings)
	if !ok || got.Artifact.Name != "vsc-a" {
		t.Fatalf("expected uid-a binding, got ok=%v ref=%#v", ok, got)
	}
}

func TestFindDataBindingForPVC_WrongUIDDoesNotMatchByName(t *testing.T) {
	pvc := pvcManifest("data-a", "default", "uid-a")
	pvc.SetUID("uid-a")
	bindings := []snapshot.DataBindingRef{dataBindingRef("uid-b", "data-a", "vsc-b")}
	if _, ok := findDataBindingForPVC(pvc, bindings); ok {
		t.Fatal("expected no match when PVC UID conflicts with binding targetUID")
	}
}

func TestFindDataBindingForPVC_IdentityFallbackWhenUIDEmpty(t *testing.T) {
	pvc := pvcManifest("data-a", "default", "")
	bindings := []snapshot.DataBindingRef{{
		Target: snapshot.ObjectRef{
			APIVersion: "v1",
			Kind:       "PersistentVolumeClaim",
			Namespace:  "default",
			Name:       "data-a",
		},
		Artifact: snapshot.ObjectRef{Kind: "VolumeSnapshotContent", Name: "vsc-a"},
	}}
	got, ok := findDataBindingForPVC(pvc, bindings)
	if !ok || got.Artifact.Name != "vsc-a" {
		t.Fatalf("expected identity fallback match, got ok=%v ref=%#v", ok, got)
	}
}

func TestFindDataBindingForPVC_MissingBinding(t *testing.T) {
	pvc := pvcManifest("missing", "default", "uid-missing")
	pvc.SetUID("uid-missing")
	if _, ok := findDataBindingForPVC(pvc, nil); ok {
		t.Fatal("expected no match")
	}
}
