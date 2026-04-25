# Универсальная модель snapshot-дерева и роль ownerReferences

**Статус:** Historical design (частично реализовано). Нормативный runtime-контракт — в `spec/system-spec.md`.
> ⚠️ This document contains historical and potentially outdated design decisions.
> Current normative behavior is defined in:
> - [`spec/system-spec.md`](../../spec/system-spec.md)
> - [`design/implementation-plan.md`](../implementation-plan.md) (current state)

**Коротко:** snapshot-система — это **обобщённое дерево heterogeneous `*Snapshot` / `*SnapshotContent`**, связь через **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`**, единый **`Ready`**, каскадная деградация снизу вверх и **вычисляемый** dedup; **`NamespaceSnapshot`** — текущий верхний узел, **не** особый тип правил.

---

## Часть A. Общая модель snapshot’ов

### A.1. Единая сущность

Любой snapshot в системе — **узел дерева**, независимо от типа:

- `NamespaceSnapshot`, `DemoVirtualMachineSnapshot`, `DemoVirtualDiskSnapshot`, любой будущий **`XxxxSnapshot`**.

Для каждого — соответствующий **`XxxxSnapshotContent`** (зафиксированный результат: данные + манифесты).

| Слой | Роль |
|------|------|
| **`XxxxSnapshot`** | Декларация / интент |
| **`XxxxSnapshotContent`** | Зафиксированный результат |

### A.2. Дерево snapshot’ов

- Узел может иметь **дочерние snapshot’ы**.
- Связь задаётся **только** через:
  - **`status.childrenSnapshotRefs`** на snapshot;
  - **`status.childrenSnapshotContentRefs`** на content.

Дерево **heterogeneous**: дети другого типа (например `NamespaceSnapshot` → `DemoVirtualMachineSnapshot` → `DemoVirtualDiskSnapshot`). Модель **не привязана** к конкретным GVK на уровне правил: generic-логика опирается на **refs + conditions**, без знания «demo-kind» по имени типа.

**Жёсткая граница (полный состав дерева run):** всё, что **не** перечислено в **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`** на пути от root этого run, **не** входит в **логическое** дерево snapshot-run, **даже** если объект есть в API. Обход, агрегация **`Ready`** и вычисление dedup **игнорируют** такие объекты как узлы дерева (см. [`05-tree-and-graph-invariants.md`](05-tree-and-graph-invariants.md) **INV-REF1**, **INV-S0** в [`06-coverage-dedup-keys.md`](06-coverage-dedup-keys.md)).

**Согласование с текущим spec/runtime:** для `childrenSnapshotRefs` используется strict ref `{ apiVersion, kind, name }`; namespace дочернего snapshot **не хранится в ref** и всегда берётся из namespace родительского `NamespaceSnapshot`. Отдельные cross-namespace refs не являются частью текущего API-контракта.

### A.3. Роли уровней

| Уровень | Пример | Ответственность |
|---------|---------|------------------|
| **Leaf** | `DemoVirtualDiskSnapshot` | Данные (VS, MCP, chunks…), **собственный** `Ready` |
| **Intermediate** | `DemoVirtualMachineSnapshot` | Агрегация детей, свои зависимости, проброс состояния вверх |
| **Root** | сейчас `NamespaceSnapshot` | Верхняя агрегация, итоговое состояние снимка |

`NamespaceSnapshot` — **один из** типов snapshot; root только потому, что **пока** нет уровня выше namespace (в будущем возможен например `ClusterSnapshot` → тогда NS станет промежуточным). **Формального** отличия правил дерева нет: те же refs, тот же `Ready`, те же правила каскада.

### A.4. `Ready` — единая модель

- Один основной **`Ready`** (condition), **без** `SubtreeReady`, `domainSummary` и т.п.

**Успех (снизу вверх):**

- Лист **`Ready=True`**, когда выполнены **его** зависимости.
- Родитель **`Ready=True`** только если: все **обязательные** дети **`Ready=True`** **и** собственные зависимости готовы.

**Деградация (снизу вверх):**

- Ломается leaf или зависимость → leaf **`Ready=False`** → родитель **`Ready=False`** → до корня.
- **`reason` / `message`**: перенос **первопричины** снизу вверх (корень показывает реальную причину). **Дизайн PR5:** `reason` **не переписывается** на промежуточных уровнях — вверх идёт **тот же код**, что у первичного узла; в `message` — уточнение пути (см. [`07-ready-delete-matrix.md`](07-ready-delete-matrix.md) §3).

**Fail-closed (агрегация `Ready`).** Если состояние **обязательного** ребёнка, заданного в **refs**, **нельзя надёжно** определить (ошибка чтения API, нет ожидаемых **conditions**, конфликт состояний), предок **обязан** **`Ready=False`** — **не** оставаться **`Ready=True`** из-за «не смогли проверить». Нормативно: **INV-R4** в [`07-ready-delete-matrix.md`](07-ready-delete-matrix.md).

**Несколько детей с `Ready=False` и разными `reason`:** на родителе выбирается один код по фиксированному E6-приоритету (`ChildSnapshotFailed` > `SubtreeManifestCapturePending` > `ChildSnapshotPending`) — см. **INV-R5** в [`07`](07-ready-delete-matrix.md) §3.

Примеры `reason`: `ManifestChunkMissing`, `ChildSnapshotMissing`, `ManifestCheckpointMissing`, `VolumeSnapshotNotReady`, …

### A.5. Что считается поломкой (триггер деградации)

Любое нарушение **наблюдаемое через API и `children*Refs`** (не «внутреннее состояние контроллера» как первичный источник истины для каскада), в т.ч.:

- удалён дочерний **Snapshot**;
- удалён дочерний **SnapshotContent**;
- удалён / сломан **ManifestCheckpoint**;
- удалена **chunk**;
- **VolumeSnapshot** не готов / исчез.

→ **`Ready=False`** на затронутом уровне и **каскад вверх**.

### A.6. Dedup

- **Не** часть модели хранения; логика **reconcile**.
- Принцип: **нельзя дважды** захватить один и тот же ресурс (один PVC → один VS; один logical disk → один snapshot-path).
- Вычисляется из **текущего API** (дети по refs, VS, MCP, chunks…); **не** в `status` / аннотациях / отдельных cache-полях.
- **`ownerReference` ≠ dedup`** (см. часть B).
- **Fail-closed:** при **невозможности надёжно** вычислить exclude/dedup контроллер **не** принимает решение об исключении/захвате ресурса «внаглую» по неполным данным — **INV-E1** в [`06-coverage-dedup-keys.md`](06-coverage-dedup-keys.md).

### A.7. Инварианты (чеклист)

1. Любой **`XxxxSnapshot`** — узел дерева.  
2. Дерево строится через **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`**.  
3. Модель **не зависит** от конкретных типов в коде generic (только refs + conditions + вычисления).  
4. **`Ready`** — единственный condition готовности для каскада успеха/деградации.  
5. Деградация **всегда** снизу вверх с сохранением причины.  
6. Dedup: **вычисляется**, не хранится.  
7. **`ownerReference`**: не для dedup; иерархия жизненного цикла / GC (часть B).  
8. Объекты **вне** **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`** на пути от root run **не** входят в логическое дерево, **даже** если существуют в API.  
9. При **неопределённости** состояния обязательного ребёнка (из **refs**) предок считается **деградировавшим**: **`Ready=False`**, а не «без изменений» (**INV-R4**, [`07`](07-ready-delete-matrix.md)).  
10. Запись в **`children*Refs`** — **parent-owned**: parent snapshot controller записывает полный список своих прямых children; child controllers не self-register (**INV-REF-M1**, [`05`](05-tree-and-graph-invariants.md) §1).  
11. Удаление элемента ref — результат recompute child set родительским controller’ом (**INV-REF-M2**, [`05`](05-tree-and-graph-invariants.md) §1).  
12. Пустые / отсутствующие **`childrenSnapshotContentRefs`** **не** разрешают generic восстанавливать content через list API без normative правила (**INV-REF-C1**, [`05`](05-tree-and-graph-invariants.md) §1).

---

## Часть B. Роль ownerReferences

### B.1. Что это в системе

`ownerReference` — механизм Kubernetes для **жизненного цикла** объектов, **не** для логики snapshot-дерева.

В контексте snapshot-дерева:

- задаёт **иерархию владения** для GC;
- участвует в **каскадном удалении**;
- помогает не оставлять мусор (VS, временные объекты).

### B.2. Главное правило

**`ownerReference` ≠ логическое дерево snapshot’ов.**

- Структура дерева — **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`**.
- **`Ready`** — состояние дерева (через conditions).
- **Dedup** — вычисление.
- **`ownerReference`** — удаление / GC.

Нельзя восстанавливать или обходить дерево **только** по `ownerRef` (может быть неполным, запрещённым политикой, отсутствующим).

### B.3. Зачем нужен

**Каскадное удаление (GC):** удалён родитель → дети с ownerRef на родителя уходят вместе с политикой GC.

**Очистка временных ресурсов:** VS и вспомогательные объекты с ownerRef на disk snapshot / content — не остаются сиротами.

### B.4. Чего ownerReference **не** делает

| ❌ | Почему |
|----|--------|
| Не описывает дерево snapshot’ов | Источник истины — **refs** |
| Не используется для dedup | Dedup только из фактов API |
| Не участвует напрямую в вычислении **`Ready`** | **`Ready`** — от детей по refs и зависимостей |

### B.5. Таблица «кто за что»

| Механизм | Назначение |
|----------|------------|
| **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`** | Логическое дерево |
| **`Ready`** (condition) | Состояние дерева, каскад |
| **Dedup (вычисляемый)** | Корректность данных |
| **`ownerReference`** | Жизненный цикл и GC |

### B.6. Рекомендуемый паттерн (**не** обязательный)

Ниже — **типовой** wiring для GC; **не** норматив «всегда так в проде»: Kubernetes и политика модуля могут запретить цепочку **ownerRef** — тогда **лейблы + финализаторы** и см. **B.7** (**best-effort**).

- **`XxxxSnapshot`** → ownerRef на родительский **`YyyySnapshot`** (если политика namespace/GC позволяет).
- **`XxxxSnapshotContent`** → ownerRef на соответствующий **`XxxxSnapshot`**.
- **Leaf** (например `VolumeSnapshot`) → ownerRef на **`DemoVirtualDiskSnapshot`** (или аналог); если нельзя — **лейблы + финализаторы**.

### B.7. Ограничения Kubernetes

`ownerRef` для namespaced ресурсов **не** может ссылаться на объект в **другом** namespace; admission может запретить. Поэтому: **best-effort**, не единственный источник истины для структуры.

### B.8. Взаимодействие с деградацией

Удаление дочернего snapshot через GC → родитель **теряет** обязательного ребёнка в API → при следующем reconcile родитель → **`Ready=False`**, **`reason`**, например **`ChildSnapshotMissing`**. Это частный случай **структурного** нарушения дерева по **refs** (**INV-R2** в [`07-ready-delete-matrix.md`](07-ready-delete-matrix.md)).  
**Инициатор** удаления — GC/ownerRef; **логика деградации** — reconcile + **`Ready`**.

---

**Одна фраза:** `ownerReference` управляет жизненным циклом и удалением, **не** определяет структуру дерева, **не** участвует в dedup и **не** заменяет **`Ready`** — дерево живёт в **refs**, состояние в **`Ready`**, dedup в **вычислении**.
