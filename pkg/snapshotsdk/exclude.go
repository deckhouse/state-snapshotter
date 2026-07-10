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

package snapshotsdk

import (
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// IsExcluded reports whether an object's labels carry the absolute exclude veto (ExcludeLabelKey). The
// veto is key-presence only — the value is ignored, matching Velero's backup.velero.io/exclude-from-backup
// convention (both "true" and "" exclude).
func IsExcluded(labels map[string]string) bool {
	if labels == nil {
		return false
	}
	_, ok := labels[ExcludeLabelKey]
	return ok
}

// PartitionExcluded splits candidate source objects into kept (no veto) and excluded (veto present). A
// domain enumerator MUST call it over the source objects it is about to turn into children: build children
// only from kept, and record excluded into DomainCaptureState.ExcludedRefs (published via EnsureChildren).
// The SDK cannot filter inside EnsureChildren because it sees only the built child-CR specs, not the source
// objects' labels — so the veto is applied here, in the domain, at the point of enumeration. nil entries
// are skipped. Order is preserved for both partitions.
func PartitionExcluded(objs []client.Object) (kept, excluded []client.Object) {
	for _, o := range objs {
		if o == nil {
			continue
		}
		if IsExcluded(o.GetLabels()) {
			excluded = append(excluded, o)
		} else {
			kept = append(kept, o)
		}
	}
	return kept, excluded
}

// sortExcludedRefs orders excluded refs deterministically by (apiVersion, kind, name), matching
// children.SortRefs so published lists are stable and change detection is order-insensitive.
func sortExcludedRefs(refs []ExcludedObjectRef) {
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

// normalizeExcludedRefs returns a sorted, de-duplicated, NON-NIL copy of refs. Non-nil matters: the domain
// field is written without omitempty, so a nil slice would marshal to JSON null (rejected by the
// non-nullable CRD array); an empty [] is the correct "nothing excluded" wire value.
func normalizeExcludedRefs(refs []ExcludedObjectRef) []ExcludedObjectRef {
	out := make([]ExcludedObjectRef, 0, len(refs))
	seen := make(map[ExcludedObjectRef]struct{}, len(refs))
	for _, r := range refs {
		if _, dup := seen[r]; dup {
			continue
		}
		seen[r] = struct{}{}
		out = append(out, r)
	}
	sortExcludedRefs(out)
	return out
}

// excludedRefsEqualIgnoreOrder reports set equality of two excluded-ref slices (order-insensitive).
func excludedRefsEqualIgnoreOrder(a, b []ExcludedObjectRef) bool {
	if len(a) != len(b) {
		return false
	}
	aa := normalizeExcludedRefs(a)
	bb := normalizeExcludedRefs(b)
	if len(aa) != len(bb) {
		return false
	}
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}
	return true
}
