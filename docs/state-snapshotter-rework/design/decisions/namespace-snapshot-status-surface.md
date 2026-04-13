# Decision: NamespaceSnapshot status — conditions only (no `status.phase`)

## Status

Accepted for **NamespaceSnapshot** API in this module. Уточнение набора **имён** condition types — по мере стабилизации (`pkg/snapshot`, design [`namespace-snapshot-controller.md`](../namespace-snapshot-controller.md)).

## Context

Некоторые Kubernetes-ресурсы дублируют состояние в **`status.phase`** и в **`status.conditions`**, что даёт расхождения и лишний источник правды. Для NamespaceSnapshot нужен **один** поверхностный контракт для операторов и автоматизации.

## Decision

В **`NamespaceSnapshot.status` не использовать поле `phase`**. Источник истины для жизненного цикла и ошибок — **`conditions`** плюс поля фактов: **`status.boundSnapshotContentName`** — единое root-level поле привязки для snapshot-линии (имя cluster-scoped content; конкретный content kind задаётся парой GVK / контроллером, не именем JSON-поля), плюс `observedGeneration`, при необходимости временные метки. Агрегаты вроде «готов / не готов» выводятся из **Ready**, **Bound** и прочих согласованных типов.

## Consequences

- CRD/OpenAPI и контроллер не пишут и не ожидают `status.phase`.
- CLI/UI и алерты ориентируются на `kubectl get … -o jsonpath='{.status.conditions[?(@.type=="Ready")]…}'` (или эквивалент).
- **Не путать** с маркерами **поставки** в плане (N0, N1, N2) и с историческими названиями **R2 phase 1 / 2b** у DSC — это орг. этапы, не поле API.

## Related

- [`namespace-snapshot-controller.md`](../namespace-snapshot-controller.md) §6
- [`namespace-snapshot-content-decision.md`](namespace-snapshot-content-decision.md) (вид content)
