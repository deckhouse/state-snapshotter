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

package snaphelpers

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
)

func TestWatchObjectForGVK(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := ssv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	typed := []struct {
		gvk  schema.GroupVersionKind
		want any
	}{
		{storagev1alpha1.SchemeGroupVersion.WithKind("Snapshot"), &storagev1alpha1.Snapshot{}},
		{storagev1alpha1.SchemeGroupVersion.WithKind("SnapshotContent"), &storagev1alpha1.SnapshotContent{}},
		{ssv1alpha1.SchemeGroupVersion.WithKind("ManifestCheckpoint"), &ssv1alpha1.ManifestCheckpoint{}},
	}
	for _, tc := range typed {
		if got := WatchObjectForGVK(scheme, tc.gvk); reflect.TypeOf(got) != reflect.TypeOf(tc.want) {
			t.Errorf("%s: got %T, want %T", tc.gvk, got, tc.want)
		}
	}

	for _, gvk := range []schema.GroupVersionKind{
		{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshot"},
		{Group: "demo.deckhouse.io", Version: "v1alpha1", Kind: "DemoSnapshot"},
	} {
		for _, s := range []*runtime.Scheme{scheme, nil} {
			got := WatchObjectForGVK(s, gvk)
			u, ok := got.(*unstructured.Unstructured)
			if !ok {
				t.Fatalf("%s: got %T, want *unstructured.Unstructured", gvk, got)
			}
			if u.GroupVersionKind() != gvk {
				t.Errorf("%s: unstructured GVK = %s", gvk, u.GroupVersionKind())
			}
		}
	}
}
