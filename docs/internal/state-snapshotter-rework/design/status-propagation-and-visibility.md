# Status propagation and progress/degradation visibility

Design note for one behavioural model with two implementation phases. Normative contract lives in
[`spec/system-spec.md` §3.8 / §3.9.7](../spec/system-spec.md); reasons taxonomy and the condition model
are summarised in ADR [`snapshot-rework/2026-06-03-snapshot-conditions-model.md`](../../../snapshot-rework/2026-06-03-snapshot-conditions-model.md).
The Ready/Failed/Delete matrix is [`demo-domain-csd/07-ready-delete-matrix.md`](demo-domain-csd/07-ready-delete-matrix.md).

## 1. One model

`SnapshotContent.status.conditions[Ready]` always explains the current state of the node:

- **pending** — not ready yet, but expected (own requests still running, or children not all ready);
- **failed/degraded** — already broken or lost (own request artifact missing/failed, or a child failed).

Both states travel up through the **same** pipeline; nothing analyses state on the `Snapshot` side:

```
leaf SnapshotContent
  ManifestsReady=False / DataReady=False (specific artifact/request reason)
  Ready=False
        │
        ▼
parent SnapshotContent
  ChildrenReady=False
  Ready=False                  message: direct child + failed leaf + original reason/message
        │
        ▼
root SnapshotContent
  Ready=False (full chain)
        │
        ▼
root Snapshot
  Ready := verbatim mirror(bound SnapshotContent.Ready)
```

**Single aggregator (INV-COND2):** `SnapshotContent.Ready = ManifestsReady && DataReady && ChildrenReady`, computed only by
`SnapshotContentController.buildCommonSnapshotContentStatusPlan`.

**Mirror-only Snapshot (INV-COND4):** `Snapshot.Ready := mirror(SnapshotContent.Ready)` (status/reason/message
copied verbatim). After bind the Snapshot controller writes **no** semantic `Ready` state — neither pending nor
terminal; a terminal MCP failure flows through content (`ManifestsReady=False/ManifestCheckpointFailed`) and is
mirrored (eventual consistency). A namespaced `Snapshot` MUST NOT re-traverse children, read child conditions, or
build its own diagnostics. The only exception is the bridge for failures the content tree cannot yet represent:
a terminal child-`Snapshot` capture failure before any child SnapshotContent exists, and pre-publish
capture-planning failures (build/list targets, plan drift) that occur before requests are published to content.

**Subtree archive latch (INV-COND5):** `ManifestsArchived` is a separate, lifelong latch on `SnapshotContent`
(mirrored verbatim onto `Snapshot` and domain `XxxxSnapshot` by the same content→snapshot mirror path). It is
**not** a leg of the `Ready` formula. It records the irreversible fact that this node and every descendant content
node captured their manifests into a `ManifestCheckpoint` at least once:

- `True / Archived` — own manifest leg reached readiness once **and** every child content is `ManifestsArchived=True`.
  Once `True` it never re-opens, even if `ManifestsReady`/`Ready` later degrades or a child disappears (the capture
  already happened). `Snapshot.spec` is immutable, so there is no recapture and no generation bookkeeping.
- `False / Capturing` — transient: not archived yet and not failed (includes the fail-closed
  `NamespaceCaptureIncomplete` wait while capture RBAC is still missing).
- `False / Failed` — terminal: own manifest leg failed terminally **before** archive, or a descendant is
  `ManifestsArchived=Failed` (the subtree can never be archived). Sticky for that immutable Snapshot.

Primary consumer: the namespace-capture RBAC hook, which keeps the transient per-namespace `RoleBinding` while the
root `Snapshot` is not yet `ManifestsArchived` (neither `Archived` nor `Failed`).

## 2. Reason taxonomy (shared by both phases)

| Bucket | Reason | Where it is set |
|---|---|---|
| Pending | `ManifestCapturePending` | own MCP not yet published / not Ready |
| Pending | `DataCapturePending` | own data artifacts (VSC) published but not yet ready |
| Pending | `ChildrenPending` | some child SnapshotContent not Ready=True (non-terminal) |
| Pending (pre-bind, Snapshot only) | `ContentBindingPending` | Snapshot has no bound content / content has no Ready yet |
| Failed | `ManifestCheckpointFailed` | own ManifestCheckpoint terminally failed |
| Failed | `ArtifactMissing` | a published data artifact ref points to a missing artifact |
| Failed | `DataArtifactInvalid` / `DataArtifactNotSupported` | published data ref malformed / unsupported kind |
| Failed | `ArtifactFailed` | (Phase 2) a previously-ready artifact degraded |
| Failed | `ChildrenFailed` | a required child has a terminal Ready=False |

Reason priority on `Ready=False` (single reason chosen when several legs are unsatisfied):

```
manifestsFailed > volumesFailed > childrenFailed > manifestsPending > volumesPending > childrenPending > Completed
```

Terminal own-leg failures win over child failures; terminal failures win over pending; own legs win over
children at equal severity (manifest leg before volume leg). `PlanningReady` is a gate/barrier and is NOT part of this formula.

## 3. Message formats

**Pending, with progress count** (`formatReadyProgress`): `"<ready>/<total> ready"`, plus a capped pending list
(max 5, then `" (+N more)"`):

- Data: `waiting for volume snapshot artifacts: 2/5 ready; pending: pvc-a, pvc-b`
- Children: `waiting for child snapshot contents: 5/9 ready; pending: child-a, child-b`

**Terminal failure, with full leaf chain.** The diagnostic chain is built only in content aggregation. The
canonical `ChildrenFailed` message is parseable and pins the original leaf while updating the direct child at each
hop:

```
child SnapshotContent <direct-child> failed: leaf=<failed-leaf> reason=<original-reason> message=<original-message>
```

Example seen on the root Snapshot (3-level VM→Disk tree):

```
child SnapshotContent vm-content failed: leaf=disk-content reason=ArtifactMissing message=VolumeSnapshotContent/snap-abc for target pvc-1 is missing
```

Propagation rule when a parent inspects a terminal child `C`:

- if `C.reason == ChildrenFailed` — `C` is intermediate; reuse `leaf`/`reason`/`message` parsed from `C`'s
  message but set `direct-child = C.name`;
- otherwise — `C` is the failed leaf; set `leaf = C.name`, `reason = C.reason`, `message = C.message`.

## 4. Phase 1 — progress-aware Ready=False (this slice)

Scope:

- richer reason/message for `ManifestsReady=False` (`ManifestCapturePending`, `ManifestCheckpointFailed`) and
  `DataReady=False` (`DataCapturePending`, `ArtifactMissing`, `DataArtifactInvalid`/`NotSupported`);
- richer reason/message for `ChildrenReady=False` (`ChildrenPending` with count, `ChildrenFailed` with leaf chain);
- early `Ready=False` on SnapshotContent right after creation (no MCP name → `ManifestCapturePending`);
- `Snapshot.Ready` verbatim mirror; pre-bind transitional `ContentBindingPending` only;
- recompute degraded state correctly **if already woken** — e.g. SnapshotContent already `Ready=True`, a later
  reconcile sees a published artifact missing → `DataReady=False`/`ArtifactMissing` → `Ready=False`. This does
  not need a watch and is covered by a unit test.

Out of scope for Phase 1: damaged-artifact **wake-up** (watches/revalidation), restore/download validation, new
upload/download semantics.

The content message MUST NOT read the Snapshot for cosmetics (no content→snapshot dependency); the empty-MCP
message is generic: `waiting for manifest capture checkpoint to be published`.

## 5. Phase 2a — MCP/VSC degradation wake-up (this slice)

Phase 2a is **not** a new condition model — the model already exists (§1–§3). It only guarantees that a
degraded **durable artifact** (ManifestCheckpoint, VolumeSnapshotContent) automatically **wakes** the owning
`SnapshotContent` so the existing aggregator recomputes `ManifestsReady`/`DataReady`/`Ready` and the failure propagates up.

**Core split (model-wide):**

- **truth = explicit refs** in `status`/`spec` (`status.manifestCheckpointName`, `status.dataRefs[]`,
  `status.childrenSnapshotContentRefs[]`). Truth defines what MUST exist and be valid.
- **ownerRef = lifecycle / GC / wake-up routing / linkage self-healing**. ownerRef never decides membership or
  validation.
- **watch handler = enqueue only**, never writes conditions.

**No watermark (simplification accepted in 2a).** Classification is based **only** on the current artifact
state, never on a prior `Ready=True`. "If a published ref exists, the artifact MUST exist and be valid."

Manifest path (surfaced only through `ManifestsReady`):

- no `manifestCheckpointName` → `ManifestCapturePending`;
- `manifestCheckpointName` set, MCP **NotFound** → `ManifestCapturePending` (legitimate: the name is published
  before the MCP is created — `capture.go`);
- MCP `Ready=False` with terminal `Failed` reason → `ManifestCheckpointFailed` (terminal, MCP message kept);
- MCP `Ready=False` non-terminal / no Ready condition → `ManifestCapturePending`;
- MCP `Ready=True` → validate chunk integrity (below), then continue data validation.

Chunk **existence** (exact-ref GET, **only** when MCP `Ready=True`):

- iterate `MCP.status.chunks[]` (truth) and GET each `ManifestCheckpointContentChunk/<name>`, stopping at the
  first one that is `NotFound` → `ManifestCheckpointFailed` (terminal),
  message `ManifestCheckpoint <mcp> references missing chunk <chunk>`;
- **existence only — never content.** The reconcile check does NOT read/decode `.spec.data` and does NOT verify
  checksums; that is content validation and stays on the explicit read/download/archive path. The GET is
  **metadata-only** (`PartialObjectMetadata`) so the chunk payload is never transferred on reconcile;
- a transient (non-`NotFound`) GET error is returned as a reconcile error (requeue), **not** mapped to a terminal
  failure — a network blip must not falsely break the tree;
- **get-only, no list/watch.** The check uses the uncached `APIReader` on purpose: a cached client Get would make
  the controller cache start a chunk informer (implicit list/watch), which 2a forbids;
- **chunk deletion does not self-wake** a reconcile (no chunk watch). This is accepted: every reconcile recomputes
  correctly, and the read/download/archive path fails immediately on use regardless. So a chunk deleted under a
  `Ready=True` content may leave a stale `Ready=True` until the next reconcile (any MCP/VSC/child event, or a
  parent/child relay) wakes the content.

Data path (surfaced only through `DataReady`):

- `dataRef` published, VSC **NotFound** → `ArtifactMissing` (terminal: `dataRefs[]` is published only after the
  VSC exists and is owned, so a missing VSC is a real integrity loss);
- VSC exists but `readyToUse=false` / no ready → `DataCapturePending` (pending; **not** turned into a terminal
  failure in 2a);
- VSC ready → ok.

`ArtifactFailed` is **deferred** (not wired in 2a): `readyToUse=false` is treated as transient
`DataCapturePending`, consistent with "status reflects the artifact's current state, not its history".

**Wake-up watches (ownerRef-only, enqueue-only) — `INV-OWNCHAIN`:**

```
ManifestCheckpoint event → MCP.ownerRef → SnapshotContent → enqueue SnapshotContent
VolumeSnapshotContent event → VSC.ownerRef → SnapshotContent → enqueue SnapshotContent
child SnapshotContent Ready change → child.ownerRef → parent SnapshotContent → enqueue parent (already present)
```

- Handlers only build `reconcile.Request`; they never patch status. A missing/broken ownerRef is logged and
  dropped; the next reconcile still recomputes from truth (`INV-RECONCILE-TRUTH`).
- **No reverse-index** by `dataRefs[]` and **no chunk→content shortcut**. Chunk wake-up, if ever added, must hop
  `chunk → MCP.ownerRef → SnapshotContent` (intermediate parents are never skipped).
- Each watch is registered guarded by RESTMapping: a not-yet-installed CRD (e.g. `VolumeSnapshotContent` under
  envtest) degrades to "no watch" instead of failing startup; correctness is preserved by reconcile-time
  revalidation.

**VSC ownerRef self-healing.** On every reconcile the content re-asserts the `SnapshotContent` ownerRef on each
VSC referenced by `status.dataRefs[]` (same handoff shape as the snapshot-side publisher; idempotent;
best-effort). This keeps ownerRef-only wake-up robust without using `dataRefs[]` as a routing index. A missing
VSC is left to data readiness (`ArtifactMissing`); a deleting VSC is not patched.

**Propagation & sibling isolation** already hold via ancestor-chain aggregation (Phase 1): a terminal leg
(`ArtifactMissing`/`ManifestCheckpointFailed`) propagates as `ChildrenFailed`; a pending leg
(`DataCapturePending`/`ManifestCapturePending`) propagates as `ChildrenPending`; only the ancestor chain
degrades, ready siblings stay `Ready=True`; root `Snapshot` mirrors root content `Ready`.

**RBAC.** MCP and VSC already have `get/list/watch`; chunks already have `get` (used by the exact-ref check).
Chunk `list/watch` is **not** added. Chunk `delete` is unused (deletion is GC via ownerRef `Controller=true`);
removing it is an optional separate cleanup.

## 6. Out of scope (Phase 2a)

- `ManifestCheckpointContentChunk` `list/watch` and a **chunk→content wake-up** (so chunk deletion self-wakes a
  reconcile); chunk→content shortcut routing (any future chunk wake-up must hop `chunk → MCP → SnapshotContent`).
- Chunk **content/consistency** validation beyond existence (e.g. checksum re-verification in conditions); that
  stays on the read/download/archive path.
- Reverse-index wake-up by `status.dataRefs[]`.
- `ArtifactFailed` wiring; periodic polling; restore/download/upload semantics.
