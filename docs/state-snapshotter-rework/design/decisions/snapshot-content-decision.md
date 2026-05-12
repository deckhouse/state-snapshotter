# Decision: Snapshot paired with SnapshotContent (+ ObjectKeeper)

## Status

**Historical / superseded by common `SnapshotContent` model.** Нормативный контракт сейчас — [`../spec/system-spec.md`](../../spec/system-spec.md); `snapshot-rework/` остаётся длинной историей решений, если явно помечено как historical.

**Согласованность:** binding root ↔ content, Ready, artifact ownership, политика удаления — выводить из ТЗ в `snapshot-rework` и этого design. **Gate по API scope** (cluster vs namespaced root) — [`snapshot-scope.md`](snapshot-scope.md).

## Context

Нужен явный носитель результата снимка namespace в паре с **`Snapshot`**, по тому же паттерну, что и другие доменные `XxxSnapshot` / `SnapshotContent`. Текущая целевая пара — **`Snapshot` + общий `SnapshotContent`**.

Удержание артефактов и связь с **ObjectKeeper** — как в ТЗ (`snapshot-rework`), без отдельной «миграции» с промежуточной схемы: реализацию **сразу** ведём к NS + SnapshotContent + OK.

## Decision

1. Публичная пара для snapshot: **`Snapshot`** и **`SnapshotContent`** (CRD, группа/apiVersion — **как в ТЗ** в `snapshot-rework`, при реализации сверять YAML из ТЗ с фактическим CRD в репозитории).
2. **ObjectKeeper** связываем с моделью по ТЗ: для корневого content — сценарий с **FollowObjectWithTTL** (и прочие правила из ТЗ); детали не дублировать здесь.
3. Статус **`Snapshot`**: **только `conditions`** (и поля фактов), **без `status.phase`** — [`snapshot-status-surface.md`](snapshot-status-surface.md).

## Consequences

- **Bootstrap / unified:** пара GVK `Snapshot` / общий `SnapshotContent` в desired list, CSD при необходимости.
- **Код:** новые/обновлённые типы, CRD, reconciler(ы), тесты — под SnapshotContent + OK; **не** закладывать миграцию со старого пути через `SnapshotContent` для namespace root.
- **Документы в `docs/`:** остаются планом поставки и выжимкой; **истина по сценарию** — `snapshot-rework/`.

## Supersedes

Ранее зафиксированный вариант отдельного namespace content — **снят** текущей common-content моделью.

## Alternatives considered (кратко)

1. **Отдельный namespace content** — superseded: текущий runtime использует общий `SnapshotContent`.
2. **Миграционный dual-mode** — не требуется для v0: legacy content API/CRD удалены.
