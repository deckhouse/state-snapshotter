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

package snapshotsdk

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestIsExcluded(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{"nil labels", nil, false},
		{"no veto", map[string]string{"app": "x"}, false},
		{"veto empty value", map[string]string{ExcludeLabelKey: ""}, true},
		{"veto true value", map[string]string{ExcludeLabelKey: "true"}, true},
		{"veto arbitrary value", map[string]string{ExcludeLabelKey: "whatever"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsExcluded(tc.labels); got != tc.want {
				t.Fatalf("IsExcluded(%v) = %v, want %v", tc.labels, got, tc.want)
			}
		})
	}
}

func mkObj(name string, labels map[string]string) client.Object {
	o := &unstructured.Unstructured{}
	o.SetName(name)
	if labels != nil {
		o.SetLabels(labels)
	}
	return o
}

func TestPartitionExcluded(t *testing.T) {
	veto := map[string]string{ExcludeLabelKey: ""}
	a := mkObj("a", nil)
	b := mkObj("b", veto)
	c := mkObj("c", map[string]string{"x": "y"})
	d := mkObj("d", veto)

	kept, excluded := PartitionExcluded([]client.Object{a, b, nil, c, d})

	if len(kept) != 2 || kept[0].GetName() != "a" || kept[1].GetName() != "c" {
		t.Fatalf("kept = %v, want [a c] in order", names(kept))
	}
	if len(excluded) != 2 || excluded[0].GetName() != "b" || excluded[1].GetName() != "d" {
		t.Fatalf("excluded = %v, want [b d] in order", names(excluded))
	}
}

func names(objs []client.Object) []string {
	out := make([]string, 0, len(objs))
	for _, o := range objs {
		out = append(out, o.GetName())
	}
	return out
}

func TestNormalizeExcludedRefs_SortsDedupsNonNil(t *testing.T) {
	got := normalizeExcludedRefs([]ExcludedObjectRef{
		{APIVersion: "demo/v1", Kind: "Disk", Name: "b"},
		{APIVersion: "demo/v1", Kind: "Disk", Name: "a"},
		{APIVersion: "demo/v1", Kind: "Disk", Name: "b"},
	})
	if got == nil {
		t.Fatalf("normalizeExcludedRefs returned nil; must be non-nil empty-safe slice")
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (deduped); got %+v", len(got), got)
	}
	if got[0].Name != "a" || got[1].Name != "b" {
		t.Fatalf("order = %+v, want [a b] sorted", got)
	}

	empty := normalizeExcludedRefs(nil)
	if empty == nil {
		t.Fatalf("normalizeExcludedRefs(nil) must be a non-nil empty slice, got nil")
	}
	if len(empty) != 0 {
		t.Fatalf("normalizeExcludedRefs(nil) len = %d, want 0", len(empty))
	}
}

func TestExcludedRefsEqualIgnoreOrder(t *testing.T) {
	x := []ExcludedObjectRef{
		{APIVersion: "demo/v1", Kind: "Disk", Name: "a"},
		{APIVersion: "demo/v1", Kind: "Disk", Name: "b"},
	}
	y := []ExcludedObjectRef{
		{APIVersion: "demo/v1", Kind: "Disk", Name: "b"},
		{APIVersion: "demo/v1", Kind: "Disk", Name: "a"},
	}
	if !excludedRefsEqualIgnoreOrder(x, y) {
		t.Fatalf("expected order-insensitive equality")
	}
	z := []ExcludedObjectRef{{APIVersion: "demo/v1", Kind: "Disk", Name: "a"}}
	if excludedRefsEqualIgnoreOrder(x, z) {
		t.Fatalf("expected inequality for different sizes")
	}
}
