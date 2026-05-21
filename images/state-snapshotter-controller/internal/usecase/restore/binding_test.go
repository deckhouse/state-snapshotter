package restore

import (
	"testing"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
