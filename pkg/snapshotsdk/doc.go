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

// Package snapshotsdk is the capture-side protocol facade for domain snapshot controllers.
//
// A domain snapshot controller (for example DemoVirtualDiskSnapshot / DemoVirtualMachineSnapshot)
// owns three domain decisions and nothing more: what its source is, what child snapshots it implies,
// and which PVC it captures as its data. Everything else — talking to ManifestCaptureRequest, the
// storage-foundation VolumeCaptureRequest, owner references, optimistic-locked status patches, and the
// derived planning-barrier condition — is Kubernetes transport that this SDK hides behind a small set of
// intent verbs.
//
// # Model
//
// The SDK models one snapshot as a manifest capture, a single logical data capture, and a set of child
// snapshots. The domain expresses intent; the SDK makes the cluster match it, idempotently and
// crash/restart-safely.
//
// SDK v1 is delete-free and follows a publish-gated immutability model. EnsureChildren creates/adopts and
// publishes the desired child refs but never deletes children; EnsureVolumeCapture and EnsureManifestCapture
// likewise create/reuse and publish, never delete. The planning barrier (ChildrenSnapshotReady=True, written
// by MarkPlanningReady) is the final commit point of the planning phase, but each individual planning
// artifact becomes immutable the moment it is published — even before the barrier. This yields three states
// for every Ensure* method:
//
//   - State 1 — nothing published, barrier not committed: the SDK converges freely (create/reuse).
//   - State 2 — published, barrier not committed: a desired artifact that diverges from the published one is
//     terminal drift (ErrTopologyDrift / ErrManifestDrift, or a fail-closed VCR error), so a restart with
//     non-deterministic discovery cannot silently rewrite already-published planning intent.
//   - State 3 — barrier committed: the SDK is inert — every Ensure* returns nil, creating, reusing, and
//     validating nothing. Ownership has passed to the core controller; a post-commit divergence is an
//     invalid state the SDK neither repairs nor reports.
//
// Suppression after the barrier is driven solely by the durable ChildrenSnapshotReady condition: the SDK
// consults no execution-phase signal and never waits on the core controller. This keeps the SDK a pure
// publication layer with no diff-based mutation — the dangerous case at restart, where discovery may be
// incomplete or transitional. Any detached leftover is reclaimed by ownerRef garbage collection (the parent
// owns each child) or a future cleanup component, not by the SDK.
//
// # Lifecycle (capture-only, v1)
//
// A typical domain Reconcile resolves its source, then drives child snapshot planning, data capture, and
// manifest capture, and closes with a planning barrier:
//
//	if !valid { return sdk.MarkNotReady(ctx, t, NotReadyStatus{Reason: "InvalidSourceRef"}) }
//	if err := sdk.EnsureChildren(ctx, t, children); err != nil { return sdk.MarkPlanningFailed(...) }
//	if err := sdk.EnsureVolumeCapture(ctx, t, VolumeCaptureSpec{DataRef: dataRef}); err != nil { ... }
//	if err := sdk.EnsureManifestCapture(ctx, t, ManifestCaptureSpec{Targets: manifestTargets}); err != nil { ... }
//	return sdk.MarkPlanningReady(ctx, t, "planning complete")
//
// # Restart-safe recipe
//
// Every Ensure* method is a restart-safe recipe: it reads durable cluster/status state (the published
// names/refs and the barrier condition), reconciles the cluster toward the desired set, and publishes the
// resulting names/refs into the snapshot status. Re-running after a crash converges to the same result and
// never duplicates or strands child resources; once the barrier is committed it is inert (see Model).
//
// # Boundaries
//
// The SDK depends only on the shared api module and Kubernetes client libraries. It never imports a
// domain package and never references domain kinds or field names; the SnapshotAdapter is the single
// seam that maps a concrete domain snapshot to the generic protocol.
//
// The capture facade is a typed, semantic API and never exposes unstructured objects. The one sanctioned
// unstructured boundary is the restore extension point (subpackage transform), which must operate over
// arbitrary captured manifests whose Go types are unknown at compile time. See that package's doc.
package snapshotsdk
