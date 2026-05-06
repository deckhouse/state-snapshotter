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

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

func SortSnapshotContentChildRefs(refs []storagev1alpha1.SnapshotContentChildRef) {
	sort.Slice(refs, func(i, j int) bool {
		return refs[i].Name < refs[j].Name
	})
}

func SnapshotContentChildRefsEqualIgnoreOrder(a, b []storagev1alpha1.SnapshotContentChildRef) bool {
	if len(a) != len(b) {
		return false
	}
	aa := append([]storagev1alpha1.SnapshotContentChildRef(nil), a...)
	bb := append([]storagev1alpha1.SnapshotContentChildRef(nil), b...)
	SortSnapshotContentChildRefs(aa)
	SortSnapshotContentChildRefs(bb)
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}
	return true
}

func SortSnapshotChildRefs(refs []storagev1alpha1.SnapshotChildRef) {
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

func SnapshotChildRefsEqualIgnoreOrder(a, b []storagev1alpha1.SnapshotChildRef) bool {
	if len(a) != len(b) {
		return false
	}
	aa := append([]storagev1alpha1.SnapshotChildRef(nil), a...)
	bb := append([]storagev1alpha1.SnapshotChildRef(nil), b...)
	SortSnapshotChildRefs(aa)
	SortSnapshotChildRefs(bb)
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}
	return true
}
