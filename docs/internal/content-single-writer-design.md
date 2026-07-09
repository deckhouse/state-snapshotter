# Content single-writer — «порядок» in who writes `SnapshotContent` (design)

Status: **DESIGN / not implemented.** Plan-only; no code changes yet. Companion to
`wave5-namespace-domain-design.md` (which first stated the "root reconciler is content-free" goal — this
doc finishes that migration and adds the single-writer rule for child edges).

Paths are relative to `images/state-snapshotter-controller/` unless stated otherwise.

---

## 1. Goal & non-goals

**Goal.** Bring order to *who* writes `SnapshotContent`. Two rules:

1. **Domains never write `SnapshotContent`.** This includes the in-process **namespace domain**
   (`internal/controllers/snapshot/` `SnapshotReconciler`), not only external/demo domain controllers.
   A domain publishes only onto its **own** `Snapshot.status` (`childrenSnapshotRefs`, `phase`, the
   mirrored `data` leaf). It never creates, binds, or patches a `SnapshotContent`.
2. **One writer per content field.** In particular `status.childrenSnapshotContentRefs` has a **single
   writer** — the `SnapshotContentController` aggregator — which projects the owner Snapshot's
   `status.childrenSnapshotRefs` into content edges once each child has settled. No append-only co-writers.

This is the user's model: *"один реконсайлер, который просто из детей `xxxSnapshot` перенесёт в content,
когда мы точно понимаем, что на `xxxSnapshot` дети устоялись."*

**Non-goals.**

- The **coverage-gate relax** (`Ready`→`Planned`, coverage from `VCR.spec.targets[]` / `subtreePlanned`)
  is a *separate* step (§8.5 / Block 5) and is not part of the single-writer refactor. The **eager-shell**
  work (§9) *is* the primary deadlock fix and is in scope.
- No change to the content **condition/Ready** model (`ManifestsReady`/`VolumeReady`/`ChildrenReady`/
  `Ready`, `subtreeManifestsPersisted`) **except** the §3.6 `ChildrenReady` read barrier that eager shells
  require (empty edges + declared children ⇒ `ChildrenLinkPending`, not `Ready`). Otherwise only *who
  writes which status field* changes.
- No API/CRD shape change (the `childrenSnapshotContentRefs` CEL is *strengthened* set-once in §3.4, not
  reshaped).

---

## 2. Current state — who writes `SnapshotContent` today

Writers grouped by the invoking controller. "core" = allowed to write content; "domain" = must become
content-free.

### 2.1 Core writers (keep)

- **`GenericSnapshotBinderController`** (`genericbinder/`) — content creator/binder:
  - children edges — `domain_content.go:81` `PublishSnapshotContentChildrenFromSnapshotRefs`
  - data leg (enrich + VSC ownerRef handoff + dataRefs) — `domain_content.go:188/195/199`
  - `manifestCheckpointName` — `controller.go:511`
  - import legs — `genericbinder/import.go:139/149/266/273/277`
- **`SnapshotContentController`** (`snapshotcontent/`) — aggregator: conditions, `Ready`,
  `subtreeManifestsPersisted`, `excludedRefs`, MCP/VSC ownerRef handoff, cascade teardown.
- **`VolumeSnapshotImportController`** (`volumesnapshotimport/controller.go:213/278/285/289`) — import.

### 2.2 Domain writer that violates rule #1 — namespace domain `SnapshotReconciler` (`snapshot/`)

| What it writes on content | Where | Category |
|---|---|---|
| `LinkChildVolumeContentRef` (orphan edge) | `orphan_pvc_volume_snapshot.go:571` | children |
| `PublishSnapshotContentChildrenFromSnapshotRefs` | `import.go:114` | children |
| `EnsureVolumeChildContent` (**creates** child content) | `orphan_pvc_volume_snapshot.go:556` | create |
| `EnsureVolumeSnapshotContentsOwnedByContent` + `PublishSnapshotContentDataRef(s)` | `orphan_pvc_volume_snapshot.go:561/564`, `volume_capture.go:186/189` | data |
| `PublishSnapshotContentManifestCheckpointName` | `capture.go:318`, `import.go:106`, `orphan_pvc_volume_snapshot.go:659` | MCP-name |

`wave5-namespace-domain-design.md` §1 already declared the target: *"the binder is the single creator of
`SnapshotContent` for all kinds including the root … the root reconciler is content-free."* For the
manifest / orphan / import legs the migration simply never landed — that is the "беспорядок".

### 2.3 The two `childrenSnapshotContentRefs` writers

Both are **append-only + optimistic-lock** precisely because there are two of them (they must not clobber
each other):

- `PublishSnapshotContentChildrenRefs` / `…FromSnapshotRefs` (`status_publish.go`) — domain children:
  resolve each non-leaf `childrenSnapshotRefs` entry to its bound content name
  (`ResolveChildSnapshotRefToBoundContentName`), ensure the parent→child ownerRef, append the edge.
- `LinkChildVolumeContentRef` (`volume_child_content.go:111`) — one orphan child-volume-node edge.

The append-only + 409-retry dance (`MergeFromWithOptimisticLock`) exists **only** to make two writers
safe. Collapse to one writer and that incidental complexity disappears.

### 2.4 The aggregator already resolves the full expected child set (read-only today)

`SnapshotContentController` already computes, from the owner Snapshot's `status.childrenSnapshotRefs`,
**both** kinds of expected child content — it just uses them to *fail-close*, not to *write*:

- domain children — `declaredNonLeafChildContentNames` (`controller.go:760`): resolves each non-leaf ref
  to its bound content name (skips VS visibility leaves).
- orphan leaves — `unlinkedOrphanChildContents` + `orphanChildContentNameFromVSLeaf`
  (`controller.go:1030/1096`): for each CSI `VolumeSnapshot` visibility leaf, derives the child
  volume-node content name from the VS UID (`ChildVolumeContentName`).

So the "single writer" is ~90% present already: the resolution logic lives in the aggregator; only the
**write** of `childrenSnapshotContentRefs` sits in the wrong controllers.

---

## 3. Target model

```
domain (namespace / demo / external)      creator = GenericSnapshotBinderController   main = SnapshotContentController
──────────────────────────────────       ──────────────────────────────────────      ──────────────────────────────
writes ONLY own Snapshot.status:          creates + binds SnapshotContent (EAGER):    SINGLE writer of ALL derived status:
  - childrenSnapshotRefs                     - spec.snapshotRef + deletionPolicy        content.status:
  - phase                                    - parent/child ownerRef                      - conditions / Ready
  - domainSpecificController                 - bind (boundSnapshotContentName)            - childrenSnapshotContentRefs (frozen)
    (mcr/vcr names, excludedRefs)            - finalizer                                  - manifestCheckpointName (MCR→MCP)
  - data (mirror)                          watches all xxxSnapshot (incl. VS, §11)        - data (VCR→data)
                                           writes NO status (content OR snapshot)       xxxSnapshot.status (sideways):
never touches SnapshotContent                                                            - commonController latches
                                                                                           (manifestCaptured / dataCaptured /
                                                                                            subtreeManifestsPersisted /
                                                                                            subtreePlanned)
                                                                                         - reaps MCR/VCR after durable handoff
                                                                                         - Ready mirror (owner via snapshotRef)
```

**Creator/main split (decision 2026-07-07; main-owned `commonController` update 2026-07-08).** The binder
is the *creator*: it creates the content object, writes `spec`, sets the parent/child ownerRef, binds
(`boundSnapshotContentName`), and manages the finalizer — and nothing on `status` (neither
`SnapshotContent.status` nor `xxxSnapshot.status`). The `SnapshotContentController` aggregator is the
*main*: it is the **sole writer of every `SnapshotContent.status` field**, including the
`manifestCheckpointName` (MCR→MCP) and `data` (VCR→data) projections that the binder / namespace domain
write today. **Under the main-owned decision (2026-07-08) the aggregator is ALSO the writer of the
snapshot's `captureState.commonController` latches** (`manifestCaptured` / `dataCaptured`,
`subtreeManifestsPersisted`, `subtreePlanned`) — written **sideways onto the `xxxSnapshot`**, exactly as it
already sideways-writes `Snapshot.Ready` via `patchOwnerReadyFromContent` — **and it performs the MCR/VCR
reap** (set the leg latch on the snapshot, then delete the transient request, in one pass). This collapses
today's two-pass handoff (main projects → binder wakes → binder latches+reaps) into a single main pass and
makes the binder a pure creator. Domains stay content-free (rule #1). This supersedes the earlier "binder
writes own-node legs" split below (§2.1 documents today's code; §3.1–§3.3 and §4 describe the target).

**Why the latches must be main-written and snapshot-native (suppression timing).** The domain SDK, when it
finds an MCR/VCR absent, does an **authoritative uncached read** of the leg latch on the snapshot and
recreates the request only if the latch is `false` (`capture.go:258-260`). The invariant is therefore: the
latch MUST be set on the `xxxSnapshot` **before** the request is deleted, **by the same actor that deletes
it**. A "compute on `content.status` + binder mirrors to snapshot" scheme would open a window between main's
delete and the binder's mirror in which the domain uncached-reads a still-`false` snapshot latch and
recreates a request main just reaped — churn. Hence main writes the snapshot's `commonController` latch
**directly** and reaps in the same pass; no content-side latch field, no mirror hop.

### 3.1 Child-edge single writer (aggregator)

In `reconcileCommonSnapshotContentStatus`, add one step that **computes and writes** the desired child
edge set, reusing the resolvers that today only read:

- desired set = `declaredNonLeafChildContentNames` (domain, resolved+bound) ∪ existing-orphan
  child-volume-node contents (from `unlinkedOrphanChildContents`' resolution, but keeping those that
  exist rather than reporting the unlinked remainder).
  *Superseded by §11 (2026-07-07): once `VolumeSnapshot` is a registered domain kind there is no orphan
  partition — every declared child (incl. kind `VolumeSnapshot`) resolves uniformly via
  `ResolveChildSnapshotRefToBoundContentName`; the desired set is just the resolved declared set.*
- for each edge: ensure the parent→child ownerRef (`ensureChildSnapshotContentOwnedByParent`,
  `controller.go:1124` — already present).
- write `status.childrenSnapshotContentRefs`.

**Write semantics — keep union/monotonic, drop optimistic-lock.** With a single writer there is no
write-war, so the field can be written with the normal condition-gated status patch alongside the
conditions block. It stays **monotonic** (edges added as children settle, removed only at teardown):
publish an edge only once its child is resolved+bound/created (same precondition as the current
publishers), and never drop an already-published edge just because a child is momentarily unresolvable
(preserve existing edges — the existing `validate*` fail-closed reads already keep the parent not-Ready
while a child is pending). This avoids churn from partial per-reconcile views.

**End-state — one atomic write of the frozen set.** The desired target (see §3.4) is stronger than
monotonic append: the single writer computes the **complete** frozen child set once and writes it in
**one** status patch, after which the field is **immutable**. The write fires only at the write barrier
(§3.5) — not merely at `phase>=Planned`, but once **every** declared child has materialized content.
Incremental monotonic append (above) is the acceptable **interim** while immutability is not yet landed; it
converges to the same set but over several passes.

### 3.2 Liveness / wake-up

No new watches strictly required — the aggregator **self-requeues every `defaultSnapshotContentRequeueAfter`
(500 ms) while `!ready`** (`controller.go:364`), which already drives the child→parent convergence wave.
The linking step piggybacks on that loop. Existing optimizations still apply:

- `mapSnapshotContentToParentContent` (child content ownerRef → parent) wakes the parent when a child
  content appears.
- `AddSnapshotStatusWatch` / `mapSnapshotStatusToBoundCommonContent` wakes a content on its owner's
  `boundSnapshotContentName`.

(Optional later optimization: also wake the parent content on owner `childrenSnapshotRefs` change and on a
declared child's `boundSnapshotContentName`; not required for correctness given self-requeue.)

### 3.3 Removals after the writer moves

- delete `LinkChildVolumeContentRef` and its call (`orphan_pvc_volume_snapshot.go:571`). *Timing (as
  executed): kept through Slice 1 — splitting the orphan create/link across controllers regressed
  orphan-wave convergence — and removed with the §11.6 dismantling (plan Block 3d). Until then the edge
  set has two append-only writers (aggregator + orphan linker), so the §3.4 immutability CEL must not
  land before the dismantling.*
- delete the `PublishSnapshotContentChildrenFromSnapshotRefs` call in the binder
  (`domain_content.go:79-88`) and in the namespace domain (`import.go:114`).
- `PublishSnapshotContentChildrenRefs` / `…FromSnapshotRefs` / `PublishSnapshotContentLeafChildrenRefs`
  either become internal helpers of the aggregator or are removed; the append-only + optimistic-lock
  comments become obsolete.
- rewrite the API field doc comment on `SnapshotContentStatus.ChildrenSnapshotContentRefs`
  (`api/storage/v1alpha1/snapshotcontent_types.go`): it currently documents "both writers are append-only"
  and names `PublishSnapshotContentChildrenRefs` / `LinkChildVolumeContentRef` — replace with the
  single-writer + immutable (frozen-set) contract (§3.4).

### 3.4 Make `childrenSnapshotContentRefs` immutable (frozen set)

Today the field carries an **append-only / no-shrink** CEL rule
(`self.size() >= oldSelf.size()`, `snapshotcontent_types.go:176`), introduced as belt-and-suspenders for
the **two** optimistic-locked append writers. Once there is a single writer that emits the complete frozen
set (§3.1 end-state), we **strengthen no-shrink to immutable (set-once)**: the frozen expected child set
(wave7 "Late Planned") is decided at `phase>=Planned` and must never mutate afterwards; a failed child
stays a node (E3 degradation), so there is never a legitimate reason to rewrite it.

**Precondition (hard):** immutability is only correct once the writer sets the **complete** set in a single
transition, at the write barrier of §3.5 (every declared child has content). If content is populated
incrementally (empty → +edge → +edge), an "immutable" rule rejects the 2nd write. So §3.4 lands **after**
§3.1's atomic frozen-set write, not before.

**CEL options** (validate on a real apiserver — envtest — since CEL is not checked by build/gofmt/controller-gen):

- **Option A — immutable once set (recommended):**
  `rule="oldSelf.size() == 0 || self == oldSelf"`. Allows the single first-set from empty, then freezes.
  Decouples from *when* content is created (the writer may still create content first and set children in
  its first complete pass). `self == oldSelf` on the list is O(n) equality — within the apiserver CEL
  estimated-cost budget (strictly cheaper than the rejected per-entry `oldSelf.all/self.exists` O(n·m)).
- **Option B — strict immutable:** `rule="self == oldSelf"`. Pure immutability, but a create carries no
  `oldSelf` so the field must be populated **at content creation** by the creator (binder) — a larger
  change coupled to the Late-Planned "create content with the frozen set" model. Prefer A unless we also
  move to create-time population.

**Must verify before enabling:** no code path *rewrites/shrinks* the field after first set — teardown
(cascade finalizers) and E3 degradation only **read** `childrenSnapshotContentRefs`, they must not patch it.
Any such writer would be rejected by the immutable rule and must be removed first.

Keep the rule O(1)/O(n) for CEL cost; keep `+optional` (a leaf has no children); update the field doc
comment (§3.3) in the same change.

**Upper floor — freeze the DECLARED set, not only the content set.** The Option A CEL above is the *lower*
floor: it freezes the durable content edge set (`SnapshotContent.status.childrenSnapshotContentRefs`). By
itself it is fail-*open* on the way in — nothing stops a domain from GROWING its declared child set
(`Snapshot.status.childrenSnapshotRefs`) after `phase>=Planned`. That growth is a wedge hazard: the
aggregator would try to append the new child's edge into the now-immutable content set, the CEL rejects it,
and the node hangs at `Ready=False/ChildrenLinkPending` forever. So the freeze needs a matching *upper*
floor on the declared set. Two enforcements (plan `sdk-children-planned-freeze`) provide it:

1. **SDK `EnsureChildren` guard** (`pkg/snapshotsdk/capture.go`). At `phase>=Planned` (and the terminal
   `Failed`) `EnsureChildren` rejects any GROWTH of the declared set — or change of the excluded set — with
   the typed `ErrChildrenSetFrozen`, **fail-closed and BEFORE any child CR is created** (side-effect-free
   reject). An idempotent re-publish of the same set stays a no-op at any phase. Recommended domain reaction:
   `sdk.Fail(GraphPlanningFailed)`.
2. **Namespace-domain re-plan skip** (`internal/controllers/snapshot/namespace_capture_run.go`).
   `reconcileNamespaceCapture` gates its whole plan+enumerate+freeze block (PublishSnapshotSource,
   `planNamespaceChildren`, `EnsureChildren`, the residual/orphan-PVC wave, `MarkPlanned`) behind
   `namespaceDomainPrePlanned`; past `Planned` the composition is frozen and re-planning is skipped, so a
   top-level object added to the namespace after `Planned` is never enumerated into a new child. A declared
   child deleted after `Planned` is deliberately NOT recreated and instead surfaces as terminal
   `ChildSnapshotLost` (see the ADR «Судьба исчезнувших объявленных детей»).

Together the two floors make the point-in-time child set immutable from both directions. See the ADR
«Фриз объявленного набора — enforced (SDK + домен), не только конвенция».

**Ordering (updated).** The original ordering constraint — "land the SDK guard *before* enabling Option A"
— is obsolete: Option A is **already committed and live** on `wave7`, so the lower floor is enforced while
the upper floor is added on top. That means the wedge window described above is already open, so the
upper-floor guard closes an existing hazard and must land as soon as possible (not gated behind the CEL).

### 3.5 Write barrier — commit only when every child has content

The precondition for writing `childrenSnapshotContentRefs` is **stronger than "children declared + owner
`phase>=Planned`"**. An edge points to a child's **SnapshotContent** (resolved today via the child's
`status.boundSnapshotContentName`, `ResolveChildSnapshotRefToBoundContentName`,
`internal/usecase/child_snapshot_resolve.go:72`), which exists only after the binder created+bound it. So
the write barrier is both:

1. `childrenSnapshotRefs` is complete/frozen — the Late-Planned enumeration (all children, **including the
   orphan volume leaves**, present — *per §11 orphan `VolumeSnapshot` entries are regular domain children,
   not a separate leaf category*); AND
2. every declared child has **materialized content**:
   - domain child — `boundSnapshotContentName` resolvable (`declaredNonLeafChildContentNames` → complete);
   - orphan leaf — its child-volume-node content exists (`unlinkedOrphanChildContents` → empty).
     *Superseded by §11: orphan `VolumeSnapshot` children are domain children — the first bullet covers
     them; the separate orphan condition disappears with the machinery.*

This is exactly the pair the aggregator already computes for its fail-closed Ready gate; the single writer
reuses it as the **write** gate. Under §11 the pair collapses to the first bullet alone — every declared
child (incl. kind `VolumeSnapshot`) resolves uniformly via `ResolveChildSnapshotRefToBoundContentName`.
Condition 1 (this node's plan frozen) is the OWN `planned` leg that may be
backed by the proposed `subtreePlanned` latch (§8.1); the write gate needs only that own leg, not the full
subtree recursion.

**Deterministic-name aside — why we still wait.** Content names are pure (`names.ContentName(uid)` =
`nss-content-<h16(uid)>`, `api/names/names.go:72`), so the full *name* set is technically knowable at
Planned before any child content exists. We deliberately do **not** write early: an edge must never dangle
(point at a not-yet-created content), which matters doubly once the field is immutable (§3.4). Wait for real
content.

**No cycle.** A child's content only needs the parent content to **exist** (created at parent Planned), not
the parent's `childrenSnapshotContentRefs`. Writing children late gates nothing upstream — the field lives
on the already-existing parent content.

**Incomplete set = pending, not partial (decision 2026-07-07).** If a declared child never materializes
content (e.g. it fails before its own Planned), the aggregator does **not** write a partial set and does
**not** time out: the parent stays `ChildrenReady=False` / `ChildrenLinkPending` (today's behavior). A
terminal child failure surfaces on the parent via the child's own Ready terminal reason (`ChildrenFailed`),
independently of the frozen-set write. The single immutable write fires only when the barrier is fully
satisfied.

**Content-exists (A) vs data-materialized (B) — the barrier is A, not B (correction 2026-07-07).** Two
distinct child milestones must not be conflated:

- **Milestone A — content exists / bound:** the child `SnapshotContent` object is created and the child's
  `status.boundSnapshotContentName` resolves. This is what §3.5 condition 2 requires; it is the write
  barrier for the edge.
- **Milestone B — `status.data` published:** `SnapshotContent.status.data.source.uid` appears only **after**
  the child's `VolumeCaptureRequest` completes and `PublishSnapshotContentDataRef(s)` runs. *Native-CSI
  domains (§11.4, kind `VolumeSnapshot`) reach B without a VCR — the aggregator projects `status.data` from
  the owner's CSI binding (`boundVolumeSnapshotContentName` → VSC); the A→B window exists identically.*

A happens **before** B. Therefore **an existing edge (barrier A) does NOT imply a materialized
`status.data.source.uid` (B)**. This matters for orphan/residual PVC coverage, which reads B
(`pvcUIDsFromSnapshotContentDataRefs` → `status.data.source.uid`,
`internal/usecase/volumecapture/subtree_covered_pvc.go:172`) with an in-flight-VCR fallback
(`pvcUIDsFromPendingVCR`, `spec.targets[].uid`, same file :189) precisely for the A→B window.

Consequence: **the A→B fallback in `coveredPVCUIDsForContent` must be kept** — moving edge-writing to the
single writer does not close the A→B window, so the fallback is still the only signal during it. For
VCR-based domains the fallback reads the in-flight VCR targets; for native-CSI domains (§11.7) it reads the
owner's `snapshotSource` PVC UID. The fallback could be removed **only** if the barrier is strengthened from
A to B (write the edge only once the child's `status.data` is published), which is a **stronger, separate
gate**: it delays `childrenSnapshotContentRefs` linking until each child's volume capture finishes and
couples edge-linking to capture completion. Not adopted here; noted as an option.

### 3.6 `ChildrenReady` read barrier — evaluate only after the frozen set is committed

**Constraint (2026-07-07):** `ChildrenReady` must be computed **only once `childrenSnapshotContentRefs` is
committed** for the node. Before that it is `False` (a "children not linked yet" reason, e.g.
`ChildrenLinkPending`), **never `True`**.

**Why eager shells (§9) force this.** Today `validateCommonContentChildren`
(`internal/controllers/snapshotcontent/controller.go:940-1009`) reads the LINKED edges and, when
`childrenSnapshotContentRefs` is **empty** with no unlinked orphans, returns `ChildrenReady=True`
("no child content", `controller.go:1006-1008`). With eager empty shells a parent content **exists early with
empty edges** even though it has declared children whose frozen set is not written yet — the current code
would read that transient as `ChildrenReady=True → Ready=True`, flipping a subtree Ready before its children
are even linked. So the (now real and long) empty-edges window must read **pending**, not ready.

**Leaf vs pending disambiguation.** Under set-once immutability (§3.4) an empty `childrenSnapshotContentRefs`
is ambiguous — a true leaf writes empty, a not-yet-written parent is also empty. Resolve by comparing the
**declared** child set (the node's frozen `childrenSnapshotRefs`; *under §11 there is no separate
owner-derived orphan set*) against the **linked** edges:

- declared children exist AND edges not yet the complete set → `ChildrenReady=False` (`ChildrenLinkPending`);
- declared set empty (true leaf) → `ChildrenReady=True` ("no child content") is correct.

This **generalizes today's orphan-only gate** `unlinkedOrphanChildContents` (`controller.go:995-1004`, which
already recomputes the declared orphan set and holds `ChildrenLinkPending`) to **all** declared children
(*and per §11 replaces that orphan-only gate entirely — `unlinkedOrphanChildContents` is removed with the
orphan machinery, §11.6*). Under the atomic frozen-set write (§3.1 end-state) there is no partial-link state:
edges are either empty (pending) or the complete set (evaluate readiness) — so the gate reduces to "declared
non-empty AND edges empty → pending".

**No cycle.** §3.5 writes edges once every child has **content** (A); §3.6 then evaluates each linked child's
**Ready** (recursive B/subtree). Edge-commit precedes readiness evaluation, so the two barriers compose
without a loop.

**Alternative:** an explicit "children frozen" marker (boolean/latch) written by the single writer at the §3.5
barrier, so `ChildrenReady` keys on the marker instead of re-deriving the declared set. The declared-vs-linked
comparison needs no new field; decide during migration.

---

## 4. Migration — staged, "по порядку"

Each slice is independently shippable and independently green.

### Slice 0 — eager shell creation (decision A, §9)

Prerequisite ordering fix. Decouple `SnapshotContent` **object** creation from `phase>=Planned` (§9): the
binder creates the empty shell (`spec.snapshotRef` + parent ownerRef) as soon as the node exists; edges and
data remain late. Land together with / before slice 1 so removing the Planned create-gate does not let an old
writer publish partial edges.

- audit + repoint any reader that treats "content exists" as "node planned/ready" (INV-CONTENT-CREATE-1).
- **add the §3.6 `ChildrenReady` read barrier (mandatory, same slice):** `validateCommonContentChildren`
  (`snapshotcontent/controller.go:940-1009`) must return `ChildrenLinkPending` when the node has declared
  children but empty `childrenSnapshotContentRefs` — generalize the orphan-only `unlinkedOrphanChildContents`
  gate to all declared children. Without this, eager shells read `ChildrenReady=True` prematurely
  (INV-CONTENT-CHILDREN-4).
- drop `isDomainPlanningComplete` as the create gate (`genericbinder/controller.go:259-261`); keep it (or the
  `subtreePlanned`/own-`planned` leg) only on the edge write.
- confirm empty-shell teardown/GC and the `spec.snapshotRef` handshake do not require Planned.
- **Acceptance:** the creation cycle (§9.2) is broken — root + demo tree reach `Ready=True` without the
  Planned↔children-Ready deadlock; a parent with an eager empty shell reads `ChildrenReady=False` until its
  frozen set is committed (not a premature `True`); orphan wave still green.

### Slice 1 — child edges → single writer (aggregator)

The current thread. Move child-edge projection into `SnapshotContentController`; remove the two external
writers. Child-volume-node content **creation** still lives in the namespace domain for now
(`EnsureVolumeChildContent`); the aggregator only *links* it once it exists. This is safe because linking
is exactly what the aggregator already resolves for its fail-closed gate. *(Interim only: the
child-volume-node path itself is dismantled by §11 in Slice 3 — see §11.6.)*

- **Acceptance:** demo tree (root Snapshot + child domain nodes) and the orphan/residual-PVC wave both
  reach `Ready=True`; `childrenSnapshotContentRefs` is written only by the aggregator (grep: no other
  caller); integration `n5_pr7` + non-isolated regressions green; e2e `capture`/`volumedata` green.

### Slice 2 — manifestCheckpointName projection → aggregator (main)

Move `PublishSnapshotContentManifestCheckpointName` off **both** the binder (`controller.go:511`) **and**
the namespace domain (`capture.go:318`, `import.go:106`, `orphan_pvc_volume_snapshot.go:659`) into the
`SnapshotContentController` aggregator (creator/main, §3 target). The aggregator projects the leg from the
owning `xxxSnapshot`'s `captureState.manifestCaptureRequestName` → MCR → MCP → `status.manifestCheckpointName`
(it already watches MCP via `artifactWakeUpGVKs`; add an MCR watch, which the binder has today at
`controller.go:942`).

**Orphan branch — no owning `xxxSnapshot`.** *Superseded by §11 (2026-07-07) — not implemented.* An orphan
child-volume-node content has no `xxxSnapshot` owner (its `spec.snapshotRef` points at the orphan CSI
`VolumeSnapshot`), so there is no `captureState`. For it the aggregator derives the manifest leg from
`spec.snapshotRef.uid` (the VS UID): `SnapshotVolumeMCRName(vsUID)` → MCR UID → MCP name. It must be
**latch-idempotent**: the domain deletes the per-orphan MCR once its MCP is Ready
(`orphan_pvc_volume_snapshot.go:594-607`), so once `manifestCheckpointName` is published the aggregator
keeps it even when the MCR is gone. *Under §11 the VS is a domain kind with its own `captureState`
(`manifestCaptureRequestName` published by the foundation domain controller), so the standard projection
covers it and this branch is never needed.*

### Slice 3 — data leg → aggregator (main) + VS-domain dismantling (content-free domain)

The largest slice; finishes rule #1 for the namespace domain (strict INV-CONTENT-WRITER-1).
**Re-cut 2026-07-07 per §11** — the orphan-specific sub-items below are superseded by the `VolumeSnapshot`
domain (§11.9); the slice now pairs with the foundation-side VS-domain blocks.

- **data leg → aggregator.** Move `EnrichDataBindingsWithVolumeMetadata` +
  `EnsureVolumeSnapshotContentsOwnedByContent` + `PublishSnapshotContentDataRef(s)` off the binder
  (`domain_content.go:188-199`) and the namespace domain (`orphan_pvc_volume_snapshot.go:534-564`,
  `volume_capture.go:179-189`) into the `SnapshotContentController` aggregator. For a domain data-leaf the
  aggregator reads `captureState.volumeCaptureRequestName` → VCR → VSC → `status.data`; for an owner of
  kind **`VolumeSnapshot`** (native-CSI domain, §11.4) it reads `owner.status.boundVolumeSnapshotContentName`
  → VSC → binding. Nobody watches VCR today (verified — the binder watches only snapshots/contents/MCR), so
  add a VCR watch to the aggregator or lean on its 500 ms self-requeue while `!ready` (§3.2).
- ~~**orphan child-volume-node content creation → binder (watch orphan VS leaves).**~~ **Superseded by §11
  (2026-07-07).** `VolumeSnapshot` is a registered domain kind (CSD): the binder watches it through the
  standard `AddWatchForPair` and creates+binds its content eagerly like any kind — no orphan-VS carve-out
  watch, no `ChildVolumeContentName` naming, no domain-side `boundSnapshotContentName` bind. Remove
  `EnsureVolumeChildContent` and the rest of the §11.6 table. The namespace domain keeps only: create the
  orphan `VolumeSnapshot` for each residual PVC and declare it via regular `EnsureChildren`.
- **restore `spec.snapshotRef` repoint → binder.** The recycle-bin restore repoint
  (`snapshot/static_bind.go` `repointContentSnapshotRef`) and the namespace static-bind content adoption
  also move to the binder (it writes `spec` universally as the creator) — the last remaining `SnapshotContent`
  writes under `internal/controllers/snapshot/`.

After slice 3, a grep for `snapshotcontent.` writers (create/patch/status/spec) under
`internal/controllers/snapshot/` returns nothing but read-only helpers (INV-CONTENT-WRITER-1 strict).

### Slice 4 — `childrenSnapshotContentRefs` immutable (frozen set)

Lands **after** the single writer emits the complete frozen set in one write (§3.1 end-state / §3.4
precondition). Steps:

- switch the aggregator to compute+write the complete frozen set in a single transition (not incremental
  append).
- strengthen the CEL rule from no-shrink to immutable (§3.4, Option A recommended:
  `oldSelf.size() == 0 || self == oldSelf`); regenerate CRDs (`hack/generate_code.sh`, after confirmation).
- verify no teardown/degradation path rewrites the field.
- update the field doc comment (§3.3).
- **Acceptance:** envtest proves the apiserver rejects any post-set mutation (shrink AND reorder/replace)
  while allowing the first complete set; demo tree + orphan wave still reach `Ready=True`; regressions green.

---

## 5. Invariants (must hold after each slice)

- **INV-CONTENT-WRITER-1:** no controller under `internal/controllers/snapshot/` (namespace domain) and
  no domain controller writes `SnapshotContent` (create/patch/status). Enforceable by a grep guard / lint.
- **INV-CONTENT-CHILDREN-1:** `status.childrenSnapshotContentRefs` has exactly one writer — the
  `SnapshotContentController` aggregator.
- **INV-CONTENT-CHILDREN-2 (after slice 4):** `status.childrenSnapshotContentRefs` is **immutable once
  set** — the frozen expected child set is written once and never mutated (API-enforced via the CEL
  transition rule, §3.4). This strengthens the interim no-shrink rule.
- **INV-CONTENT-CHILDREN-3 (edges never dangle):** an edge is committed only after the child's content
  exists (§3.5); the frozen set is written only when **every** declared child has content — an incomplete
  set is never partially written, the parent stays pending instead.
- **INV-CONTENT-CHILDREN-4 (ChildrenReady read barrier, §3.6):** `ChildrenReady` is `True` only after the
  frozen `childrenSnapshotContentRefs` is committed. While the node has declared children but empty edges
  (the eager-shell window, §9), `ChildrenReady=False` (`ChildrenLinkPending`) — an empty edge set is read as
  `True` **only** for a true leaf (declared child set empty). This generalizes the orphan-only
  `unlinkedOrphanChildContents` gate to all declared children (*that gate itself is removed by §11*).
- **Monotonic edges (interim, until slice 4):** an edge, once published, is removed only by teardown —
  never dropped on a partial per-reconcile view. Superseded by INV-CONTENT-CHILDREN-2 once the frozen set
  is written atomically.
- **Ready model — mostly unchanged, one addition:** the fail-closed reads
  (`validateCommonContentChildren`, `aggregateChildrenSubtreeManifestsPersisted`; *`unlinkedOrphanChildContents`
  until it is removed by §11*) keep their meaning and now read edges written by the same controller in the
  same pass. The one change is the §3.6 read barrier (INV-CONTENT-CHILDREN-4):
  `validateCommonContentChildren` must treat empty edges with a non-empty declared child set as pending, not
  `ChildrenReady=True` — required by eager shells (§9).

---

## 6. Testing

- **Unit:** aggregator child-projection — domain-only children, orphan-only leaves, mixed (*post-§11:
  "orphan leaves" become regular `VolumeSnapshot` domain children — same projection path*); partial
  (unresolved/unbound child) → edge withheld, existing edges preserved; teardown removal.
- **Unit (§3.6 read barrier):** eager parent shell with declared children + empty `childrenSnapshotContentRefs`
  → `ChildrenReady=False` (`ChildrenLinkPending`); true leaf (no declared children) + empty edges →
  `ChildrenReady=True`; committed frozen set → readiness evaluated over the complete linked set
  (INV-CONTENT-CHILDREN-4).
- **Integration (envtest):** `n5_pr7` orphan-wave (isolated CSI-simulator pass) + non-isolated
  regression; `snapshot_root_lifecycle` / `snapshot_recreate`.
- **e2e:** `capture` (root + demo tree `Ready`), `volumedata` (*post-§11: orphan PVC captured as a
  `VolumeSnapshot` domain child with data-bearing content* + domain-VCR coverage).
- **envtest CEL (slice 4):** apiserver accepts the first complete set on an empty field, then rejects
  every subsequent change — shrink, append, reorder, and full replace — proving INV-CONTENT-CHILDREN-2.
- **Guard:** grep/lint asserting INV-CONTENT-WRITER-1 and INV-CONTENT-CHILDREN-1 (single-writer).

---

## 7. Risks & mitigations

- **Churn from replace-set writes** → keep union/monotonic semantics (§3.1), not blind replace.
- **Latency of linking** relative to the old inline publish → covered by the existing 500 ms self-requeue;
  add owner/child wake-up watches only if a measured regression appears.
- **Slice 3 creation ownership** — RESOLVED 2026-07-07 by §11: `VolumeSnapshot` is a CSD-registered domain
  kind, the binder creates+binds its content through the standard pair watch; no orphan-only creation path
  remains.
- **Immutability vs incremental population (slice 4)** — enabling the immutable CEL rule before the writer
  emits the complete frozen set in one write would wedge every capture (2nd edge write rejected by the
  apiserver). Order is mandatory: atomic frozen-set write first (§3.1 end-state), then flip the rule (§3.4).
  Mitigation: land slice 4 only after an envtest proves the single-pass complete-set write.
- **Hidden field rewriter** — a teardown/degradation/restore path that patches `childrenSnapshotContentRefs`
  after set would be rejected once immutable. Audit (grep for status writes to the field) before slice 4;
  the single-writer work (slices 1–3) should already have removed all but the aggregator.
- **Frozen-set correctness depends on Late-Planned enumeration** — the immutable single write assumes
  `childrenSnapshotRefs` is genuinely complete at the write barrier (§3.5 condition 1: all children —
  domain kinds and orphan `VolumeSnapshot` children alike (§11) — enumerated by Planned). If a child is
  appended to `childrenSnapshotRefs` *after* the frozen set is committed, immutability locks it out.
  Mitigation: confirm the orphan-wave enumeration completes by the write barrier before enabling
  immutability (slice 4); keep the interim append semantics until then.

---

## 8. Related design — `subtreePlanned` latch & manifest-exclude

Captured from the design discussion (2026-07-07). These are adjacent to the single-writer work: the
content-write barrier (§3.5) needs a "plan is frozen" signal, and the manifest-exclude is another consumer
of the complete frozen `childrenSnapshotContentRefs` graph.

### 8.1 `subtreePlanned` latch (snapshot-native, resolved)

Analog of `subtreeManifestsPersisted` but for the planning phase. Definition (semantic):

```
subtreePlanned(n) = planned(n) AND every descendant of n is planned
```

Computed recursively as `planned(n) ∧ ⋀(c ∈ direct children) subtreePlanned(c)` — each child's latch
already encodes its own subtree, so a parent checks only DIRECT children yet transitively knows the whole
subtree (same fixpoint pattern as `subtreeManifestsPersisted`). `planned(n)` (own leg) = the node reached
Planned with its direct children (*all domain kinds — per §11 orphan `VolumeSnapshot` entries are regular
domain children*) fully enumerated in `childrenSnapshotRefs` (Late-Planned). Monotonic (spec is immutable;
no recapture).

Two consumers, at DIFFERENT levels:

- **content-write gate (§3.5): uses the OWN `planned` leg only** (this node's direct children frozen), then
  resolves direct child contents — it does NOT need the full recursion. Confirmed with user 2026-07-07.
- **Snapshot / domain wave completion: uses the recursive `subtreePlanned`** — a node learns "my whole
  subtree is planned" by checking only direct children's latch (no subtree walk).

**RESOLVED (2026-07-07): SNAPSHOT-NATIVE. Writer UPDATED (2026-07-08): main-owned.**
- **Placement / source-of-truth:** the latch lives on the `xxxSnapshot` as
  `captureState.commonController.subtreePlanned` (a core-written field, sibling of
  `subtreeManifestsPersisted`). Domains only **read** it — content-free is preserved (writing your own
  `Snapshot.status` is allowed; only `SnapshotContent` is off-limits). Rationale: "planned" is a
  Snapshot-phase concept (`SnapshotContent` has no `phase`), and the latch must be readable **before** the
  content edges are frozen (§3.5); a content-native latch (recursing `childrenSnapshotContentRefs`) would
  lag the actual planning and be circular. The `subtreeManifestsPersisted` symmetry does **not** force
  content-native here — that is the manifest-durability axis (content), this is the planning axis (snapshot).
- **Who computes it — `main` (`SnapshotContentController`), UPDATED 2026-07-08.** Originally scoped to the
  binder; under the **main-owned `commonController`** decision (§2/§3) the entire `commonController`
  sub-structure — the capture-leg latches, the `subtreeManifestsPersisted` mirror, and `subtreePlanned` —
  is written by the aggregator **sideways onto the `xxxSnapshot`** (the same sideways-write path it already
  uses for `Snapshot.Ready`). Main watches every `xxxSnapshot` kind (`AddSnapshotStatusWatch`), so it wakes
  on child phase/latch changes; it computes `planned(n) ∧ ⋀(direct children) subtreePlanned(c)` by reading
  each direct child snapshot's `phase` + `subtreePlanned` latch, resolving the direct-child list from the
  owner's `childrenSnapshotRefs` (NOT the frozen `childrenSnapshotContentRefs` — that would be circular,
  since the freeze is gated on planning). `VolumeSnapshot` children carry their own `captureState.phase`
  (the foundation domain controller runs `MarkPlanned`) and participate in the recursion like every domain
  kind. This makes `subtreePlanned` genuinely **computed by a single writer** (main), not a binder→snapshot
  mirror; the binder writes nothing on `status`.

### 8.2 Relationship to `subtreeManifestsPersisted` (do NOT conflate)

Three ordered subtree properties along the lifecycle:

```
subtreePlanned  ≤  subtree edge-linked  ≤  subtreeManifestsPersisted
(structure)        (childrenSnapshot-       (every node's MCP Ready)
                    ContentRefs written)
```

- `subtreePlanned` **cannot replace** `subtreeManifestsPersisted`. The manifest-exclude is
  **persisted-based**: it subtracts the object identities descendants ACTUALLY captured, which needs Ready
  MCPs — "planned" only means structure is known.
- The `subtreeManifestsPersisted` **latch** is largely a fail-closed guard against a partial
  `childrenSnapshotContentRefs` graph. The complete immutable frozen set (§3.4/§3.5) removes that partial
  view, so the latch's guard role is weakened: the exclude can walk the complete graph and require each MCP
  Ready inline, and the first-Ready gate is subsumed by recursive `ChildrenReady`. Potential **follow-up**:
  drop the `subtreeManifestsPersisted` field (the persisted requirement stays, it just moves into the walk).
  NOT decided — direction question was skipped 2026-07-07.

### 8.3 Manifest-exclude must go through the API-service endpoint

The exclude source is the `snapshotcontents/<name>/subtree-manifest-identities` service subresource:

- **client (SDK):** `pkg/snapshotsdk/subtree_identities.go` `SubtreeManifestIdentities` →
  `subresourceREST.Get().AbsPath("/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/snapshotcontents/<name>/subtree-manifest-identities")`; fail-closed 409 → `ErrSubtreeIdentitiesPending`.
- **server:** `internal/api/restore_handler.go` `HandleContentSubtreeManifestIdentities` →
  `internal/usecase/subtree_manifest_identities.go` `BuildSubtreeManifestIdentities`. It walks
  `childrenSnapshotContentRefs` and reads each MCP archive **server-side**, returning only identities
  (`apiVersion/kind/namespace/name/uid`). Domains have no RBAC on `SnapshotContent`/`ManifestCheckpoint`;
  the endpoint (RBAC-granted in `hooks/go/030-domain-rbac`) is how they read subtree coverage.
- Because the endpoint walks `childrenSnapshotContentRefs`, it is another **consumer of the complete frozen
  set** (§3.4/§3.5) — reinforcing the single-writer/frozen-set need.

**Current state (interim, verified 2026-07-07):** the ROOT namespace capture does NOT use the endpoint. It
reads archives **in-reconciler** via `usecase.BuildRootNamespaceManifestCaptureTargets`
(`root_capture_run_exclude.go` → `arch.GetArchiveFromCheckpoint`); `WithSubresourceREST` is intentionally
not wired for the root's `captureSDK()` (`namespace_capture_run.go:46-47`). Per wave5 (§8) the root should
migrate to `exclude = sdk.SubtreeManifestIdentities(...)` and delete the in-reconciler builder.

**Follow-up:** migrate the root manifest-exclude to the API-service endpoint. Independent of the
single-writer slices, but consistent with them (both rely on a complete, correctly-linked
`childrenSnapshotContentRefs` graph) — do it after the frozen-set (§3.4/§3.5) so the endpoint's subtree walk
is over the complete graph.

### 8.4 Impact on orphan / residual PVC coverage detection

How "is this PVC already in a domain snapshot?" is answered today, and what the single-writer + frozen-set +
`subtreePlanned` work changes.

**Today (verified 2026-07-07).** Coverage is a **PVC-UID set**, not a manifest check:

- orphan wave gate: `allDeclaredDomainChildSnapshotsReady` (`parent_graph.go`) — run only after every declared
  DOMAIN child is `Ready=True`, so a PVC a domain child covers is never momentarily seen as orphan.
- covered set: `CollectSubtreeCoveredPVCUIDsFromSnapshot`
  (`internal/usecase/volumecapture/subtree_covered_pvc_from_snapshot.go`) walks `status.childrenSnapshotRefs`
  (skipping CSI VS visibility leaves), resolves each domain child's `status.boundSnapshotContentName`, and
  reads the covered UID from that content: `status.data.source.uid` (milestone B), with a fallback to the
  in-flight VCR `spec.targets[].uid` for the A→B window (`coveredPVCUIDsForContent`, `subtree_covered_pvc.go`).
- residual = `List(PVCs in ns, resourceSelector)` − covered UIDs (`residualPVCTargets`,
  `namespace_pvc_candidates.go`).
- fail-closed on duplicate UID across descendants (`ErrDuplicateCoveredPVCUID`).

The coverage signal is **UID-based data-binding**; the `subtree-manifest-identities` endpoint (§8.3) is NOT
involved in orphan volume detection — it is the parallel manifest-dedup leg (key `apiVersion|kind|namespace|name`).

**What changes (deltas):**

- **Walk source:** coverage can read the immutable `childrenSnapshotContentRefs` of the root content once the
  frozen set exists (§3.4/§3.5) instead of re-resolving `boundSnapshotContentName` per child off
  `childrenSnapshotRefs`. Content-free `…FromSnapshot` remains needed until the root content is bound
  (Late-Planned); after binding + frozen-set, a single consistent graph is available. Legacy content-tree
  walk (`CollectSubtreeCoveredPVCUIDs`) and the Snapshot-graph walk converge.
- **Wave gate:** `allDeclaredDomainChildSnapshotsReady` (direct-child walk + `observedGeneration`) can become
  a read of the recursive `subtreePlanned` latch (§8.1) — but note coverage still needs **B** (data), and
  `subtreePlanned` is only structure (A-level), so the gate change and the coverage read stay separate.
- **A→B fallback stays, but its source changes (§8.5).** Per §3.5 correction: the write barrier is milestone A
  (content exists/bound), while coverage reads milestone B (`status.data.source.uid`). Edge existence does not
  imply B, so a fallback remains the only signal in the A→B window. Removing it requires strengthening the
  barrier to B (edge only after `status.data` published) — a separate, stronger gate. The fallback should read
  the VCR name from the owning `xxxSnapshot` (`captureState.domainSpecificController.volumeCaptureRequestName`),
  not derive it from the content UID — see §8.5; for native-CSI kinds without a VCR (`VolumeSnapshot`, §11.7)
  it reads the owner's `snapshotSource` PVC UID instead.
- **Fewer races / duplicates:** a single writer + immutable edge set removes partial/reordered graph views
  that the fail-closed `DuplicateCoveredPVCUID` currently defends against.

**Unchanged:** coverage stays **PVC-UID based** (`status.data.source.uid`); the manifest leg stays separate.

### 8.5 Orphan PVC list construction — walk content, VCR fallback via `xxxSnapshot` (proposal 2026-07-07)

Target algorithm for building the residual/orphan PVC list, once the frozen content graph exists (§3.4/§3.5):

```
orphan set = List(PVCs in ns, resourceSelector) − coveredUIDs(rootContent)
```

`coveredUIDs` **walks the content tree** — the root content's frozen `childrenSnapshotContentRefs` subtree
(~~skipping orphan-output nodes: `IsChildVolumeNodeContent`~~ — *superseded by §11: no orphan-output nodes
exist; `VolumeSnapshot` contents are regular data-bearing nodes*). Per content node it first determines,
**authoritatively from the CSD** (not from the shape of the tree), whether the node **carries a data leg**:

- **data-bearing?** `reg.RequiresDataArtifact(kind)` where `kind = content.spec.snapshotRef.kind` — backed by
  CSD `spec.requiresDataArtifact` (`customsnapshotdefinition_types.go:65-69`) and exposed via the **existing**
  accessor `GVKRegistry.RequiresDataArtifact` (`pkg/snapshot/gvk_registry.go:177-179`; already used by the
  binder at `controller.go:425`, `import.go:162`, `domain_content.go:283`). Unmarked kinds read `false`.

Then, per node:

1. **not data-bearing** (`RequiresDataArtifact == false`, manifest-only) → contributes **no** covered PVC UID.
2. **data-bearing, `status.data` present** → covered UID = `status.data.source.uid` (milestone B).
3. **data-bearing, no `status.data` yet (A→B window)** → **fallback via the owning snapshot**:
   - resolve `content.spec.snapshotRef` (`snapshotcontent_types.go:70-81`, required back-ref) → GET the owning
     `xxxSnapshot`;
   - read `status.captureState.domainSpecificController.volumeCaptureRequestName`
     (`domainCaptureStateString`, `domain_content.go:406-410`);
   - GET that `VolumeCaptureRequest`; `vcctrl.ParseVolumeCaptureTargets` → covered PVC UIDs
     (`spec.targets[].uid`);
   - **native-CSI domains (no VCR, §11.7):** if `volumeCaptureRequestName` is empty and the owner is a
     native-CSI kind (e.g. `VolumeSnapshot`), the covered PVC UID is the owner's
     `status.snapshotSource.uid` (the source PVC, published at adoption).

**Always recurse into `childrenSnapshotContentRefs`**, independent of whether this node is data-bearing — a node
may have both data and children.

**No "children ⇒ no data" heuristic (correction 2026-07-07).** Today `coveredPVCUIDsForContent`
(`subtree_covered_pvc.go:144-170`) short-circuits with `if hasChildren { return nil }` before the VCR fallback —
i.e. it infers "a node with children has no data." That happens to hold for the current demo tree but is **not**
an invariant: a snapshot kind may carry **both** children and a data leg, now or in future. The data-bearing
decision MUST come from `RequiresDataArtifact(kind)` (CSD `spec.requiresDataArtifact`), and the child recursion
MUST be unconditional. Drop the `hasChildren` branch. (Minor: the accessor is keyed by snapshot **Kind** string;
if two registered kinds ever share a Kind across apiVersions, widen the registry key to the full GVK.)

**Why the fallback source changes (correctness, not just cosmetics).** Today `coveredPVCUIDsForContent`'s
fallback (`pvcUIDsFromPendingVCR`, `subtree_covered_pvc.go:189`) derives the VCR name from the **content UID**
(`SnapshotContentVCRName(content.UID)` = `names.VolumeCaptureRequestName(content.UID)`). That matches only the
**content-owned** (root/orphan) VCR naming. A **domain data-leaf's** VCR is **snapshot-owned** — named off the
**snapshot UID** (`SnapshotOwnedVCRName(snapshotUID)`) and its real name is published in
`status.captureState.…volumeCaptureRequestName`. So a content-UID-derived lookup does **not** find a domain
child's in-flight VCR. Reading `volumeCaptureRequestName` off the `xxxSnapshot` — exactly what the binder's
data-leg projection already does (`domain_content.go:90-104`) — is the correct, uniform source.

**Walk-source caveat.** The content-tree walk is authoritative only **after** the frozen set exists; during the
Late-Planned window (root content bound, edges not yet frozen) coverage still needs the snapshot-graph walk
(`…FromSnapshot`). They converge post-frozen-set (§8.4). Eager shells (§9) do not change this: an eager root
shell has empty `childrenSnapshotContentRefs` until the frozen write.

**Unchanged:** A/B ordering (coverage reads B, fallback covers the A→B window), UID-based coverage, and the
fail-closed duplicate-UID guard (`ErrDuplicateCoveredPVCUID`). Three things change: **what we walk** (content
tree, not snapshot graph), **where the fallback VCR name comes from** (`xxxSnapshot.captureState`, not content
UID), and **how data-bearing-ness is decided** (`RequiresDataArtifact(kind)` from CSD, not the `hasChildren`
heuristic).

**Correctness note.** For a given leaf, the VCR targets are the pre-image of the eventual `status.data.source` —
the two paths (data vs VCR-targets) must yield the same PVC UID for that node.

---

## 9. Content creation timing — eager shell (decision 2026-07-07)

**Decision: Option A — create the `SnapshotContent` object as an empty shell as early as the node's
`Snapshot`/`xxxSnapshot` exists, decoupled from `phase>=Planned`.** `childrenSnapshotContentRefs` (frozen set,
§3.4/§3.5) and `status.data` (post-VCR) are written later, by their respective writers.

### 9.1 Today: lazy creation at `phase>=Planned` (verified 2026-07-07)

- root + domain child content: created by `GenericSnapshotBinderController.Reconcile`
  (`internal/controllers/genericbinder/controller.go:257-398`), gated on `isDomainPlanningComplete`
  (`phase>=Planned`, `controller.go:259-261` / `domain_content.go:418-426`). Below Planned the binder returns
  without creating.
- domain child content additionally **waits for the parent content to be bound**
  (`ResolveParentSnapshotContentOwnerRef` → `pending` until parent `status.boundSnapshotContentName`,
  `lifecycle_ownerrefs.go:277`) — parent-first.
- orphan child-volume-node content: `EnsureVolumeChildContent` (`snapshotcontent/volume_child_content.go:51`),
  created **post root-bind** by the namespace domain; the pre-Planned wave is deliberately content-free.
- name is deterministic from UID (`names.ContentName(uid)`), so the content name is known long before creation.

### 9.2 Why the timing matters now — the creation cycle

The lazy-at-Planned + parent-first + wave7 orphan gate form a circular dependency:

```
root content (bound)  ← root phase=Planned  ← domain children Ready  ← children content (bound)  ← root content
       (C4: create at Planned)   (C5: wave7 gate)      (needs data B)        (C1: parent-first ownerRef)
```

- **C1** child content needs the parent content object bound (ownerRef).
- **C4** content is created only at the node's own `phase>=Planned`.
- **C5** root `MarkPlanned` is gated on `allDeclaredDomainChildSnapshotsReady`.

Breaking any one edge opens the cycle. Eager shell creation breaks **C4** (root/parent content no longer
waits for its own Planned), which is the cleanest lever.

### 9.3 Three separated events per node (do not conflate)

1. **Shell create** — empty content object (`spec.snapshotRef` + parent ownerRef). **Now: eager** (node
   exists), no plan/children/data required.
2. **`childrenSnapshotContentRefs`** — frozen set, single writer, written **late** at the §3.5 barrier
   (milestone A of every child).
3. **`status.data`** — written **late**, after the node's VCR completes (milestone B; for native-CSI kinds
   (§11.4) — after the owner's CSI binding delivers the VSC).

The `phase>=Planned` gate moves **off event 1 and onto event 2** (edge writing), not shell creation.

### 9.4 Concrete changes (grounding, not final code)

- **Binder:** drop `isDomainPlanningComplete` as the **create** gate; a shell needs only `spec.snapshotRef` +
  ownerRef, not the frozen child set. Keep `PatchUnstructuredBoundContentName` right after `Create`.
- **ownerRef:** `ResolveParentSnapshotContentOwnerRef` resolves immediately because the parent shell already
  exists — still top-down (parent shell before child shell) but with no Planned coupling and no long pending.
- **Aggregator:** `phase>=Planned` (own leg, §3.5 / §8.1) now gates only the frozen-set edge write.
- **Orphan child-volume-node: EAGER too (RESOLVED 2026-07-07, one uniform rule; mechanism updated by §11).**
  The shell for a `VolumeSnapshot` node is created+bound by the binder eagerly, exactly like root/domain
  shells — but via the **standard CSD-registered domain-kind watch** (`AddWatchForPair`), not an orphan-VS
  carve-out watch. `EnsureVolumeChildContent` (namespace domain) is removed (§11.6). The content's parent
  ownerRef resolves against the eager root shell (tree case); a standalone VS content has no parent.
- name unchanged (`names.ContentName(uid)`).

### 9.5 Interactions & invariants

- **Immutability = Option A only.** Children are unknown at create, so `childrenSnapshotContentRefs` must be
  set-once-from-empty (§3.4 Option A `oldSelf.size() == 0 || self == oldSelf`); create-time population
  (Option B) is incompatible with eager empty shells.
- **Existence ≠ readiness (new invariant).** A shell existing does NOT mean Planned/Ready/data-materialized.
  Every reader that today infers "content exists ⇒ node planned/ready" must switch to the phase/latch or to
  the frozen edge set. INV-CONTENT-CREATE-1 below.
- **ChildrenReady must gate on the frozen edge set (§3.6).** The most consequential such reader is
  `validateCommonContentChildren`: an eager parent shell has empty `childrenSnapshotContentRefs`, which today
  reads `ChildrenReady=True` ("no child content"). It must instead read `ChildrenLinkPending` while the node
  has declared children but no committed frozen set (INV-CONTENT-CHILDREN-4). Without this, eager shells flip
  subtrees Ready prematurely.
- **Coverage unchanged by this.** Milestone B still gates coverage; a shell (A) carries no `status.data`, so
  the A→B fallback stays (VCR targets, or `snapshotSource` for native-CSI kinds — §3.5 correction / §8.4 /
  §11.7).
- **Edges still never dangle.** Shells are created top-down, so by the time an edge is written (§3.5 barrier)
  the child shell provably exists — eager creation only makes A reachable sooner, it does not write edges early.

**INV-CONTENT-CREATE-1:** the presence of a `SnapshotContent` object implies nothing about plan/readiness/data;
only `phase` / `subtreePlanned` (structure), the frozen `childrenSnapshotContentRefs` (edges), and
`status.data` / Ready (data) carry those meanings.

### 9.6 Risks

- **Readers keying on "content exists" as a proxy for Planned/Ready** — must be audited and repointed before
  going eager (breaks INV-CONTENT-CREATE-1 otherwise). Grep create/existence checks on `SnapshotContent`.
- **Empty-shell teardown / GC** — a shell created before the node commits must be cleaned on abort; confirm
  finalizer/ObjectKeeper + owner cascade handle an edge-less, data-less shell.
- **Binder `spec.snapshotRef` handshake at create-time** — verify the handshake does not itself require
  `phase>=Planned` once the create gate is removed.
- **Migration ordering** — eager creation should land together with (or before) the single-writer frozen-set
  work so that removing the Planned create-gate does not transiently let an old writer publish partial edges.

---

## 10. Import under the creator/main model (unified, not special)

Import is **not** a special content path. The lifecycle is the same shape as capture — `d8` creates the
`xxxSnapshot`, then (if the node bears data) a `DataImport` — so the same creator/main split applies:

- **creator = binder.** The binder creates + binds the `SnapshotContent` for an import `xxxSnapshot`
  exactly as for capture (object + `spec` + ownerRef + bind + finalizer). It does **not** write status.
  The current root-import create path (`snapshot/import.go:73-100`, namespace domain) moves to the binder
  so the domain stays content-free.
- **main = aggregator.** The aggregator is the single writer of import `status`, projecting from the import
  artifacts instead of MCR/VCR: `manifestCheckpointName` from the **uploaded** reconstructed
  `ManifestCheckpoint` (`ReconstructedManifestCheckpointName(snapshotUID, "")`), `data` from the
  `DataImport` reverse-lookup → `VolumeSnapshotContent`. It branches on `spec.source.import` vs capture.
  Child edges (`childrenSnapshotContentRefs`) use the **same** universal projection from the owner's
  `childrenSnapshotRefs` (Slice 1) — import is not special there.
- **What stays in the import controller.** The CSI-specific `VolumeSnapshot` binding (legacy
  `status.boundVolumeSnapshotContentName`/`readyToUse` on the imported `VolumeSnapshot`,
  `volumesnapshotimport/controller.go`) stays — it writes the `VolumeSnapshot`, not the `SnapshotContent`.

### 10.1 Import-MCP durability — ObjectKeeper backstop

On the capture path the MCP is born owned by an execution `ObjectKeeper`
(`manifestcapture/checkpoint_controller.go`), so it is GC-safe from creation. On import the reconstructed
MCP is created **ownerless** (`internal/usecase/import_upload.go:115-116`) because a cluster-scoped MCP
cannot be owned by the namespaced snapshot, and the eager content shell may not exist yet at upload time.
This leaves a window where a crash/delete during import can orphan the MCP + chunks with no sweeper.

**Fix:** at upload/create, attach an `ObjectKeeper` that `FollowObjects` the `xxxSnapshot` (mirrors
capture's execution OK), so the reconstructed MCP is GC-safe from birth. Do **not** rely on
`ownerRef → content-shell` (upload can precede the shell → the Create would fail). Later, in
`ensureManifestCheckpointOwnedByContent` (aggregator), after the content ownerRef is patched onto the MCP,
**remove the now-redundant ObjectKeeper** (drop its ownerRef and delete the dedicated OK) so the MCP is GC'd
with its content like the capture MCP.

---

## 11. VolumeSnapshot domain — orphan PVCs become a first-class domain (decision 2026-07-07)

**Decision.** The forked CSI `VolumeSnapshot` (v1) becomes a **registered domain snapshot kind**, driven by
a dedicated domain controller in **storage-foundation** through `pkg/snapshotsdk` — the same pattern external
teams (e.g. virtualization) will use. This **replaces the entire orphan-PVC special path** in
state-snapshotter (visibility leaves, `LabelChildVolumeNode` child-volume-node contents, per-orphan MCR/MCP,
`bindOrphanVSToChildContent`) with the standard domain protocol: the namespace domain merely **creates** the
orphan `VolumeSnapshot` and declares it as a regular child; everything else is the uniform
creator/main pipeline.

Scope is **wider than orphans**: every **new** `VolumeSnapshot` in the cluster — including user-created ones —
is a domain object (a standalone VS is a one-node tree). Rationale: a manually created VS must be
downloadable via `d8` and re-importable/restorable, which requires its `SnapshotContent` to carry an MCP from
the start.

### 11.1 Domain object — forked `VolumeSnapshot` v1 (CRD + patch 003)

- **v1 only.** The stable `v1` schema (hand-maintained CRD in storage-foundation `crds/`) gains the protocol
  status fields: `status.captureState`, `status.childrenSnapshotRefs`, `status.snapshotSource`,
  `status.conditions` — alongside the existing fork fields (`status.boundSnapshotContentName` — the SS
  protocol bind to the `SnapshotContent`, `status.data`, `spec.source.import`). This satisfies the CSD CRD
  contract (`boundSnapshotContentName` + `conditions`, `internal/controllers/csd/controller.go:57-65`).
  Not to be confused with the **upstream CSI** field `status.boundVolumeSnapshotContentName` (VS → VSC
  binding, written by the fork's CSI machinery) — that one is untouched and is the data-leg source (§11.4).
- **v1beta1 is stripped** of all fork fields and stays as legacy; it is never treated as a domain version.
  Verified: the external-snapshotter client module ships only `client/apis/volumesnapshot/v1`, so the strip
  touches only the hand-maintained CRD yaml.
- **Patch 003** (`storage-foundation/images/snapshot-controller/patches/`) is extended:
  1. the v1 **client types** gain the new status fields for round-trip safety — the fork's typed
     `UpdateStatus` would otherwise erase fields unknown to its structs;
  2. **behavior:** on entry to the unready path (`syncUnreadySnapshot`, i.e. `!ready || !bound`) the fork
     stamps a **"taken into work" label** on the VS (e.g. `storage-foundation.deckhouse.io/processed`),
     check-then-set (idempotent across resyncs), placed **after** the existing `spec.source.import` skip so
     import VSs are never labeled. The existing skip behavior is unchanged.
- **Two readiness signals, documented separately:** `status.readyToUse` (CSI binding, written by the fork)
  vs `conditions[Ready]` (state-snapshotter protocol: manifest+data captured, content consistent, written by
  the core). Different meanings, no conflict.

### 11.2 Domain controller — storage-foundation, via the SDK

- New controller-runtime reconciler in **storage-foundation `images/controller`** (the manager that already
  runs the VCR controller): registration is a minimal `main.go` edit; all logic in new files. There is no
  existing VolumeSnapshot reconciler in that manager today (the only VS watchers are the fork's informer
  loop and the mutating webhook), so this is net-new and does not disturb the fork.
- SDK dependency: `github.com/deckhouse/state-snapshotter/{api,pkg/snapshotsdk}` via **pseudo-versions**,
  moving to tags once the module stabilizes.
- The reconciler follows the demo-disk recipe (`virtualdisksnapshot_controller.go` pattern): adapter over the
  forked VS status fields, `PublishSnapshotSource` (the source PVC), `EnsureManifestCapture(target=PVC)`,
  `MarkPlanned`, `CoreCaptureOutcome` loop → `ConfirmConsistent`/`Reject`. It does **not** call
  `EnsureVolumeCapture` (see §11.4 — the data artifact is native CSI).

### 11.3 Adoption — fork-label discriminator, veto, `managed` latch

**Old/new discriminator = the fork label (no watermark, no hooks, no external state).** The fork knows
unambiguously whether it takes a VS into work (`!ready || !bound`); the label it stamps at that moment is the
adoption trigger. Properties:

- VSs that were already ready+bound before the module never enter the unready path → never labeled → legacy,
  untouched forever.
- The label lives on the VS object itself → survives module disable/enable; a VS created while the module was
  off sits pending, gets labeled and adopted after re-enable. No missed windows.
- Edge (accepted): a **pre-module VS that never became ready** is taken into work by the new
  snapshot-controller, gets labeled and becomes managed — the semantics are "ready before the module — don't
  touch", not strictly creation-time. A VS that became ready **before the module version carrying the label
  patch** stays legacy (the feature did not exist yet).

**Adoption flow (domain reconciler):** sees the fork label → evaluates the **veto** → latches its own label
`managed: "true"/"false"` on the VS. All later filtering keys on `managed`; the decision never flips.

**Veto:** `ExcludeLabelKey` (`state-snapshotter.deckhouse.io/exclude`) is checked **on the source PVC only**
(consistent with all domains — veto applies to sources, cf. the demo VM controller vetoing disks), **once, at
adoption**. A vetoed standalone VS behaves exactly like a legacy VS: the CSI snapshot works, but no
MCR/content is created and it is not `d8`-exportable (making it exportable would be a one-step veto bypass).
In the namespace tree the PVC veto keeps today's behavior: no orphan VS is created and the PVC is recorded in
`excludedRefs`.

**Skipped by the domain reconciler:** import VSs (`spec.source.import != nil`; never labeled by the fork
anyway), unlabeled VSs (legacy), `managed: "false"`, and **pre-provisioned VSs without a PVC source**
(`spec.source.volumeSnapshotContentName` set: no PVC to capture a manifest of and nothing to restore —
skipped with an Event on the object).

### 11.4 Capture legs

- **Manifest leg — standard SDK path.** The domain calls `EnsureManifestCapture(target=PVC)`; the MCR name
  lands in `captureState.domainSpecificController.manifestCaptureRequestName` and the aggregator projects
  MCR → MCP → `manifestCheckpointName` exactly as in Slice 2 — the per-orphan MCR branch (§4 Slice 2 "orphan
  branch") is **superseded** and removed. If the PVC is already gone at capture time (user created the VS
  and deleted the PVC), the protocol fails terminally (Ready=False + reason/message); the CSI artifact is not
  touched.
- **Data leg — native CSI, no new fields.** The VS *is* the volume capture: the fork binds it to a VSC
  (`status.boundVolumeSnapshotContentName`, `readyToUse`). The domain does **not** create a VCR. Instead the
  **aggregator**, for an owner of kind `VolumeSnapshot`, reads `owner.status.boundVolumeSnapshotContentName`
  → VSC → projects `content.status.data`, and performs the durability handoff (VSC `deletionPolicy: Retain`
  + ownerRef → content; the `EnsureVolumeSnapshotContentsOwnedByContent` machinery moves under the
  aggregator per Slice 3).
- **`dataCaptured` latch — main (main-owned `commonController`, 2026-07-08).** For kind `VolumeSnapshot` the
  aggregator (main) marks `captureState.commonController.dataCaptured` **sideways on the VS** once it has
  published `content.status.data` and the VSC is owned by the content — **in the same pass** as the data-leg
  projection + durability handoff. This supersedes the earlier "binder marks it" scoping (§2/§3 main-owned):
  main already watches the VS (`AddSnapshotStatusWatch`) and the content, so no new watch machinery is
  needed and the former two-pass main→binder handoff collapses into main's single pass.
- **CSI errors are transient.** The data leg does **not** go terminal from `VS.status.error` alone — the node
  stays Capturing (CSI retries). Terminality rules for the data leg are explicit: deletion of the VS
  mid-capture (teardown path) or protocol-level rejection only. (Design checkpoint §11.8.)

### 11.5 Registration — CSD, domain-capture marking, RBAC

- **CSD shipped by storage-foundation** (the domain owner owns its CSD, as virtualization will):
  `apiVersion: snapshot.storage.k8s.io/v1`, `kind: VolumeSnapshot`, `requiresDataArtifact: true`,
  `source: PersistentVolumeClaim (v1)`.
- **CSD-registered kinds are domain-capture by definition.** Today `unifiedruntime.Syncer` calls
  `MarkDomainCaptureKind` only for kinds in the hardcoded `DomainCaptureSnapshotKinds` list — a CSD kind
  outside the lists gets the binder watch (`AddWatchForPair`) but **not** domain-capture semantics
  (eager capture-leg init etc.). Fix: mark every CSD-derived kind as domain-capture without hardcoded list
  membership. `VolumeSnapshot` is **NOT** added to `DedicatedSnapshotControllerKinds` — that list is
  SS-internal activation ordering for in-process dedicated controllers; the VS controller lives in
  foundation and activates itself. The built-in `Snapshot` stays a bootstrap kind.
- **RBAC:** foundation ships its own controller's rights (VS full, MCR create/get, PVC read, CSD/Snapshot
  read); the SS `030-domain-rbac` hook grants the core access to the VS GVR from the CSD as usual.

### 11.6 What is dismantled in state-snapshotter (supersedes)

The entire parallel orphan machinery is removed once the VS domain lands:

| Removed | Where |
|---|---|
| `IsVolumeSnapshotVisibilityLeaf` + every skip branch keyed on it | `pkg/snapshot/visibility_leaf.go`, `status_publish.go`, `namespace_capture_plan.go`, `ready_mirror.go`, aggregator |
| `LabelChildVolumeNode` + `EnsureVolumeChildContent` + `ChildVolumeContentName` naming | `snapshotcontent/volume_child_content.go`, aggregator special cases |
| `orphanChildContentNameFromVSLeaf` / `unlinkedOrphanChildContents` orphan-link gate | `snapshotcontent/controller.go` |
| per-orphan MCR/MCP creation + latch-idempotent orphan MCP projection (§4 Slice 2 orphan branch) | `orphan_pvc_volume_snapshot.go`, aggregator |
| `bindOrphanVSToChildContent` (domain-side bind of `VS.status.boundSnapshotContentName`) | `orphan_pvc_volume_snapshot.go` — the **binder** binds VS like any domain kind |
| VS-leaf partition maintenance (`reconcileOrphanPVCVolumeSnapshotChildLeaves`) | `orphan_pvc_volume_snapshot.go` — replaced by regular `EnsureChildren` refs |
| `"VolumeSnapshot"` transient-artifact special case | `snapshotcontent/data_readiness.go` |

The namespace domain **keeps** exactly two responsibilities for orphans: create the orphan `VolumeSnapshot`
(ownerRef to the namespace `Snapshot` for GC) for each residual PVC, and declare it as a **regular** child
ref via `EnsureChildren` (kind `VolumeSnapshot` entries are no longer "visibility leaves" — they resolve via
`ResolveChildSnapshotRefToBoundContentName` like every domain child). Content naming becomes the standard
`names.ContentName(vsUID)`; the core mirrors `conditions[Ready]` onto the VS like any domain CR (the
`ready_mirror.go` "VS leaf has no bind model" skip is removed).

### 11.7 Coverage & namespace-tree semantics

- **A standalone (user-created) VS does NOT cover its PVC** for namespace capture: it is a point-in-time from
  an arbitrary past moment. Coverage is computed over the **tree** (declared children) only; the namespace
  domain creates its own fresh orphan VS for such a PVC (two VSs on one PVC are legal in CSI).
- **§8.5 simplification:** `VolumeSnapshot` nodes are data-bearing via CSD `requiresDataArtifact: true` — the
  standard `RequiresDataArtifact(kind)` decision applies; the orphan-output node skip
  (`IsChildVolumeNodeContent`) disappears with the machinery. **Fallback nuance:** a VS node has no VCR, so in
  the A→B window the covered PVC UID comes from the owner VS itself — `status.snapshotSource.uid` (the source
  PVC, published at adoption) — instead of VCR `spec.targets[].uid`. The §8.5 fallback rule becomes:
  data-bearing + no `status.data` → read `captureState.domainSpecificController.volumeCaptureRequestName` if
  present (VCR-based domains), else the owner's `snapshotSource` PVC UID (native-CSI domains like
  `VolumeSnapshot`).

### 11.8 Lifecycle, GC, and design checkpoints

- **Tree nodes:** orphan VS deleted via ownerRef cascade when the tree is deleted; its content via the
  existing content-tree GC; the VSC stays owned by the content (Retain + ownerRef) — as today.
- **Standalone VS deletion — semantics change (must be documented in ADR):** with Retain + content-ownerRef
  handoff, deleting a user VS no longer frees storage directly (upstream `Delete` class policy would have);
  the VSC is deleted when the content is GC'd after the owner VS disappears. Checkpoint: confirm the
  unlinked-content GC covers a standalone content whose owner VS is gone (a content→VS ownerRef is
  impossible: cluster-scoped → namespaced), and document the deletion-to-storage-free window.
- **Checkpoint — build infra:** `state-snapshotter/{api,pkg/snapshotsdk}` modules must resolve from the
  foundation werf build (GOPROXY/SOURCE_REPO; private repo → GOPRIVATE).
- **Checkpoint — helm ordering:** the CSD (a CR of the SS module's CRD) shipped in foundation templates —
  confirm CRD-before-CR at converge (`module.yaml` already requires `state-snapshotter >= 0.0.0`).
- **Checkpoint — `VS.status.error` policy:** explicit terminality rules per §11.4 (no auto-terminal from a
  transient CSI error).

### 11.9 Impact on migration slices

- **Slice 3 (Block 3) is re-cut.** The "orphan child-content creation → binder (watch orphan VS leaves)" and
  "namespace domain binds `VolumeSnapshot.status.boundSnapshotContentName`" sub-items are **superseded**: VS
  is a registered domain kind, the binder watches it through the standard CSD `AddWatchForPair` and binds it
  like any kind. What remains of Slice 3: data-leg projection → aggregator (with the kind-`VolumeSnapshot`
  native-CSI branch of §11.4 replacing the old "orphan child reads `spec.snapshotRef` → bound VSC" wording —
  same data source, now via the owner), restore `spec.snapshotRef` repoint → binder, namespace domain
  content-free STRICT, plus the §11.6 dismantling.
- **Slice 2's orphan branch** (aggregator deriving per-orphan MCP from the VS UID) is superseded: the VS
  domain publishes `manifestCaptureRequestName` like every domain; the standard projection applies.
- **§8.5 / Block 5** simplifies per §11.7 (no orphan-output skip; fallback rule extended for native-CSI
  domains).
- **New implementation blocks (storage-foundation side):** CRD v1 extension + v1beta1 strip + patch 003
  (types + label), VS domain reconciler + adapter + adoption/veto logic, CSD + RBAC templates, go.mod wiring.
  Sequenced in the implementation plan.
