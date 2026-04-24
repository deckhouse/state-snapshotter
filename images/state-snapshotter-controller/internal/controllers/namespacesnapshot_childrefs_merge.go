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

package controllers

import (
	"sort"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

func namespaceSnapshotChildRefsEqual(a, b []storagev1alpha1.NamespaceSnapshotChildRef) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].APIVersion != b[i].APIVersion || a[i].Kind != b[i].Kind ||
			a[i].Name != b[i].Name || a[i].Namespace != b[i].Namespace {
			return false
		}
	}
	return true
}

func namespaceSnapshotChildRefKey(ref storagev1alpha1.NamespaceSnapshotChildRef) string {
	return ref.APIVersion + "\x00" + ref.Kind + "\x00" + ref.Namespace + "\x00" + ref.Name
}

// mergeNamespaceSnapshotChildRefs returns a new slice: all entries from existing, then each upsert overwrites
// or appends by key (apiVersion, kind, namespace, name). Result is sorted for stable status (spec §3.2 / INV-REF-M1).
func mergeNamespaceSnapshotChildRefs(existing, upsert []storagev1alpha1.NamespaceSnapshotChildRef) []storagev1alpha1.NamespaceSnapshotChildRef {
	m := make(map[string]storagev1alpha1.NamespaceSnapshotChildRef, len(existing)+len(upsert))
	order := make([]string, 0, len(existing)+len(upsert))
	add := func(ref storagev1alpha1.NamespaceSnapshotChildRef) {
		k := namespaceSnapshotChildRefKey(ref)
		if _, ok := m[k]; !ok {
			order = append(order, k)
		}
		m[k] = ref
	}
	for i := range existing {
		add(existing[i])
	}
	for i := range upsert {
		add(upsert[i])
	}
	sort.Strings(order)
	out := make([]storagev1alpha1.NamespaceSnapshotChildRef, 0, len(order))
	for _, k := range order {
		out = append(out, m[k])
	}
	return out
}

func namespaceSnapshotChildRefsEqualIgnoreOrder(a, b []storagev1alpha1.NamespaceSnapshotChildRef) bool {
	if len(a) != len(b) {
		return false
	}
	sa := namespaceSnapshotChildRefsSortedCopy(a)
	sb := namespaceSnapshotChildRefsSortedCopy(b)
	return namespaceSnapshotChildRefsEqual(sa, sb)
}

func namespaceSnapshotChildRefsSortedCopy(src []storagev1alpha1.NamespaceSnapshotChildRef) []storagev1alpha1.NamespaceSnapshotChildRef {
	cp := append([]storagev1alpha1.NamespaceSnapshotChildRef(nil), src...)
	sort.Slice(cp, func(i, j int) bool {
		if cp[i].APIVersion != cp[j].APIVersion {
			return cp[i].APIVersion < cp[j].APIVersion
		}
		if cp[i].Kind != cp[j].Kind {
			return cp[i].Kind < cp[j].Kind
		}
		if cp[i].Namespace != cp[j].Namespace {
			return cp[i].Namespace < cp[j].Namespace
		}
		return cp[i].Name < cp[j].Name
	})
	return cp
}

func namespaceSnapshotContentChildRefsEqual(a, b []storagev1alpha1.NamespaceSnapshotContentChildRef) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name {
			return false
		}
	}
	return true
}

// mergeNamespaceSnapshotContentChildRefs merges by child content name (key within parent NamespaceSnapshotContent).
func mergeNamespaceSnapshotContentChildRefs(existing, upsert []storagev1alpha1.NamespaceSnapshotContentChildRef) []storagev1alpha1.NamespaceSnapshotContentChildRef {
	m := make(map[string]storagev1alpha1.NamespaceSnapshotContentChildRef, len(existing)+len(upsert))
	order := make([]string, 0, len(existing)+len(upsert))
	add := func(ref storagev1alpha1.NamespaceSnapshotContentChildRef) {
		k := ref.Name
		if _, ok := m[k]; !ok {
			order = append(order, k)
		}
		m[k] = ref
	}
	for i := range existing {
		add(existing[i])
	}
	for i := range upsert {
		add(upsert[i])
	}
	sort.Strings(order)
	out := make([]storagev1alpha1.NamespaceSnapshotContentChildRef, 0, len(order))
	for _, k := range order {
		out = append(out, m[k])
	}
	return out
}

func namespaceSnapshotContentChildRefsEqualIgnoreOrder(a, b []storagev1alpha1.NamespaceSnapshotContentChildRef) bool {
	if len(a) != len(b) {
		return false
	}
	sa := namespaceSnapshotContentChildRefsSortedCopy(a)
	sb := namespaceSnapshotContentChildRefsSortedCopy(b)
	return namespaceSnapshotContentChildRefsEqual(sa, sb)
}

func namespaceSnapshotContentChildRefsSortedCopy(src []storagev1alpha1.NamespaceSnapshotContentChildRef) []storagev1alpha1.NamespaceSnapshotContentChildRef {
	cp := append([]storagev1alpha1.NamespaceSnapshotContentChildRef(nil), src...)
	sort.Slice(cp, func(i, j int) bool {
		return cp[i].Name < cp[j].Name
	})
	return cp
}

// removeNamespaceSnapshotChildRefsByKeys returns existing refs minus any whose (namespace,name) appears in remove (INV-REF-M2: caller must only pass keys it owns).
func removeNamespaceSnapshotChildRefsByKeys(existing, remove []storagev1alpha1.NamespaceSnapshotChildRef) []storagev1alpha1.NamespaceSnapshotChildRef {
	if len(remove) == 0 {
		return namespaceSnapshotChildRefsSortedCopy(existing)
	}
	rm := make(map[string]struct{}, len(remove))
	for i := range remove {
		rm[namespaceSnapshotChildRefKey(remove[i])] = struct{}{}
	}
	var out []storagev1alpha1.NamespaceSnapshotChildRef
	for i := range existing {
		if _, drop := rm[namespaceSnapshotChildRefKey(existing[i])]; drop {
			continue
		}
		out = append(out, existing[i])
	}
	return namespaceSnapshotChildRefsSortedCopy(out)
}

// removeNamespaceSnapshotContentChildRefsByKeys drops child content refs listed in remove (by Name).
func removeNamespaceSnapshotContentChildRefsByKeys(existing, remove []storagev1alpha1.NamespaceSnapshotContentChildRef) []storagev1alpha1.NamespaceSnapshotContentChildRef {
	if len(remove) == 0 {
		return namespaceSnapshotContentChildRefsSortedCopy(existing)
	}
	rm := make(map[string]struct{}, len(remove))
	for i := range remove {
		rm[remove[i].Name] = struct{}{}
	}
	var out []storagev1alpha1.NamespaceSnapshotContentChildRef
	for i := range existing {
		if _, drop := rm[existing[i].Name]; drop {
			continue
		}
		out = append(out, existing[i])
	}
	return namespaceSnapshotContentChildRefsSortedCopy(out)
}
