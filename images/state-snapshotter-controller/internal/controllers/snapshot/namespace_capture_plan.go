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
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk"
)

// subtractManifestTargets returns base minus every target whose identity is in exclude — the namespace
// manifest MCR target set for the root aggregator (wave5 design §6.3: base = namespace allowlist,
// exclude = union of object identities captured across descendant snapshots' subtrees).
//
// Matching key is apiVersion|kind|name. Namespace is implied equal: the root manifest leg targets only
// objects in the root's own namespace, and namespacemanifest.ManifestTarget carries no namespace field
// (it is the MCR's namespace). The exclude identities may carry a namespace (they come from the recursive
// subresource); it is intentionally NOT part of the key, since both sides are the same namespace by
// construction. UID is likewise omitted for now (a recreated object under the same apiVersion|kind|name
// is treated as the same manifest slot — matching the pre-wave5 in-reconciler exclude behavior).
//
// Pure function (no client) so the base-minus-exclude difference is unit-tested directly (design §9).
func subtractManifestTargets(base []namespacemanifest.ManifestTarget, exclude []snapshotsdk.SubtreeManifestIdentity) []namespacemanifest.ManifestTarget {
	if len(exclude) == 0 {
		return base
	}
	excludeKeys := make(map[string]struct{}, len(exclude))
	for _, id := range exclude {
		excludeKeys[manifestIdentityKey(id.APIVersion, id.Kind, id.Name)] = struct{}{}
	}
	out := make([]namespacemanifest.ManifestTarget, 0, len(base))
	for _, t := range base {
		if _, dropped := excludeKeys[manifestIdentityKey(t.APIVersion, t.Kind, t.Name)]; dropped {
			continue
		}
		out = append(out, t)
	}
	return out
}

// manifestIdentityKey is the apiVersion|kind|name matching key shared by base targets and exclude
// identities (see subtractManifestTargets).
func manifestIdentityKey(apiVersion, kind, name string) string {
	return apiVersion + "|" + kind + "|" + name
}

// allDirectDomainChildrenAtLeastPlanned reports whether every DIRECT DOMAIN child of the root has reached
// capture barrier 1 (domainSpecificController.phase >= Planned). It is the "direct domain children
// planned" half of the root's phase=Finished gate (design §4.2/§6.2).
//
// CSI VolumeSnapshot visibility leaves (the orphan/residual PVC wave) are skipped: they have no domain
// controller and therefore no phase, and are gated by the aggregator's fail-closed orphan-link gate
// (ChildrenReady=ChildrenLinkPending until each orphan child content is linked), not here. phaseByName
// maps a child ref Name to its observed capture phase (empty = no phase yet). A root with no domain
// children passes vacuously.
//
// Pure function (phases supplied by the caller) so the gate is unit-tested directly (design §9).
func allDirectDomainChildrenAtLeastPlanned(refs []storagev1alpha1.SnapshotChildRef, phaseByName map[string]storagev1alpha1.SnapshotCapturePhase) bool {
	for _, ref := range refs {
		if snapshotpkg.IsVolumeSnapshotVisibilityLeaf(ref) {
			continue
		}
		switch phaseByName[ref.Name] {
		case storagev1alpha1.SnapshotCapturePhasePlanned, storagev1alpha1.SnapshotCapturePhaseFinished:
			continue
		default:
			return false
		}
	}
	return true
}
