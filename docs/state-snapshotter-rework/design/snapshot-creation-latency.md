# Snapshot-creation latency

Single, self-contained document for the snapshot-creation latency work. It answers three questions:

- **What to apply** — the validated fixes, file-by-file, as a re-application guide (not a patch / cherry-pick).
- **What was proven** — confirmed root causes, confirmed-but-secondary optimizations, and architecture/correctness
  cleanups.
- **What did not work** — rejected hypotheses (so nobody re-runs them) and the issues still open.

Repos: `state-snapshotter` (controller + demo domain-controller) and `storage-foundation` (VCR controller).
`storage-e2e` is measurement tooling only — see [Tooling](#10-tooling).

> This is the single, authoritative latency document. Any earlier separate "fixes/re-apply" and
> "findings/classification" notes have been consolidated here and removed.

---

## 1. Purpose / scope

The investigation went from ~10 hypotheses to **3 confirmed bottlenecks**, several correct-but-secondary
optimizations, one architecture/correctness cleanup, several rejected hypotheses, and a child-graph-planning cost
(H3) that is now **closed** by Commit D.3 (H1 was disproven as a distinct problem and had been absorbed into H3).
The remaining SETS=10 tail is a **new, narrower question** — the root manifest leg (`ChildrenSnapshotReady` →
`Ready` ≈10s) — which is a separate investigation, not a child-graph fix.

Sections 4–6 are a **re-application guide**: apply by hand on a fresh `main`; section order is the recommended
application order. Sections 3, 7, 8 are the **classification** (why each change matters, what was disproven, what
remains). **Do not claim snapshot scalability is solved** — see section 8.

## 2. Current validated state

Two distinct benchmarks — **do not conflate them**:

- **Parallel same-shape snapshot burst** (N independent trees of the same shape): SET=1 ~3s, TREES=5 ~6s.
- **Namespace fan-out benchmark** (one root Snapshot over N independent standard sets): after Commit D.3,
  **SETS=10 ROOT Ready ~23–24s client-measured** (3 warm runs: 23.4 / 23.7 / 24.2s), down from ~29–30s. Still
  above target, but the bottleneck has moved (see below).

CPU is no longer the bottleneck (genericbinder mapper fix; controller mostly idle during fan-out), the
reverse-watch mapper `List` is gone, the rate limiter is fixed, polling is largely event-driven, and **child-graph
planning cost is closed** (D.3: per-child `Get`s → one `List`/GVK; worst pass 11.1s → ~0.8–1.1s). The remaining
SETS=10 tail is **latency-bound** and now concentrated in the **root manifest leg**: after all children are ready
(`ChildrenSnapshotReady` ~13s) the root Snapshot still takes ~10s to reach `Ready` (`ManifestsArchived`/`Ready`
~24s). That leg is the **next** open investigation — a new hypothesis, not a child-graph fix.

## 3. Confirmed bottlenecks (real root causes, measurable effect)

| # | Root cause | Evidence / effect |
|---|---|---|
| B1 | **client-go rate limiter QPS=5 / Burst=10** on the shared manager client serialized uncached reads + status patches under a multi-tree burst, inflating one reconcile to 4–15s **regardless of `MaxConcurrentReconciles`**. | Raising to 50/100: TREES=5 **57s→6s**, TREES=1 **15s→3s**; reconcile durMs mean 4491→125ms, max 14812→847ms. The dominant tail. Fix = [FIX 1](#fix-1-do-first--raise-manager-client-qpsburst-to-50100). |
| B2 | **500ms self-requeue "poll handshakes" instead of watches** between controllers (MCR↔ManifestCheckpoint, bottom-up `ManifestsArchived` latch, root-MCR planning, VCR↔VolumeSnapshotContent). | Converting to event-driven wakes removed structural per-hop latency (e.g. archive-latch last-child→root ~12s→2.4s). Correct independent of wall-clock. Fixes = [FIX 2](#fix-2--mcr-controller-watch-manifestcheckpoint), [FIX 3](#fix-3--snapshotcontent-two-reverse-lookup-watches), [FIX 5](#fix-5--storage-foundation-vcr-watch-volumesnapshotcontent). |
| B3 | **genericbinder reverse-watch `List`+decode CPU/alloc** — three map functions each did a full unstructured `List` + JSON decode per event (O(#snapshots/#contents)). | pprof: ~69% CPU / ~84% alloc, grows with tree size. Direct-ref O(1) routing → **33.5s→29.2s**. A real scaling liability (load fix), though **not** the dominant wall-clock term at SETS=10. Fix = [FIX 8](#fix-8--genericbinder-reverse-watch-mappers-direct-ref-o1-routing). |

`MaxConcurrentReconciles=1` (implicit in several controllers) was a ceiling once B1 was fixed; raising it needs
one real correctness fix (shared `Config` mutation under concurrent reconciles) — see [FIX 6](#fix-6--concurrency-ceilings--the-one-required-correctness-fix).

---

## 4. Validated fixes to re-apply

### FIX 1 (do first) — raise manager client QPS/Burst to 50/100

Root cause B1. In each `main.go`, right after the `rest.Config` is created and **before** `manager.New`, set:

```go
kConfig.QPS = 50
kConfig.Burst = 100
```

| repo | file | anchor |
|---|---|---|
| state-snapshotter | `images/state-snapshotter-controller/cmd/main.go` | after config built, before scheme/manager (`log.Info("[main] kubernetes config ... created")`) |
| state-snapshotter | `images/domain-controller/cmd/domain-controller/main.go` | same anchor (`[domain-main] kubernetes config ... created`) |
| storage-foundation | `images/controller/cmd/main.go` | same anchor (before `apiruntime.NewScheme()`) |

Why: the decisive fix. Precedent in-repo: the capture path already used QPS 100 / Burst 200 on its own clients.
The domain-controller is the demo **planning** layer; note in a comment that a production domain controller must
set its own QPS (not a contract).

Validated: TREES=5 57s→6s; TREES=1 15s→3s; reconcile durMs mean 4491→125ms, max 14812→847ms.

Caveat: **QPS=50/100 is capacity tuning, not proof of low work.** It does not reduce work; it stops the default
limiter from serializing it. Validated by flat reconciles-per-tree and no post-Ready storm — not by showing the
work is small. Pick a production value deliberately.

### FIX 2 — MCR controller: watch ManifestCheckpoint

Root cause B2. File `images/state-snapshotter-controller/internal/controllers/manifestcapture/checkpoint_controller.go`.

1. Add a mapper `mapManifestCheckpointToMCR(ctx, obj) []reconcile.Request` that reads
   `checkpoint.spec.manifestCaptureRequestRef` (name+namespace) and enqueues that MCR. (`Owns()` cannot route it
   because the checkpoint is owned by the execution ObjectKeeper, not the MCR.)
2. In `SetupWithManager`, add to the builder:
   ```go
   .Watches(&storagev1alpha1.ManifestCheckpoint{}, handler.EnqueueRequestsFromMapFunc(mapManifestCheckpointToMCR))
   ```

Validated: clean tree 10–17s → 5–7s. Apply [FIX 6](#fix-6--concurrency-ceilings--the-one-required-correctness-fix) (concurrency + `configMu`) to this same file at the same time.

### FIX 3 — SnapshotContent: two reverse-lookup watches (pre-adoption MCP wake + event-driven archive latch)

Root cause B2. File `images/state-snapshotter-controller/internal/controllers/snapshotcontent/controller.go`.
Drives both the manifest-leg wake before ownerRef adoption (L9a) and the bottom-up archive latch (C-2).

Register cache field indexes (per content GVK, in the setup that registers each GVK):

```go
const indexKeyManifestCheckpointName = ".status.manifestCheckpointName"
const indexKeyChildContentName       = ".status.childrenSnapshotContentRefs.name"
```

- `extractManifestCheckpointNameIndex` → projects `status.manifestCheckpointName`.
- `extractChildContentNamesIndex` → projects every `status.childrenSnapshotContentRefs[].name`.
- Register both with `mgr.GetFieldIndexer().IndexField(...)` for each content GVK.

Add two mappers + watches (keep the existing ownerRef-based watches as dual-path backstop):

1. **L9a pre-adoption MCP wake** — `mapManifestCheckpointToContent` uses `lookupContentsByManifestCheckpointName`
   (List by `indexKeyManifestCheckpointName`) so a ManifestCheckpoint event wakes the content whose
   `status.manifestCheckpointName` matches, even before ownerRef adoption. Wire into `addArtifactWakeUpWatches`
   alongside the ownerRef mapper (`ownerRefToContentRequests`).
2. **C-2 event-driven archive latch** — `mapChildContentToParentContentsByEdge` lists parents by
   `indexKeyChildContentName` = this content's name and enqueues them:
   ```go
   .Watches(obj, handler.EnqueueRequestsFromMapFunc(mapSnapshotContentToParentContent))        // existing ownerRef path
   .Watches(obj, handler.EnqueueRequestsFromMapFunc(r.mapChildContentToParentContentsByEdge))  // new forward-edge path
   ```

Keep `MaxConcurrentReconciles: 8` (controller options) and the 500ms self-requeue backstop. No status contract
changes. Validated (C-2): archive-latch gap last-child→root ~12s → ~2.4s.

> This is also the file changed by [ARCH 2 (H2)](#arch-2--single-snapshotcontent-controller-with-dynamic-snapshot-status-watches-h2): `SetupWithManager` uses `builder.Build(r)` (not
> `Complete(r)`) to retain the primary controller handle, so snapshot-status watches attach to it dynamically
> instead of building a second per-GVK controller.

### FIX 5 — storage-foundation VCR: watch VolumeSnapshotContent (event-driven data leg)

Root cause B2. Files under `images/controller/internal/controllers/`.

1. `constants.go`: add label key `LabelKeyVCRNamespaceFull = "storage.deckhouse.io/vcr-namespace"` (a `vcr-name`
   label already exists).
2. At VSC creation (`volumecapturerequest_snapshot_bulk.go`): stamp both `LabelKeyVCRNameFull` and
   `LabelKeyVCRNamespaceFull` on the CSI VolumeSnapshotContent so it carries its owning VCR coordinates.
3. `volumecapturerequest_controller.go`: add mapper `mapVolumeSnapshotContentToVCR(ctx, obj)` that reads those
   two labels and enqueues that VCR; wire into `SetupWithManager`:
   ```go
   .Watches(&snapshotv1.VolumeSnapshotContent{}, handler.EnqueueRequestsFromMapFunc(mapVolumeSnapshotContentToVCR))
   ```
   Also apply [FIX 6](#fix-6--concurrency-ceilings--the-one-required-correctness-fix) concurrency here.

Keep the 5s requeue as a safety net (covers VSCs created before the label existed).

### FIX 6 — concurrency ceilings + the one required correctness fix

Raise `MaxConcurrentReconciles` (conservative 4) on the controllers that were implicitly 1, and add the `Config`
race guard.

| repo | file | change |
|---|---|---|
| state-snapshotter | `genericbinder/controller.go` | `MaxConcurrentReconciles: 4` **+ RateLimiter** `NewTypedItemExponentialFailureRateLimiter[ctrl.Request](200ms, 10s)` (via `genericBinderControllerOptions()`) |
| state-snapshotter | `manifestcapture/checkpoint_controller.go` | `MaxConcurrentReconciles: 4` **plus** the `configMu` guard below (no RateLimiter here) |
| state-snapshotter | `domain-controller/.../demo/virtualmachinesnapshot_controller.go`, `.../virtualdisksnapshot_controller.go` | `MaxConcurrentReconciles: 4` (snapshot demo controllers only — **not** the VM/Disk lifecycle controllers) |
| storage-foundation | `volumecapturerequest_controller.go` | `MaxConcurrentReconciles: 4` + `RateLimiter: NewTypedItemExponentialFailureRateLimiter[ctrl.Request](200ms, 10s)` |

**Required correctness fix (`checkpoint_controller.go`):** `loadConfigFromConfigMap` rewrites shared `Config`
fields (`MaxChunkSizeBytes`, `DefaultTTL`, `DefaultTTLStr`) on every reconcile — a data race once concurrency
> 1. Guard them with a mutex `configMu` behind accessors (`cfgMaxChunkSizeBytes`, `cfgDefaultTTL`,
`cfgDefaultTTLStr`) and snapshot the config once per reconcile before use. Do not raise concurrency here without
this guard.

Full concurrency picture: genericbinder 4, checkpoint 4, foundation VCR 4, demo VMS/VDS 4, SnapshotContent 8.
These did not move the wall on their own (the gate was downstream each time) but remove the ceiling and are
prerequisites for FIX 2–5 to run correctly under load. Start at 4, not 8.

### FIX 8 — genericbinder reverse-watch mappers: direct-ref O(1) routing

Root cause B3 (**load/throughput fix, not the wall-clock fix**). Files under
`images/state-snapshotter-controller/internal/controllers/genericbinder/`.

The three reverse-watch map functions each did a full `unstructuredClient.List` of a GVK + JSON decode of every
object, then filtered to the one match — O(#snapshots/#contents) work + allocations **per event**. Replace with
the references already on the event object (no `List`, no decode):

| file | mapper | change |
|---|---|---|
| `content_watch.go` | `mapBoundContentToSnapshots` | read `content.spec.snapshotRef`, enqueue it directly (O(1)) |
| `content_watch.go` | `mapParentContentToChildSnapshots` | `Get` the owning child Snapshot (from `content.spec.snapshotRef`), enqueue the parents it lists in `status.childrenSnapshotRefs` |
| `mcr_watch.go` | `mapMCRToOwningSnapshots` | walk `obj.GetOwnerReferences()` for the matching Kind/APIVersion, enqueue the owner |

Update `controller.go` watch registrations to the standalone (no-`r.`) mapper signatures where applicable. No
reconcile-contract or status changes; no field index needed (the direct references are the index). Optional
diagnostics shipped alongside (off by default): per-mapper atomic counters (`watch_map_stats.go`, env
`STATE_SNAPSHOTTER_WATCH_MAP_STATS`) and controller-runtime metrics on `:8080` in `cmd/main.go`.

Validated (SETS=10, post-deploy): the two `List`-based mappers **disappear** from the CPU profile; watch-path
`unstructuredClient.List` drops to ~1%; controller ~73% idle during fan-out; reconciles bounded (~3–5 / object),
0 errors, no post-Ready storm. ROOT Ready ~33.5s → **~29.2s** (−13%): a real scaling liability but **not** the
dominant wall-clock term.

---

## 5. Architectural / correctness improvements

These are correct event-driven / single-owner changes worth keeping. They are **not** headline latency
root-cause fixes; apply them as architecture, not as wall-clock levers.

### ARCH 1 — Snapshot controller: wake the gated parent on child-content archive

File `images/state-snapshotter-controller/internal/controllers/snapshot/content_watch.go`, handler
`snapshotContentToSnapshotEnqueueHandler`. Today it wakes only the **bound owner** Snapshot. Also wake the
**gated parent(s)** — the Snapshot whose root-MCR gate (`usecase.requireContentManifestsArchived`) reads this
child content's archive latch — on the `ManifestsArchived` False→True transition:

- `UpdateFunc`: `archivedTransition := !archived(old) && archived(new)`; when true, additionally enqueue gated
  parents.
- `CreateFunc`: enqueue gated parents when the created content is already `ManifestsArchived=True`
  (resync/restart).
- Gated-parent resolution: `content.spec.snapshotRef` (owning child Snapshot S) →
  `findParentsReferencingChildSnapshot(S)` (Snapshots listing S in `status.childrenSnapshotRefs`,
  namespace-local). Helper: `gatedParentRequestsFromContent`.
- **Dedup** bound-owner + gated-parent requests by `NamespacedName` within one event
  (`enqueueContentDrivenSnapshots`).

Do not weaken the gate (`requireContentManifestsArchived` / `BuildRootNamespaceManifestCaptureTargets`); keep the
500ms backstop. A root's own content maps to no parent (no self-wake cycle). Reclassified: earlier looked like a
root-MCR latency fix, but the root MCR was not gated on this wake (Commit B/C decomposition). Validated (wake
path only): root MCR created ~30–31s → ~24.4s; ROOT Ready ~37s → ~33.5s — confirms the path fires; does **not**
close the SETS=10 tail.

### ARCH 2 — single SnapshotContent controller with dynamic snapshot-status watches (H2)

**Status: implemented and validated. Architecture/correctness cleanup, latency-neutral.**

Problem (was open hypothesis H2): a single `SnapshotContent` UID was reconciled by **two distinct
controller-runtime Controllers** — the primary `For(SnapshotContent)` controller
(`snapshotcontent-storage.deckhouse.io-SnapshotContent`) and a per-snapshot-GVK snapshot-status watch controller
(`snapshotcontent-snapshot-<gvk>`), each registered `.Complete(r)` with the **same** reconciler. Both ran the
full reconcile and both patched status → concurrent same-key processing and 409 conflict churn.

Fix, file `images/state-snapshotter-controller/internal/controllers/snapshotcontent/controller.go`:

- `SetupWithManager` / `AddWatchForContent` call `built, err := builder.Build(r)` (instead of `Complete(r)`) and,
  for the common-SnapshotContent GVK, retain the handle in `r.primaryContentController`. `Complete` is exactly
  `Build` with the handle discarded, so nothing else changes.
- `addSnapshotStatusWatchLocked` attaches each runtime-discovered snapshot GVK as an **additional event source on
  the single primary controller** via `r.primaryContentController.Watch(source.Kind(...,
  mapSnapshotStatusToBoundCommonContent))`, instead of building a second Controller. `Controller.Watch` accepts
  sources before or after `Start`, so this preserves the registry-driven / dynamic-CSD activation model.
- The snapshot-status wake is **necessary** and kept: the reconciler reads the owning Snapshot's
  `status.childrenSnapshotRefs` (authoritative declared child set) via `APIReader` to evaluate the
  `ManifestsArchived` latch, so a Snapshot `status.boundSnapshotContentName` change must still wake the bound
  content. `activeSnapshotWatchSet` dedups repeat registrations.

Validated (SETS=10 ×3 + SETS=1, post-deploy):

- **Topology:** exactly one `snapshotcontent-*` controller in startup logs and in the
  `controller_runtime_reconcile_total` registry; no `snapshotcontent-snapshot-<gvk>` controller. (The
  `snapshot-demo…` controllers are domain *Snapshot* controllers — a different concern.)
- **Dynamic watches:** 3 discovered snapshot GVKs (`storage Snapshot`, `DemoVirtualDiskSnapshot`,
  `DemoVirtualMachineSnapshot`) attached to the single controller (8 EventSources total = 5 builder + 3
  snapshot-status); gauges `resolved=3, active=3, stale=0` (dedup works). Virtualization GVK skipped ("not in
  API").
- **Reconcile ownership:** every content reconcile (and every touch of the root content UID) comes from the
  single primary controller; zero from any per-GVK controller.
- **Conflicts:** PUT-409s ~20–27 per SETS=10 run, down from the duplicate-controller era's 34–167/run; the
  dual-writer same-object race is structurally eliminated. Residual 409s are transparent optimistic-lock retries
  on `Update`/finalizer writes (`reconcile_errors_total = 0`); no functional impact.
- **Latency:** SETS=10 ~25s server-side — unchanged within run-to-run noise (this is a cleanup, not a latency
  lever). No reconcile inflation (~477/run, at the pre-Commit-C baseline).
- **Regressions:** none observed in Ready propagation, delete/finalizer handling, retry, or the snapshot→content
  wake. Failure propagation is covered by the integration suite (not exercised by the benchmark).

---

## 6. Confirmed optimizations (correct and helpful, but NOT the bottleneck)

Keep these — they reduce work or are prerequisites — but do not credit them with solving scalability.

- **Commit B — skip child-graph replan after readiness:** 34s → ~28s (−18%) on SETS=10. An optimization, not a
  root-cause fix; the namespace fan-out tail remained.
- **T-cost — defer the expensive declared-child walk** to the only pass that can latch `ManifestsArchived=True`
  (`snapshotcontent/controller.go`, `aggregateChildrenManifestsArchived` takes `ownManifestReady bool`; defer the
  uncached `declaredNonLeafChildContentNames` walk until `ownManifestReady && no pending linked child`):
  observe-lag ~4.3s → ~3.1s.
- **APIReader audit** — cache only the three watched-object mirror reads; keep every correctness-critical uncached
  read (see [Appendix](#11-appendix--apireader-audit)). Hygiene, no regression.
- **Concurrency ceilings + `configMu` race fix** ([FIX 6](#fix-6--concurrency-ceilings--the-one-required-correctness-fix)) — mandatory correctness/prerequisite; no wall-clock move
  on its own (the gate was downstream each time).
- **MCP→MCR and VSC→VCR watches** ([FIX 2](#fix-2--mcr-controller-watch-manifestcheckpoint), [FIX 5](#fix-5--storage-foundation-vcr-watch-volumesnapshotcontent)) — correct event-driven architecture regardless of
  the measured win.
- Keep any per-reconcile SnapshotContent trace at **debug** level (diagnostics, not a fix).

---

## 7. Rejected hypotheses (checked — do not revisit)

| Hypothesis | Result | Why |
|---|---|---|
| **Commit C — VSC wake loss dominates leaf latency** | **False** | Dual-path VSC→content wake raised content reconciles (490 → 554/579/613, SETS=10 ×3) with **no** wall-clock improvement (staircase 20→18-19s, observe-lag 6/11→~5/9). Leaves are gated on `ManifestCapturePending`, not on the VSC wake; the cost-cut guard's precondition (manifest latched while volume pending) never holds for a leaf, so it cannot fire. Extra wakes just added load. |
| **genericbinder reverse `List` is the remaining wall-clock bottleneck** | **False as a wall-clock cause** | pprof confirmed it as a major CPU/alloc hotspot (kept as B3 / FIX 8), but removing it moved wall only 33.5s→29.2s. The residual tail is latency-bound, not CPU-bound. |
| **Archive latch is the remaining dominant tail** | **No (partially true earlier)** | The event-driven archive latch (C-2) correctly cut last-child→root ~12s→2.4s and is **kept**, but it is not the remaining dominant bottleneck at SETS=10. |
| **Repeated child-graph planning is the dominant root latency** | **Partially true** | Removing repeated planning (Commit B) gave ~18% wall, but did not eliminate the fan-out tail → not the dominant term. (Superseded by H3: the *pending-phase* re-plan, not the single MCR pass, is the remaining cost.) |
| **H1 leaf staircase is a distinct leaf-side bottleneck (`vscReady → leaf content Ready`)** | **False** | Per-leaf server-side + log trace: once created, a leaf latches fast/constant (MCP created→Ready ~0–1s, content Ready ~1–4s). The staircase is purely *delayed creation*, gated by the repeated root re-plan (H3). No distinct leaf problem. |
| **"Leaf-skip": skip child-graph planning for leaf snapshot GVKs** | **False (no-op)** | Premised on leaves running planning. `reconcileParentOwnedChildGraph` is registered `For(&storagev1alpha1.Snapshot{})` and runs **only** on the root; demo VM/disk snapshots are reconciled by the separate domain-controller. The "leaf `DemoVirtualDiskSnapshot` spent 10s planning" log lines were the **root** reconcile triggered via the `nss-chw` relay, mislabeled with the relay's inherited logger context (`"snapshot":{"name":"bench-root"}` on every one). Nothing to skip. |

---

## 8. Remaining open issues (open investigations)

State at SETS=10 (pre-D.3): **ROOT Ready ~25s server-side (~29–30s client-measured)**. CPU, mapper `List`, the
rate limiter, and most polling are **no longer** bottlenecks. The remaining tail is **latency-bound** and, per the
Commit-D audit below, sat in **repeated root child-graph planning** (H3). H1 is not a distinct hypothesis.
**After D.3 (see below): wall ~23–24s client-measured** and the child-graph-planning cost is effectively closed;
the residual tail has moved to the **root manifest leg** (ChildrenSnapshotReady ~13s → Ready ~24s).

- **H3 — repeated root child-graph planning (CLOSED by Commit D.3; kept for history).** Was the primary open
  bottleneck; the per-pass cost was attributed by the Commit-D instrumentation and then removed by D.3 (per-child
  `Get`s → one `List`/GVK): worst pass 11.1s → ~0.8–1.1s, the three `Get`-heavy sections dropped ~7×, wall
  ~29–30s → ~23–24s. The mechanism below remains true — the root is still re-reconciled many times and still
  re-plans each pending pass — but each pass is now cheap, so neither the per-pass cost nor the duplicate passes
  are worth optimizing further (see "no longer recommended" below). Original diagnosis, for the record:
  `reconcileParentOwnedChildGraph` runs **only** on the root unified
  `Snapshot` (it is registered `For(&storagev1alpha1.Snapshot{})`; demo VM/disk snapshots are reconciled by the
  separate domain-controller, not by this path). During the pending fan-out phase the root is re-reconciled
  **~36×/run** (SETS=10) — ~2/3 of those wakes come from the `nss-chw-*` child-watch relays firing on every child
  snapshot status change — and each pending pass re-runs the **full** O(N) plan (`childGraphReplanSkippable` only
  skips *after* `ChildrenSnapshotReady=Completed`). Measured passes reached **child-graph-planning ~10s** and
  root reconcile **~16–17.5s** end-to-end, growing with the child/coverage set. This repeated full re-plan — not
  any per-leaf step — is what delays child creation and produces the observed leaf-Ready staircase. The Commit-D
  instrumentation (below) has now **attributed the per-pass cost**: it is the **per-child `Get`-heavy sections**
  (`coverageWalk` + `ensureChildren` + `priorityReady`), **not** the source `List`s; and the wall is inflated by
  **both** cost-per-pass growth **and** concurrent duplicate re-plans (the relay calls `Reconcile` directly).

> **H1 (leaf staircase) is absorbed by H3 — not a separate problem.** Earlier framing (`vscReady → leaf content
> Ready` grows ~4→9–11s, suspected leaf-side MCP/worker contention) was disproven by a per-leaf server-side +
> log trace: once a leaf is *created*, its legs latch fast and roughly constant (MCP created→Ready ~0–1s,
> content Ready ~1–4s after). The staircase is entirely in **when each leaf is created**, and creation is gated
> by the repeated root re-plan (H3). There is no leaf-level child-graph planning to fix — see the rejected
> "leaf-skip" hypothesis in [section 7](#7-rejected-hypotheses-checked--do-not-revisit).

> **H2 is closed** — see [ARCH 2](#arch-2--single-snapshotcontent-controller-with-dynamic-snapshot-status-watches-h2). It was implemented and validated as an architecture/correctness cleanup
> (duplicate controller removed, single dynamic `Watch` on the primary controller, correctness OK, latency
> unchanged within noise). It is **not** a remaining latency lever.

### Commit D — instrument `reconcileParentOwnedChildGraph` (diagnosis before optimizing)

**Status: instrumentation applied and measured (SETS=10, warm).** `reconcileParentOwnedChildGraph` accumulates
per-section wall time (`childGraphPlanningTimings`) and logs one breakdown per pass (covering the hot "priority
layer pending" early return that dominates fan-out), at the same 150ms threshold as the caller's total:
`resolveMappings` · `listSources` (with `listCalls`/`sourceObjects`) · `coverageWalk`
(`IsCovered`/`ObservePlannedSnapshot` recursive `childrenSnapshotRefs` `Get`s) · `ensureChildren` (per-child
`Get`+`Create`/`Patch`) · `priorityReady` (per-child readiness `Get`) · `publish`. File
`images/state-snapshotter-controller/internal/controllers/snapshot/parent_graph.go` (diagnosis-only; no
behaviour, data-model, or status-contract change).

**Measured attribution (worst pass, totalMs=11144):** coverageWalk **4501** + ensureChildren **3396** +
priorityReady **3132** = ~11.0s (99%); listSources **41** (2 List calls / 30 source objects), resolveMappings
**72**, publish **0**. Across the 6 logged passes: coverageWalk **17.5s**, priorityReady **12.8s**,
ensureChildren **11.4s**, listSources **0.27s**, resolveMappings **0.18s**, publish **0.05s**.

Conclusions:

- **The cost is per-child `Get`s, not source `List`s.** The earlier "uncached dynamic `List`s / ~40k GET" framing
  was wrong on attribution: listing sources is ~40ms; the ~seconds are spent in the three sections that do a
  `Get` (and recursion) **per child** — the recursive coverage walk over `childrenSnapshotRefs`, the per-child
  ensure `Get`, and the per-child readiness `Get`. A single `List` of children would be far cheaper than N `Get`s.
- **Both axes are bad.** Cost-per-pass grows with N (coverageWalk 289→4685ms across passes) **and** passes are
  duplicated concurrently — pairs of passes with near-identical duration fire in the same second (e.g. 4261/4234
  at 12:10:45; 11134/11144 at 12:10:56) because the `nss-chw` relay calls `r.main.Reconcile` **directly** (not
  via the workqueue), so concurrent child events spawn concurrent full re-plans of the same root (~42s of planning
  work packed into ~26s wall).

Fixes, one variable at a time, guided by the numbers:

- **D.3 — collapse per-child `Get`s into one `List` per child GVK (implemented and validated, SETS=10 warm).**
  `parent_graph.go` only. A per-pass `childSnapshotReadCache` lazily lists each child snapshot GVK once (via
  `r.Client` — the **same** client the former per-item `Get`s used, so the source of truth is identical) and
  serves the coverage walk, the ensure existence check, and the per-child readiness check from that map. No
  status-contract, membership-skip, debounce, or coverage-invariant change. Guardrail: all three reads were
  already `r.Client` (cached), **not** intentionally-uncached `APIReader`, so this is purely N `Get`s → 1 `List`.
  **Measured (SETS=10 warm, 3 runs after redeploy):** the three `Get`-heavy sections collapsed exactly as
  predicted — worst child-graph pass **11144ms → 788–1089ms** (target <3–4s met); section sums per run dropped
  coverageWalk **17.5s → ~2.5s**, ensureChildren **11.4s → ~2.3–2.6s**, priorityReady **12.8s → ~0.01–0.02s**
  (near-eliminated); per-pass child reads went from N per-child `Get`s to **≤2 `List`/pass** (`childListCalls`).
  Wall (client) **~29–30s → 23.4/23.7/24.2s**; ROOT Ready correctness unchanged (20 children, 20 leaves Ready
  every run). Risk-3 (stale per-pass list) did **not** materialize: wall dropped rather than grew, so newly-created
  children were not pushed into an extra pass in a way that cost latency. Residual wall is now dominated by the
  root manifest leg (ChildrenSnapshotReady ~13s → Ready ~24s) and leaf-creation cadence, **not** child-graph
  planning — H3's planning cost is effectively closed; the remaining tail moves to the manifest leg.
  Pre-deploy risk review (checked in code, not just claimed):
  - **List-GVK convention** — the cache builds an `UnstructuredList` with `Kind: gvk.Kind+"List"` and lists via
    `r.Client`. Precedent: `snapshotcontent/controller.go` already does exactly this against the module's snapshot
    GVKs in production, and the former coverage/ensure/readiness `r.Client.Get`s on the same GVKs worked, so the
    cache has informers and a RESTMapping. Not a new risk.
  - **Namespace scope** — the cache lists a single namespace (the root Snapshot's). A `Get` with a differently
    namespaced key now returns a **hard error**, not a misleading `NotFound` (guard + `TestChildSnapshotReadCache`).
    Correct today because the whole run tree is namespace-local to the root; the guard prevents a future footgun.
  - **Stale list within one pass** — the list is taken once per pass, so a child created earlier in the *same*
    pass by `ensureChildren` is not visible to the later `priorityReady` in that pass. This is **latency-safe**: a
    freshly-created child has not run its own reconcile, so it is **not** `ChildrenSnapshotReady` regardless of
    whether the read sees it (`NotFound`) or sees it (present-but-not-ready) — either way the layer stays *pending*
    and requeues; the next pass (woken by the child event) re-lists fresh. No extra pass is added versus the former
    post-`Create` `Get`. If, contrary to this reasoning, the wall does **not** drop or grows after deploy, this is
    the first thing to re-check.
- **D.2 — NO LONGER RECOMMENDED (premise gone).** Was: avoid the walk when child *membership* is unchanged. It
  only mattered while a pass was expensive; after D.3 a full pass is ~0.8–1.1s and the walk is ~2.5s summed across
  all passes, so the invariant-proof risk of a membership-skip is not justified by the remaining cost.
- **D.1′ — NO LONGER RECOMMENDED (premise gone).** Was: coalesce/dedupe the concurrent relay-driven root re-plans.
  Duplicate passes only hurt because each pass was expensive; now that passes are cheap, changing event-delivery
  semantics (a debounce can hide a lost event) is not worth the risk for the remaining latency.

Adjacent future work: choose the production manager client QPS/Burst deliberately (50/100 was capacity tuning, not
proof of low work — see FIX 1 caveat).

### H4 — root manifest leg (`ChildrenSnapshotReady` → `Ready` ≈10s) — NEW open investigation

With H3 closed, this is the **only** remaining SETS=10 latency issue and it is well-localised. After every child
is ready (`ChildrenSnapshotReady=True` at ~13s), the root Snapshot still takes ~10s to reach `Ready`
(`ManifestsArchived`/`Ready` at ~24s). New problem statement — **not** a child-graph fix:

> Why, once `ChildrenSnapshotReady=True`, does the root Snapshot take ~10s more to become `Ready`?

**Diagnosis (server-side trace of 3 warm SETS=10 runs + controller logs; no code change).** The 6 requested
sub-intervals (offsets from root create, `lastTransitionTime` second-granularity):

| boundary | r1 | r2 | r3 |
|---|---|---|---|
| ChildrenSnapshotReady (snap) | 13 | 10 | 12 |
| root MCP created | 22 | 19 | 87 |
| root MCP Ready | 23 | 19 | 87 |
| content ManifestsReady | 24 | 23 | 89 |
| ManifestsArchived (snap) | 25 | 24 | 89 |
| Ready (snap) | 26 | 26 | 89 |

- **Dominant interval = `ChildrenSnapshotReady → ManifestsArchived`** (r1 **12s**, r2 **14s**, r3 **77s**), inside
  which `ChildrenSnapshotReady → root MCP created` is the largest sub-part (~9s warm, ~75s in r3). **Interval 6
  (`ManifestsArchived → Ready`) ≈ 0–2s** on all runs — the root Ready mirror is not the problem.
- **Classification: lost wake → self-requeue/poll backstop** (not real capture work, not an expensive reconcile).
  Controller logs show the manifest-leg reverse watches erroring at runtime and dropping the wake:
  `field label not supported: .status.childrenSnapshotContentRefs.name` (**669×** in one window),
  `.status.manifestCheckpointName` (**10×**), `.status.dataRef.artifact.name` (**6×**), each followed by
  `self-requeue backstops` and `ManifestCheckpoint event resolved to no SnapshotContent … dropping`. The bottom-up
  archive latch therefore advances on the slow self-requeue cadence, not on events. The child-content
  `ManifestsArchived=True` staircase confirms it (r1 tail `…19, 24`; r3 stragglers `…76, 78, 89`), and the root
  subtree latch waits for the slowest child. r3's 77s is the same interval with the lost-wake tail fully exposed.
- **Root cause (code): the reverse-lookup `List`s read through the manager client, which does not cache
  unstructured objects, so `client.MatchingFields` is sent to the API server as a field selector it rejects.**
  The three field indexes (`indexKeyManifestCheckpointName` / `indexKeyChildContentName` /
  `indexKeyDataRefArtifactName`) **are** registered (`SetupWithManager` runs at startup with
  `SnapshotContentGVKs = [CommonSnapshotContentGVK]` and an empty `activeContentWatchSet`, so the guard does not
  skip them — the earlier "guard skips `IndexField`" hypothesis was **disproven**). The real defect is that
  `manager.Options` sets no `Client.Cache.Unstructured=true`, so controller-runtime's default applies:
  **unstructured `Get`/`List` bypass the cache and go to the API server.** `Get`-by-name still works (name
  selectors are supported), but the reverse-lookup `List`s (`snapshotcontent/controller.go` `reverseLookupReader`
  sites: `lookupContentsByManifestCheckpointName`, `mapVolumeSnapshotContentToContent`,
  `mapChildContentToParentContentsByEdge`) pass a **custom status field selector** the API server refuses
  (`field label not supported`). The registered cache indexes are therefore never consulted, and FIX 2 / FIX 3 /
  FIX 5's event wakes are effectively dead — only their poll/requeue backstops carry the archive wave. Not caused
  by D.3.

**Fix (H4.1, implemented; measurement pending redeploy).** Route the three enqueue-only reverse-lookup `List`s
through the **manager cache** (`mgr.GetCache()`, exposed as `r.cacheReader` via `reverseLookupReader()`), which
uses the registered `indexKey*` indexes, instead of `r.Client` (which hits the API server for unstructured). This
is deliberately **not** a global `Client.Cache.Unstructured=true` flip — that would also change D.3's child-List
read semantics (cached/eventually-consistent) and couple two unrelated changes. The reverse lookups only enqueue
`reconcile.Request`s and are fully backstopped by the 500ms self-requeue, so an eventually-consistent cache read is
safe by design. Unit tests are unaffected (they wire an indexed fake client as `Client`; `cacheReader` is nil in
tests and `reverseLookupReader()` falls back to `Client`). No status-contract change; poll/requeue backstops stay.
Acceptance (SETS=10 warm r1/r2): the three `field label not supported` errors disappear, the reverse-lookup
uncached API `List`s collapse, the apiserver-timeout count drops, `ChildrenSnapshotReady → ManifestsArchived`
falls well below the current ~12–14s, and Ready correctness is unchanged. r3-style lost-wake tails should also
disappear. Validate with the same server-side trace across 3 warm runs; **r3 (89s) is excluded from latency stats
as a control-plane-stall / leader-election-lost incident during a load spike** (both controllers lost their lease
to the same apiserver at 13:27; leader-election hardening is tracked separately, intentionally **not** in H4.1 so
it cannot mask the load problem).

---

## 9. Application checklist

1. [FIX 1](#fix-1-do-first--raise-manager-client-qpsburst-to-50100) (QPS/Burst, 3 files) — biggest win, independent.
2. [FIX 6](#fix-6--concurrency-ceilings--the-one-required-correctness-fix) (concurrency + `configMu` guard) — before/with the event watches.
3. [FIX 2](#fix-2--mcr-controller-watch-manifestcheckpoint), [FIX 3](#fix-3--snapshotcontent-two-reverse-lookup-watches), [FIX 5](#fix-5--storage-foundation-vcr-watch-volumesnapshotcontent) (event-driven wake-ups) — any order; each keeps its poll/requeue backstop.
4. [FIX 8](#fix-8--genericbinder-reverse-watch-mappers-direct-ref-o1-routing) (genericbinder O(1) routing) — load fix.
5. [ARCH 1](#arch-1--snapshot-controller-wake-the-gated-parent-on-child-content-archive) (gated-parent wake) and [ARCH 2](#arch-2--single-snapshotcontent-controller-with-dynamic-snapshot-status-watches-h2) (single content controller) — apply as architecture/correctness; not
   headline latency fixes.
6. Confirmed optimizations (section 6) — keep; hygiene.
7. H3 (section 8) — **closed** by Commit D.3 (per-child `Get`s → one `List`/GVK; validated SETS=10 warm). D.2 (membership-skip) and D.1′ (relay debounce) are **no longer recommended** — the premise is gone. Next open investigation is **H4 — root manifest leg** (`ChildrenSnapshotReady` → `Ready` ≈10s). Do not re-apply rejected hypotheses (section 7, incl. leaf-skip / distinct-H1).

## 10. Tooling (storage-e2e, measurement only)

- Namespace fan-out benchmark `tests/snapshot-latency/namespace_scale_test.go` (N independent standard sets, one
  root Snapshot) and per-object trace `trace_scale_test.go` (per-content manifest-leg / content-side-lag).
- SSH client fix presenting the adjacent OpenSSH certificate: `internal/infrastructure/ssh/client.go`
  (`loadCertSigner`) so cert-only clusters connect.
- The harness builds its own SSH tunnel to a cluster node; if that node's key/user changes (e.g. after a node
  redeploy) the harness cannot connect. The same benchmark can be driven directly via `kubectl` against a working
  kubeconfig, taking timings from server-side condition `lastTransitionTime` relative to the Snapshot
  `creationTimestamp` (immune to client-side polling lag). Note a **cold-start artifact**: the first namespace
  snapshot after an idle/fresh controller can show a one-off inflated root-manifest leg (~80s at SETS=1) that
  disappears on warm runs (~4s) — warm up before measuring.

These produced the numbers above; re-apply only if `storage-e2e` is also reset.

---

## 11. Appendix — APIReader audit (switch to cache vs keep uncached)

Part of the L4 load-shaving (section 6). The win is replacing `r.APIReader.Get` with the cached `r.Client.Get`
**only** on event-driven mirror reads of a **watched** object, where a stale cache costs at most one extra
reconcile and the object is watched so the mirror re-fires. Everything else uses APIReader for a **correctness**
reason and must stay uncached.

**Switch to cached `r.Client.Get` — the ONLY reads the audit found safe to cache (all applied):**

| file | function | read that was switched |
|---|---|---|
| `genericbinder/controller.go` | `checkConsistencyAndSetReady` | bound SnapshotContent GET by `contentKey` (Ready-mirror): `r.APIReader.Get` → `r.Client.Get` |
| `snapshot/content_reader.go` | `getSnapshotContentCached` (new; used by the mirrors below) | bound SnapshotContent for the Ready/ManifestsArchived mirror |
| `snapshot/ready_patch.go` | `mirrorSnapshotReadyFromBoundContent`, `mirrorSnapshotManifestsArchivedFromBoundContent` | `getSnapshotContentFresh` → `getSnapshotContentCached` |

Rationale: SnapshotContent is watched (its status change re-enqueues the bound Snapshot), so these mirrors are
event-driven and a stale cache costs at most one extra reconcile before convergence; `INV-RECONCILE-TRUTH` is the
backstop. This is the whole L4 win — do not extend it beyond these three.

**KEEP as `r.APIReader` — the audit concluded these NEED the uncached read (do NOT "optimize"):**

- **UID-barrier reads of ObjectKeeper after creation** — `genericbinder/controller.go` (~545/599),
  `manifestcapture/checkpoint_controller.go` (~369): must read-after-write the just-created ObjectKeeper UID; a
  cache miss breaks the barrier.
- **Read-after-write existence check of the MCR** — `snapshot/capture.go` (~182): the split-client cache can lag
  a just-created MCR; the gate relies on the uncached read (Create tolerates AlreadyExists).
- **Read-after-write child GET in the binder** — `genericbinder/controller.go` (`childObj` GET, ~826): kept
  uncached deliberately; NOT the mirror read above.
- **`declaredNonLeafChildContentNames` owner reads** (`snapshotcontent`): correctness-critical for the one-way
  `ManifestsArchived` latch — deferred by T-cost, never cached. This is also the read that keeps ARCH 2's
  snapshot-status wake necessary.
- **Edge-set preserve reads** — `snapshotcontent/status_publish.go`, `volume_child_content.go`: current published
  edge set read uncached so it reflects edges just written by the other writer.
- **Internal-only manifest/chunk reads** — `usecase/archive_service.go`, `usecase/import_manifest_reconstruct.go`:
  ManifestCheckpoint + chunks have no informer; they must bypass the cache like the `/manifests` API server.
- **Other binder content/child reads** (`genericbinder` `import.go`/`domain_content.go`,
  `PublishSnapshotContentChildrenFromSnapshotRefs`, safe-to-delete checks): reviewed and left on APIReader — they
  gate planning/binding/deletion where cache lag would risk a wrong decision.

Conclusion: the only unnecessary uncached reads were the three watched-object mirror reads above (now cached).
Every other `APIReader` use is a deliberate correctness choice (UID barrier, read-after-write, one-way latch,
edge-preserve, or informer-less internal objects) and must stay.
