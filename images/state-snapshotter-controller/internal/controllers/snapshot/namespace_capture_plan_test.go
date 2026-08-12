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

package snapshot

import (
	"testing"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk"
)

func TestSubtractManifestTargetsDropsExcludedIdentities(t *testing.T) {
	base := []namespacemanifest.ManifestTarget{
		{APIVersion: "v1", Kind: "ConfigMap", Name: "keep"},
		{APIVersion: "v1", Kind: "Secret", Name: "child-owned"},
		{APIVersion: "apps/v1", Kind: "Deployment", Name: "keep-deploy"},
	}
	exclude := []snapshotsdk.SubtreeManifestIdentity{
		// Namespace on the exclude side must NOT affect matching (implied equal).
		{APIVersion: "v1", Kind: "Secret", Namespace: "ns", Name: "child-owned", UID: "u1"},
		// An identity not present in base is a harmless no-op.
		{APIVersion: "v1", Kind: "ConfigMap", Namespace: "ns", Name: "not-in-base"},
	}

	got := subtractManifestTargets(base, exclude)

	if len(got) != 2 {
		t.Fatalf("expected 2 targets after exclude, got %d: %#v", len(got), got)
	}
	for _, tgt := range got {
		if tgt.Kind == "Secret" && tgt.Name == "child-owned" {
			t.Fatalf("excluded target still present: %#v", got)
		}
	}
}

func TestSubtractManifestTargetsEmptyExcludeReturnsBase(t *testing.T) {
	base := []namespacemanifest.ManifestTarget{{APIVersion: "v1", Kind: "ConfigMap", Name: "a"}}
	got := subtractManifestTargets(base, nil)
	if len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("empty exclude must return base unchanged, got %#v", got)
	}
}

func TestAllDirectDomainChildrenAtLeastPlanned(t *testing.T) {
	domainRef := func(name string) storagev1alpha1.SnapshotChildRef {
		return storagev1alpha1.SnapshotChildRef{APIVersion: "storage.deckhouse.io/v1alpha1", Kind: "Snapshot", Name: name}
	}
	// An orphan CSI VolumeSnapshot: under the content-single-writer model it is an ordinary domain child
	// (no longer a skipped "visibility leaf"), so it participates in the at-least-Planned gate like any
	// other child — it must reach Planned/Finished to satisfy the gate.
	orphanLeaf := storagev1alpha1.SnapshotChildRef{
		APIVersion: snapshotpkg.CSISnapshotAPIVersion,
		Kind:       snapshotpkg.KindVolumeSnapshot,
		Name:       "nss-vs-orphan",
	}

	tests := []struct {
		name    string
		refs    []storagev1alpha1.SnapshotChildRef
		phases  map[string]storagev1alpha1.SnapshotCapturePhase
		wantAll bool
	}{
		{
			name:    "no children passes vacuously",
			refs:    nil,
			phases:  nil,
			wantAll: true,
		},
		{
			name: "all domain children planned/finished",
			refs: []storagev1alpha1.SnapshotChildRef{domainRef("a"), domainRef("b")},
			phases: map[string]storagev1alpha1.SnapshotCapturePhase{
				"a": storagev1alpha1.SnapshotCapturePhasePlanned,
				"b": storagev1alpha1.SnapshotCapturePhaseFinished,
			},
			wantAll: true,
		},
		{
			name: "one domain child still planning blocks the gate",
			refs: []storagev1alpha1.SnapshotChildRef{domainRef("a"), domainRef("b")},
			phases: map[string]storagev1alpha1.SnapshotCapturePhase{
				"a": storagev1alpha1.SnapshotCapturePhasePlanned,
				"b": storagev1alpha1.SnapshotCapturePhasePlanning,
			},
			wantAll: false,
		},
		{
			name:    "orphan VS child with no phase blocks the gate",
			refs:    []storagev1alpha1.SnapshotChildRef{domainRef("a"), orphanLeaf},
			phases:  map[string]storagev1alpha1.SnapshotCapturePhase{"a": storagev1alpha1.SnapshotCapturePhasePlanned},
			wantAll: false,
		},
		{
			name: "orphan VS child at least Planned satisfies the gate",
			refs: []storagev1alpha1.SnapshotChildRef{domainRef("a"), orphanLeaf},
			phases: map[string]storagev1alpha1.SnapshotCapturePhase{
				"a":             storagev1alpha1.SnapshotCapturePhasePlanned,
				orphanLeaf.Name: storagev1alpha1.SnapshotCapturePhaseFinished,
			},
			wantAll: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := allDirectDomainChildrenAtLeastPlanned(tc.refs, tc.phases); got != tc.wantAll {
				t.Fatalf("allDirectDomainChildrenAtLeastPlanned = %v, want %v", got, tc.wantAll)
			}
		})
	}
}
