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
// and which PVCs make up its data leg. Everything else — talking to ManifestCaptureRequest, the
// storage-foundation VolumeCaptureRequest, owner references, optimistic-locked status patches, and the
// lifecycle phase (status.captureState.domainSpecificController.phase) — is Kubernetes transport that
// this SDK hides behind a small set of intent verbs. The SDK never writes the Ready condition: the core
// always derives Ready (on every snapshot object) and the domain reads it back as its failure channel
// via CoreCaptureOutcome.
//
// # Model
//
// The SDK models one snapshot as a manifest leg, a single logical data leg, and a set of child
// snapshots. The domain expresses intent; the SDK makes the cluster match it, idempotently and
// crash/restart-safely.
//
// SDK v1 is delete-free: EnsureChildren creates/adopts and publishes the desired child refs but never
// deletes children. A child no longer desired drops out of status.childrenSnapshotRefs and is reclaimed
// by ownerRef garbage collection (the parent owns each child) or a future cleanup component, not by the
// SDK. This keeps the contract a pure publication layer with no risk of deleting a foreign object.
//
// # Lifecycle (capture-only, v1)
//
// A typical domain Reconcile resolves its source, then drives the three planning legs, publishes the
// source, marks barrier 1 (Planned), and later switches on CoreCaptureOutcome to confirm consistency
// (barrier 2 = Finished) or fail:
//
//	if !valid { return sdk.Reject(ctx, t, FailSpec{Reason: "InvalidSourceRef"}) }
//	if err := sdk.EnsureChildren(ctx, t, children); err != nil { return sdk.Fail(ctx, t, "GraphPlanningFailed", err) }
//	if err := sdk.EnsureVolumeCapture(ctx, t, VolumeCaptureSpec{DataRef: dataRef}); err != nil { ... }
//	if err := sdk.EnsureManifestCapture(ctx, t, ManifestCaptureSpec{...}); err != nil { ... }
//	_ = sdk.PublishSnapshotSource(ctx, t, SnapshotSource{...})
//	if err := sdk.MarkPlanned(ctx, t); err != nil { return err }
//	switch o := CoreCaptureOutcome(t); o.Outcome {
//	case CaptureOutcomeCaptured: return sdk.ConfirmConsistent(ctx, t) // after any consistency action (e.g. fs unfreeze)
//	case CaptureOutcomeFailed:   return sdk.Fail(ctx, t, Reason(o.Reason), errors.New(o.Message))
//	default: // CaptureOutcomeCapturing: wait
//	}
//
// # Restart-safe recipe
//
// Every Ensure* method is a restart-safe recipe: it reads durable cluster/status state (refreshing the
// snapshot via the API reader to avoid TOCTOU on the captured markers), reconciles the cluster toward
// the desired set, and publishes the resulting names/refs into the snapshot status. Re-running after a
// crash converges to the same result and never duplicates or strands child resources.
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
