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

## 1. Формирование дерева (через контроллеры, не через allowlist)

**Принцип:** состав дочерних узлов **не** задаётся статическим «разрешённым списком kinds» в generic и **не** сводится к `if kind == VM → создать Disk`. Дерево — **наблюдаемое**: какие объекты есть в API, как они связаны через **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`**, кто их создал — решают **контроллеры**, зарегистрированные через **DSC** (и политика их `spec`), плюс generic для root namespace capture.

**INV-T1.** Под **root `NamespaceSnapshot`** **не** создаётся дочерний **`NamespaceSnapshot`**.

**INV-T2 (доменная политика demo v1, не generic).** В рамках одного snapshot-run **доменные** контроллеры **не** должны создавать более **одного** активного **`DemoVirtualDiskSnapshot`** на один **`pvcUID`**. Диск **не** может одновременно быть **standalone** под root и частью subtree **VM** (один родительский контейнер: либо **`DemoVirtualMachineSnapshot`**, либо root **`NamespaceSnapshot`** — `spec.parentRef` oneOf или эквивалент в реализации). Соблюдение **INV-T2** — ответственность **доменной** логики; **generic** её **не** реализует и **не** интерпретирует **`pvcUID`** / продуктовые правила диска.

### Как появляются дети

| Шаг | Кто | Результат |
|-----|-----|-------------|
| 1 | Пользователь / CI (и при необходимости generic) | Создаётся root **`NamespaceSnapshot`** (и bind root **`NamespaceSnapshotContent`**) |
| 2 | **Доменные** контроллеры (через **DSC**) | По доменной логике и **`spec`** создают дочерние snapshot’ы / content (**VM**, **disk**, при необходимости **VolumeSnapshot** и т.д.) |
| 3 | Те же (или согласованные) контроллеры | Заполняют **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`** на родителях |
| 4 | **Generic** `NamespaceSnapshot` reconciler | Обходит дерево **по refs** (dedup, **`Ready`**, aggregated — без жёсткого списка «разрешённых детей») |

**Источник истины логического дерева snapshot-run.** Для целей **dedup**, агрегации **`Ready`** и **aggregated** обхода **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`** предков этого run задают **логическое** дерево: объект, **не** включённый в эти refs, **не** считается частью дерева run (даже если он уже существует в API).

**Согласованность refs ↔ API.** Допустимо кратковременное расхождение (объект в API появился, ссылка в **`children*Refs`** ещё не записана). **Контроллеры**, которые создают детей и пишут refs (**доменные** и др.), **обязаны** минимизировать окно расхождения: создание дочернего snapshot/content и включение его в **`children*Refs`** родителя — **одна логическая операция** (единый reconcile / согласованная цепочка patch) **или** состояние **идемпотентно** приводится к согласованному виду на следующем reconcile, **без** длительного «объект живёт, в дереве refs его нет». Generic **идемпотентно** опирается только на то, что уже отражено в refs; **повторный** reconcile при **частичном** графе — нормальная ситуация.

**Ограничения v1 (инварианты продукта, не compile-time allowlist):**

- VM snapshot может порождать disk snapshot’ы и свой content **согласно своему `spec`** и контроллеру;
- disk snapshot соответствует **одному** PVC (**1:1** в v1);
- **INV-T2** — доменная политика demo (см. выше), другие домены в будущем могут задавать иные правила.

**Generic и «знание типов»:** reconciler **`NamespaceSnapshot`** **не** ведёт allowlist доменных kinds и **не** ветвится по бизнес-смыслу типа. Он опирается на **универсальный контракт**: **(а)** дочерний объект **существует** в API по ссылке из элемента refs (**GVK + namespace + name** или эквивалент); **(б)** на узле действуют **стандартные** **`conditions`** (в т.ч. единый **`Ready`**) и общие правила их интерпретации ([`07`](07-ready-delete-matrix.md), [`08`](08-universal-snapshot-tree-model.md)). Так generic остаётся расширяемым при новых DSC-типах **без** правок «если DemoDisk».

---

## 2. Отражение в `childrenSnapshotRefs` / `childrenSnapshotContentRefs`

Эти поля — **универсальная** модель дерева для **любого** `XxxxSnapshot` / `XxxxSnapshotContent`, не специфичны для namespace и не требуют отдельных demo-полей.

| Правило | Содержание |
|---------|------------|
| **R1** | На **`NamespaceSnapshot.status.childrenSnapshotRefs`** — **прямые** дети **того run**, которые контроллеры **фактически** создали и записали в refs (**любые** поддерживаемые DSC типы, а не заранее зафиксированная таблица «родитель → kinds»). |
| **R2** | Форма графа **наблюдаема** по refs: например, диски под VM snapshot перечисляются у **`DemoVirtualMachineSnapshot.status.childrenSnapshotRefs`**, а не дублируются как прямые дети root, **если** так задано доменной политикой записи refs. Обход для aggregated / dedup / **`Ready`** — от корня по **единой** модели refs. |
| **R3** | **`childrenSnapshotContentRefs`** на соответствующем **`NamespaceSnapshotContent`** (root) указывают на content дочерних snapshot’ов **прямых** детей root (в т.ч. `DemoVirtualMachineSnapshotContent`, `DemoVirtualDiskSnapshotContent` для standalone диска). |
| **R4** | Корневой **`NamespaceSnapshotContent`** несёт **`manifestCheckpointName`** для **root** namespace MCP; MCP доменных leaf — на **`DemoVirtualDiskSnapshotContent`** (и при необходимости на VM snapshot content). |

**Целевая форма элементов refs** (после расширения PR1 → PR5 в spec): достаточно идентифицировать ребёнка в API (**`apiGroup`, `kind`, `namespace`, `name`** или эквивалент); до переноса в CRD дизайн не меняет код.

---

## 3. Граница generic vs domain

### Generic controller (например `NamespaceSnapshot`)

| # | Обязанность |
|---|-------------|
| G1 | **Идемпотентно** работает через **общую модель дерева**: читает **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`**, резолвит детей по элементу refs, читает их **`conditions`** (прежде всего **`Ready`**) по **универсальному** контракту — **без** доменных kind-веток и **без** продуктовых правил вроде **INV-T2**. |
| G2 | Перед root manifest/volume capture — **вычисляет** dedup из **живого API** по обходу дерева из §2 + VS/MCP/chunks ([`06-coverage-dedup-keys.md`](06-coverage-dedup-keys.md) §4). **Не** хранить и **не** читать coverage в CR. |
| G3 | Агрегирует **`Ready`** на root **только** из **`Ready`** детей (каскад §1 в [`07-ready-delete-matrix.md`](07-ready-delete-matrix.md)) и собственных зависимостей root MCP. **Без** отдельных summary-полей и **без** `SubtreeReady`. |
| G4 | Не создаёт child **`NamespaceSnapshot`**; не создаёт **Demo*** CRD. |

### Только demo controllers

| # | Обязанность |
|---|-------------|
| D1 | Создают demo snapshot/content, VS, MCP; заполняют **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`** на своих и родительских узлах по правилам §2; соблюдают **согласованность refs ↔ API** (§1, абзац под таблицей «Как появляются дети»). |
| D2 | Обеспечивают, чтобы по API было видно VS/MCP (**лейблы** и при необходимости **ownerRef** — только lifecycle/видимость по [`08`](08-universal-snapshot-tree-model.md) часть B), для детерминированного **вычисления** dedup по дереву refs (**не** выводить dedup из ownerRef). |
| D3 | Соблюдают **INV-T2** (доменная политика demo) и согласованную запись refs (без противоречий графу в API). |

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

Детали и ограничения — **[`08-universal-snapshot-tree-model.md`](08-universal-snapshot-tree-model.md) часть B**. Ниже — **типовые** связи для demo v1 (**пример** wiring, а не exhaustive allowlist всех будущих `XxxxSnapshot`; новые kinds следуют тому же паттерну: DSC → контроллер → refs + ownerRef по [`08`](08-universal-snapshot-tree-model.md)).

| Объект | ownerRef → |
|--------|------------|
| `DemoVirtualMachineSnapshot` | root `NamespaceSnapshot` |
| `DemoVirtualMachineSnapshotContent` | `DemoVirtualMachineSnapshot` |
| `DemoVirtualDiskSnapshot` (под VM) | `DemoVirtualMachineSnapshot` |
| `DemoVirtualDiskSnapshot` (standalone) | root `NamespaceSnapshot` |
| `DemoVirtualDiskSnapshotContent` | `DemoVirtualDiskSnapshot` |
| `VolumeSnapshot` | предпочтительно `DemoVirtualDiskSnapshot`; иначе лейблы + финализаторы ([`08`](08-universal-snapshot-tree-model.md) B.6–B.7). |

**INV-O1.** Dedup **не** выводится из ownerRef — см. [`06-coverage-dedup-keys.md`](06-coverage-dedup-keys.md).
