# Demo domain API (CRD)

**Статус:** Proposed (этап дизайна).  
**Группа / версия (черновик):** отдельная группа, например **`demo.state-snapshotter.deckhouse.io/v1alpha1`** — **не** смешивать с `storage.deckhouse.io` и shipping unified snapshot kinds. Итоговое имя группы — на ревью.

## Цель

CRD **эмулируют** продуктовую модель виртуализации (VM → диски → PVC) и snapshot-ы к ним, без копирования боевых API виртуализации в этот модуль.

## Ключевая модель (после ревью)

- **`NamespaceSnapshot`** — **текущий** root в этой архитектуре (один на сценарий «снимок namespace»); по правилам дерева он **такой же узел**, как любой `XxxxSnapshot`, а не отдельный «тип дерева» ([`08-universal-snapshot-tree-model.md`](08-universal-snapshot-tree-model.md)). Он **не** порождает **дочерние `NamespaceSnapshot`**.
- Дочерние узлы — **heterogeneous** kinds: доменные (**DemoVirtualMachineSnapshot** / **DemoVirtualDiskSnapshot**) и при необходимости **VolumeSnapshot** (CSI) + соответствующие **`*SnapshotContent`** / CSI content types.
- Логическое дерево — **только** общие **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`** (элементы с **GVK + namespace + name** или эквивалент); **без** отдельных `domainChild*Refs` и без параллельного «domain-only» графа. Сегодняшний spec может описывать элементы refs минимально — **расширение содержимого тех же полей** переносится в `spec/system-spec.md` вместе с PR5 ([`08`](08-universal-snapshot-tree-model.md) §A, согласование с PR1).

## Ресурсы «инвентаря» (опционально для демо-кластера)

| Kind | Назначение |
|------|------------|
| **DemoVirtualMachine** | Логическая VM; `spec` — ссылки на диски (`DemoVirtualMachine` → `DemoVirtualDisk`). |
| **DemoVirtualDisk** | Диск; **`spec.persistentVolumeClaimRef`** (name, namespace) — **required**. |

## Snapshot-ресурсы (обязательные для трека)

| Kind | Роль |
|------|------|
| **DemoVirtualMachineSnapshot** | Snapshot VM: порождает дочерние **DemoVirtualDiskSnapshot** (и их content); агрегирует готовность. **Не** `NamespaceSnapshot`. |
| **DemoVirtualDiskSnapshot** | Leaf домена: **VolumeSnapshot** для PVC + **MCR→MCP** для manifest-части диска; связанный **DemoVirtualDiskSnapshotContent** (или согласованное имя). |

Имена **VirtualMachineSnapshot** / **VirtualDiskSnapshot** в тексте ревью = продуктовая абстракция; в demo-группе — префикс **Demo*** для изоляции.

## Поля (минимум для ревью)

### DemoVirtualDisk

- `spec.persistentVolumeClaimRef`: `{ name, namespace }` — **required**.

### DemoVirtualMachineSnapshot

- `spec.virtualMachineRef` (name, namespace).
- Опционально: политика класса / лейблов для дисковых snapshot.

### DemoVirtualDiskSnapshot

- `spec.virtualDiskRef` (или PVC-only режим — на ревью).
- Связь с родителем: `spec.parentVirtualMachineSnapshotRef` и/или **ownerRef** на родительский snapshot kind — см. [`03-snapshot-flow.md`](03-snapshot-flow.md).

## Связь с NamespaceSnapshot

- Один **`NamespaceSnapshot`** + один корневой **`NamespaceSnapshotContent`** для **namespace-level** manifest capture (как N2a сегодня для root).
- Доменное дерево **под** root — отдельные API-объекты (demo VM/Disk snapshot kinds, VolumeSnapshot), связываемые с root через **те же `children*Refs`**; **ownerRef** — жизненный цикл/GC, **не** источник истины дерева и **не** dedup ([`04-coverage-dedup.md`](04-coverage-dedup.md), [`08` часть B](08-universal-snapshot-tree-model.md)).

## Генерация

- Новые типы — отдельный пакет под `api/`, отдельные CRD YAML, **без** изменения существующих shipping типов, кроме **согласованных** non-breaking расширений полей графа на `NamespaceSnapshot` / `NamespaceSnapshotContent` после апрува.

## Открытые вопросы на ревью

1. Точная схема **элементов** в **`childrenSnapshotRefs` / `childrenSnapshotContentRefs`** (один список heterogeneous children; совместимость с PR4 traversal после обновления spec).
2. Нужны ли оба уровня VM/Disk inventory CRD или достаточно snapshot kinds + PVC.
