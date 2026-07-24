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

// Package snapshotsdk is the capture-side protocol for domain snapshot controllers.
//
// A domain snapshot controller (for example DemoVirtualDiskSnapshot / DemoVirtualMachineSnapshot)
// owns three domain decisions and nothing more: what its source is, what child snapshots it implies,
// and which PVCs make up its data leg. Everything else — talking to ManifestCaptureRequest, the
// storage-foundation VolumeCaptureRequest, owner references, optimistic-locked status patches, and the
// lifecycle phase (status.captureState.domainSpecificController.phase) — is Kubernetes transport that
// this SDK hides behind planning verbs and DomainCaptureStatus (the single public writer for
// phase/reason/message). The SDK never writes the Ready condition: the core always derives Ready
// (on every snapshot object) and the domain reads it back as its failure channel via CoreCaptureOutcome.
//
// # Model
//
// The SDK models one snapshot as a manifest leg, a single logical data leg, and a set of child
// snapshots. The domain expresses intent; the SDK makes the cluster match it, idempotently and
// crash/restart-safely.
//
// SDK v1 is delete-free and publication is ADDITIVE (union): EnsureChildren creates/adopts the desired
// children and unions their refs into status.childrenSnapshotRefs, never removing a ref. A child no longer
// enumerated by its emitter is therefore NOT dropped from the published set — its ref stays; only the
// leftover child OBJECT is reclaimed, by ownerRef garbage collection (the parent owns each child, so it is
// collected when the parent is deleted) or a future cleanup component, not by the SDK. This keeps the
// contract a pure publication layer with no risk of deleting a foreign object.
//
// The declared set is also FROZEN once the node declares barrier 1: at phase>=Planned (and at the terminal
// Failed) EnsureChildren rejects any attempt to GROW it (or change the excluded set) with
// ErrChildrenSetFrozen — fail-closed, before any child CR is created. The declared membership is the
// snapshot's point-in-time composition and mirrors the immutable SnapshotContent.childrenSnapshotContentRefs;
// the recommended domain reaction is DomainCaptureStatus with phase=Failed and reason GraphPlanningFailed.
//
// # Exclude veto
//
// The label ExcludeLabelKey (state-snapshotter.deckhouse.io/exclude) is an absolute, always-active veto:
// any object carrying it (value ignored) is dropped from every snapshot, at every level of the tree,
// independently of the root's spec.resourceSelector. The core folds the veto into ResolveResourceSelector
// so all core legs honor it with one edit, but a domain enumerator sees only the child specs it builds —
// not the source objects' labels — so it MUST apply the veto itself with PartitionExcluded: build children
// from the kept objects, and hand the excluded refs to EnsureChildren. The SDK publishes those excluded
// refs into status.captureState.domainSpecificController.excludedRefs (the transient INPUT); the core
// aggregates them into the durable SnapshotContent.status.excludedRefs and mirrors that onto the top-level
// status.excludedRefs. The domain never writes the durable aggregate or the top-level mirror.
//
// # Lifecycle (capture-only, v1)
//
// A typical domain Reconcile resolves its source, then drives the three planning legs, publishes the
// source, marks barrier 1 (Planned), and later switches on CoreCaptureOutcome to confirm consistency
// (barrier 2 = Finished). All phase/reason/message writes go through DomainCaptureStatus:
//
//	status := sdk.DomainCaptureStatus(t)
//	if !valid {
//	    return status.Phase(PhaseFailed).Reason("InvalidSourceRef").Message("...").Apply(ctx)
//	}
//	kept, dropped := PartitionExcluded(sourceObjs) // honor the state-snapshotter.deckhouse.io/exclude veto
//	children, excludedRefs := buildFrom(kept), refsOf(dropped)
//	if err := sdk.EnsureChildren(ctx, t, children, excludedRefs); err != nil {
//	    return status.Phase(PhaseFailed).Reason("GraphPlanningFailed").Message(err.Error()).Apply(ctx)
//	}
//	if err := sdk.EnsureVolumeCapture(ctx, t, VolumeCaptureSpec{DataRef: dataRef}); err != nil { ... }
//	if err := sdk.EnsureManifestCapture(ctx, t, ManifestCaptureSpec{...}); err != nil { ... }
//	_ = sdk.PublishSnapshotSource(ctx, t, SnapshotSource{...})
//	if err := status.Phase(PhasePlanned).Apply(ctx); err != nil { return err }
//	switch o := CoreCaptureOutcome(t); o.Outcome {
//	case CaptureOutcomeCaptured:
//	    return status.Phase(PhaseFinished).Apply(ctx) // after any consistency action (e.g. fs unfreeze)
//	case CaptureOutcomeFailed:
//	    return nil // core owns the terminal Ready; domain typically stops
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
