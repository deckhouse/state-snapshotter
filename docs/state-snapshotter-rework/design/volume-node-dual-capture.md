# Logical SnapshotContent: manifest + data (`dataRefs[]`)

**Status:** Accepted target design; **docs roadmap stabilized** (PR-0 ✅). Normative: [`spec/system-spec.md`](../spec/system-spec.md) **§3.9**. **PR backlog PR-0…PR-9:** [`implementation-plan.md`](implementation-plan.md) **§2.4.5**.

**Supersedes:** earlier draft that treated **`SnapshotContent` as one volume artifact** (single `dataRef`, synthetic PVC child nodes per extra PVC). **Target model:** one **`SnapshotContent` = one logical snapshot node** with bulk manifests (MCR `Targets[]`) and **zero or more** durable data bindings (`dataRefs[]`).

**Scope:** state-snapshotter planning and execution. **Out of scope:** VCR/VSC CSI wiring, Ceph/Rook sidecar compatibility, shadow `VolumeSnapshot` — **storage-foundation** (plain reference when available in the monorepo workspace: `storage-foundation/docs/63742164-sidecar-migration.md`).

**Related:** [`demo-domain-csd/05-tree-and-graph-invariants.md`](demo-domain-csd/05-tree-and-graph-invariants.md); dedup — [`demo-domain-csd/06-coverage-dedup-keys.md`](demo-domain-csd/06-coverage-dedup-keys.md) §4; lifecycle — [`demo-domain-csd/090-unified-snapshot-controller-lifecycle.md`](demo-domain-csd/090-unified-snapshot-controller-lifecycle.md).

---

## 1. Core formula

```text
SnapshotContent (logical snapshot node) =
  one manifestCheckpointName → MCP (0..N manifest objects via MCR.spec.targets[])
+ zero or more dataRefs[]     → PVC target identity ↔ VSC artifact
+ zero or more childrenSnapshotContentRefs[] → real domain decomposition only
```

**Not:** `SnapshotContent = one volume` or `SnapshotContent = one data artifact`.

**Co-ownership invariant (manifest + data):** for every PVC captured in this scope, the **sanitized PVC manifest** and the matching **`dataRefs[]` entry** (same `pvcUID` / target identity) **MUST** live on the **same** `SnapshotContent`. Another scope **MUST NOT** duplicate that PVC manifest or data binding.

**Symmetric bulk requests (target model):**

```text
ManifestCaptureRequest                    VolumeCaptureRequest
├── spec.targets[]  (manifest objects)    ├── spec.targets[]  (PVC / volume targets)
└── status.checkpointName → MCP           └── status.dataRefs[] → VSC bindings

SnapshotContent (published durable refs)
├── status.manifestCheckpointName  ← from MCR.status.checkpointName
└── status.dataRefs[]              ← from VCR.status.dataRefs[] (after handoff)
```

| Rule | Detail |
|------|--------|
| **One active MCR per logical content** | `spec.targets[]` = all manifest targets in ownership scope |
| **One active VCR per logical content** | `spec.targets[]` = all PVC/data targets in ownership scope (**not** N separate VCRs) |
| **Parallel legs** | MCR and VCR may run in parallel; node incomplete until both durable results are published |
| **Temporary tracking** | `Snapshot.status.manifestCaptureRequestName` and `Snapshot.status.volumeCaptureRequestName` (singular each) |

**storage-foundation prerequisite:** if current `VolumeCaptureRequest` is single-PVC only, add a **foundation PR** before N5 data capture: bulk `spec.targets[]`, `status.dataRefs[]`, `Ready=True` only when **all** targets complete. See [`implementation-plan.md`](implementation-plan.md) **§2.4.5** **PR-F**.

---

## 2. Node roles

| Role | MCP | `dataRefs[]` | `children` | Notes |
|------|-----|--------------|------------|--------|
| **Namespace root** (`Snapshot` / root content) | May be **empty** or residual namespace manifests only (after E5 exclude) | **Residual** PVCs owned by root scope, not covered by children | Real domain decomposition | Aggregator-first; `Ready` may depend mainly on children |
| **Non-root domain** (`XxxxSnapshot` / content) | **MUST** include at least **sourceRef** object manifest; **MAY** include owned PVC manifests | 0..N PVC/VSC for PVCs this node owns | Real children only | **MUST NOT** be data-only; **sourceRef** NotFound/deleted before capture → **fail-closed** |
| **Manifest-only** (no data path) | As required by scope | **Empty** (or omitted) | As planned | No VCR for this node |

---

## 3. Examples (ownership scope)

### 3.1 Namespace residual PVC (root scope)

```text
NamespaceSnapshotContent
├── manifestCheckpointName → MCP
│   └── targets: … residual namespace objects … + sanitized PVC manifests for root-owned PVCs
├── dataRefs[]:
│   └── { target: PVC(uid), artifact: VolumeSnapshotContent }
└── childrenSnapshotContentRefs[] → domain contents
```

Root **does not** create synthetic child snapshot nodes **only** because a second PVC appeared. Extra PVCs → **additional `dataRefs[]` entries** (and matching PVC entries in the same MCR `Targets[]` when root owns them).

**Residual root PVC discovery (allowed):**

```text
PVC candidates (namespace list MAY be used here)
  − pvcUIDs covered by child/domain subtree (dataRefs, pending VCR, E5)
  − policy exclusions
= residual root-owned PVC → root dataRefs[] + root MCR targets
```

Listing is **candidate discovery** only; **ownership** still follows tree coverage and dedup — not “every PVC in namespace belongs to root”.

### 3.2 Domain object with multiple PVCs (same content)

```text
DomainSnapshotContent  (e.g. VM snapshot)
├── MCP: domain sourceRef manifest + sanitized PVC manifests for PVCs owned here
├── dataRefs[]: one entry per owned PVC (pvcUID → VSC)
└── children[]: only if product requires real decomposition (e.g. separate DemoVirtualDisk snapshot CRD)
```

### 3.3 Real domain decomposition (optional child)

When decomposition is **real** (separate domain snapshot type / lifecycle), not a PVC-counting trick:

```text
VMSnapshotContent
├── MCP: VM manifest (+ optional VM-owned PVCs if policy keeps them on VM node)
└── children[] → DiskSnapshotContent
        ├── MCP: disk sourceRef manifest + PVC manifest for that disk
        └── dataRefs[]: pvcUID → VSC for that disk's PVC
```

**Do not** add a child node **only** because `dataRefs[]` already has two entries on the parent.

---

## 4. Dual-capture plan (one MCR + one VCR per logical node)

Per bound `SnapshotContent`, the owning snapshot controller runs **one capture plan**:

1. **Ensure** content + lifecycle ownerRef (unchanged).
2. **Ensure one MCR** with `spec.targets[]` = all manifest objects for this scope (sourceRef + owned PVCs + allowed namespace residuals), using **explicit-target** filter rules for PVC (see §5).
3. **In parallel, ensure one VCR** with `spec.targets[]` = all PVC/data targets this scope owns (0..N). Foundation controller creates **VSC** per target and fills **`VCR.status.dataRefs[]`**.
4. **Publish** on the **same** `SnapshotContent` (after handoff, never publish requests as durable refs):
   - `status.manifestCheckpointName` ← `MCR.status.checkpointName` (MCP);
   - `status.dataRefs[]` ← `VCR.status.dataRefs[]` (cluster-scoped artifacts, e.g. VSC).
5. **Temporary** on snapshot: `status.manifestCaptureRequestName` and `status.volumeCaptureRequestName` (at most **one** of each per logical node).
6. **Safe cleanup** of MCR and VCR only after **all** required artifacts are bound to `SnapshotContent` with ownerRef handoff; then clear request names and delete requests.
7. **`SnapshotContentController`** validates persisted refs only; does **not** create MCR/VCR.

MCR and VCR are **not** sequential phases (“manifests first, then data”). Either request may finish first; the node is incomplete until **required** MCP and **required** `dataRefs[]` are published and validated.

---

## 5. PVC manifest handling

### 5.1 Scoped capture (not blanket “no PVC in root”)

| Rule | Detail |
|------|--------|
| **Who includes PVC in MCP** | The `SnapshotContent` whose **ownership scope** includes that PVC (root residual, domain node, disk child, etc.) |
| **Who must not duplicate** | Another scope that does not own the PVC — enforced by subtree dedup + E5 manifest exclude + data `pvcUID` dedup |
| **Bulk allowlist** | Root namespace MCR still uses allowlist − E5; **PVC targets are added explicitly** to `Targets[]` when root owns residual PVCs |
| **Explicit-target filter** | For objects listed in `MCR.spec.targets`, **do not** apply global `storageKinds` skip; **do not** skip PVC solely because it has Pod/StatefulSet `ownerReferences` when it is an explicit target |

### 5.2 Sanitized PVC in MCP

- Use existing `CleanObjectForSnapshot` PVC branch (drop `status`, `spec.volumeName`, etc.).
- MCP holds **metadata/spec snapshot**; volume bytes only in VSC.

---

## 6. Target API: `dataRefs[]` (present since PR-1; publish/readiness later)

**Единственный data artifact contract.** Поле **`status.dataRefs[]`** в CRD с **PR-1**; SCC readiness — **PR-2**; publish из VCR — **PR-4** ✅ (после **PR-F** ✅). **PR-6** ✅ production owned PVC planning: **root** = namespace PVC candidates − subtree-covered `targetUID` (`dataRefs[]` + pending VCR on descendants); **domain node** = this content's `dataRefs[]` + pending VCR only (not full namespace list); duplicate `pvcUID` fail-closed. Stub annotation **test-only**. **PR-5** ✅ scoped MCP + residual PVC MCR targets. **PR-7** ✅ envtest vertical slice (not production SoT). **PR-4 runtime:** MCR и VCR — независимые bulk-leg (`ensureVolumeCaptureLeg` не блокирует MCR); publish только после validate → VSC ownerRef handoff → `dataRefs[]`; при `VolumeCaptureFailed` VCR и `volumeCaptureRequestName` остаются для отладки. Singular `status.dataRef` удалён, **без** legacy bridge. **Delivery:** [`implementation-plan.md`](implementation-plan.md) **§2.4.5**.

### 6.1 Status shape (sketch)

```yaml
status:
  dataRefs:
    - target:
        apiVersion: v1
        kind: PersistentVolumeClaim
        namespace: demo
        name: data
        uid: "<pvc-uid>"
      artifact:
        apiVersion: snapshot.storage.k8s.io/v1
        kind: VolumeSnapshotContent
        name: snapcontent-123
```

### 6.2 OpenAPI / schema requirements

| Requirement | Rule |
|-------------|------|
| **List type** | **`listType=map`** on `status.dataRefs` |
| **Map key** | Prefer **`listMapKey=target.uid`** if kubebuilder supports nested key cleanly |
| **Map key fallback** | If nested `listMapKey` is inconvenient, add flat **`targetUID`** (or equivalent) on each entry and use **`listMapKey=targetUID`** |
| **Uniqueness** | At most one entry per **`pvcUID`** (`target.uid` / `targetUID`) per `SnapshotContent`; **`artifact.name`** unique per content |
| **Readers / writers** | **`dataRefs[]` only** — no `dataRef` fallback, no dual-write |
| **Old `dataRef`** | Remove or **deprecated** in CRD; not used in code paths |
| **Per-ref status** | Optional later; v1 uses aggregate `SnapshotContent.Ready` |
| **In-flight VCR** | `Snapshot.status.volumeCaptureRequestName` → **one** bulk VCR per logical node |

### 6.3 Bulk `VolumeCaptureRequest` (target; storage-foundation)

Illustrative symmetric shape (foundation owns CRD/controller):

```yaml
spec:
  targets:
    - apiVersion: v1
      kind: PersistentVolumeClaim
      namespace: demo
      name: data
      uid: "<pvc-uid>"
status:
  dataRefs:
    - target: { ... }
      artifact:
        apiVersion: snapshot.storage.k8s.io/v1
        kind: VolumeSnapshotContent
        name: snapcontent-123
  conditions:
    - type: Ready
      status: "True"   # only when all spec.targets[] have artifacts Ready
```

| Requirement | Rule |
|-------------|------|
| **Targets** | `spec.targets[]` lists every PVC in the ownership scope (same identities as MCR PVC manifests / `SnapshotContent.dataRefs[].target`) |
| **Result** | `status.dataRefs[]` on VCR mirrors durable bindings before publish to `SnapshotContent` |
| **Ready** | `VCR Ready=True` only when **all** targets have Ready artifacts |
| **Not target** | N separate VCR objects per PVC on one logical content |

---

## 7. Ready aggregation

```text
SnapshotContent Ready =
  MCP Ready
∧ (∀ required dataRefs[].artifact are Ready)
∧ (∀ child SnapshotContent are Ready)
```

- **Manifest-only node:** no required `dataRefs[]` (empty list satisfies data leg).
- **Node that owns N PVCs:** **all N** bindings must reach Ready VSC; partial progress → content **not Ready**.
- **Aggregator-only root:** still publishes **`manifestCheckpointName`** → **Ready MCP** with **empty archive** (0 objects). **MCP Ready** = that checkpoint is Ready, not “no MCP”. Children still gate aggregate Ready when applicable.

`SnapshotContentController` order remains: manifest → data (all refs) → children.

Snapshot-level **`Ready`** continues to mirror bound content (**§3.8** spec).

---

## 8. Dedup and tree ownership

| Topic | Rule |
|-------|--------|
| **Data dedup key** | `pvcUID` — **one** VSC per `pvcUID` per snapshot-run |
| **Manifest dedup** | E5 exclude + “already in child/domain MCP” — unchanged |
| **Tree SoT** | Ownership via `children*Refs` + dedup — **not** “namespace PVC list alone”. List **MAY** seed **residual root** candidates only (**§3.1**) |
| **Root vs domain overlap** | PVC covered by child/domain `dataRefs[]` or pending VCR in subtree → **must not** appear again on root |
| **Pod using PVC** | **Not** ownership; may affect `dataConsistency` on the owning snapshot node only |

---

## 9. Restore (future data path)

When restore applies PVC manifests from a content’s MCP:

1. For each PVC object in MCP, find matching entry in **`dataRefs[]`** on the **same** content node.
2. **Primary match:** `target.uid` (stable across rename).
3. **Fallback:** `(apiVersion, kind, namespace, name)` from manifest metadata.
4. Build VRR (or equivalent) using `artifact` as VSC source.

Missing binding for a PVC present in MCP → **fail-closed** for restore-with-data (not silent skip).

**Aggregator-only root** with **empty MCP**: restore still has valid **`manifestCheckpointName`** (empty archive); data restore for residual PVCs uses root **`dataRefs[]`**; subtree data/manifests via **children**.

---

## 10. Boundary: state-snapshotter vs storage-foundation

| Responsibility | Module |
|----------------|--------|
| Snapshot tree, ensure one MCR + one VCR per content, publish refs from request status, Ready, dedup, scoped PVC filters | **state-snapshotter** |
| Bulk VCR contract (`spec.targets[]`, `status.dataRefs[]`), VSC per target, CSI, sidecar/Rook | **storage-foundation** |
| Aggregated manifests HTTP API | **manifest-only** read path; unchanged |

---

## 11. Delivery roadmap

**SSOT:** [`implementation-plan.md`](implementation-plan.md) **§2.4.5** — **PR-0 … PR-9** table + per-PR implementation checklist.

| PR | Summary |
|----|---------|
| PR-0 | Docs stabilization ✅ |
| PR-1 | API contract + mechanical consumers (`dataRefs[]`; remove `dataRef`) ✅ |
| PR-2 | SCC `resolveDataReadiness` over `dataRefs[]` ✅ |
| PR-3 | Restore tree publish / end-to-end restore graph with `dataRefs[]` ✅ |
| PR-F | storage-foundation: bulk VCR (`targets[]`, `status.dataRefs[]`) — see `docs/pr-f-bulk-volumecapturerequest.md` |
| PR-4 | state-snapshotter: one VCR per content, publish from VCR → content |
| PR-5 | Scoped PVC manifests ✅ |
| PR-6 | Ownership/dedup ✅ |
| PR-7 | Envtest 2-PVC subtree vertical slice ✅ (`n5_pr7_two_pvc_integration_test.go`) |
| PR-8 | E2E local-thin (`hack/pr8-smoke.sh`) ✅ |
| PR-9 | Runbook/status |

**Removed:** synthetic PVC child nodes; one-volume-per-content. **Optional later:** Pod → `dataConsistency`.

**Gate:** N5 PRs **MUST NOT** ship inside N2 manifests-only without explicit gate.

---

## 12. Test themes

| ID | Theme |
|----|--------|
| D1 | Domain content: MCR `Targets[]` includes sourceRef + owned PVC manifests |
| D2 | Same content: one bulk VCR with N targets → N `dataRefs[]` → N Ready VSC |
| D3 | Content `Ready=False` until all required dataRefs ready |
| D4 | Root: residual PVC on root `dataRefs[]`, not on child-only-for-PVC |
| D5 | Domain-covered PVC: not duplicated on root |
| D6 | Duplicate `pvcUID` in one run → fail-closed |
| D7 | Manifest-only node: empty `dataRefs[]`, MCP only |
| D8 | Non-root content: MCP must contain sourceRef manifest (not data-only) |
| D9 | Restore: two PVCs in MCP → two distinct VSC via `dataRefs[]` match |

---

## 13. Document map

| Document | Role |
|----------|------|
| This file | Target **design** for logical content + dual-capture |
| [`spec/system-spec.md`](../spec/system-spec.md) **§3.9** | Normative MUST/MUST NOT |
| [`implementation-plan.md`](implementation-plan.md) **§2.4.5** | PR-0…PR-9 + checklist |

When design and spec disagree during implementation, update **spec first**, then this file.
