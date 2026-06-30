# Snapshot creation: interaction map, request budget & 45–60s latency analysis

> **Status:** analysis note (design/). Not a normative contract — does not change
> `spec/system-spec.md`. All figures are **static-analysis estimates** derived from the
> reconcile code on branch `snapshot-sdk-v1` (poll cadence × convergence time), **not measured**.
> Confirm against controller logs / a real trace before sizing any work.

## Scenario

The "standard scheme" analysed here:

- one **VM snapshot** (`DemoVirtualMachineSnapshot`) where the VM owns **one data disk**, plus
- one **standalone data-disk snapshot** (`DemoVirtualDiskSnapshot`).

The user creates **2** snapshot CRs; the VM snapshot fans out **1** child disk snapshot.

The path spans **three controller pods**:

1. `domain-controller` — plans capture requests, owns no content.
2. `storage-foundation` — executes the data leg (VCR → CSI `VolumeSnapshotContent`).
3. `state-snapshotter-controller` (ssc) — `manifestcapture` → `genericbinder` → `snapshotcontent`.

## Headline finding

Even a 0-byte PVC takes 45–60s **not because of heavy work**, but because the pipeline crosses
~6–8 controller boundaries **in series**, and several boundaries are detected only by **fixed 5s
polls with no watch** — chiefly the foundation VCR→CSI readiness loop and the generic-binder Ready
mirror. The VM tree adds a bottom-up **child→parent archive serialization** on top.

## What gets created (~24 objects)

| Object | Count | Created by (pod) | Role |
|---|---:|---|---|
| `DemoVirtualMachineSnapshot` (VMS) | 1 | user | VM-manifest root, 1 child ref |
| `DemoVirtualDiskSnapshot` — VM child | 1 | domain · `EnsureChildren` | VM disk: data + manifest |
| `DemoVirtualDiskSnapshot` — standalone | 1 | user | disk: data + manifest |
| `ManifestCaptureRequest` (MCR) | 3 | domain · SDK | 1 per snapshot (manifest leg) |
| `ManifestCheckpoint` (+ObjectKeeper +chunk) | 3 (+3 +~3) | ssc · manifestcapture | manifest artifact |
| `VolumeCaptureRequest` (VCR) | 2 | domain · SDK | data leg — only data leaves |
| `VolumeSnapshotContent` (+ObjectKeeper) | 2 (+2) | storage-foundation | CSI physical snapshot |
| `SnapshotContent` (+root ObjectKeeper) | 3 (+2) | ssc · genericbinder | content carrier / aggregation |

Each data leaf (the VM's disk + the standalone disk) carries a full manifest + data leg; the VM
root is manifest-only with one child.

## Critical path — hop by hop (one data leaf)

"Detect" = how the next stage notices the previous one finished. Watch = event-driven (fast);
poll = fixed requeue (adds up to one interval of dead time per hop, every time).

| # | Stage | Pod | Detect mechanism | Typical |
|---:|---|---|---|---:|
| 1 | Plan MCR / VCR / children, set `ChildrenSnapshotReady` | domain | watch (no requeue) | 1–3s |
| 2 | VCR → ObjectKeeper + VSC; wait CSI `readyToUse` | foundation | **5s poll — no VSC watch** | 5–15s |
| 3 | Bind `SnapshotContent`; pick up VCR `Ready` | ssc · binder | **5s poll** | 0–5s |
| 4 | MCR → MCP + chunks + ownerRef handoff | ssc · mcp | 200ms / 500ms | 2–4s |
| 5 | Aggregate `ManifestsReady` + `VolumesReady` | ssc · content | 500ms self-requeue | 1–3s |
| 6 | Child→parent archive wave (VM tree only) | ssc · content | 500ms self-requeue | 2–5s |
| 7 | Mirror `SnapshotContent.Ready` → demo snapshot | ssc · binder | **5s poll — no content watch** | 0–5s |
| + | Cross-pod informer cache propagation × ~6 | all | — | 2–6s |

**Key file references** (branch `snapshot-sdk-v1`):

- Hop 2 (5s poll): `storage-foundation/images/controller/internal/controllers/volumecapturerequest_controller.go:233,238`;
  no VSC watch — `SetupWithManager` at `:647-651` only does `For(VolumeCaptureRequest)`.
- Hop 3 / 7 (5s polls): `ssc .../internal/controllers/genericbinder/controller.go:277,341,377,401`.
- Hops 5–6 (500ms self-requeue until Ready): `ssc .../internal/controllers/snapshotcontent/controller.go:351-357`.
- Hop 4 (200ms UID barrier / 500ms handoff): `ssc .../internal/controllers/manifestcapture/checkpoint_controller.go:330,571,584,590`.

## Latency budget (typical, one leaf)

Derived estimate (poll interval × hops), not a trace:

| Phase | Seconds (typical) |
|---|---:|
| Domain plan | 2 |
| Foundation VCR→CSI (5s poll) | 11 |
| Binder picks up VCR (5s) | 4 |
| Manifest MCP build/handoff | 3 |
| Content aggregate | 3 |
| Archive wave (child→parent) | 4 |
| Ready mirror (5s) | 4 |
| Cross-pod propagation | 5 |
| **Sum (one leaf)** | **~36** |

Bars sum to ~36s for one leaf; the VM tree's child→parent serialization plus missed-watch retries
and CSI variance push the observed wall clock to **45–60s**.

## Request volume over the run (estimated)

Object creation is cheap (~24 writes). The cost is **reconcile reads**: self-requeuing controllers
re-list/re-get their world every poll tick until Ready.

| Controller | Est. API reads / run | Why |
|---|---:|---|
| `snapshotcontent` (500ms × 3 nodes) | ~1500 | ~90 reconciles/node over ~45s; VM root spins the whole time waiting for its child's archive latch |
| `genericbinder` (5s × 3 snapshots) | ~320 | Ready mirror + link polls |
| `domain-controller` (plan + watch) | ~140 | first reconciles + child-watch re-reconciles |
| `storage-foundation` VCR (5s × 2) | ~120 | poll until CSI `readyToUse` |
| `manifestcapture` (once × 3) | ~60 | MCP build per MCR |

Mostly cached reads (cheap on the apiserver, but real reconcile CPU + churn). Order-of-magnitude only.

### Per-reconcile detail (first reconcile)

**`DemoVirtualMachineSnapshot` (domain):** 1 List (DemoVirtualDisk) + ~7 Get + 2 Create (child VDS,
MCR) + ≤3 Status patch. Re-runs on the child-create watch (+1 List, ~3 Get). No periodic requeue on
success.

**`DemoVirtualDiskSnapshot` data leaf (domain):** ~9 Get (VDS, disk, PVC via APIReader, VCR×2, MCR,
statuses) + 2 Create (VCR, MCR) + ≤3 Status patch. `RequeueAfter 500ms` while PVC missing.
Manifest-only disk (no PVC) skips the VCR: ~5 Get, 1 Create.

**`GenericSnapshotBinder` (ssc, per pass):** 1–N Get snapshot (per GVK) + ~6–10 Get
(MCR/VCR/content/child SCs) + 1 Create SnapshotContent (first pass) + 2–4 Patch
(ownerRef/dataRefs/mcpName) + `markCaptureDone` (Get+StatusPatch+Delete MCR/VCR). Default workqueue
limiter, `MaxConcurrentReconciles=1`, Ready mirror polls every 5s.

**`SnapshotContent` (ssc, per pass):** 1 Get content + (1+N) Get MCP/chunks + 1–N Get VSC per
dataRef + 1–N Get child SC + 0–1 Status patch (changed-gate). Repeats every **500ms** until
`Ready=True` — the single largest source of read volume.

## Root-cause bottlenecks (ranked)

1. **5s polls with no watch on the data leg (latency).** The foundation VCR controller creates the
   `VolumeSnapshotContent` then returns `RequeueAfter: 5s` until CSI sets `readyToUse`, but has **no
   watch on VSC**. An empty volume physically ready in ~1–2s is only noticed on the next 5s tick.
   The ssc binder then adds another 5s poll to pick up VCR `Ready`.
2. **Cross-pod, multi-hop serialization (latency).** Three controller pods hand off via CRD status;
   each boundary pays cache-propagation latency plus the next stage's detect interval. ~6–8
   sequential hops, several on a 5s cadence.
3. **Child→parent archive wave (latency, tree depth).** A VM-root `SnapshotContent` cannot reach
   `Ready` until its child disk content is Ready (monotonic `ManifestsArchived` subtree latch,
   `snapshotcontent/controller.go:351-357`). The tree converges bottom-up and strictly serially.
4. **500ms SnapshotContent self-requeue (load).** Dominant request amplifier: every content node
   re-evaluates its whole subtree twice a second for the entire 45–60s.

## Highest-leverage fixes

| Change | Where | Expected effect |
|---|---|---|
| Watch `VolumeSnapshotContent` in the VCR controller | `storage-foundation .../volumecapturerequest_controller.go:647` | Event-driven CSI readiness; removes a 5–15s poll wait (biggest single win) |
| Watch the bound `SnapshotContent` for Ready mirroring | `ssc .../genericbinder/controller.go:401` (comment notes the missing reverse watch) | Removes the 0–5s Ready-mirror poll per tree level |
| Shorten VCR pending requeue 5s → ~1s (or watch-driven) | `storage-foundation .../volumecapturerequest_controller.go:233,238` | Cuts dead time on the data leg even before adding the watch |
| Back off the 500ms content loop once legs are observed | `ssc .../snapshotcontent/controller.go:356` | Cuts reconcile read volume ~5–10× with little latency cost |

## Caveats

- All numbers are derived from static reading of reconcile code on `snapshot-sdk-v1`, not from a
  trace. CSI snapshot creation time for an empty volume is driver-dependent and was assumed ~1–2s.
- The `storage-foundation` VCR path is in a separate repository; its timing dominates the data leg
  but is outside `state-snapshotter`.
