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
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk/internal/storagefoundation"
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

func TestTargetsManifestOnly(t *testing.T) {
	base := []ssv1alpha1.ManifestTarget{{APIVersion: "demo/v1", Kind: "Disk", Name: "d"}}
	got := Targets(base, nil, "ns")
	if len(got) != 1 || got[0] != base[0] {
		t.Fatalf("manifest-only must return base unchanged, got %#v", got)
	}
}

func TestTargetsMergesOwnedPVCAndSorts(t *testing.T) {
	base := []ssv1alpha1.ManifestTarget{{APIVersion: "demo/v1", Kind: "Disk", Name: "d"}}
	owned := &storagefoundation.Target{APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc-a"}
	got := Targets(base, owned, "ns")
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

func TestTargetsSkipsNonPVCOwnedTarget(t *testing.T) {
	base := []ssv1alpha1.ManifestTarget{{APIVersion: "demo/v1", Kind: "Disk", Name: "d"}}
	owned := &storagefoundation.Target{APIVersion: "v1", Kind: "Secret", Name: "ignored"}
	got := Targets(base, owned, "ns")
	if len(got) != 1 || got[0] != base[0] {
		t.Fatalf("non-PVC owned target must be skipped, got %#v", got)
	}
}

func TestTargetsDedupesOverlapWithBase(t *testing.T) {
	base := []ssv1alpha1.ManifestTarget{{APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc-a"}}
	owned := &storagefoundation.Target{APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "pvc-a"}
	got := Targets(base, owned, "ns")
	if len(got) != 1 {
		t.Fatalf("expected dedup to a single target, got %#v", got)
	}
}
