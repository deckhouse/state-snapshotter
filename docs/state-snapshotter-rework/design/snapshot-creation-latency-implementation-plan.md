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
