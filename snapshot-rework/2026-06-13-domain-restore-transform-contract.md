# ADR: Domain restore-transform contract (manifests-with-data-restoration)

- **Дата:** 2026-06-13
- **Статус:** Proposed (целевая модель). Текущий in-process `DomainRestoreTransformer` — это v0-реализация той же семантики; внешний транспорт — future work.
- **Связано с:** `snapshot-rework/2026-06-10-restore-manifests-compiler.md` (restore compiler), `docs/state-snapshotter-rework/spec/system-spec.md`.

## Контекст

Restore compiler (`manifests-with-data-restoration`) компилирует дерево снапшотов в apply-ready манифесты снизу вверх. Доменная часть (например `DemoVirtualDisk` → `spec.dataSource` на свой снапшот; covered-PVC) сейчас живёт **в том же бинаре** как in-process реализация интерфейса `restore.DomainRestoreTransformer` (`internal/controllers/demo/restore_transform.go`), зарегистрированного в `restore.Service` через `NewService(..., transformers...)`.

Цель — вынести доменную логику за границу generic-компилятора так, чтобы:
1. generic-компилятор оставался **domain-free** (не знает имён доменных полей/kind'ов);
2. домены подключались декларативно, без пересборки контроллера;
3. контракт был **versioned и эволюционируемым** — он заведомо будет неидеален, и неидеальность должна чиниться **аддитивно** (новые поля/версия), а не сломом формы.

Ключевое осознание: «APIService» — не самоцель. Нужны три его свойства: декларативный discovery, стабильный versioned-контракт, RBAC/mTLS. Их можно получить без превращения каждого домена в aggregated apiserver (см. «Транспорт»).

## Решение (зафиксированная форма)

Целевой контракт доменной restore-трансформации:

- **Гранулярность: per-node.** Один вызов на узел дерева (совпадает с post-order). Домен видит объекты своего узла и результаты **прямых** детей.
- **Generic владеет набором объектов и инвариантами.** Generic делает sanitize, namespace rewrite, dedup, ordering и владеет множеством объектов. Домен **только мутирует** свои объекты и объявляет, что подавить.
- **Модель мутации: patches + suppress** (AdmissionReview-подобно). Домен возвращает патчи к **своим** объектам и список объектов на подавление. Домен **не** возвращает полные манифесты и **не** создаёт новые объекты (в v1alpha1).
- **Versioned envelope.** Запрос/ответ обёрнуты как `AdmissionReview`: `apiVersion`, `kind`, `request{}`/`response{}`. Внутри версии — только аддитивные поля.
- **Домен — чистая идемпотентная функция запроса.** Без сайд-эффектов; generic свободно ретраит/кэширует; fail-whole безопасен.
- **Один контракт — много транспортов.** Контракт = типы request/response. In-process, REST и (опционально) aggregated APIService используют **одни и те же типы**; меняется только транспорт.

## Контракт (v1alpha1)

### Request

```jsonc
{
  "apiVersion": "restore.state-snapshotter.deckhouse.io/v1alpha1",
  "kind": "RestoreTransformRequest",
  "request": {
    "uid": "…",                         // трассировка/идемпотентность
    "node": { "apiVersion": "...", "kind": "DemoVirtualDiskSnapshot", "name": "...", "namespace": "ns" },
    "targetNamespace": "restore-ns",
    "dataRefs": [ /* PVC -> VSC артефакты ЭТОГО узла (read-only) */ ],
    "objects": [ /* sanitized объекты узла; generic уже почистил (read-only вход, мутируется только через patches) */ ],
    "children": [
      {
        "node": { ... },
        "objects": [ /* restored объекты ПРЯМОГО ребёнка (read-only context) */ ]
      }
    ]
  }
}
```

### Response

```jsonc
{
  "apiVersion": "restore.state-snapshotter.deckhouse.io/v1alpha1",
  "kind": "RestoreTransformResponse",
  "response": {
    "uid": "…",                         // = request.uid
    "allowed": true,                    // false => fail-whole, message обязателен
    "message": "",
    "patches": [
      { "target": { /* objectRef ИЗ request.objects */ }, "patchType": "JSONPatch", "patch": [ /* RFC6902 */ ] }
    ],
    "suppress": [ { /* objectRef ИЗ request.objects */ } ]  // generic не эмитит эти объекты
  }
}
```

### Нормативные правила (INV-DRT)

- **INV-DRT1 (suppress scope).** `suppress[]` ссылается **только** на объекты `request.objects` (текущий узел). Подавить объект ребёнка/родителя нельзя — иначе домен ломает generic-инварианты дерева. children передаются **read-only как контекст**.
- **INV-DRT2 (patch scope).** `patches[].target` — **только** объекты `request.objects`. Патчить children запрещено (например VM-трансформер не имеет права мутировать диск). children — read-only.
- **INV-DRT3 (generic валидирует результат патча).** После применения патчей generic проверяет каждый объект; нарушение → fail-whole (`ErrContractViolation`):
  - `apiVersion`/`kind`/`metadata.name`/`metadata.namespace` **не изменились**;
  - объект **не стал** cluster-scoped (namespace не опустошён);
  - **запрещённые/runtime-поля не вернулись** (то, что вырезал sanitizer: `uid`, `resourceVersion`, `managedFields`, `status`, stale-аннотации, `PVC.spec.volumeName` и т.п.);
  - не появились **control-plane kinds** (Snapshot/SnapshotContent/VS/VSC/VRR/`*Snapshot`).
  То есть домен **не может обойти sanitizer через patch**.
- **INV-DRT4 (no additions в v1alpha1).** Трансформация **не создаёт новые объекты**. Набор объектов на выходе ⊆ входного (минус `suppress`). Расширение `additions` — будущая версия, явно вне v1alpha1.
- **INV-DRT5 (single owner).** Объект обрабатывается максимум одним доменом (discovery по `handlesGroups`, см. ниже). 0 доменов = generic passthrough (объект проходит как есть после sanitize).
- **INV-DRT6 (pure/idempotent).** Трансформация — чистая функция запроса, без сайд-эффектов и без чтения внешнего состояния, влияющего на результат.
- **INV-DRT7 (fail-whole).** `allowed=false`, недоступность домена, таймаут или провал валидации (INV-DRT3) → падает весь restore (`409 Conflict` для контрактных нарушений / `503` для недоступности). Частичной выдачи нет.

### children: форма на вырост

Для MVP `children[].objects` — полные restored-манифесты прямого ребёнка. Контракт **резервирует** место под облегчённый вариант, чтобы payload не пух на глубоких деревьях:

```jsonc
"children": [ { "node": {...}, "objects": [...], "outputs": [ /* summary: refs/ключевые поля */ ] } ]
```

Переход на summary (или передачу только `outputs`) — аддитивное изменение в рамках версии; домены, которым хватает идентичности ребёнка (например VM нужен лишь `kind/name` восстановленного диска), смогут не запрашивать полные объекты.

## Discovery и capability negotiation

Переиспользуем существующий реестр доменов — `DomainSpecificSnapshotController` (DSC). Домен объявляет:

```yaml
spec:
  restoreTransform:
    handlesGroups: ["demo.state-snapshotter.deckhouse.io"]   # какие группы CR обслуживает (INV-DRT5)
    supportedContractVersions: ["restore.state-snapshotter.deckhouse.io/v1alpha1"]
    # транспорт (для remote): service ref + path; для in-process отсутствует
    service: { name: ..., namespace: ..., port: 443, path: /restore-transform }
```

- Generic строит карту `group → домен` из **готовых** (`Ready=True`) DSC; коллизия (две группы-владельца на один объект) → `ErrContractViolation`.
- Generic выбирает максимально общую `supportedContractVersions`. Контракт устарел у домена → договорённость по версии, без флаг-дня.

## Транспорт (эволюция, контракт неизменен)

```
сейчас:   in-process DomainRestoreTransformer (demo) — v0 реализация семантики ниже
интерим:  тот же контракт по REST (webhook-style) к domain Service; discovery из DSC; mTLS
цель:     по желанию отдельный домен промотируется в aggregated APIService subresource
```

- **In-process (сейчас):** Go-интерфейс, нулевая сеть.
- **REST/webhook (интерим):** `POST https://<domain-svc>.<ns>.svc:443/restore-transform`, mTLS как у admission-вебхуков. Лёгкий: ноль аггрегации per-domain, discovery через DSC.
- **Aggregated APIService (опционально):** `POST /apis/<domain-group>/v1alpha1/namespaces/{ns}/{resource}/{name}/restore-transform`. k8s-native, но cert+`APIService`+аггрегация **на каждый домен** — применять точечно, не по умолчанию.

Все три используют один и тот же `RestoreTransformRequest/Response`.

## Рассмотренные альтернативы (и почему отклонены)

- **Replace-манифесты (домен возвращает полные объекты узла).** Отклонено: generic теряет контроль над набором объектов, dedup и sanitizer обходятся; restore compiler быстро превращается в **набор доменных mini-restore-компиляторов** с расходящимися инвариантами. Patch+suppress даёт ту же выразительность, сохраняя ownership.
- **Per-object вызов.** Отклонено: round-trip на каждый объект, потеря контекста соседей.
- **Per-subtree (домен traverse сам).** Отклонено: домены дублируют обход и generic-инварианты (sanitize/dedup/ordering), неизбежный дрейф.
- **Типизированный intent (домен возвращает не патчи, а «dataSource=…»).** Отклонено как единственная модель: слишком жёстко для неизвестных доменов. Patch-модель покрывает intent-случаи и оставляет гибкость; при желании поверх patch можно ввести типизированные intent-хелперы позже (аддитивно).

## Migration note

- Текущий Go `restore.DomainRestoreTransformer` (`TransformObject` + `CoveredPVCNames`) — это **in-process реализация той же семантической модели**: `TransformObject` ≙ `patches` для своего объекта, `CoveredPVCNames` ≙ `suppress`. Внешний транспорт (REST/APIService) — future work.
- Следующий шаг (после принятия ADR, отдельной задачей): при необходимости привести текущий интерфейс ближе к контракту — выразить мутацию как «patch к своему объекту» и обобщить `CoveredPVCNames` → `suppress []objectRef`, плюс добавить generic-валидацию результата (INV-DRT3), которая полезна уже и для in-process.
- Реализация внешнего транспорта и расширение DSC под `restoreTransform` — отдельные задачи; этот ADR фиксирует только целевую форму контракта.

## Открытые вопросы (аддитивно, вне v1alpha1)

- `additions`: домен создаёт новые объекты (с строгой валидацией identity/scope) — новая версия.
- `patchType` помимо `JSONPatch` (strategic-merge / server-side apply).
- `children.outputs` summary вместо полных объектов — оптимизация payload на глубоких деревьях.
- batching нескольких узлов в один вызов.
- кросс-узловой контроль одинакового VSC у разных узлов (сейчас проверяется только в пределах узла).

## Главный итог

Неидеальным будет **содержание** контракта (какие поля домену реально нужны) — и это ок, потому что аддитивно. Сломать больно может только **форма**: гранулярность, владелец набора объектов, наличие версии. ADR фиксирует именно эти три по образцу `AdmissionReview`, поэтому будущая неидеальность лечится новой минорной версией, а не миграцией.
