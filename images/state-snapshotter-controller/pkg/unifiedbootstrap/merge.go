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

package unifiedbootstrap

import "sort"

// MergeBootstrapAndCSDPairs combines bootstrap defaults with CSD-derived pairs.
// For the same Snapshot GVK key, the CSD entry wins (overrides bootstrap) so the content side
// matches the CSD mapping. Order is deterministic (sorted by snapshot GVK string).
func MergeBootstrapAndCSDPairs(bootstrap, fromCSD []UnifiedGVKPair) []UnifiedGVKPair {
	bySnap := make(map[string]UnifiedGVKPair)
	order := make([]string, 0)
	add := func(p UnifiedGVKPair) {
		k := p.Snapshot.String()
		if _, ok := bySnap[k]; !ok {
			order = append(order, k)
		}
		bySnap[k] = p
	}
	for _, p := range bootstrap {
		add(p)
	}
	for _, p := range fromCSD {
		add(p)
	}
	sort.Strings(order)
	out := make([]UnifiedGVKPair, 0, len(order))
	for _, k := range order {
		out = append(out, bySnap[k])
	}
	return out
}
