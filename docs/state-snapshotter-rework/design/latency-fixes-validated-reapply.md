# Snapshot-creation latency — fix instructions (what and where to change)

Line under the latency investigation. This is a **re-application guide**, not a patch and not a cherry-pick
list: each section says exactly which file to touch, where in it, what to add, and why. Apply by hand on a
fresh `main`. Order of sections is the recommended application order.

Repos: `state-snapshotter` (controller + demo domain-controller) and `storage-foundation` (VCR controller).
`storage-e2e` is measurement tooling only — see the last section.

## What was actually wrong (three root causes)

1. **The first dominant tail in a multi-tree burst was the client-go default rate limiter (QPS=5 / Burst=10)**
   on the shared manager client. Under a multi-tree burst, uncached reads + status patches queued behind 5 QPS
   and inflated one reconcile to 4–15s, serializing the tree-Ready tail **regardless of
   `MaxConcurrentReconciles`**. Fixing this alone gave ~5–10× (TREES=5 57s→6s, TREES=1 15s→3s). **Do this
   first.** (This is the first tail; a second, still-open tail appears in the namespace fan-out benchmark — see
   Open.)
2. **500ms self-requeue "poll handshakes" instead of watches** between controllers (MCR↔ManifestCheckpoint,
   the bottom-up `ManifestsArchived` latch, root-MCR planning, VCR↔VolumeSnapshotContent). Each poll hop added
   seconds; converting to event-driven wake-ups removed structural latency.
3. **Implicit `MaxConcurrentReconciles=1`** in several controllers was a ceiling once (1) was fixed; raising it
   needs one real correctness fix (shared `Config` mutation under concurrent reconciles).

Current validated state after applied fixes — two distinct benchmarks, do not conflate:

- **Parallel same-shape snapshot burst** (N independent trees of the same shape): SET=1 ~3s, TREES=5 ~6s.
- **Namespace fan-out benchmark** (one root Snapshot over N independent standard sets): SETS=10 ROOT Ready
  ~29.2s after FIX 8 (was ~33.5s) — **still above target and an open scaling issue.**

**Do not claim snapshot scalability is solved by these fixes.** They removed the first (rate-limiter) tail and
several poll-handshake tails; FIX 8 additionally removed the genericbinder reverse-watch `List`+decode CPU/alloc
hotspot (a load fix). The remaining SETS=10 namespace fan-out tail is now **latency-bound** (per-level dependency
chain / requeue cadence / content-side maturation), **not** CPU or `List` cost, and is **not** closed by anything
here — see the last "Open" section.

---

## FIX 1 (do first) — raise manager client QPS/Burst to 50/100

Root cause #1. In each `main.go`, right after the `rest.Config` is created and **before** `manager.New`, set:

```go
kConfig.QPS = 50
kConfig.Burst = 100
```

Where:

| repo | file | anchor |
|---|---|---|
| state-snapshotter | `images/state-snapshotter-controller/cmd/main.go` | after config is built, before scheme/manager creation (`log.Info("[main] kubernetes config ... created")`) |
| state-snapshotter | `images/domain-controller/cmd/domain-controller/main.go` | same anchor (`[domain-main] kubernetes config ... created`) |
| storage-foundation | `images/controller/cmd/main.go` | same anchor (before `apiruntime.NewScheme()`) |

Why: this is the decisive fix. Precedent in-repo: the capture path already used QPS 100 / Burst 200 on its own
clients. The domain-controller is the demo **planning** layer; note in a comment that a production domain
controller must set its own QPS (not a contract).

Validated: TREES=5 57s→6s; TREES=1 15s→3s; reconcile durMs mean 4491→125ms, max 14812→847ms.

Caveat: **QPS=50/100 is capacity tuning, not proof of low work.** It does not reduce the amount of work; it
just stops the default limiter from serializing it. It was validated by flat reconciles-per-tree and no
post-Ready storm — not by showing the work itself is small. Pick a production value deliberately.

---

## FIX 2 — MCR controller: watch ManifestCheckpoint (remove MCR↔checkpoint 500ms poll)

Root cause #2. File `images/state-snapshotter-controller/internal/controllers/manifestcapture/checkpoint_controller.go`.

1. Add a mapper `mapManifestCheckpointToMCR(ctx, obj) []reconcile.Request` that reads
   `checkpoint.spec.manifestCaptureRequestRef` (name+namespace) and enqueues that MCR. (Owns() cannot route it
   because the checkpoint is owned by the execution ObjectKeeper, not the MCR.)
2. In `SetupWithManager`, add to the builder:
   ```go
   .Watches(&storagev1alpha1.ManifestCheckpoint{}, handler.EnqueueRequestsFromMapFunc(mapManifestCheckpointToMCR))
   ```

Validated: clean tree 10–17s → 5–7s.

Apply FIX 6 (concurrency + `configMu`) to this same file at the same time.

---

## FIX 3 — SnapshotContent: two reverse-lookup watches (pre-adoption MCP wake + event-driven archive latch)

Root cause #2. File `images/state-snapshotter-controller/internal/controllers/snapshotcontent/controller.go`.
This drives both the manifest-leg wake before ownerRef adoption (L9a) and the bottom-up archive latch (C-2).

Register two cache field indexes (per content GVK, in the setup that registers each GVK):

```go
const indexKeyManifestCheckpointName = ".status.manifestCheckpointName"
const indexKeyChildContentName       = ".status.childrenSnapshotContentRefs.name"
```

- `extractManifestCheckpointNameIndex` → projects `status.manifestCheckpointName`.
- `extractChildContentNamesIndex` → projects every `status.childrenSnapshotContentRefs[].name`.
- Register both with `mgr.GetFieldIndexer().IndexField(...)` for each content GVK.

Add two mappers + watches (keep the existing ownerRef-based watches as dual-path backstop):

1. **L9a pre-adoption MCP wake** — mapper `mapManifestCheckpointToContent` uses
   `lookupContentsByManifestCheckpointName` (List by `indexKeyManifestCheckpointName`) so a ManifestCheckpoint
   event wakes the content whose `status.manifestCheckpointName` matches, even before ownerRef adoption. Wire
   it into `addArtifactWakeUpWatches` alongside the ownerRef mapper (`ownerRefToContentRequests`).
2. **C-2 event-driven archive latch** — mapper `mapChildContentToParentContentsByEdge` lists parents by
   `indexKeyChildContentName` = this content's name and enqueues them:
   ```go
   .Watches(obj, handler.EnqueueRequestsFromMapFunc(mapSnapshotContentToParentContent))        // existing ownerRef path
   .Watches(obj, handler.EnqueueRequestsFromMapFunc(r.mapChildContentToParentContentsByEdge))  // new forward-edge path
   ```

Keep `MaxConcurrentReconciles: 8` for this reconciler (set in its controller options), and keep the 500ms
self-requeue as a backstop. No status contract changes.

Validated (C-2): archive-latch gap last-child→root ~12s → ~2.4s.

---

## FIX 4 — Snapshot controller: wake the gated parent on child-content archive (event-driven root MCR)

Root cause #2. File `images/state-snapshotter-controller/internal/controllers/snapshot/content_watch.go`, in
the SnapshotContent watch handler `snapshotContentToSnapshotEnqueueHandler`.

Today the handler wakes only the **bound owner** Snapshot. Also wake the **gated parent(s)** — the Snapshot
whose root-MCR gate (`usecase.requireContentManifestsArchived`) reads this child content's archive latch —
on the `ManifestsArchived` False→True transition:

- `UpdateFunc`: compute `archivedTransition := !archived(old) && archived(new)`; when true, additionally
  enqueue gated parents.
- `CreateFunc`: enqueue gated parents when the created content is already `ManifestsArchived=True`
  (resync/restart).
- Gated-parent resolution: `content.spec.snapshotRef` (owning child Snapshot S) →
  `findParentsReferencingChildSnapshot(S)` (Snapshots that list S in `status.childrenSnapshotRefs`,
  namespace-local). Helper: `gatedParentRequestsFromContent`.
- **Dedup** the bound-owner + gated-parent requests by `NamespacedName` within one event
  (`enqueueContentDrivenSnapshots`).

Do not weaken the gate (`requireContentManifestsArchived` / `BuildRootNamespaceManifestCaptureTargets`); keep
the 500ms backstop. A root's own content maps to no parent (no self-wake cycle).

Validated: root MCR created ~30–31s → ~24.4s; ROOT Ready ~37s → ~33.5s (SETS=10, 3 runs). This confirms the
wake-up path, but does not close the remaining SETS=10 tail.

---

## FIX 5 — storage-foundation VCR: watch VolumeSnapshotContent (event-driven data leg)

Root cause #2. Files under `images/controller/internal/controllers/`.

1. `constants.go`: add label key `LabelKeyVCRNamespaceFull = "storage.deckhouse.io/vcr-namespace"` (a
   `vcr-name` label already exists).
2. At VSC creation (`volumecapturerequest_snapshot_bulk.go`): stamp both `LabelKeyVCRNameFull` and
   `LabelKeyVCRNamespaceFull` on the CSI VolumeSnapshotContent so it carries its owning VCR coordinates.
3. `volumecapturerequest_controller.go`: add mapper `mapVolumeSnapshotContentToVCR(ctx, obj)` that reads those
   two labels and enqueues that VCR; wire into `SetupWithManager`:
   ```go
   .Watches(&snapshotv1.VolumeSnapshotContent{}, handler.EnqueueRequestsFromMapFunc(mapVolumeSnapshotContentToVCR))
   ```
   Also apply FIX 6 concurrency here.

Keep the 5s requeue as a safety net (covers VSCs created before the label existed).

---

## FIX 6 — concurrency ceilings + the one required correctness fix

Root cause #3. Raise `MaxConcurrentReconciles` (conservative 4) on the controllers that were implicitly 1, and
add the `Config` race guard.

| repo | file | change |
|---|---|---|
| state-snapshotter | `genericbinder/controller.go` | `MaxConcurrentReconciles: 4` **+ RateLimiter** `NewTypedItemExponentialFailureRateLimiter[ctrl.Request](200ms, 10s)` (via `genericBinderControllerOptions()`) |
| state-snapshotter | `manifestcapture/checkpoint_controller.go` | `MaxConcurrentReconciles: 4` **plus** the `configMu` guard below (no RateLimiter here) |
| state-snapshotter | `domain-controller/.../demo/virtualmachinesnapshot_controller.go`, `.../virtualdisksnapshot_controller.go` | `MaxConcurrentReconciles: 4` (snapshot demo controllers only — **not** the VM/Disk lifecycle controllers) |
| storage-foundation | `volumecapturerequest_controller.go` | `MaxConcurrentReconciles: 4` + `RateLimiter: NewTypedItemExponentialFailureRateLimiter[ctrl.Request](200ms, 10s)` |

**Required correctness fix (checkpoint_controller.go):** `loadConfigFromConfigMap` rewrites shared `Config`
fields (`MaxChunkSizeBytes`, `DefaultTTL`, `DefaultTTLStr`) on every reconcile — a data race once concurrency
> 1. Guard them with a mutex `configMu` behind accessor methods (`cfgMaxChunkSizeBytes`, `cfgDefaultTTL`,
`cfgDefaultTTLStr`) and snapshot the config once per reconcile before use. Do not raise concurrency here
without this guard.

The SnapshotContent aggregator also runs at `MaxConcurrentReconciles: 8` (`snapshotContentControllerOptions`),
set as part of FIX 3 — no RateLimiter there. Full concurrency picture: genericbinder 4, checkpoint 4, foundation
VCR 4, demo VMS/VDS 4, SnapshotContent 8.

These did not move the wall on their own (the gate was downstream each time) but remove the ceiling and are
prerequisites for FIX 2–5 to run correctly under load. Start at 4, not 8.

---

## FIX 7 (optional) — load-shaving / reconcile-cost trims

Small, validated no-regression; hygiene, not required for the headline win.

- **T-cost** — `snapshotcontent/controller.go`, `aggregateChildrenManifestsArchived`: take an
  `ownManifestReady bool` and **defer** the expensive uncached `declaredNonLeafChildContentNames` walk to the
  only pass that can latch True (`ownManifestReady && no pending linked child`). Observe lag ~4.3s → ~3.1s.
- Keep any per-reconcile SnapshotContent trace at **debug** level (it is diagnostics, not a fix).

See the **Appendix — APIReader audit** at the end for the full switch-to-cache vs keep-uncached list (part of
the L4 load-shaving).

---

## FIX 8 — genericbinder reverse-watch mappers: direct-ref O(1) routing (remove full unstructured `List`+decode)

**Load/throughput fix, not the wall-clock fix.** Files under
`images/state-snapshotter-controller/internal/controllers/genericbinder/`.

Root cause: the three reverse-watch map functions each did a full `unstructuredClient.List` of a GVK + JSON
decode of every object, then filtered to the one match — O(#snapshots/#contents) work + allocations **per
event** (SETS=10 profile: ~69% CPU, ~84% alloc; grows with tree size). Replace with the references that already
exist on the event object (no `List`, no decode):

| file | mapper | change |
|---|---|---|
| `content_watch.go` | `mapBoundContentToSnapshots` | read `content.spec.snapshotRef`, enqueue it directly (O(1)) |
| `content_watch.go` | `mapParentContentToChildSnapshots` | `Get` the owning child Snapshot (from `content.spec.snapshotRef`), enqueue the parents it lists in `status.childrenSnapshotRefs` |
| `mcr_watch.go` | `mapMCRToOwningSnapshots` | walk `obj.GetOwnerReferences()` for the matching Kind/APIVersion, enqueue the owner |

Update `controller.go` watch registrations to the standalone (no-`r.`) mapper signatures where applicable. No
reconcile-contract or status changes; no field index needed (the direct references are the index). Keep no extra
backstop — the existing watch/requeue coverage is unchanged.

Optional diagnostics shipped alongside (gate off by default): per-mapper atomic counters
(`watch_map_stats.go`, env `STATE_SNAPSHOTTER_WATCH_MAP_STATS`) and explicit controller-runtime metrics on
`:8080` in `cmd/main.go`.

Validated (SETS=10, post-deploy): the two `List`-based mappers **disappear** from the CPU profile; watch-path
`unstructuredClient.List` drops to ~1%; controller is ~73% idle during the fan-out; reconciles stay bounded
(~3–5 / object), 0 errors, no post-Ready storm. ROOT Ready ~33.5s → **~29.2s** (−13%): the mapper cost was a
real scaling liability but **not** the dominant wall-clock term — the residual tail is latency-bound (see Open).

---

## Open (NOT fixed here) — next lever

At SETS=10, ROOT Ready ~29.2s (after FIX 8). FIX 8 proved (by CPU profile) that the reverse-watch mapper
`List`+decode cost — though real and growing with size — was **not** the dominant wall-clock term: with it gone
the controller is ~73% idle during the fan-out, so the tail is **latency-bound**, not CPU-bound. Data path
(VCR/CSI-VSC ready) completes ~14.5s, but per-content `VolumesReady`/`ManifestsReady` spread to ~29–34s (a
straggler). This **content-side maturation** tail (per-level dependency chain / requeue cadence / late artifact
observation) needs a **critical-path timing** run, not another `List`/CPU optimization, and is addressed by none
of the fixes above. It is the next thing to investigate, not something to re-apply.

Run #2 (critical-path timing) to locate the wall-clock: enable `STATE_SNAPSHOTTER_WATCH_MAP_STATS`, and collect
per-level content timings, top-5 straggler contents, artifact-ready→content-ready deltas, and the
root direct-children-archived → root-MCR → root-Ready chain — then decide the next fix.

Diagnostic plan (run before writing any fix), per SnapshotContent — especially the top stragglers:

1. **VCR Ready** and **CSI VSC readyToUse** — when the volume artifacts became ready.
2. **MCR created / MCP Ready** (per content, not just root) — when the manifest leg's checkpoint became ready.
3. **First reconcile of the content after BOTH artifacts are ready** — the gap between "both inputs ready" and
   the next reconcile of this content key.
4. **content `ManifestsReady` / `VolumesReady` / `Ready`** transition times.
5. **Classify the top stragglers** by which leg gated (VOLUME vs MANIFEST) and whether the output lagged its
   input:
   - MANIFEST leg, MCP Ready early but `ManifestsReady` late → content-side wake-up / queue-starvation.
   - MANIFEST leg, MCP Ready itself late → manifest capture cost.
   - VOLUME leg, `VolumesReady` ≫ VCR-Ready waterline → volume-leg wake-up.
   - VOLUME leg, `VolumesReady` ≈ waterline → foundation/CSI.

The e2e trace probe (`trace_scale_test.go`, per-content manifest-leg / content-side-lag) already emits most of
these; item 3 (first reconcile after both ready) needs the controller reconcile trace at debug.

---

## Application checklist

1. FIX 1 (QPS/Burst, 3 files) — biggest win, independent.
2. FIX 6 (concurrency + `configMu` guard) — before/with the event watches.
3. FIX 2, 3, 4, 5 (event-driven wake-ups) — any order; each keeps its poll/requeue backstop.
4. FIX 7 (optional trims).
5. Leave the Open item alone until diagnosed.

## Tooling (storage-e2e, measurement only)

- Namespace fan-out benchmark `tests/snapshot-latency/namespace_scale_test.go` (N independent standard sets,
  one root Snapshot) and per-object trace `trace_scale_test.go` (per-content manifest-leg / content-side-lag).
- SSH client fix presenting the adjacent OpenSSH certificate: `internal/infrastructure/ssh/client.go`
  (`loadCertSigner`) so cert-only clusters connect.
These produced the numbers above; re-apply only if `storage-e2e` is also reset.

---

## Appendix — APIReader audit (switch to cache vs keep uncached)

Part of the L4 load-shaving (FIX 7). The win is replacing `r.APIReader.Get` with the cached `r.Client.Get`
**only** on event-driven mirror reads of a **watched** object, where a stale cache costs at most one extra
reconcile and the object is watched so the mirror re-fires. Everything else uses APIReader for a **correctness**
reason and must stay uncached.

**Switch to cached `r.Client.Get` — the ONLY reads the audit found safe to cache (all applied):**

| file | function | read that was switched |
|---|---|---|
| `genericbinder/controller.go` | `checkConsistencyAndSetReady` | the bound SnapshotContent GET by `contentKey` (Ready-mirror) — `r.APIReader.Get(ctx, contentKey, contentObj)` → `r.Client.Get` |
| `snapshot/content_reader.go` | `getSnapshotContentCached` (new; used by the mirrors below) | bound SnapshotContent for the Ready/ManifestsArchived mirror |
| `snapshot/ready_patch.go` | `mirrorSnapshotReadyFromBoundContent`, `mirrorSnapshotManifestsArchivedFromBoundContent` | switched from `getSnapshotContentFresh` → `getSnapshotContentCached` |

Rationale: SnapshotContent is watched (its status change re-enqueues the bound Snapshot), so these mirrors are
event-driven and a stale cache costs at most one extra reconcile before convergence; `INV-RECONCILE-TRUTH` is
the backstop. This is the whole L4 win — do not extend it beyond these three.

**KEEP as `r.APIReader` — the audit concluded these NEED the uncached read (do NOT "optimize"):**

- **UID-barrier reads of ObjectKeeper after creation** — `genericbinder/controller.go` (~545/599),
  `manifestcapture/checkpoint_controller.go` (~369): must read-after-write the just-created ObjectKeeper UID;
  a cache miss breaks the barrier.
- **Read-after-write existence check of the MCR** — `snapshot/capture.go` (~182): the split-client cache can
  lag a just-created MCR; the gate relies on the uncached read (Create tolerates AlreadyExists).
- **Read-after-write child GET in the binder** — `genericbinder/controller.go` (`childObj` GET, ~826, commented
  "Uses APIReader for read-after-write consistency"): kept uncached deliberately; this is NOT the mirror read
  above.
- **`declaredNonLeafChildContentNames` owner reads** (`snapshotcontent`): correctness-critical for the one-way
  `ManifestsArchived` latch — deferred by T-cost, never cached.
- **Edge-set preserve reads** — `snapshotcontent/status_publish.go`, `volume_child_content.go`: the current
  published edge set is read uncached so it reflects edges just written by the other writer.
- **Internal-only manifest/chunk reads** — `usecase/archive_service.go`, `usecase/import_manifest_reconstruct.go`:
  ManifestCheckpoint + chunks have no informer; they must bypass the cache like the `/manifests` API server.
- **Other binder content/child reads** (`genericbinder` `import.go`/`domain_content.go`,
  `PublishSnapshotContentChildrenFromSnapshotRefs`, safe-to-delete checks): reviewed and left on APIReader —
  they gate planning/binding/deletion where cache lag would risk a wrong decision, for a smaller latency gain.

Conclusion of the audit: the only unnecessary uncached reads were the three watched-object mirror reads above
(now cached). Every other `APIReader` use is a deliberate correctness choice (UID barrier, read-after-write,
one-way latch, edge-preserve, or informer-less internal objects) and must stay.
