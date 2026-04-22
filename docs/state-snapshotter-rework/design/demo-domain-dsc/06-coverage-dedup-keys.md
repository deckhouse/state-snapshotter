# Единица дедупликации и **вычисляемое** coverage (v1)

**Статус:** Proposed — **зафиксировано для согласования перед кодом.**  
**Базовая модель:** [`08-universal-snapshot-tree-model.md`](08-universal-snapshot-tree-model.md) (dedup не хранится; ownerRef ≠ dedup).

**Не использовать:** persisted **`domainCoverage`**, аннотации-кэш, отдельные summary-поля в CR.

---

## 1. Два слоя dedup

| Слой | Вопрос |
|------|--------|
| **Data dedup** | Один **PVC** — не два **VolumeSnapshot** в одном root run. |
| **Resource dedup** | Один logical disk / объект не дважды в subtree и не повторно в generic root MCP, если уже покрыт **более специфичным** subtree. |

**Принцип:** **generic path** не повторно захватывает ресурс, уже покрытый более специфичным subtree (см. также [`04-coverage-dedup.md`](04-coverage-dedup.md)).

---

## 2. Ключи «тот же ресурс» (входы **функции вычисления**, не поля CR)

| Ресурс | Канонический ключ при вычислении |
|--------|----------------------------------|
| **PVC** | `metadata.uid` PVC (**`pvcUID`**). |
| **Диск в v1** | Достаточно **`pvcUID`**, если диск 1:1 с PVC. |
| **Объект в root allowlist** | `{ apiGroup, kind, namespace, name }` или **uid**. |

Инварианты **INV-D1** / **INV-D2** (два disk snapshot на один `pvcUID`; standalone vs под VM) — по-прежнему; контроль — reconcile demo + проверка API, не поле в status.

---

## 3. Кто что делает

| Роль | Обязанность |
|------|-------------|
| **Demo controllers** | Создают объекты дерева, VS, MCP; заполняют **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`**; лейблы/ownerRef для однозначной выявляемости в API. **Не** пишут coverage в CR. |
| **Generic controller** | Перед root capture **вычисляет** exclude-множества, обходя дерево по **общим refs** (§4). |

---

## 4. Алгоритм вычисления (черновик)

**Граница области (INV-S0).** Dedup и exclude считаются **только в пределах дерева текущего snapshot-run** — обход от конкретного root **`NamespaceSnapshot`** по **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`**. **Не** опираться на «все похожие snapshot-объекты в namespace» или чужие деревья без явной связи через refs этого run.

**Реализация (чувствительное место PR5).** Конкретный детерминированный критерий «объект **принадлежит этому run**» (лейблы с UID root snapshot, цепочка от refs, ограничения **ownerRef** между namespace и т.д.) должен быть **жёстко** зафиксирован в **`spec/system-spec.md`** и покрыт тестами; на уровне дизайна достаточно **INV-S0** и осознания, что ошибка здесь даст ложный dedup или пропуск exclude.

На reconcile root **`NamespaceSnapshot`** (или helper в `pkg/…` без импорта demo-типов по имени, только по **динамическим** GVK из refs):

1. Прочитать **`status.childrenSnapshotRefs`** (и при необходимости content refs) с **root** и **рекурсивно** обойти детей по тем же полям на дочерних snapshot’ах.
2. Для каждого узла — get/list: **VolumeSnapshot**, **ManifestCheckpoint** / chunks, статусы MCP, дочерние snapshot’ы из refs.
3. Построить множество **`pvcUID`**, для которых уже существует доменный путь данных (VS), принадлежащий этому root run (лейблы / ownerRef / цепочка от refs — как согласовано в реализации).
4. Построить множество **object refs**, чьи манифесты уже в **domain MCP** (не в root MCP).
5. Применить (3)–(4) как **exclude** при планировании generic root MCR / volume path.

**INV-C1.** Результат **не** сериализуется в CR; на каждом reconcile **пересчитывается** из API.

**INV-P1.** Generic **не** создаёт второй **VolumeSnapshot** для **`pvcUID`**, если вычисление (3) уже нашло доменный VS для этого root run.

**INV-C2.** Удаление chunk / поломка MCP **не** требует чистки полей coverage: следующий reconcile пересчитает факты; **`Ready`** каскадом отражает деградацию ([`07-ready-delete-matrix.md`](07-ready-delete-matrix.md)).

---

## 5. ownerRef

Не источник dedup и не источник дерева — см. **[`08` часть B](08-universal-snapshot-tree-model.md#часть-b-роль-ownerreferences)**.
