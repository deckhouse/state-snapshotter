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

package namespacemanifest

import (
	corev1 "k8s.io/api/core/v1"

	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
)

// ManifestTargetsFromVolumeTargets converts owned PVC volume targets to explicit MCR manifest targets.
func ManifestTargetsFromVolumeTargets(targets []vcpkg.Target) []ManifestTarget {
	out := make([]ManifestTarget, 0, len(targets))
	for _, t := range targets {
		if t.Kind != "PersistentVolumeClaim" {
			continue
		}
		apiVersion := t.APIVersion
		if apiVersion == "" {
			apiVersion = corev1.SchemeGroupVersion.String()
		}
		out = append(out, ManifestTarget{
			APIVersion: apiVersion,
			Kind:       t.Kind,
			Name:       t.Name,
		})
	}
	sortManifestTargets(out)
	return out
}
