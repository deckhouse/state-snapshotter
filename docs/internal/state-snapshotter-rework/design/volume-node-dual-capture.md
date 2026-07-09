# Logical SnapshotContent: manifest + data (Variant A: singular `dataRef`)

**Status:** Accepted target design (**Variant A**, cardinality ≤1). Normative: [`spec/system-spec.md`](../spec/system-spec.md) **§3.9**. **PR backlog PR-0…PR-9:** [`implementation-plan.md`](implementation-plan.md) **§2.4.5**.

**Supersedes:** the earlier `dataRefs[] 0..N` draft (a single `SnapshotContent` carrying a *list* of data bindings). **Target model (Variant A, decided by the team):** one **`SnapshotContent` = one logical snapshot node** with bulk manifests (MCR `Targets[]`) and **at most one** durable data binding — **singular `status.dataRef`** (an object, not a list). A multiplet of volumes is modeled **structurally** as **child volume nodes** (each its own `SnapshotContent` with its own MCP + single `dataRef`), never as a list on one node. `status.dataRef` is a singular object (CRD `type: object`, not an array), so ≥2-on-one-content is **structurally unrepresentable** at the API/type level — no CEL rule and no runtime fail-closed needed.

**Why singular (Variant A):** a PVC that belongs to **no owner** (loose/orphan) is itself a **standalone snapshot node** in the tree (its own content + manifest + dataRef + namespaced handle), and a real domain leaf owns **exactly one** PVC. So every logical content carries **≤1** dataRef by construction. This removes the heaviest part of the multi-PVC refactor (generic decomposition / runtime fail-closed) while keeping per-CR restore (Variant B) intact.

**Scope:** state-snapshotter planning and execution. **Out of scope:** VCR/VSC CSI wiring, Ceph/Rook sidecar compatibility, shadow `VolumeSnapshot` — **storage-foundation** (plain reference when available in the monorepo workspace: `storage-foundation/docs/63742164-sidecar-migration.md`).

**Related:** [`demo-domain-csd/05-tree-and-graph-invariants.md`](demo-domain-csd/05-tree-and-graph-invariants.md); dedup — [`demo-domain-csd/06-coverage-dedup-keys.md`](demo-domain-csd/06-coverage-dedup-keys.md) §4; lifecycle — [`demo-domain-csd/090-unified-snapshot-controller-lifecycle.md`](demo-domain-csd/090-unified-snapshot-controller-lifecycle.md).

---

## 1. Core formula

```text
SnapshotContent (logical snapshot node) =
  one manifestCheckpointName → MCP (0..N manifest objects via MCR.spec.targets[])
+ at most one dataRef          → PVC target identity ↔ VSC artifact (singular object)
+ zero or more childrenSnapshotContentRefs[] → real domain decomposition AND child volume nodes
```

**Not:** `SnapshotContent = one volume` and **not** `SnapshotContent = a list of data artifacts`. It is **one logical node with ≤1 data artifact**; extra volumes are **child volume nodes**.

**Co-ownership invariant (manifest + data):** the **sanitized PVC manifest** and the matching **`dataRef`** (same `pvcUID` / target identity) **MUST** live on the **same** `SnapshotContent`. Because a node carries ≤1 dataRef, this means: each PVC's manifest lives on the **node that owns that PVC's dataRef** — for a loose/orphan PVC that is its **own child volume node** (its own MCP holds just that PVC manifest), **not** the root. Another scope **MUST NOT** duplicate that PVC manifest or data binding.

**Requests (target model):**

```text
ManifestCaptureRequest                    VolumeCaptureRequest (domain leg) / CSI VolumeSnapshot (orphan leg)
├── spec.targets[]  (manifest objects)    ├── spec.targets[] = exactly one PVC (domain leaf)
└── status.checkpointName → MCP           └── status.dataRefs[] → VSC binding (foundation still returns a list)

SnapshotContent (published durable refs)
├── status.manifestCheckpointName  ← from MCR.status.checkpointName
└── status.dataRef                 ← the single binding (domain VCR dataRefs[0], or orphan VS→VSC) after handoff
```

| Rule | Detail |
|------|--------|
| **One MCP per logical content** | A node's manifests live in exactly one MCP. A child volume node has its **own** MCP holding only that PVC's manifest (per-child MCP scoping). |
| **≤1 dataRef per content** | A node owns at most one PVC's data; multi-PVC scope fans out into child volume nodes. |
| **Domain VCR guard** | The foundation VCR still returns `status.dataRefs[]`; a domain content owns exactly one PVC, so >1 binding for one content is a **terminal capture failure** (never silently truncated). |
| **Orphan leg** | Root residual/orphan PVCs do **not** use a root VCR; each becomes a child volume node backed by a CSI `VolumeSnapshot` handle + its own per-orphan MCR/MCP. |
| **Temporary tracking** | `Snapshot.status.manifestCaptureRequestName` / `volumeCaptureRequestName`; child volume node tracks its own `manifestCaptureRequestName`. |

---

## 2. Node roles

| Role | MCP | `dataRef` | `children` | Notes |
|------|-----|-----------|------------|--------|
| **Namespace root** (`Snapshot` / root content) | Residual namespace manifests only (after E5 exclude); **no** residual PVC manifests (those move to child volume nodes) | **nil** (aggregator) | Real domain decomposition **+ child volume nodes** for each orphan/residual PVC | Aggregator-first; `Ready` from manifests + children |
| **Child volume node** (orphan/loose PVC) | **MUST** hold exactly that PVC's sanitized manifest (own per-orphan MCR/MCP) | **one** binding: pvcUID → VSC | none | Namespaced handle = CSI `VolumeSnapshot` whose `status.boundSnapshotContentName` → this child (INV-ORPHAN4) |
| **Non-root domain leaf** (`XxxxSnapshot` / content) | **MUST** include **sourceRef** manifest + the one owned PVC manifest | **one** binding for the owned PVC | Real children only | **MUST NOT** be data-only; **sourceRef** NotFound/deleted before capture → **fail-closed**; >1 VCR binding → terminal |
| **Manifest-only** (no data path) | As required by scope | **nil** | As planned | No VCR/orphan leg for this node |

---

## 3. Examples (ownership scope)

### 3.1 Namespace residual/orphan PVC (root scope) — fans out to child volume nodes

```text
NamespaceSnapshotContent (root, aggregator)
├── manifestCheckpointName → MCP
│   └── targets: … residual namespace objects …   (NO residual PVC manifests here)
├── dataRef: nil
└── childrenSnapshotContentRefs[]
    ├── DomainSnapshotContent (real domain decomposition)
    └── <root>-vol-<first12(sha256(<root>|<pvcUID>))>  (child volume node per orphan/loose PVC)
        ├── manifestCheckpointName → own MCP (sanitized manifest for THIS PVC only)
        ├── dataRef: { target: PVC(uid), artifact: VolumeSnapshotContent }
        └── handle: CSI VolumeSnapshot, status.boundSnapshotContentName → this child
```

Root is an **aggregator** (`dataRef=nil`): each loose/orphan PVC becomes a **standalone child volume node**, so its data binding and PVC manifest move off the root entirely. A second orphan PVC → a **second child volume node**, never a second entry on one node.

**Residual root PVC discovery (allowed):**

```text
PVC candidates (namespace list MAY be used here)
  − pvcUIDs covered by child/domain subtree (dataRef on children, pending VCR, E5)
  − policy exclusions
= residual root-owned PVC → one child volume node each (own MCR/MCP + dataRef)
```

Listing is **candidate discovery** only; **ownership** still follows tree coverage and dedup — not “every PVC in namespace belongs to root”. Child volume contents are skipped when computing subtree coverage so their PVCs stay in scope while their manifests are excluded from the root MCR.

### 3.2 Domain leaf with one PVC (same content)

```text
DomainSnapshotContent  (e.g. DemoVirtualDisk snapshot — a real volume leaf)
├── MCP: domain sourceRef manifest + the one owned PVC manifest
├── dataRef: pvcUID → VSC for that PVC
└── children[]: none (the leaf already is the volume node)
```

A domain content owns **exactly one** PVC (real decomposition gives 1 PVC/leaf). The foundation VCR may still return `status.dataRefs[]`; if it reports **>1** binding for a single domain content, that is a **terminal capture failure**, not a silent `[0]` pick.

### 3.3 Real domain decomposition (VM → disks)

When decomposition is **real** (separate domain snapshot type / lifecycle):

```text
VMSnapshotContent  (aggregator, dataRef=nil)
├── MCP: VM manifest
└── children[] → DiskSnapshotContent (one per disk)
        ├── MCP: disk sourceRef manifest + PVC manifest for that disk
        └── dataRef: pvcUID → VSC for that disk's PVC
```

Every volume is a leaf node with its **own** content + MCP + single `dataRef`. There is no “two dataRefs on one parent” case to special-case.

---

## 4. Capture plan (per logical node)

### 4.1 Domain leaf (one PVC, own dataRef)

Per bound domain `SnapshotContent`, the owning snapshot controller runs **one capture plan**:

1. **Ensure** content + lifecycle ownerRef (unchanged).
2. **Ensure one MCR** with `spec.targets[]` = manifest objects for this scope (sourceRef + the owned PVC), using **explicit-target** filter rules for PVC (see §5).
3. **In parallel, ensure one VCR** with `spec.targets[]` = the one PVC this leaf owns. Foundation creates the **VSC** and fills **`VCR.status.dataRefs[]`**.
4. **Publish** on the **same** `SnapshotContent` (after handoff):
   - `status.manifestCheckpointName` ← `MCR.status.checkpointName` (MCP);
   - `status.dataRef` ← the single `VCR.status.dataRefs[0]` (terminal failure if VCR reports >1).
5. **Temporary** on snapshot: `status.manifestCaptureRequestName` / `status.volumeCaptureRequestName`.
6. **Safe cleanup** of MCR and VCR only after the artifact is bound with ownerRef handoff; then clear request names and delete requests.

### 4.2 Root residual/orphan PVC (child volume node — INV-ORPHAN4)

The root does **not** run a root VCR for residual PVCs. For each loose/orphan PVC the root `SnapshotReconciler`:

1. **EnsureVolumeChildContent**: deterministic child content `<rootContentName>-vol-<first12(sha256(<rootContentName>|<pvcUID>))>`, ownerRef→root (`controller=true`), `spec.deletionPolicy` mirrors root, label `state-snapshotter.deckhouse.io/child-volume-node=true` (authoritative child-volume-node marker, not the name infix); link `root.status.childrenSnapshotContentRefs += child` (idempotent, optimistic lock).
2. **Per-orphan MCR/MCP**: create a `ManifestCaptureRequest` whose targets are **only** that PVC → its own MCP holds the PVC manifest; the residual PVC manifest is **removed** from the root MCR.
3. **CSI VolumeSnapshot handle**: the shared controller writes the orphan VS `status.boundSnapshotContentName` → the child content (D4a, merge-patch + `MergeFromWithOptimisticLock` under `RetryOnConflict`). The VS stays in `childrenSnapshotRefs`, now as a real content child (not visibility-only).
4. **Publish** on the **child** content: `status.manifestCheckpointName` (own MCP) + `status.dataRef` (single VSC). The root's `dataRef` stays **nil**.

`SnapshotContentController` validates persisted refs only; it does **not** create MCR/VCR. MCP and data legs are **not** sequential phases; the node is incomplete until its required MCP and (when applicable) its single `dataRef` are published and validated.

---

## 5. PVC manifest handling

### 5.1 Scoped capture (not blanket “no PVC in root”)

| Rule | Detail |
|------|--------|
| **Who includes PVC in MCP** | The `SnapshotContent` that **owns that PVC's `dataRef`** — i.e. the **child volume node** for an orphan/loose PVC, or the domain leaf for a domain-owned PVC (co-ownership). The root MCP does **not** hold residual PVC manifests. |
| **Who must not duplicate** | Any other scope — enforced by subtree dedup + E5 manifest exclude + data `pvcUID` dedup |
| **Per-orphan MCR** | Each orphan child volume node has its **own** MCR whose `spec.targets` = that single PVC; the residual PVC is removed from the root MCR |
| **Explicit-target filter** | For objects listed in `MCR.spec.targets`, **do not** apply global `storageKinds` skip; **do not** skip PVC solely because it has Pod/StatefulSet `ownerReferences` when it is an explicit target |

### 5.2 Sanitized PVC in MCP

- Use existing `CleanObjectForSnapshot` PVC branch (drop `status`, `spec.volumeName`, etc.).
- MCP holds **metadata/spec snapshot**; volume bytes only in VSC.

---

## 6. API: singular `status.dataRef` (Variant A)

**Единственный data artifact contract.** Поле **`status.dataRef`** — singular object (cardinality ≤1), enforced **структурно** типом CRD (`type: object`, не массив; CEL не нужен); список `dataRefs[]` удалён. SCC readiness итерирует `dataRef` как срез длины 0/1. publish из VCR (домен) или VS→VSC (orphan) после handoff. **root** = aggregator (`dataRef=nil`); residual/orphan PVC планируется как **child volume node** (свой MCR/MCP + один `dataRef`), а не как запись на root; duplicate `pvcUID` в поддереве — fail-closed. Domain VCR может вернуть `status.dataRefs[]` массивом — для доменного content >1 binding это **terminal capture failure**, не молчаливый `[0]`. **Delivery:** [`implementation-plan.md`](implementation-plan.md) **§2.4.5**.

### 6.1 Status shape (sketch)

```yaml
status:
  manifestCheckpointName: mcp-...
  dataRef:                       # singular object, omitted on aggregator/manifest-only nodes
    targetUID: "<pvc-uid>"
    target:
      apiVersion: v1
      kind: PersistentVolumeClaim
      namespace: demo
      name: data
      uid: "<pvc-uid>"
    artifact:
      apiVersion: snapshot.storage.k8s.io/v1
      kind: VolumeSnapshotContent
      name: snapcontent-123
  childrenSnapshotContentRefs: [ ... ]   # multi-PVC scope lives here, as child volume nodes
```

### 6.2 OpenAPI / schema requirements

| Requirement | Rule |
|-------------|------|
| **Type** | `status.dataRef` is an **object** (`*SnapshotDataBinding`), **not** an array |
| **Required sub-field** | `targetUID` is **required**, `MinLength=1` when `dataRef` is present |
| **Cardinality** | ≤1 by construction; ≥2-on-one-content is unrepresentable (no list to hold it) |
| **Multi-PVC** | Modeled as `childrenSnapshotContentRefs[]` child volume nodes, each its own singular `dataRef` |
| **Readers / writers** | **`dataRef` only** — no `dataRefs[]` fallback, no dual-write; the `DataRefList()` bridge exposes it as a 0/1 slice for generic helpers |
| **Per-ref status** | Optional later; v1 uses aggregate `SnapshotContent.Ready` |

### 6.3 `VolumeCaptureRequest` (storage-foundation) → singular publish

The foundation VCR contract is unchanged (`spec.targets[]`, `status.dataRefs[]`); state-snapshotter scopes a **domain** VCR to **one** PVC and publishes its single binding:

```yaml
spec:
  targets:
    - apiVersion: v1
      kind: PersistentVolumeClaim
      namespace: demo
      name: data
      uid: "<pvc-uid>"
status:
  dataRefs:                      # foundation list; domain content expects exactly one
    - target: { ... }
      artifact:
        apiVersion: snapshot.storage.k8s.io/v1
        kind: VolumeSnapshotContent
        name: snapcontent-123
  conditions:
    - type: Ready
      status: "True"
```

| Requirement | Rule |
|-------------|------|
| **Targets** | A domain VCR's `spec.targets[]` lists the **one** PVC the leaf owns |
| **Result** | `status.dataRefs[0]` is published to the content's singular `status.dataRef` |
| **Guard** | `len(status.dataRefs) > 1` for a single content → **terminal** `VolumeCaptureFailed`, never truncate |
| **Orphan leg** | No root VCR; the orphan child volume node binds a CSI `VolumeSnapshot` → VSC into its own `dataRef` |

---

## 7. Ready aggregation

```text
SnapshotContent Ready =
  ManifestsReady (MCP Ready)
∧ DataReady   (the single dataRef.artifact is Ready, or no dataRef)
∧ ChildrenReady  (∀ child SnapshotContent are Ready)
```

- **Manifest-only node:** no `dataRef` (data leg N/A).
- **Child volume node:** becomes Ready when **its own** MCP is Ready **and** its single `dataRef` VSC is Ready.
- **Aggregator root/VM (dataRef=nil):** Ready via **ManifestsReady** (own MCP, possibly an empty archive) **+ ChildrenReady** over `childrenSnapshotContentRefs` (the orphan/disk volume nodes). A single pending `dataRef` surfaces `DataCapturePending` as "0/1 ready".

`SnapshotContentController` order remains: manifest → data (the single ref) → children.

Snapshot-level **`Ready`** continues to mirror bound content (**§3.8** spec).

---

## 8. Dedup and tree ownership

| Topic | Rule |
|-------|--------|
| **Data dedup key** | `pvcUID` — **one** VSC per `pvcUID` per snapshot-run |
| **Manifest dedup** | E5 exclude + “already in child/domain MCP” — unchanged; child volume node MCPs hold their PVC manifest, root MCR excludes it |
| **Subtree coverage** | `CollectSubtreeCoveredPVCUIDs` descends `childrenSnapshotContentRefs` and unions each node's singular `dataRef`; **child volume contents are skipped** as coverage so their own PVC stays in scope for their leg |
| **Tree SoT** | Ownership via `children*Refs` + dedup — **not** “namespace PVC list alone”. List **MAY** seed **residual root** candidates only (**§3.1**) |
| **Root vs domain overlap** | PVC covered by a child/domain `dataRef` or pending VCR in subtree → **must not** be re-planned on root |
| **Pod using PVC** | **Not** ownership; may affect `dataConsistency` on the owning snapshot node only |

---

## 9. Restore (future data path)

When restore applies PVC manifests from a content’s MCP:

1. For each PVC object in MCP, find the matching **`dataRef`** on the **same** content node (a child volume node holds exactly its PVC manifest + its single `dataRef`).
2. **Primary match:** `target.uid` (stable across rename).
3. **Fallback:** `(apiVersion, kind, namespace, name)` from manifest metadata.
4. Build VRR (or equivalent) using `artifact` as VSC source.

Missing binding for a PVC present in MCP → **fail-closed** for restore-with-data (not silent skip).

**Aggregator root** (`dataRef=nil`): the restore resolver traverses `childrenSnapshotContentRefs`; an orphan PVC's manifest + `dataRef` are resolved from its **child volume node** (reached via the CSI `VolumeSnapshot` handle `status.boundSnapshotContentName`), and each child is emitted as its own restore node. The root contributes only its namespace manifests + children.

---

## 10. Boundary: state-snapshotter vs storage-foundation

| Responsibility | Module |
|----------------|--------|
| Snapshot tree, child volume nodes for orphan PVCs, per-content MCR + (domain) VCR / (orphan) VS handle, publish singular `dataRef`, Ready, dedup, scoped PVC filters | **state-snapshotter** |
| VCR contract (`spec.targets[]`, `status.dataRefs[]`), VSC per target, extended-VS (`status.boundSnapshotContentName`), CSI, sidecar/Rook | **storage-foundation** |
| Aggregated manifests HTTP API | **manifest-only** read path; unchanged |

---

## 11. Delivery roadmap

**SSOT:** [`implementation-plan.md`](implementation-plan.md) **§2.4.5** — **PR-0 … PR-9** table + per-PR implementation checklist.

| PR | Summary |
|----|---------|
| PR-0 | Docs stabilization ✅ |
| PR-1 | API contract + mechanical consumers ✅ |
| PR-2 | SCC `resolveDataReadiness` over the data leg ✅ |
| PR-3 | Restore tree publish / end-to-end restore graph ✅ |
| PR-F | storage-foundation: VCR (`targets[]`, `status.dataRefs[]`) + extended-VS fork ✅ |
| PR-4 | state-snapshotter: per-content VCR, publish from VCR → content ✅ |
| PR-5 | Scoped PVC manifests ✅ |
| PR-6 | Ownership/dedup ✅ |
| PR-7 | Envtest 2-PVC subtree vertical slice ✅ (`n5_pr7_two_pvc_integration_test.go`) |
| PR-8 | E2E local-thin (`hack/demo-e2e.sh`) ✅ |
| PR-9 | Runbook/status |
| **C2 / Variant A** | `dataRefs[] → singular dataRef`; orphan PVC → child volume node (own MCR/MCP + dataRef + VS handle); INV-ORPHAN4 inversion; restore traverses children |

**Variant A model:** ≤1 `dataRef` per content; a multi-PVC scope is **child volume nodes**, each a full node (own MCP + single `dataRef` + namespaced handle). **Optional later:** Pod → `dataConsistency`.

**Gate:** N5 PRs **MUST NOT** ship inside N2 manifests-only without explicit gate.

---

## 12. Test themes

| ID | Theme |
|----|--------|
| D1 | Domain content: MCR `Targets[]` includes sourceRef + the one owned PVC manifest |
| D2 | Domain leaf: one VCR target → single `dataRef` → Ready VSC; VCR `dataRefs[]` len>1 → terminal |
| D3 | Content `Ready=False` until its single `dataRef` ready (DataCapturePending "0/1 ready") |
| D4 | Root: residual/orphan PVC → child volume node (root `dataRef=nil`), not a root entry |
| D4a | Orphan VS handle: `status.boundSnapshotContentName` → child volume content (INV-ORPHAN4) |
| D5 | Domain-covered PVC: not duplicated/re-planned on root |
| D6 | Duplicate `pvcUID` across subtree children → fail-closed |
| D7 | Manifest-only / aggregator node: `dataRef=nil`, MCP only |
| D8 | Non-root content: MCP must contain sourceRef manifest (not data-only) |
| D9 | Restore: two orphan PVCs → two child volume nodes, each its own MCP+`dataRef` matched |

---

## 13. Document map

| Document | Role |
|----------|------|
| This file | Target **design** for logical content + dual-capture |
| [`spec/system-spec.md`](../spec/system-spec.md) **§3.9** | Normative MUST/MUST NOT |
| [`implementation-plan.md`](implementation-plan.md) **§2.4.5** | PR-0…PR-9 + checklist |

When design and spec disagree during implementation, update **spec first**, then this file.
