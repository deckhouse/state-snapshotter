/*
Copyright 2025 Flant JSC

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

func childRef(ns, name string) storagev1alpha1.NamespaceSnapshotChildRef {
	return storagev1alpha1.NamespaceSnapshotChildRef{
		APIVersion: "demo.test/v1",
		Kind:       "DemoSnapshot",
		Namespace:  ns,
		Name:       name,
	}
}

func TestMergeNamespaceSnapshotChildRefs(t *testing.T) {
	keep := childRef("ns1", "other")
	child := childRef("ns1", "child")
	got := mergeNamespaceSnapshotChildRefs([]storagev1alpha1.NamespaceSnapshotChildRef{keep}, []storagev1alpha1.NamespaceSnapshotChildRef{child})
	if len(got) != 2 {
		t.Fatalf("want 2 refs got %d %+v", len(got), got)
	}
	if got[0].Name != "child" || got[1].Name != "other" {
		t.Fatalf("want stable sort by name got %+v", got)
	}
	if !namespaceSnapshotChildRefsEqualIgnoreOrder(got, []storagev1alpha1.NamespaceSnapshotChildRef{keep, child}) {
		t.Fatalf("multiset mismatch %+v", got)
	}

	overwrite := mergeNamespaceSnapshotChildRefs(
		[]storagev1alpha1.NamespaceSnapshotChildRef{childRef("ns1", "x")},
		[]storagev1alpha1.NamespaceSnapshotChildRef{childRef("ns1", "x")},
	)
	if len(overwrite) != 1 || overwrite[0].Name != "x" {
		t.Fatalf("same-key merge: %+v", overwrite)
	}
}

func TestRemoveNamespaceSnapshotChildRefsByKeys(t *testing.T) {
	existing := []storagev1alpha1.NamespaceSnapshotChildRef{
		childRef("ns", "keep"),
		childRef("ns", "drop"),
	}
	remove := []storagev1alpha1.NamespaceSnapshotChildRef{childRef("ns", "drop")}
	got := removeNamespaceSnapshotChildRefsByKeys(existing, remove)
	if len(got) != 1 || got[0].Name != "keep" {
		t.Fatalf("got %+v", got)
	}
}

func TestRemoveNamespaceSnapshotContentChildRefsByKeys(t *testing.T) {
	existing := []storagev1alpha1.NamespaceSnapshotContentChildRef{{Name: "a"}, {Name: "b"}}
	got := removeNamespaceSnapshotContentChildRefsByKeys(existing, []storagev1alpha1.NamespaceSnapshotContentChildRef{{Name: "a"}})
	if len(got) != 1 || got[0].Name != "b" {
		t.Fatalf("got %+v", got)
	}
}

func TestMergeNamespaceSnapshotContentChildRefs(t *testing.T) {
	existing := []storagev1alpha1.NamespaceSnapshotContentChildRef{{Name: "x"}}
	upsert := []storagev1alpha1.NamespaceSnapshotContentChildRef{{Name: "y"}}
	got := mergeNamespaceSnapshotContentChildRefs(existing, upsert)
	if len(got) != 2 {
		t.Fatalf("want len 2 got %d", len(got))
	}
	got2 := mergeNamespaceSnapshotContentChildRefs(got, []storagev1alpha1.NamespaceSnapshotContentChildRef{{Name: "x"}})
	if len(got2) != 2 {
		t.Fatalf("re-upsert same name: want len 2 got %d", len(got2))
	}
}
