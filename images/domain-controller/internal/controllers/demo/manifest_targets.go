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

package demo

import (
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"

	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	vcpkg "github.com/deckhouse/state-snapshotter/images/domain-controller/pkg/volumecapture"
)

// manifestTargetsFromVolumeTargets converts owned PVC volume targets to explicit MCR manifest targets.
// It works directly on the api ManifestTarget type (no intermediate controller-layer type).
func manifestTargetsFromVolumeTargets(targets []vcpkg.Target) []ssv1alpha1.ManifestTarget {
	out := make([]ssv1alpha1.ManifestTarget, 0, len(targets))
	for _, t := range targets {
		if t.Kind != "PersistentVolumeClaim" {
			continue
		}
		apiVersion := t.APIVersion
		if apiVersion == "" {
			apiVersion = corev1.SchemeGroupVersion.String()
		}
		out = append(out, ssv1alpha1.ManifestTarget{
			APIVersion: apiVersion,
			Kind:       t.Kind,
			Name:       t.Name,
		})
	}
	sortManifestTargets(out)
	return out
}

// appendOwnedPVCManifestTargets adds owned PVC targets not already present and not in subtree exclude (E5).
func appendOwnedPVCManifestTargets(
	base []ssv1alpha1.ManifestTarget,
	owned []ssv1alpha1.ManifestTarget,
	exclude map[string]struct{},
	snapshotNamespace string,
) []ssv1alpha1.ManifestTarget {
	if len(owned) == 0 {
		return base
	}
	seen := make(map[string]struct{}, len(base)+len(owned))
	for _, t := range base {
		seen[manifestTargetDedupKey(snapshotNamespace, t)] = struct{}{}
	}
	out := append([]ssv1alpha1.ManifestTarget(nil), base...)
	for _, t := range owned {
		k := manifestTargetDedupKey(snapshotNamespace, t)
		if _, skip := exclude[k]; skip {
			continue
		}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, t)
	}
	sortManifestTargets(out)
	return out
}

func sortManifestTargets(targets []ssv1alpha1.ManifestTarget) {
	sort.Slice(targets, func(i, j int) bool {
		a, b := targets[i], targets[j]
		if a.APIVersion != b.APIVersion {
			return a.APIVersion < b.APIVersion
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Name < b.Name
	})
}

// manifestTargetDedupKey matches aggregated manifest identity (apiVersion|kind|namespace|name).
func manifestTargetDedupKey(snapshotNamespace string, t ssv1alpha1.ManifestTarget) string {
	ns := snapshotNamespace
	if ns == "" {
		ns = "_cluster"
	}
	return fmt.Sprintf("%s|%s|%s|%s", t.APIVersion, t.Kind, ns, t.Name)
}
