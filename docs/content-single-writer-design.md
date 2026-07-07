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
writes ONLY own Snapshot.status:          creates + binds SnapshotContent (EAGER):    SINGLE writer of ALL content status:
  - childrenSnapshotRefs                     - spec.snapshotRef + deletionPolicy        - conditions / Ready
  - phase (+ subtreePlanned latch)           - parent/child ownerRef                    - childrenSnapshotContentRefs (single, frozen)
  - data (mirror)                            - bind (boundSnapshotContentName)          - manifestCheckpointName (MCR→MCP)
                                             - finalizer                                - data (VCR→data)
                                           watches all xxxSnapshot + orphan VS leaves   - subtreeManifestsPersisted / excludedRefs
never touches SnapshotContent             writes NO content.status                     projects owner childrenSnapshotRefs → child edges
```

**Creator/main split (decision 2026-07-07).** The binder is the *creator*: it creates the content
object, writes `spec`, sets the parent/child ownerRef, binds (`boundSnapshotContentName`), and manages the
finalizer — and nothing on `status`. The `SnapshotContentController` aggregator is the *main*: it is the
**sole writer of every `SnapshotContent.status` field**, including the `manifestCheckpointName` (MCR→MCP)
and `data` (VCR→data) projections that the binder / namespace domain write today. Domains stay content-free
(rule #1). This supersedes the earlier "binder writes own-node legs" split below (§2.1 documents today's
code; §3.1–§3.3 and §4 describe the target).

### 3.1 Child-edge single writer (aggregator)

In `reconcileCommonSnapshotContentStatus`, add one step that **computes and writes** the desired child
edge set, reusing the resolvers that today only read:

- desired set = `declaredNonLeafChildContentNames` (domain, resolved+bound) ∪ existing-orphan
  child-volume-node contents (from `unlinkedOrphanChildContents`' resolution, but keeping those that
  exist rather than reporting the unlinked remainder).
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

- delete `LinkChildVolumeContentRef` and its call (`orphan_pvc_volume_snapshot.go:571`).
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

### 3.5 Write barrier — commit only when every child has content

The precondition for writing `childrenSnapshotContentRefs` is **stronger than "children declared + owner
`phase>=Planned`"**. An edge points to a child's **SnapshotContent** (resolved today via the child's
`status.boundSnapshotContentName`, `ResolveChildSnapshotRefToBoundContentName`,
`internal/usecase/child_snapshot_resolve.go:72`), which exists only after the binder created+bound it. So
the write barrier is both:

1. `childrenSnapshotRefs` is complete/frozen — the Late-Planned enumeration (all children, **including the
   orphan volume leaves**, present); AND
2. every declared child has **materialized content**:
   - domain child — `boundSnapshotContentName` resolvable (`declaredNonLeafChildContentNames` → complete);
   - orphan leaf — its child-volume-node content exists (`unlinkedOrphanChildContents` → empty).

This is exactly the pair the aggregator already computes for its fail-closed Ready gate; the single writer
reuses it as the **write** gate. Condition 1 (this node's plan frozen) is the OWN `planned` leg that may be
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
  the child's `VolumeCaptureRequest` completes and `PublishSnapshotContentDataRef(s)` runs.

A happens **before** B. Therefore **an existing edge (barrier A) does NOT imply a materialized
`status.data.source.uid` (B)**. This matters for orphan/residual PVC coverage, which reads B
(`pvcUIDsFromSnapshotContentDataRefs` → `status.data.source.uid`,
`internal/usecase/volumecapture/subtree_covered_pvc.go:172`) with an in-flight-VCR fallback
(`pvcUIDsFromPendingVCR`, `spec.targets[].uid`, same file :189) precisely for the A→B window.

Consequence: **the VCR fallback in `coveredPVCUIDsForContent` must be kept** — moving edge-writing to the
single writer does not close the A→B window, so the fallback is still the only signal during it. The
fallback could be removed **only** if the barrier is strengthened from A to B (write the edge only once the
child's `status.data` is published), which is a **stronger, separate gate**: it delays `childrenSnapshotContentRefs`
linking until each child's volume capture finishes and couples edge-linking to VCR completion. Not adopted
here; noted as an option.

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
**declared** child set (the node's frozen `childrenSnapshotRefs` / owner-derived orphan set) against the
**linked** edges:

- declared children exist AND edges not yet the complete set → `ChildrenReady=False` (`ChildrenLinkPending`);
- declared set empty (true leaf) → `ChildrenReady=True` ("no child content") is correct.

This **generalizes today's orphan-only gate** `unlinkedOrphanChildContents` (`controller.go:995-1004`, which
already recomputes the declared orphan set and holds `ChildrenLinkPending`) to **all** declared children
(domain + orphan). Under the atomic frozen-set write (§3.1 end-state) there is no partial-link state: edges are
either empty (pending) or the complete set (evaluate readiness) — so the gate reduces to "declared non-empty
AND edges empty → pending".

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
is exactly what the aggregator already resolves for its fail-closed gate.

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

**Orphan branch — no owning `xxxSnapshot`.** An orphan child-volume-node content has no `xxxSnapshot`
owner (its `spec.snapshotRef` points at the orphan CSI `VolumeSnapshot`), so there is no `captureState`.
For it the aggregator derives the manifest leg from `spec.snapshotRef.uid` (the VS UID):
`SnapshotVolumeMCRName(vsUID)` → MCR UID → MCP name. It must be **latch-idempotent**: the domain deletes
the per-orphan MCR once its MCP is Ready (`orphan_pvc_volume_snapshot.go:594-607`), so once
`manifestCheckpointName` is published the aggregator keeps it even when the MCR is gone.

### Slice 3 — data leg → aggregator (main) + orphan content creation → binder (content-free domain)

The largest slice; finishes rule #1 for the namespace domain (strict INV-CONTENT-WRITER-1).

- **data leg → aggregator.** Move `EnrichDataBindingsWithVolumeMetadata` +
  `EnsureVolumeSnapshotContentsOwnedByContent` + `PublishSnapshotContentDataRef(s)` off the binder
  (`domain_content.go:188-199`) and the namespace domain (`orphan_pvc_volume_snapshot.go:534-564`,
  `volume_capture.go:179-189`) into the `SnapshotContentController` aggregator. For a domain data-leaf the
  aggregator reads `captureState.volumeCaptureRequestName` → VCR → VSC → `status.data`; for an orphan
  child it reads `spec.snapshotRef` (the orphan VS) → its bound VSC → binding. Nobody watches VCR today
  (verified — the binder watches only snapshots/contents/MCR), so add a VCR watch to the aggregator or
  lean on its 500 ms self-requeue while `!ready` (§3.2).
- **orphan child-volume-node content creation → binder (RESOLVED 2026-07-07: binder, eager).** The binder
  is the single creator for **all** nodes. Extend the binder watch to the orphan **CSI `VolumeSnapshot`**
  leaves and create+bind the orphan shell **eagerly** as soon as the leaf exists (same uniform rule as the
  root/domain eager shell, §9): the orphan content's parent ownerRef resolves against the eager root shell.
  Remove `EnsureVolumeChildContent` creation from `orphan_pvc_volume_snapshot.go`. This supersedes the
  earlier "(a) aggregator creates orphan content" option — keeping *all* content creation in one creator
  (the binder) is the uniform creator/main model, not an orphan-only carve-out. The namespace domain keeps
  only: create the orphan CSI `VolumeSnapshot`, publish the VS visibility leaf into `childrenSnapshotRefs`,
  and bind `VolumeSnapshot.status.boundSnapshotContentName` → deterministic child name
  (`ChildVolumeContentName(vsUID)`, needs no content read).
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
  `unlinkedOrphanChildContents` gate to all declared children.
- **Monotonic edges (interim, until slice 4):** an edge, once published, is removed only by teardown —
  never dropped on a partial per-reconcile view. Superseded by INV-CONTENT-CHILDREN-2 once the frozen set
  is written atomically.
- **Ready model — mostly unchanged, one addition:** the fail-closed reads
  (`validateCommonContentChildren`, `aggregateChildrenSubtreeManifestsPersisted`, `unlinkedOrphanChildContents`)
  keep their meaning and now read edges written by the same controller in the same pass. The one change is the
  §3.6 read barrier (INV-CONTENT-CHILDREN-4): `validateCommonContentChildren` must treat empty edges with a
  non-empty declared child set as pending, not `ChildrenReady=True` — required by eager shells (§9).

---

## 6. Testing

- **Unit:** aggregator child-projection — domain-only children, orphan-only leaves, mixed; partial
  (unresolved/unbound child) → edge withheld, existing edges preserved; teardown removal.
- **Unit (§3.6 read barrier):** eager parent shell with declared children + empty `childrenSnapshotContentRefs`
  → `ChildrenReady=False` (`ChildrenLinkPending`); true leaf (no declared children) + empty edges →
  `ChildrenReady=True`; committed frozen set → readiness evaluated over the complete linked set
  (INV-CONTENT-CHILDREN-4).
- **Integration (envtest):** `n5_pr7` orphan-wave (isolated CSI-simulator pass) + non-isolated
  regression; `snapshot_root_lifecycle` / `snapshot_recreate`.
- **e2e:** `capture` (root + demo tree `Ready`), `volumedata` (orphan child volume node + domain-VCR
  coverage).
- **envtest CEL (slice 4):** apiserver accepts the first complete set on an empty field, then rejects
  every subsequent change — shrink, append, reorder, and full replace — proving INV-CONTENT-CHILDREN-2.
- **Guard:** grep/lint asserting INV-CONTENT-WRITER-1 and INV-CONTENT-CHILDREN-1 (single-writer).

---

## 7. Risks & mitigations

- **Churn from replace-set writes** → keep union/monotonic semantics (§3.1), not blind replace.
- **Latency of linking** relative to the old inline publish → covered by the existing 500 ms self-requeue;
  add owner/child wake-up watches only if a measured regression appears.
- **Slice 3 creation ownership** is the one real design decision (§4, open question) — resolve before
  touching the data leg; slices 1–2 are mechanical moves and can land first.
- **Immutability vs incremental population (slice 4)** — enabling the immutable CEL rule before the writer
  emits the complete frozen set in one write would wedge every capture (2nd edge write rejected by the
  apiserver). Order is mandatory: atomic frozen-set write first (§3.1 end-state), then flip the rule (§3.4).
  Mitigation: land slice 4 only after an envtest proves the single-pass complete-set write.
- **Hidden field rewriter** — a teardown/degradation/restore path that patches `childrenSnapshotContentRefs`
  after set would be rejected once immutable. Audit (grep for status writes to the field) before slice 4;
  the single-writer work (slices 1–3) should already have removed all but the aggregator.
- **Frozen-set correctness depends on Late-Planned enumeration** — the immutable single write assumes
  `childrenSnapshotRefs` is genuinely complete at the write barrier (§3.5 condition 1: all domain + orphan
  children enumerated by Planned). If an orphan leaf is appended to `childrenSnapshotRefs` *after* the frozen
  set is committed, immutability locks it out. Mitigation: confirm the orphan-wave enumeration completes by
  the write barrier before enabling immutability (slice 4); keep the interim append semantics until then.

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
Planned with its direct children (domain + orphan leaves) fully enumerated in `childrenSnapshotRefs`
(Late-Planned). Monotonic (spec is immutable; no recapture).

Two consumers, at DIFFERENT levels:

- **content-write gate (§3.5): uses the OWN `planned` leg only** (this node's direct children frozen), then
  resolves direct child contents — it does NOT need the full recursion. Confirmed with user 2026-07-07.
- **Snapshot / domain wave completion: uses the recursive `subtreePlanned`** — a node learns "my whole
  subtree is planned" by checking only direct children's latch (no subtree walk).

**RESOLVED (2026-07-07): SNAPSHOT-NATIVE.**
- **Placement / source-of-truth:** the latch lives on the `xxxSnapshot` as
  `captureState.commonController.subtreePlanned` (a core-written field, sibling of
  `subtreeManifestsPersisted`). Domains only **read** it — content-free is preserved (writing your own
  `Snapshot.status` is allowed; only `SnapshotContent` is off-limits). Rationale: "planned" is a
  Snapshot-phase concept (`SnapshotContent` has no `phase`), and the latch must be readable **before** the
  content edges are frozen (§3.5); a content-native latch (recursing `childrenSnapshotContentRefs`) would
  lag the actual planning and be circular. The `subtreeManifestsPersisted` symmetry does **not** force
  content-native here — that is the manifest-durability axis (content), this is the planning axis (snapshot).
- **Who computes it:** the **binder** (the only core controller subscribed to every `xxxSnapshot` kind),
  which already writes the sibling `captureState.commonController.subtreeManifestsPersisted` mirror
  (`mirrorSubtreeManifestsPersistedFromContent`, `genericbinder/controller.go:771`). It computes
  `planned(n) ∧ ⋀(direct children) subtreePlanned(c)` by reading each direct child snapshot's
  phase + latch; CSI VS visibility leaves count as enumerated (no phase of their own). Child→parent
  wake-up reuses the existing snapshot-status watch.

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
- **VCR fallback stays, but its source changes (§8.5).** Per §3.5 correction: the write barrier is milestone A
  (content exists/bound), while coverage reads milestone B (`status.data.source.uid`). Edge existence does not
  imply B, so a VCR fallback remains the only signal in the A→B window. Removing it requires strengthening the
  barrier to B (edge only after `status.data` published) — a separate, stronger gate. The fallback should read
  the VCR name from the owning `xxxSnapshot` (`captureState.volumeCaptureRequestName`), not derive it from the
  content UID — see §8.5.
- **Fewer races / duplicates:** a single writer + immutable edge set removes partial/reordered graph views
  that the fail-closed `DuplicateCoveredPVCUID` currently defends against.

**Unchanged:** coverage stays **PVC-UID based** (`status.data.source.uid`); the manifest leg stays separate.

### 8.5 Orphan PVC list construction — walk content, VCR fallback via `xxxSnapshot` (proposal 2026-07-07)

Target algorithm for building the residual/orphan PVC list, once the frozen content graph exists (§3.4/§3.5):

```
orphan set = List(PVCs in ns, resourceSelector) − coveredUIDs(rootContent)
```

`coveredUIDs` **walks the content tree** — the root content's frozen `childrenSnapshotContentRefs` subtree
(skipping orphan-output nodes: `IsChildVolumeNodeContent`). Per content node it first determines, **authoritatively
from the CSD** (not from the shape of the tree), whether the node **carries a data leg**:

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
     (`spec.targets[].uid`).

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
3. **`status.data`** — written **late**, after the node's VCR completes (milestone B).

The `phase>=Planned` gate moves **off event 1 and onto event 2** (edge writing), not shell creation.

### 9.4 Concrete changes (grounding, not final code)

- **Binder:** drop `isDomainPlanningComplete` as the **create** gate; a shell needs only `spec.snapshotRef` +
  ownerRef, not the frozen child set. Keep `PatchUnstructuredBoundContentName` right after `Create`.
- **ownerRef:** `ResolveParentSnapshotContentOwnerRef` resolves immediately because the parent shell already
  exists — still top-down (parent shell before child shell) but with no Planned coupling and no long pending.
- **Aggregator:** `phase>=Planned` (own leg, §3.5 / §8.1) now gates only the frozen-set edge write.
- **Orphan child-volume-node: EAGER too (RESOLVED 2026-07-07, one uniform rule).** The binder creates+binds
  the orphan shell as soon as the orphan CSI `VolumeSnapshot` leaf exists — decoupled from Planned/bind,
  exactly like root/domain shells. No post-bind carve-out. Creation moves from `EnsureVolumeChildContent`
  (namespace domain) to the binder's orphan-VS watch (§4 Slice 3). The orphan content's parent ownerRef
  resolves against the eager root shell, so eager creation is safe.
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
  the VCR fallback stays (§3.5 correction / §8.4).
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
