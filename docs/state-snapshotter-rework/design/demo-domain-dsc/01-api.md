# Demo domain API (CRD)

**Статус:** Proposed (этап дизайна).  
**Группа / версия (черновик):** отдельная группа, например **`demo.state-snapshotter.deckhouse.io/v1alpha1`** — **не** смешивать с `storage.deckhouse.io` и shipping unified snapshot kinds. Итоговое имя группы — на ревью.

## Цель

CRD **эмулируют** продуктовую модель виртуализации (VM → диски → PVC) и snapshot-ы к ним, без копирования боевых API виртуализации в этот модуль.

## Ключевая модель (после ревью)

- **`NamespaceSnapshot`** — **только root** snapshot namespace (один на сценарий «снимок namespace»). Он **не** является универсальным контейнером для каждого дочернего шага и **не** порождает **дочерние `NamespaceSnapshot`**.
- Дочерние узлы дерева — **heterogeneous snapshot kinds**: доменные (например **VirtualMachineSnapshot** / **VirtualDiskSnapshot** в demo-именовании — **DemoVirtualMachineSnapshot** / **DemoVirtualDiskSnapshot**) и при необходимости **VolumeSnapshot** (CSI) + соответствующие **`*SnapshotContent`** / CSI content types.
- Связь root NS ↔ этим деревом — через **поля статуса / refs на граф** (формат — отдельное решение: расширение текущих `childrenSnapshotRefs` только на NS **недостаточно**; нужны ссылки на **GVK + namespace + name** или параллельная структура на `NamespaceSnapshot` / `NamespaceSnapshotContent` — фиксируется при переносе в spec).

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
- Доменное дерево **под** root — отдельные API-объекты (demo VM/Disk snapshot kinds, VolumeSnapshot), связываемые с root через **граф в статусе** (и ownerRef/GC — отдельно от dedup, см. [`04-coverage-dedup.md`](04-coverage-dedup.md)).

## Генерация

- Новые типы — отдельный пакет под `api/`, отдельные CRD YAML, **без** изменения существующих shipping типов, кроме **согласованных** non-breaking расширений полей графа на `NamespaceSnapshot` / `NamespaceSnapshotContent` после апрува.

## Открытые вопросы на ревью

1. Точная схема **refs с root NS на heterogeneous children** (одно поле vs несколько; совместимость с PR4 traversal).
2. Нужны ли оба уровня VM/Disk inventory CRD или достаточно snapshot kinds + PVC.
