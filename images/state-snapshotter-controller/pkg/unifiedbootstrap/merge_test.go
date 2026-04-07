package unifiedbootstrap

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestMergeBootstrapAndDSCPairs_dscOverrides(t *testing.T) {
	bootstrap := []UnifiedGVKPair{
		{
			Snapshot:        schema.GroupVersionKind{Group: "g", Version: "v1", Kind: "Snap"},
			SnapshotContent: schema.GroupVersionKind{Group: "g", Version: "v1", Kind: "SnapContentOld"},
		},
	}
	fromDSC := []UnifiedGVKPair{
		{
			Snapshot:        schema.GroupVersionKind{Group: "g", Version: "v1", Kind: "Snap"},
			SnapshotContent: schema.GroupVersionKind{Group: "g", Version: "v1", Kind: "SnapContentNew"},
		},
	}
	got := MergeBootstrapAndDSCPairs(bootstrap, fromDSC)
	if len(got) != 1 {
		t.Fatalf("len %d", len(got))
	}
	if got[0].SnapshotContent.Kind != "SnapContentNew" {
		t.Fatalf("DSC pair should override bootstrap: %+v", got[0])
	}
}

func TestMergeBootstrapAndDSCPairs_union(t *testing.T) {
	bootstrap := []UnifiedGVKPair{
		{Snapshot: schema.GroupVersionKind{Group: "a", Version: "v1", Kind: "S1"}, SnapshotContent: schema.GroupVersionKind{Group: "a", Version: "v1", Kind: "S1C"}},
	}
	fromDSC := []UnifiedGVKPair{
		{Snapshot: schema.GroupVersionKind{Group: "b", Version: "v1", Kind: "S2"}, SnapshotContent: schema.GroupVersionKind{Group: "b", Version: "v1", Kind: "S2C"}},
	}
	got := MergeBootstrapAndDSCPairs(bootstrap, fromDSC)
	if len(got) != 2 {
		t.Fatalf("len %d", len(got))
	}
	// sorted by snapshot string: a/v1,S1 before b/v1,S2
	if got[0].Snapshot.Group != "a" {
		t.Fatalf("order: %+v", got)
	}
}
