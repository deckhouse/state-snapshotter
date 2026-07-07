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
optimizations, one architecture/correctness cleanup, 2–3 rejected hypotheses, and 2 still-open latency issues.

Sections 4–6 are a **re-application guide**: apply by hand on a fresh `main`; section order is the recommended
application order. Sections 3, 7, 8 are the **classification** (why each change matters, what was disproven, what
remains). **Do not claim snapshot scalability is solved** — see section 8.

## 2. Current validated state

Two distinct benchmarks — **do not conflate them**:

- **Parallel same-shape snapshot burst** (N independent trees of the same shape): SET=1 ~3s, TREES=5 ~6s.
- **Namespace fan-out benchmark** (one root Snapshot over N independent standard sets): **SETS=10 ROOT Ready
  ~25s server-side (~29–30s client-measured)** — still above target and an open scaling issue.

CPU is no longer the bottleneck (genericbinder mapper fix; controller mostly idle during fan-out), the
reverse-watch mapper `List` is gone, the rate limiter is fixed, and polling is largely event-driven. The
remaining SETS=10 tail is **latency-bound** (per-level dependency chain / requeue cadence / content-side
maturation), not CPU or `List` cost, and is **not** closed by anything here.

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
| **Repeated child-graph planning is the dominant root latency** | **Partially true** | Removing repeated planning (Commit B) gave ~18% wall, but did not eliminate the fan-out tail → not the dominant term. |

---

## 8. Remaining open issues (open investigations)

State at SETS=10: **ROOT Ready ~25s server-side (~29–30s client-measured)**. CPU, mapper `List`, the rate
limiter, and most polling are **no longer** bottlenecks. The remaining tail is **latency-bound**. Diagnosis has
already been run (per-object critical-path timing, 3 stable runs, pprof, Commit B/C decomposition, single-clock
leaf trace); what remains are two open hypotheses, not another diagnostic pass.

- **H1 — leaf staircase (PRIMARY open latency issue).** `vscReady → leaf content Ready` grows ~4→9–11s across the
  20 leaves. CSI per-volume is flat (~3s) and foundation VCR ≈ 0; the growth is on the state-snapshotter
  leaf-content side, where leaves sit in `ManifestCapturePending` and the manifest+volume legs latch together at
  the end. Root cause not yet isolated (suspected: leaf manifest-capture (MCP) readiness pacing + worker
  contention). Diagnosis-first.
- **H3 — child-graph planning O(N) (secondary open issue).** `reconcileParentOwnedChildGraph` still costs ~7s on
  the single MCR-creating pass (Commit B removed only the *repeated* replans). Needs separate profiling.

> **H2 is closed** — see [ARCH 2](#arch-2--single-snapshotcontent-controller-with-dynamic-snapshot-status-watches-h2). It was implemented and validated as an architecture/correctness cleanup
> (duplicate controller removed, single dynamic `Watch` on the primary controller, correctness OK, latency
> unchanged within noise). It is **not** a remaining latency lever.

Adjacent future work: choose the production manager client QPS/Burst deliberately (50/100 was capacity tuning, not
proof of low work — see FIX 1 caveat).

---

## 9. Application checklist

1. [FIX 1](#fix-1-do-first--raise-manager-client-qpsburst-to-50100) (QPS/Burst, 3 files) — biggest win, independent.
2. [FIX 6](#fix-6--concurrency-ceilings--the-one-required-correctness-fix) (concurrency + `configMu` guard) — before/with the event watches.
3. [FIX 2](#fix-2--mcr-controller-watch-manifestcheckpoint), [FIX 3](#fix-3--snapshotcontent-two-reverse-lookup-watches), [FIX 5](#fix-5--storage-foundation-vcr-watch-volumesnapshotcontent) (event-driven wake-ups) — any order; each keeps its poll/requeue backstop.
4. [FIX 8](#fix-8--genericbinder-reverse-watch-mappers-direct-ref-o1-routing) (genericbinder O(1) routing) — load fix.
5. [ARCH 1](#arch-1--snapshot-controller-wake-the-gated-parent-on-child-content-archive) (gated-parent wake) and [ARCH 2](#arch-2--single-snapshotcontent-controller-with-dynamic-snapshot-status-watches-h2) (single content controller) — apply as architecture/correctness; not
   headline latency fixes.
6. Confirmed optimizations (section 6) — keep; hygiene.
7. Open issues H1 / H3 (section 8) — open investigation; do not re-apply rejected hypotheses (section 7).

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
