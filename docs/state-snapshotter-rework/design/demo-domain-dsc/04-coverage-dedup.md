# Dedup: data + domain resources (computed)

**Статус:** Proposed (расширено по ревью).  
**Роль этого файла:** смысл dedup и уровни; **алгоритм и ключи** — в [`06-coverage-dedup-keys.md`](06-coverage-dedup-keys.md). **«Coverage»** здесь — не сущность и не поле на CR: это **вычисляемое на reconcile** множество «уже покрыто» для exclude.

## Жёсткое разделение: ownerRef **≠** dedup

| Механизм | Назначение | Не используется для |
|----------|------------|---------------------|
| **ownerReference** | Иерархия, **GC**, deletion | Dedup «уже покрыто» |
| **Вычисляемый набор уже покрытых ресурсов** по дереву текущего snapshot-run | Какие PVC / ref объекта **исключить** из generic root capture | Замены ownerRef |
| **`Ready` (каскад)** | Готовность / деградация дерева снизу вверх | Отдельного `SubtreeReady` и хранения снимка coverage |

**Граница run:** dedup и exclude считаются **только** в пределах дерева **текущего** snapshot-run (обход от root по **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`**), а не «по всем snapshot-объектам namespace» и не глобально между несвязанными run — **INV-S0** в [`06`](06-coverage-dedup-keys.md).

Dedup **не** требует поля `status.domainCoverage`: достаточно **детерминированного обхода** графа + статусов VS/MCP/chunks (см. `06` §4).

---

## Два уровня dedup

### 1. Data dedup

Не два **VolumeSnapshot** на один **PVC** в одном root run. Проверка — через **вычисленное** множество `pvcUID` с уже существующим доменным VS (`06`).

### 2. Resource dedup

Два разных класса риска (оба закрываются **вычислением** по refs + API, детали — `06`):

1. **Внутри доменного дерева:** один и тот же доменный ресурс (например тот же логический диск / **`pvcUID`**) не должен оказаться в **двух несогласованных ветках** subtree — политика домена (**INV-T2** в [`05`](05-tree-and-graph-invariants.md)).
2. **Между domain subtree и generic root capture:** объект уже попал в **domain MCP** / subtree этого run → **generic** не должен включить его **повторно** в **root** MCP того же run.

> **Generic path must not re-capture any resource already covered by a more specific domain subtree** — проверка **на лету**, без денормализованного кэша в CR.

---

## Нерешённое на ревью

- Поведение **aggregated** PR4 при объектах только в domain MCP.
- Порядок reconcile vs гонка «generic раньше увидел VS» — закрепить тестом **INV-P1 (data / VS)** в `06`.
