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

package usecase

import (
	"context"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	volumecaptureuc "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase/volumecapture"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk"
)

// Run-graph errors for root manifest capture. ErrRunGraphChildSnapshotNotFound / ErrRunGraphChildNotBound
// are returned by ResolveChildSnapshotRefToBoundContentName (child_snapshot_resolve.go) — kept here as the
// shared run-graph error vocabulary — and surface transiently through the orphan-coverage walk while a
// child is still binding. The subtree manifest-exclude set is no longer computed in-reconciler (see
// BuildRootNamespaceManifestCaptureTargets): the fail-closed subtree readiness / not-Ready / double-capture
// signalling now lives in the subtree-manifest-identities service endpoint and surfaces on the SDK side as
// snapshotsdk.ErrSubtreeIdentitiesPending.
var (
	ErrRunGraphChildSnapshotNotFound = errors.New("child snapshot object not found for status.childrenSnapshotRefs entry")
	ErrRunGraphChildNotBound         = errors.New("child snapshot has empty boundSnapshotContentName")
)

// BuildRootNamespaceManifestCaptureTargets builds the root Snapshot's own manifest targets for the resolved
// target namespace: the namespace-scoped allowlist base MINUS (a) the residual/orphan root-owned PVC
// manifests (each captured by its own VolumeSnapshot domain child) and (b) the subtree identities already
// captured by descendant content nodes.
//
// The subtree exclude set (b) is supplied by the caller via subtreeExclude — the union of object identities
// returned by the snapshotcontents/<name>/subtree-manifest-identities service endpoint
// (snapshotsdk.SubtreeManifestIdentities), walked over the DIRECT children's bound content subtrees. That
// endpoint is FAIL-CLOSED: while any descendant ManifestCheckpoint is not Ready (or a child has not bound
// its content, or an object is double-captured) it returns 409 -> ErrSubtreeIdentitiesPending on the SDK
// side, and the caller requeues WITHOUT calling this builder — so a non-empty subtreeExclude here is always
// a complete, consistent subtree set (the wave barrier the in-reconciler archive read used to enforce is
// now the endpoint's job). When the root has no children the caller passes an empty subtreeExclude and this
// degenerates to the full namespace-scoped allowlist minus the residual PVCs.
func BuildRootNamespaceManifestCaptureTargets(
	ctx context.Context,
	dyn dynamic.Interface,
	disco discovery.DiscoveryInterface,
	c client.Reader,
	rootNS *storagev1alpha1.Snapshot,
	rootContentName string,
	snapshotKinds namespacemanifest.SnapshotMachineryGVKs,
	dataBearing volumecaptureuc.DataBearingKindFunc,
	subtreeExclude []snapshotsdk.SubtreeManifestIdentity,
) ([]namespacemanifest.ManifestTarget, []schema.GroupVersionResource, error) {
	targetNamespace := ResolveSnapshotTargetNamespace(rootNS)

	rootContent := &storagev1alpha1.SnapshotContent{}
	if err := c.Get(ctx, client.ObjectKey{Name: rootContentName}, rootContent); err != nil {
		return nil, nil, fmt.Errorf("get root SnapshotContent %q: %w", rootContentName, err)
	}

	ownedPVC, err := volumecaptureuc.OwnedPVCManifestTargetsForSnapshot(ctx, c, rootNS, rootContent, dataBearing)
	if err != nil {
		return nil, nil, err
	}

	// The manifest base is the whole namespace minus the built-in exclusions, minus objects carrying the
	// exclude veto label. The PVC leg applies the same label check, so a vetoed object is dropped from both
	// and never half-captured. The label-excluded objects are not recorded here: this leg runs after barrier
	// 1, when the excluded membership is already frozen. RootLabelExcludedManifestRefs recorded them at
	// planning time, and that frozen record is subtracted below as well.
	base, _, unreadable, err := namespacemanifest.BuildManifestCaptureTargets(ctx, dyn, disco, targetNamespace, snapshotKinds)
	if err != nil {
		return nil, unreadable, err
	}
	base = withoutFrozenExcludedRefs(base, rootNS)

	// Capture the Namespace object itself in the root node's own manifests. It is the only cluster-scoped
	// target allowed in a namespace snapshot (name == the snapshot's target namespace, enforced by the MCR
	// webhook and the capture executor). Downstream: /manifests serves it verbatim, while the restore
	// sanitizer drops cluster-scoped objects (metadata.namespace == "") so it never re-applies. It is never
	// captured by any descendant node, so it never appears in the subtree exclude set below and survives
	// FilterManifestTargets. Side effect: the root MCR is never empty (always has at least this target).
	base = append(base, namespacemanifest.ManifestTarget{
		APIVersion: "v1",
		Kind:       "Namespace",
		Name:       targetNamespace,
	})

	// A residual/orphan root PVC is captured as its own VolumeSnapshot domain child: that child owns
	// its own SnapshotContent + ManifestCheckpoint holding the PVC manifest +
	// its own data leg. The root is a pure aggregator (dataRef=nil) and MUST NOT carry any PVC manifest. So
	// the residual root-owned PVCs (ownedPVC) are excluded from the root MCR UP FRONT — independent of
	// whether the orphan VolumeSnapshot's content / MCP already exists — because CSI binding of the orphan
	// VolumeSnapshot is async and would otherwise race the (near-instant) root manifest leg into
	// double-capturing the PVC manifest on both the root and the child (co-ownership violation).
	// Every residual root PVC goes through ensureOrphanPVCVolumeSnapshots → VolumeSnapshot child, so dropping
	// them here never loses a manifest.
	exclude := make(map[string]struct{}, len(ownedPVC)+len(subtreeExclude))
	for _, t := range ownedPVC {
		exclude[namespacemanifest.ManifestTargetDedupKey(targetNamespace, t)] = struct{}{}
	}
	// Subtract the manifest objects already captured across descendant content-node subtrees (E5 / INV-S0),
	// as reported by the subtree-manifest-identities endpoint. This covers domain-owned PVC manifests (which
	// never appear in the root residual set) and is the durable dedup once children publish their MCPs.
	for _, id := range subtreeExclude {
		exclude[subtreeIdentityExcludeKey(id)] = struct{}{}
	}
	return namespacemanifest.FilterManifestTargets(base, exclude, targetNamespace), unreadable, nil
}

// RootLabelExcludedManifestRefs returns the objects of the root's target namespace that the manifest leg
// leaves out because they carry the exclude label, as the ExcludedObjectRefs the root records in
// status.captureState.domainSpecificController.excludedRefs. It is called at planning time, before barrier
// 1 freezes the excluded membership, so the record is fixed together with the child set; the manifest leg
// itself is planned later and only subtracts that record (see BuildRootNamespaceManifestCaptureTargets).
//
// Only objects the manifest leg would otherwise have captured are returned: namespacemanifest applies the
// built-in rules first, so a labelled object those rules drop anyway is not a user exclusion and is not
// here. PersistentVolumeClaims are left out as well: whether a PVC would otherwise have been captured
// depends on the subtree coverage, which only the volume leg knows, so the volume leg records them
// (volumecapture.ListResidualRootPVCTargets).
//
// unreadable carries the resource types that could not be listed; the caller must not freeze a record that
// misses them, the same fail-closed rule the manifest leg follows.
func RootLabelExcludedManifestRefs(
	ctx context.Context,
	dyn dynamic.Interface,
	disco discovery.DiscoveryInterface,
	rootNS *storagev1alpha1.Snapshot,
	snapshotKinds namespacemanifest.SnapshotMachineryGVKs,
) ([]storagev1alpha1.ExcludedObjectRef, []schema.GroupVersionResource, error) {
	_, excluded, unreadable, err := namespacemanifest.BuildManifestCaptureTargets(ctx, dyn, disco, ResolveSnapshotTargetNamespace(rootNS), snapshotKinds)
	if err != nil {
		return nil, unreadable, err
	}
	out := make([]storagev1alpha1.ExcludedObjectRef, 0, len(excluded))
	for _, t := range excluded {
		if t.APIVersion == "v1" && t.Kind == "PersistentVolumeClaim" {
			continue
		}
		out = append(out, storagev1alpha1.ExcludedObjectRef{APIVersion: t.APIVersion, Kind: t.Kind, Name: t.Name})
	}
	return out, unreadable, nil
}

// withoutFrozenExcludedRefs drops from the manifest base every object the root recorded as excluded at
// planning time (status.captureState.domainSpecificController.excludedRefs). The label check alone would
// capture an object whose label was removed after barrier 1, while the snapshot still lists it as
// excluded; subtracting the record keeps the two consistent. The match is by group, kind and name: a
// domain source may be recorded under a version other than the one discovery prefers.
func withoutFrozenExcludedRefs(base []namespacemanifest.ManifestTarget, rootNS *storagev1alpha1.Snapshot) []namespacemanifest.ManifestTarget {
	cs := rootNS.Status.CaptureState
	if cs == nil || cs.DomainSpecificController == nil || len(cs.DomainSpecificController.ExcludedRefs) == 0 {
		return base
	}
	frozen := make(map[string]struct{}, len(cs.DomainSpecificController.ExcludedRefs))
	for _, ref := range cs.DomainSpecificController.ExcludedRefs {
		frozen[groupKindNameKey(ref.APIVersion, ref.Kind, ref.Name)] = struct{}{}
	}
	out := make([]namespacemanifest.ManifestTarget, 0, len(base))
	for _, t := range base {
		if _, drop := frozen[groupKindNameKey(t.APIVersion, t.Kind, t.Name)]; drop {
			continue
		}
		out = append(out, t)
	}
	return out
}

// groupKindNameKey renders a version-agnostic identity. An unparsable apiVersion is kept verbatim as the
// group so it still matches only itself.
func groupKindNameKey(apiVersion, kind, name string) string {
	group := apiVersion
	if gv, err := schema.ParseGroupVersion(apiVersion); err == nil {
		group = gv.Group
	}
	return group + "|" + kind + "|" + name
}

// subtreeIdentityExcludeKey renders a subtree-manifest identity to the same dedup key
// namespacemanifest.ManifestTargetDedupKey / aggregatedObjectIdentityKey use (apiVersion|kind|ns|name, with
// a cluster-scoped object's empty namespace normalized to "_cluster"), so an identity captured in a
// descendant subtree matches and drops the corresponding base target for the same object. The identity's
// uid is intentionally NOT part of the key (a recreated object of the same name is still the same manifest
// slot for exclude purposes).
func subtreeIdentityExcludeKey(id snapshotsdk.SubtreeManifestIdentity) string {
	ns := id.Namespace
	if ns == "" {
		ns = "_cluster"
	}
	return fmt.Sprintf("%s|%s|%s|%s", id.APIVersion, id.Kind, ns, id.Name)
}
