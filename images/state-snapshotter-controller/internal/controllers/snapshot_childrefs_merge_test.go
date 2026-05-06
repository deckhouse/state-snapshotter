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

package controllers

import (
	"testing"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

func childRef(name string) storagev1alpha1.SnapshotChildRef {
	return storagev1alpha1.SnapshotChildRef{
		APIVersion: "demo.test/v1",
		Kind:       "DemoSnapshot",
		Name:       name,
	}
}

func TestMergeSnapshotChildRefs(t *testing.T) {
	keep := childRef("other")
	child := childRef("child")
	got := mergeSnapshotChildRefs([]storagev1alpha1.SnapshotChildRef{keep}, []storagev1alpha1.SnapshotChildRef{child})
	if len(got) != 2 {
		t.Fatalf("want 2 refs got %d %+v", len(got), got)
	}
	if got[0].Name != "child" || got[1].Name != "other" {
		t.Fatalf("want stable sort by name got %+v", got)
	}
	if !snapshotChildRefsEqualIgnoreOrder(got, []storagev1alpha1.SnapshotChildRef{keep, child}) {
		t.Fatalf("multiset mismatch %+v", got)
	}

	overwrite := mergeSnapshotChildRefs(
		[]storagev1alpha1.SnapshotChildRef{childRef("x")},
		[]storagev1alpha1.SnapshotChildRef{childRef("x")},
	)
	if len(overwrite) != 1 || overwrite[0].Name != "x" {
		t.Fatalf("same-key merge: %+v", overwrite)
	}
}

func TestRemoveSnapshotChildRefsByKeys(t *testing.T) {
	existing := []storagev1alpha1.SnapshotChildRef{
		childRef("keep"),
		childRef("drop"),
	}
	remove := []storagev1alpha1.SnapshotChildRef{childRef("drop")}
	got := removeSnapshotChildRefsByKeys(existing, remove)
	if len(got) != 1 || got[0].Name != "keep" {
		t.Fatalf("got %+v", got)
	}
}

func TestRemoveSnapshotContentChildRefsByKeys(t *testing.T) {
	existing := []storagev1alpha1.SnapshotContentChildRef{{Name: "a"}, {Name: "b"}}
	got := removeSnapshotContentChildRefsByKeys(existing, []storagev1alpha1.SnapshotContentChildRef{{Name: "a"}})
	if len(got) != 1 || got[0].Name != "b" {
		t.Fatalf("got %+v", got)
	}
}

func TestMergeSnapshotContentChildRefs(t *testing.T) {
	existing := []storagev1alpha1.SnapshotContentChildRef{{Name: "x"}}
	upsert := []storagev1alpha1.SnapshotContentChildRef{{Name: "y"}}
	got := mergeSnapshotContentChildRefs(existing, upsert)
	if len(got) != 2 {
		t.Fatalf("want len 2 got %d", len(got))
	}
	got2 := mergeSnapshotContentChildRefs(got, []storagev1alpha1.SnapshotContentChildRef{{Name: "x"}})
	if len(got2) != 2 {
		t.Fatalf("re-upsert same name: want len 2 got %d", len(got2))
	}
}
