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

// Package manifest builds the ManifestCaptureRequest target set from the domain's declared manifest
// targets. The manifest leg is independent of the data leg: the domain declares EVERY object it wants
// captured as YAML (including any PVC whose data it also captures), and the SDK never derives or injects
// targets from the data-leg VolumeCaptureRequest.
package manifest

import (
	"fmt"
	"sort"

	"k8s.io/apimachinery/pkg/types"

	"github.com/deckhouse/state-snapshotter/api/names"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
)

// RequestName returns the deterministic ManifestCaptureRequest name for a snapshot, keyed by its UID
// (unified wave4C scheme, see api/names). The name is derivable from the snapshot alone, so it is stable
// across reconciles and restarts.
func RequestName(snapshotUID types.UID) string {
	return names.ManifestCaptureRequestName(snapshotUID)
}

// Targets normalizes the domain's declared manifest targets: deduplicated by (apiVersion|kind|name) and
// sorted deterministically so the resulting MCR spec is stable across reconciles. It is a pure function of
// its input — it depends on nothing but the declared set (no data-leg / VolumeCaptureRequest read).
func Targets(declared []ssv1alpha1.ManifestTarget) []ssv1alpha1.ManifestTarget {
	seen := make(map[string]struct{}, len(declared))
	out := make([]ssv1alpha1.ManifestTarget, 0, len(declared))
	for _, t := range declared {
		k := dedupKey(t)
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, t)
	}
	sortTargets(out)
	return out
}

func sortTargets(targets []ssv1alpha1.ManifestTarget) {
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

func dedupKey(t ssv1alpha1.ManifestTarget) string {
	return fmt.Sprintf("%s|%s|%s", t.APIVersion, t.Kind, t.Name)
}
