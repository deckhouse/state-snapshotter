## Orphan PVC capture via standard CSI VolumeSnapshot (root residual data leg)

> **Import addendum (ADR `2026-06-15-snapshot-export-import.md` §12, 2026-06-26).** Этот документ — capture-side. Импорт данных orphan-PVC (extended `VolumeSnapshot`) теперь идёт через единый маркер `spec.source.import: {}` (вместо `spec.source.dataImportName`), а владеющий `DataImport` находится reverse-lookup по `DataImport.spec.targetRef`. Форк extended-VS добавляет `status.{storageClassName,size,volumeMode}`, которые `volumesnapshotimport` зеркалит из `DataImport.spec`. Capture-семантика ниже не меняется.

> **Ready-gate addendum (conditions-model ADR `2026-06-03-snapshot-conditions-model.md` §2.3, 2026-06-28).** Финальная residual/orphan-PVC-волна теперь **явно гейтит первый `Ready=True`** корневого `SnapshotContent`. Завершив волну (нет orphan-таргетов **или** вся финальная orphan-PVC-волна захвачена и готова), reconciler штампует латч-поле `SnapshotContent.status.residualVolumeCapture.phase=Complete` (MergeFrom-патч, идемпотентно; reconciler — единственный писатель). До латча агрегатор держит `Ready=False/ResidualVolumeCapturePending` (fail-closed, низший приоритет среди ног `Ready`), детерминированно убирая флап `Ready` True→False→True, который возникал, когда корень преждевременно показывал `Ready=True` до завершения orphan-волны. Монотонно + upgrade-guard (уже-`Ready=True` корень не пере-гейтится). Точная механика ноги (какая именно нога роняла `Ready` при поздней линковке orphan-узла) — в §2.3 conditions-model.

- **Status:** Proposed (2026-06-09). Capture-side only; restore deferred.
- **Scope:** namespace (root) `Snapshot` residual data leg for **orphan/uncovered PVCs**.
- **Supersedes (narrowly):** parts of the N5 root-residual data leg that publish `SnapshotContent.status.dataRefs[]` from a **`VolumeCaptureRequest`** for the **root** scope; and the blanket prohibition on creating `VolumeSnapshot` in state-snapshotter (system-spec §3.9.9 / line "no shadow VolumeSnapshot").
- **Unchanged:** domain/non-root nodes keep the **VCR** path verbatim (§3.9.5–§3.9.6). Restore semantics and `restore-rollout-guard` are not touched (this ADR is capture-side only).

### Context

Today the root namespace node captures **residual** PVCs (namespace PVCs minus subtree-covered minus exclusions) through the same execution machinery as domain nodes: it creates a `VolumeCaptureRequest` (VCR), which produces a `VolumeSnapshotContent` (VSC), then publishes the VSC into root `SnapshotContent.status.dataRefs[]` (see `internal/controllers/snapshot/volume_capture.go`, `internal/usecase/volumecapture/domain_owned_targets.go`).

Two problems for the **orphan PVC** case:

1. **VCR is execution-request machinery owned by the domain layer.** We want the namespace/root controller to be "thin" for volumes it does not own: an orphan PVC (no domain controller, no CSD mapping) should be snapshotted with the **standard CSI mechanism** (`VolumeSnapshot` / `VolumeSnapshotContent`), not with our internal VCR.
2. **Restore of an orphan PVC** must work with only standard primitives and **no new entities**: the PVC manifest already lives in the root `ManifestCheckpoint` (PVC is in the manifest allowlist), and the volume artifact lives in root `dataRefs[]` as a VSC. Restore (future) recreates the PVC from the MCP manifest with `spec.dataSource` → a `VolumeSnapshot` rebuilt from the stored VSC.

### Decision (Shape A)

For the **root namespace node only**, the residual data leg is implemented with standard CSI `VolumeSnapshot` instead of VCR:

1. **Coverage is unchanged.** `subtree-covered pvcUIDs` are collected from **child SnapshotContents' `dataRefs`** (`CollectSubtreeCoveredPVCUIDs`). Orphan = namespace PVC − covered − policy exclusions. Dedup stays content-driven.
2. **No VCR from the namespace controller.** For each orphan PVC the root controller creates a standard `snapshot.storage.k8s.io/v1` **`VolumeSnapshot`** (`spec.source.persistentVolumeClaimName` = the PVC). It MUST NOT create a VCR for residual PVCs.
3. **Wait for bind.** Read `VolumeSnapshot.status.boundVolumeSnapshotContentName` and `status.readyToUse=true`.
4. **Publish durable artifact.** Add the bound **VSC** to root `SnapshotContent.status.dataRefs[]` (`target` = source PVC identity incl. `pvcUID`; `artifact` = `VolumeSnapshotContent`). The VSC MUST be durable independent of the VS: the `VolumeSnapshot` MUST use a class/policy whose **`deletionPolicy=Retain`** for the bound VSC, and the controller MUST hand off the VSC **ownerRef → root `SnapshotContent`** (same durable-artifact handoff as §3.9.6).
5. **Visibility as a Snapshot-level leaf.** The `VolumeSnapshot` is recorded in **`Snapshot.status.childrenSnapshotRefs[]`** (`apiVersion`/`kind`/`name`) for tree visibility and lifecycle only. It is **NOT** added to `SnapshotContent.status.childrenSnapshotContentRefs[]`. Therefore content readiness aggregation and PVC dedup are unaffected: orphan-PVC readiness flows through the **data leg** (`VolumesReady` via `dataRefs[]`), exactly as residual capture does today.
6. **PVC manifest** continues to be captured into the root MCP (PVC is in the allowlist). No change.
7. **VolumeSnapshot is never a manifest.** No `VolumeSnapshot` object may enter any MCP / manifest inventory / aggregated manifests, for **all** VS (not only ours). The allowlist already excludes it; add an explicit capture-side filter so a future allowlist change cannot leak VS.
8. **Domain path unchanged.** Domain/non-root controllers keep creating VCRs and publishing `dataRefs[]` from VCR output (§3.9.5–§3.9.6).
9. **No API expansion now.** `dataRefs[]` is not extended with a `snapshotRef`. Leave a code-level TODO only:
   ```text
   // TODO: later add snapshotRef to dataRefs:
   // snapshotRef:
   //   apiVersion: snapshot.storage.k8s.io/v1
   //   kind: VolumeSnapshot
   //   namespace: <ns>
   //   name: <vs-name>
   ```

### Invariants

- **INV-ORPHAN1:** the namespace/root controller MUST NOT create a `VolumeCaptureRequest` for residual/orphan PVCs. VCR remains domain-owned.
- **INV-ORPHAN2:** an orphan PVC's volume artifact is a **VSC bound from a standard CSI `VolumeSnapshot`**, durable in root `dataRefs[]` with ownerRef → root `SnapshotContent`; the VS deletion policy MUST retain the VSC.
- **INV-ORPHAN3:** `VolumeSnapshot` objects MUST NOT appear in any MCP / manifest inventory / aggregated manifests (capture-side filter), for all VS. The MCP MUST still hold the PVC manifest.
- **INV-ORPHAN4:** an orphan-PVC VS leaf appears **only** in `Snapshot.status.childrenSnapshotRefs[]` (visibility/lifecycle), **never** in `SnapshotContent.status.childrenSnapshotContentRefs[]`. Aggregation (`ChildrenReady`) and dedup remain content-driven and MUST NOT observe the VS leaf.
- **INV-ORPHAN5:** coverage/dedup is unchanged — `subtree-covered pvcUIDs` come from child SnapshotContents; orphan = not covered. At most one data capture per `pvcUID` per run (preserves INV-P1).
- **INV-ORPHAN6 (Ready-gate, 2026-06-28):** the root `SnapshotContent` MUST NOT emit its **first** `Ready=True` until the reconciler has latched `status.residualVolumeCapture.phase=Complete` on completion of the final orphan-PVC wave (fail-closed). Only the reconciler writes the latch (status field, MergeFrom, idempotent); the aggregator reads it locally and never writes it. The latch is monotonic (never reverts; an already-`Ready=True` root is not re-gated, so a controller rollout cannot flap `Ready`). See conditions-model ADR §2.3.

### Lifecycle / GC

- The `VolumeSnapshot` is a per-run **execution/visibility** object: ownerRef → root `Snapshot` (or root ObjectKeeper, consistent with existing root lifecycle), so it is garbage-collected with the run.
- The bound **VSC** is the **durable artifact**: `deletionPolicy=Retain`, ownerRef → root `SnapshotContent`; it is removed by the unified content deletion algorithm, not by VS deletion.
- Pending/failed states reuse the existing data-leg taxonomy (§3.9.10): not-yet-`readyToUse` → `DataCapturePending` with progress count; missing/invalid artifact → terminal (`ArtifactMissing` / `DataArtifactInvalid`).

### Conflicts explicitly resolved (this ADR supersedes for the orphan-root case only)

- **§3.9.9 "no shadow VolumeSnapshot workarounds; remains storage-foundation":** narrow exception — root MAY create a **standard** CSI `VolumeSnapshot` for orphan PVC residual capture. We still do NOT implement CSI driver wiring / sidecars / Ceph-Rook compatibility; the external-snapshotter performs the work.
- **§3.9.5 / §3.9.6 "dataRefs ← VCR; VCR is the bulk volume mechanism":** the **root residual** data leg MAY publish `dataRefs[]` from a CSI-VS-bound VSC instead of from a VCR. Domain nodes keep VCR.
- **§3.9.4 "children only for real domain decomposition; MUST NOT one-volume→one-content":** preserved. The VS leaf is a `Snapshot`-level visibility ref, **not** a `SnapshotContent` node; no per-PVC content node is created. No one-volume→one-content content nodes are introduced.

### Alternatives considered

- **Shape B — real child `SnapshotContent` (volume node) per orphan PVC.** Rejected for now: creates a content entity per PVC and is exactly the one-volume→one-content shape §3.9.4 forbids.
- **Keep VCR on root.** Rejected per requirement "VCR is domain-only".
- **Status quo (residual VCR→VSC on root, no VS).** Insufficient: requirement is standard CSI VS + tree visibility for orphan PVCs.
- **VS as `childrenSnapshotContentRefs` entry without a content.** Rejected as unimplementable: the aggregator `Get`s a common `SnapshotContent` per child ref (→ `NotFound` → permanent `ChildrenPending`) and dedup walks child contents (→ double capture).

### Scope / stage / out of scope

- **Stage:** new slice within the N5 / namespace-flow track (root residual data leg moves to CSI VS for orphan PVCs). Declared in `operations/project-status.md`.
- **Out of scope:** restore implementation (future), `snapshotRef` API field, changing the domain VCR path, VSC for covered PVCs.
- **Tests:** orphan-PVC capture path (VS created, bound, VSC in root `dataRefs[]`, no VCR), VS-absent-from-MCP guard, existing N5/tree tests stay green.
