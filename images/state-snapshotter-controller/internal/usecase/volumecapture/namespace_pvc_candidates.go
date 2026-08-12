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

	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
)

// ListNamespacePVCTargets lists all PVCs in namespace as volume capture targets (residual candidate discovery only).
func ListNamespacePVCTargets(ctx context.Context, c client.Reader, namespace string) ([]vcpkg.Target, error) {
	targets, _, err := listNamespacePVCTargetsWithLabels(ctx, c, namespace)
	return targets, err
}

// listNamespacePVCTargetsWithLabels lists all namespace PVCs once, returning both the volume capture targets
// and their labels keyed by UID. The labels feed resourceSelector filtering from the same List call, so the
// residual leg does not issue a second namespace-wide List (avoiding a TOCTOU between two list snapshots).
func listNamespacePVCTargetsWithLabels(ctx context.Context, c client.Reader, namespace string) ([]vcpkg.Target, map[string]labels.Set, error) {
	if namespace == "" {
		return nil, nil, fmt.Errorf("namespace is required to list PVC candidates")
	}
	list := &corev1.PersistentVolumeClaimList{}
	if err := c.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return nil, nil, fmt.Errorf("list PVCs in namespace %s: %w", namespace, err)
	}
	out := make([]vcpkg.Target, 0, len(list.Items))
	labelsByUID := make(map[string]labels.Set, len(list.Items))
	for i := range list.Items {
		pvc := &list.Items[i]
		if pvc.UID == "" {
			return nil, nil, fmt.Errorf("PVC %s/%s has empty uid", namespace, pvc.Name)
		}
		out = append(out, vcpkg.Target{
			UID:        string(pvc.UID),
			APIVersion: corev1.SchemeGroupVersion.String(),
			Kind:       "PersistentVolumeClaim",
			Name:       pvc.Name,
			Namespace:  pvc.Namespace,
		})
		labelsByUID[string(pvc.UID)] = labels.Set(pvc.Labels)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UID != out[j].UID {
			return out[i].UID < out[j].UID
		}
		return out[i].Name < out[j].Name
	})
	return out, labelsByUID, nil
}

// residualPVCTargets returns namespace PVC candidates minus subtree-covered UIDs.
func residualPVCTargets(candidates []vcpkg.Target, covered map[types.UID]struct{}) []vcpkg.Target {
	if len(candidates) == 0 {
		return nil
	}
	out := make([]vcpkg.Target, 0, len(candidates))
	for _, t := range candidates {
		if _, skip := covered[types.UID(t.UID)]; skip {
			continue
		}
		out = append(out, t)
	}
	return out
}
