# Demo domain API (CRD)

**Статус:** Historical design (частично реализовано). Нормативный контракт — в `spec/system-spec.md`; этот файл фиксирует контекст и целевую эволюцию.  
> ⚠️ This document contains historical and potentially outdated design decisions.
> Current normative behavior is defined in:
> - [`spec/system-spec.md`](../../spec/system-spec.md)
> - [`design/implementation-plan.md`](../implementation-plan.md) (current state)

**Группа / версия:** `demo.state-snapshotter.deckhouse.io/v1alpha1` (реализовано).

## Цель

CRD **эмулируют** продуктовую модель виртуализации (VM → диски → PVC) и snapshot-ы к ним, без копирования боевых API виртуализации в этот модуль.

## Ключевая модель (текущее состояние)

- **`NamespaceSnapshot`** — **текущий** root (один на сценарий «снимок namespace»); по дереву это **тот же** класс узла, что любой `XxxxSnapshot`, не отдельный «тип дерева» ([`08-universal-snapshot-tree-model.md`](08-universal-snapshot-tree-model.md)). Под root **в этом треке** **нет** вложенного **`NamespaceSnapshot`** (**INV-T1**, [`05`](05-tree-and-graph-invariants.md)): дети — **heterogeneous** kinds; «верхность» — только отсутствие в продукте snapshot **выше** namespace. Связь с поддеревом — **только** **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`** и **`Ready`**.
- Дочерние узлы — **heterogeneous** kinds: доменные (**DemoVirtualMachineSnapshot** / **DemoVirtualDiskSnapshot**). CSI/VolumeSnapshot в demo-треке — future work.
- Логическое дерево — **только** общие **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`** (элементы `childrenSnapshotRefs`: **`apiVersion` + `kind` + `name`**; namespace дочернего snapshot не хранится в ref и берётся от parent `NamespaceSnapshot`); **без** отдельных `domainChild*Refs` и без параллельного «domain-only» графа. Сегодняшний spec может описывать элементы refs минимально — **расширение содержимого тех же полей** переносится в `spec/system-spec.md` вместе с PR5 ([`08`](08-universal-snapshot-tree-model.md) §A, согласование с PR1).

## Inventory CRD (**не** входят в текущую реализацию)

Как в **[`05-tree-and-graph-invariants.md`](05-tree-and-graph-invariants.md) §0:** **`DemoVirtualMachine`** / **`DemoVirtualDisk`** в текущую реализацию не входят. Отдельные inventory CRD — возможное расширение позже.

## Snapshot-ресурсы (обязательные для трека)

| Kind | Роль |
|------|------|
| **DemoVirtualMachineSnapshot** | Snapshot-узел VM: domain wiring в graph refs (`children*Refs`), parent указывается через `parentSnapshotRef` (kind=`NamespaceSnapshot`), `Ready` через generic E6. |
| **DemoVirtualDiskSnapshot** | Доменный disk-узел: создаётся отдельно (standalone или под VM), пишет common content, использует demo Ready stub (`Completed`) в текущем PR5. |

Имена **VirtualMachineSnapshot** / **VirtualDiskSnapshot** в тексте ревью = продуктовая абстракция; в demo-группе — префикс **Demo*** для изоляции.

## Поля (текущая реализация)

### DemoVirtualMachineSnapshot

- `spec.parentSnapshotRef` — положение snapshot-узла в namespace-local дереве.
- `spec.sourceRef` — обязательная ссылка на namespace-local `DemoVirtualMachine`, который снимается (`apiVersion/kind/name`, без `namespace`).
- Список дисков внутри VM spec в текущем PR5b не используется; child disks создаются отдельными `DemoVirtualDiskSnapshot` и связываются через `parentSnapshotRef` (kind=`DemoVirtualMachineSnapshot`).

### DemoVirtualDiskSnapshot

- `spec.sourceRef` — обязательная ссылка на namespace-local `DemoVirtualDisk`, который снимается (`apiVersion/kind/name`, без `namespace`).
- parent указывается только через `spec.parentSnapshotRef` (`apiVersion/kind/name`, namespace-local).
- `parentSnapshotRef` у `DemoVirtualDiskSnapshot` — универсальная ссылка на parent snapshot-узел;
  в **текущей demo-реализации** поддержаны `NamespaceSnapshot` и `DemoVirtualMachineSnapshot`,
  но общая модель graph/E6 не ограничивает будущие parent kinds этими двумя.

## Связь с NamespaceSnapshot

- Один **`NamespaceSnapshot`** + один корневой **`SnapshotContent`** для **namespace-level** manifest capture (как N2a сегодня для root).
- Объекты **ниже** root в этом run — **те же** отдельные snapshot kinds (demo, CSI и др.), связанные с root **только** через **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`**; **ownerRef** — lifecycle/GC, **не** источник истины дерева и **не** dedup ([`04-coverage-dedup.md`](04-coverage-dedup.md), [`08` часть B](08-universal-snapshot-tree-model.md)).

## Открытые вопросы

1. Переход demo с Ready stub к реальному CSI/data-path (VolumeSnapshot/VCR) без нарушения текущего E6 контракта.
2. Совмещение heterogeneous demo traversal с PR4 aggregated read-path в нормативной части spec.

## Implementation notes (кратко)

- Новые типы — отдельный пакет под `api/`, отдельные CRD YAML, **без** изменения существующих shipping типов, кроме **согласованных** non-breaking расширений полей графа на `NamespaceSnapshot` / `SnapshotContent` после апрува.
