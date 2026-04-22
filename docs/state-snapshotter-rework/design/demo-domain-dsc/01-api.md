# Demo domain API (CRD)

**Статус:** Proposed (этап дизайна).  
**Группа / версия (черновик):** отдельная группа, например **`demo.state-snapshotter.deckhouse.io/v1alpha1`** — **не** смешивать с `storage.deckhouse.io` и shipping unified snapshot kinds. Итоговое имя группы — на ревью.

## Цель

CRD **эмулируют** продуктовую модель виртуализации (VM → диски → PVC) и snapshot-ы к ним, без копирования боевых API виртуализации в этот модуль.

## Ключевая модель (после ревью)

- **`NamespaceSnapshot`** — **текущий** root (один на сценарий «снимок namespace»); по дереву это **тот же** класс узла, что любой `XxxxSnapshot`, не отдельный «тип дерева» ([`08-universal-snapshot-tree-model.md`](08-universal-snapshot-tree-model.md)). Под root **в этом треке** **нет** вложенного **`NamespaceSnapshot`** (**INV-T1**, [`05`](05-tree-and-graph-invariants.md)): дети — **heterogeneous** kinds; «верхность» — только отсутствие в продукте snapshot **выше** namespace. Связь с поддеревом — **только** **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`** и **`Ready`**.
- Дочерние узлы — **heterogeneous** kinds: доменные (**DemoVirtualMachineSnapshot** / **DemoVirtualDiskSnapshot**) и при необходимости **VolumeSnapshot** (CSI) + соответствующие **`*SnapshotContent`** / CSI content types.
- Логическое дерево — **только** общие **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`** (элементы с **GVK + namespace + name** или эквивалент); **без** отдельных `domainChild*Refs` и без параллельного «domain-only» графа. Сегодняшний spec может описывать элементы refs минимально — **расширение содержимого тех же полей** переносится в `spec/system-spec.md` вместе с PR5 ([`08`](08-universal-snapshot-tree-model.md) §A, согласование с PR1).

## Inventory CRD (**не** входят в v1)

Как в **[`05-tree-and-graph-invariants.md`](05-tree-and-graph-invariants.md) §0:** **`DemoVirtualMachine`** / **`DemoVirtualDisk`** в **v1 не входят**. Отдельные inventory CRD — **возможное расширение после v1**; до введения inventory весь состав VM/дисков задаётся **self-contained** в **`DemoVirtualMachineSnapshot.spec`** (и политикой доменного контроллера). **Не** трактовать этот раздел как обязательную часть первой итерации demo snapshot flow.

## Snapshot-ресурсы (обязательные для трека)

| Kind | Роль |
|------|------|
| **DemoVirtualMachineSnapshot** | Snapshot-узел VM: **доменный** контроллер (DSC) создаёт связанные **DemoVirtualDiskSnapshot** / content и **отражает** их в **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`**; агрегирует **`Ready`** по §1 [`07-ready-delete-matrix.md`](07-ready-delete-matrix.md). **Не** `NamespaceSnapshot`. |
| **DemoVirtualDiskSnapshot** | **Нижний доменный snapshot-узел** (не «leaf дерева» в буквальном смысле): ниже остаются **DemoVirtualDiskSnapshotContent**, **VolumeSnapshot** и артефакты данных/манифеста. **VolumeSnapshot** для PVC + **MCR→MCP** для manifest-части диска; связанный **DemoVirtualDiskSnapshotContent** (или согласованное имя). |

Имена **VirtualMachineSnapshot** / **VirtualDiskSnapshot** в тексте ревью = продуктовая абстракция; в demo-группе — префикс **Demo*** для изоляции.

## Поля (минимум для ревью) — v1 **self-contained**, без inventory

### DemoVirtualMachineSnapshot

- **Без** обязательного **`DemoVirtualMachine`:** `spec` **self-contained** — логический идентификатор VM (поле и формат — зафиксировать до кода: строка, внешний ref и т.д.) + перечень дисков через **`pvcRef`** (или эквивалент), согласованный с [`05`](05-tree-and-graph-invariants.md) §0.
- Опционально: политика класса / лейблов для дисковых snapshot.

### DemoVirtualDiskSnapshot

- **До кода нужно выбрать ровно один** вариант v1 (иначе API считать **незамороженным**):
  1. **PVC-only:** например **`spec.persistentVolumeClaimRef`** `{ name, namespace }` — **required**; без **`DemoVirtualDisk`**.
  2. **`spec.virtualDiskRef`** — только если введены inventory CRD (**после v1**); не смешивать с v1 без inventory.

- Связь с родителем: `spec.parentVirtualMachineSnapshotRef` и/или **ownerRef** на родительский snapshot kind — см. [`03-snapshot-flow.md`](03-snapshot-flow.md).

## Связь с NamespaceSnapshot

- Один **`NamespaceSnapshot`** + один корневой **`NamespaceSnapshotContent`** для **namespace-level** manifest capture (как N2a сегодня для root).
- Объекты **ниже** root в этом run — **те же** отдельные snapshot kinds (demo, CSI и др.), связанные с root **только** через **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`**; **ownerRef** — lifecycle/GC, **не** источник истины дерева и **не** dedup ([`04-coverage-dedup.md`](04-coverage-dedup.md), [`08` часть B](08-universal-snapshot-tree-model.md)).

## Открытые вопросы на ревью

1. Точная схема **элементов** в **`childrenSnapshotRefs` / `childrenSnapshotContentRefs`** (один список heterogeneous children; совместимость с PR4 traversal после обновления spec).
2. Финальный выбор для **DemoVirtualDiskSnapshot** v1: пункт **1** или **2** в разделе полей выше (PVC-only vs deferred `virtualDiskRef` с inventory).

## Implementation notes (кратко)

- Новые типы — отдельный пакет под `api/`, отдельные CRD YAML, **без** изменения существующих shipping типов, кроме **согласованных** non-breaking расширений полей графа на `NamespaceSnapshot` / `NamespaceSnapshotContent` после апрува.
