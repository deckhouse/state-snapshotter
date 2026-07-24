# Historical design notes

This directory contains historical design notes and ADR drafts from earlier snapshot architecture iterations.
These documents may mention superseded concepts such as:

- `NamespaceSnapshot`
- `NamespaceSnapshotContent`
- `Demo*SnapshotContent`
- `contentCRDName`
- `SnapshotContent.spec.snapshotRef`
- `SnapshotContentController` owning all `SnapshotContent.status`

They are kept for context only and are not the current implementation contract.
Current source of truth:

- `docs/internal/state-snapshotter-rework/spec/system-spec.md`
- `docs/internal/state-snapshotter-rework/testing/pre-e2e-smoke-validation.md`
- `docs/internal/state-snapshotter-rework/operations/project-status.md`

Legacy terms are allowed only in explicitly historical documents. Do not use historical documents as implementation instructions without checking the current spec.

---

# snapshot-rework

Здесь лежат **расширенные ADR и черновики** по unified snapshots, DSC и смежным темам. Нормативные выдержки для реализации и тестов — в **[`docs/internal/state-snapshotter-rework/`](../docs/internal/state-snapshotter-rework/)** (spec, design, testing, operations).

Визуальная схема unified snapshots (ownerRef vs логика, scope, ObjectKeeper): [`unified-snapshot-detailed.png`](unified-snapshot-detailed.png) · исходник [`unified-snapshot-detailed.drawio`](unified-snapshot-detailed.drawio). Ссылка также в шапке [`unified-origin.md`](unified-origin.md).

**Этапы и текущий фокус** — в [`design/implementation-plan.md`](../docs/internal/state-snapshotter-rework/design/implementation-plan.md) и [`operations/project-status.md`](../docs/internal/state-snapshotter-rework/operations/project-status.md).

| Тема в старом указателе `snapshot-rework/plan/dorabotki-i-testy.md` (удалён) | Канонический документ |
|------------------------------------------------------------------------------|------------------------|
| §0–1 registry/runtime, контекст продукта | [`spec/system-spec.md`](../docs/internal/state-snapshotter-rework/spec/system-spec.md) |
| §2, §4–5 дорожная карта, открытые вопросы | [`design/implementation-plan.md`](../docs/internal/state-snapshotter-rework/design/implementation-plan.md) |
| §3 тесты, команды | [`testing/e2e-testing-strategy.md`](../docs/internal/state-snapshotter-rework/testing/e2e-testing-strategy.md) |
| Прогресс / стадии | [`operations/project-status.md`](../docs/internal/state-snapshotter-rework/operations/project-status.md) |

При смене контракта обновляй **`docs/internal/state-snapshotter-rework/spec/system-spec.md`** и при необходимости соответствующий ADR в этом каталоге.

**Conditions snapshot/SnapshotContent (PlanningReady / ManifestsReady / VolumesReady / ChildrenReady / Ready) + failure propagation:** draft ADR — [`2026-06-03-snapshot-conditions-model.md`](2026-06-03-snapshot-conditions-model.md). Нормативные выдержки переносятся в `spec/system-spec.md` §3.8 / §3.9.7.

**Orphan PVC root residual via standard CSI VolumeSnapshot (capture-side):** ADR — [`2026-06-09-orphan-pvc-csi-volumesnapshot.md`](2026-06-09-orphan-pvc-csi-volumesnapshot.md). Нормативная выдержка — `spec/system-spec.md` §3.9.11.

**N2b PR4 (aggregated manifests download):** current normative contract — [`spec/snapshot-aggregated-read.md`](../docs/internal/state-snapshotter-rework/spec/snapshot-aggregated-read.md) and [`api/snapshot-read.md`](../docs/internal/state-snapshotter-rework/api/snapshot-read.md); разбиение по PR — [`design/implementation-plan.md`](../docs/internal/state-snapshotter-rework/design/implementation-plan.md) §2.4.2; кластерный smoke — [`testing/e2e-testing-strategy.md`](../docs/internal/state-snapshotter-rework/testing/e2e-testing-strategy.md).

**Delete protection (unified snapshot):** нормативный контракт — [`design/delete-protection-contract.md`](../docs/internal/state-snapshotter-rework/design/delete-protection-contract.md); план внедрения — [`docs/2026-07-20-1640-unified-snapshot-delete-protection.plan.md`](../docs/2026-07-20-1640-unified-snapshot-delete-protection.plan.md). Авторитетный маркер `state-snapshotter.deckhouse.io/delete-protected`, break-glass `deckhouse.io/allow-delete`, admission delete-guard (VAP, `enforcement: Audit|Deny`), fail-fast деградация по `deletionTimestamp` (contract §10.1). Алгоритм каскадного удаления — [`unified-snapshot-deletion-algorithm.md`](unified-snapshot-deletion-algorithm.md); он **не** отменяет delete-guard, а работает под ним (exempt-акторы каскада перечислены в контракте).

**Restore manifests compiler (`/manifests-with-data-restoration` rework):** ADR — [`2026-06-10-restore-manifests-compiler.md`](2026-06-10-restore-manifests-compiler.md). Общий core для `/manifests` и restore, read-path restore-safe фильтр, `targetNamespace` rewrite, post-order трансформация снизу вверх, внутренние restore-трансформеры (без новых HTTP subresources). Нормативные выдержки после согласования — в `spec/snapshot-aggregated-read.md` (restore-раздел).
