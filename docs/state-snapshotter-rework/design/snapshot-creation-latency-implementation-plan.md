# Snapshot creation latency — implementation plan

> **Status:** implementation plan (design/). Builds on
> [`snapshot-creation-latency-analysis.md`](./snapshot-creation-latency-analysis.md) (baseline) and
> [`snapshot-creation-latency-optimization.md`](./snapshot-creation-latency-optimization.md) (levers).
> Not a normative contract; does not change `spec/system-spec.md`.
>
> **Implementation status (pre-redeploy code batch):**
> - **L1 — done:** VSC watch in the `storage-foundation` VCR controller (VCR-coordinate labels stamped
>   at VSC creation + `mapVolumeSnapshotContentToVCR`); 5s requeue kept as a safety net.
> - **L2 — done:** `genericbinder` `MaxConcurrentReconciles: 1 → 4` + 200ms→10s rate limiter
>   (`genericBinderControllerOptions`).
> - **L3 — already present:** the binder's reverse content watch (`mapBoundContentToSnapshots` on the
>   common content GVK) and the `SnapshotReconciler`'s `Watches(&SnapshotContent{})` already make the
>   Ready mirror event-driven; only a stale "no reverse reference" comment was corrected.
> - **L0a / L5 / L7 — deferred** (after the L0b cluster baseline): L0a is large cross-pod correlation-id
>   plumbing; L5 and L7 are load-only and L5's 500ms self-requeue is a deliberate drop-safe
>   archive-wave driver — both are best validated against the reconcile-count harness once a baseline
>   exists, to avoid muddying the L1/L2 latency attribution.
> - **L8 — done:** MCR controller now watches `ManifestCheckpoint` via
>   `spec.manifestCaptureRequestRef` (`mapManifestCheckpointToMCR`), removing the 500ms poll gap in
>   `finalizeMCRIfCheckpointHandedOff` while it waits for the SnapshotContent ownerRef handoff. See
>   "L8 — Manifest-leg checkpoint watch" below. Needs redeploy to re-measure on cluster.
> - **L9a — done:** the symmetric `snapshotcontent` side of L8. The MCP wake-up mapper is now dual-path
>   (`mapManifestCheckpointToContent`): ownerRef when adopted, else a pre-adoption reverse lookup by
>   `status.manifestCheckpointName` via a new cache field index. Removes the 500ms adoption-poll gap
>   without touching the ownership model. See "L9a — Dual-path artifact routing" below. Needs redeploy
>   to re-measure on cluster.
> - **L9c — done:** finalizer-add 409 conflicts in `snapshotcontent` are treated as benign (requeue,
>   not `Reconciler error`), removing rate-limited backoff from concurrent-reconcile races.
> - **L9b — deferred:** lengthen the `snapshotcontent` 500ms self-requeue once L9a is validated.
>
> **Cluster measurement update (post-L1/L2, ms timeline via `kubectl logs --timestamps`):**
> - The original "45–60s" premise was **not reproducible** on the current cluster. Real numbers:
>   single **manifest-only** leaf ≈ **2s**, single **PVC-backed** leaf ≈ **3.5s**, full **VM tree**
>   (VM owning a PVC disk + child manifests) ≈ **10–17s**.
> - CSI is **not** the bottleneck here: `VSC created → readyToUse` was ≈0s. After L1 the 5s VCR poll
>   gap is gone; the remaining leaf time is genuine event-driven cross-pod work + redundant re-mirror
>   passes (load, not latency).
> - **Tree latency root cause = the manifest leg**, not data/mirror: `ManifestsArchived` took ~5–6s on
>   the critical path. The MCR controller created the `ManifestCheckpoint` with `Ready=True`
>   synchronously but then **polled at 500ms** waiting for SnapshotContent to adopt it (ownerRef
>   handoff), and `snapshotcontent` polls at its own 500ms self-requeue to adopt/aggregate — a
>   two-controller 500ms handshake that multiplies with tree depth/manifest count. L8 removes the MCR
>   side of that handshake; the `snapshotcontent` adoption-poll side remains (harder, deferred).

## Goal

**Reduce standard snapshot creation wall-clock from 45–60s to 15–25s without increasing
steady-state API load.**

Standard scheme: a VM snapshot (VM owns 1 data disk) + 1 standalone data-disk snapshot.

## Scope & non-goals

This is **polishing of the already-implemented solution, not an architectural redesign.** We do
**not** change:

- the snapshot/content model or its GVKs (`Snapshot` / `SnapshotContent` / `MCR` / `VCR` / `MCP` /
  CSI `VolumeSnapshotContent`),
- the controller/pod topology (`domain-controller` / `storage-foundation` / `ssc`) or which
  controller owns what,
- the condition/readiness contract (`ChildrenSnapshotReady`, `ManifestsReady`, `VolumesReady`,
  `ChildrenReady`, `Ready`, the archive latch) or the ownership/handoff invariants,
- the planning barrier / SDK 3-state model or the domain↔common boundary.

All changes are **local optimizations within the existing design**: make readiness event-driven
instead of poll-driven (watches), parallelize work that is already independent (concurrency), and
tune fallback intervals. Nothing here alters `spec/system-spec.md`; if any task is found to require
a contract change, it is **out of scope** for this plan and must be raised separately.

## Principle

Measure first, then make readiness event-driven, then parallelize, and only use interval tuning as
a stopgap. **Order: L0a → (L1 + L2 + L3 + L5) → L7 (after its watch lands) → L4 stopgap (optional)
→ L0b baseline + L6 validation.** L0a (local instrumentation) is a prerequisite for the code fixes;
the cluster baseline (L0b) must **not** block the obvious code fixes. L7 (cached-read cleanup) rides
on the watches added by L3. L4/L5 are temporary and must not become the architecture.

## Task breakdown

### L0a — Latency trace points & log fields  *(do first, local)*

**Why:** without per-hop timestamps we argue about estimates, not facts. This is pure
instrumentation — no cluster needed — so it can land before everything else and unblocks the code
fixes immediately.

**Change:** emit a structured log event (and, ideally, a duration metric) at each key transition,
keyed by a correlation id (root snapshot UID) so a single creation can be reconstructed across the
three pods:

| Transition | Where it is set today |
|---|---|
| Snapshot created | domain VMS/VDS reconcile entry (`virtualmachinesnapshot_controller.go`, `virtualdisksnapshot_controller.go`) |
| Domain planned MCR/VCR/children | `MarkPlanningReady` → `ChildrenSnapshotReady=True` (SDK `capture.go:267-277`) |
| VCR Ready | `storage-foundation .../volumecapturerequest_controller.go:602` (`finalizeVCR`) |
| MCP Ready | `ssc .../manifestcapture/checkpoint_controller.go` (MCP status complete) |
| SnapshotContent ManifestsReady / VolumesReady / ChildrenReady / Ready | `ssc .../snapshotcontent/controller.go` status patch (~`:476`) |
| Snapshot Ready mirror | `ssc .../genericbinder/controller.go:644-708` (`checkConsistencyAndSetReady`) |

**Acceptance:** for one creation, logs across all three pods can be joined by snapshot UID and a
script prints per-hop deltas. No behavior change.

**Tests:** unit assertion that the log/metric fires on each transition.

**Cluster:** **NO** — instrumentation + unit tests are entirely local.

---

### L0b — Baseline measurement on cluster

**Why:** turn the L0a instrumentation into actual before/after numbers, and measure the genuine CSI
`readyToUse` time for an empty volume in the target driver (how much of hop 2 is real CSI latency
vs. poll dead-time).

**Change:** none (measurement run). Capture per-hop deltas for the standard scheme on a live
cluster, both before and after L1–L5.

**Acceptance:** a recorded baseline + post-change run with per-hop breakdown.

**Cluster:** **YES** — real cluster with the target CSI driver. Does **not** block L1–L3/L5
development.

---

### L1 — VSC watch in the VolumeCaptureRequest controller  *(main fix)*

**Problem:** the VCR controller waits for CSI `VolumeSnapshotContent.status.readyToUse` via
`RequeueAfter: 5s` and has **no watch on VSC** — readiness is noticed only on the next 5s tick
(`storage-foundation .../volumecapturerequest_controller.go:233,238`; `SetupWithManager`
`:647-651` only `For(&VolumeCaptureRequest{})`).

**Change:** add a watch that maps a `VolumeSnapshotContent` update/delete to the owning/referencing
VCR and reconciles it immediately:

```
VolumeSnapshotContent update/delete
  → map to owning / referencing VolumeCaptureRequest
  → enqueue that VCR
```

Mapping options:
- **Preferred:** stamp a label on the VSC at creation (`vcr-name`/`vcr-uid`) and index on it — cheap,
  robust, no graph traversal.
- **Fallback:** the ownership chain `VSC → ObjectKeeper(FollowObject) → VCR` (ObjectKeeper name
  encodes VCR UID, `objectKeeperNameForVCR(vcr.UID)`, and `FollowObjectRef` points back to the VCR).
  Chain-walk is more fragile and more expensive — use only if labeling at creation is not feasible.

**Acceptance:** VCR transitions to Ready within ~CSI-actual time of `readyToUse` (sub-second after
the event), not on a 5s boundary; the 5s requeue remains only as a safety net.

**Tests:** envtest — create VCR, simulate VSC `readyToUse=true`, assert VCR reconciled without
waiting 5s.

**Repo:** `storage-foundation` (separate repository — needs its own PR).

**Expected effect:** −5–10s (removes the largest dead-time).

**Cluster:** code + envtest local; final confirmation on cluster (with L0 trace).

---

### L2 — Increase genericbinder concurrency  *(conflict-safe)*

**Problem:** `genericbinder` registers with `builder.Complete(r)` and **no `WithOptions`**
(`ssc .../genericbinder/controller.go:874`), so `MaxConcurrentReconciles=1`. The VMS, its child
VDS, and the standalone VDS are processed strictly one at a time.

**Change:** `MaxConcurrentReconciles: 1 → 4` first; raise to 8 **only** after the L0a trace and
conflict data confirm it is safe and beneficial. Add a bounded rate limiter matching
Snapshot/SnapshotContent (200ms→10s). Apply in `registerSnapshotWatch` and the runtime
`AddWatchForPair` path.

**Must verify (gating):**
- idempotency of `Create`/`Patch` (Get-then-Create on NotFound; merge patches).
- conflict retry on status writes (`markCaptureDone` already uses `RetryOnConflict`).
- no races on `SnapshotContent` patch from concurrent snapshots sharing a parent content.
- child/parent ordering invariant still holds.

**Acceptance:** scheme's snapshots are bound concurrently; no increase in conflict-error rate beyond
retry tolerance; all existing genericbinder tests pass + new concurrency test.

**Tests:** unit/envtest with N snapshots reconciled in parallel; assert no lost updates, stable
final state.

**Expected effect (scheme):** −5–10s wall-clock.

**Cluster:** local envtest sufficient for correctness; cluster for wall-clock confirmation.

---

### L3 — Fix/verify binder ← SnapshotContent Ready reverse-watch

**Problem:** the final mirror `SnapshotContent.Ready → Snapshot.Ready` may wait on the 5s fallback
(`genericbinder/controller.go:401`; comment `:393-394`: "no reverse reference … mirrored through
polling"). The binder already registers a reverse content watch (`:860-863`,
`mapBoundContentToSnapshots`).

**Change:** confirm the watched content GVK is the **common** `SnapshotContent` GVK that the
aggregator actually writes `Ready=True` on (`snapshotcontent/controller.go` status patch). If the
watched GVK ≠ aggregating GVK, wire the watch to the common content GVK so the mirror is
event-driven and the 5s requeue becomes a rare safety net.

**Acceptance:** flipping `SnapshotContent.Ready` wakes the bound snapshot within a propagation
cycle (sub-second), not 5s; fallback still present but off the critical path.

**Tests:** envtest — set common `SnapshotContent.Ready=True`, assert owning snapshot reconciled
promptly.

**Expected effect:** −2–5s per tree level.

**Cluster:** local envtest sufficient; cluster for confirmation.

---

### L4 — Configurable short fallback intervals  *(stopgap)*

**Only if L1/L3 are not ready quickly.** Make the fallback intervals configurable and lower:
- VCR pending: `5s → 1s` (`volumecapturerequest_controller.go:233,238`).
- binder fallback: `5s → 1–2s` (`genericbinder/controller.go:277,341,377,401`).

**Acceptance:** intervals come from config (not hardcoded); default tunable. Latency win measured;
reconcile/API churn delta measured and acceptable.

**Note:** temporary, not architectural. Increases reconcile churn — pair with L5.

**Cluster:** cluster needed to confirm the latency/load trade.

---

### L5 — SnapshotContent reconcile backoff + watch coverage  *(load, not latency)*

**Problem:** `snapshotcontent` self-requeues every 500ms and re-reads the subtree until Ready
(`controller.go:356-357`).

**Change:** keep 500ms until first meaningful progress, then exponential backoff (1s/2s/5s); ensure
watches exist on MCP / VSC / child `SnapshotContent` so progress is event-driven rather than
poll-driven (`addArtifactWakeUpWatches`, `controller.go` `SetupWithManager`).

**Acceptance:** reconcile read volume drops ~5–10× for a slow subtree with no latency regression on
the happy path (watches carry progress).

**Tests:** envtest counting reconciles over a fixed convergence; assert reduced count, same final
state. (Existing `reconcile_count_*_test.go` harness is a natural fit.)

**Expected effect:** ~0 latency; large API/reconcile-load reduction.

**Cluster:** local envtest sufficient.

---

### L7 — Replace unjustified `APIReader` reads with cached reads  *(load + coupling cleanup)*

**Problem:** several hot-path reads bypass the cache via `APIReader` (uncached direct GET on every
reconcile) where the object is already a locally-watched/reconciled type in the same manager. These
are not read-after-write cases — they are compensating for a **missing watch**, so they pay an
extra apiserver round-trip per reconcile *and* still keep a polling requeue. Once L1/L3/L5 add the
watches, these reads must move back to the cached client.

Not-justified / questionable sites (verified):

| Site | Read | Verdict | Depends on |
|---|---|---|---|
| `ssc genericbinder/controller.go:670` (`checkConsistencyAndSetReady`) | common `SnapshotContent` | uncached only because binder has no reverse-watch | **L3** |
| `ssc snapshot/content_reader.go:30` (`getSnapshotContentFresh`, used by `capture.go:413`) | bound `SnapshotContent` | same — "avoid stale mirror" is solved by a watch | **L3** |
| `ssc snapshot/capture.go:403` (`reconcileN2aRootReadyAfterManifestCapture`) | own `Snapshot` (read `boundSnapshotContentName`) | watched type; cache + binder status-write watch suffice | **L3** |
| `ssc genericbinder/controller.go:819` | child `Snapshot` existence | existence check; child arrives via dynamic-watch | L3 / standalone |

Defensible (leave, but document): `storage-foundation .../volumecapturerequest_snapshot_bulk.go:223`
(`StorageClass`) reads uncached while neighbouring PVC/PV use the cached client — keep `APIReader`
to avoid a cluster-wide `StorageClass` informer + keep get-only RBAC (same rationale as
`datarefs_publish.go:98` for PV), but add a one-line comment so the inconsistency is intentional.

**Clearly justified — do NOT touch:** UID-barrier read-after-create
(`lifecycle_ownerrefs.go:91`, `manifestcapture/checkpoint_controller.go:329`,
`genericbinder/controller.go:543,596`); MCR-gate (`snapshot/capture.go:182`, avoids a full
namespace re-list); fail-closed declared-children set (`snapshotcontent/controller.go:862`);
safe-to-delete handoff gates (`genericbinder/domain_content.go:60,102`); deletion/cleanup paths
(`controller.go:477`, `snapshotcontent/controller.go:1405`).

**Change:** after the corresponding watch lands (L3 for the `SnapshotContent`/`Snapshot` mirror
reads), switch those `APIReader.Get` calls to the cached `Client.Get`. The
`reconcile_count_harness_test.go` (two interceptors over one store) already distinguishes cached vs
uncached reads and protects against silent regressions.

**Acceptance:** the four sites above read from cache; the watch (L3) drives freshness; no stale-Ready
regression in envtest; `reconcile_count_*` harness confirms the read moved to the cached client.

**Tests:** reuse the L3 envtest (Ready flips promptly) + the reconcile-count harness.

**Expected effect:** removes one uncached apiserver GET per reconcile on the binder/snapshot hot
path; ~0 latency by itself but meaningful steady-state load reduction and decoupling.

**Cluster:** local envtest sufficient.

---

### L8 — Manifest-leg checkpoint watch  *(main tree fix — done)*

**Why:** for the **tree** scheme, the dominant cost is the manifest leg reaching `ManifestsArchived`
(~5–6s on the critical path), not the data leg or the Ready mirror (those were the leaf-only costs
L1 addressed). The `ManifestCheckpointController` watched only `For(&ManifestCaptureRequest{})` and
**not** the `ManifestCheckpoint` it manages, so `finalizeMCRIfCheckpointHandedOff` polled at a fixed
`RequeueAfter: 500ms` while waiting for `SnapshotContent` to adopt the checkpoint (ownerRef handoff).
That 500ms gap multiplies with manifest count / tree depth.

**Change:** add a reverse watch on `ManifestCheckpoint` keyed by `spec.manifestCaptureRequestRef`
(`mapManifestCheckpointToMCR`) in `SetupWithManager`. The checkpoint is controller-owned by the
execution `ObjectKeeper` (not the MCR), so `Owns()` cannot route it — the spec back-reference is the
stable link. The handler only enqueues; the reconcile recomputes from truth refs, so a stale ref
simply yields no enqueue and the 500ms self-requeue still converges (safety net retained). This is the
exact L1 pattern applied to the manifest leg.

**Acceptance:** MCR finalizes within one watch hop of the SnapshotContent ownerRef handoff instead of
up to 500ms later; tree `ManifestsArchived` wall-clock drops proportionally to manifest count.

**Tests:** `TestMapManifestCheckpointToMCR` (valid back-ref enqueues; nil/missing-name/missing-ns/wrong
type yield no enqueue). Cluster re-measure of the VM tree after redeploy.

**Cluster:** local unit sufficient for the mapper; **YES** for the latency re-measure.

**Open follow-up (addressed by L9a below):** the symmetric 500ms gap is on the `snapshotcontent` side —
it adopts the checkpoint and aggregates `ManifestsReady/Archived` off its own 500ms self-requeue because
the MCP is `ObjectKeeper`-owned (not content-owned) until adoption, so the existing artifact wake-up
watch (routed by ownerRef only) does not fire pre-adoption.

---

### L9a — Dual-path artifact routing for the MCP wake-up  *(snapshotcontent side — done)*

**Why:** this is the symmetric half of L8. The `snapshotcontent` artifact wake-up watch routes a
`ManifestCheckpoint` event to its owning content **by ownerRef only**. Before adoption the MCP is
controller-owned by the execution `ObjectKeeper`, not by the `SnapshotContent`, so that watch finds no
SnapshotContent ownerRef and **drops** the event — the content only discovers a `Ready` MCP on its own
500ms self-requeue. This is the classic wake-up⇄adoption cycle: the watch needs an ownerRef to wake the
content, but the ownerRef is only added once the content wakes up and adopts.

**Change (routing only — ownership model unchanged):** make the MCP mapper *dual-path*.

- *path 1 (ownerRef):* unchanged — once adopted, route by the SnapshotContent ownerRef like every other
  artifact.
- *path 2 (pre-adoption reverse lookup):* when there is no SnapshotContent ownerRef yet, resolve the
  owning content via the deterministic **1:1** link `content.status.manifestCheckpointName == mcp.Name`,
  backed by a new cache field index (`indexKeyManifestCheckpointName` =
  `.status.manifestCheckpointName`, registered per content GVK in `SetupWithManager`).

Adoption logic is untouched: the MCP is still `ObjectKeeper`-owned before adoption and SnapshotContent-
owned after. Only the *resolver* gains a second path. Safety rests on two verified invariants:
`status.manifestCheckpointName` is set-once (derived from the per-content MCR UID, immutable after
publish) and globally 1:1, so the reverse lookup can only ever mis-*time* a wake-up (spurious enqueue →
idempotent no-op reconcile, or a missed enqueue → the 500ms self-requeue still backstops) — never pick a
wrong owner. The mapper is read-only and never writes status/ownership.

**Acceptance:** content wakes within one watch hop of the MCP becoming `Ready` instead of up to 500ms
later; combined with L8 the manifest-leg 500ms handshake is removed on both sides.

**Tests:** `TestExtractManifestCheckpointNameIndex` (index projection) and
`TestMapManifestCheckpointToContent_DualPath` (ownerRef path wins when present; reverse-lookup resolves
pre-adoption; unknown name and nil object resolve to nothing). Cluster re-measure of the VM tree after
redeploy.

**Cluster:** local unit sufficient for mapper + index; **YES** for the latency re-measure.

### L9c — Benign finalizer-add conflicts  *(contention noise — done)*

**Why:** the same `SnapshotContent` is reconciled by several controller instances that share one
`Reconciler` (the `For`-content controller and the per-snapshot status-watch controllers). Two workers
can race on the parent-protection finalizer `Update`, and the loser surfaced a 409 as a `Reconciler
error` → rate-limited backoff (200ms→10s) for nothing, adding latency variance under concurrency.

**Change:** in the finalizer-add path, treat `errors.IsConflict` as **benign** — log at V(1) and
`Requeue` to re-read instead of returning the error. `AddFinalizer` is idempotent, so whichever writer
lands first wins and the next pass is a no-op. Non-conflict errors are still surfaced.

**Acceptance:** no `Reconciler error` / backoff from finalizer races; finalizer still converges.

**Tests:** covered by the existing `snapshotcontent` reconcile suite (no behavior change on the
non-conflict path).

**Open follow-up (L9b, deferred):** once L9a makes the manifest leg reliably event-driven, lengthen the
`snapshotcontent` 500ms self-requeue to a 5–10s safety-net interval to cut steady-state reconcile load.
Validate against the reconcile-count harness before changing.

---

### L2b-ssc — Parallelize manifestcapture controller  *(scalability — done)*

Raise ManifestCaptureRequest/checkpoint controller concurrency from implicit 1 to 4 with a bounded rate
limiter. This targets the state-snapshotter-only bottleneck observed in parallel tree creation. It is
intentionally scoped to state-snapshotter; storage-foundation VCR concurrency remains unchanged and may
become the next bottleneck after this change.

**Why:** with `SNAP_TREES=5` concurrent VM-snapshot trees, per-tree time-to-Ready grew almost linearly
(~12s solo → ~52s avg / ~60s wall). The `ManifestCaptureRequest` (checkpoint) controller ran with the
implicit `MaxConcurrentReconciles=1`, so independent MCRs queued behind one worker.

**Pre-change safety checks (all cleared):**
- No global mutex / singleflight inside the checkpoint controller.
- MCP / chunk / ObjectKeeper names are deterministic per MCR (`GenerateManifestCheckpointNameFromUID`,
  UID-aware `ManifestCaptureRequestObjectKeeperName`); different MCRs never collide, and the already-exists
  path rejects a keeper owned by another MCR (UID mismatch).
- MCR/MCP status writes are idempotent `Patch`/`Update`-with-retry; controller-runtime still serializes
  reconciles of the *same* object.
- OwnerRef handoff uses per-object refs, no shared mutable state — **except** `loadConfigFromConfigMap`,
  which rewrote the shared `*config.Options` manifest fields (`MaxChunkSizeBytes`, `DefaultTTL`,
  `DefaultTTLStr`) on every reconcile. Under concurrency this is a data race, so those fields are now
  guarded by a `configMu` RWMutex with small accessor helpers; readers snapshot the value once.

**Change:** `manifestcapture/checkpoint_controller.go` — `controller.Options{ MaxConcurrentReconciles: 4,
RateLimiter: 200ms→10s }` (rate limiter already present); add `configMu` guard around the config fields.

**Validation:** `SNAP_TREES=1` must not regress; `SNAP_TREES=5` wall-clock should drop off the linear
~52–60s (realistically ~30–45s, not necessarily 15–25s, because the foundation VCR/data-leg is still a
single worker); no rise in conflicts/errors. If effect holds and no conflicts appear, try 8.

**Tests:** all `manifestcapture` unit tests green under `-race`; full controller unit suite green.

**Cluster result (deployed `latency-cut` @ 1ddcea9):**
- `SNAP_TREES=1`: leaf 2.0–2.6s; single tree 14–15s — no regression (prior variance 12–18s).
- `SNAP_TREES=5`: wall 63.9s, per-tree min 47.4 / p50 52.2 / avg 53.9 / p90 56.5 / max 63.9s —
  **essentially unchanged vs baseline (~60s wall / ~52s avg).**
- Conflicts/errors: only benign L9c "Finalizer add conflicted … (benign)" at DEBUG; no real errors,
  no stuck MCR/MCP.
- Single-tree per-hop breakdown: bound content @ ~1s; `content/ManifestsReady`+`VolumesReady` @ ~6–10s
  (the CSI/VCR **capture leg**); `content/ManifestsArchived`+`Ready` @ ~13–14s (the archived latch tail).

**Conclusion:** the manifest/MCR path is **not** the concurrency gate — manifests become ready *together
with* volumes and the tail is the archived latch, not checkpoint creation. Parallelizing the MCR
controller is therefore safe and correct but does not move the 5-tree wall clock. The remaining
serialization is downstream: the **storage-foundation VCR / data-leg still runs single-worker**
(`MaxConcurrentReconciles=1`) and gates the capture leg under concurrency — exactly the predicted
"bottleneck moves to foundation/VCR" outcome. Next lever: **L2b-foundation** (raise VCR concurrency in
storage-foundation), tracked separately since it is out of this state-snapshotter-only iteration.

---

### L2b-foundation — Parallelize the VolumeCaptureRequest controller  *(scalability — storage-foundation)*

Raise the storage-foundation **VolumeCaptureRequest** controller concurrency from implicit 1 to 4 with a
bounded rate limiter. This targets the capture-leg serialization that L2b-ssc showed to be the real gate
under concurrent tree creation. Scoped to the VCR controller only — no VSC-watch changes, no edits to the
other storage-foundation controllers.

**Change:** `images/controller/internal/controllers/volumecapturerequest_controller.go` —
`WithOptions(controller.Options{ MaxConcurrentReconciles: 4, RateLimiter: 200ms→10s })`.

**Pre-change safety checks (all cleared):**
- No shared mutable state in the reconciler struct (`Client`, `APIReader`, `Scheme`, `Config`); `Config`
  is set at startup and never rewritten in reconcile (unlike the ssc checkpoint controller — no config
  guard needed).
- No package-level or shared caches/maps/slices; only function-local maps (e.g. `validateSnapshotTargets`
  `seen`).
- Deterministic per-VCR names: `objectKeeperNameForVCR(vcr.UID)`, `snapshotVSCName(vcr.UID, hash(targetUID))`
  — different VCRs never collide.
- Idempotent create/patch: VSC uses Get-before-Create on a deterministic name; status uses
  `RetryOnConflict` + re-Get + `MergeFrom` patch.
- `RetryOnConflict` on status/finalize (bulk progress patch and `finalizeVCR`).
- No assumption that a single VCR reconcile runs globally alone; controller-runtime still serializes
  reconciles of the same VCR, and parallelism only spans distinct VCRs.

**Acceptance:** `SNAP_TREES=1` no worse than current; `SNAP_TREES=5` wall clock notably lower;
`SNAP_TREES=10` degradation sub-linear; no rise in conflicts/errors/stuck VCR.

**Tests:** storage-foundation controller unit suite green under `-race`.

**Cluster result (deployed foundation `controller-799cd99d8`):**
- `SNAP_TREES=1`: leaf 2.1s, tree 15.4s — no regression.
- `SNAP_TREES=5`: wall 63.3s, avg 57.0s — **still unchanged.** VCR concurrency did not move the wall.
- Foundation log evidence: the 5 burst VCRs created their VSCs within ~1.7s of each other **and all 5
  logged `VolumeCaptureRequest completed` within ~4s** (11:13:05→11:13:09). Only 2 benign cleanup-race
  errors, no stuck VCR.

**Decisive finding:** VCR concurrency now works (5 capture legs finish in ~4s), so the **capture leg is not
the gate** either. Both capture legs (manifest via L2b-ssc, volume via L2b-foundation) complete within
seconds for all 5 trees, yet trees only reach Ready ~50s later. The remaining ~50s is **entirely
downstream in state-snapshotter**: the `SnapshotContent` `ManifestsArchived` subtree latch + re-mirror
propagation, which produced ~1400 reconciles in the 4-min window. This is the archived-latch tail flagged
for last — it is now the dominant and only remaining lever. `SNAP_TREES=10` was not run because VCR is
demonstrably not the bottleneck.

**Next:** investigate the `ManifestsArchived` archive-wave / re-mirror burst in `snapshotcontent` +
`genericbinder` (500ms self-requeue waves, redundant enqueues) — see re-mirror dedup and L9b notes above.

---

### L3b — SnapshotContent archived-latch tail (diagnose → fix)  *(state-snapshotter only)*

**Status:** trace instrumentation landed; cluster measurement pending.

**Context (from L2b-foundation):** both capture legs finish in ~4s for all 5 trees, but trees reach Ready
~50s later. The tail lives entirely in `snapshotcontent` Ready aggregation. Propagation is *already*
event-driven bottom-up (child ManifestsArchived → parent via `mapSnapshotContentToParentContent` ownerRef
watch; MCP via dual-path L9a; VSC via ownerRef), and the 500ms self-requeue is only a backstop — so the
tail is **not** explained by a missing watch. It must be measured before touching logic.

**Step 1 — trace only (no logic change), DONE:** `reconcileCommonSnapshotContentStatus` emits one
structured line per reconcile — `snapshotcontent trace` — with: `content`, `uid`, `gen`, `childRefs`
(declared child count), the five leg statuses (`manifestsReady`/`volumesReady`/`childrenReady`/
`manifestsArchived`/`ready` as T/F/U), `gate` (`plan.readyReason` — exactly which leg still blocks Ready),
`patch` (`changed`/`noop`/`conflict`/`patch-error`), and `durMs`. This lets a TREES=5 burst be reduced to
a per-content timeline: leaf archived → parent sees child archived → parent patches archived → snapshot
mirror Ready, plus the noop-spin and conflict counts.

**Main hypothesis to confirm/refute:** capture done ~4s, but the bottom-up archive latch is driven mostly
by 500ms self-requeue (event wakeups arriving as no-ops before the child transition is observable), and/or
`genericbinder` re-wakes already-mirrored snapshots, so the queue/conflicts stretch Ready to ~50s.

**Candidate fixes (only after the trace confirms which one), by priority:**
- **A.** event-driven wakeup from child SnapshotContent status update (verify the child→parent watch
  actually fires on the ManifestsArchived transition, not just on create).
- **B.** changed-gate/dedup for ManifestsArchived/Ready — do not requeue when already final.
- **C.** `genericbinder`: dedup re-mirror enqueue — do not re-wake bound snapshots once Ready is mirrored.
- **D.** if a tail remains: raise/verify snapshotcontent concurrency + conflict retry.
  - Note: `aggregateChildrenManifestsArchived → declaredNonLeafChildContentNames` does an **uncached
    `APIReader.Get`** on the owning Snapshot *every* non-leaf reconcile (controller.go ~878). Under the
    500ms wave × 8 workers × N trees this is a per-reconcile direct-API cost worth quantifying from `durMs`.

**Acceptance:** TREES=1 ≤ ~15s; TREES=5 wall well below 60s; capture-done ~4s must not carry a +50s Ready
tail; snapshotcontent reconcile count drops multiplicatively.

**Step 2 — TREES=5 burst measured (trace deployed), DONE.** Wall ~57s (matches baseline). The trace
**refutes hypotheses A/B/C**: only **79** `snapshotcontent trace` lines for the whole 5-tree burst (40
`noop` / 39 `changed`) — there is **no 500ms self-requeue storm and no re-mirror flood**. Instead the
reconcile *itself* is slow: **durMs mean 4.5s, median 3.0s, max 14.8s**, summing to ~355s of reconcile
wall-time; divided by the 8 workers that is ~44s ≈ the observed tail. Every ~14s reconcile is a **non-leaf
(parent) content** (`gate=ChildrenPending`/`Completed`); leaf disk contents are fast.

**Confirmed root cause (not A/B/C/archive-wave):** the shared manager client uses the client-go **default
rate limit QPS 5 / Burst 10** (`kubutils.KubernetesDefaultConfigCreate` sets neither, and `main.go` passes
it straight to `manager.New`). Every controller sharing `mgr.GetClient()`/`mgr.GetAPIReader()` draws on
that one 5 QPS limiter. A parent SnapshotContent reconcile does an **uncached `APIReader.Get`** on the
owning Snapshot (declared-child set, controller.go ~878) plus a status patch; under a concurrent
multi-tree burst those requests queue behind the 5 QPS token bucket, inflating a single reconcile to
4–15s and serializing the tree-Ready tail regardless of `MaxConcurrentReconciles=8`. This is the same
limiter the `Snapshot` capture path already bypasses with a `QPS 100 / Burst 200` config copy.

**Fix applied (cmd/main.go):** raise the shared manager client to **`QPS=50 / Burst=100`** before
`manager.New`. Low-risk, in-repo precedent (capture clients use 100/200), single-line lever.

**Step 3 — cluster validation after redeploy, DONE. Decisive win:**

| Metric | Before (QPS 5/10) | After (QPS 50/100) |
|---|---|---|
| TREES=5 wall-clock | ~57s | **6s** (first tree 3s) |
| TREES=1 | ~15s | **3s** |
| reconcile durMs mean | 4491ms | **125ms** |
| reconcile durMs median | 3001ms | **45ms** |
| reconcile durMs max | 14812ms | **847ms** |

**Step 4 — scale proof (QPS 50/100), TREES=1/5/10, DONE.** Confirms the QPS bump is an *enabling* fix (a
genuinely bottlenecked client), not a mask over runaway churn: per-tree work is flat and the controller
quiesces after Ready.

| Metric | T=1 | T=5 | T=10 |
|---|---|---|---|
| wall-clock | 1s | 8s | 12s |
| snapshotcontent reconciles / tree | 33.0 | 31.8 | 28.8 |
| genericbinder reconciles / tree | 27.0 | 32.4 | 29.6 |
| status patches (≈API writes) / tree | 11 | 13.2 | 9.5 |
| post-Ready reconciles (30s window) | 0 | 1 | 0 |
| reconcile durMs mean / max | 23 / 58 | 209 / 1097 | 258 / 1314 |
| conflicts / reconciler-errors | 0 / 0 | 4 / 1 | 22 / 1 |

**L3b result:** the long archived-latch tail was **not** caused by archive propagation logic. It was
caused by the manager client's default client-go rate limiter **QPS=5 / Burst=10**. Raising it to
**QPS=50 / Burst=100** makes throughput **sublinear up to TREES=10**, with **stable work per tree** (~30
reconciles/tree, flat across scale) and **no post-Ready storm**. Target (15–25s) is met with margin
(T=10 wall 12s). No changes to the archive-wave / re-mirror / self-requeue logic were required — A/B/C
were symptoms, not the cause. The per-reconcile diagnostic trace is retained at V(1) (debug), not INFO.

### L4-load — mirror-path cached reads *(separate future item, do NOT mix with the latency fix)*

**Scope (only these two — both are hot mirror-path reads of a *watched* SnapshotContent):**
- `genericbinder/controller.go:673`
- `snapshot/content_reader.go:30-34` (`snapshotContentReader`)

**Change:** `APIReader.Get(SnapshotContent)` → `Client.Get(SnapshotContent)`.

**Rationale:**
- SnapshotContent is watched (content watch enqueues the bound snapshot on Ready/ManifestsArchived change);
- stale cache is safe: at most one extra reconcile before convergence;
- the Ready mirror is event-driven;
- INV-RECONCILE-TRUTH remains the backstop;
- removes a direct apiserver GET on every mirror pass (~30 mirror reconciles/tree at scale).

**Acceptance:**
- APIReader call count on the mirror path drops (unit test asserts the read routes to Client, not APIReader);
- TREES=1/5/10 do not regress;
- post-Ready storm stays 0;
- Ready mirror does not stall;
- conflicts/errors do not grow.

**Explicitly out of scope (do NOT change):**
- `genericbinder/controller.go:822` (child Snapshot existence) and `snapshot/parent_graph.go:328` (graph child
  walk): higher risk of delaying planning/binding on cache lag, smaller payoff.
- **Do NOT cache `declaredNonLeafChildContentNames` owner reads (`snapshotcontent:922,949`): they are
  correctness-critical for the one-way `ManifestsArchived` latch** — a stale, smaller declared set could
  permanently mislatch the archive over an unlinked subtree (duplicate-root-capture). Pinned by
  `TestOwnerSnapshotReadStaysOnAPIReaderForDeclaredChildren`.

**Other load-shaving (independent, later):** reduce benign conflicts observed at N≥10 (0→4→22 across
T=1/5/10); changed-gate/dedup on ManifestsArchived/Ready to trim no-op passes; validate at TREES=20/50.

---

### L6 — e2e latency assertion / report  *(validation)*

**Change:** an e2e test that creates the standard scheme and asserts wall-clock to `Snapshot.Ready`
under a threshold (e.g. < 25s), emitting the per-hop breakdown from L0a.

**Acceptance:** green under target; produces a per-hop latency report artifact.

**Cluster:** **YES** — real cluster with CSI driver; see `testing/e2e-testing-strategy.md`.

## Sequencing & dependencies

```
L0a trace instrumentation ─┬─► L1 VSC watch
                            ├─► L2 binder concurrency
                            ├─► L3 binder reverse-watch ──► L7 cached-read cleanup
                            └─► L5 content backoff
L0b baseline cluster run ───────────────┐
L1/L2/L3/L5/L7 after implementation ────┼─► L6 e2e latency report
L4 optional stopgap ─────────────────────┘
```

- **L0a (local instrumentation) is the only hard prerequisite** for the code fixes — it does not
  need a cluster, so it must not block anything.
- **L1, L2, L3, L5 are independent** and can be done in parallel right after L0a.
- **L0b (cluster baseline) runs in parallel** with code work; it informs numbers but does **not**
  gate the code fixes.
- **L4** is conditional (only if L1/L3 slip).
- **L7** depends on L3 (its watch must land first), then flips the four `APIReader` reads to cache.
- **L6** depends on L0a (trace), L0b (cluster), and ideally on L1–L3/L5 to demonstrate the target.

## Target outcome

| Milestone | Expected scheme wall-clock |
|---|---:|
| Baseline (today) | 45–60s |
| + L4/L5 stopgap only | ~30s |
| + L1 + L2 + L3 (recommended) | **15–22s** |
| Hard floor (driver/cache bound) | ~8–12s per leaf |

## Where a live cluster is required

- **L0b** baseline measurement run (real CSI driver) — to replace estimates with facts.
- **L1/L2/L3/L4** wall-clock confirmation (development + correctness are local via unit/envtest).
- **L6** e2e latency assertion.
- Plus a one-off measurement of **CSI `readyToUse` time for an empty volume** in the target driver,
  to know how much of hop 2 is genuine CSI latency vs. poll dead-time.

Cluster-dependent probes are tracked separately from code changes. Code changes L1–L5 must remain
developable and testable via unit/envtest.

## Risk & rollback

- L2 (concurrency) carries the most correctness risk — gate on the conflict-safety checklist and
  keep it revertable (single option flag).
- L4 interval cuts are config-only and trivially revertable.
- L1/L3 add watches (additive); risk is a dead/incorrect mapper — covered by envtest.
