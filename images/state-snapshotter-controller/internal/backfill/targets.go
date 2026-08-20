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

package backfill

import (
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	apinames "github.com/deckhouse/state-snapshotter/api/names"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

const (
	// groupStorage is the state-snapshotter API group (Snapshot).
	groupStorage = storagev1alpha1.APIGroup
	// groupCSI is the external-snapshotter API group.
	groupCSI = "snapshot.storage.k8s.io"
)

// DefaultTargets is the canonical set of protected kinds and their migration classifiers. Kinds whose CRD
// is not installed are tolerated (skipped) at list time. Demo/PoC domain snapshot kinds are intentionally
// omitted: they live outside the core module and their legacy children already follow the nss-snap- name
// scheme, so a cluster that runs them can extend this set.
//
// Only namespaced snapshot nodes are listed. The cluster-scoped part of a tree — SnapshotContent,
// ManifestCheckpoint and its chunks, ObjectKeeper, the CSI VolumeSnapshotContent — carries no marker at all
// now, so there is nothing there to backfill.
func DefaultTargets() []Target {
	return []Target{
		// Root Snapshot is NOT protected (user-deletable). A CHILD Snapshot carries the deterministic
		// nss-snap- name; that prefix cleanly separates children (ours) from the user-named root.
		{GVK: gvk(groupStorage, "Snapshot"), IsOurs: hasNamePrefix(apinames.PrefixChildSnapshot)},

		// CSI VolumeSnapshot is SHARED, and being managed says nothing about which side of the share an
		// object is on: a snapshot a user took by hand is captured too, and carries managed=true like any
		// other. What separates them is origin — the provenance its creator stamped — plus the deterministic
		// orphan-VS name, which is the same fact for a node made before that label existed.
		{GVK: gvkV(groupCSI, "v1", "VolumeSnapshot"), IsOurs: anyOf(hasProvenance, hasNamePrefix(apinames.PrefixOrphanVS))},
	}
}

func gvk(group, kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: group, Version: "v1alpha1", Kind: kind}
}

func gvkV(group, version, kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: group, Version: version, Kind: kind}
}

// hasNamePrefix matches any of the given deterministic name prefixes.
func hasNamePrefix(prefixes ...string) Classifier {
	return func(obj *unstructured.Unstructured) bool {
		name := obj.GetName()
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
}

// anyOf ORs several classifiers (an object is ours if any signal matches).
func anyOf(classifiers ...Classifier) Classifier {
	return func(obj *unstructured.Unstructured) bool {
		for _, c := range classifiers {
			if c(obj) {
				return true
			}
		}
		return false
	}
}

// hasProvenance matches an object its creator stamped as made by this module.
func hasProvenance(obj *unstructured.Unstructured) bool {
	return storagev1alpha1.HasProvenance(obj)
}
