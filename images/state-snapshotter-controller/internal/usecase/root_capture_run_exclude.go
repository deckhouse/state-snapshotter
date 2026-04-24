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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshotgraphregistry"
)

// Run-graph errors for root manifest capture (INV-S0 / INV-E1, E5).
var (
	ErrRunGraphChildSnapshotNotFound = errors.New("child snapshot object not found for status.childrenSnapshotRefs entry")
	ErrRunGraphChildNotBound         = errors.New("child snapshot has empty boundSnapshotContentName")
	ErrRunGraphChildNotReachable     = errors.New("child snapshot content not reachable from root NamespaceSnapshotContent via childrenSnapshotContentRefs graph")
	// ErrSubtreeManifestCapturePending is returned when exclude cannot be computed yet because a descendant
	// NamespaceSnapshotContent has no MCP link or the MCP is not Ready (fail-closed: do not create root MCR with an incomplete exclude set).
	ErrSubtreeManifestCapturePending = errors.New("subtree manifest capture pending for root exclude")
	// ErrSubtreeManifestCaptureFailed is returned when a descendant ManifestCheckpoint is terminally Failed
	// (distinct from pending / not Ready yet).
	ErrSubtreeManifestCaptureFailed = errors.New("subtree manifest capture failed for root exclude")
)

// BuildRootNamespaceManifestCaptureTargets lists namespace allowlist targets then, when the root
// NamespaceSnapshot has status.childrenSnapshotRefs, subtracts manifest objects already captured
// in descendant NamespaceSnapshotContent ManifestCheckpoints reachable only via that ref graph.
// It does not list unrelated snapshots in the namespace to infer subtree membership (INV-S0).
//
// live supplies the current GVKRegistry (see pkg/snapshot.GVKRegistry) and optional TryRefresh when the
// registry may be stale vs RESTMapper (CRD appeared after last DSC reconcile). Child snapshot refs carry
// explicit apiVersion/kind/name (strict); subtree traversal still uses the registry for snapshot↔content mapping.
//
// When status.childrenSnapshotRefs is empty, behavior matches N2a root capture: full namespace
// allowlist without subtree exclude.
//
// While childrenSnapshotRefs is non-empty, descendant NamespaceSnapshotContent nodes reached from the root
// must publish a Ready ManifestCheckpoint before exclude keys are derived; otherwise ErrSubtreeManifestCapturePending.
func BuildRootNamespaceManifestCaptureTargets(
	ctx context.Context,
	arch *ArchiveService,
	dyn dynamic.Interface,
	c client.Reader,
	live snapshotgraphregistry.LiveReader,
	rootNS *storagev1alpha1.NamespaceSnapshot,
	rootNSCName string,
) ([]namespacemanifest.ManifestTarget, error) {
	if arch == nil {
		return nil, fmt.Errorf("archive service is required for root capture when childrenSnapshotRefs may be set")
	}
	base, err := namespacemanifest.BuildManifestCaptureTargets(ctx, dyn, rootNS.Namespace)
	if err != nil {
		return nil, err
	}
	if len(rootNS.Status.ChildrenSnapshotRefs) == 0 {
		return base, nil
	}
	if live == nil {
		return nil, fmt.Errorf("snapshot graph registry is required when status.childrenSnapshotRefs is non-empty")
	}
	excl, err := collectRunSubtreeManifestExcludeKeys(ctx, arch, c, live, rootNS, rootNSCName)
	if err != nil {
		return nil, err
	}
	return namespacemanifest.FilterManifestTargets(base, excl, rootNS.Namespace), nil
}

func collectRunSubtreeManifestExcludeKeys(
	ctx context.Context,
	arch *ArchiveService,
	c client.Reader,
	live snapshotgraphregistry.LiveReader,
	rootNS *storagev1alpha1.NamespaceSnapshot,
	rootNSCName string,
) (map[string]struct{}, error) {
	reg := live.Current()
	if reg == nil {
		if err := live.TryRefresh(ctx); err != nil && !errors.Is(err, snapshotgraphregistry.ErrRefreshNotConfigured) {
			return nil, err
		}
		reg = live.Current()
	}
	if reg == nil {
		return nil, fmt.Errorf("%w: GVK registry is required when status.childrenSnapshotRefs is non-empty (graph registry not ready or DSC/bootstrap pairs not merged yet)", snapshotgraphregistry.ErrGraphRegistryNotReady)
	}
	visited := make(map[string]struct{})
	exclude := make(map[string]struct{})

	visitNSC := func(ctx context.Context, nsc *storagev1alpha1.NamespaceSnapshotContent) error {
		visited[nsc.Name] = struct{}{}
		if nsc.Name == rootNSCName {
			return nil
		}
		if nsc.Status.ManifestCheckpointName == "" {
			return fmt.Errorf("%w: NamespaceSnapshotContent %q has empty manifestCheckpointName (subtree capture not finished)",
				ErrSubtreeManifestCapturePending, nsc.Name)
		}
		mcp := &ssv1alpha1.ManifestCheckpoint{}
		if err := c.Get(ctx, client.ObjectKey{Name: nsc.Status.ManifestCheckpointName}, mcp); err != nil {
			if apierrors.IsNotFound(err) {
				return fmt.Errorf("get ManifestCheckpoint %q for NamespaceSnapshotContent %q: %w",
					nsc.Status.ManifestCheckpointName, nsc.Name, err)
			}
			return fmt.Errorf("get ManifestCheckpoint %q: %w", nsc.Status.ManifestCheckpointName, err)
		}
		readyCond := meta.FindStatusCondition(mcp.Status.Conditions, ssv1alpha1.ManifestCheckpointConditionTypeReady)
		if readyCond != nil && readyCond.Status == metav1.ConditionFalse &&
			readyCond.Reason == ssv1alpha1.ManifestCheckpointConditionReasonFailed {
			return fmt.Errorf("%w: ManifestCheckpoint %q for NamespaceSnapshotContent %q: %s",
				ErrSubtreeManifestCaptureFailed, nsc.Status.ManifestCheckpointName, nsc.Name, readyCond.Message)
		}
		if readyCond == nil || readyCond.Status != metav1.ConditionTrue {
			return fmt.Errorf("%w: ManifestCheckpoint %q for NamespaceSnapshotContent %q is not Ready (exclude set would be incomplete)",
				ErrSubtreeManifestCapturePending, nsc.Status.ManifestCheckpointName, nsc.Name)
		}
		req := &ArchiveRequest{
			CheckpointName:  nsc.Status.ManifestCheckpointName,
			CheckpointUID:   string(mcp.UID),
			SourceNamespace: mcp.Spec.SourceNamespace,
		}
		raw, _, err := arch.GetArchiveFromCheckpoint(ctx, mcp, req)
		if err != nil {
			return fmt.Errorf("read ManifestCheckpoint %q archive: %w", nsc.Status.ManifestCheckpointName, err)
		}
		var arr []map[string]interface{}
		if err := json.Unmarshal(raw, &arr); err != nil {
			return fmt.Errorf("decode ManifestCheckpoint %q JSON: %w", nsc.Status.ManifestCheckpointName, err)
		}
		for _, obj := range arr {
			k, err := manifestObjectIdentityKeyFromMap(obj)
			if err != nil {
				return fmt.Errorf("ManifestCheckpoint %q: %w", nsc.Status.ManifestCheckpointName, err)
			}
			exclude[k] = struct{}{}
		}
		return nil
	}

	hooks := &DedicatedContentVisitHooks{
		Visit: func(_ context.Context, _ schema.GroupVersionKind, contentName string, _ *unstructured.Unstructured, _ bool) error {
			visited[contentName] = struct{}{}
			return nil
		},
	}

	if err := WalkNamespaceSnapshotContentSubtreeWithRegistryMaybeRefresh(ctx, c, rootNSCName, visitNSC, live, hooks); err != nil {
		return nil, err
	}

	for i := range rootNS.Status.ChildrenSnapshotRefs {
		ch := rootNS.Status.ChildrenSnapshotRefs[i]
		resolved, err := ResolveChildSnapshotRefToBoundContentName(ctx, c, ch, rootNS.Namespace)
		if err != nil {
			return nil, err
		}
		ns := ch.Namespace
		if ns == "" {
			ns = rootNS.Namespace
		}
		if _, ok := visited[resolved]; !ok {
			return nil, fmt.Errorf("%w: childrenSnapshotRefs %s/%s -> %q not visited from root NamespaceSnapshotContent %q",
				ErrRunGraphChildNotReachable, ns, ch.Name, resolved, rootNSCName)
		}
	}

	return exclude, nil
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
