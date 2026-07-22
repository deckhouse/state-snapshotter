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

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ExcludeLabelKey is the absolute snapshot veto label. Any namespaced object carrying this label key
// (the value is ignored, matching Velero's backup.velero.io/exclude-from-backup convention) is excluded
// from EVERY snapshot, unconditionally: it is honored independently of spec.resourceSelector and at every
// level of the snapshot tree. Recursion is intrinsic and free — the check is local at each enumeration
// point: a labeled parent's subtree is never expanded, and a labeled child drops out alone.
//
// This is the single exported source of truth for the veto key, reused by the core, the SDK, and domain
// controllers; there must be no hardcoded string duplicates elsewhere.
const ExcludeLabelKey = APIGroup + "/exclude"

// LabelDeleteProtected is the canonical authoritative protection state for the unified snapshot tree
// (delete-protection-contract.md). It is NOT a diagnostic marker: it is the single source of truth the
// delete-guard admission consults. Its presence with value LabelDeleteProtectedValue means the object is
// an internal node of a unified snapshot and MUST NOT be deleted by a direct user DELETE — legal removal
// happens only through root Snapshot teardown / controller GC / reclaim (exempt actors) or an explicit
// break-glass annotation. Properties: authoritative (admission reads only this), immutable for users
// (protected against UPDATE removal/change), and written exclusively by the authoritative write-path that
// introduces the node into the tree (steady state) or, once, by the versioned migration backfill for
// legacy objects. The root Snapshot deliberately does NOT carry it. Delete protection is a property of the
// snapshot protocol (tree correctness), not of an object's "importance".
//
// This is the single exported source of truth for the key, reused by the core, the SDK, domain
// controllers and storage-foundation (VS/VSC write-path); there must be no hardcoded string duplicates.
const LabelDeleteProtected = APIGroup + "/delete-protected"

// LabelDeleteProtectedValue is the only meaningful value of LabelDeleteProtected. Protection is keyed on
// the exact key=value pair; any other value is treated by the guard as "not protected".
const LabelDeleteProtectedValue = "true"

// StampDeleteProtected sets the canonical delete-protected label on obj's labels (allocating the map if
// nil). Write-paths MUST call it while BUILDING the object, before Create, so the object appears in the
// API already protected — there is no guaranteed ordering between Create and a follow-up Patch, so a
// post-create patch would leave a short unprotected window. It is idempotent.
func StampDeleteProtected(obj metav1.Object) {
	l := obj.GetLabels()
	if l == nil {
		l = map[string]string{}
	}
	l[LabelDeleteProtected] = LabelDeleteProtectedValue
	obj.SetLabels(l)
}

// IsDeleteProtected reports whether obj carries the exact delete-protected key=value pair. This is the
// only predicate the delete-guard admission and the backfill inventory rely on.
func IsDeleteProtected(obj metav1.Object) bool {
	return obj.GetLabels()[LabelDeleteProtected] == LabelDeleteProtectedValue
}
