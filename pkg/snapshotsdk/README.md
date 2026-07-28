> Language: **English** · [Русский](./README.ru.md)

# Snapshot SDK — domain integration guide (capture)

> **Status: developer-facing usage guide** for teams integrating their domain with the
> snapshot controller through `pkg/snapshotsdk`. This is *how to use it*, not the normative
> contract. The normative source for the domain↔core contract is the SDK ADR
> (`2026-06-29-domain-snapshot-sdk.md`); the godoc in `pkg/snapshotsdk` is normative for the exact
> Go signatures and code-level invariants; this README is **not normative**. The code-quality
> contract is [`CLAUDE.md`](./CLAUDE.md). The reference implementation is the demo controllers in the
> `sds-unified-snapshots-poc` repo (`images/domain-controller/internal/controllers/demo`).
>
> SDK v1 scope is **capture-only** (snapshot planning: child snapshots + data capture +
> manifest capture + lifecycle barriers). Restore is a separate sanctioned boundary
> (`pkg/snapshotsdk/transform`) and is out of scope here.

**In one paragraph:** `snapshotsdk` lets a domain controller **describe snapshot intent** (child
snapshots, an optional data PVC, manifest targets, the captured source) without implementing
snapshot orchestration. The domain decides **what** to snapshot; the SDK decides **how** capture
requests, ownership, status patching, and the restart-safe lifecycle are managed. The SDK never
writes the `Ready` condition — the core controller derives `Ready` on every snapshot, and the
domain reads it back as its failure channel.

## The snapshot lifecycle in one minute

1. A user creates a domain snapshot CR (`MySnapshot`).
2. The domain controller validates the source object (and short-circuits entirely in import mode).
3. The controller decides four things:
   - which **manifest targets** to save;
   - whether to save **PVC data**;
   - which **child snapshots** are needed;
   - which candidate source objects are **excluded** (the exclude veto).
4. The controller hands this to the SDK, which creates the capture requests, publishes the refs
   and the captured source, and stamps the domain lifecycle phase.
5. The controller declares **barrier 1** (`DomainCaptureStatus` → `phase=Planned`).
6. The core controller captures the legs, materializes the `SnapshotContent`, flips the per-leg
   latches, and writes `Ready`.
7. The controller switches on `CoreCaptureOutcome` and, once every leg is captured, declares
   **barrier 2** (`DomainCaptureStatus` → `phase=Finished`) after any consistency action.

```
User
  |
  v
Domain Snapshot CR
  |
  v
Domain Controller
  |-- resolve + publish source
  |-- discover children (+ exclude veto)
  |-- resolve PVC
  |-- choose manifest targets
  |
  v
Snapshot SDK
  |-- EnsureChildren
  |-- EnsureVolumeCapture
  |-- EnsureManifestCapture
  |
  v
Barrier 1: phase = Planned      (DomainCaptureStatus)
  |
  v
Core snapshot controller  --->  captures legs, materializes SnapshotContent,
  |                              flips commonController latches, writes Ready
  v
CoreCaptureOutcome
  |-- Captured  -> DomainCaptureStatus -> phase = Finished   (Barrier 2)
  |-- Failed    -> stop (core owns the terminal Ready)
  |-- Capturing -> wait / requeue
```

That is the whole flow. The rest of this guide covers each step in detail.

## What the snapshot SDK is and why it exists

`pkg/snapshotsdk` is a domain-neutral library that standardizes the **capture phase** of a
snapshot (planning: child snapshots + data capture + manifest capture + lifecycle barriers). The
domain team **describes the intent** of a snapshot ("what we capture"), and the SDK takes over the
orchestration ("how to lay it out in Kubernetes").

Before the SDK, every domain team had to implement all of this itself:

- ownerRefs on capture objects;
- create/adopt of capture requests;
- idempotency;
- recovery after a restart;
- the domain lifecycle phase (barriers) and its monotonic guard;
- optimistic-lock status patching.

The result was predictable: duplicated code, behavioral drift between domains, subtle race
conditions, and inconsistent snapshot semantics.

The SDK was introduced to:

- standardize the lifecycle of capture requests;
- remove boilerplate;
- enforce invariants identically across all domains.

**The SDK owns:**

- the lifecycle of capture requests (VCR / MCR / child snapshots);
- status patching of the domain-written fields (optimistic-lock);
- the domain lifecycle phase (`Planning`/`Planned`/`Finished`/`Failed`) and its barriers;
- restart-safe behavior;
- child-set freeze and manifest-target drift signalling.

**The domain controller keeps:**

- source validation (`sourceRef`);
- topology discovery (which child snapshots are needed) and the exclude veto;
- PVC resolution for data capture;
- domain-specific errors/reasons.

## TL;DR — what is required from you

**Conceptually:** the SDK lets a domain team **describe snapshot intent** rather than implement
its orchestration.

**In practice** the domain provides just these things:

- an **adapter** (`SnapshotAdapter`) — a thin wrapper over your snapshot CR;
- the **child-snapshot topology** and its **excluded** source objects;
- an **optional PVC** for data capture;
- **manifest targets**;
- the captured **source** to publish.

Everything else (ownerRefs, creating capture requests, optimistic-lock status patching, the
lifecycle phase and its barriers, idempotency, restart-safety, freeze/drift) is done by the SDK.

## What the SDK removes from your code

The domain team **no longer implements by hand**:

- ownerRef management;
- naming of capture requests;
- create-or-adopt logic;
- optimistic-lock status patching;
- lifecycle phase / barrier handling;
- child-set freeze and manifest-target drift checks;
- restart-safe reconciliation of capture requests.

## The capture lifecycle: phases and barriers

There is **no `ChildrenSnapshotReady` condition**. The domain lifecycle is a single field —
`status.captureState.domainSpecificController.phase` — that the SDK writes on the domain's behalf.
It takes four values:

| Phase | Meaning | Set by |
|---|---|---|
| `Planning` | the domain is creating objects/refs (children, MCR/VCR), or waiting on a recoverable dependency | `DomainCaptureStatus` |
| `Planned` | **barrier 1**: everything created and published | `DomainCaptureStatus` |
| `Finished` | **barrier 2**: the domain finished its side (incl. consistency actions) | `DomainCaptureStatus` |
| `Failed` | terminal: the domain hit an unrecoverable error | `DomainCaptureStatus` |

Two properties matter:

- **The forward chain never regresses** and `Failed` is a **terminal sink**. Domains publish
  `PhasePlanned` on every reconcile before switching on the outcome; the monotonic guard means a
  snapshot that already reached `Finished` is never dragged back to `Planned`, and once it is
  `Failed` it stays `Failed`. A non-terminal "waiting for X" state must **not** use `PhaseFailed`
  — it stays in `Planning` (or the current phase) and surfaces a diagnostic via `DomainCaptureStatus`
  (see below), the way a Pod stays `Pending` with a message.
- **The SDK never writes conditions.** The only condition on a snapshot is the core-owned `Ready`.
  The core mirrors `phase=Failed` into `Ready=False`, and it is the sole writer of the terminal
  `Ready` on both the `SnapshotContent` and its owning snapshot. The domain **reads** `Ready` back
  (via the adapter and `CoreCaptureOutcome`) as its failure channel.

`phase>=Planned` is the handoff: the core controller waits for barrier 1 before it takes over the
`SnapshotContent`.

## Where the contract lives (interface map)

The entire public contract is in the `pkg/snapshotsdk` module:

| File | Type | Implemented by |
|---|---|---|
| `capture.go` / `domain_capture_status.go` | `CaptureSDK` (= `Planning` + `SourcePublisher` + `ManifestExclude` + `CaptureInspection` + `DomainCaptureStatus`) | **SDK** (you call it) |
| `adapter.go` | `SnapshotAdapter` | **you** (one per snapshot type) |
| `volumecapture.go` | `VolumeCaptureProvider` | SDK by default (`NewStorageFoundationProvider`) |
| `types.go` | `ChildSpec`, `VolumeCaptureSpec`, `ManifestCaptureSpec`, `SourceRef`, `SnapshotSource`, `DomainCaptureState`, `CaptureOutcomeResult`, `ExcludedObjectRef` | DTOs you pass to / read from the verbs |

The interfaces are declared on the **consumer side** — at the *boundary*, i.e. on the
**integration seam** between the domain controller and the domain-neutral SDK — rather than
dumped into a single `interfaces.go`. This is deliberate: the layout encodes the architecture.

## What the domain controller does: decide four things + drive two barriers

For each snapshot node the domain controller determines:

- which **manifest targets** to save → `EnsureManifestCapture`;
- whether to save **PVC data** (0 or 1) → `EnsureVolumeCapture`;
- which **child snapshots** are needed (0..N) and which source objects are **excluded** →
  `EnsureChildren`;
- the captured **source** to publish → `PublishSnapshotSource`.

The controller expresses intent for each of them, declares barrier 1, then switches on the core
outcome to declare barrier 2 (or stop):

1. **Child snapshots** (`EnsureChildren`) — e.g. a VM snapshot owns the snapshots of its disks.
2. **Data capture** (`EnsureVolumeCapture`) — capture the contents of a **single** PVC (see the
   `DataRef` section).
3. **Manifest capture** (`EnsureManifestCapture`) — capture the manifest targets the domain
   declares for this node. The manifest and data legs are **independent declarations**: if the
   domain also captures a PVC's data and wants that PVC's YAML, it lists the PVC in the manifest
   targets explicitly. The SDK never derives manifest targets from the data leg.
4. **Barrier 1** (`DomainCaptureStatus` → `PhasePlanned`) — "everything is planned"; the core waits
   for exactly this before it takes over the `SnapshotContent`.
5. **Barrier 2** (`DomainCaptureStatus` → `PhaseFinished`) — declared once `CoreCaptureOutcome`
   reports every leg captured, after any consistency action (e.g. fs unfreeze).

---

## Step 1 — implement `SnapshotAdapter` for your CRD

### What it is

`SnapshotAdapter` is a **domain-specific adapter type**: a small wrapper over your snapshot CR
struct. It is an ordinary Go struct in your controller's package that implements the
`SnapshotAdapter` interface; the SDK does not provide it. Technically it is a value wrapper over a
pointer to your snapshot, carrying the mapping methods. The name is up to you; in the demo it is
`demoVirtualDiskSnapshotAdapter`:

```go
type myDomainSnapshotAdapter struct {
	snap *MyDomainSnapshot
}
```

### Why it is needed and why you cannot skip it

This is **dependency inversion**. The SDK (`pkg/snapshotsdk`) is a separate domain-neutral
module; it **must not** import `MyDomainSnapshot`. If the SDK wrote
`s.Status.CaptureState.DomainSpecificController.VolumeCaptureRequestName` directly, it would have
to import every domain CRD — and it would stop being generic. The adapter inverts the dependency:
**the domain depends on the SDK, not the other way around.** The SDK knows only the interface; the
"generic concept → concrete status field" mapping lives in the domain.

Why not the workarounds:
- **A raw `client.Object` + reflection / `unstructured`** is exactly the "magic" the demo
  contract forbids: stringly-typed access to `status.*`, runtime panics instead of compile
  errors.
- **A generic `New[T]`** does not help: the generic still does not know *how* to fetch
  `sourceRef` or *where* to put the VCR name; you need a mapping function — the same adapter in a
  different shape.

### Interface (what to implement)

```go
type SnapshotAdapter interface {
	Object() client.Object                       // live object; the SDK refreshes & patches it
	SourceRef() SourceRef                         // spec.sourceRef

	GetDomainCaptureState() DomainCaptureState    // status.captureState.domainSpecificController
	SetDomainCaptureState(DomainCaptureState)     //   (+ top-level status.childrenSnapshotRefs)

	GetSnapshotSource() *SnapshotSource           // top-level status.sourceRef (read; nil = unset)
	SetSnapshotSource(*SnapshotSource)            // top-level status.sourceRef (write)

	CoreCaptureState() CoreCaptureState           // read-only core handoff (commonController latches)

	ReadyStatus() metav1.ConditionStatus          // read-only core-written status.conditions[Ready]
	ReadyReason() string
	ReadyMessage() string
}
```

**Writer discipline.** The SDK writes ONLY `status.captureState.domainSpecificController` (via
`Get/SetDomainCaptureState`), the top-level `status.childrenSnapshotRefs` (via the same), and the
top-level `status.sourceRef` (via `Get/SetSnapshotSource`). It NEVER writes the `Ready` condition
and NEVER writes the core-owned `captureState.commonController` — it only reads them
(`CoreCaptureState`, `ReadyStatus`/`ReadyReason`/`ReadyMessage`).

Contract rules:
- **No side effects.** No API calls in the methods — only reading/writing struct fields. All
  cluster access is done by the SDK.
- `Object()` returns the **same pointer** that the other methods read/write (the same `s`).
- This is the **only place** where `client.Object` crosses the domain↔SDK boundary. In the body of
  `Reconcile` you do **not** call these mapping methods directly — only the intent verbs
  (`Ensure*` / `DomainCaptureStatus` / `PublishSnapshotSource`).

A 1:1 template — `internal/controllers/demo/snapshot_adapter.go`.

## Step 2 — build the `CaptureSDK` (once per reconciler)

```go
func (r *MySnapshotReconciler) capture() snapshotsdk.CaptureSDK {
	return snapshotsdk.New(r.Client, r.APIReader, snapshotsdk.NewStorageFoundationProvider(r.Client))
}
```

- `Client` — writes and cache reads.
- `APIReader` — a live (uncached) reader; the SDK uses it for TOCTOU-safe refreshes of the leg
  latches and the frozen phase/child set.
- `VolumeCaptureProvider` — the data-capture backend; the default is the storage-foundation
  `VolumeCaptureRequest`.

Optional dependencies are supplied via `Option`s. An **aggregator** that builds a manifest leg
spanning objects its children also capture needs the subresource REST client for
`SubtreeManifestIdentities` (see the manifest-exclude section):

```go
snapshotsdk.New(r.Client, r.APIReader, provider, snapshotsdk.WithSubresourceREST(restClient))
```

A leaf/parent that does not use that capability may omit it.

## Step 3 — in `Reconcile`: wrap the object in the adapter and call the verbs in order

"Getting" the adapter = construct it as a literal from the object you just fetched from the
cluster. There is no factory:

```go
func (r *MySnapshotReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	s := &MyDomainSnapshot{}
	if err := r.Client.Get(ctx, req.NamespacedName, s); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if s.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	// Import mode: NO capture planning — the live source may be absent. Domain planning is
	// trivially complete (the core materializes SnapshotContent from the uploaded manifests).
	if s.IsImportMode() {
		return ctrl.Result{}, nil
	}

	adapter := myDomainSnapshotAdapter{snap: s} // ← this is the whole "get the adapter"
	sdk := r.capture()

	// 1. Source validation — your logic.
	//    Invalid sourceRef → TERMINAL (PhaseFailed).
	if /* sourceRef invalid */ {
		return ctrl.Result{}, sdk.DomainCaptureStatus(adapter).
			Phase(snapshotsdk.PhaseFailed).
			Reason("InvalidSourceRef").
			Message("...").
			Apply(ctx)
	}
	//    Source not found → RECOVERABLE: publish Planning + message and requeue (it may still appear).
	if /* source not found */ {
		if err := sdk.DomainCaptureStatus(adapter).
			Phase(snapshotsdk.PhasePlanning).
			Message("waiting for <source> to exist").
			Apply(ctx); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: retry}, nil
	}

	// 2. Publish the captured live source (top-level status.sourceRef; used by import-mode recreation).
	if err := sdk.PublishSnapshotSource(ctx, adapter, snapshotsdk.SnapshotSource{ /* APIVersion, Kind, Name, Namespace, UID */ }); err != nil {
		return ctrl.Result{}, err
	}

	// 3. Children (a leaf with no children → nil, nil). Honor the exclude veto (PartitionExcluded).
	if err := sdk.EnsureChildren(ctx, adapter, childSpecs, excludedRefs); err != nil {
		if errors.Is(err, snapshotsdk.ErrChildrenSetFrozen) { // a post-Planned set growth
			return ctrl.Result{}, sdk.DomainCaptureStatus(adapter).
				Phase(snapshotsdk.PhaseFailed).
				Reason(snapshotsdk.Reason(storagev1alpha1.ReasonGraphPlanningFailed)).
				Message(err.Error()).
				Apply(ctx)
		}
		return ctrl.Result{}, err
	}

	// 4. Data capture (no PVC → DataRef: nil = no-op; PVC not there yet → DomainCaptureStatus + requeue).
	if err := sdk.EnsureVolumeCapture(ctx, adapter, snapshotsdk.VolumeCaptureSpec{DataRef: dataRef}); err != nil {
		return ctrl.Result{}, err
	}

	// 5. Manifest (always at least one target).
	if err := sdk.EnsureManifestCapture(ctx, adapter, snapshotsdk.ManifestCaptureSpec{Targets: manifestTargets}); err != nil {
		return ctrl.Result{}, err
	}

	// 6. Barrier 1 — everything planned/published.
	if err := sdk.DomainCaptureStatus(adapter).Phase(snapshotsdk.PhasePlanned).Apply(ctx); err != nil {
		return ctrl.Result{}, err
	}

	// 7. Barrier 2 — switch on the core-derived capture outcome.
	switch outcome := snapshotsdk.CoreCaptureOutcome(adapter); outcome.Outcome {
	case snapshotsdk.CaptureOutcomeCaptured:
		return ctrl.Result{}, sdk.DomainCaptureStatus(adapter).Phase(snapshotsdk.PhaseFinished).Apply(ctx) // after any consistency action (e.g. fs unfreeze)
	case snapshotsdk.CaptureOutcomeFailed:
		return ctrl.Result{}, nil // the core owns the terminal Ready; nothing for the domain to re-drive
	default: // CaptureOutcomeCapturing: wait
		return ctrl.Result{RequeueAfter: retry}, nil
	}
}
```

Order: the planning calls (`EnsureChildren` / `EnsureVolumeCapture` / `EnsureManifestCapture`) are
**independent** and may run in any order relative to each other. Each verb depends only on its own
spec and never reads another leg's result, so the requests they produce are identical regardless of
call order — in particular `EnsureManifestCapture` builds the MCR solely from its declared `Targets`
and does not consult the data-leg VCR. Barrier 1 (`DomainCaptureStatus` → `PhasePlanned`) comes after all three planning
calls; the barrier-2 outcome switch comes last. On an error from any `Ensure*`, just `return err`
and the reconcile retries.

### Import-mode short-circuit

`spec.mode: Import` switches a snapshot off capture entirely. The live source (and its disks/PVCs)
may be absent on import, so the domain controller does **no** capture planning — no source lookup,
no children, no MCR/VCR. It returns early (`if s.IsImportMode() { return ctrl.Result{}, nil }`).
The core controller materializes the backing `SnapshotContent` from the uploaded manifests
(reconstructed checkpoint) and the data leg from the matching import; domain planning is trivially
complete for an import node.

---

## `manifestTargets` — which manifests end up in one MCR

`EnsureManifestCapture(ctx, adapter, ManifestCaptureSpec{Targets: ...})` takes the **complete
desired set** of manifest target identities (`apiVersion`/`kind`/`name`; the namespace is implicit,
equal to the snapshot's) that the domain controller considers to belong to this snapshot node. The
SDK turns this list into **one** `ManifestCaptureRequest` and publishes its name into
`status.captureState.domainSpecificController.manifestCaptureRequestName`.

```go
manifestTargets := []snapshotsdk.ManifestTarget{{
	APIVersion: demov1alpha1.SchemeGroupVersion.String(),
	Kind:       "DemoVirtualDisk",
	Name:       source.Name,
}}
// A disk with a data leg also lists the PVC whose YAML it wants captured alongside the disk:
if dataRef != nil {
	manifestTargets = append(manifestTargets, snapshotsdk.ManifestTarget{
		APIVersion: dataRef.APIVersion,
		Kind:       dataRef.Kind,
		Name:       dataRef.Name,
	})
}
```

The SDK does not decide for the domain which manifests belong to the node. It is only responsible
for the transport mechanics: create/verify one MCR, set the ownerRef, publish its name, and
preserve restart-safe behavior. It captures **exactly** the targets the domain declares — it never
derives or injects targets from the data leg. A PVC whose YAML must be captured is listed in
`Targets` by the domain (see the disk controller).

### Manifest capture cannot be empty (`ErrEmptyManifest`)

Every snapshot captures at least its own source object's manifest. The declared target set MUST be
**non-empty**: the SDK does **not** substitute the snapshotted resource for you and does **not**
inject a PVC from the data leg. An empty `Targets` returns `snapshotsdk.ErrEmptyManifest` **before**
any cluster mutation (the MCR CRD enforces the same invariant via CEL as a second line of defense).
An empty set is a domain planning bug, not a transient state — recommended reaction
`DomainCaptureStatus` with `PhaseFailed` and reason `GraphPlanningFailed`. The captured-latch suppression wins over this guard: once the core
has stamped the manifest leg captured, the call is a no-op (`nil`) regardless of input — a late
post-capture recomputation that came up empty must not fail an already-captured snapshot.

### Manifest capture is adopt-then-drift (`ErrManifestTargetsDrift`)

A snapshot is point-in-time, so the `ManifestCaptureRequest`'s target set is the **frozen** capture
plan. `EnsureManifestCapture` is **adopt-then-drift**: when the MCR is absent it creates it and
publishes its name; when the MCR already exists it **adopts** it — idempotently publishing the name
into status, never patching `spec.targets` — and only THEN, if this reconcile declares a
**different** set (compared as **sets** by `(apiVersion, kind, name)`; order and duplicates do not
matter), it **signals** `snapshotsdk.ErrManifestTargetsDrift`. Drift is a **signal, not a
decision**: the name is already published, so the leg is established regardless — the **caller**
decides what to do. A domain typically reacts with `DomainCaptureStatus` → `PhaseFailed` /
`GraphPlanningFailed`; the
namespace-root aggregator **ignores** it (it recomputes a shifting set over a live namespace, and
the first plan wins). Immutability of `spec.targets` is the apiserver's job: the MCR CRD's CEL
transition rule (`self.targets == oldSelf.targets`) rejects any change — the SDK itself never
patches the targets. An identical re-declaration is a no-op.

```go
if err := sdk.EnsureManifestCapture(ctx, adapter, snapshotsdk.ManifestCaptureSpec{Targets: manifestTargets}); err != nil {
	if errors.Is(err, snapshotsdk.ErrManifestTargetsDrift) {
		_ = sdk.DomainCaptureStatus(adapter).
			Phase(snapshotsdk.PhaseFailed).
			Reason(snapshotsdk.Reason(storagev1alpha1.ReasonGraphPlanningFailed)).
			Message(err.Error()).
			Apply(ctx)
	}
	return ctrl.Result{}, err
}
```

The compared set is exactly the domain's declared `Targets` — the manifest leg is not augmented
from the data leg, so a data-backed PVC only participates in the comparison if the domain declared
it in `Targets`.

A caller that wants to skip building targets once the leg is established gates on
`snapshotsdk.ManifestCaptureNeeded(adapter)` — a pure status read that is true iff the MCR name is
not yet published **and** the core has not latched the manifest leg captured. The namespace-root
uses it to avoid re-listing the live namespace once its MCR exists.

---

## `childSpecs` and `excludedRefs` — what they are and how to build them

```go
EnsureChildren(ctx, adapter, desired []ChildSpec, excluded []ExcludedObjectRef) error
```

`EnsureChildren` takes the **desired set** of child snapshots **and** the set of source objects the
domain vetoed while enumerating (the exclude veto). Both are published in one status patch:
children into the top-level `status.childrenSnapshotRefs`, excluded into
`status.captureState.domainSpecificController.excludedRefs`.

### `ChildSpec`

```go
type ChildSpec struct {
	Object client.Object // fully assembled by the domain: the child snapshot CR
}
```

This is the **child-builder seam**: the domain constructs the child object in full (kind, name,
`spec.sourceRef`, labels), and the SDK takes on only the k8s mechanics:

- sets the parent's **controller ownerRef** on the child object;
- does **create-or-adopt** (creates if absent; adopts an existing one);
- derives the child `SnapshotChildRef` from GVK+name and **additively unions** it into
  `status.childrenSnapshotRefs`.

The SDK **never invents** the child's domain spec fields — you are the only one who does that.

### How to build it (example: VM → disk snapshots)

Name each child deterministically with `snapshotsdk.ChildSnapshotName(parentSnapshotUID, sourceUID)`
— the same UID scheme the core uses — so a repeated reconcile never spawns duplicates. Connectivity
is carried by the ownerRef/childRefs the SDK writes, not by the name:

```go
kept, excluded := snapshotsdk.PartitionExcluded(ownedDisks) // honor state-snapshotter.deckhouse.io/exclude

children := make([]snapshotsdk.ChildSpec, 0, len(kept))
for _, o := range kept {
	disk := o.(*demov1alpha1.DemoVirtualDisk)
	children = append(children, snapshotsdk.ChildSpec{Object: &demov1alpha1.DemoVirtualDiskSnapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: vm.Namespace, Name: snapshotsdk.ChildSnapshotName(vm.UID, disk.UID)},
		Spec: demov1alpha1.DemoVirtualDiskSnapshotSpec{
			SourceRef: &demov1alpha1.SnapshotSourceRef{Kind: "DemoVirtualDisk", Name: disk.Name},
		},
	}})
}

excludedRefs := make([]snapshotsdk.ExcludedObjectRef, 0, len(excluded))
for _, o := range excluded {
	excludedRefs = append(excludedRefs, snapshotsdk.ExcludedObjectRef{
		APIVersion: demov1alpha1.SchemeGroupVersion.String(), Kind: "DemoVirtualDisk", Name: o.GetName(),
	})
}
```

### Important `EnsureChildren` invariants

- **Additive publication (union), delete-free (SDK v1).** The SDK creates/adopts and **unions** the
  freshly derived refs into the currently published set — it never removes a ref and never deletes a
  child object. A child no longer enumerated by its emitter keeps its published ref; only the
  leftover child object is reclaimed by ownerRef GC or a future cleanup component, not by the SDK.
  A `nil`/empty desired set therefore publishes no new refs and leaves the current set intact.
- **The child set is FROZEN at barrier 1 (`ErrChildrenSetFrozen`).** Once the node is at
  `phase>=Planned` (including the terminal `Failed`), `EnsureChildren` rejects any attempt to
  **grow** the declared set (or change the excluded set) with `snapshotsdk.ErrChildrenSetFrozen` —
  fail-closed and **before** any child CR is created, so a rejected call has no side effects. An
  idempotent re-publish of the same set (desired ⊆ published, excluded unchanged) stays a no-op at
  any phase. The freeze mirrors the immutable `SnapshotContent.status.childrenSnapshotContentRefs`;
  a late-added edge would wedge the node forever, so the recommended reaction is
  `DomainCaptureStatus` → `PhaseFailed` / `GraphPlanningFailed`:
  ```go
  if err := sdk.EnsureChildren(ctx, adapter, childSpecs, excludedRefs); err != nil {
  	if errors.Is(err, snapshotsdk.ErrChildrenSetFrozen) {
  		return ctrl.Result{}, sdk.DomainCaptureStatus(adapter).
  			Phase(snapshotsdk.PhaseFailed).
  			Reason(snapshotsdk.Reason(storagev1alpha1.ReasonGraphPlanningFailed)).
  			Message(err.Error()).
  			Apply(ctx)
  	}
  	// Recoverable (adoption conflict / transient API error): requeue with backoff, phase stays pre-Planned.
  	return ctrl.Result{}, err
  }
  ```
- Child names must be **deterministic** (the same name for the same logical child), otherwise a
  repeated reconcile will spawn duplicates. Use `snapshotsdk.ChildSnapshotName`.

Reference: `virtualmachinesnapshot_controller.go` (a parent with children).

---

## The exclude veto

The label `state-snapshotter.deckhouse.io/exclude` (`snapshotsdk.ExcludeLabelKey`) is an
**absolute, always-active** veto: any object carrying it (value ignored) is dropped from every
snapshot, at every level of the tree, independently of the root's `spec.resourceSelector`.

The core folds the veto into its own resource resolution, but a **domain enumerator sees only the
child specs it builds — not the source objects' labels** — so it MUST apply the veto itself:

- call `snapshotsdk.PartitionExcluded(sourceObjs)` → `(kept, excluded)`;
- build children from `kept`;
- hand the `excluded` refs to `EnsureChildren` as the 4th argument.

The SDK publishes those excluded refs into
`status.captureState.domainSpecificController.excludedRefs` (the transient INPUT the core folds into
the durable `SnapshotContent.status.excludedRefs` and mirrors onto the top-level
`status.excludedRefs`). The domain never writes the durable aggregate or the top-level mirror. Pass
an empty/`nil` excluded set when nothing is vetoed — a data-leaf that never enumerates children
always does. A vetoed child gets no child snapshot (and hence no VCR/MCR); an incomplete image is
accepted by design (no consistency-group machinery; the operator owns that trade-off).

---

## `DataRef` — what it is and why it is exactly one PVC

`EnsureVolumeCapture(ctx, adapter, VolumeCaptureSpec{DataRef: ...})` describes **data capture** —
capturing the contents of a single PVC. `Target` is the PVC identity that the domain resolved
itself:

```go
type Target struct {
	UID        string
	APIVersion string // "v1"
	Kind       string // "PersistentVolumeClaim"
	Name       string
	Namespace  string
}
```

The domain finds its own PVC and makes its own readiness decisions. A **missing PVC is recoverable,
not terminal**: the domain surfaces it via `DomainCaptureStatus` (`PhasePlanning` + message, optionally
`Reason`) and requeues via `ctrl.Result` — it must **not** enter the terminal `Failed`
sink, or a PVC that shows up later could never be captured. From the `DataRef` the SDK creates a storage-foundation
`VolumeCaptureRequest` (VCR) and publishes its name in
`status.captureState.domainSpecificController.volumeCaptureRequestName`. This is the data leg only —
it does **not** feed the manifest leg; if the PVC's YAML must also be captured, the domain lists it
in the manifest `Targets`.

### Invariant: a snapshot's data is EXACTLY ONE (optional) data ref

```
GOOD: one snapshot node = at most one data capture (one PVC)

VM Snapshot
 ├── Disk Snapshot A -> PVC A
 └── Disk Snapshot B -> PVC B

BAD: several PVCs on one node (not part of the model)

VM Snapshot
 ├── PVC A
 └── PVC B
```

**One snapshot node = at most one data capture (one PVC).** If the domain has several PVCs, that
is **not** several `DataRef`s but several **child** snapshot nodes (each with its single PVC).

The canonical model is **one logical data capture per snapshot** (Variant A, cardinality ≤1; see
`api/storage/v1alpha1` `SnapshotContent.dataRef` — it too is a single pointer). That is why the
field is a single pointer, not a slice:

```go
type VolumeCaptureSpec struct {
	DataRef *Target // one PVC, or nil
}
```

- **`DataRef == nil`** → a manifest-only snapshot: the SDK does not create a VCR and does not
  publish a name (an explicit no-op).
- **`DataRef != nil`** → a normal data capture of a single PVC.
- In the demo, `resolveDemoVirtualDiskDataRef` returns `*snapshotsdk.Target` (a PVC), or `nil` for a
  manifest-only disk, or a non-empty "pending" message when the PVC is not present yet.

> A `[]Target` slice is impossible here **by design**: the type itself forbids "several data
> captures on one snapshot". PVC multiplicity is expressed only through child nodes. The only
> place a list of targets actually exists is the unstructured wrapper over the foundation CRD
> `VolumeCaptureRequest` (`spec.targets[]`) inside `internal/storagefoundation`; the SDK always
> puts exactly one element there.

### How to build it (example: disk → its PVC)

```go
pvcName := source.Spec.PersistentVolumeClaimName
if pvcName == "" {
	return nil, "", nil // manifest-only disk: DataRef stays nil
}
pvc := &corev1.PersistentVolumeClaim{}
if err := reader.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: pvcName}, pvc); err != nil {
	if apierrors.IsNotFound(err) {
		// RECOVERABLE: the PVC may appear later → return a "pending" message; the caller surfaces it
		// via DomainCaptureStatus (PhasePlanning + Message) and requeues (NOT PhaseFailed, NOT an error).
		return nil, fmt.Sprintf("waiting for PersistentVolumeClaim %q to exist", pvcName), nil
	}
	return nil, "", err
}
dataRef := &snapshotsdk.Target{
	UID:        string(pvc.UID),
	APIVersion: corev1.SchemeGroupVersion.String(),
	Kind:       "PersistentVolumeClaim",
	Name:       pvc.Name,
	Namespace:  pvc.Namespace,
}
```

Reference: `virtualdisksnapshot_controller.go` (a leaf with PVC data capture).

---

## Publishing the captured source (`status.sourceRef`)

`PublishSnapshotSource(ctx, adapter, SnapshotSource{...})` publishes the captured live source's full
reference into the top-level `status.sourceRef`. It is self-contained (`apiVersion`, `kind`, `name`,
`namespace`, `uid`) so `d8`-cli reads it as a single block to rebuild the import-mode source. It is
**not** part of the readiness formula. Only domain snapshots that capture a live source publish it;
a nil/zero source is a no-op.

```go
if err := sdk.PublishSnapshotSource(ctx, adapter, snapshotsdk.SnapshotSource{
	APIVersion: demov1alpha1.SchemeGroupVersion.String(),
	Kind:       "DemoVirtualDisk",
	Name:       source.Name,
	Namespace:  source.Namespace,
	UID:        source.UID,
}); err != nil {
	return ctrl.Result{}, err
}
```

---

## Barrier 2 — waiting on the core with `CoreCaptureOutcome`

After barrier 1, the core captures the legs and flips the per-leg success latches on
`status.captureState.commonController` (`manifestCaptured`, `dataCaptured`; each is a `*bool`: nil =
no such leg, false = declared but not captured, true = captured). The domain never writes these — it
**reads** them through `CoreCaptureOutcome`, which derives a tri-state from the latches plus the
terminal `Ready` reason:

```go
switch outcome := snapshotsdk.CoreCaptureOutcome(adapter); outcome.Outcome {
case snapshotsdk.CaptureOutcomeCaptured:
	// Every declared leg is captured and Ready is not terminal → confirm consistency (barrier 2).
	return ctrl.Result{}, sdk.DomainCaptureStatus(adapter).Phase(snapshotsdk.PhaseFinished).Apply(ctx)
case snapshotsdk.CaptureOutcomeFailed:
	// The core surfaced a terminal Ready reason (own manifest/volume leg, or a bubbled child failure).
	// The domain does NOT re-drive it into phase=Failed — turning a core-owned leg failure into a
	// terminal is the core's job (Variant A). Stop; requeuing would only spin.
	// outcome.Reason / outcome.Message carry the terminal detail.
	return ctrl.Result{}, nil
default: // CaptureOutcomeCapturing
	return ctrl.Result{RequeueAfter: retry}, nil
}
```

`CaptureOutcomeFailed` is checked first: a terminal `Ready` reason (`IsReasonTerminal`) wins over
the success latches (which are success-only and never express failure).

## Barrier 2 in an aggregator — `childrenSettled` + `CoreCaptureOutcome`

An aggregator (e.g. a VM whose child disks each own a data leg) declares barrier 2 not on a coarse
child `Ready` rollup, but on the core-derived capture outcome of **its own** node plus the
`childrenSettled` latch (every direct child is terminal — captured-OK OR failed). The only safe
pattern is a switch on `CoreCaptureOutcome` (`Failed` checked FIRST), with the consistency action
gated on `childrenSettled` OR the domain's own freeze-deadline:

```go
switch outcome := snapshotsdk.CoreCaptureOutcome(adapter); outcome.Outcome {
case snapshotsdk.CaptureOutcomeFailed:
	// Checked FIRST. The terminal (including a bubbled ChildrenFailed from a failed child) is owned by the
	// core — the domain does not re-drive it. But stopping does NOT excuse COMPENSATING the consistency
	// action: if the fs was frozen, unfreeze MUST run here (else the freeze leaks); the Captured branch is
	// unreachable in this case.
	return ctrl.Result{}, r.thawIfFrozen(ctx, adapter) // compensation
case snapshotsdk.CaptureOutcomeCaptured:
	// Every OWN leg is captured. The consistency action (e.g. guest-fs unfreeze) is gated on the node's own
	// childrenSettled OR a domain freeze-deadline: childrenSettled has no core TTL, a hung child never flips
	// it, so liveness is the domain-side deadline's job, not the core's.
	settled := adapter.CoreCaptureState().ChildrenSettled
	if (settled == nil || !*settled) && !r.freezeDeadlineExceeded(adapter) {
		return ctrl.Result{RequeueAfter: retry}, nil // wait for children to settle
	}
	if err := r.thawIfFrozen(ctx, adapter); err != nil { // consistency action
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, sdk.DomainCaptureStatus(adapter).Phase(snapshotsdk.PhaseFinished).Apply(ctx)
default: // CaptureOutcomeCapturing
	return ctrl.Result{RequeueAfter: retry}, nil
}
```

> **Why NOT a loop over `IsReasonTerminal(child.ReadyReason)`.** Stopping on each child's terminal
> reason and otherwise waiting for every child's `AllLegsCaptured` is **dangerous**: a child can fail
> with a domain free-form reason that is not in `TerminalReadyReasons`, and such a loop hangs forever.
> `childrenSettled` counts a failure as terminal too (not only success), and `CaptureOutcomeFailed`
> bubbles the core's `ChildrenFailed` — together they cover both a child failure and a child hang (the
> latter via the domain freeze-deadline).

For **fine-grained** per-child diagnostics there is `ChildrenCaptureStates(ctx, adapter)`: it resolves
the declared child refs and returns, for each, its `Ready` status/reason/message and `AllLegsCaptured`
(whether all its declared legs are latched). Children are read as unstructured by their ref GVK (any
domain child kind); a child not found yet has an empty `Ready` and `AllLegsCaptured=false`. This is a
read-only inspection, NOT a replacement for the gate above.

## Manifest exclude for aggregators — `SubtreeManifestIdentities` (optional)

This capability is only for **aggregators** whose own manifest leg spans objects their descendant
snapshots already capture (e.g. a namespace-root Snapshot, or a VM whose disk children capture part
of its objects). It builds the aggregator's MCR as `EnsureManifestCapture(base − exclude)`, where
the exclude set is everything the subtree already captured.

`SubtreeManifestIdentities(ctx, adapter)` returns the union of object identities captured across the
node's DIRECT children subtrees. It requires the subresource REST client
(`WithSubresourceREST`; without it the call returns a configuration error). It is **fail-closed**:
if any subtree is not fully persisted or a child has not bound its content yet, it returns
`snapshotsdk.ErrSubtreeIdentitiesPending` and the caller requeues — a partial exclude is never
returned. A node with no children returns an empty set. A leaf/parent that does not aggregate
overlapping manifests does not need this at all.

**Built-in pre-gate.** Before touching the subresource the method consults **its own** node's
`CoreCaptureState().ChildSubtreesManifestsPersisted` — the core-computed latch "the subtrees of ALL
declared direct children are fully persisted" (children-only: this node's own manifests are NOT
included, so it can flip `true` before this node's own MCR even exists). When it is explicitly `false`
the method returns `ErrSubtreeIdentitiesPending` **without a single REST call** — saving endpoint
round-trips and 409-requeue cycles while descendants are still capturing. `nil` (the adapter does not
map the field, or it is not computed yet) disables the pre-gate and preserves the prior behavior;
correctness is still held by the subresource's fail-closed 409.

---

## Domain capture status — the single public status API

`DomainCaptureStatus` is the **single public API** for writing domain capture status
(`phase` / `reason` / `message`) into `status.captureState.domainSpecificController`.
Use it for every lifecycle write: recoverable waiting, barrier 1, barrier 2, and terminal domain failure.

State machine (monotonic; never regresses):

```
Planning  -->  Planned  -->  Finished
     |              |
     +--------------+---->  Failed  (terminal sink)
```

| Phase | When | Typical call |
|---|---|---|
| `Planning` | creating refs, or waiting on a recoverable dependency | `.Phase(PhasePlanning).Reason(...).Message(...).Apply(ctx)` |
| `Planned` | **barrier 1** — everything planned/published | `.Phase(PhasePlanned).Apply(ctx)` |
| `Finished` | **barrier 2** — domain side done (incl. consistency actions) | `.Phase(PhaseFinished).Apply(ctx)` |
| `Failed` | terminal domain failure (invalid spec, planning contract violation) | `.Phase(PhaseFailed).Reason(...).Message(...).Apply(ctx)` |

**Properties:**

- **Entering `Phase(Planned)` / `Phase(Finished)` force-clears `reason` and `message`** on the wire.
  A same-phase re-apply may update the message (e.g. a post-barrier diagnostic) without regressing phase.
- **`Failed` is a terminal sink** — once set, the SDK never leaves it. Recoverable waiting must stay
  in `Planning` (with a message and optional reason), not `Failed` — the Pod model of staying
  `Pending` with a diagnostic.
- **The forward chain never regresses** — a snapshot that reached `Finished` is never dragged back to
  `Planned`.
- **Core-owned leg failures are not written as `phase=Failed` by the domain.** The core surfaces them
  on `Ready`; the domain observes `CoreCaptureOutcome=Failed` and stops.

### Waiting, barriers, and failure (one API)

Recoverable wait (source or PVC not ready yet):

```go
if err := sdk.DomainCaptureStatus(adapter).
    Phase(snapshotsdk.PhasePlanning).
    Message("waiting for PersistentVolumeClaim \"data\" to exist").
    Apply(ctx); err != nil {
    return ctrl.Result{}, err
}
return ctrl.Result{RequeueAfter: retry}, nil
```

Wait with a machine-readable progress reason:

```go
if err := sdk.DomainCaptureStatus(adapter).
    Phase(snapshotsdk.PhasePlanning).
    Reason("Snapshotting").
    Message(fmt.Sprintf("Waiting for the snapshot of virtual disk %q to be ready to use.", name)).
    Apply(ctx); err != nil {
    return ctrl.Result{}, err
}
return ctrl.Result{RequeueAfter: retry}, nil
```

Barrier 1 (after all `Ensure*` planning calls):

```go
if err := sdk.DomainCaptureStatus(adapter).Phase(snapshotsdk.PhasePlanned).Apply(ctx); err != nil {
    return ctrl.Result{}, err
}
```

Barrier 2 (after `CoreCaptureOutcome=Captured` and any consistency action):

```go
return ctrl.Result{}, sdk.DomainCaptureStatus(adapter).Phase(snapshotsdk.PhaseFinished).Apply(ctx)
```

Terminal domain failure (invalid `sourceRef`, `ErrChildrenSetFrozen`, `ErrManifestTargetsDrift`,
`ErrEmptyManifest`):

```go
return ctrl.Result{}, sdk.DomainCaptureStatus(adapter).
    Phase(snapshotsdk.PhaseFailed).
    Reason(snapshotsdk.Reason(storagev1alpha1.ReasonGraphPlanningFailed)).
    Message(err.Error()).
    Apply(ctx)
```

`Apply` goes through the SDK status-write path (adapter + optimistic patch, monotonic transitions,
Failed sink). It never writes `status.conditions`. Prefer a fresh writer (or a full
Phase/Reason/Message rewrite) before each `Apply` so stale reason/message cannot leak across reconcile
branches.

## Guarantees you can rely on

- **Idempotency / restart-safe.** Any `Ensure*` can be called every reconcile; a repeated call
  breaks nothing and creates no duplicates (deterministic VCR/MCR/child names).
- **Per-leg suppression via the core latches.** Once the **core** flips a leg's success latch on
  `captureState.commonController`, that leg's `Ensure*` becomes a no-op: `EnsureVolumeCapture` is
  suppressed once `dataCaptured` is true, and `EnsureManifestCapture` once `manifestCaptured` is
  true (so a request deleted by the binder after capture is not recreated). This is **per leg**, not
  a single global "after the barrier everything is frozen" switch. The child set has its own freeze
  (`phase>=Planned`, `ErrChildrenSetFrozen`), and a changed manifest target set is **signalled** as
  drift (`ErrManifestTargetsDrift`) after the MCR name is adopted — `spec.targets` immutability
  itself is enforced by the apiserver CEL.
- **The domain/SDK boundary.** The domain owns: source validation, child planning, the exclude veto,
  domain objects. The SDK owns: ownerRefs, capture orchestration, request lifecycle, the
  domain-written status fields and the lifecycle phase. The **core** owns the `Ready` condition and
  the `commonController` leg latches.

## Where to start in practice

Take the demo implementation as a starting point and adapt it to your type:
1. `internal/controllers/demo/snapshot_adapter.go` — the adapter;
2. `virtualdisksnapshot_controller.go` (a leaf with PVC data capture) **or**
   `virtualmachinesnapshot_controller.go` (a parent with children, manifest-only) — the reconcile
   skeleton.

This is the reference implementation: the demo controllers in the `sds-unified-snapshots-poc` repo
are deliberately kept as executable documentation of the SDK.
