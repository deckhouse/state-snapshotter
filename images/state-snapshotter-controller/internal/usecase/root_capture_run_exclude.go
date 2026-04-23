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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
)

// Run-graph errors for root manifest capture (INV-S0 / INV-E1, E5).
var (
	ErrRunGraphChildSnapshotNotFound = errors.New("child snapshot object not found for status.childrenSnapshotRefs entry")
	ErrRunGraphChildNotBound         = errors.New("child snapshot has empty boundSnapshotContentName")
	ErrRunGraphChildNotReachable     = errors.New("child snapshot content not reachable from root NamespaceSnapshotContent via childrenSnapshotContentRefs graph")
)

// BuildRootNamespaceManifestCaptureTargets lists namespace allowlist targets then, when the root
// NamespaceSnapshot has status.childrenSnapshotRefs, subtracts manifest objects already captured
// in descendant NamespaceSnapshotContent ManifestCheckpoints reachable only via that ref graph.
// It does not list unrelated snapshots in the namespace to infer subtree membership (INV-S0).
//
// Current slice limitations (not final universal contract):
//   - When status.childrenSnapshotRefs is empty, behavior matches legacy root capture: full namespace
//     allowlist without subtree exclude (transition mode; not graph-first for that case).
//   - resolveChildSnapshotContentName is hardcoded to NamespaceSnapshot, DemoVirtualDiskSnapshot, and
//     DemoVirtualMachineSnapshot until a generic child resolution exists in API/spec.
//
// If a root MCR was created before a descendant MCP became readable, spec.targets may be stale until the
// operator deletes the MCR (CapturePlanDrift); exclude is applied on the next create from a fresh plan.
func BuildRootNamespaceManifestCaptureTargets(
	ctx context.Context,
	arch *ArchiveService,
	dyn dynamic.Interface,
	c client.Reader,
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
	excl, err := collectRunSubtreeManifestExcludeKeys(ctx, arch, c, rootNS, rootNSCName)
	if err != nil {
		return nil, err
	}
	return namespacemanifest.FilterManifestTargets(base, excl, rootNS.Namespace), nil
}

func collectRunSubtreeManifestExcludeKeys(
	ctx context.Context,
	arch *ArchiveService,
	c client.Reader,
	rootNS *storagev1alpha1.NamespaceSnapshot,
	rootNSCName string,
) (map[string]struct{}, error) {
	visited := make(map[string]struct{})
	exclude := make(map[string]struct{})

	visitNSC := func(ctx context.Context, nsc *storagev1alpha1.NamespaceSnapshotContent) error {
		visited[nsc.Name] = struct{}{}
		if nsc.Name == rootNSCName || nsc.Status.ManifestCheckpointName == "" {
			return nil
		}
		mcp := &ssv1alpha1.ManifestCheckpoint{}
		if err := c.Get(ctx, client.ObjectKey{Name: nsc.Status.ManifestCheckpointName}, mcp); err != nil {
			if apierrors.IsNotFound(err) {
				return fmt.Errorf("get ManifestCheckpoint %q for NamespaceSnapshotContent %q: %w",
					nsc.Status.ManifestCheckpointName, nsc.Name, err)
			}
			return fmt.Errorf("get ManifestCheckpoint %q: %w", nsc.Status.ManifestCheckpointName, err)
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

	leaves := &DemoSnapshotContentLeaves{
		DiskContent: func(ctx context.Context, d *demov1alpha1.DemoVirtualDiskSnapshotContent) error {
			visited[d.Name] = struct{}{}
			return nil
		},
		MachineContent: func(ctx context.Context, m *demov1alpha1.DemoVirtualMachineSnapshotContent) error {
			visited[m.Name] = struct{}{}
			return nil
		},
	}

	if err := WalkNamespaceSnapshotContentSubtreeWithAllDemoLeaves(ctx, c, rootNSCName, visitNSC, leaves); err != nil {
		return nil, err
	}

	for i := range rootNS.Status.ChildrenSnapshotRefs {
		ch := rootNS.Status.ChildrenSnapshotRefs[i]
		ns := ch.Namespace
		if ns == "" {
			ns = rootNS.Namespace
		}
		resolved, err := resolveChildSnapshotContentName(ctx, c, ns, ch.Name)
		if err != nil {
			return nil, err
		}
		if _, ok := visited[resolved]; !ok {
			return nil, fmt.Errorf("%w: childrenSnapshotRefs %s/%s -> %q not visited from root NamespaceSnapshotContent %q",
				ErrRunGraphChildNotReachable, ns, ch.Name, resolved, rootNSCName)
		}
	}

	return exclude, nil
}

func resolveChildSnapshotContentName(ctx context.Context, c client.Reader, childNS, childName string) (string, error) {
	key := types.NamespacedName{Namespace: childNS, Name: childName}

	nsChild := &storagev1alpha1.NamespaceSnapshot{}
	if err := c.Get(ctx, key, nsChild); err == nil {
		if nsChild.Status.BoundSnapshotContentName == "" {
			return "", fmt.Errorf("%s/%s: %w", childNS, childName, ErrRunGraphChildNotBound)
		}
		return nsChild.Status.BoundSnapshotContentName, nil
	} else if !apierrors.IsNotFound(err) {
		return "", err
	}

	disk := &demov1alpha1.DemoVirtualDiskSnapshot{}
	if err := c.Get(ctx, key, disk); err == nil {
		if disk.Status.BoundSnapshotContentName == "" {
			return "", fmt.Errorf("%s/%s: %w", childNS, childName, ErrRunGraphChildNotBound)
		}
		return disk.Status.BoundSnapshotContentName, nil
	} else if !apierrors.IsNotFound(err) {
		return "", err
	}

	vm := &demov1alpha1.DemoVirtualMachineSnapshot{}
	if err := c.Get(ctx, key, vm); err == nil {
		if vm.Status.BoundSnapshotContentName == "" {
			return "", fmt.Errorf("%s/%s: %w", childNS, childName, ErrRunGraphChildNotBound)
		}
		return vm.Status.BoundSnapshotContentName, nil
	} else if !apierrors.IsNotFound(err) {
		return "", err
	}

	return "", fmt.Errorf("%s/%s: %w", childNS, childName, ErrRunGraphChildSnapshotNotFound)
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
