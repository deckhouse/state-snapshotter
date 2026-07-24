# ТЗ: DomainCaptureStatus builder (`pkg/snapshotsdk`)

- **Дата:** 2026-07-20 (rev. 5)
- **Статус:** Accepted / implemented (rev. 5)
- **Суть:** `DomainCaptureStatus` — **единый** публичный интерфейс записи domain capture status (`phase` / `reason` / `message`); старые lifecycle status verbs удалены; на wire публикуется существующая фаза `Planning`.

Virtualization reference pinned @ `0be3d87cb975`.

---

## 1. Зачем builder

Сегодня lifecycle-запись — набор разрозненных публичных verbs:

```text
ReportProgress(message)  → только message (phase часто "")
MarkPlanned()            → Planned, clear reason/message
ConfirmConsistent()      → Finished, clear reason/message
Fail / Reject            → Failed + reason/message
```

Проблемы:

1. Waiting с machine-readable reason нельзя выразить одним intent (`ReportProgress` не принимает reason; `Planning` никто не пишет).
2. Каждый verb сам кодирует правила очистки/валидации — нет единого «желаемого domain status» на время reconcile.
3. Два параллельных публичных API (verbs + builder) размывают контракт и дублируют документацию/тесты.
4. Virtualization уже мыслит накопителем (`ConditionBuilder`: Status/Reason/Message). Нам нужна та же идея для domain-секции, не для conditions.

**Центральная мысль:**

> Добавить `DomainCaptureStatus` builder как **единый публичный интерфейс** управления domain capture status, мигрировать на него все текущие lifecycle paths, **удалить** старые публичные status verbs и вернуть на wire существующую фазу `Planning`.

Builder **не** facade и **не** additional API. Старые methods **не** остаются semantic shortcuts.

---

## 2. Scope

### Делаем

- Вводим `DomainCaptureStatus` как единый публичный API записи `phase` / `reason` / `message`.
- Мигрируем существующих consumers со старых lifecycle methods на builder.
- Удаляем старые публичные методы (`MarkPlanned`, `ConfirmConsistent`, `Fail`, `Reject`, `ReportProgress`) либо оставляем только **непубличные** helpers, если они нужны реализации.
- Начинаем публиковать `Planning` на waiting paths (через builder).
- Обновляем `pkg/snapshotsdk/README.md` и `pkg/snapshotsdk/README.ru.md` под **один** интерфейс.

### Не делаем

- Compatibility wrappers для старых status verbs.
- Сохранение двух параллельных публичных API (verbs + builder).
- Partial-update semantics как отдельный режим builder’а.
- Запись domain-контроллером в `status.conditions`.
- Изменение materialization (`Ensure*`) — вне scope status verbs.

---

## 3. Текущее состояние (кратко)

| Факт | Где |
|---|---|
| Domain writes через adapter + `patch.Status` (retry on conflict) | `internal/patch/patch.go` |
| Полная triple пишет `setPhase` | `capture.go` |
| Публичные verbs → `setPhase` / message-only | `MarkPlanned`, `ConfirmConsistent`, `Fail`, `Reject`, `ReportProgress` |
| Additive `DomainCaptureStatus` уже есть (rev. 4) | `domain_capture_status.go` — **переработать**: verbs убрать из public surface |
| `Planning` в enum/`phaseRank` есть | `capture_state_types.go`, `phaseRank` |
| Monotonic + Failed sink; запрещённый переход = тихий no-op | `phaseCanAdvance`, `capture_phase_test.go` |
| Adapter — единственный seam | `adapter.go` |

Waiting demo сегодня: `phase=""` + `ReportProgress(message)`  
(POC: `virtualdisksnapshot_controller.go` ≈ pending-ветка).

---

## 4. Virtualization: что берём / что нет

**Берём:** fluent Reason/Message (+ Phase вместо Condition.Status); mental model «собрал желаемое → применил»; перезапись полей на ветках handler’а.

**Не копируем буквально:** `defer SetCondition` in-memory (у нас optimistic Get+Patch; ошибку из defer неудобно вернуть); прямую мутацию `status.conditions`; `Build() → wire CRD struct` наружу (обход adapter/SDK).

Источник: [ConditionBuilder](https://github.com/deckhouse/virtualization/blob/0be3d87cb9756847335c3ca0ff9ca877bdcb8b87/images/virtualization-artifact/pkg/controller/conditions/builder.go), usage в [vmsnapshot life_cycle](https://github.com/deckhouse/virtualization/blob/0be3d87cb9756847335c3ca0ff9ca877bdcb8b87/images/virtualization-artifact/pkg/controller/vmsnapshot/internal/life_cycle.go).

---

## 5. Целевой контракт

### 5.1. Единый публичный API

```go
status := sdk.DomainCaptureStatus(adapter)

status.
    Phase(PhasePlanning).
    Reason(ReasonSnapshotting).
    Message(message).
    Apply(ctx)
```

Барьер 1 (Planned):

```go
status.
    Phase(PhasePlanned).
    Apply(ctx)
```

Барьер 2 / завершение (Finished):

```go
status.
    Phase(PhaseFinished).
    Apply(ctx)
```

Ошибка (Failed):

```go
status.
    Phase(PhaseFailed).
    Reason(reason).
    Message(err.Error()).
    Apply(ctx)
```

Waiting без machine-readable reason (reason optional):

```go
status.
    Phase(PhasePlanning).
    Message(message).
    Apply(ctx)
```

`DomainCaptureStatus(adapter)` возвращает fluent writer; `Apply(ctx)` идёт через существующий status-write path (`setPhase` / эквивалент: adapter + monotonic rules).

### 5.2. Удаляемые публичные методы

Из публичного API SDK **удалить** (не оставлять wrappers):

```text
MarkPlanned
ConfirmConsistent
Fail
Reject
ReportProgress
```

Если реализации удобны тонкие unexported helpers с прежней логикой — допустимо; наружу они не экспортируются и не документируются.

### 5.3. Правила при apply полной triple

| Phase | Reason | Message |
|---|---|---|
| `Planning` | optional | optional |
| `Planned` / `Finished` (вход в фазу) | принудительно `""` | принудительно `""` |
| `Planned` / `Finished` (same-phase re-apply) | из builder (в т.ч. diagnostic / clear) | из builder |
| `Failed` | как у прежнего `Fail`/`Reject` path | optional (рекомендуется) |

Для `Failed` семантика обязательности reason **совпадает** с прежним контрактом failure verbs (сегодня пишут `string(reason)` без отдельного reject на пустой reason) — **не** вводим новую validation policy только для builder.

Переходы — как у текущего `phaseCanAdvance` (**тихий no-op** на regress; Failed sink).  
Idempotent identical apply — no-op.  
`status.conditions` не трогать.

Первый PR: только `Phase` / `Reason` / `Message` — **без** shortcuts `Planning()` / `Planned()` / `Finished()` / `Failed(...)`.

### 5.4. Стили использования

Допустимы оба стиля:

- короткий fluent expression на одну ветку;
- reconcile-local accumulator с перезаписью полей на ветках и явным `Apply` перед early return (waiting), без обязательного `defer`.

---

## 6. `ReportProgress` и публикация `Planning`

Отдельного публичного `ReportProgress` и патча «`"" → Planning` внутри него» **нет**.

Все waiting consumers мигрируют с:

```go
sdk.ReportProgress(ctx, adapter, message)
```

на:

```go
sdk.DomainCaptureStatus(adapter).
    Phase(PhasePlanning).
    Message(message). // Reason — optional, когда есть
    Apply(ctx)
```

После миграции `ReportProgress` удаляется из публичного API.

### Доказательство безопасности `Planning` на wire (binder/core)

Barrier 1 = только `Planned` \| `Finished`:

```107:113:images/state-snapshotter-controller/internal/controllers/genericbinder/domain_content.go
func domainCaptureAtLeastPlanned(obj *unstructured.Unstructured) bool {
	switch storagev1alpha1.SnapshotCapturePhase(domainCapturePhase(obj)) {
	case storagev1alpha1.SnapshotCapturePhasePlanned, storagev1alpha1.SnapshotCapturePhaseFinished:
		return true
	default:
		return false
	}
}
```

Тест: и `Planning`, и `""` **не** проходят (`genericbinder/barrier_test.go`).

Namespace pre-Planned: absent **или** Planning — один default-branch (`namespace_children_plan.go`).

**Вывод:** публикация `Planning` вместо `""` не открывает content takeover и не ломает pre-Planned gates; меняет observability под существующий enum.

---

## 7. Изменения по файлам (ориентир)

| Область | Что |
|---|---|
| `pkg/snapshotsdk` | `DomainCaptureStatus` = единственный публичный status-write API; удалить публичные `MarkPlanned` / `ConfirmConsistent` / `Fail` / `Reject` / `ReportProgress` (или unexport) |
| Consumers в этом репо | миграция всех вызовов старых verbs на builder |
| Demo / POC (если в scope PR или follow-up) | waiting и lifecycle paths → builder; без `ReportProgress` |
| Tests | переписать verb-тесты на builder; покрыть waiting / Planned / Finished / Failed / monotonic / Failed sink |
| `pkg/snapshotsdk/README.md` и `pkg/snapshotsdk/README.ru.md` | единый API; убрать документацию старых verbs; все примеры на builder; state machine + waiting/barrier/failure |
| API godoc (optional same PR) | reason/message не только для Failed |

README отражает **фактический** выбранный публичный API после реализации, не псевдокод из ТЗ. Документацию **вне** `pkg/snapshotsdk/` не менять без прямой необходимости.

---

## 8. README SDK (обязательно)

В `pkg/snapshotsdk/README.md` и `pkg/snapshotsdk/README.ru.md`:

- удалить документацию старых lifecycle methods;
- заменить все примеры на builder;
- описать одну state machine: `Planning → Planned → Finished`, плюс `Failed`;
- показать waiting, barrier и failure через **один** API;
- **не** называть builder facade / additional / additive API;
- явно: это **единый** способ публиковать domain capture status.

---

## 9. Тест-план (минимум)

1. Apply полной triple `Planning` + reason + message через adapter/patch path.
2. Waiting: `Planning` + message (без reason) публикует phase на wire.
3. Чужие domain-поля (MCR/VCR/children/excluded) не затираются.
4. Regress phase → тихий no-op, как у `setPhase`.
5. Failed через builder; последующий Planned не воскрешает.
6. Idempotent повтор.
7. Planned/Finished очищают reason/message.
8. Публичные символы старых verbs отсутствуют (compile / API surface).
9. Consumers в репо собраны без старых вызовов; прежние phase-тесты переписаны на builder и green.

---

## 10. Зафиксировано / open

**Зафиксировано:**

- единый публичный API = `DomainCaptureStatus` + `Phase` / `Reason` / `Message` + `Apply`;
- старые публичные status verbs удаляются (без compatibility wrappers);
- partial updates не входят в задачу;
- regress — тихий no-op, как у текущего `setPhase`;
- для `Failed` нет новой validation policy сверх прежнего failure path;
- форма apply: `sdk.DomainCaptureStatus(adapter).….Apply(ctx)` (уже выбранная в rev. 4 реализации — сохраняем).

**Open (не блокирует ТЗ):**

- полный список внешних importers SDK вне этого репо (POC / другие модули) — мигрировать в том же PR или явным follow-up с gate «нет публичных verbs».

---

## 11. Критерий готовности

> Добавить `DomainCaptureStatus` builder как единый публичный интерфейс управления domain capture status, мигрировать на него все текущие lifecycle paths, удалить старые публичные status verbs и вернуть на wire существующую фазу `Planning`.

---

## 12. Порядок работ

1. Зафиксировать `DomainCaptureStatus` как единственный публичный status-write path (доработать/убрать additive-формулировки в godoc).
2. Мигрировать все in-repo consumers: `MarkPlanned` / `ConfirmConsistent` / `Fail` / `Reject` / `ReportProgress` → builder.
3. Удалить (или unexport) старые публичные методы; убрать из интерфейсов `CaptureSDK` / связанных faces.
4. Unit tests (§9): phase machine и lifecycle paths только через builder.
5. Обновить `pkg/snapshotsdk/README.md` и `pkg/snapshotsdk/README.ru.md`:
   - единый раздел записи domain capture status;
   - waiting / barrier / failure только через builder;
   - state machine `Planning → Planned → Finished` + `Failed`;
   - без документации и рекомендаций по старым verbs;
   - без слов facade / additional / additive как описание роли builder’а.
6. Demo/POC waiting-path — в том же PR, если consumer в scope; иначе явный follow-up до объявления API стабильным.
