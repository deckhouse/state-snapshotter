# Architecture overview (pointer)

Диаграммы и подробные описания паттернов контроллеров этого модуля находятся в корневом каталоге **[`docs/architecture/`](../../architecture/)** (например `controller-pattern.md`, `ready-condition-*.md`).

При добавлении PlantUML-обзора всего модуля можно разместить файл `system-overview.puml` **здесь** рядом с этим README, чтобы соответствовать структуре `docs/state-snapshotter-rework/architecture/` из правил репозитория.

**Навигация по rework-докам:** [`../README.md`](../README.md) (CSI / `storage.deckhouse.io` / manifest / unified), runbook и CSD — в [`../operations/`](../operations/).

**Состояние реализации (кратко):** reconciler **`CustomSnapshotDefinition`** — `internal/controllers/csd/controller.go` (статусы + после успешного reconcile вызов **`unifiedruntime.Syncer.Sync`** для additive watch и обновления layered state). Пакет **`pkg/unifiedruntime`** — явные слои desired/eligible/merged/resolved, monotonic active keys, Prometheus gauges и лог при **stale** active. Детали и галочки — [`../design/implementation-plan.md`](../design/implementation-plan.md), техдизайн — [`../design/r2-phase-2b-r3-runtime-registry.md`](../design/r2-phase-2b-r3-runtime-registry.md).
