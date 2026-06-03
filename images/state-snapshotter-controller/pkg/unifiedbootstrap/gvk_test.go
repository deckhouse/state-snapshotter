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
	gv := schema.GroupVersion{Group: "storage.deckhouse.io", Version: "v1alpha1"}
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{gv})
	snap := schema.GroupVersionKind{Group: gv.Group, Version: gv.Version, Kind: "Snapshot"}
	content := schema.GroupVersionKind{Group: gv.Group, Version: gv.Version, Kind: "SnapshotContent"}
	mapper.Add(snap, meta.RESTScopeNamespace)
	mapper.Add(content, meta.RESTScopeRoot)

	pairs := []UnifiedGVKPair{
		{Snapshot: snap, SnapshotContent: content},
		{
			// Snapshot side intentionally not registered in the mapper, so this pair must be dropped.
			Snapshot:        schema.GroupVersionKind{Group: "snapshot.internal.virtualization.deckhouse.io", Version: "v1alpha1", Kind: "InternalVirtualizationVirtualMachineSnapshot"},
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
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{{Group: "storage.deckhouse.io", Version: "v1alpha1"}})
	pair := UnifiedGVKPair{
		Snapshot:        schema.GroupVersionKind{Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "Snapshot"},
		SnapshotContent: schema.GroupVersionKind{Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "SnapshotContent"},
	}
	snaps, contents := ResolveAvailableUnifiedGVKPairs(mapper, []UnifiedGVKPair{pair}, logr.Discard())
	assertEqualSliceLens(t, snaps, contents)
	if len(snaps) != 0 || len(contents) != 0 {
		t.Fatalf("expected empty, got snapshots=%d contents=%d", len(snaps), len(contents))
	}
}

func TestResolveAvailableUnifiedGVKPairs_skipsWhenOnlySnapshotMaps(t *testing.T) {
	gv := schema.GroupVersion{Group: "storage.deckhouse.io", Version: "v1alpha1"}
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
	genericSnap := schema.GroupVersionKind{Group: "snapshot.internal.virtualization.deckhouse.io", Version: "v1alpha1", Kind: "InternalVirtualizationVirtualMachineSnapshot"}
	snapGVKs := []schema.GroupVersionKind{
		{Group: "demo.state-snapshotter.deckhouse.io", Version: "v1alpha1", Kind: "DemoVirtualDiskSnapshot"},
		{Group: "demo.state-snapshotter.deckhouse.io", Version: "v1alpha1", Kind: "DemoVirtualMachineSnapshot"},
		{Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "Snapshot"},
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
		{Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "Snapshot"},
		{Group: "snapshot.internal.virtualization.deckhouse.io", Version: "v1alpha1", Kind: "InternalVirtualizationVirtualMachineSnapshot"},
	}
	contentGVKs := []schema.GroupVersionKind{
		{Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "SnapshotContent"},
		{Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "SnapshotContent"},
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

func TestDefaultGraphRegistryBuiltInPairs_containsOnlySnapshot(t *testing.T) {
	pairs := DefaultGraphRegistryBuiltInPairs()
	if len(pairs) != 1 {
		t.Fatalf("expected one graph built-in pair, got %d: %v", len(pairs), pairs)
	}
	if pairs[0].Snapshot.Kind != "Snapshot" || pairs[0].SnapshotContent.Kind != "SnapshotContent" {
		t.Fatalf("expected Snapshot graph built-in, got %v", pairs[0])
	}
	for _, p := range pairs {
		switch p.Snapshot.Kind {
		case "DemoVirtualMachineSnapshot", "DemoVirtualDiskSnapshot":
			t.Fatalf("demo pair must be CSD-gated, found graph built-in: %v", p)
		}
	}
}

func TestDefaultUnifiedRuntimeBootstrapPairs_remainsSeparateFromGraphBuiltIns(t *testing.T) {
	runtimePairs := DefaultUnifiedRuntimeBootstrapPairs()
	if len(runtimePairs) <= len(DefaultGraphRegistryBuiltInPairs()) {
		t.Fatalf("expected unified runtime bootstrap to keep its broader static list, got %v", runtimePairs)
	}
	var hasSnapshot bool
	for _, p := range runtimePairs {
		if p.Snapshot.Kind == "Snapshot" && p.SnapshotContent.Kind == "SnapshotContent" {
			hasSnapshot = true
		}
	}
	if !hasSnapshot {
		t.Fatalf("expected generic Snapshot/SnapshotContent in unified runtime bootstrap, got %v", runtimePairs)
	}
}

func TestResolveAvailableUnifiedGVKPairs_skipsWhenOnlySnapshotContentMaps(t *testing.T) {
	gv := schema.GroupVersion{Group: "storage.deckhouse.io", Version: "v1alpha1"}
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
