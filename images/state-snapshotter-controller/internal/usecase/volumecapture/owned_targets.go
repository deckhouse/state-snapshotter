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

package volumecapture

import (
	"context"
	"fmt"
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
)

// ListOwnedPVCTargetsForLogicalContent returns PVC volume targets owned by this logical SnapshotContent node.
// Root residual scope: namespace PVC candidates minus subtree-covered UIDs.
// Domain/non-root scope: only this node's dataRefs[] and pending VCR targets (not full namespace list).
func ListOwnedPVCTargetsForLogicalContent(
	ctx context.Context,
	c client.Reader,
	snap *storagev1alpha1.Snapshot,
	content *storagev1alpha1.SnapshotContent,
	dataBearing DataBearingKindFunc,
) ([]vcpkg.Target, error) {
	namespace, err := snapshotNamespaceForOwnedTargets(snap, content)
	if err != nil {
		return nil, err
	}
	// content may be nil in the "late Planned" pre-barrier wave: the residual root scope is content-free
	// (IsResidualRootPVCCaptureScope / listResidualRootOwnedPVCTargets ignore content), and the domain-node
	// path is nil-safe (listDomainNodeOwnedPVCTargets returns no targets for a nil content).
	var out []vcpkg.Target
	if IsResidualRootPVCCaptureScope(snap, content) {
		out, err = listResidualRootOwnedPVCTargets(ctx, c, namespace, snap, content, dataBearing)
	} else {
		out, err = listDomainNodeOwnedPVCTargets(ctx, c, namespace, content)
	}
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].UID < out[j].UID
	})
	return out, nil
}

// ListOwnedPVCTargetsForSnapshotContent is the domain/demo entry point when only namespace + content are known.
// Domain nodes never use namespace-wide residual discovery.
func ListOwnedPVCTargetsForSnapshotContent(
	ctx context.Context,
	c client.Reader,
	namespace string,
	content *storagev1alpha1.SnapshotContent,
) ([]vcpkg.Target, error) {
	return listDomainNodeOwnedPVCTargets(ctx, c, namespace, content)
}

func snapshotNamespaceForOwnedTargets(snap *storagev1alpha1.Snapshot, _ *storagev1alpha1.SnapshotContent) (string, error) {
	if snap != nil && snap.Namespace != "" {
		return snap.Namespace, nil
	}
	return "", fmt.Errorf("namespace is required to resolve owned PVC targets")
}
