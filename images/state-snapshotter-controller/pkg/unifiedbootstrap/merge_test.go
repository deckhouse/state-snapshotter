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

package unifiedbootstrap

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestMergeBootstrapAndCSDPairs_csdOverrides(t *testing.T) {
	bootstrap := []UnifiedGVKPair{
		{
			Snapshot:        schema.GroupVersionKind{Group: "g", Version: "v1", Kind: "Snap"},
			SnapshotContent: schema.GroupVersionKind{Group: "g", Version: "v1", Kind: "SnapContentOld"},
		},
	}
	fromCSD := []UnifiedGVKPair{
		{
			Snapshot:        schema.GroupVersionKind{Group: "g", Version: "v1", Kind: "Snap"},
			SnapshotContent: schema.GroupVersionKind{Group: "g", Version: "v1", Kind: "SnapContentNew"},
		},
	}
	got := MergeBootstrapAndCSDPairs(bootstrap, fromCSD)
	if len(got) != 1 {
		t.Fatalf("len %d", len(got))
	}
	if got[0].SnapshotContent.Kind != "SnapContentNew" {
		t.Fatalf("CSD pair should override bootstrap: %+v", got[0])
	}
}

func TestMergeBootstrapAndCSDPairs_union(t *testing.T) {
	bootstrap := []UnifiedGVKPair{
		{Snapshot: schema.GroupVersionKind{Group: "a", Version: "v1", Kind: "S1"}, SnapshotContent: schema.GroupVersionKind{Group: "a", Version: "v1", Kind: "S1C"}},
	}
	fromCSD := []UnifiedGVKPair{
		{Snapshot: schema.GroupVersionKind{Group: "b", Version: "v1", Kind: "S2"}, SnapshotContent: schema.GroupVersionKind{Group: "b", Version: "v1", Kind: "S2C"}},
	}
	got := MergeBootstrapAndCSDPairs(bootstrap, fromCSD)
	if len(got) != 2 {
		t.Fatalf("len %d", len(got))
	}
	// sorted by snapshot string: a/v1,S1 before b/v1,S2
	if got[0].Snapshot.Group != "a" {
		t.Fatalf("order: %+v", got)
	}
}
