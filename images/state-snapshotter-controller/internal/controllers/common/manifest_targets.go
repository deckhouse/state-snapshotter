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

package common

import (
	"sort"

	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
)

// ManifestTargetsEqual compares capture plans in canonical order (APIVersion, Kind, Name).
func ManifestTargetsEqual(a, b []ssv1alpha1.ManifestTarget) bool {
	if len(a) != len(b) {
		return false
	}
	aa := append([]ssv1alpha1.ManifestTarget(nil), a...)
	bb := append([]ssv1alpha1.ManifestTarget(nil), b...)
	sort.Slice(aa, func(i, j int) bool {
		if aa[i].APIVersion != aa[j].APIVersion {
			return aa[i].APIVersion < aa[j].APIVersion
		}
		if aa[i].Kind != aa[j].Kind {
			return aa[i].Kind < aa[j].Kind
		}
		return aa[i].Name < aa[j].Name
	})
	sort.Slice(bb, func(i, j int) bool {
		if bb[i].APIVersion != bb[j].APIVersion {
			return bb[i].APIVersion < bb[j].APIVersion
		}
		if bb[i].Kind != bb[j].Kind {
			return bb[i].Kind < bb[j].Kind
		}
		return bb[i].Name < bb[j].Name
	})
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}
	return true
}
