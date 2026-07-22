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
	// groupStorage is the state-snapshotter API group (Snapshot / SnapshotContent).
	groupStorage = storagev1alpha1.APIGroup
	// groupManifest is the manifest-checkpoint API group (same group string, different Go package).
	groupManifest = storagev1alpha1.APIGroup
	// groupDeckhouse hosts ObjectKeeper.
	groupDeckhouse = "deckhouse.io"
	// groupCSI is the external-snapshotter API group.
	groupCSI = "snapshot.storage.k8s.io"

	// labelManaged latches the adoption veto outcome on a CSI VolumeSnapshot ("true" => domain-captured).
	// It is set by storage-foundation; the backfill reads it ONLY as a migration provenance signal.
	labelManaged = storagev1alpha1.APIGroup + "/managed"

	// kindObjectKeeper is the ownerRef kind that marks a CSI VolumeSnapshotContent as one of ours.
	kindObjectKeeper = "ObjectKeeper"
)

// DefaultTargets is the canonical set of protected kinds and their migration classifiers. Kinds whose CRD
// is not installed are tolerated (skipped) at list time. Demo/PoC domain snapshot kinds are intentionally
// omitted: they live outside the core module and their legacy children already follow the nss-snap- name
// scheme, so a cluster that runs them can extend this set.
func DefaultTargets() []Target {
	return []Target{
		// Root Snapshot is NOT protected (user-deletable). A CHILD Snapshot carries the deterministic
		// nss-snap- name; that prefix cleanly separates children (ours) from the user-named root.
		{GVK: gvk(groupStorage, "Snapshot"), IsOurs: hasNamePrefix(apinames.PrefixChildSnapshot)},

		// SnapshotContent / ManifestCheckpoint / ManifestCheckpointContentChunk are our EXCLUSIVE CRDs:
		// nothing else creates them, so the whole kind is ours.
		{GVK: gvk(groupStorage, "SnapshotContent"), IsOurs: always},
		{GVK: gvk(groupManifest, "ManifestCheckpoint"), IsOurs: always},
		{GVK: gvk(groupManifest, "ManifestCheckpointContentChunk"), IsOurs: always},

		// ObjectKeeper is a SHARED deckhouse.io CRD: only OUR keepers (deterministic nss-ok- / nss-import-ok-
		// names) are protected. Fail closed on anything else.
		{GVK: gvk(groupDeckhouse, kindObjectKeeper), IsOurs: hasNamePrefix(apinames.PrefixObjectKeeper, apinames.PrefixImportKeeper)},

		// CSI VolumeSnapshot is SHARED: ours iff domain-managed (managed=true) or carrying our orphan-VS
		// name. A vetoed (managed=false) or foreign VolumeSnapshot is left untouched.
		{GVK: gvkV(groupCSI, "v1", "VolumeSnapshot"), IsOurs: anyOf(managedIsTrue, hasNamePrefix(apinames.PrefixOrphanVS))},

		// CSI VolumeSnapshotContent is SHARED: ours iff owned by one of our ObjectKeepers (the durable
		// handoff owner storage-foundation stamps). No ownerRef to our keeper => not ours.
		{GVK: gvkV(groupCSI, "v1", "VolumeSnapshotContent"), IsOurs: ownedByKind(kindObjectKeeper)},
	}
}

func gvk(group, kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: group, Version: "v1alpha1", Kind: kind}
}

func gvkV(group, version, kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: group, Version: version, Kind: kind}
}

// always classifies every object of the kind as ours (used for our exclusive CRDs).
func always(*unstructured.Unstructured) bool { return true }

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

// managedIsTrue matches a CSI VolumeSnapshot latched as domain-managed.
func managedIsTrue(obj *unstructured.Unstructured) bool {
	return obj.GetLabels()[labelManaged] == "true"
}

// ownedByKind matches an object carrying an ownerReference to the given kind (used to recognize our CSI
// VolumeSnapshotContent by its ObjectKeeper owner).
func ownedByKind(kind string) Classifier {
	return func(obj *unstructured.Unstructured) bool {
		for _, ref := range obj.GetOwnerReferences() {
			if ref.Kind == kind {
				return true
			}
		}
		return false
	}
}
