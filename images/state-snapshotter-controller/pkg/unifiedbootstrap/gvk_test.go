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

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func assertEqualSliceLens(t *testing.T, snaps, contents []schema.GroupVersionKind) {
	t.Helper()
	if len(snaps) != len(contents) {
		t.Fatalf("contract: snapshot and snapshotContent slices must have equal length; got %d and %d", len(snaps), len(contents))
	}
}

func TestResolveAvailableUnifiedGVKPairs_keepsOnlyPairsWithBothMappings(t *testing.T) {
	gv := schema.GroupVersion{Group: "state-snapshotter.deckhouse.io", Version: "v1alpha1"}
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{gv})
	snap := schema.GroupVersionKind{Group: gv.Group, Version: gv.Version, Kind: "Snapshot"}
	content := schema.GroupVersionKind{Group: gv.Group, Version: gv.Version, Kind: "SnapshotContent"}
	mapper.Add(snap, meta.RESTScopeNamespace)
	mapper.Add(content, meta.RESTScopeRoot)

	pairs := []UnifiedGVKPair{
		{Snapshot: snap, SnapshotContent: content},
		{
			// Snapshot side intentionally not registered in the mapper, so this pair must be dropped.
			Snapshot:        schema.GroupVersionKind{Group: "example.deckhouse.io", Version: "v1alpha1", Kind: "ExampleDomainSnapshot"},
			SnapshotContent: content,
		},
	}

	snaps, contents := ResolveAvailableUnifiedGVKPairs(mapper, pairs, logr.Discard())
	assertEqualSliceLens(t, snaps, contents)
	if len(snaps) != 1 || len(contents) != 1 {
		t.Fatalf("expected 1 pair kept, got snapshots=%d contents=%d", len(snaps), len(contents))
	}
	if snaps[0] != snap || contents[0] != content {
		t.Fatalf("unexpected pair: snap=%v content=%v", snaps[0], contents[0])
	}
}

func TestResolveAvailableUnifiedGVKPairs_emptyWhenNothingMaps(t *testing.T) {
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{{Group: "state-snapshotter.deckhouse.io", Version: "v1alpha1"}})
	pair := UnifiedGVKPair{
		Snapshot:        schema.GroupVersionKind{Group: "state-snapshotter.deckhouse.io", Version: "v1alpha1", Kind: "Snapshot"},
		SnapshotContent: schema.GroupVersionKind{Group: "state-snapshotter.deckhouse.io", Version: "v1alpha1", Kind: "SnapshotContent"},
	}
	snaps, contents := ResolveAvailableUnifiedGVKPairs(mapper, []UnifiedGVKPair{pair}, logr.Discard())
	assertEqualSliceLens(t, snaps, contents)
	if len(snaps) != 0 || len(contents) != 0 {
		t.Fatalf("expected empty, got snapshots=%d contents=%d", len(snaps), len(contents))
	}
}

func TestResolveAvailableUnifiedGVKPairs_skipsWhenOnlySnapshotMaps(t *testing.T) {
	gv := schema.GroupVersion{Group: "state-snapshotter.deckhouse.io", Version: "v1alpha1"}
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{gv})
	snap := schema.GroupVersionKind{Group: gv.Group, Version: gv.Version, Kind: "Snapshot"}
	content := schema.GroupVersionKind{Group: gv.Group, Version: gv.Version, Kind: "SnapshotContent"}
	mapper.Add(snap, meta.RESTScopeNamespace)

	pair := UnifiedGVKPair{Snapshot: snap, SnapshotContent: content}
	snaps, contents := ResolveAvailableUnifiedGVKPairs(mapper, []UnifiedGVKPair{pair}, logr.Discard())
	assertEqualSliceLens(t, snaps, contents)
	if len(snaps) != 0 {
		t.Fatalf("expected pair skipped when SnapshotContent missing from API, got %d snapshot GVKs", len(snaps))
	}
}

func TestFilterGenericSnapshotGVKPairs_skipsDedicatedKinds(t *testing.T) {
	// Snapshot (root) and the demo kinds have dedicated reconcilers, so the
	// generic binder must not watch them. Only a non-dedicated kind survives.
	genericSnap := schema.GroupVersionKind{Group: "example.deckhouse.io", Version: "v1alpha1", Kind: "ExampleDomainSnapshot"}
	snapGVKs := []schema.GroupVersionKind{
		{Group: "demo.state-snapshotter.deckhouse.io", Version: "v1alpha1", Kind: "DemoVirtualDiskSnapshot"},
		{Group: "demo.state-snapshotter.deckhouse.io", Version: "v1alpha1", Kind: "DemoVirtualMachineSnapshot"},
		{Group: "state-snapshotter.deckhouse.io", Version: "v1alpha1", Kind: "Snapshot"},
		genericSnap,
	}
	commonContent := CommonSnapshotContentGVK()
	contentGVKs := []schema.GroupVersionKind{
		commonContent,
		commonContent,
		commonContent,
		commonContent,
	}
	sOut, cOut := FilterGenericSnapshotGVKPairs(snapGVKs, contentGVKs)
	if len(sOut) != 1 || sOut[0] != genericSnap || len(cOut) != 1 || cOut[0].Kind != "SnapshotContent" {
		t.Fatalf("got snaps=%v contents=%v", sOut, cOut)
	}
}

func TestFilterGenericSnapshotContentGVKs_skipsDedicatedSnapshotPairs(t *testing.T) {
	// Root Snapshot is dedicated (SnapshotReconciler), so its content side must be
	// dropped; only the non-dedicated kind's content survives.
	snapGVKs := []schema.GroupVersionKind{
		{Group: "state-snapshotter.deckhouse.io", Version: "v1alpha1", Kind: "Snapshot"},
		{Group: "example.deckhouse.io", Version: "v1alpha1", Kind: "ExampleDomainSnapshot"},
	}
	contentGVKs := []schema.GroupVersionKind{
		{Group: "state-snapshotter.deckhouse.io", Version: "v1alpha1", Kind: "SnapshotContent"},
		{Group: "state-snapshotter.deckhouse.io", Version: "v1alpha1", Kind: "SnapshotContent"},
	}
	out := FilterGenericSnapshotContentGVKs(snapGVKs, contentGVKs)
	if len(out) != 1 || out[0].Kind != "SnapshotContent" {
		t.Fatalf("expected single generic SnapshotContent GVK, got %v", out)
	}
}

func TestAppendGVKIfMissing_DoesNotDuplicateCommonSnapshotContent(t *testing.T) {
	common := CommonSnapshotContentGVK()
	out := AppendGVKIfMissing([]schema.GroupVersionKind{common}, common)
	if len(out) != 1 {
		t.Fatalf("expected common SnapshotContent GVK to stay unique, got %v", out)
	}
}

func TestDefaultGraphRegistryBuiltInPairs_containsSnapshotAndVolumeSnapshot(t *testing.T) {
	pairs := DefaultGraphRegistryBuiltInPairs()
	if len(pairs) != 2 {
		t.Fatalf("expected two graph built-in pairs (Snapshot + VolumeSnapshot), got %d: %v", len(pairs), pairs)
	}

	var sawSnapshot, sawVolumeSnapshot bool
	for _, p := range pairs {
		if p.SnapshotContent != CommonSnapshotContentGVK() {
			t.Fatalf("built-in pair %v must use the common SnapshotContent", p)
		}
		switch {
		case p.Snapshot.Group == "state-snapshotter.deckhouse.io" && p.Snapshot.Kind == "Snapshot":
			sawSnapshot = true
			if p.RequiresDataArtifact {
				t.Fatalf("root Snapshot must not require a data artifact: %v", p)
			}
		case p.Snapshot.Group == "snapshot.storage.k8s.io" && p.Snapshot.Kind == "VolumeSnapshot":
			sawVolumeSnapshot = true
			if !p.RequiresDataArtifact {
				t.Fatalf("built-in VolumeSnapshot must require a data artifact: %v", p)
			}
		default:
			t.Fatalf("unexpected built-in pair (only Snapshot + VolumeSnapshot are built in): %v", p)
		}
	}
	if !sawSnapshot || !sawVolumeSnapshot {
		t.Fatalf("expected both the root Snapshot and the CSI VolumeSnapshot built-in pairs, got %v", pairs)
	}
}

func TestDefaultGraphRegistryBuiltInPairs_hasNoCSDGatedDomainPairs(t *testing.T) {
	// The built-in list carries only kinds whose RBAC contract is met by the controller's static
	// rbac-for-us.yaml: the core Snapshot and the CSI VolumeSnapshot (PVC/VS/VSC/VSClass). It must NEVER
	// carry a CSD-gated domain kind (e.g. virtualization/demo), which has no static RBAC contract and would
	// widen the watch surface into forbidden list/watch loops.
	for _, p := range DefaultGraphRegistryBuiltInPairs() {
		switch p.Snapshot.Kind {
		case "DemoVirtualMachineSnapshot", "DemoVirtualDiskSnapshot":
			t.Fatalf("demo pair must be CSD-gated, found graph built-in: %v", p)
		}
		isRootSnapshot := p.Snapshot.Group == "state-snapshotter.deckhouse.io" && p.Snapshot.Kind == "Snapshot"
		isVolumeSnapshot := p.Snapshot.Group == "snapshot.storage.k8s.io" && p.Snapshot.Kind == "VolumeSnapshot"
		if !isRootSnapshot && !isVolumeSnapshot {
			t.Fatalf("built-in pairs must contain only the core Snapshot + CSI VolumeSnapshot, found: %v", p)
		}
	}
}

func TestResolveAvailableUnifiedGVKPairs_skipsWhenOnlySnapshotContentMaps(t *testing.T) {
	gv := schema.GroupVersion{Group: "state-snapshotter.deckhouse.io", Version: "v1alpha1"}
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{gv})
	snap := schema.GroupVersionKind{Group: gv.Group, Version: gv.Version, Kind: "Snapshot"}
	content := schema.GroupVersionKind{Group: gv.Group, Version: gv.Version, Kind: "SnapshotContent"}
	mapper.Add(content, meta.RESTScopeRoot)

	pair := UnifiedGVKPair{Snapshot: snap, SnapshotContent: content}
	snaps, contents := ResolveAvailableUnifiedGVKPairs(mapper, []UnifiedGVKPair{pair}, logr.Discard())
	assertEqualSliceLens(t, snaps, contents)
	if len(snaps) != 0 {
		t.Fatalf("expected pair skipped when Snapshot missing from API, got %d snapshot GVKs", len(snaps))
	}
}
