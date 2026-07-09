# Decision: Snapshot API scope (cluster-scoped vs namespaced)

## Status

**Resolved** — для текущей поставки (MVP и код в репозитории) выбран **namespaced** root; детали ниже в **Chosen option**.

Связанный design: [`../snapshot-controller.md`](../snapshot-controller.md) (§12, §13).

## Context

Выбор влияет на:

- модель **RBAC** и видимость list/watch;
- **admission** / SubjectAccessReview (особенно если root cluster-scoped, а цель — конкретный namespace);
- UX и **restore access control**;
- форму **API** (`metadata.namespace` у root или его отсутствие).

## Options (кратко)

| Вариант | Плюсы | Минусы / cost |
|---------|--------|----------------|
| **Cluster-scoped** root | Root логически «снимок namespace как ресурс кластера», отделён от содержимого целевого namespace | Сложнее RBAC, admission, ограничение доступа по `spec.source.namespaceName` |
| **Namespaced** root | Проще права и UX для владельцев namespace | Семантика «объект в том же namespace, который снимаем» (или явное поле target) — нужно зафиксировать в CRD |

**Примечание (не решение):** для MVP, если нет жёсткой продуктовой причины держать root cluster-scoped, стоит внимательно рассматривать **namespaced** — иначе почти неизбежен отдельный хвост admission / SAR / RBAC.

## Chosen option

**Namespaced** — `Snapshot` живёт в том же namespace, который снимается на текущем этапе (без отдельного `spec.source` для смены цели). Это снижает сложность RBAC, admission и SAR относительно cluster-scoped root и достаточно для N0–N1 и скелета N2.

## Consequences

После заполнения **Chosen option** обновить:

- [`../snapshot-controller.md`](../snapshot-controller.md) §4.1, §12, §13.1 (пункт перестаёт блокировать; при желании оставить ссылку «решено в snapshot-scope.md»);
- CRD OpenAPI, RBAC, тесты и будущую выдержку в `spec/system-spec.md`.

## Gate

**Критерий допуска N1 (runtime skeleton):** **Chosen option** ≠ TBD — выполнено.

**N1** (см. [`../snapshot-controller.md`](../snapshot-controller.md) §16) не начинать, пока критерий не выполнен. Эквивалент в другом нормативном документе допустим **только** со ссылкой отсюда.

После заполнения Chosen option поле **Status** в этом файле можно обновить (например на Resolved) для навигации, но это **не** меняет критерий: истина — содержимое **Chosen option**.
