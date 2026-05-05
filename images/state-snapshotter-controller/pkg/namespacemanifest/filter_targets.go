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

package namespacemanifest

import "fmt"

// ManifestTargetDedupKey matches aggregated manifest identity (apiVersion|kind|namespace|name).
// Snapshot root capture is namespaced-only; snapshotNamespace is the target namespace.
func ManifestTargetDedupKey(snapshotNamespace string, t ManifestTarget) string {
	ns := snapshotNamespace
	if ns == "" {
		ns = "_cluster"
	}
	return fmt.Sprintf("%s|%s|%s|%s", t.APIVersion, t.Kind, ns, t.Name)
}

// FilterManifestTargets removes entries whose ManifestTargetDedupKey appears in exclude.
// exclude is built only from the current snapshot-run graph (see usecase root capture exclude).
func FilterManifestTargets(targets []ManifestTarget, exclude map[string]struct{}, snapshotNamespace string) []ManifestTarget {
	if len(exclude) == 0 {
		return targets
	}
	out := make([]ManifestTarget, 0, len(targets))
	for _, t := range targets {
		k := ManifestTargetDedupKey(snapshotNamespace, t)
		if _, skip := exclude[k]; skip {
			continue
		}
		out = append(out, t)
	}
	sortManifestTargets(out)
	return out
}
