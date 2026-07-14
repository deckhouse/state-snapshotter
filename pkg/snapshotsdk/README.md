> Language: **English** · [Русский](./README.ru.md)

# Snapshot SDK — domain integration guide (capture)

> **Status: developer-facing usage guide** for teams integrating their domain with the
> snapshot controller through `pkg/snapshotsdk`. This is *how to use it*, not the normative
> contract. Normative sources are the godoc in `pkg/snapshotsdk` (interfaces and invariants)
> and the code-quality contract in [`CLAUDE.md`](./CLAUDE.md). The reference implementation is
> the demo controllers under `images/domain-controller/internal/controllers/demo`.
>
> SDK v1 scope is **capture-only** (snapshot planning: child snapshots + data capture +
> manifest capture + barrier). Restore is a separate sanctioned boundary
> (`pkg/snapshotsdk/transform`) and is out of scope here.

**In one paragraph:** `snapshotsdk` lets a domain controller **describe snapshot intent** (child
snapshots, an optional data PVC, manifests) without implementing snapshot orchestration. The
domain decides **what** to snapshot; the SDK decides **how** capture requests, conditions,
ownership, and restart-safe planning are managed.

## The snapshot lifecycle in one minute

1. A user creates a domain snapshot CR (`MySnapshot`).
2. The domain controller validates the source object.
3. The controller decides three things:
   - which **manifests** to save;
   - whether to save **PVC data**;
   - which **child snapshots** are needed.
4. The controller hands this to the SDK.
5. The SDK creates capture requests (MCR / VCR / child snapshots) and publishes status.
6. The controller raises planning-ready (the planning barrier).
7. The core controller materializes the `SnapshotContent`.

```
User
  |
  v
Domain Snapshot CR
  |
  v
Domain Controller
  |-- discover children
  |-- resolve PVC
  |-- choose manifests
  |
  v
Snapshot SDK
  |-- EnsureChildren
  |-- EnsureVolumeCapture
  |-- EnsureManifestCapture
  |
  v
Planning barrier = Ready
  |
  v
Core snapshot controller
  |
  v
SnapshotContent
```

That is the whole flow. The rest of this guide covers each step in detail.

## What the snapshot SDK is and why it exists

`pkg/snapshotsdk` is a domain-neutral library that standardizes the **capture phase** of a
snapshot (planning: child snapshots + data capture + manifest capture + barrier). The domain
team **describes the intent** of a snapshot ("what we capture"), and the SDK takes over the
orchestration ("how to lay it out in Kubernetes").

Before the SDK, every domain team had to implement all of this itself:

- ownerRefs on capture objects;
- status conditions;
- create/adopt of capture requests;
- idempotency;
- recovery after a restart;
- planning-barrier semantics;
- optimistic-lock status patching.

The result was predictable: duplicated code, behavioral drift between domains, subtle race
conditions, and inconsistent snapshot semantics.

The SDK was introduced to:

- standardize the lifecycle of capture requests;
- remove boilerplate;
- enforce invariants identically across all domains.

**The SDK owns:**

- the lifecycle of capture requests (VCR / MCR / child snapshots);
- status patching (optimistic-lock);
- the planning barrier;
- restart-safe behavior;
- drift detection (child snapshots, data capture, manifest capture).

**The domain controller keeps:**

- source validation (`sourceRef`);
- topology discovery (which child snapshots are needed);
- PVC resolution for data capture;
- domain-specific errors/reasons.

## TL;DR — what is required from you

**Conceptually:** the SDK lets a domain team **describe snapshot intent** rather than implement
its orchestration.

**In practice** the domain provides just four things:

- an **adapter** (`SnapshotAdapter`) — a thin wrapper over your snapshot CR;
- the **child-snapshot topology**;
- an **optional PVC** for data capture;
- **manifest targets**.

Everything else (ownerRefs, creating capture requests, optimistic-lock status patching, the
barrier condition name, idempotency, restart-safety, drift) is done by the SDK.

## What the SDK removes from your code

The domain team **no longer implements by hand**:

- ownerRef management;
- naming of capture requests;
- create-or-adopt logic;
- optimistic-lock status patching;
- barrier-condition handling;
- topology drift checks;
- restart-safe reconciliation of capture requests.

## What the "planning barrier" is

The planning barrier is a status marker that separates two phases:

- the **domain planning phase** — the domain controller decides what to snapshot;
- the **core processing phase** — the core controller materializes the `SnapshotContent`.

When `MarkPlanningReady()` is called, ownership of the snapshot **passes from the domain
controller to the core controller**.

Technical detail (safe to skip on first read): the barrier is a **durable** status condition
(`ChildrenSnapshotReady`), not a runtime synchronization primitive; it survives restarts and is
raised by exactly one `MarkPlanningReady` call. Until it is raised, the core controller does not
touch the `SnapshotContent`.

## Where the contract lives (interface map)

The entire public contract is in the `pkg/snapshotsdk` module:

| File | Type | Implemented by |
|---|---|---|
| `capture.go` | `CaptureSDK` (= `Planning` + `PlanningBarrier` + `ReadinessFault`) | **SDK** (you call it) |
| `adapter.go` | `SnapshotAdapter` | **you** (one per snapshot type) |
| `volumecapture.go` | `VolumeCaptureProvider` | SDK by default (`NewStorageFoundationProvider`) |
| `types.go` | `ChildSpec`, `VolumeCaptureSpec`, `ManifestCaptureSpec`, `NotReadyStatus`, `SourceRef`, `DomainCaptureState` | DTOs you pass to the verbs |

The interfaces are declared on the **consumer side** — at the *boundary*, i.e. on the
**integration seam** between the domain controller and the domain-neutral SDK — rather than
dumped into a single `interfaces.go`. This is deliberate: the layout encodes the architecture.

## What the domain controller does: decide three things + raise the barrier

For each snapshot node the domain controller determines three things:

- which **manifests** to save → `EnsureManifestCapture`;
- whether to save **PVC data** (0 or 1) → `EnsureVolumeCapture`;
- which **child snapshots** are needed (0..N) → `EnsureChildren`.

In the canonical model this is `manifest capture + data capture of a single PVC (0..1) + child
snapshots (0..N)`. The controller expresses intent for each of them and finally lowers the
barrier:

1. **Child snapshots** (`EnsureChildren`) — e.g. a VM snapshot owns the snapshots of its disks.
2. **Data capture** (`EnsureVolumeCapture`) — capture the contents of a **single** PVC (see the
   `DataRef` section).
3. **Manifest capture** (`EnsureManifestCapture`) — capture the source manifest (+ the owned PVC
   from data capture).
4. **Barrier** (`MarkPlanningReady`) — "everything is planned"; the core controller waits for
   exactly this before it takes over the `SnapshotContent`.

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
`s.Status.VolumeCaptureRequestName` directly, it would have to import every domain CRD — and it
would stop being generic. The adapter inverts the dependency: **the domain depends on the SDK,
not the other way around.** The SDK knows only the interface; the "generic concept → concrete
status field" mapping lives in the domain.

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
	Object() client.Object                     // live object; the SDK patches it
	SourceRef() SourceRef                       // spec.sourceRef
	GetConditions() []metav1.Condition          // status.conditions (read; planning barrier)
	SetConditions([]metav1.Condition)           // status.conditions (write)
	GetDomainCaptureState() DomainCaptureState  // durable planning result (read)
	SetDomainCaptureState(DomainCaptureState)   // durable planning result (write)
}
```

Contract rules:
- **No side effects.** No API calls in the methods — only reading/writing struct fields. All
  cluster access is done by the SDK.
- `Object()` returns the **same pointer** that the other methods read/write (the same `s`).
- This is the **only place** where `client.Object` and `metav1.Condition` cross the domain↔SDK
  boundary. In the body of `Reconcile` you do **not** call these methods directly — only
  `Ensure*` / `Mark*`.

A 1:1 template — `internal/controllers/demo/snapshot_adapter.go`.

## Step 2 — build the `CaptureSDK` (once per reconciler)

```go
func (r *MySnapshotReconciler) capture() snapshotsdk.CaptureSDK {
	return snapshotsdk.New(r.Client, snapshotsdk.NewStorageFoundationProvider(r.Client))
}
```

- `Client` — writes and cache reads.
- `VolumeCaptureProvider` — the data-capture backend; the default is the storage-foundation
  `VolumeCaptureRequest`.

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

	adapter := myDomainSnapshotAdapter{snap: s} // ← this is the whole "get the adapter"
	sdk := r.capture()

	// 1. Source validation — your logic. Invalid / not found → Ready=False and return.
	if /* source invalid / not found */ {
		return ctrl.Result{}, sdk.MarkNotReady(ctx, adapter, snapshotsdk.NotReadyStatus{Reason: ..., Message: ...})
	}

	// 2. Children (a leaf with no children → nil).
	if err := sdk.EnsureChildren(ctx, adapter, childSpecs); err != nil {
		return ctrl.Result{}, err
	}

	// 3. Data capture (no PVC → DataRef: nil = no-op; artifact not there yet → MarkNotReady +
	//    requeue via ctrl.Result, see below).
	if err := sdk.EnsureVolumeCapture(ctx, adapter, snapshotsdk.VolumeCaptureSpec{DataRef: dataRef}); err != nil {
		return ctrl.Result{}, err
	}

	// 4. Manifest (always).
	if err := sdk.EnsureManifestCapture(ctx, adapter, snapshotsdk.ManifestCaptureSpec{
		Targets: manifestTargets,
	}); err != nil {
		return ctrl.Result{}, err
	}

	// 5. Barrier — last.
	return ctrl.Result{}, sdk.MarkPlanningReady(ctx, adapter, "planning complete")
}
```

Order: the planning calls (`EnsureChildren` / `EnsureVolumeCapture` / `EnsureManifestCapture`) may
run in any order relative to each other, but **`MarkPlanningReady` is always last**. On an error
from any `Ensure*`, just `return err` and the reconcile retries (drift errors are additionally
mapped to `MarkPlanningFailed`, see the sections below).

---

## `manifestTargets` — which manifests end up in one MCR

`EnsureManifestCapture(ctx, adapter, ManifestCaptureSpec{Targets: ...})` takes the **full desired
set** of manifest objects that the domain controller considers to belong to this snapshot node.
The SDK turns this list into **one** `ManifestCaptureRequest`.

```go
manifestTargets := []snapshotsdk.ManifestTarget{{
	APIVersion: demov1alpha1.SchemeGroupVersion.String(),
	Kind:       "DemoVirtualDisk",
	Name:       source.Name,
}}
```

If the domain decides that additional namespaced objects should be captured alongside the main
source object, it adds them to this same list. After capture these objects end up in this node's
MCP, and the root/parent capture can exclude them from its own MCR through the existing subtree
exclude mechanism.

The SDK does not decide for the domain which domain manifests belong to the node. It is only
responsible for the transport mechanics: create/verify one MCR, set the ownerRef, publish
`status.manifestCaptureRequestName`, preserve restart-safe behavior, and when needed add the
technical owned-PVC target from data capture.

### Manifest capture — fail-closed on drift (symmetric with children/data)

After the first publish of the MCR its target set is **immutable**, the same way the children
topology and the data slot are. If on a later reconcile the desired target set diverges from the
already published MCR (comparison of **sets** by `(apiVersion, kind, name)`; order and duplicates
do not matter), `EnsureManifestCapture` returns `snapshotsdk.ErrManifestDrift`: it does **not**
patch/recreate/delete the MCR and does not touch status. The domain publishes the outcome via
`MarkPlanningFailed(ReasonManifestDrift)`:

```go
if err := sdk.EnsureManifestCapture(ctx, adapter, snapshotsdk.ManifestCaptureSpec{Targets: manifestTargets}); err != nil {
	if errors.Is(err, snapshotsdk.ErrManifestDrift) {
		_ = sdk.MarkPlanningFailed(ctx, adapter, snapshotsdk.Reason(storagev1alpha1.ReasonManifestDrift), err)
	}
	return ctrl.Result{}, err
}
```

The technical owned-PVC target (from data capture) is added to the desired set **before** the
comparison, so it does not cause a false-positive drift.

> ⚠️ **Manifest capture cannot be empty.** The final target set (your targets + the owned-PVC
> augmentation from data capture) must contain **at least one** target. Nuances:
> - the domain **may** pass an empty initial `Targets` set;
> - the owned-PVC augmentation **may** make the final set valid (≥1) even with an empty input;
> - the SDK checks the **final** set; if it is empty, `EnsureManifestCapture` returns
>   `snapshotsdk.ErrEmptyManifest` **before** any cluster access (no MCR is created, status is not
>   touched).
>
> The SDK does **not** substitute the snapshotted resource for you — passing at least one manifest
> target (at minimum the resource itself) is the domain's responsibility. An empty
> `ErrEmptyManifest` is a signal of a planning bug in the controller, not a transient state.

---

## `childSpecs` — what it is and how to build it

`EnsureChildren(ctx, adapter, desired []ChildSpec)` takes the **desired set** of child snapshots.

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
- derives the child `SnapshotChildRef` from GVK+name and publishes the set to
  `status.childrenSnapshotRefs` (via `Set/GetDomainCaptureState`).

The SDK **never invents** the child's domain spec fields — you are the only one who does that.

### How to build it (example: VM → disk snapshots)

```go
var childSpecs []snapshotsdk.ChildSpec
for _, diskName := range vm.Spec.DiskNames {
	child := &demov1alpha1.DemoVirtualDiskSnapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, Name: childSnapshotName(s, diskName)},
		Spec: demov1alpha1.DemoVirtualDiskSnapshotSpec{
			SourceRef: demov1alpha1.SourceRef{Kind: "DemoVirtualDisk", Name: diskName},
		},
	}
	childSpecs = append(childSpecs, snapshotsdk.ChildSpec{Object: child})
}
```

### Important `EnsureChildren` invariants

- **Full desired set.** You pass the full target set of children every reconcile (not an
  increment). A duplicate in the set (two specs with the same `(apiVersion, kind, name)`) is a
  planning error and `EnsureChildren` fails (this is not drift).
- **Delete-free (SDK v1, R23).** The SDK only creates/adopts and publishes refs. It does **not**
  delete child objects.
- **Topology is immutable after the barrier commit (fail-closed).** The commit marker is
  `ChildrenSnapshotReady=True` (set by `MarkPlanningReady`), not "refs are non-empty". **Before**
  the commit the set may still converge to a freshly observed desired. **After** the commit the
  desired must **match** the published one by identity `(apiVersion, kind, name)` — comparison of
  **sets, not length** (`[A,B] → [A,C]` at equal count is also a divergence; a committed leaf
  `[] → [A]` is too). If the set diverges (e.g. after a restart discovery saw a different set),
  `EnsureChildren` returns `snapshotsdk.ErrTopologyDrift`, creates nothing, and does not touch the
  published refs. Handle it like this:
  ```go
  if err := sdk.EnsureChildren(ctx, adapter, childSpecs); err != nil {
  	reason := snapshotsdk.Reason(storagev1alpha1.ReasonCreateChildFailed)
  	if errors.Is(err, snapshotsdk.ErrTopologyDrift) {
  		reason = snapshotsdk.Reason(storagev1alpha1.ReasonTopologyDrift)
  	}
  	_ = sdk.MarkPlanningFailed(ctx, adapter, reason, err)
  	return ctrl.Result{}, err
  }
  ```
- A `nil`/empty set is valid before the commit (a leaf with no child snapshots publishes empty
  refs); after committing a non-empty set, `nil` is drift, as is the appearance of a new child
  object on a committed empty leaf.
- Child names must be **deterministic** (the same name for the same logical child), otherwise a
  repeated reconcile will spawn duplicates.

Reference: `virtualmachinesnapshot_controller.go` (a parent with children).

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

The domain finds its own PVC and makes its own readiness decisions (no PVC → `MarkNotReady` with
`ArtifactMissing`, and the domain arranges the re-check itself via `ctrl.Result`). From the
`DataRef` the SDK creates a storage-foundation `VolumeCaptureRequest` (VCR), publishes its name in
`status.volumeCaptureRequestName`, and later mixes the owned PVC into the manifest capture.

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
`api/storage/v1alpha1` `SnapshotContent.status.dataRef` — it too is a `*SnapshotDataBinding`, a
single pointer). A snapshot node may have **at most one** data ref (one PVC) or none. Several
different data artifacts on one node are **not** part of the model — if the domain has several
disks, that is several **child** snapshots (each with its single PVC), not several refs on one.

That is why the field is a single pointer, not a slice:

```go
type VolumeCaptureSpec struct {
	DataRef *Target // one PVC, or nil
}
```

- **`DataRef == nil`** → a manifest-only snapshot: the SDK does not create a VCR and does not
  publish a name (an explicit no-op).
- **`DataRef != nil`** → a normal data capture of a single PVC.
- In the demo, `resolveDemoVirtualDiskDataRef` returns `*snapshotsdk.Target` (a PVC) or `nil`.

> A `[]Target` slice is impossible here **by design**: the type itself forbids "several data
> captures on one snapshot". PVC multiplicity is expressed only through child nodes. The only
> place a list of targets actually exists is the unstructured wrapper over the foundation CRD
> `VolumeCaptureRequest` (`spec.targets[]`) inside `internal/storagefoundation`; the SDK always
> puts exactly one element there.

### How to build it (example: disk → its PVC)

```go
pvcName := source.Spec.PersistentVolumeClaimName
if pvcName == "" {
	// manifest-only disk: data is not captured
	return nil // DataRef stays nil
}
pvc := &corev1.PersistentVolumeClaim{}
if err := reader.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: pvcName}, pvc); err != nil {
	if apierrors.IsNotFound(err) {
		// the artifact may appear later → surface Ready=False (via MarkNotReady); the requeue is
		// done by the controller via ctrl.Result{RequeueAfter: ...}, NOT an error
		return /* MarkNotReady{Reason: ArtifactMissing} + ctrl.Result{RequeueAfter: ...} */
	}
	return err
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

## Failure / not-ready paths

- `MarkNotReady(ctx, adapter, NotReadyStatus{Reason, Message, Cause})` — publishes `Ready=False`
  (invalid source, an artifact that has not appeared yet). The SDK only publishes the condition;
  whether and when to retry is decided by the controller via its `ctrl.Result` (the SDK does not
  drive the reconcile loop).
- `MarkPlanningFailed(ctx, adapter, reason, cause)` — planning is blocked (by a domain reason, by
  `ReasonTopologyDrift` on child topology divergence, or by `ReasonManifestDrift` on manifest
  target divergence); it writes the barrier condition to `False` (instead of `MarkPlanningReady`).
- The difference: `MarkNotReady` is about the **source/artifact**; `MarkPlanningFailed` is about
  the **planning barrier** itself.

## Guarantees you can rely on

- **Idempotency / restart-safe.** Any `Ensure*` can be called every reconcile; a repeated call
  breaks nothing and creates no duplicates (deterministic VCR/MCR/child names).
- **Suppression by the planning barrier.** After you call `MarkPlanningReady`
  (`ChildrenSnapshotReady=True`), every `Ensure*` becomes a no-op — the SDK creates nothing,
  reuses nothing, and validates nothing (the planning phase is frozen, ownership has passed to the
  core controller). If a request is later deleted (e.g. by TTL after the durable handoff), the SDK
  does **not** recreate it. Before the barrier, per-artifact immutability applies: an
  already-published artifact that diverges from the desired is drift (fail-closed), not a silent
  overwrite.
- **The domain/SDK boundary.** The domain owns: source validation, child planning, domain objects.
  The SDK owns: conditions, ownerRefs, capture orchestration, request lifecycle.

## Where to start in practice

Take the demo implementation as a starting point and adapt it to your type:
1. `internal/controllers/demo/snapshot_adapter.go` — the adapter;
2. `virtualdisksnapshot_controller.go` (a leaf with PVC data capture) **or**
   `virtualmachinesnapshot_controller.go` (a parent with children, manifest-only) — the reconcile
   skeleton.

This is the reference implementation: the demo controllers are deliberately kept as executable
documentation of the SDK.
