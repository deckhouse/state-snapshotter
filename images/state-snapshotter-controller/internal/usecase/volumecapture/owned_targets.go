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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
)

// ListOwnedPVCTargetsForLogicalContent returns residual PVC targets owned by this logical SnapshotContent node:
// namespace PVC candidates minus subtree-covered UIDs (PR-6). Empty slice means manifest-only (no volume leg).
func ListOwnedPVCTargetsForLogicalContent(
	ctx context.Context,
	c client.Reader,
	snap *storagev1alpha1.Snapshot,
	content *storagev1alpha1.SnapshotContent,
) ([]vcpkg.Target, error) {
	namespace, err := snapshotNamespaceForOwnedTargets(snap, content)
	if err != nil {
		return nil, err
	}
	if content == nil {
		return nil, nil
	}
	covered, err := CollectSubtreeCoveredPVCUIDs(ctx, c, namespace, content)
	if err != nil {
		return nil, err
	}
	candidates, err := ListNamespacePVCTargets(ctx, c, namespace)
	if err != nil {
		return nil, err
	}
	out := residualPVCTargets(candidates, covered)
	sort.Slice(out, func(i, j int) bool {
		return out[i].UID < out[j].UID
	})
	return out, nil
}

// ListOwnedPVCTargetsForSnapshotContent is the domain/demo entry point when only namespace + content are known.
func ListOwnedPVCTargetsForSnapshotContent(
	ctx context.Context,
	c client.Reader,
	namespace string,
	content *storagev1alpha1.SnapshotContent,
) ([]vcpkg.Target, error) {
	snap := &storagev1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace},
	}
	return ListOwnedPVCTargetsForLogicalContent(ctx, c, snap, content)
}

func snapshotNamespaceForOwnedTargets(snap *storagev1alpha1.Snapshot, content *storagev1alpha1.SnapshotContent) (string, error) {
	if snap != nil && snap.Namespace != "" {
		return snap.Namespace, nil
	}
	return "", fmt.Errorf("namespace is required to resolve owned PVC targets")
}
