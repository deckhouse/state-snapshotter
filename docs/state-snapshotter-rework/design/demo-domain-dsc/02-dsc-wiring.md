# DSC wiring для demo domain

**Статус:** Historical design (частично реализовано). Нормативный контракт — в `spec/system-spec.md`.
> ⚠️ This document contains historical and potentially outdated design decisions.
> Current normative behavior is defined in:
> - [`spec/system-spec.md`](../../spec/system-spec.md)
> - [`design/implementation-plan.md`](../implementation-plan.md) (current state)

## Принцип

**`DomainSpecificSnapshotController` (DSC)** — декларативная регистрация пар **snapshot / snapshot content** для доменных kinds, чтобы они участвовали в **unified registry** и **runtime watches** по существующим правилам (см. [`spec/system-spec.md`](../../spec/system-spec.md) §0, [`operations/dsc-rbac-and-mcr.md`](../../operations/dsc-rbac-and-mcr.md)).

**Разделение понятий:** запуск dedicated controller process не равен активации kind в graph discovery. `DemoVirtualDiskSnapshotController` и `DemoVirtualMachineSnapshotController` стартуют всегда и могут обработать manual snapshot без DSC. `NamespaceSnapshot` создаёт demo children только если соответствующая resource→snapshot mapping пришла из eligible DSC.

**DSC и generic — граница:** DSC **не** определяет состав дерева snapshot-run и **не** используется **`NamespaceSnapshot`** reconciler’ом для обхода или «разрешённых детей». DSC нужен для **регистрации** GVK, **wiring** watches и единообразного runtime (события → reconcile) доменных контроллеров. Состав run задаётся только **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`** в API (нормативно **[`spec/system-spec.md`](../../spec/system-spec.md) §3**, мотивы — [`05-tree-and-graph-invariants.md`](05-tree-and-graph-invariants.md)); generic читает refs **динамически**, без обращения к DSC как к источнику структуры дерева.

Generic **`NamespaceSnapshot`** reconciler **не** получает `if demoKind`: он ведёт **root** namespace capture и опирается на **универсальную** модель дерева — **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`**, единый **`Ready`**, и **вычисляемый** exclude для generic capture (без persisted «domain summary»; см. [`04-coverage-dedup.md`](04-coverage-dedup.md), [`08`](08-universal-snapshot-tree-model.md)).

**Parent link в demo API:** `parentSnapshotRef` — универсальная ссылка на родительский snapshot-узел в namespace-local graph. Для текущего demo wiring поддержка parent kinds ограничена реализацией контроллеров (например, Disk: `NamespaceSnapshot` / `DemoVirtualMachineSnapshot`) и **не** является закрытым списком для общей модели graph/E6.

## Объекты DSC (черновик)

Минимум **два** DSC (или один DSC с двумя парами kinds — если схема и KindConflict это позволяют). Таблица — **пример** demo v1, **не** фиксированный allowlist: новые kinds подключаются **дополнительными** DSC **без** изменений generic **`NamespaceSnapshot`** reconciler ([`05`](05-tree-and-graph-invariants.md) — дерево без compile-time списка детей).

| DSC | Snapshot kind | SnapshotContent kind | Назначение |
|-----|---------------|----------------------|------------|
| A | demo VM snapshot | common content | VM |
| B | demo disk snapshot | common content | Нижний доменный disk-узел |

**VolumeSnapshot** / **VolumeSnapshotContent** — обычно CSI API group; для участия в дереве под root **не** требуется DSC state-snapshotter (если драйвер стандартный). Для текущего PR5a/PR5b это **future work**: demo-путь сейчас опирается на `Demo*Snapshot` + `*Content` и stub `Ready`, без production CSI/data-path.

## Кто что создаёт

| Компонент | Создаёт |
|-----------|---------|
| Пользователь / CI | **Root** `NamespaceSnapshot` (и далее стандартный bind **одного** root `SnapshotContent`). |
| **Demo domain controllers** | Demo VM/Disk snapshot objects, **MCR/MCP** по политике leaf — **не** вложенные **`NamespaceSnapshot`**. Demo controllers владеют planning/execution requests, потому что только они знают domain model: выбирают own manifest scope, создают MCR/VCR/DataExport/VolumeSnapshot requests при необходимости и child snapshots. Demo controllers пишут только свои snapshot status (`boundSnapshotContentName`, `childrenSnapshotRefs`, snapshot `Ready`); common content status writes выполняет **`SnapshotContentController`** как aggregator/lifecycle controller. Запись в **`childrenSnapshotRefs`** на root (и у промежуточных узлов) — **merge-safe** (**INV-REF-M1** / **INV-REF-M2**, [`05-tree-and-graph-invariants.md`](05-tree-and-graph-invariants.md) §1): патчи **не** перетирают чужие элементы и **не** заменяют список целиком; при нескольких писателях — **согласованный** контракт (единый writer, **merge** по ключу элемента ref, **RetryOnConflict** и т.д. — **[`spec/system-spec.md`](../../spec/system-spec.md) §3** и код PR5). |
| Generic NS controller | Root MCR→MCP pipeline, обновление root SnapshotContent/NS статусов, **вычисление** exclude для generic capture по фактам API и дереву refs. |

**Demo orchestrator** — **опционально**: те же функции (синхронизация доменного дерева с root, помощь в refs) могут жить **целиком** в доменных контроллерах. Если выделяется отдельным бинарником/пакетом, он **не** вводит отдельной модели дерева и **не** является источником истины поверх **`children*Refs`**; **не** второй control-plane с неявным SoT.

## Откуда берётся «что снимать» (v1 без inventory CRD)

- В **v1** отдельные **`DemoVirtualMachine`** / **`DemoVirtualDisk`** **не** используются ([`01-api.md`](01-api.md), [`05` §0](05-tree-and-graph-invariants.md)): доменный reconciler опирается на **self-contained** `spec` **`DemoVirtualMachineSnapshot`** / **`DemoVirtualDiskSnapshot`** (PVC, идентификатор VM и т.д.) и на **живой** API (PVC, namespace), **не** на list/watch inventory CRD.
- После возможного введения inventory в **v1+** маппинг «инвентарь → snapshot» станет отдельным решением; generic по-прежнему **без** веток под конкретные demo GVK.

## Приоритет / порядок

- Порядок «disk subtree **не** обгоняет VM snapshot» — **требование к доменной** логике (фазы / `status` / политика контроллеров), **не** механизм, который обеспечивает generic **`NamespaceSnapshot`**. Generic **не** управляет очерёдностью доменных шагов — [`03-snapshot-flow.md`](03-snapshot-flow.md).
- KindConflict / Accepted — как для любых DSC.

## RBAC

- Demo controllers: чтение объектов, нужных по **`spec`** (PVC, namespace и т.д.), создание/обновление **demo snapshot** CRD, **VolumeSnapshot**, **ManifestCaptureRequest**; обновление **refs** на root **merge-safe** (**INV-REF-M1** / **INV-REF-M2**, [`05`](05-tree-and-graph-invariants.md) §1; см. таблицу «Кто что создаёт»). **RBAC:** grant на создание **ещё одного** `NamespaceSnapshot` как ребёнка root **в этом треке не нужен**, потому что дети под root — **heterogeneous** kinds (`Demo*`, `VolumeSnapshot`, …) по **INV-T1** ([`05-tree-and-graph-invariants.md`](05-tree-and-graph-invariants.md)); `NamespaceSnapshot` — **тот же** snapshot-узел, что и любой `XxxxSnapshot`, а «верхность» root — следствие **текущего** продуктового потолка (нет snapshot выше namespace), **не** отдельного класса объектов в kube. **Не** предполагать запись persisted coverage в status root.

## Не делать

- Не описывать и не реализовывать **вложенный** `NamespaceSnapshot` под root для VM/disk вместо heterogeneous детей — это **политика трека** (**INV-T1**), а не утверждение, что `NamespaceSnapshot` «особый» kind или «верхний доменный контроллер» в kube.
- Не добавлять `if demoKind` в generic **`NamespaceSnapshot`** reconciler; не встраивать доменные ветки в **unified** DSC/registry-слой там, где он должен оставаться тип-агностичным (см. [`spec/system-spec.md`](../../spec/system-spec.md) §0, ADR).
- Не использовать **DSC** как источник истины для **состава дерева** snapshot-run (только **`children*Refs`** в API).
