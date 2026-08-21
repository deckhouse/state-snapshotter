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

package restore

import (
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// cloneDataBindings copies a content's dataRefs onto the resolved node, so the compiled tree does not
// alias the slice returned by the content wrapper. Matching a captured PVC to one of these bindings is
// part of the shared restore compilation (transform.FindDataBindingForPVC), not of this resolver.
func cloneDataBindings(refs []snapshot.DataBindingRef) []snapshot.DataBindingRef {
	if len(refs) == 0 {
		return nil
	}
	out := make([]snapshot.DataBindingRef, len(refs))
	copy(out, refs)
	return out
}
