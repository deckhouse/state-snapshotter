# Decision: NamespaceSnapshot API scope (cluster-scoped vs namespaced)

## Status

**Pending** — решение в проработке (удобная человекочитаемая метка; **не** отдельный формальный gate).

Связанный design: [`../namespace-snapshot-controller.md`](../namespace-snapshot-controller.md) (§12, §13).

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

**TBD** — после продуктового решения: одна строка (**cluster-scoped** или **namespaced**) + при необходимости ссылка на ADR.

## Consequences

После заполнения **Chosen option** обновить:

- [`../namespace-snapshot-controller.md`](../namespace-snapshot-controller.md) §4.1, §12, §13.1 (пункт перестаёт блокировать; при желании оставить ссылку «решено в namespace-snapshot-scope.md»);
- CRD OpenAPI, RBAC, тесты и будущую выдержку в `spec/system-spec.md`.

## Gate

**Единственный формальный критерий допуска Phase 2:** **Chosen option** ≠ TBD (записан выбранный вариант или ссылка на принятое продуктовое решение). Пока **Chosen option = TBD**, нельзя финализировать scope CRD, RBAC и admission.

**Phase 2** (см. [`../namespace-snapshot-controller.md`](../namespace-snapshot-controller.md) §16) не начинать, пока критерий не выполнен. Эквивалент в другом нормативном документе допустим **только** со ссылкой отсюда.

После заполнения Chosen option поле **Status** в этом файле можно обновить (например на Resolved) для навигации, но это **не** меняет критерий: истина — содержимое **Chosen option**.
