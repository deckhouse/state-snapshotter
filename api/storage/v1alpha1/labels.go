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

// ExcludeLabelKey is the absolute snapshot veto label. Any namespaced object carrying this label key
// (the value is ignored, matching Velero's backup.velero.io/exclude-from-backup convention) is excluded
// from EVERY snapshot, unconditionally: it is honored independently of spec.resourceSelector and at every
// level of the snapshot tree. Recursion is intrinsic and free — the check is local at each enumeration
// point: a labeled parent's subtree is never expanded, and a labeled child drops out alone.
//
// This is the single exported source of truth for the veto key, reused by the core, the SDK, and domain
// controllers; there must be no hardcoded string duplicates elsewhere.
const ExcludeLabelKey = APIGroup + "/exclude"
