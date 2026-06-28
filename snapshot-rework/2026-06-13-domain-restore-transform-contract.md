# ADR: Direction for domain restore-transform contract

- **Дата:** 2026-06-13
- **Статус:** Superseded (commit `core-remove-demo`, 2026-06) — реализован out-of-process вариант: core делегирует domain-поддерево доменному **aggregated apiserver** (`manifests-with-data-restoration`) через kube-apiserver aggregation layer (интерфейс `restore.DomainSubtreeRestorer`, клиент `internal/api/domain_restore_client.go`), а доменная мутация живёт в `internal/domainapi` отдельного бинаря. PoC HTTP-transport (`RESTORE_TRANSFORM_ENDPOINT`, `demo.RestoreTransformHandler`, `restore.RESTTransformer`, `pkg/restoretransform`) удалён. Документ оставлен как историческая фиксация направления.
- **Связано с:** `snapshot-rework/2026-06-10-restore-manifests-compiler.md` (restore compiler), `docs/state-snapshotter-rework/spec/system-spec.md`.

## Контекст

Restore compiler (`manifests-with-data-restoration`) компилирует дерево снапшотов в apply-ready манифесты снизу вверх. Доменная часть (например `DemoVirtualDisk` → `spec.dataSource` на свой снапшот; covered-PVC) сейчас живёт **в том же бинаре** как in-process реализация Go-интерфейса `restore.DomainRestoreTransformer` (`internal/controllers/demo/restore_transform.go`).

Это нормально для текущего этапа, но стратегически доменная restore-логика — это **будущая публичная граница между state-snapshotter core и доменными модулями**:

1. Это **публичный контракт** между core и доменами, а не внутренняя деталь.
2. **Доменные модули могут жить вне core** (отдельные модули/контроллеры/команды).
3. Поэтому контракт должен быть **независим от Go-интерфейса** (transport-independent), а не «вот наш Go-тип, импортируйте его».
4. Сейчас **разрабатывается v0 этого контракта** — in-process реализация. Это **самостоятельный вариант**, а не временная заглушка: **никаких миграций не предполагается**. v0 — полноценная реализация контракта, просто без внешнего транспорта.
5. Точная форма endpoint / request / response — **отдельный ADR/design doc позже**.

Контракт заведомо будет неидеален. Цель сейчас — зафиксировать **форму** (что меняется аддитивно, а что является несущей конструкцией), не углубляясь в поля.

## Решение (направление, high-level форма)

Будущий контракт доменной restore-трансформации:

- **AdmissionReview-подобный** request/response.
- **Versioned envelope** — эволюция аддитивная, ломающие изменения только через новую версию.
- **Per-node вызов** — один вызов на узел дерева (совпадает с post-order).
- **Generic владеет набором объектов** — sanitize, namespace rewrite, dedup, ordering и само множество объектов остаются за core.
- **Domain возвращает mutations + suppress intent** — домен правит свои объекты и объявляет, что подавить; он **не** возвращает полный набор манифестов.
- **Children — read-only контекст** — результаты детей передаются для справки, домен их не мутирует.
- **No additions в v1alpha1** — трансформация **не создаёт** новые объекты.
- **Fail-whole on contract violation** — любое нарушение контракта (или недоступность домена) валит весь restore, без частичной выдачи.

Суть направления одной фразой: **generic — главный; domain не создаёт объекты, а возвращает patch/suppress; контракт versioned и transport-independent.**

## Design principles (нормативно)

Два принципа фиксируются как направление (без детальной схемы):

### P1. Domain-owned field allowlist (а не denylist запрещённых полей)

Доменный модуль **обязан объявить, какие поля он вправе менять** для каждого обслуживаемого GVK. Generic guard отклоняет **любую** мутацию вне этого allowlist. Allowlist предпочтительнее denylist запрещённых полей: denylist приходится вечно догонять под новые runtime-поля, allowlist конечен, явен и самодокументируем.

Пример (иллюстративно, не финальная схема):

```
DemoVirtualDisk:
  allowed restore mutations:
    - spec.dataSource
```

Тогда домен не сможет — даже прислав patch — изменить `metadata.name`, `metadata.namespace`, `spec.storageClassName`, `spec.resources`, `status` и т.п.: всё вне allowlist отклоняется. Identity (GVK/name/namespace) и scope автоматически неизменны, т.к. вне allowlist.

Нормативно:
- prefer allowlist over denylist;
- domain declares mutable field paths per handled GVK;
- generic OutputGuard rejects mutations outside the allowlist.

### P2. Transport-independent OutputGuard

Restore compiler **обязан** валидировать результат доменной трансформации через generic `OutputGuard`. Один и тот же guard защищает **и** in-process v0-реализацию, **и** возможные будущие remote/API-трансформеры. Настоящая граница — это контракт + guard, а транспорт — сменная деталь; v0 служит conformance-эталоном.

Нормативно:
- OutputGuard is transport-independent;
- the same guard protects the in-process v0 and any future remote implementation;
- v0 develops guard + allowlist first, **not** HTTP/API transport.

## Почему так (несущая конструкция)

- **Generic-владение набором** не даёт restore compiler'у разъехаться в набор доменных mini-компиляторов с расходящимися инвариантами (sanitize/dedup/order). Поэтому **replace-манифесты отклонены** как модель: mutation+suppress даёт ту же выразительность, сохраняя контроль за core.
- **No additions + read-only children** удерживают доменную поверхность минимальной и предотвращают обход core-инвариантов (домен не может «протащить» новые объекты, подавить чужие или мутировать соседние узлы).
- **Versioned + transport-independent** — единственный способ пережить будущую неидеальность контракта без флаг-дня и без переписывания доменов.

## v0 (разрабатываемый вариант)

- **Никаких миграций.** Разрабатывается **v0** этого контракта как in-process реализация. Это конечный самостоятельный вариант на текущем этапе, а не переходный слой «до миграции».
- Go `restore.DomainRestoreTransformer` (`TransformObject` + `CoveredPVCNames`) — **и есть** in-process v0 этого контракта: `TransformObject` ≙ mutation своего объекта, `CoveredPVCNames` ≙ suppress intent.
- Объём v0 (в этом порядке): зафиксировать интерфейс как in-process v0; добавить generic `OutputGuard` поверх трансформера; ввести allowlist-валидацию (P1) внутри guard (P2); привести `CoveredPVCNames` к suppress-семантике; покрыть тестами границы домена.
- **v0 начинается с `OutputGuard` + allowlist, а не с HTTP/API транспорта.** Внешний транспорт — это **отдельное возможное будущее направление** (см. Open questions), не запланированный переход от v0; именно поэтому контракт transport-independent — чтобы такой вариант не ломал v0.

## Open questions / future design (отдельные ADR/design docs)

Намеренно **не** фиксируется в этом ADR:

- **per-object vs per-node** — логический API: вызов на каждый объект (как в PoC: `CoveredPVCNames`/`TransformObject`, два вызова на объект) vs один node-level вызов (все объекты узла → изменённые объекты + `suppressRefs`). PoC-форма — транспортный компромисс, не финальный публичный контракт;
- **exact transport** — HTTP webhook vs aggregated APIService vs иное;
- **exact request/response schema** — точные JSON-поля envelope, patch и suppress;
- **discovery** — поля в `DomainSpecificSnapshotController` (какие группы обслуживает домен, версии контракта, адрес);
- **payload size / children summary** — полные манифесты детей vs облегчённый summary на глубоких деревьях;
- **authn / authz** — mTLS, RBAC, доверие к доменному ответу;
- **timeout / retry policy** — поведение при медленном/недоступном домене (с учётом того, что трансформация задумана как чистая/идемпотентная);
- **allowlist declaration format** — как именно домен объявляет mutable field paths per GVK (P1) и точный перечень проверок `OutputGuard` (P2);
- **typed intents vs raw patches** — высокоуровневые декларативные intents поверх/вместо сырых патчей;
- **RestorePlan / preview / diff** — детерминированный материализованный артефакт восстановления как отдельная сущность;
- **domain conformance suite** — исполняемый набор проверок инвариантов для внешних доменных трансформеров;
- **diagnostics schema** — машиночитаемая причина при fail-whole (какой узел/домен/объект и почему).

## Главный итог

Мы не говорим доменным командам «вот API, реализуйте». Мы фиксируем **форму будущего API**: generic главный; domain не создаёт объекты и возвращает patch/suppress; контракт versioned и transport-independent. Сейчас разрабатывается **v0** этого контракта как in-process реализация — самостоятельный вариант, **без миграций**. Точные поля и транспорт — следующими документами.

## Appendix: PoC transport (демонстрация для доменных команд)

> **Статус: PoC / демонстрация.** Это **не** production-контракт и **не** Kubernetes APIService. Это исполняемый пример HTTP-транспорта поверх того же семантического контракта (mutation своего объекта + suppress), чтобы доменные команды видели форму запроса/ответа. Точный транспорт по-прежнему открыт (см. Open questions) — этот PoC его не фиксирует и не является миграцией от in-process v0.

**Граница владения (читать до копирования паттерна).** Транспорт делится строго:

```
generic state-snapshotter:  только transport-agnostic интерфейс DomainRestoreTransformer + REST-клиент
domain controller:          REST-сервер / transform endpoint
```

Generic core **не хостит** доменное API и **не поднимает** доменный endpoint. В PoC сервер живёт в том же бинаре **только потому, что демо-домен (PR5a) и так вшит в этот бинарь**; при этом он регистрируется через **обвязку демо-домена** (`demo.AddRestoreTransformServerToManager`, manager Runnable — как любой доменный контроллер), а **не** в generic `cmd/main.go` и **не** в generic core. Это **не** целевая архитектура: в production реальный доменный контроллер обслуживает endpoint из своего модуля/бинаря, а generic держит только клиент. Анти-паттерн, которого избегаем: «generic state-snapshotter поднимает REST-сервер для domain transform».

**Расположение в коде (layering).** Wire-контракт вынесен в импортируемый пакет, отдельно от generic-имплементации и от домена:

```
pkg/restoretransform          — импортируемый wire-контракт (DTO, endpoint path, version, env). Ноль зависимостей.
internal/usecase/restore      — generic implementation detail (REST-клиент, DomainRestoreTransformer-адаптер, интеграция с RestoreNode). Потребитель контракта, не владелец.
internal/controllers/demo     — PoC domain implementation (HTTP handler/server) контракта.
```

Почему так: отдельный доменный модуль **не может** импортировать `internal/` чужого модуля. Контракт обязан жить в `pkg/restoretransform`, иначе для внешнего домена это не контракт. Generic restore — потребитель контракта, а не его владелец; демо — одна из реализаций.

**Что это даёт.** Тот же `restore.DomainRestoreTransformer` обслуживается по HTTP: generic-сторона — domain-free REST-клиент (`restore.RESTTransformer`), доменная сторона — тонкий HTTP-адаптер над своей in-process-логикой (демо: `demo.RestoreTransformHandler`). Поведение восстановления при включённом эндпоинте эквивалентно in-process-варианту.

**Включение (PoC).** Один env драйвит обе стороны; не задан — работает прежний in-process путь:

```
RESTORE_TRANSFORM_ENDPOINT=http://127.0.0.1:9181/apis/restore.state-snapshotter.deckhouse.io/v1alpha1/transform
```

При заданном env restore compiler (generic) делегирует трансформацию по этому URL — это конфигурация **клиента**. Демо-домен (владелец endpoint) поднимает HTTP-сервер на loopback-хосте, и для PoC адрес прослушивания выводится из того же URL — это **сокращение PoC** ради одного env; в production доменный контроллер владел бы своей listen-конфигурацией отдельно. **Никаких новых k8s-объектов** (Service/APIService) — co-located loopback HTTP в том же поде.

**Канонический эндпоинт (то, что реализует доменная команда):**

```
POST /apis/restore.state-snapshotter.deckhouse.io/v1alpha1/transform
Content-Type: application/json
```

**Request** (`object` — уже sanitized unstructured; `node.snapshotRef` — снапшот-CR, владеющий узлом; `children` — read-only контекст):

`uid` — **непрозрачный** per-call токен корреляции: домен обязан вернуть его как есть и **не** парсить (identity объекта — в `targetRef`, не в `uid`).

```json
{
  "request": {
    "uid": "a1b2c3d4-0000-0000-0000-000000000000",
    "targetRef": { "apiVersion": "demo.state-snapshotter.deckhouse.io/v1alpha1", "kind": "DemoVirtualDisk", "namespace": "restored-ns", "name": "disk-a" },
    "node": { "snapshotRef": { "apiVersion": "demo.state-snapshotter.deckhouse.io/v1alpha1", "kind": "DemoVirtualDiskSnapshot", "namespace": "source-ns", "name": "disk-a-snap" } },
    "restoreContext": { "sourceNamespace": "source-ns", "targetNamespace": "restored-ns" },
    "object": { "apiVersion": "demo.state-snapshotter.deckhouse.io/v1alpha1", "kind": "DemoVirtualDisk", "metadata": { "name": "disk-a", "namespace": "restored-ns" }, "spec": { "persistentVolumeClaimName": "disk-a-pvc" } },
    "dataRefs": [],
    "children": []
  }
}
```

**Response** (PoC возвращает полный трансформированный объект, не patch; `suppressRefs` — PoC только PVC):

```json
{
  "response": {
    "uid": "a1b2c3d4-0000-0000-0000-000000000000",
    "handled": true,
    "allowed": true,
    "object": { "apiVersion": "demo.state-snapshotter.deckhouse.io/v1alpha1", "kind": "DemoVirtualDisk", "metadata": { "name": "disk-a", "namespace": "restored-ns" }, "spec": { "persistentVolumeClaimName": "disk-a-pvc", "dataSource": { "apiGroup": "demo.state-snapshotter.deckhouse.io", "kind": "DemoVirtualDiskSnapshot", "name": "disk-a-snap" } } },
    "suppressRefs": [ { "apiVersion": "v1", "kind": "PersistentVolumeClaim", "namespace": "restored-ns", "name": "disk-a-pvc" } ],
    "warnings": []
  }
}
```

Объект не обслуживается доменом → `{"response":{"uid":"…","handled":false,"allowed":true}}`. Ошибка домена → `allowed:false` + `status.{reason,message}`.

**Минимальные generic-гарантии в PoC (владелец пайплайна — generic):** `response.uid` обязан совпадать с `request.uid`; при `handled:true` обязан присутствовать `object`; generic отклоняет смену identity (GVK/name/namespace) доменом; `allowed:false` — hard fail-whole; `suppressRefs` не-PVC (или PVC без имени) — fail-closed (`ErrContractViolation`), а не молчаливый пропуск; недоступный/битый endpoint — restore падает, без частичного результата. Полный `OutputGuard`/allowlist (P1/P2) в PoC ещё **не** реализован — это остаётся целевой работой v0.

**Что PoC доказал (и что НЕ зафиксировано как финальный API).** Доказано одно: доменную трансформацию можно вынести за процесс по HTTP, **не ломая** generic restore pipeline (`transformNodeObjects` не тронут). Это и есть результат — transport boundary существует. При этом **текущая форма контракта не считается финальным публичным API**: два метода

```
CoveredPVCNames(node, objects) -> set of PVC names   // suppress intent
TransformObject(node, object, children) -> mutated object
```

удобны для минимальной интеграции, но порождают **два вызова на объект** (suppress-вызов + transform-вызов) — это осознанный **транспортный компромисс PoC**, а не рекомендация для долгоживущего API. Открытый вопрос дизайна — **per-object vs per-node** (один node-level вызов: все объекты узла на вход → изменённые объекты + `suppressRefs` на выход). Решение откладывается в отдельный design step; PoC его намеренно не предрешает.
