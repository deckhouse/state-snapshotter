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

package manifest

import (
	"testing"

	"k8s.io/apimachinery/pkg/types"

	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
)

func TestRequestNameDeterministic(t *testing.T) {
	a := RequestName(types.UID("uid-1"))
	b := RequestName(types.UID("uid-1"))
	if a != b {
		t.Fatalf("RequestName not deterministic: %q != %q", a, b)
	}
	if a == RequestName(types.UID("uid-2")) {
		t.Fatal("RequestName collides for different UIDs")
	}
}

func TestTargetsSingle(t *testing.T) {
	declared := []ssv1alpha1.ManifestTarget{{APIVersion: "demo/v1", Kind: "Disk", Name: "d"}}
	got := Targets(declared)
	if len(got) != 1 || got[0] != declared[0] {
		t.Fatalf("single declared target must pass through unchanged, got %#v", got)
	}
}

// The domain declares BOTH the object and its PVC explicitly (the manifest leg does not derive the PVC
// from the data leg); Targets returns exactly the declared set, deduplicated and sorted deterministically.
func TestTargetsDeclaredSetSorted(t *testing.T) {
	declared := []ssv1alpha1.ManifestTarget{
		{APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc-a"},
		{APIVersion: "demo/v1", Kind: "Disk", Name: "d"},
	}
	got := Targets(declared)
	want := []ssv1alpha1.ManifestTarget{
		{APIVersion: "demo/v1", Kind: "Disk", Name: "d"},
		{APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc-a"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("at %d got %#v want %#v (full %#v)", i, got[i], want[i], got)
		}
	}
}

func TestTargetsDedupesDeclaredDuplicates(t *testing.T) {
	declared := []ssv1alpha1.ManifestTarget{
		{APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc-a"},
		{APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc-a"},
	}
	got := Targets(declared)
	if len(got) != 1 {
		t.Fatalf("expected dedup to a single target, got %#v", got)
	}
}

func TestTargetsEmpty(t *testing.T) {
	if got := Targets(nil); len(got) != 0 {
		t.Fatalf("nil declared set must yield empty, got %#v", got)
	}
}
