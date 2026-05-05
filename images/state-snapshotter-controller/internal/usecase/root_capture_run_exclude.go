/*
Copyright 2025 Flant JSC

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
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
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
	c client.Reader,
	rootNS *storagev1alpha1.Snapshot,
	rootContentName string,
) ([]namespacemanifest.ManifestTarget, error) {
	if arch == nil {
		return nil, fmt.Errorf("archive service is required for root capture when childrenSnapshotRefs may be set")
	}
	targetNamespace := ResolveSnapshotTargetNamespace(rootNS)
	base, err := namespacemanifest.BuildManifestCaptureTargets(ctx, dyn, targetNamespace)
	if err != nil {
		return nil, err
	}
	if len(rootNS.Status.ChildrenSnapshotRefs) == 0 {
		return base, nil
	}
	excl, err := collectRunSubtreeManifestExcludeKeys(ctx, arch, c, rootNS, rootContentName)
	if err != nil {
		return nil, err
	}
	filtered := namespacemanifest.FilterManifestTargets(base, excl, targetNamespace)
	return filtered, nil
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
		resolved, err := ResolveChildSnapshotRefToBoundContentName(ctx, c, ch, rootNS.Namespace)
		if err != nil {
			return nil, err
		}
		if _, ok := visited[resolved]; !ok {
			return nil, fmt.Errorf("%w: childrenSnapshotRefs %s/%s -> %q not visited from root SnapshotContent %q",
				ErrRunGraphChildNotReachable, rootNS.Namespace, ch.Name, resolved, rootContentName)
		}
	}

	return exclude, nil
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
