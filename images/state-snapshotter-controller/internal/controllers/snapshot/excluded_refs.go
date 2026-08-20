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

package snapshot

import (
	"sort"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

func sortExcludedObjectRefs(refs []storagev1alpha1.ExcludedObjectRef) {
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].APIVersion != refs[j].APIVersion {
			return refs[i].APIVersion < refs[j].APIVersion
		}
		if refs[i].Kind != refs[j].Kind {
			return refs[i].Kind < refs[j].Kind
		}
		return refs[i].Name < refs[j].Name
	})
}

// excludedObjectRefsEqualIgnoreOrder reports set equality regardless of order.
func excludedObjectRefsEqualIgnoreOrder(a, b []storagev1alpha1.ExcludedObjectRef) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[storagev1alpha1.ExcludedObjectRef]int, len(a))
	for _, ref := range a {
		seen[ref]++
	}
	for _, ref := range b {
		seen[ref]--
		if seen[ref] < 0 {
			return false
		}
	}
	for _, c := range seen {
		if c != 0 {
			return false
		}
	}
	return true
}
