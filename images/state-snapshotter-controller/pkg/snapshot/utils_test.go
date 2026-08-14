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

package snapshot

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// contentWithStatusData wraps an unstructured SnapshotContent carrying data as status.data (nil writes no
// status at all) through the production reader, so these tests exercise the real wire parser.
func contentWithStatusData(t *testing.T, data map[string]interface{}) SnapshotContentLike {
	t.Helper()
	obj := &unstructured.Unstructured{Object: map[string]interface{}{}}
	obj.SetName("content-a")
	if data != nil {
		if err := unstructured.SetNestedMap(obj.Object, data, "status", "data"); err != nil {
			t.Fatalf("set status.data: %v", err)
		}
	}
	like, err := ExtractSnapshotContentLike(obj)
	if err != nil {
		t.Fatalf("ExtractSnapshotContentLike: %v", err)
	}
	return like
}

// The wire shape of status.data can be wider than DataBindingRef: a key the struct does not model must be
// IGNORED, never surfaced and never an error. Two such keys are in the fixture. `size` is a live one — the
// schema declares it and this projection deliberately does not carry it. `accessModes` is a dropped one, kept
// so that a parser which starts filling a re-added field is caught here.
//
// The tolerance is NOT a promise that a dropped key still arrives from the apiserver: a v1 CRD prunes unknown
// keys on READ as well as on write, so an `accessModes` left in etcd by a pre-removal controller is
// unreachable through the API while the served schema omits the key. The stored value survives until the
// object's next write, and re-adding the property to the schema before then would make it visible again. The
// keys in this fixture reach the parser for a different reason: it is assembled by hand and passes no pruning
// decoder. What the parser must survive is divergence between schema and code (a rolling update where CRD and
// binary disagree, a client writing more than we model) and exactly such non-pruning decode paths.
//
// The whole DataBindingRef is compared rather than selected fields — it holds no slice, so == covers every
// field there is. That is what makes this a real check in both directions: a parser that started filling a
// re-added field would disagree with want, and so would one that stopped filling a field it should.
func TestGetStatusDataRefs_ParsesTheModelledFieldsAndIgnoresTheRest(t *testing.T) {
	like := contentWithStatusData(t, map[string]interface{}{
		"sourceRef": map[string]interface{}{
			"apiVersion": "v1", "kind": "PersistentVolumeClaim", "name": "pvc-a",
			"namespace": "ns1", "uid": "pvc-uid-a",
		},
		"artifactRef": map[string]interface{}{
			"apiVersion": "snapshot.storage.k8s.io/v1", "kind": "VolumeSnapshotContent",
			"name": "vsc-a", "uid": "vsc-uid-a",
		},
		"volumeMode":       "Block",
		"fsType":           "ext4",
		"storageClassName": "sc-a",
		"size":             "10Gi",
		"accessModes":      []interface{}{"ReadWriteOnce"},
	})

	want := DataBindingRef{
		TargetUID:        "pvc-uid-a",
		Target:           ObjectRef{APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc-a", Namespace: "ns1", UID: "pvc-uid-a"},
		Artifact:         ObjectRef{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: "vsc-a", UID: "vsc-uid-a"},
		VolumeMode:       "Block",
		FsType:           "ext4",
		StorageClassName: "sc-a",
	}

	refs := like.GetStatusDataRefs()
	// Variant A: status.data is a single object, returned as a 0/1-length slice.
	if len(refs) != 1 {
		t.Fatalf("a content with status.data must yield exactly one binding, got %d: %#v", len(refs), refs)
	}
	if refs[0] != want {
		t.Fatalf("parsed binding = %#v, want %#v", refs[0], want)
	}
}

// No status.data at all yields NO binding, not a zero-valued one: readiness and coverage helpers treat a
// present binding as a promise that an artifact exists.
func TestGetStatusDataRefs_NoDataYieldsNoBindings(t *testing.T) {
	if refs := contentWithStatusData(t, nil).GetStatusDataRefs(); len(refs) != 0 {
		t.Fatalf("a content without status.data must yield no bindings, got %#v", refs)
	}
}
