# Дерево snapshot kinds, граф на root и ownerRef (инварианты v1)

**Статус:** Proposed — **зафиксировано для согласования перед кодом.**  
**Область:** только demo-domain трек; shipping `childrenSnapshotRefs` (только child **NamespaceSnapshot**) не меняем семантически — добавляем **параллельное** представление графа.

---

## 0. Минимальный API итерации v1 (зафиксировано)

| Решение | Значение |
|---------|----------|
| **Inventory CRD** (`DemoVirtualMachine`, `DemoVirtualDisk`) | **Не входят в v1.** Источник правды для состава VM — поля **`DemoVirtualMachineSnapshot.spec`** (имя/идентификатор логической VM + список дисков с **`pvcRef`**). Отдельные inventory CRD — **v1.1+**, если понадобится ближе к продукту. |
| **`DemoVirtualMachineSnapshotContent`** | **Да, в v1** — отдельный kind (DSC-пара к VM snapshot): хранение ссылок на MCP / агрегат состояния VM-ветки. |
| **`DemoVirtualDiskSnapshotContent`** | **Да, в v1** — leaf content для MCP диска + связь с **VolumeSnapshot**. |

Итого **обязательные** demo snapshot kinds в v1: **`DemoVirtualMachineSnapshot`**, **`DemoVirtualMachineSnapshotContent`**, **`DemoVirtualDiskSnapshot`**, **`DemoVirtualDiskSnapshotContent`**. Плюс стандартные **VolumeSnapshot** / **VolumeSnapshotContent** (CSI API).

---

## 1. Кто чей ребёнок (единственная таблица)

**INV-T1.** Под **root `NamespaceSnapshot`** **не** создаётся ни одного дочернего **`NamespaceSnapshot`**.

| Родитель (логический узел) | Допустимые дети (kinds) | Примечание |
|---------------------------|-------------------------|------------|
| **`NamespaceSnapshot`** (root) | `DemoVirtualMachineSnapshot` | 0..N (для v1 достаточно 0..1). |
| **`NamespaceSnapshot`** (root) | `DemoVirtualDiskSnapshot` | **Только standalone** диск: диск **не** входит ни в одну VM из `spec` текущего root run (см. **INV-T2**). |
| **`DemoVirtualMachineSnapshot`** | `DemoVirtualDiskSnapshot` | По списку дисков из `spec` VM snapshot. |
| **`DemoVirtualMachineSnapshot`** | `DemoVirtualMachineSnapshotContent` | Ровно **1:1** с VM snapshot (как пара snapshot/content). |
| **`DemoVirtualDiskSnapshot`** | `DemoVirtualDiskSnapshotContent` | Ровно **1:1**. |
| **`DemoVirtualDiskSnapshot`** | `VolumeSnapshot` | Ровно **один** VS на дисковый snapshot в v1 (PVC). |
| **`VolumeSnapshot`** | *(нет детей в этом дереве)* | Leaf данных. |

**INV-T2. Взаимоисключающая роль `DemoVirtualDiskSnapshot`.** У каждого объекта **`DemoVirtualDiskSnapshot`** — **ровно один** родитель-контейнер снимка:

- либо **`spec.parentRef` указывает на `DemoVirtualMachineSnapshot`** (диск под VM),
- либо **`spec.parentRef` указывает на root `NamespaceSnapshot`** (standalone диск),

**но не оба.** Один и тот же **логический диск** (тот же `pvcRef` / тот же ключ инвентаря из [`06-coverage-dedup-keys.md`](06-coverage-dedup-keys.md)) **не** может иметь **два** активных `DemoVirtualDiskSnapshot` в рамках одного root run (resource dedup).

---

## 2. Отражение в refs на `NamespaceSnapshot` / `NamespaceSnapshotContent`

Существующие **`status.childrenSnapshotRefs`** / **`status.childrenSnapshotContentRefs`** в shipping API остаются для **child NamespaceSnapshot** (N2b legacy). Для heterogeneous v1 вводится **отдельное** поле (черновое имя — согласовать в CRD bump):

| Поле (черновик) | Ресурс | Содержимое элемента |
|-----------------|--------|---------------------|
| **`status.domainChildSnapshotRefs`** | `NamespaceSnapshot` | `{ "apiGroup", "kind", "namespace", "name" }` — только прямые дети root: VM snapshot(s) и **только standalone** disk snapshot(s). |
| **`status.domainChildSnapshotContentRefs`** | `NamespaceSnapshotContent` (root) | `{ "apiGroup", "kind", "namespace", "name" }` — content объекты, соответствующие **прямым** детям (например `DemoVirtualMachineSnapshotContent`, `DemoVirtualDiskSnapshotContent` для standalone дисков). |

**INV-G1.** Дети VM-ветки (**`DemoVirtualDiskSnapshot`** под **`DemoVirtualMachineSnapshot`**) **не** дублируются в **`domainChildSnapshotRefs`** root NS — они видны через обход от VM snapshot (aggregated / readiness — см. [`07-ready-delete-matrix.md`](07-ready-delete-matrix.md)).

**INV-G2.** Корневой **`NamespaceSnapshotContent`** по-прежнему несёт **`manifestCheckpointName`** для **root namespace MCP**; MCP доменных leaf остаются на **`DemoVirtualDiskSnapshotContent`** (и при необходимости на VM snapshot content).

**Имя полей** (`domainChild*` vs другое) — финализировать в OpenAPI при реализации; до смены CRD допустима **временная** сериализованная аннотация **только для demo** — явно пометить как tech debt в PR.

---

## 3. Граница generic vs domain (контракт для PR5)

### Generic `NamespaceSnapshot` controller **обязан** уметь (без импорта demo-пакетов / без `if Demo*`):

| # | Обязанность |
|---|-------------|
| G1 | Bind **одного** root **`NamespaceSnapshotContent`**; root MCR→MCP; статусы N2a/N2b для **root** MCP. |
| G2 | Читать с **root `NamespaceSnapshot`** (или связанного объекта по согласованному контракту) **только** структурированные данные: **`status.domainCoverage`** (или эквивалент — см. [`06-coverage-dedup-keys.md`](06-coverage-dedup-keys.md)) = списки ключей **исключения** из generic manifest/volume capture. Формат — **стабильный JSON**, версия схемы поля при необходимости. |
| G3 | Агрегировать **`Ready` / `Ready=False`** root по **таблице** в [`07-ready-delete-matrix.md`](07-ready-delete-matrix.md), читая **абстрактные** условия с дочерних узлов (через поле **`status.domainSubtreeSummary`** на root или через существующие `conditions` — выбрать один канал в реализации, **не два**). |
| G4 | Не создавать **child `NamespaceSnapshot`**; не создавать **Demo*** CRD. |

### Только demo controllers **могут**:

| # | Обязанность |
|---|-------------|
| D1 | Создавать/обновлять **`DemoVirtualMachineSnapshot`**, **`DemoVirtualDiskSnapshot`**, оба `*SnapshotContent`, **VolumeSnapshot**. |
| D2 | Заполнять **`domainChild*Refs`** на root NS/NSC и **`domainCoverage`** (ключи dedup). |
| D3 | Вычислять **standalone vs under-VM** для диска и **INV-T2**. |

**Контракт «общий для generic»:** только **`domainCoverage`** + **`domainSubtreeSummary`** (имена черновые) + **`domainChild*Refs`**. Любая другая связь generic ↔ demo **запрещена** без обновления этого документа.

---

## 4. OwnerRef (иерархия / GC), не dedup

| Объект | `ownerReferences` (контроллер / блокирующий GC) |
|--------|-----------------------------------------------|
| `DemoVirtualMachineSnapshot` | **Root** `NamespaceSnapshot` (UID). |
| `DemoVirtualMachineSnapshotContent` | `DemoVirtualMachineSnapshot`. |
| `DemoVirtualDiskSnapshot` (под VM) | `DemoVirtualMachineSnapshot`. |
| `DemoVirtualDiskSnapshot` (standalone) | **Root** `NamespaceSnapshot`. |
| `DemoVirtualDiskSnapshotContent` | `DemoVirtualDiskSnapshot`. |
| `VolumeSnapshot` | **Рекомендация v1:** `ownerReference` на **`DemoVirtualDiskSnapshot`** (controller=false или по политике CR); если политика кластера запрещает — **обязательные** лейблы `state-snapshotter.deckhouse.io/root-namespace-snapshot-uid` + `.../demo-virtual-disk-snapshot-name` + финализаторы (см. [`07-ready-delete-matrix.md`](07-ready-delete-matrix.md)). |

**INV-O1.** **Dedup** не выводится из наличия ownerRef: см. [`04-coverage-dedup.md`](04-coverage-dedup.md) и [`06-coverage-dedup-keys.md`](06-coverage-dedup-keys.md).
