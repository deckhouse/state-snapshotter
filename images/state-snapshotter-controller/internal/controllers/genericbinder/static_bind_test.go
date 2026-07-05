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

package genericbinder

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

func domainStaticBindObj(mode string) *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "demo.state-snapshotter.deckhouse.io/v1alpha1",
		"kind":       "DemoVirtualDiskSnapshot",
		"metadata": map[string]interface{}{
			"name":      "disk-snap",
			"namespace": "project-a",
			"uid":       "domain-uid-1",
		},
		"spec": map[string]interface{}{},
	}}
	if mode != "" {
		_ = unstructured.SetNestedField(o.Object, mode, "spec", "mode")
	}
	return o
}

func TestSnapshotIsStaticBind(t *testing.T) {
	cases := []struct {
		name string
		mode string
		want bool
	}{
		{"static-bind", string(storagev1alpha1.SnapshotModeStaticBind), true},
		{"import", string(storagev1alpha1.SnapshotModeImport), false},
		{"capture", string(storagev1alpha1.SnapshotModeCapture), false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		if got := snapshotIsStaticBind(domainStaticBindObj(tc.mode)); got != tc.want {
			t.Errorf("%s: snapshotIsStaticBind=%v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestGenericStaticBindRefMatches(t *testing.T) {
	obj := domainStaticBindObj(string(storagev1alpha1.SnapshotModeStaticBind))
	gv := "demo.state-snapshotter.deckhouse.io/v1alpha1"
	kind := "DemoVirtualDiskSnapshot"
	uid := types.UID("domain-uid-1")
	cases := []struct {
		name string
		ref  *storagev1alpha1.SnapshotSubjectRef
		want bool
	}{
		{"nil", nil, false},
		{"match-no-uid", &storagev1alpha1.SnapshotSubjectRef{APIVersion: gv, Kind: kind, Name: "disk-snap", Namespace: "project-a"}, true},
		{"match-uid", &storagev1alpha1.SnapshotSubjectRef{APIVersion: gv, Kind: kind, Name: "disk-snap", Namespace: "project-a", UID: uid}, true},
		{"uid-mismatch", &storagev1alpha1.SnapshotSubjectRef{APIVersion: gv, Kind: kind, Name: "disk-snap", Namespace: "project-a", UID: types.UID("stale")}, false},
		{"wrong-kind", &storagev1alpha1.SnapshotSubjectRef{APIVersion: gv, Kind: "Other", Name: "disk-snap", Namespace: "project-a"}, false},
		{"wrong-apiversion", &storagev1alpha1.SnapshotSubjectRef{APIVersion: "x/v1", Kind: kind, Name: "disk-snap", Namespace: "project-a"}, false},
		{"wrong-name", &storagev1alpha1.SnapshotSubjectRef{APIVersion: gv, Kind: kind, Name: "other", Namespace: "project-a"}, false},
		{"wrong-namespace", &storagev1alpha1.SnapshotSubjectRef{APIVersion: gv, Kind: kind, Name: "disk-snap", Namespace: "other"}, false},
	}
	for _, tc := range cases {
		if got := genericStaticBindRefMatches(tc.ref, obj); got != tc.want {
			t.Errorf("%s: genericStaticBindRefMatches=%v, want %v", tc.name, got, tc.want)
		}
	}
}
