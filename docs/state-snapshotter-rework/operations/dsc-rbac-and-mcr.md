# DSC, RBAC hook и отличие от MCR (D2)

Связующий документ: **кто что пишет в статус**, откуда берётся **RBACReady**, и почему **DomainSpecificSnapshotController** не стоит путать с **ManifestCaptureRequest**. Нормативно по статусам DSC см. ADR [`snapshot-rework/2026-01-23-unified-snapshots-registry.md`](../../../snapshot-rework/2026-01-23-unified-snapshots-registry.md) и код `internal/controllers/domainspecificsnapshot_controller.go`.

---

## DomainSpecificSnapshotController (DSC)

**Назначение:** модуль (через свой hook/оператор) создаёт кластерный объект DSC и указывает **маппинг** имён CRD: какие `*Snapshot` / `*SnapshotContent` типы модуль «предъявляет» unified-контроллеру.

**Что делает reconciler state-snapshotter**

- Разрешает имена CRD в API (`CustomResourceDefinition`), проверяет инварианты (в т.ч. cluster-scoped content).
- Выставляет **`Accepted`** (и причины `InvalidSpec`, `KindConflict` и т.д.).
- Вычисляет производный **`Ready`** по правилам ADR (например, нет конфликта, RBAC готов с точки зрения статуса).
- После успешного полного reconcile всех DSC вызывает **`unifiedruntime.Syncer.Sync`**: merge bootstrap ∪ eligible DSC → resolved → additive watches.

**Что контроллер DSC сам не выставляет**

- Условие **`RBACReady`**: его в реальном кластере выставляет **модульный hook** (или другой компонент модуля), когда RBAC для соответствующих типов согласован. Контроллер state-snapshotter **читает** это условие как вход формулы watch-eligibility (`Accepted` + `RBACReady` + поколения).

Итог: **DSC — контракт регистрации типов и статуса приёмки**; **RBAC для снимков в API — ответственность модуля**, сигнал о готовности приходит через `RBACReady`.

---

## Формула активации watch (кратко)

Для участия пары GVK в merge и попытки поднять watches нужно (см. `pkg/dscregistry`):

- `Accepted=True` и `RBACReady=True`;
- оба условия согласованы с **поколением** spec (`observedGeneration`).

**`Ready`** на DSC **не** входит в предикат «можно ли вешать watch» — это отдельный UX/агрегированный статус.

---

## Почему DSC ≠ MCR (ManifestCaptureRequest)

| Аспект | DSC | MCR (manifest line) |
|--------|-----|----------------------|
| Вопрос | **Какие** unified snapshot / snapshot content **типы** существуют в кластере для данного модуля | **Запрос на захват** набора API-объектов в манифест |
| CRD группа | `state-snapshotter.deckhouse.io` (DSC) | Свои типы manifest-трека |
| Связь с unified watches | Прямая: eligible DSC → `Sync` → `AddWatch*` | Нет прямой регистрации GVK unified-снимков |

Путаница часто возникает, потому что оба объекта «про модуль» и «про снимки», но **DSC — реестр типов для unified контроллера**, а **MCR — операция захвата**, живущая в другом контуре (см. D1 «manifest line» в [`../README.md`](../README.md)).

---

## RBAC «из DSC» в смысле эксплуатации

- **Создание** Role/RoleBinding/ClusterRole для работы доменного оператора с конкретными CRD — на стороне **модуля** (Helm/hook).
- DSC **не генерирует** RBAC; он лишь **ожидает**, что модуль выставит **`RBACReady=True`**, когда политика применена.
- Если RBAC не готов: DSC может быть `Accepted=True`, но watch по формуле не активируется — это ожидаемо до сигнала hook.

---

## Ссылки

- Runbook (метрики, stale, CRD): [`runbook-degraded-and-unified-runtime.md`](runbook-degraded-and-unified-runtime.md)
- Обзор линий продукта: [`../README.md`](../README.md)
- План задач D1–D3: [`../design/implementation-plan.md`](../design/implementation-plan.md) §2.4
