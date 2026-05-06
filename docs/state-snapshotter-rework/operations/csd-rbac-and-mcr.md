# CSD, RBAC hook и отличие от MCR (D2)

Связующий документ: **кто что пишет в статус**, откуда берётся **RBACReady**, и почему **CustomSnapshotDefinition** не стоит путать с **ManifestCaptureRequest**. Нормативно по статусам CSD см. ADR [`snapshot-rework/2026-01-23-unified-snapshots-registry.md`](../../../snapshot-rework/2026-01-23-unified-snapshots-registry.md) и код `internal/controllers/csd/controller.go`.

---

## CustomSnapshotDefinition (CSD)

**Назначение:** модуль (через свой hook/оператор) создаёт кластерный объект CSD и указывает **маппинг** имён CRD: какой source resource обслуживает какой `*Snapshot` тип. В v0 CSD mapping — это `resourceCRDName -> snapshotCRDName`; content side всегда общий cluster-scoped `storage.deckhouse.io/SnapshotContent`.

**Что делает reconciler state-snapshotter**

- Разрешает имена CRD в API (`CustomResourceDefinition`), проверяет инварианты (в т.ч. cluster-scoped content).
- Выставляет **`Accepted`** (и причины `InvalidSpec`, `KindConflict` и т.д.).
- Вычисляет производный **`Ready`** по правилам ADR (например, нет конфликта, RBAC готов с точки зрения статуса).
- После успешного полного reconcile всех CSD вызывает **`unifiedruntime.Syncer.Sync`**: merge bootstrap ∪ eligible CSD → resolved → additive watches.

**Что контроллер CSD сам не выставляет**

- Условие **`RBACReady`**: его в реальном кластере выставляет **модульный hook / внешний Deckhouse RBAC controller** (или другой компонент модуля), когда RBAC для соответствующих типов уже согласован и применён. Контроллер state-snapshotter **читает** это условие как вход формулы watch-eligibility (`Accepted` + `RBACReady` + поколения).

Итог: **CSD — контракт регистрации типов и статуса приёмки**; **RBAC для снимков в API — ответственность модуля / внешнего RBAC controller**, сигнал о готовности приходит через `RBACReady`.

**Content model:** `spec.snapshotResourceMapping[]` contains only `resourceCRDName`, `snapshotCRDName`, and optional `priority`. The state-snapshotter common layer owns `SnapshotContent` lifecycle, ObjectKeeper/Retain, MCP/data refs, and content-tree aggregation.

---

## Формула активации watch (кратко)

Для участия пары GVK в merge и попытки поднять watches нужно (см. `pkg/csdregistry`):

- `Accepted=True` и `RBACReady=True`;
- оба условия согласованы с **поколением** spec (`observedGeneration`).

**`Ready`** на CSD **не** входит в предикат «можно ли вешать watch» — это отдельный UX/агрегированный статус.

---

## Почему CSD ≠ MCR (ManifestCaptureRequest)

| Аспект | CSD | MCR (manifest line) |
|--------|-----|----------------------|
| Вопрос | **Какие** unified snapshot / snapshot content **типы** существуют в кластере для данного модуля | **Запрос на захват** набора API-объектов в манифест |
| CRD группа | `state-snapshotter.deckhouse.io` (CSD) | Свои типы manifest-трека |
| Связь с unified watches | Прямая: eligible CSD → `Sync` → `AddWatch*` | Нет прямой регистрации GVK unified-снимков |

Путаница часто возникает, потому что оба объекта «про модуль» и «про снимки», но **CSD — реестр типов для unified контроллера**, а **MCR — операция захвата**, живущая в другом контуре (см. D1 «manifest line» в [`../README.md`](../README.md)).

MCR остаётся строго namespaced capture request: все targets должны быть namespaced resources в namespace MCR. Cluster-scoped resources, включая Kubernetes `Namespace`, не захватываются через MCR. Empty MCR допустим и создаёт empty MCP (0 objects), что используется для пустого root `Snapshot` own scope.

---

## RBAC «из CSD» в смысле эксплуатации

- Static controller RBAC covers only core state-snapshotter resources, MCR/MCP/chunks, `Snapshot` / `SnapshotContent`, ObjectKeeper, and the standard root `Snapshot` manifest allowlist.
- Source of truth for static production controller RBAC is the Helm template `templates/controller/rbac-for-us.yaml`, not `+kubebuilder:rbac` markers in controller code.
- Controller code under `images/state-snapshotter-controller/internal/controllers/` MUST NOT use `+kubebuilder:rbac` markers; update the Helm template for static core permissions instead.
- Domain/demo resources are not part of the static production controller RBAC contract.
- Demo controllers that live in the same binary are examples/dev controllers. Their permissions must be granted by the CSD/module RBAC path, not by generic static ClusterRole rules.
- Domain controllers MUST NOT use kubebuilder RBAC markers as the source of production RBAC. `+kubebuilder:rbac` markers in example/domain controllers are forbidden to avoid accidentally leaking domain permissions into static controller RBAC.
- **Создание** Role/RoleBinding/ClusterRole для работы доменного оператора с конкретными CRD — на стороне **модуля / внешнего Deckhouse RBAC controller/hook**.
- CSD **не генерирует** RBAC в текущей реализации; он лишь **ожидает**, что модуль выставит **`RBACReady=True`**, когда политика применена и effective permissions уже существуют.
- В текущем спринте real-cluster smoke/e2e сами создают test-only ClusterRole/ClusterRoleBinding для demo/domain resources, эмулируя внешний RBAC controller, и только после проверки permissions выставляют `RBACReady=True`.
- Integration/envtest не проверяет реальный Kubernetes RBAC enforcement; проверки `RBACReady` там симулируют статусный handshake, а не выдачу прав API server.
- Если RBAC не готов: CSD может быть `Accepted=True`, но watch по формуле не активируется — это ожидаемо до сигнала hook.
- Demo materialization captures existing domain objects directly (`DemoVirtualDisk`, `DemoVirtualMachine`), not placeholder ConfigMap payloads.
- Removing synthetic marker materialization does **not** solve dynamic domain RBAC. If domain controllers lack `get/list/watch` for resources declared by CSD mappings, that is handled by the separate CSD RBAC track; do not add broad static self-grants as part of materialization cleanup.

---

## Ссылки

- Runbook (метрики, stale, CRD): [`runbook-degraded-and-unified-runtime.md`](runbook-degraded-and-unified-runtime.md)
- Обзор линий продукта: [`../README.md`](../README.md)
- План задач D1–D3: [`../design/implementation-plan.md`](../design/implementation-plan.md) §2.4
