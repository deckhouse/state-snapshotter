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

	// resourceSelector narrows the manifest base to objects matching the user selector (nil = capture all).
	// The same selector is applied to the PVC and CSD legs so excluded objects are dropped consistently.
	selector, err := rootNS.ResolveResourceSelector()
	if err != nil {
		return nil, nil, fmt.Errorf("resolve spec.resourceSelector: %w", err)
	}

	base, unreadable, err := namespacemanifest.BuildManifestCaptureTargets(ctx, dyn, disco, targetNamespace, snapshotKinds, selector)
	if err != nil {
		return nil, unreadable, err
	}

	// A residual/orphan root PVC is captured as its own VolumeSnapshot domain child (content-single-writer
	// design §11.6): that child owns its own SnapshotContent + ManifestCheckpoint holding the PVC manifest +
	// its own data leg. The root is a pure aggregator (dataRef=nil) and MUST NOT carry any PVC manifest. So
	// the residual root-owned PVCs (ownedPVC) are excluded from the root MCR UP FRONT — independent of
	// whether the orphan VolumeSnapshot's content / MCP already exists — because CSI binding of the orphan
	// VolumeSnapshot is async and would otherwise race the (near-instant) root manifest leg into
	// double-capturing the PVC manifest on both the root and the child (co-ownership violation, spec §3.9.2).
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
