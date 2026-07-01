# Snapshot creation latency: how much can we cut, and how

> **Status:** optimization study (design/). Companion to
> [`snapshot-creation-latency-analysis.md`](./snapshot-creation-latency-analysis.md) (the baseline).
> Not a normative contract. Savings are **engineering estimates** from static reading of
> `snapshot-sdk-v1` + `storage-foundation`; validate with a real trace before committing scope.

## Goal & baseline

Baseline for the standard scheme (VM with 1 data disk + 1 standalone data disk): **45–60s**
observed; ~36s typical critical-path budget for a single data leaf. The time is **not** spent on
work — it is spent crossing ~6–8 controller boundaries in series, several detected only by fixed
polls with no (or incomplete) watch, plus a bottom-up child→parent archive serialization for the
tree.

Question: which levers cut the most, at what cost, and what is the realistic floor.

## The latency floor (what we can never remove)

Even with everything event-driven and zero poll dead-time, a data leaf still pays:

- **CSI snapshot creation** in the external-snapshotter (driver-dependent; assume ~1–3s for an
  empty volume — measure this in your driver, it may be the real dominant term).
- **Cross-pod informer cache propagation** across ~6 hops (~0.3–0.5s each ≈ 2–3s).
- **Manifest leg** (MCP + chunk write + ownerRef handoff): ~1–2s of real writes.

**Floor ≈ 6–10s per leaf.** The VM tree adds one level of (unavoidable) child→parent ordering on
top. Anything above the floor is poll dead-time and serialization we can attack.

## Optimization levers (ranked by leverage)

| # | Lever | Mechanism | Est. saving | Effort | Risk | Confidence |
|---:|---|---|---:|---|---|---|
| L1 | **VSC watch in foundation VCR** | event-driven CSI `readyToUse` instead of 5s poll with no watch | **5–10s** | M (days) | low–med | **high** (clear gap) |
| L2 | **Binder concurrency 1 → 8** | parallelize the 2–3 snapshots of the scheme through `genericbinder` instead of serializing | **5–10s** (scheme) | S (hours) | low | **high** |
| L3 | **Complete binder reverse-watch → drop 5s Ready fallback** | ensure the watch covers the *common* `SnapshotContent` Ready transition so `controller.go:401` rarely fires | **2–5s × tree levels** | M | med | med (already partly watched) |
| L4 | **VCR pending requeue 5s → 1s** | cheaper stopgap if L1 not done yet | **~4s** | S (hours) | med (API load) | high |
| L5 | **Binder link/owner polls 5s → 1–2s** | hops 3 & 7 stopgap | **~3–6s** | S | med (API load) | high |
| L6 | **Back off 500ms content loop after legs observed** | load reduction, not latency | ~0s latency, ~5–10× fewer reads | S | low | high |
| L7 | **Reduce pod hops / co-locate responsibilities** | fewer cross-pod status round-trips | 2–4s | L (weeks) | high | low |

S = hours, M = days, L = weeks.

## Recommended roadmap

### Tier 0 — quick tuning (hours, reversible)

Ship L4 + L5 + L6 as interval/config changes. Pure latency-for-load trade:

- VCR pending `RequeueAfter`: `5s → 1s`
  (`storage-foundation/.../volumecapturerequest_controller.go:233,238`).
- Binder owner/link/Ready requeues: `5s → 1–2s`
  (`ssc/.../genericbinder/controller.go:277,341,377,401`).
- SnapshotContent loop: keep 500ms until first leg observed, then exponential back-off to a few
  seconds (`ssc/.../snapshotcontent/controller.go:356`).

**Expected:** ~36s → ~22–26s per leaf; fewer wasted reconciles overall (L6 offsets L4/L5 load).
**Caveat:** shorter intervals raise reconcile/API load on foundation + ssc — measure QPS.

### Tier 1 — event-driven readiness (days, the real fix)

- **L1:** add `Watches(&VolumeSnapshotContent, map→owning VCR)` in the VCR controller's
  `SetupWithManager` (`volumecapturerequest_controller.go:647`). Detection of CSI `readyToUse`
  becomes sub-second instead of "next 5s tick". This is the single clearest win and also removes
  most of the L4 load.
- **L2:** give `genericbinder` `WithOptions(MaxConcurrentReconciles: 8)` (and a bounded rate
  limiter like Snapshot/SnapshotContent) in `registerSnapshotWatch` /
  `builder.Complete` (`genericbinder/controller.go:858-874`). Today it is concurrency=1, so the
  VMS, its child VDS, and the standalone VDS are processed strictly one at a time — for the scheme
  this serialization alone is several seconds.
- **L3:** confirm the existing reverse content watch (`controller.go:860-863`,
  `mapBoundContentToSnapshots`) fires on the **common** `SnapshotContent` Ready transition that the
  aggregator actually writes; if it does, the 5s fallback at `:401` stops being on the critical
  path. If the watched GVK ≠ aggregating GVK, wire the watch to the common content GVK.

**Expected (cumulative with Tier 0 intervals as safety net):** ~22–26s → **~12–18s** per leaf;
VM tree converges close to leaf time + one propagation level.

### Tier 2 — structural (weeks, only if Tier 0+1 insufficient)

- **L7:** reduce the number of cross-pod hops (e.g. fewer status round-trips between binder and
  content, or co-locating the manifest+content responsibilities). Diminishing returns vs. risk;
  pursue only if the floor must drop below ~10s.

## Before / after budget (one data leaf, typical)

| Phase | Now | After Tier 0 | After Tier 1 |
|---|---:|---:|---:|
| Domain plan | 2 | 2 | 2 |
| Foundation VCR → CSI ready | 11 | 7 | 2–3 (≈ CSI actual) |
| Binder picks up VCR | 4 | 1–2 | ~0 (watch) |
| Manifest MCP build/handoff | 3 | 3 | 2–3 |
| Content aggregate | 3 | 2 | 1–2 |
| Archive wave (child→parent) | 4 | 3 | 1–2 |
| Ready mirror | 4 | 1–2 | ~0 (watch) |
| Cross-pod propagation | 5 | 4 | 3 |
| **Total (leaf)** | **~36** | **~23–25** | **~12–16** |
| **Scheme wall clock** | **45–60s** | **~28–35s** | **~15–22s** | (with L2 parallelism) |

## How much, in one line

- **Quick tuning (hours):** ~45–60s → **~30s** (≈ −40%), at the cost of more API load.
- **Event-driven readiness + binder concurrency (days):** ~45–60s → **~15–22s** (≈ −60–70%) **and**
  far fewer reconcile reads. This is the recommended target.
- **Hard floor (driver/cache bound):** ~8–12s per leaf; below that needs structural changes (Tier 2)
  with poor ROI.

## Validation plan (before committing scope)

1. Measure real CSI `readyToUse` time for an empty volume in the target driver — confirms how much
   of hop 2 is poll dead-time vs. genuine CSI latency.
2. Instrument per-hop timestamps (domain `ChildrenSnapshotReady` → VCR Ready → content
   `VolumesReady`/`ManifestsReady` → snapshot Ready) from controller logs for one snapshot.
3. Confirm L3: does the binder wake on common `SnapshotContent` Ready, or only poll?
4. Re-estimate L2 benefit by creating the scheme's snapshots simultaneously and watching binder
   queue depth.

## Caveats

- Savings are additive only where phases are independent; cross-pod propagation has a practical
  floor regardless of intervals.
- Tier 0 interval cuts trade latency for API/reconcile load — pair with L6 and watch apiserver QPS.
- The dominant data-leg term (L1) lives in `storage-foundation`, a separate repository.
