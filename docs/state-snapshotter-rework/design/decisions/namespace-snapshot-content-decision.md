# Decision: NamespaceSnapshot paired with SnapshotContent (+ ObjectKeeper)

## Status

**Accepted** — целевая модель внедрения. Детальный поток, поля и примеры — **только** в [`snapshot-rework/`](../../../snapshot-rework/) (файлы в этом каталоге для контракта **не меняем** в пользу `docs/`; инженерные документы здесь только план поставки и ссылки).

**Согласованность:** binding root ↔ content, Ready, artifact ownership, политика удаления — выводить из ТЗ в `snapshot-rework` и этого design. **Gate по API scope** (cluster vs namespaced root) — [`namespace-snapshot-scope.md`](namespace-snapshot-scope.md).

## Context

Нужен явный носитель результата снимка namespace в паре с **`NamespaceSnapshot`**, по тому же паттерну, что и другие доменные `XxxSnapshot` / `SnapshotContent`. Общий **`SnapshotContent`** (`storage.deckhouse.io`) для **корня** namespace-snapshot **не** используем: целевая пара — **`NamespaceSnapshot` + `SnapshotContent`**.

Удержание артефактов и связь с **ObjectKeeper** — как в ТЗ (`snapshot-rework`), без отдельной «миграции» с промежуточной схемы: реализацию **сразу** ведём к NS + NSC + OK.

## Decision

1. Публичная пара для namespace snapshot: **`NamespaceSnapshot`** и **`SnapshotContent`** (CRD, группа/apiVersion — **как в ТЗ** в `snapshot-rework`, при реализации сверять YAML из ТЗ с фактическим CRD в репозитории).
2. **ObjectKeeper** связываем с моделью по ТЗ: для корневого content — сценарий с **FollowObjectWithTTL** (и прочие правила из ТЗ); детали не дублировать здесь.
3. Статус **`NamespaceSnapshot`**: **только `conditions`** (и поля фактов), **без `status.phase`** — [`namespace-snapshot-status-surface.md`](namespace-snapshot-status-surface.md).

## Consequences

- **Bootstrap / unified:** пара GVK `NamespaceSnapshot` / `SnapshotContent` в desired list, DSC при необходимости; убрать опору на generic `SnapshotContent` **именно как носитель результата** для этого root.
- **Код:** новые/обновлённые типы, CRD, reconciler(ы), тесты — под NSC + OK; **не** закладывать миграцию со старого пути через `SnapshotContent` для namespace root.
- **Документы в `docs/`:** остаются планом поставки и выжимкой; **истина по сценарию** — `snapshot-rework/`.

## Supersedes

Ранее зафиксированный вариант «MVP только через общий `SnapshotContent`» для пары namespace root — **снят** этим решением.

## Alternatives considered (кратко)

1. **Только generic `SnapshotContent`** — отклонено для целевой поставки (остаётся возможным для других видов снимков в unified-модели, но не как носитель для namespace root по текущему ТЗ).
2. **Миграция с SnapshotContent на NSC** — не требуется: делаем сразу NSC.
