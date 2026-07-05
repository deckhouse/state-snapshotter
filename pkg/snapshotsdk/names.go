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

import apinames "github.com/deckhouse/state-snapshotter/api/names"

// ChildSnapshotName re-exports api/names.ChildSnapshotName so domain controllers name their sub-children
// with the same UID scheme the core uses for root-owned children — one definition, zero duplicates. The
// name is nss-snap-<h8(parentSnapshotUID)>-<h16(sourceUID)>: pass the domain snapshot's own UID as the
// parent and the captured source object's UID as the source. Connectivity is carried by refs/ownerRefs,
// so the name is opaque; never reverse-derive it.
var ChildSnapshotName = apinames.ChildSnapshotName
