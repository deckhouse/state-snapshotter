# Demo domain-specific nested snapshot (via DSC)

**Статус:** Proposed — **сначала документы → ревью → только потом код.**  
**Назначение:** reference-модель «как в виртуализации» + **data dedup** (PVC / VolumeSnapshot) и **resource dedup** (доменные объекты не дважды в subtree и не в generic root MCP); замена **synthetic tree** как основного способа проверки дерева.

## ADR (кратко)

| | |
|--|--|
| **Контекст** | N2a / N2b + PR4; synthetic scaffold не проверяет реальный domain wiring и риск двойного snapshot **данных** и **доменных объектов**. |
| **Решение** | Под корневым **`NamespaceSnapshot`** (снимок namespace как **root**) живёт **heterogeneous** дерево **domain-specific snapshot kinds** (например VM/Disk snapshot + при необходимости **VolumeSnapshot** и их `*Content`), подключённых через **DSC** и те же pipeline **MCR→MCP** / **VCR→VolumeSnapshot** где нужно. **Вложенные `NamespaceSnapshot` не порождаются** — это не «PR5 = child NS вместо synthetic», а **реальное дерево разных snapshot kinds**. |
| **Инвариант** | Generic path **не** повторно захватывает ресурс, уже покрытый более специфичным domain subtree — минимум **PVC-backed data** и **доменные объекты** (например VirtualDisk). См. [`04-coverage-dedup.md`](04-coverage-dedup.md). |
| **Ограничения** | Demo API отдельно от shipping; код изолирован; **не** код до апрува пакета. Текущий PR4 aggregated / NSC graph — **расширение** под heterogeneous узлы — отдельный шаг в спеке/плане после согласования дерева. |

## Документы этапа 1 (архитектурный обзор)

| # | Файл | Содержание |
|---|------|------------|
| 1 | [`01-api.md`](01-api.md) | Demo CRD, связь с root NS **без** child NS. |
| 2 | [`02-dsc-wiring.md`](02-dsc-wiring.md) | DSC; demo controllers; без child NS. |
| 3 | [`03-snapshot-flow.md`](03-snapshot-flow.md) | Root NS → domain/CSI snapshot tree; reconcile; ownerRef **≠** dedup. |
| 4 | [`04-coverage-dedup.md`](04-coverage-dedup.md) | Data + resource dedup; coverage/exclude; ownerRef только иерархия/GC. |
| — | [`../../testing/demo-domain-dsc-test-plan.md`](../../testing/demo-domain-dsc-test-plan.md) | Сценарии тестов. |

## Документы этапа 2 (фиксация перед кодом)

| # | Файл | Содержание |
|---|------|------------|
| 5 | [`05-tree-and-graph-invariants.md`](05-tree-and-graph-invariants.md) | Таблица дерева kinds; **INV-T**/**INV-G**; **refs** `domainChild*`; **generic vs domain**; **ownerRef** по kind. |
| 6 | [`06-coverage-dedup-keys.md`](06-coverage-dedup-keys.md) | Ключ **«тот же PVC»** / диск; схема **`status.domainCoverage`**; инварианты **INV-P**/**INV-D**/**INV-C**. |
| 7 | [`07-ready-delete-matrix.md`](07-ready-delete-matrix.md) | Ready leaf (manifest **и** volume); failed propagation; **delete/finalizers** по kind. |

**Минимальный API v1** зафиксирован в **§0** файла [`05-tree-and-graph-invariants.md`](05-tree-and-graph-invariants.md).

## Связь с существующими SSOT

- Namespace / N2: [`namespace-snapshot-controller.md`](../namespace-snapshot-controller.md), [`implementation-plan.md`](../implementation-plan.md) §2.4, [`spec/system-spec.md`](../../spec/system-spec.md).
- Aggregated manifests сегодня завязаны на обход **NSC**-графа; для heterogeneous kinds потребуется **согласованное расширение** — после фиксации модели: [`spec/namespace-snapshot-aggregated-manifests-pr4.md`](../../spec/namespace-snapshot-aggregated-manifests-pr4.md) + план.
- DSC / RBAC: [`operations/dsc-rbac-and-mcr.md`](../../operations/dsc-rbac-and-mcr.md).

## Synthetic tree

- **Не** основной механизм для нового трека; scaffold остаётся для регрессии до миграции тестов.
