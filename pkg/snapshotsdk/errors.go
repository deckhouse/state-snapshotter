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

import "errors"

// ErrTopologyDrift is returned by EnsureChildren when the desired child set differs from the set already
// published in status.childrenSnapshotRefs. The published set is the authoritative, immutable snapshot
// topology, so a drifted desired set is a terminal, fail-closed condition: EnsureChildren creates nothing,
// deletes nothing, and leaves the published refs untouched. The domain controller should surface it as a
// planning failure (MarkPlanningFailed with api/storage/v1alpha1.ReasonTopologyDrift). Callers match it
// with errors.Is(err, snapshotsdk.ErrTopologyDrift).
var ErrTopologyDrift = errors.New("snapshot child topology changed after publication")

// ErrManifestDrift is returned by EnsureManifestCapture when the desired manifest target set differs from
// the set of an already-existing ManifestCaptureRequest. Like the child snapshot topology and the data
// capture, manifest capture is fail-closed: the SDK does not silently accept a stale request nor self-heal
// it by update/patch/delete. EnsureManifestCapture leaves the existing request and the status untouched. The
// domain controller should surface it as a planning failure (MarkPlanningFailed with
// api/storage/v1alpha1.ReasonManifestDrift). Callers match it with errors.Is(err, snapshotsdk.ErrManifestDrift).
var ErrManifestDrift = errors.New("snapshot manifest capture targets changed after publication")

// ErrEmptyManifest is returned by EnsureManifestCapture when the FINAL manifest target set — after the SDK
// has augmented the domain-provided targets with any owned-PVC target derived from the data capture — is
// empty. Every snapshot node captures at least one manifest target (the captured resource itself, plus any
// extra objects the domain chooses), so an empty final set is an SDK invariant violation: it signals a
// controller planning bug (the domain passed no manifest targets and there was no owned-PVC to rescue the
// set), not a transient condition. EnsureManifestCapture is fail-closed here — it returns before any cluster
// mutation (no MCR Get/Create, no status patch). The SDK deliberately does NOT inject the source object on
// the domain's behalf; supplying at least one manifest target is the domain's responsibility. Callers match
// it with errors.Is(err, snapshotsdk.ErrEmptyManifest).
var ErrEmptyManifest = errors.New("manifest capture has no targets")
