# R2 phase 2b / R3: runtime registry и динамические watch

Техдизайн и **состояние реализации** (ядро закрыто в коде). Цель шага — не «полный симметричный hot reload», а осмысленное поведение во время жизни процесса.

**Целевая формулировка:** DSC изменился → реестр обновлён → **новые** eligible типы начинают watch’иться **без рестарта pod**. Снятие watch и полная симметрия desired/active **не гарантируются**; при необходимости — лог, метрика, рекомендация рестарта.

Связанные документы: [`implementation-plan.md`](implementation-plan.md), ADR [`snapshot-rework/2026-01-23-unified-snapshots-registry.md`](../../../snapshot-rework/2026-01-23-unified-snapshots-registry.md).

---

## Приоритет относительно остального трека

1. ~~**R2 2b / R3 (ядро)**~~ — сделано в коде (`pkg/unifiedruntime`, интеграционный proof, метрики/лог stale).
2. ~~**D1–D3**~~ ✅ — [`README.md`](../README.md), [`operations/runbook-degraded-and-unified-runtime.md`](../operations/runbook-degraded-and-unified-runtime.md), [`operations/dsc-rbac-and-mcr.md`](../operations/dsc-rbac-and-mcr.md). Поддерживать в актуальном виде при изменениях кода.
3. **R5 и feature gates** — rollout/operability, не сердцевина механики.
4. **M1 / M2 (manifest)** — отдельный трек, не смешивать с R2/R3 в одной реализации.

---

## Слои состояния (явная модель)

| Слой | Смысл | Пример источника |
|------|--------|-------------------|
| **desiredPairs** | Что хотим по политике | `bootstrap ∪ DSC` (DSC переопределяет snapshot GVK) |
| **eligiblePairs** | Подмножество после формулы eligibility | `Accepted` + `RBACReady` + согласованные `observedGeneration`; `Ready` не в предикате watch |
| **resolvedPairs** | То, что реально есть в API | После RESTMapper / discovery (CRD есть, GVK валиден) |
| **activeWatches** | Что уже подвешено в этом процессе | Фактически зарегистрированные watch в controller-runtime |

Инвариант мышления: **registry state** и **wiring controller-runtime** — разные слои; не смешивать в одну структуру без границ.

---

## Модель переходов (на каждый актуальный reconcile DSC / тик реестра)

1. Пересчитать **desired** (bootstrap + актуальный срез DSC).
2. Применить формулу → **eligible**.
3. Прогнать discovery → **resolved** (отсутствующий CRD не валит процесс).
4. **diff(resolved, activeWatches)**:
   - **add:** для отсутствующих в `active` — добавить watch (best-effort; один сбой не должен ломать уже работающие типы).
   - **remove / inactive:** тип выпал из eligible или CRD исчез — пометить в registry как **inactive**, залогировать; **clean unwatch без рестарта не обещаем** (ограничение библиотеки / стоимость).

Принципы:

- **Additive hot reload:** главный договор — новые eligible типы подхватываются без рестарта.
- **Best-effort deactivation:** «больше не следим смыслово» ≠ «informer гарантированно убит».
- **Fail-open на уровне процесса, fail-closed на уровне типа:** один битый DSC или один неудачный add не блокирует весь unified-трек; проблемный тип изолируется.

---

## Опасности (не потерять)

- Не блокировать unified-трек из-за одного некорректного DSC.
- Не требовать идеального symmetric unwatch, если controller-runtime это не поддерживает удобно.
- Не смешивать registry и wiring в одну «кашу».
- Не допускать сценария, где один неудачный `Watch` ломает уже работающие GVK.

---

## Тесты (ориентиры для «proof R3»)

Помимо существующих unit/integration:

| Сценарий | Статус | Как сделано / заметки |
|----------|--------|------------------------|
| DSC стал eligible после перехода **RBACReady=True** → `Sync` → layered state + оба watch зарегистрированы (`active` keys) | ✅ | `test/integration/unified_runtime_hot_add_test.go`, **`Serial`**. Маппинг на **RegistrationTestSnapshot** (изоляция от lifecycle-спек с прямым `Reconcile` на **TestSnapshot**; иначе глобальный watch + ручной Reconcile → 409). Перед тестом удаляются DSC `integration-unified-runtime-hot-add` и `integration-dsc-smoke`, чтобы не было **KindConflict** на один snapshot kind. |
| DSC появился **после** старта контроллера (новый тип без рестарта pod) | ✅ | Покрыто тем же hot-add сценарием относительно процесса envtest: manager уже поднят, DSC создаётся в тесте. |
| Unit: слои merge / list DSC error → bootstrap-only | ✅ | `pkg/unifiedruntime/layers_test.go` |
| DSC **потерял** eligibility → тип помечен inactive; процесс жив | ⬜ / частично | Выпадение из **resolved** логируется (`V(1)`). Если ранее оба watch успели подняться, ключ остаётся в monotonic **active** → считается **stale** (`stale_active_snapshot_gvk_count`, **Info**-лог + restart hint). Отдельного флага «inactive» в API и отдельного integration proof ещё нет. |
| Два независимых DSC: один в ошибке, второй корректен | ⬜ | Частично косвенно (KindConflict / InvalidSpec); отдельный интеграционный proof — в бэклоге. |

**Позиционирование proof:** интеграционный hot-add доказывает цепочку **DSC eligible → Sync → обновление registry state (layered + active keys)**, а не полную безопасность «долгоживущей смеси» additive watches в одном envtest manager (watches не снимаются; `active` монотонен).

---

## Честные ограничения (отражены в D1–D3)

- Что hot-reload **умеет** (в первую очередь add watches) и что **не умеет** (unwatch) — [`../README.md`](../README.md), runbook [`../operations/runbook-degraded-and-unified-runtime.md`](../operations/runbook-degraded-and-unified-runtime.md).
- Поведение при исчезновении CRD (degraded / fail-open по ADR) — там же §1 runbook.

---

## Реализация в коде (по состоянию репозитория)

- **`pkg/unifiedruntime`:** `LayeredGVKState` + `BuildLayeredGVKState` явно разделяют **bootstrap desired**, **eligible DSC**, **merged desired**, **resolved** (RESTMapper); `Syncer` хранит `lastState` (последний успешный расчёт слоёв; это **desired/resolved view**, не «всё уже применено в рантайме») и множество **active** snapshot GVK (`activeSnapshotGVKKeys`: монотонно, ключ только если **оба** `AddWatch*` прошли). Публичные `LastLayeredState()` / `ActiveSnapshotGVKKeys()` под тесты и observability. После каждого успешного `reconcileAll` DSC вызывается `Sync`: пересчёт слоёв + additive `AddWatch*`. `ResolvedSnapshotKeySet()` — дифф «выпал из resolved» (лог `V(1)`).
- **`internal/controllers`:** `UnifiedRuntimeSync` → `syncer.Sync`; `GenericSnapshotBinderController.AddWatchForPair`, `SnapshotContentController.AddWatchForContent` (идемпотентность по GVK; при ошибке `Complete` — откат с `RevertSnapshotRegistrationIfExact` all-or-nothing, см. ниже).
- **`cmd/main.go`:** как раньше: DSC после первичного `SetupWithManager` unified-контроллеров; syncer с bootstrap + `GetAPIReader()` для DSC.
- **`test/integration/setup_test.go`:** для интеграции повторён production-like bootstrap: merge DSC + resolve mapper → `NewGenericSnapshotBinderController` / `NewSnapshotContentController` → `SetupWithManager` → `unifiedruntime.NewSyncer` → `AddDomainSpecificSnapshotControllerToManager(..., unifiedSyncer.Sync)`. Не раздувать дальше без выноса в хелперы (риск «второй main.go»).
- **`test/integration`:** `dsc_api_smoke_test` — маппинг на RegistrationTest*; `controller_registration_test` — без повторного `SetupWithManager` на общем `mgr` (контроллеры уже в BeforeSuite).
- **Observability (R3):** gauges на registry controller-runtime (`pkg/unifiedruntime/metrics.go`): число resolved snapshot GVK, число monotonic-active ключей, число **stale** (в `active`, но не в текущем resolved). После каждого `Sync` — обновление gauge + лог `V(2)` со счётчиками; если stale > 0 — `Info` со списком `staleSnapshotGVKKeys` и текстом про перезапуск pod для чистых informer’ов.
- **Опционально:** расширение proof-сценариев по таблице «Тесты» выше.

### Риски / контракты (зафиксировано по ревью)

- **Повторный `Complete()` при уже запущенном manager** — опирается на поведение controller-runtime (`Add` runnable после `Start`). Работает в текущей версии и CI, но при апгрейде c-r стоит прогонять интеграционные тесты и при необходимости перечитать release notes.
- **Откат при ошибке watch:** при неудачном `Complete` откатываются слайсы (если в этом вызове был append) и вызывается **`RevertSnapshotRegistrationIfExact`** — только **все-или-ничего**: explicit mapping, оба GVK в maps и обратный индекс `snapshotKindByContentGroupKind` должны совпадать с переданной парой; иначе **no-op** (не снимаем «половину» состояния). Если append не было (`needAppend` / bootstrap-защита), registry при ошибке **не** откатываем — тогда возможен редкий trade-off: `RegisterSnapshotContentMapping` мог расширить explicit mapping, а watch не поднялся; это **осознанный** компромисс против случайного сноса bootstrap. Для финального R3 это снимает явный **active-state** слой.
- **Смена content side для того же snapshot kind** при только additive-модели по-прежнему хрупкая тема для будущего R3 / явного registry.
- **Тесты:** happy-path revert + no-op при несовпадении content GVK / snapshot GVK (`TestRevertSnapshotRegistrationIfExact_*`). Идемпотентность add-path после частичного фейла — отдельный integration/unit тест в бэклоге R3.
