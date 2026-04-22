# Дерево snapshot kinds и инварианты demo v1

**Статус:** Proposed — **зафиксировано для согласования перед кодом.**  
**Базовая модель дерева, `Ready`, dedup, ownerRef:** [`08-universal-snapshot-tree-model.md`](08-universal-snapshot-tree-model.md).

**Не вводить** отдельных полей `domainChild*Refs`, `domainCoverage`, `domainSubtreeSummary` и отдельного condition `SubtreeReady` — используются **общие** **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`** и единый **`Ready`**.

**Лоб:** **`childrenSnapshotRefs`** и **`childrenSnapshotContentRefs`** — это **универсальная** модель дерева для **любого** `XxxxSnapshot` / `XxxxSnapshotContent` в системе, **не** namespace-specific и **не** demo-only механизм; `NamespaceSnapshot` — один из типов узла, использующий те же поля.

---

## 0. Минимальный API итерации v1 (зафиксировано)

| Решение | Значение |
|---------|----------|
| **Inventory CRD** (`DemoVirtualMachine`, `DemoVirtualDisk`) | **Не входят в v1.** Состав VM — в **`DemoVirtualMachineSnapshot.spec`** (+ `pvcRef` на диски). |
| **`DemoVirtualMachineSnapshotContent`** / **`DemoVirtualDiskSnapshotContent`** | **Да, в v1** (DSC-пары). |
| **Дерево** | Связи только через **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`** на соответствующих **`XxxxSnapshot` / `XxxxSnapshotContent`** (см. §2 и [`08`](08-universal-snapshot-tree-model.md) A.2). |

---

## 1. Кто чей ребёнок (логическая таблица kinds)

**INV-T1.** Под **root `NamespaceSnapshot`** **не** создаётся дочерний **`NamespaceSnapshot`** (в этом треке).

| Родитель | Допустимые дети (kinds) | Примечание |
|----------|-------------------------|------------|
| **`NamespaceSnapshot`** (root) | `DemoVirtualMachineSnapshot` | 0..N (v1: достаточно 0..1). |
| **`NamespaceSnapshot`** (root) | `DemoVirtualDiskSnapshot` | Только **standalone** диск (**INV-T2**). |
| **`DemoVirtualMachineSnapshot`** | `DemoVirtualDiskSnapshot`, `DemoVirtualMachineSnapshotContent` | Диски по spec VM; content **1:1**. |
| **`DemoVirtualDiskSnapshot`** | `DemoVirtualDiskSnapshotContent`, `VolumeSnapshot` | Content **1:1**; один VS на PVC в v1. |
| **`VolumeSnapshot`** | — | Leaf данных. |

**INV-T2.** У **`DemoVirtualDiskSnapshot`** ровно один родательский контейнер: либо **`DemoVirtualMachineSnapshot`**, либо **root `NamespaceSnapshot`** (`spec.parentRef` oneOf). Один **`pvcUID`** — не два активных disk snapshot в одном root run.

---

## 2. Отражение в `childrenSnapshotRefs` / `childrenSnapshotContentRefs`

Эти поля — **универсальная** модель дерева для **любого** `XxxxSnapshot` / `XxxxSnapshotContent`, не специфичны для namespace и не требуют отдельных demo-полей.

| Правило | Содержание |
|---------|------------|
| **R1** | На **`NamespaceSnapshot.status.childrenSnapshotRefs`** перечисляются **прямые** дочерние snapshot’ы **любых** поддерживаемых типов (в demo v1 — VM snapshot и **только standalone** disk snapshot). |
| **R2** | Диски под VM **не** дублируются в refs root NS: они перечислены в **`DemoVirtualMachineSnapshot.status.childrenSnapshotRefs`**. Обход дерева для aggregated / dedup / агрегации **`Ready`** — от корня по **единой** модели refs. |
| **R3** | **`childrenSnapshotContentRefs`** на соответствующем **`NamespaceSnapshotContent`** (root) указывают на content дочерних snapshot’ов **прямых** детей root (в т.ч. `DemoVirtualMachineSnapshotContent`, `DemoVirtualDiskSnapshotContent` для standalone диска). |
| **R4** | Корневой **`NamespaceSnapshotContent`** несёт **`manifestCheckpointName`** для **root** namespace MCP; MCP доменных leaf — на **`DemoVirtualDiskSnapshotContent`** (и при необходимости на VM snapshot content). |

**Целевая форма элементов refs** (после расширения PR1 → PR5 в spec): достаточно идентифицировать ребёнка в API (**`apiGroup`, `kind`, `namespace`, `name`** или эквивалент); до переноса в CRD дизайн не меняет код.

---

## 3. Граница generic vs domain

### Generic controller (например `NamespaceSnapshot`)

| # | Обязанность |
|---|-------------|
| G1 | Работает через **общую модель дерева**: читает **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`**, обходит детей **без** знания конкретных demo GVK «по имени» (достаточно типа+ссылки из refs и общих правил conditions). |
| G2 | Перед root manifest/volume capture — **вычисляет** dedup из **живого API** по обходу дерева из §2 + VS/MCP/chunks ([`06-coverage-dedup-keys.md`](06-coverage-dedup-keys.md) §4). **Не** хранить и **не** читать coverage в CR. |
| G3 | Агрегирует **`Ready`** на root **только** из **`Ready`** детей (каскад §1 в [`07-ready-delete-matrix.md`](07-ready-delete-matrix.md)) и собственных зависимостей root MCP. **Без** отдельных summary-полей и **без** `SubtreeReady`. |
| G4 | Не создаёт child **`NamespaceSnapshot`**; не создаёт **Demo*** CRD. |

### Только demo controllers

| # | Обязанность |
|---|-------------|
| D1 | Создают demo snapshot/content, VS, MCP; заполняют **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`** на своих и родительских узлах по правилам §2. |
| D2 | Обеспечивают, чтобы по API было видно VS/MCP (**лейблы** и при необходимости **ownerRef** — только lifecycle/видимость по [`08`](08-universal-snapshot-tree-model.md) часть B), для детерминированного **вычисления** dedup по дереву refs (**не** выводить dedup из ownerRef). |
| D3 | Соблюдают **INV-T2** (standalone vs под VM). |

**Контракт:** дерево — **только** общие refs; dedup — **вычисление**; готовность — **единый `Ready`**; **без** domain-специфичных полей в CR.

**Три оси (не смешивать):**

| Ось | Источник истины |
|-----|-----------------|
| Логическое дерево | **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`** |
| Lifecycle / GC / delete cascade | **`ownerReference`** (и финализаторы), см. §4 и [`08` часть B](08-universal-snapshot-tree-model.md) |
| Готовность / деградация | Condition **`Ready`** по детям из refs + зависимостям узла, **не** по ownerRef |

Обход **refs** нужен и для dedup/aggregated, и как вход для агрегации **`Ready`**; **ownerRef** для этого **не** подменяет refs и **не** определяет dedup (**INV-O1**).

---

## 4. ownerRef по kind (кратко)

Детали и ограничения — **[`08-universal-snapshot-tree-model.md`](08-universal-snapshot-tree-model.md) часть B**. Краткая таблица для demo v1:

| Объект | ownerRef → |
|--------|------------|
| `DemoVirtualMachineSnapshot` | root `NamespaceSnapshot` |
| `DemoVirtualMachineSnapshotContent` | `DemoVirtualMachineSnapshot` |
| `DemoVirtualDiskSnapshot` (под VM) | `DemoVirtualMachineSnapshot` |
| `DemoVirtualDiskSnapshot` (standalone) | root `NamespaceSnapshot` |
| `DemoVirtualDiskSnapshotContent` | `DemoVirtualDiskSnapshot` |
| `VolumeSnapshot` | предпочтительно `DemoVirtualDiskSnapshot`; иначе лейблы + финализаторы ([`08`](08-universal-snapshot-tree-model.md) B.6–B.7). |

**INV-O1.** Dedup **не** выводится из ownerRef — см. [`06-coverage-dedup-keys.md`](06-coverage-dedup-keys.md).
