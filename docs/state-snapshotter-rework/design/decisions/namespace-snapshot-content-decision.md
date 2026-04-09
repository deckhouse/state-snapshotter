# Decision: NamespaceSnapshot uses generic SnapshotContent in MVP

## Status

Accepted for **MVP** scope described in [`../namespace-snapshot-controller.md`](../namespace-snapshot-controller.md). Revisit if generic `SnapshotContent` proves insufficient for namespace-state payloads or policy.

**Согласованность:** binding root ↔ content, Ready, artifact ownership, MCR как internal-only — как в основном design (§4.2, §8.5, §10–§11). Отдельный **gate по API scope** (cluster vs namespaced) — [`namespace-snapshot-scope.md`](namespace-snapshot-scope.md); на выбор content kind он не влияет.

## Context

Namespace snapshot нужен как **first-class root** в unified snapshot модели. Публичная модель состояния в MVP: **NamespaceSnapshot → SnapshotContent → artifact metadata** (без обязательного MCR как второго источника правды). Варианты носителя результата:

1. Отдельный CRD **`NamespaceSnapshotContent`** (пара 1:1 с NamespaceSnapshot по аналогии с другими доменами).
2. Общий **`SnapshotContent`** с дискриминатором типа снимка / ссылкой на root и общим shared lifecycle.

В репозитории уже есть **generic** `SnapshotContent` контроллер, тесты на финализаторы, linking, orphaning и т.д. Bootstrap-код исторически мог ссылаться на пару `NamespaceSnapshot` / `NamespaceSnapshotContent` — это создаёт расхождение с выбранным здесь направлением.

## Decision

В **MVP** NamespaceSnapshot **не** вводит отдельный `NamespaceSnapshotContent`. Результат фиксируется в **общем `SnapshotContent`**, с двусторонним binding на root (см. основной design, §4.2).

## Consequences

- **Плюсы:** один content-runtime, меньше special-case CRD, быстрее стабилизировать **binding contract** root ↔ content, переиспользование существующих паттернов cleanup/status.
- **Минусы:** поля `SnapshotContent` должны уметь выражать **namespace-state** artifact (metadata, backend location, тип снимка); возможная конкуренция по «ширине» схемы с другими видами снимков — контролировать версионированием и дискриминатором.
- **Миграция:** правки `pkg/unifiedbootstrap`, CRD, RBAC, регистрация пары в DSC при необходимости — отдельными изменениями, согласованными с [`../namespace-snapshot-controller.md`](../namespace-snapshot-controller.md) §14.

## Alternatives considered

1. **Отдельный NamespaceSnapshotContent** — проще изолировать схему под namespace-only поля, но дублирует lifecycle и внимание оператора на второй kind; отложено за пределы MVP.
2. **Только MCR/ManifestCheckpoint как публичный результат** — даёт дублирование состояния с root и усложняет модель «один root — один носитель результата»; в MVP MCR не является обязательным публичным контрактом для этого сценария (см. основной design §10).
