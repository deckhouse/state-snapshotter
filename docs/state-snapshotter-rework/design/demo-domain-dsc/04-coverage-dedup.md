# Coverage / dedup: data + domain resources

**Статус:** Proposed (расширено по ревью).  
**Ключи и вычисление:** [`06-coverage-dedup-keys.md`](06-coverage-dedup-keys.md) — **coverage не хранится** на CR, только **вычисляется** при reconcile.

## Жёсткое разделение: ownerRef **≠** dedup

| Механизм | Назначение | Не используется для |
|----------|------------|---------------------|
| **ownerReference** | Иерархия, **GC**, deletion | Dedup «уже покрыто» |
| **Вычисляемое coverage** (функция от API) | Какие PVC / ref объекта **исключить** из generic root capture | Замены ownerRef |
| **`Ready` (каскад)** | Готовность / деградация дерева снизу вверх | Отдельного `SubtreeReady` и хранения снимка coverage |

Dedup **не** требует поля `status.domainCoverage`: достаточно **детерминированного обхода** графа + статусов VS/MCP/chunks (см. `06` §4).

---

## Два уровня dedup

### 1. Data dedup

Не два **VolumeSnapshot** на один **PVC** в одном root run. Проверка — через **вычисленное** множество `pvcUID` с уже существующим доменным VS (`06`).

### 2. Resource dedup

Не дублировать доменный объект в двух ветках и не тянуть его второй раз в root MCP — через вычисление refs объектов, уже представленных в domain MCP (`06`).

> **Generic path must not re-capture any resource already covered by a more specific domain subtree** — проверка **на лету**, без денормализованного кэша в CR.

---

## Synthetic tree

Не основной путь; после demo-тестов — миграция/удаление scaffold отдельным PR.

## Нерешённое на ревью

- Поведение **aggregated** PR4 при объектах только в domain MCP.
- Порядок reconcile vs гонка «generic раньше увидел VS» — закрепить тестом **INV-P1** в `06`.
- Детализация **reason**/`message` при каскаде (единый **`Ready`** — [`07`](07-ready-delete-matrix.md)).
