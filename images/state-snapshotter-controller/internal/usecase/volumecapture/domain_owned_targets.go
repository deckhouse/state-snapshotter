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
//
// content-free: residual scope is decided from the Snapshot itself (its child graph / identity), not
// the bound SnapshotContent, so it holds before the root content is bound. The bound-content argument is
// retained for signature stability and is intentionally unused.
func IsResidualRootPVCCaptureScope(snap *storagev1alpha1.Snapshot, _ *storagev1alpha1.SnapshotContent) bool {
	if snap == nil {
		return false
	}
	if len(snap.Status.ChildrenSnapshotRefs) > 0 {
		return true
	}
	// Namespace-local root without a child graph (N2a): residual PVC discovery applies.
	return snap.Name != ""
}

func listResidualRootOwnedPVCTargets(
	ctx context.Context,
	c client.Reader,
	namespace string,
	snap *storagev1alpha1.Snapshot,
	_ *storagev1alpha1.SnapshotContent,
	dataBearing DataBearingKindFunc,
) ([]vcpkg.Target, error) {
	targets, _, err := residualRootPVCTargetsAndExcluded(ctx, c, namespace, snap, dataBearing)
	return targets, err
}

// ListResidualRootPVCTargets is the residual root PVC leg at planning time: the uncovered PVCs of the root
// Snapshot's namespace the orphan wave captures (targets, sorted by UID), and the ones it leaves out
// because they carry the exclude label (excluded, sorted by name), as the ExcludedObjectRefs the root
// records before barrier 1 freezes its excluded membership.
//
// Only a PVC the leg would otherwise have captured is recorded: a labelled PVC a descendant subtree
// already covers is not excluded from the snapshot, so it is in neither list.
func ListResidualRootPVCTargets(
	ctx context.Context,
	c client.Reader,
	snap *storagev1alpha1.Snapshot,
	dataBearing DataBearingKindFunc,
) ([]vcpkg.Target, []storagev1alpha1.ExcludedObjectRef, error) {
	namespace, err := snapshotNamespaceForOwnedTargets(snap, nil)
	if err != nil {
		return nil, nil, err
	}
	targets, vetoed, err := residualRootPVCTargetsAndExcluded(ctx, c, namespace, snap, dataBearing)
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(targets, func(i, j int) bool {
		return targets[i].UID < targets[j].UID
	})
	excluded := make([]storagev1alpha1.ExcludedObjectRef, 0, len(vetoed))
	for _, t := range vetoed {
		excluded = append(excluded, storagev1alpha1.ExcludedObjectRef{APIVersion: t.APIVersion, Kind: t.Kind, Name: t.Name})
	}
	sort.Slice(excluded, func(i, j int) bool {
		return excluded[i].Name < excluded[j].Name
	})
	return targets, excluded, nil
}

// residualRootPVCTargetsAndExcluded lists the namespace PVCs once and splits the uncovered ones by the
// exclude label: targets are captured, vetoed are left out.
func residualRootPVCTargetsAndExcluded(
	ctx context.Context,
	c client.Reader,
	namespace string,
	snap *storagev1alpha1.Snapshot,
	dataBearing DataBearingKindFunc,
) (targets, vetoed []vcpkg.Target, err error) {
	// content-free coverage: derive the subtree-covered PVC UID set from the Snapshot child graph
	// (status.childrenSnapshotRefs + each descendant's mirrored status.data), NOT the bound SnapshotContent
	// tree — so residual/orphan discovery is computable before the root content is bound ("late Planned").
	covered, err := CollectSubtreeCoveredPVCUIDsFromSnapshot(ctx, c, snap, dataBearing)
	if err != nil {
		return nil, nil, err
	}
	// The exclude veto is applied here so vetoed PVCs are dropped consistently from BOTH the volume-data leg
	// and the root PVC manifest exclude set (both consumers go through this lister via
	// ListOwnedPVCTargetsForLogicalContent). This mirrors the manifest leg, so a PVC is never half-captured
	// (volume node without manifest, or vice versa). The label is checked directly, not through a selector,
	// so the dropped PVCs are returned for the excludedRefs record.
	candidates, labelsByUID, err := listNamespacePVCTargetsWithLabels(ctx, c, namespace)
	if err != nil {
		return nil, nil, err
	}
	kept, labelled := splitPVCTargetsByExcludeLabel(candidates, labelsByUID)
	return residualPVCTargets(kept, covered), residualPVCTargets(labelled, covered), nil
}

// splitPVCTargetsByExcludeLabel splits PVC targets by the exclude label (value ignored), resolving the
// labels by UID from the same List that produced the candidates.
func splitPVCTargetsByExcludeLabel(candidates []vcpkg.Target, labelsByUID map[string]labels.Set) (kept, vetoed []vcpkg.Target) {
	for _, t := range candidates {
		if _, ok := labelsByUID[t.UID][storagev1alpha1.ExcludeLabelKey]; ok {
			vetoed = append(vetoed, t)
			continue
		}
		kept = append(kept, t)
	}
	return kept, vetoed
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
