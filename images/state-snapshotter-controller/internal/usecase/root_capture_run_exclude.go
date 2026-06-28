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
	"encoding/json"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	volumecaptureuc "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase/volumecapture"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// Run-graph errors for root manifest capture (INV-S0 / INV-E1, E5).
var (
	ErrRunGraphChildSnapshotNotFound = errors.New("child snapshot object not found for status.childrenSnapshotRefs entry")
	ErrRunGraphChildNotBound         = errors.New("child snapshot has empty boundSnapshotContentName")
	ErrRunGraphChildNotReachable     = errors.New("child snapshot content not reachable from root SnapshotContent via childrenSnapshotContentRefs graph")
	// ErrSubtreeManifestCapturePending is returned when exclude cannot be computed yet because a descendant
	// SnapshotContent has no MCP link or the MCP is not Ready (fail-closed: do not create root MCR with an incomplete exclude set).
	ErrSubtreeManifestCapturePending = errors.New("subtree manifest capture pending for root exclude")
	// ErrSubtreeManifestCaptureFailed is returned when a descendant ManifestCheckpoint is terminally Failed
	// (distinct from pending / not Ready yet).
	ErrSubtreeManifestCaptureFailed = errors.New("subtree manifest capture failed for root exclude")
)

// BuildRootNamespaceManifestCaptureTargets builds Snapshot own targets for the resolved
// target namespace: namespace-scoped allowlist targets, then, when the root Snapshot has
// status.childrenSnapshotRefs, subtracts manifest objects already captured in descendant content-node
// ManifestCheckpoints reachable only via that ref graph.
// It does not list unrelated snapshots in the namespace to infer subtree membership (INV-S0).
//
// Child snapshot refs carry explicit apiVersion/kind/name (strict); subtree traversal reads the common
// SnapshotContent tree by status.childrenSnapshotContentRefs.
//
// When status.childrenSnapshotRefs is empty, behavior matches N2a root capture: full
// namespace-scoped allowlist without subtree exclude.
//
// While childrenSnapshotRefs is non-empty, descendant content nodes reached from the root must publish
// a Ready ManifestCheckpoint before exclude keys are derived; otherwise ErrSubtreeManifestCapturePending.
func BuildRootNamespaceManifestCaptureTargets(
	ctx context.Context,
	arch *ArchiveService,
	dyn dynamic.Interface,
	disco discovery.DiscoveryInterface,
	c client.Reader,
	rootNS *storagev1alpha1.Snapshot,
	rootContentName string,
	snapshotKinds namespacemanifest.SnapshotMachineryGVKs,
) ([]namespacemanifest.ManifestTarget, []schema.GroupVersionResource, error) {
	if arch == nil {
		return nil, nil, fmt.Errorf("archive service is required for root capture when childrenSnapshotRefs may be set")
	}
	targetNamespace := ResolveSnapshotTargetNamespace(rootNS)

	rootContent := &storagev1alpha1.SnapshotContent{}
	if err := c.Get(ctx, client.ObjectKey{Name: rootContentName}, rootContent); err != nil {
		return nil, nil, fmt.Errorf("get root SnapshotContent %q: %w", rootContentName, err)
	}

	// Subtree readiness pre-check BEFORE the expensive full-namespace discovery listing. While the root has
	// a real subtree, descendant content nodes must publish a Ready ManifestCheckpoint before the exclude
	// set can be computed; otherwise this returns ErrSubtreeManifestCapturePending/...Failed. Computing it
	// first avoids listing the whole namespace on every requeue while children are still publishing MCPs.
	hasSubtree := hasNonVisibilityChildSnapshotRefs(rootNS.Status.ChildrenSnapshotRefs) || len(rootContent.Status.ChildrenSnapshotContentRefs) > 0
	var subtreeExcl map[string]struct{}
	if hasSubtree {
		var err error
		subtreeExcl, err = collectRunSubtreeManifestExcludeKeys(ctx, arch, c, rootNS, rootContentName)
		if err != nil {
			return nil, nil, err
		}
	}

	ownedPVC, err := volumecaptureuc.OwnedPVCManifestTargetsForSnapshot(ctx, c, rootNS, rootContent)
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

	// Variant A: a residual/orphan root PVC is captured as a standalone child volume node (its own
	// SnapshotContent + its own ManifestCheckpoint holding that PVC's manifest + its own dataRef). The root
	// is a pure aggregator (dataRef=nil) and MUST NOT carry any PVC manifest. So the residual root-owned
	// PVCs (ownedPVC) are excluded from the root MCR UP FRONT — independent of whether their child volume
	// nodes / per-orphan MCPs already exist — because CSI binding of the orphan VolumeSnapshot is async and
	// would otherwise race the (near-instant) root manifest leg into double-capturing the PVC manifest on
	// both the root and the child (co-ownership violation, spec §3.9.2). Every residual root PVC goes
	// through ensureOrphanPVCVolumeSnapshots → child volume node, so dropping them here never loses a
	// manifest.
	exclude := make(map[string]struct{}, len(ownedPVC)+len(subtreeExcl))
	for _, t := range ownedPVC {
		exclude[namespacemanifest.ManifestTargetDedupKey(targetNamespace, t)] = struct{}{}
	}
	// When the root has a real subtree (domain children or linked child volume nodes), also subtract the
	// manifest objects already captured in descendant content-node ManifestCheckpoints (E5 / INV-S0). This
	// covers domain-owned PVC manifests (which never appear in the root residual set) and is the durable
	// dedup once children publish their MCPs.
	for k := range subtreeExcl {
		exclude[k] = struct{}{}
	}
	return namespacemanifest.FilterManifestTargets(base, exclude, targetNamespace), unreadable, nil
}

// hasNonVisibilityChildSnapshotRefs reports whether any child ref is a real domain/subtree child.
// CSI VolumeSnapshot visibility leaves (orphan-PVC data leg) are excluded.
func hasNonVisibilityChildSnapshotRefs(refs []storagev1alpha1.SnapshotChildRef) bool {
	for i := range refs {
		if snapshotpkg.IsVolumeSnapshotVisibilityLeaf(refs[i]) {
			continue
		}
		return true
	}
	return false
}

func collectRunSubtreeManifestExcludeKeys(
	ctx context.Context,
	arch *ArchiveService,
	c client.Reader,
	rootNS *storagev1alpha1.Snapshot,
	rootContentName string,
) (map[string]struct{}, error) {
	visited := make(map[string]struct{})
	exclude := make(map[string]struct{})

	visitContent := func(ctx context.Context, content *storagev1alpha1.SnapshotContent) error {
		visited[content.Name] = struct{}{}
		if content.Name == rootContentName {
			return nil
		}
		return appendManifestCheckpointObjectsToExclude(ctx, arch, c, content.Status.ManifestCheckpointName, fmt.Sprintf("SnapshotContent %q", content.Name), exclude)
	}

	if err := WalkSnapshotContentSubtree(ctx, c, rootContentName, visitContent); err != nil {
		return nil, err
	}

	for i := range rootNS.Status.ChildrenSnapshotRefs {
		ch := rootNS.Status.ChildrenSnapshotRefs[i]
		if snapshotpkg.IsVolumeSnapshotVisibilityLeaf(ch) {
			continue
		}
		resolved, err := ResolveChildSnapshotRefToBoundContentName(ctx, c, ch, rootNS.Namespace)
		if err != nil {
			return nil, err
		}
		if _, ok := visited[resolved]; !ok {
			return nil, fmt.Errorf("%w: childrenSnapshotRefs %s/%s -> %q not visited from root SnapshotContent %q",
				ErrRunGraphChildNotReachable, rootNS.Namespace, ch.Name, resolved, rootContentName)
		}
		// Wave barrier: the direct child's whole subtree must be ManifestsArchived before the root MCR is
		// built. See requireContentManifestsArchived.
		if err := requireContentManifestsArchived(ctx, c, resolved); err != nil {
			return nil, err
		}
	}

	return exclude, nil
}

// requireContentManifestsArchived is the wave barrier for root manifest capture: a declared direct child
// of the root snapshot must have its bound SnapshotContent at ManifestsArchived=True before the root MCR
// is built. Because the content-node ManifestsArchived latch is fail-closed against declared-but-unlinked
// children (see snapshotcontent.aggregateChildrenManifestsArchived), a direct child's True transitively
// guarantees its ENTIRE subtree is archived and fully edge-linked. That makes WalkSnapshotContentSubtree
// reach every descendant content, so the exclude set is complete and a descendant-captured object can
// never leak back into the root MCP (the 409 duplicate-object race).
//
// A child not yet archived -> ErrSubtreeManifestCapturePending (transient requeue); a child terminally
// ManifestsArchived=False/ManifestsArchiveFailed -> ErrSubtreeManifestCaptureFailed.
//
// The root's OWN ManifestsArchived is intentionally NOT consulted: it can only become True after the root
// MCR exists and is processed (own ManifestsReady) AND all children are archived, so gating root-MCR
// creation on it would be circular and deadlock. The root content node is not special; its own latch is
// computed by the same content-controller path once its MCP is ready and children are archived.
func requireContentManifestsArchived(ctx context.Context, c client.Reader, contentName string) error {
	content := &storagev1alpha1.SnapshotContent{}
	if err := c.Get(ctx, client.ObjectKey{Name: contentName}, content); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("%w: direct child SnapshotContent %q not found (subtree not archived yet)",
				ErrSubtreeManifestCapturePending, contentName)
		}
		return fmt.Errorf("get direct child SnapshotContent %q: %w", contentName, err)
	}
	cond := meta.FindStatusCondition(content.Status.Conditions, snapshotpkg.ConditionManifestsArchived)
	if cond != nil && cond.Status == metav1.ConditionTrue {
		return nil
	}
	if cond != nil && cond.Status == metav1.ConditionFalse && cond.Reason == snapshotpkg.ReasonManifestsArchiveFailed {
		return fmt.Errorf("%w: direct child SnapshotContent %q: %s",
			ErrSubtreeManifestCaptureFailed, contentName, cond.Message)
	}
	return fmt.Errorf("%w: direct child SnapshotContent %q ManifestsArchived not yet True",
		ErrSubtreeManifestCapturePending, contentName)
}

func appendManifestCheckpointObjectsToExclude(
	ctx context.Context,
	arch *ArchiveService,
	c client.Reader,
	mcpName string,
	contentDescription string,
	exclude map[string]struct{},
) error {
	if mcpName == "" {
		return fmt.Errorf("%w: %s has empty manifestCheckpointName (subtree capture not finished)",
			ErrSubtreeManifestCapturePending, contentDescription)
	}
	mcp := &ssv1alpha1.ManifestCheckpoint{}
	if err := c.Get(ctx, client.ObjectKey{Name: mcpName}, mcp); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("%w: ManifestCheckpoint %q for %s not found (exclude set would be incomplete)",
				ErrSubtreeManifestCapturePending, mcpName, contentDescription)
		}
		return fmt.Errorf("get ManifestCheckpoint %q: %w", mcpName, err)
	}
	readyCond := meta.FindStatusCondition(mcp.Status.Conditions, ssv1alpha1.ManifestCheckpointConditionTypeReady)
	if readyCond != nil && readyCond.Status == metav1.ConditionFalse &&
		readyCond.Reason == ssv1alpha1.ManifestCheckpointConditionReasonFailed {
		return fmt.Errorf("%w: ManifestCheckpoint %q for %s: %s",
			ErrSubtreeManifestCaptureFailed, mcpName, contentDescription, readyCond.Message)
	}
	if readyCond == nil || readyCond.Status != metav1.ConditionTrue {
		return fmt.Errorf("%w: ManifestCheckpoint %q for %s is not Ready (exclude set would be incomplete)",
			ErrSubtreeManifestCapturePending, mcpName, contentDescription)
	}
	req := &ArchiveRequest{
		CheckpointName:  mcpName,
		CheckpointUID:   string(mcp.UID),
		SourceNamespace: mcp.Spec.SourceNamespace,
	}
	raw, _, err := arch.GetArchiveFromCheckpoint(ctx, mcp, req)
	if err != nil {
		return fmt.Errorf("read ManifestCheckpoint %q archive: %w", mcpName, err)
	}
	var arr []map[string]interface{}
	if err := json.Unmarshal(raw, &arr); err != nil {
		return fmt.Errorf("decode ManifestCheckpoint %q JSON: %w", mcpName, err)
	}
	for _, obj := range arr {
		k, err := manifestObjectIdentityKeyFromMap(obj)
		if err != nil {
			return fmt.Errorf("ManifestCheckpoint %q: %w", mcpName, err)
		}
		exclude[k] = struct{}{}
	}
	return nil
}

func manifestObjectIdentityKeyFromMap(obj map[string]interface{}) (string, error) {
	apiVersion, _ := obj["apiVersion"].(string)
	kind, _ := obj["kind"].(string)
	meta, ok := obj["metadata"].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("object missing metadata")
	}
	name, _ := meta["name"].(string)
	ns, _ := meta["namespace"].(string)
	if apiVersion == "" || kind == "" || name == "" {
		return "", fmt.Errorf("object missing apiVersion, kind, or metadata.name")
	}
	if ns == "" {
		ns = "_cluster"
	}
	return fmt.Sprintf("%s|%s|%s|%s", apiVersion, kind, ns, name), nil
}
