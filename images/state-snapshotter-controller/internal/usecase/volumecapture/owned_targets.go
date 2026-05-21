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
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
)

// AnnotationStubVolumeCapturePVCs lists PVC names (same namespace as Snapshot) for PR-4 vertical slice until PR-5 ownership resolver exists.
const AnnotationStubVolumeCapturePVCs = "state-snapshotter.deckhouse.io/volume-capture-stub-pvcs"

// ListOwnedPVCTargetsForLogicalContent returns PVC targets owned by this logical SnapshotContent node.
// Empty slice means no volume leg for this capture (manifest-only).
func ListOwnedPVCTargetsForLogicalContent(
	ctx context.Context,
	c client.Reader,
	snap *storagev1alpha1.Snapshot,
	_ *storagev1alpha1.SnapshotContent,
) ([]vcpkg.Target, error) {
	if snap == nil {
		return nil, nil
	}
	raw := strings.TrimSpace(snap.Annotations[AnnotationStubVolumeCapturePVCs])
	if raw == "" {
		return nil, nil
	}
	names := splitCSV(raw)
	out := make([]vcpkg.Target, 0, len(names))
	for _, name := range names {
		pvc := &corev1.PersistentVolumeClaim{}
		key := types.NamespacedName{Namespace: snap.Namespace, Name: name}
		if err := c.Get(ctx, key, pvc); err != nil {
			return nil, fmt.Errorf("get PVC %s: %w", key, err)
		}
		if pvc.UID == "" {
			return nil, fmt.Errorf("PVC %s has empty uid", key)
		}
		out = append(out, vcpkg.Target{
			UID:        string(pvc.UID),
			APIVersion: corev1.SchemeGroupVersion.String(),
			Kind:       "PersistentVolumeClaim",
			Name:       pvc.Name,
			Namespace:  pvc.Namespace,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].UID < out[j].UID
	})
	return out, nil
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
