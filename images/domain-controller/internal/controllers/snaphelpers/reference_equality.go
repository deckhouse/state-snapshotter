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

package snaphelpers

import (
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// OwnerReferencesEqual compares ownerReference slices by identity (apiVersion/kind/name/uid) and
// controller flag, order-sensitively.
func OwnerReferencesEqual(left, right []metav1.OwnerReference) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i].APIVersion != right[i].APIVersion ||
			left[i].Kind != right[i].Kind ||
			left[i].Name != right[i].Name ||
			left[i].UID != right[i].UID {
			return false
		}
		leftController := left[i].Controller != nil && *left[i].Controller
		rightController := right[i].Controller != nil && *right[i].Controller
		if leftController != rightController {
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
