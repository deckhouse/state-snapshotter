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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
)

// IsResidualRootPVCCaptureScope is true for namespace root capture (subtree residual discovery).
// Domain/demo nodes use listDomainNodeOwnedPVCTargets instead of listing all namespace PVCs.
func IsResidualRootPVCCaptureScope(snap *storagev1alpha1.Snapshot, content *storagev1alpha1.SnapshotContent) bool {
	if content != nil && len(content.Status.ChildrenSnapshotContentRefs) > 0 {
		return true
	}
	if snap != nil && len(snap.Status.ChildrenSnapshotRefs) > 0 {
		return true
	}
	// Namespace-local root without a child graph (N2a): residual PVC discovery applies.
	return snap != nil && snap.Name != ""
}

func listResidualRootOwnedPVCTargets(
	ctx context.Context,
	c client.Reader,
	namespace string,
	snap *storagev1alpha1.Snapshot,
	content *storagev1alpha1.SnapshotContent,
) ([]vcpkg.Target, error) {
	covered, err := CollectSubtreeCoveredPVCUIDs(ctx, c, namespace, content)
	if err != nil {
		return nil, err
	}
	// Resolve the user-provided resourceSelector once (nil/Everything = capture all). It is applied here so
	// excluded PVCs are dropped consistently from BOTH the volume-data leg and the root PVC manifest exclude
	// set (both consumers go through this lister via ListOwnedPVCTargetsForLogicalContent). This mirrors the
	// manifest leg, so a PVC is never half-captured (volume node without manifest, or vice versa).
	selector, err := snap.ResolveResourceSelector()
	if err != nil {
		return nil, fmt.Errorf("resolve spec.resourceSelector: %w", err)
	}
	candidates, labelsByUID, err := listNamespacePVCTargetsWithLabels(ctx, c, namespace)
	if err != nil {
		return nil, err
	}
	candidates = filterPVCTargetsBySelector(selector, candidates, labelsByUID)
	return residualPVCTargets(candidates, covered), nil
}

// filterPVCTargetsBySelector keeps only PVC targets whose labels (resolved by UID from the same List that
// produced the candidates) match selector. A nil or Everything selector returns the candidates unchanged.
func filterPVCTargetsBySelector(selector labels.Selector, candidates []vcpkg.Target, labelsByUID map[string]labels.Set) []vcpkg.Target {
	if selector == nil || selector.Empty() || len(candidates) == 0 {
		return candidates
	}
	out := make([]vcpkg.Target, 0, len(candidates))
	for _, t := range candidates {
		if selector.Matches(labelsByUID[t.UID]) {
			out = append(out, t)
		}
	}
	return out
}

// listDomainNodeOwnedPVCTargets returns PVC volume targets explicitly owned by this logical node:
// published dataRefs on the same SnapshotContent and pending VCR spec.targets[] for that content.
// It does not list all namespace PVCs (domain scope is not root residual).
func listDomainNodeOwnedPVCTargets(
	ctx context.Context,
	c client.Reader,
	namespace string,
	content *storagev1alpha1.SnapshotContent,
) ([]vcpkg.Target, error) {
	if content == nil {
		return nil, nil
	}
	byUID := make(map[string]vcpkg.Target)

	fromRefs, err := volumeTargetsFromContentDataRefs(ctx, c, namespace, content)
	if err != nil {
		return nil, err
	}
	for _, t := range fromRefs {
		byUID[t.UID] = t
	}
	fromVCR, err := pendingVolumeCaptureTargetsForContent(ctx, c, namespace, content.UID)
	if err != nil {
		return nil, err
	}
	for _, t := range fromVCR {
		byUID[t.UID] = t
	}
	out := make([]vcpkg.Target, 0, len(byUID))
	for _, t := range byUID {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].UID < out[j].UID
	})
	return out, nil
}

func volumeTargetsFromContentDataRefs(
	ctx context.Context,
	c client.Reader,
	namespace string,
	content *storagev1alpha1.SnapshotContent,
) ([]vcpkg.Target, error) {
	uids, err := pvcUIDsFromSnapshotContentDataRefs(content)
	if err != nil {
		return nil, err
	}
	out := make([]vcpkg.Target, 0, len(uids))
	for _, uid := range uids {
		t, err := volumeTargetForPVCUID(ctx, c, namespace, types.UID(uid))
		if err != nil {
			return nil, err
		}
		if t != nil {
			out = append(out, *t)
		}
	}
	return out, nil
}

func volumeTargetForPVCUID(ctx context.Context, c client.Reader, namespace string, uid types.UID) (*vcpkg.Target, error) {
	if uid == "" {
		return nil, nil
	}
	list := &corev1.PersistentVolumeClaimList{}
	if err := c.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list PVCs in namespace %s: %w", namespace, err)
	}
	for i := range list.Items {
		pvc := &list.Items[i]
		if pvc.UID != uid {
			continue
		}
		return &vcpkg.Target{
			UID:        string(pvc.UID),
			APIVersion: corev1.SchemeGroupVersion.String(),
			Kind:       "PersistentVolumeClaim",
			Name:       pvc.Name,
			Namespace:  pvc.Namespace,
		}, nil
	}
	return nil, nil
}

func pendingVolumeCaptureTargetsForContent(
	ctx context.Context,
	c client.Reader,
	namespace string,
	contentUID types.UID,
) ([]vcpkg.Target, error) {
	uids, err := pvcUIDsFromPendingVCR(ctx, c, namespace, contentUID)
	if err != nil {
		return nil, err
	}
	out := make([]vcpkg.Target, 0, len(uids))
	for _, uid := range uids {
		t, err := volumeTargetForPVCUID(ctx, c, namespace, types.UID(uid))
		if err != nil {
			return nil, err
		}
		if t != nil {
			out = append(out, *t)
		}
	}
	return out, nil
}
