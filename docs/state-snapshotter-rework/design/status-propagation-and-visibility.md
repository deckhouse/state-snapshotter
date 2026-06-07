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
  RequestsReady=False (specific artifact/request reason)
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

**Single aggregator (INV-COND2):** `SnapshotContent.Ready = RequestsReady && ChildrenReady`, computed only by
`SnapshotContentController.buildCommonSnapshotContentStatusPlan`.

**Mirror-only Snapshot (INV-COND4):** `Snapshot.Ready := mirror(SnapshotContent.Ready)` (status/reason/message
copied verbatim). After bind the Snapshot controller writes **no** semantic `Ready` state — neither pending nor
terminal; a terminal MCP failure flows through content (`RequestsReady=False/ManifestCheckpointFailed`) and is
mirrored (eventual consistency). A namespaced `Snapshot` MUST NOT re-traverse children, read child conditions, or
build its own diagnostics. The only exception is the bridge for failures the content tree cannot yet represent:
a terminal child-`Snapshot` capture failure before any child SnapshotContent exists, and pre-publish
capture-planning failures (build/list targets, plan drift) that occur before requests are published to content.

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
RequestsFailed > ChildrenFailed > RequestsPending > ChildrenPending > Completed
```

Terminal own-request failures win over child failures; terminal failures win over pending; own requests win over
children at equal severity. `DomainReady` is a gate/barrier and is NOT part of this formula.

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

- richer reason/message for `RequestsReady=False` (`ManifestCapturePending`, `DataCapturePending`,
  `ManifestCheckpointFailed`, `ArtifactMissing`, `DataArtifactInvalid`/`NotSupported`);
- richer reason/message for `ChildrenReady=False` (`ChildrenPending` with count, `ChildrenFailed` with leaf chain);
- early `Ready=False` on SnapshotContent right after creation (no MCP name → `ManifestCapturePending`);
- `Snapshot.Ready` verbatim mirror; pre-bind transitional `ContentBindingPending` only;
- recompute degraded state correctly **if already woken** — e.g. SnapshotContent already `Ready=True`, a later
  reconcile sees a published artifact missing → `RequestsReady=False`/`ArtifactMissing` → `Ready=False`. This does
  not need a watch and is covered by a unit test.

Out of scope for Phase 1: damaged-artifact **wake-up** (watches/revalidation), restore/download validation, new
upload/download semantics.

The content message MUST NOT read the Snapshot for cosmetics (no content→snapshot dependency); the empty-MCP
message is generic: `waiting for manifest capture checkpoint to be published`.

## 5. Phase 2 — damaged-artifact revalidation (next slice)

- Add artifact→content watches (ManifestCheckpoint, VolumeSnapshotContent) so a degraded artifact after
  `Ready=True` wakes the owning SnapshotContent.
- Wire `ArtifactFailed` for the "was ready, now degraded" case.
- Propagation up + sibling isolation already hold via ancestor-chain aggregation (Phase 1) — proven by tests.
- Chunk-level integrity depends on MCP/VSC surfacing degraded status (documented dependency).
- Full live/e2e pass over both slices.
