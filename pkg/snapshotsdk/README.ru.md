> Язык: [English](./README.md) · **Русский**

# Руководство: как доменной команде пользоваться snapshot SDK (capture)

> Статус: **developer-facing usage guide** для команд, интегрирующих свой домен со
> snapshot-контроллером через `pkg/snapshotsdk`. Это «как пользоваться», а не нормативный контракт.
> Норматив контракта домен↔ядро — SDK-ADR (`2026-06-29-domain-snapshot-sdk.md`); godoc в
> `pkg/snapshotsdk` — норматив точных Go-сигнатур и инвариантов уровня кода; этот README — **не
> норматив**. Контракт качества кода — [`CLAUDE.md`](./CLAUDE.md). Reference-реализация —
> demo-контроллеры в репозитории `sds-unified-snapshots-poc` (`images/domain-controller/internal/controllers/demo`).
>
> Скоуп SDK v1 — **capture-only** (планирование снапшота: дочерние снапшоты + захват данных + захват
> манифестов + барьеры жизненного цикла).
> Restore — отдельный sanctioned-boundary (`pkg/snapshotsdk/transform`), в этом гайде не рассматривается.
>
> **Переход контракта.** Ниже явно разделены **CURRENT (51eb6c2)** и **TARGET** активного плана
> namespace-root-MCR-before-`Planned`. TARGET-примечания — контекст для передачи работы, а не
> утверждение, что описанные guards или будущие API уже есть в этой ревизии; вызывать можно только
> показанные здесь текущие сигнатуры.

## Подключение SDK из другого модуля

SDK и общие API-типы — два отдельных Go-модуля. Внешний доменный контроллер должен подключать
**оба** из одного проверенного compatibility-set (самый простой вариант — одна ревизия
state-snapshotter; до появления release-тегов — по pseudo-version):

```bash
go get github.com/deckhouse/state-snapshotter/api@<state-snapshotter-commit>
go get github.com/deckhouse/state-snapshotter/pkg/snapshotsdk@<state-snapshotter-commit>
```

Локальный `replace .../api => ../../api` из репозитория SDK копировать не надо: `replace` зависимого
модуля не наследуется потребителем. Проверенный интеграционный baseline — `images/domain-controller/go.mod`
в POC; произвольная комбинация ревизии SDK и несвязанной версии развёрнутого core не является
поддержанным контрактом совместимости. Версионированная compatibility policy пока не зафиксирована.

**В одном абзаце:** `snapshotsdk` позволяет доменному контроллеру **описать намерение снимка** (дочерние
снапшоты, опциональный data-PVC, manifest-таргеты, захваченный source), не реализуя оркестрацию снапшота.
Домен решает, **что** снимать; SDK решает, **как** управляются capture-запросы, ownership, патчинг статуса и
restart-safe жизненный цикл. SDK никогда не пишет условие `Ready` — core-контроллер выводит `Ready` на каждом
снапшоте, а домен читает его обратно как свой канал ошибок.

## Жизненный цикл снапшота за одну минуту

1. Пользователь создаёт доменный snapshot-CR (`MySnapshot`).
2. Доменный контроллер валидирует объект-источник (и в import-режиме сразу выходит).
3. Контроллер решает четыре вещи:
   - какие **manifest-таргеты** сохранить;
   - нужно ли сохранять **данные PVC**;
   - какие нужны **дочерние снапшоты**;
   - какие объекты-источники **исключены** (exclude-veto).
4. Контроллер передаёт это в SDK, который создаёт capture-запросы, публикует refs и захваченный source и
   проставляет доменную фазу жизненного цикла.
5. В обычном leaf/simple-parent пути контроллер объявляет все ноги захвата, а затем
   **барьер 1** (`MarkPlanned`, `phase=Planned`). Claim-gated generic binder мог уже независимо от
   этой фазы eager-создать и забайндить shell `SnapshotContent`.
6. `Planned` разрешает core проецировать в этот shell ноги/edges/status. Core захватывает ноги,
   переключает per-leg latch-и и пишет `Ready`.
7. Контроллер переключается по `CoreCaptureOutcome` и, когда все ноги захвачены, объявляет **барьер 2**
   (`ConfirmConsistent`, `phase=Finished`) после любых consistency-действий.

```
User
  |
  v
Domain Snapshot CR
  |
  v
Domain Controller
  |-- resolve + publish source
  |-- discover children (+ exclude veto)
  |-- resolve PVC
  |-- choose manifest targets
  |
  v
Snapshot SDK
  |-- EnsureChildren
  |-- EnsureVolumeCapture
  |-- EnsureManifestCapture
  |
  v
Barrier 1: phase = Planned      (MarkPlanned)
  |
  v
Core snapshot controller  --->  проецирует и захватывает ноги в eager-bound SnapshotContent,
  |                              переключает commonController latch-и, пишет Ready
  v
CoreCaptureOutcome
  |-- Captured  -> ConfirmConsistent -> phase = Finished   (Barrier 2)
  |-- Failed    -> stop (терминальный Ready принадлежит core)
  |-- Capturing -> wait / requeue
```

Это обычный поток.

> **CURRENT compatibility-примечание — не копировать в новый агрегатор.** В 51eb6c2
> subtree-gated агрегатор сначала замораживает/публикует детей через `MarkPlanned`, а свою ногу
> `EnsureManifestCapture(base − exclude)` объявляет после персистирования child manifests. Активный
> TARGET заменяет этот late-own-MCR/persisted-pre-gate порядок: опубликованные планы поддеревьев и
> собственный MCR узла должны существовать до `Planned`. Текущий порядок описан только для безопасной
> замены существующей реализации.

## Что такое snapshot SDK и зачем он нужен

`pkg/snapshotsdk` — доменно-нейтральная библиотека, которая стандартизирует **capture-фазу** снапшота
(планирование: дочерние снапшоты + захват данных + захват манифестов + барьеры жизненного цикла). Доменная
команда **описывает намерение** снимка («что снимаем»), а оркестрацию («как разложить это в Kubernetes») берёт
на себя SDK.

До SDK каждая доменная команда была вынуждена реализовывать всё это сама:

- ownerRefs на capture-объектах;
- create/adopt capture-запросов;
- идемпотентность;
- восстановление после рестарта;
- доменную фазу жизненного цикла (барьеры) и её монотонный guard;
- optimistic-lock патчинг статуса.

Результат был предсказуемый: дублирование кода, расхождение (drift) поведения между доменами, тонкие race
conditions и несогласованная семантика снапшотов.

SDK введён, чтобы:

- стандартизировать lifecycle capture-запросов;
- убрать boilerplate;
- одинаково enforce-ить инварианты во всех доменах.

**SDK берёт на себя:**

- lifecycle capture-запросов (VCR/MCR/дочерних снапшотов);
- патчинг доменных полей статуса (optimistic-lock);
- доменную фазу жизненного цикла (`Planning`/`Planned`/`Finished`/`Failed`) и её барьеры;
- restart-safe поведение;
- заморозку набора детей и сигнализацию drift набора manifest-таргетов.

**Доменный контроллер оставляет у себя:**

- валидацию источника (`sourceRef`);
- discovery топологии (какие дочерние снапшоты нужны) и exclude-veto;
- резолв PVC для захвата данных;
- доменные ошибки/причины (reasons).

## TL;DR — что от тебя требуется

**Концептуально:** SDK позволяет доменной команде **описывать намерение снимка**, а не реализовывать его
оркестрацию.

**Практически** домен предоставляет:

- **адаптер** (`SnapshotAdapter`) — тонкую обёртку над твоим snapshot-CR;
- **топологию дочерних снапшотов** и **исключённые** объекты-источники;
- **опциональный PVC** для захвата данных;
- **manifest-таргеты**;
- захваченный **source** для публикации.

Всё остальное (ownerRefs, создание capture-запросов, optimistic-lock patch статуса, фаза жизненного цикла и её
барьеры, идемпотентность, restart-safe, заморозка/drift) делает SDK.

## Что SDK снимает с твоего кода

Доменная команда **больше не реализует вручную**:

- управление ownerRef;
- именование capture-запросов;
- логику create-or-adopt;
- optimistic-lock патчинг статуса;
- обработку фазы/барьера жизненного цикла;
- проверки заморозки набора детей и drift набора manifest-таргетов;
- restart-safe реконсиляцию capture-запросов.

## Жизненный цикл capture: фазы и барьеры

**Условия `ChildrenSnapshotReady` нет.** Доменный жизненный цикл — это одно поле,
`status.captureState.domainSpecificController.phase`, которое SDK пишет за домен. Таблица описывает
**CURRENT (51eb6c2)**:

| Фаза | Смысл | Кто ставит |
|---|---|---|
| `Planning` | опциональное явное значение до барьера; SDK-first контроллеры обычно оставляют phase пустой | домен при необходимости (в SDK нет `MarkPlanning`) |
| `Planned` | **барьер 1**: объявленный набор детей/excluded заморожен и core может начать проекцию; subtree-gated агрегатор вправе объявить собственную manifest-ногу позже | `MarkPlanned` |
| `Finished` | **барьер 2**: домен закончил свою сторону (в т.ч. consistency-действия) | `ConfirmConsistent` |
| `Failed` | терминал: домен наткнулся на неустранимую ошибку | `Fail` / `Reject` |

**TARGET (в этой ревизии не реализован):** `Planned` доказывает публикацию полного capture-плана,
включая собственный MCR узла и выбранный data plan. Новая фаза для этого не вводится.

Важны два свойства:

- **Прямая цепочка не регрессирует**, а `Failed` — **терминальный sink**. Домены зовут `MarkPlanned` на
  каждом reconcile перед переключением по outcome; монотонный guard гарантирует, что дошедший до `Finished`
  снапшот не откатится в `Planned`, а попавший в `Failed` там и останется. Нетерминальное состояние
  «жду X» **не должно** использовать `Fail`/`Reject` — оно остаётся в текущей фазе и сообщает причину через
  `ReportProgress` (только message), как Pod остаётся `Pending` с диагностикой.
- **SDK никогда не пишет conditions.** Единственное условие на снапшоте — core-owned `Ready`. Core зеркалит
  `phase=Failed` в `Ready=False` и является единственным писателем терминального `Ready` и на `SnapshotContent`,
  и на владеющем снапшоте. Домен **читает** `Ready` обратно (через адаптер и `CoreCaptureOutcome`) как канал
  ошибок.

`phase>=Planned` — текущий projection-handoff: core уже может иметь eager-созданный и забайндженный
shell `SnapshotContent`, но ждёт барьер 1 перед проекцией capture-ног, child edges, latch-ей и
агрегированного статуса.

## Где лежит контракт (карта интерфейсов)

Весь публичный контракт — в модуле `pkg/snapshotsdk`:

| Файл | Тип | Кто реализует |
|---|---|---|
| `capture.go` | `CaptureSDK` (= `Planning` + `CaptureBarrier` + `CaptureFault` + `CaptureProgress` + `SourcePublisher` + `ManifestExclude` + `CaptureInspection`) | **SDK** (ты вызываешь) |
| `adapter.go` | `SnapshotAdapter` | **ты** (по одному на свой snapshot-тип) |
| `volumecapture.go` | `VolumeCaptureProvider` | SDK по умолчанию (`NewStorageFoundationProvider`) |
| `types.go` | `ChildSpec`, `VolumeCaptureSpec`, `ManifestCaptureSpec`, `SourceRef`, `SnapshotSource`, `DomainCaptureState`, `FailSpec`, `CaptureOutcomeResult`, `ExcludedObjectRef` | DTO, передаёшь в глаголы / читаешь из них |

Интерфейсы объявлены **на стороне потребителя (consumer side)** — на *boundary*, то есть на **шве интеграции
(integration seam)** между доменным контроллером и доменно-нейтральным SDK, — а не свалены в один
`interfaces.go`. Это осознанно: layout кодирует архитектуру.

## Что делает доменный контроллер: решает четыре вещи + ведёт два барьера

Для каждого snapshot-узла доменный контроллер определяет:

- какие **manifest-таргеты** сохранить → `EnsureManifestCapture`;
- нужно ли сохранять **данные PVC** (0 или 1) → `EnsureVolumeCapture`;
- какие нужны **дочерние снапшоты** (0..N) и какие объекты-источники **исключены** → `EnsureChildren`;
- захваченный **source** для публикации → `PublishSnapshotSource`.

Контроллер выражает намерение по каждой из них, объявляет барьер 1, затем переключается по core-outcome, чтобы
объявить барьер 2 (или остановиться):

1. **Дочерние снапшоты** (`EnsureChildren`) — например VM-снапшот владеет снапшотами своих дисков.
2. **Захват данных** (`EnsureVolumeCapture`) — захват содержимого **одного** PVC (см. раздел про `DataRef`).
3. **Захват манифестов** (`EnsureManifestCapture`) — захват manifest-таргетов, которые домен объявил для этого
   узла. Manifest- и data-ноги — **независимые декларации**: если домен также снимает данные PVC и хочет
   сохранить YAML этого PVC, он **явно** перечисляет PVC в manifest-таргетах. SDK никогда не выводит
   manifest-таргеты из data-ноги.
4. **Барьер 1** (`MarkPlanned`) — в текущей реализации замораживает membership children/excluded и
   разрешает core-проекцию в возможно уже забайндженный shell `SnapshotContent`.
5. **Барьер 2** (`ConfirmConsistent`) — объявляется, когда `CoreCaptureOutcome` сообщает, что все ноги
   захвачены, после любого consistency-действия (например fs unfreeze).

---

## Шаг 1 — реализовать `SnapshotAdapter` для своего CRD

### Что это такое

`SnapshotAdapter` — это **domain-specific adapter type**: небольшая обёртка (wrapper) над структурой твоего
snapshot-CR. Это обычный Go-struct в пакете твоего контроллера, реализующий интерфейс `SnapshotAdapter`; сам
SDK его не предоставляет. Технически — value-обёртка над указателем на твой снапшот, на которой висят методы
маппинга. Имя любое; в demo это `demoVirtualDiskSnapshotAdapter`:

```go
type myDomainSnapshotAdapter struct {
	snap *MyDomainSnapshot
}
```

### Зачем он нужен и почему без него нельзя

Это **инверсия зависимости**. SDK (`pkg/snapshotsdk`) — отдельный доменно-нейтральный модуль; он
**не имеет права** импортировать `MyDomainSnapshot`. Если бы SDK писал
`s.Status.CaptureState.DomainSpecificController.VolumeCaptureRequestName` напрямую, ему пришлось бы
импортировать каждый доменный CRD — и он перестал бы быть generic. Адаптер разворачивает зависимость:
**домен зависит от SDK, а не наоборот.** SDK знает только интерфейс; маппинг «generic-понятие → конкретное
поле статуса» живёт в домене.

Почему не обходные пути:
- **Сырой `client.Object` + рефлексия/`unstructured`** — это запрещённая demo-контрактом «магия»:
  стрингли-типизированный доступ к `status.*`, падения в рантайме вместо компиляции.
- **Дженерик `New[T]`** — не помогает: дженерик всё равно не знает, *как* достать `sourceRef` или *куда*
  положить имя VCR; нужна функция-маппинг, т.е. тот же адаптер в другой форме.

### Интерфейс (что реализовать)

```go
type SnapshotAdapter interface {
	Object() client.Object                       // живой объект; SDK его рефрешит и патчит
	SourceRef() SourceRef                         // spec.sourceRef

	GetDomainCaptureState() DomainCaptureState    // status.captureState.domainSpecificController
	SetDomainCaptureState(DomainCaptureState)     //   (+ top-level status.childrenSnapshotRefs)

	GetSnapshotSource() *SnapshotSource           // top-level status.sourceRef (read; nil = не задан)
	SetSnapshotSource(*SnapshotSource)            // top-level status.sourceRef (write)

	CoreCaptureState() CoreCaptureState           // read-only handoff от core (commonController latch-и)

	ReadyStatus() metav1.ConditionStatus          // read-only core-written status.conditions[Ready]
	ReadyReason() string
	ReadyMessage() string
}
```

**Writer discipline.** SDK пишет ТОЛЬКО `status.captureState.domainSpecificController` (через
`Get/SetDomainCaptureState`), top-level `status.childrenSnapshotRefs` (через них же) и top-level
`status.sourceRef` (через `Get/SetSnapshotSource`). Он НИКОГДА не пишет условие `Ready` и НИКОГДА не пишет
core-owned `captureState.commonController` — только читает их (`CoreCaptureState`,
`ReadyStatus`/`ReadyReason`/`ReadyMessage`).

Правила контракта:
- **Без side effects.** Никаких API-вызовов в методах — только чтение/запись полей структуры. Все
  обращения к кластеру делает SDK.
- `Object()` возвращает **тот же указатель**, что читают/пишут остальные методы (то самое `s`).
- Это **единственное место**, где `client.Object` пересекает границу домен↔SDK. В теле `Reconcile` ты эти
  методы-маппинги напрямую **не зовёшь** — только глаголы намерения (`Ensure*` / `MarkPlanned` /
  `ConfirmConsistent` / `Fail` / `Reject` / `ReportProgress` / `PublishSnapshotSource`).

Образец 1:1 — `internal/controllers/demo/snapshot_adapter.go`.

## Шаг 2 — собрать `CaptureSDK` (один раз на reconciler)

```go
func (r *MySnapshotReconciler) capture() snapshotsdk.CaptureSDK {
	return snapshotsdk.New(r.Client, r.APIReader, snapshotsdk.NewStorageFoundationProvider(r.Client))
}
```

- `Client` — записи и кэш-чтения.
- `APIReader` — живой (некэшированный) reader; SDK использует его для TOCTOU-safe рефрешей latch-ей ног и
  замороженных фазы/набора детей.
- `VolumeCaptureProvider` — бэкенд захвата данных; по умолчанию storage-foundation `VolumeCaptureRequest`.

Опциональные зависимости передаются через `Option`-ы. **CURRENT (51eb6c2):** существующему агрегатору,
который использует persisted-subtree compatibility path, нужен subresource REST-клиент для
`SubtreeManifestIdentities` (см. текущий compatibility-раздел):

```go
snapshotsdk.New(r.Client, r.APIReader, provider, snapshotsdk.WithSubresourceREST(restClient))
```

Лист/родитель, не использующий эту capability, может его опустить.

## Шаг 3 — в `Reconcile`: обернуть объект в адаптер и звать глаголы по порядку

«Получить» адаптер = сконструировать литералом из объекта, который ты только что достал из кластера.
Никакой фабрики нет:

```go
func (r *MySnapshotReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	s := &MyDomainSnapshot{}
	if err := r.Client.Get(ctx, req.NamespacedName, s); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if s.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	// Import-режим: НЕТ планирования capture — живой source может отсутствовать. Доменное планирование
	// тривиально завершено (core материализует SnapshotContent из загруженных манифестов).
	if s.IsImportMode() {
		return ctrl.Result{}, nil
	}

	adapter := myDomainSnapshotAdapter{snap: s} // ← вот и весь "получить адаптер"
	sdk := r.capture()

	// 1. Валидация источника — твоя логика.
	//    Невалидный sourceRef → ТЕРМИНАЛ (Reject/Fail).
	if /* sourceRef invalid */ {
		return ctrl.Result{}, sdk.Reject(ctx, adapter, snapshotsdk.FailSpec{Reason: "InvalidSourceRef", Message: "..."})
	}
	//    Источник не найден → RECOVERABLE: сообщи прогресс и requeue (может ещё появиться).
	if /* source not found */ {
		if err := sdk.ReportProgress(ctx, adapter, "waiting for <source> to exist"); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: retry}, nil
	}

	// 2. Опубликовать захваченный живой source (top-level status.sourceRef; для import-recreation).
	if err := sdk.PublishSnapshotSource(ctx, adapter, snapshotsdk.SnapshotSource{ /* APIVersion, Kind, Name, Namespace, UID */ }); err != nil {
		return ctrl.Result{}, err
	}

	// 3. Дети (лист без детей → nil, nil). Соблюдай exclude-veto (PartitionExcluded).
	if err := sdk.EnsureChildren(ctx, adapter, childSpecs, excludedRefs); err != nil {
		if errors.Is(err, snapshotsdk.ErrChildrenSetFrozen) { // рост набора после Planned
			return ctrl.Result{}, sdk.Fail(ctx, adapter, snapshotsdk.Reason(storagev1alpha1.ReasonGraphPlanningFailed), err)
		}
		return ctrl.Result{}, err
	}

	// 4. Захват данных (нет PVC → DataRef: nil = no-op; PVC ещё нет → ReportProgress + requeue).
	if err := sdk.EnsureVolumeCapture(ctx, adapter, snapshotsdk.VolumeCaptureSpec{DataRef: dataRef}); err != nil {
		return ctrl.Result{}, err
	}

	// 5. Манифест (всегда минимум один target).
	if err := sdk.EnsureManifestCapture(ctx, adapter, snapshotsdk.ManifestCaptureSpec{Targets: manifestTargets}); err != nil {
		if errors.Is(err, snapshotsdk.ErrEmptyManifest) || errors.Is(err, snapshotsdk.ErrManifestTargetsDrift) {
			return ctrl.Result{}, sdk.Fail(ctx, adapter, snapshotsdk.Reason(storagev1alpha1.ReasonGraphPlanningFailed), err)
		}
		return ctrl.Result{}, err
	}

	// 6. Барьер 1 — в обычном leaf/simple-parent пути объявлены все ноги.
	if err := sdk.MarkPlanned(ctx, adapter); err != nil {
		return ctrl.Result{}, err
	}

	// 7. Барьер 2 — переключаемся по core-derived capture outcome.
	switch outcome := snapshotsdk.CoreCaptureOutcome(adapter); outcome.Outcome {
	case snapshotsdk.CaptureOutcomeCaptured:
		return ctrl.Result{}, sdk.ConfirmConsistent(ctx, adapter) // после любого consistency-действия (напр. fs unfreeze)
	case snapshotsdk.CaptureOutcomeFailed:
		return ctrl.Result{}, nil // терминальный Ready принадлежит core; домену нечего re-drive-ить
	default: // CaptureOutcomeCapturing: ждём
		return ctrl.Result{RequeueAfter: retry}, nil
	}
}
```

Обычный порядок для leaf/simple-parent: планирующие вызовы
(`EnsureChildren`/`EnsureVolumeCapture`/`EnsureManifestCapture`) **независимы** и
могут идти между собой в любом порядке. Каждый глагол зависит только от своего spec и никогда не читает
результат другой ноги, поэтому создаваемые запросы идентичны при любом порядке вызова — в частности,
`EnsureManifestCapture` строит MCR исключительно из своих объявленных `Targets` и не обращается к data-ноге
VCR. Барьер 1 (`MarkPlanned`) — после всех трёх планирующих вызовов; переключение по outcome барьера 2 — в
конце. Документированные терминальные сигналы планирования (`ErrChildrenSetFrozen`,
`ErrEmptyManifest`, `ErrManifestTargetsDrift`) классифицируй как показано ниже; остальные ошибки
возвращай вызывающему, чтобы reconcile повторился.

> **CURRENT compatibility-примечание — не reusable guidance.** Subtree-gated агрегатор в 51eb6c2
> вызывает `EnsureChildren`, затем `MarkPlanned`, чтобы заморозить набор детей и разрешить
> core-проекцию (content shells могли быть забайнджены eager), ждёт persisted
> `SubtreeManifestIdentities` и позже объявляет собственную manifest-ногу. POC-контроллер
> `DemoVirtualMachineSnapshot` показывает это только как migration input. Активный TARGET заменяет
> порядок на published plan identities и own MCR до `Planned`.

### Import-режим: короткое замыкание

`spec.mode: Import` полностью выключает снапшот из capture. Живой source (и его диски/PVC) на import могут
отсутствовать, поэтому доменный контроллер **не** делает никакого планирования capture — ни lookup source, ни
детей, ни MCR/VCR. Он выходит рано (`if s.IsImportMode() { return ctrl.Result{}, nil }`). Core-контроллер
материализует backing `SnapshotContent` из загруженных манифестов (реконструированный checkpoint) и data-ногу
из соответствующего import-а; доменное планирование для import-узла тривиально завершено.

---

## `manifestTargets` — какие манифесты попадут в один MCR

`EnsureManifestCapture(ctx, adapter, ManifestCaptureSpec{Targets: ...})` принимает **полный desired-set**
идентичностей manifest-таргетов (`apiVersion`/`kind`/`name`; namespace неявный, равен namespace снапшота),
которые доменный контроллер считает принадлежащими этому snapshot-узлу. SDK превращает этот список в **один**
`ManifestCaptureRequest` и публикует его имя в
`status.captureState.domainSpecificController.manifestCaptureRequestName`.

```go
manifestTargets := []snapshotsdk.ManifestTarget{{
	APIVersion: demov1alpha1.SchemeGroupVersion.String(),
	Kind:       "DemoVirtualDisk",
	Name:       source.Name,
}}
// Диск с data-ногой также перечисляет PVC, чей YAML он хочет захватить рядом с диском:
if dataRef != nil {
	manifestTargets = append(manifestTargets, snapshotsdk.ManifestTarget{
		APIVersion: dataRef.APIVersion,
		Kind:       dataRef.Kind,
		Name:       dataRef.Name,
	})
}
```

SDK не решает за домен, какие манифесты принадлежат узлу. Он отвечает только за transport-механику: создать/
проверить один MCR, проставить ownerRef, опубликовать его имя, сохранить restart-safe поведение. Он захватывает
**ровно** те таргеты, которые объявил домен, — и никогда не выводит и не подмешивает таргеты из data-ноги. PVC,
чей YAML нужно сохранить, домен перечисляет в `Targets` сам (см. disk-контроллер).

### Захват манифестов не может быть пустым (`ErrEmptyManifest`)

Каждый снапшот захватывает как минимум манифест собственного объекта-источника. Объявленный target-set обязан
быть **непустым**: SDK **не подставляет** сам снапшотируемый ресурс за тебя и **не** подмешивает PVC из
data-ноги. Пустой `Targets` возвращает `snapshotsdk.ErrEmptyManifest` **до** любой мутации кластера (MCR CRD
enforce-ит тот же инвариант через CEL как вторую линию защиты). Пустой набор — это баг планирования в домене,
а не временное состояние; рекомендуемая реакция — `sdk.Fail(GraphPlanningFailed)`. Suppression-защёлка
сильнее этого guard'а: как только ядро зафиксировало manifest-ногу captured, вызов — no-op (`nil`) при любом
входе — поздний пост-capture пересчёт, давший пустой набор, не должен провалить уже снятый снапшот.

### Захват манифестов — adopt-then-drift (`ErrManifestTargetsDrift`)

Снапшот — point-in-time, поэтому target-set у `ManifestCaptureRequest` — это **замороженный** план захвата.
`EnsureManifestCapture` работает по схеме **adopt-then-drift**: если MCR ещё нет — создаёт его и публикует имя;
если MCR уже есть — **усыновляет** его (идемпотентно публикует имя в status, `spec.targets` при этом никогда не
патчит) и только ПОСЛЕ этого, если данный reconcile объявляет **другой** набор (сравнение **множеств** по
`(apiVersion, kind, name)`; порядок и дубликаты не важны), **сигнализирует** `snapshotsdk.ErrManifestTargetsDrift`.
Drift — это **сигнал, а не решение**: имя уже опубликовано, значит нога установлена в любом случае, а что делать
дальше — решает **вызывающий**. Домен обычно реагирует `sdk.Fail(GraphPlanningFailed)`. **CURRENT
(51eb6c2):** namespace-root-агрегатор игнорирует сигнал, потому что пересчитывает подвижный набор по
живому namespace, и побеждает первый план. Активный TARGET усыновляет существующий committed root MCR
без повторного вычисления targets, поэтому текущее исключение нельзя использовать как guidance.
Иммутабельность `spec.targets` обеспечивает apiserver: CEL-правило перехода в CRD `ManifestCaptureRequest`
(`self.targets == oldSelf.targets`) отклоняет любое изменение — сам SDK таргеты никогда не патчит. Идентичная
повторная декларация — no-op.

```go
if err := sdk.EnsureManifestCapture(ctx, adapter, snapshotsdk.ManifestCaptureSpec{Targets: manifestTargets}); err != nil {
	if errors.Is(err, snapshotsdk.ErrManifestTargetsDrift) {
		return ctrl.Result{}, sdk.Fail(ctx, adapter, snapshotsdk.Reason(storagev1alpha1.ReasonGraphPlanningFailed), err)
	}
	return ctrl.Result{}, err
}
```

Сравниваемый набор — это ровно объявленные доменом `Targets`: manifest-нога не augment-ится из data-ноги,
поэтому data-backed PVC участвует в сравнении, только если домен сам объявил его в `Targets`.

Вызывающий, который хочет пропустить построение таргетов, когда нога уже установлена, гейтит вызов через
`snapshotsdk.ManifestCaptureNeeded(adapter)` — чистое чтение статуса, истинное тогда и только тогда, когда имя
MCR ещё не опубликовано **и** ядро ещё не залатчило manifest-ногу как captured. Namespace-root использует его,
чтобы не перечислять живой namespace заново, когда его MCR уже существует.

---

## `childSpecs` и `excludedRefs` — что это и как формировать

```go
EnsureChildren(ctx, adapter, desired []ChildSpec, excluded []ExcludedObjectRef) error
```

`EnsureChildren` принимает **желаемый набор** дочерних снапшотов **и** набор объектов-источников, которые домен
завето­вал при перечислении (exclude-veto). Оба публикуются одним патчем статуса: дети — в top-level
`status.childrenSnapshotRefs`, excluded — в `status.captureState.domainSpecificController.excludedRefs`.

### `ChildSpec`

```go
type ChildSpec struct {
	Object client.Object // полностью собранный доменом дочерний snapshot-CR
}
```

Это **child-builder seam**: домен сам конструирует дочерний объект целиком (kind, name,
`spec.sourceRef`, labels), а SDK берёт на себя только k8s-механику:

- проставляет на дочерний объект **controller ownerRef** родителя;
- делает **create-or-adopt** (создаёт, если нет; усыновляет существующий);
- выводит из GVK+name дочерний `SnapshotChildRef` и **аддитивно (union)** добавляет его в
  `status.childrenSnapshotRefs`.

SDK **никогда не сочиняет** доменные поля spec ребёнка — это делаешь только ты.

### Как формировать (пример: VM → снапшоты дисков)

Имя каждого ребёнка — детерминированное, через `snapshotsdk.ChildSnapshotName(parentSnapshotUID, sourceUID)`
(та же UID-схема, что у core), чтобы повторный reconcile не плодил дубликаты. Связность несут refs/ownerRefs,
которые пишет SDK, а не имя:

```go
kept, excluded := snapshotsdk.PartitionExcluded(ownedDisks) // соблюдаем state-snapshotter.deckhouse.io/exclude

children := make([]snapshotsdk.ChildSpec, 0, len(kept))
for _, o := range kept {
	disk := o.(*demov1alpha1.DemoVirtualDisk)
	children = append(children, snapshotsdk.ChildSpec{Object: &demov1alpha1.DemoVirtualDiskSnapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: vm.Namespace, Name: snapshotsdk.ChildSnapshotName(vm.UID, disk.UID)},
		Spec: demov1alpha1.DemoVirtualDiskSnapshotSpec{
			SourceRef: &demov1alpha1.SnapshotSourceRef{Kind: "DemoVirtualDisk", Name: disk.Name},
		},
	}})
}

excludedRefs := make([]snapshotsdk.ExcludedObjectRef, 0, len(excluded))
for _, o := range excluded {
	excludedRefs = append(excludedRefs, snapshotsdk.ExcludedObjectRef{
		APIVersion: demov1alpha1.SchemeGroupVersion.String(), Kind: "DemoVirtualDisk", Name: o.GetName(),
	})
}
```

### Важные инварианты `EnsureChildren`

- **Аддитивная публикация (union), delete-free (SDK v1).** SDK создаёт/усыновляет и **union-ит** свежевыведенные
  refs в текущий опубликованный набор — он никогда не удаляет ref и не удаляет дочерний объект. Ребёнок,
  который эмиттер больше не перечисляет, сохраняет свой опубликованный ref; сам остаточный дочерний объект
  забирает ownerRef-GC или будущий cleanup-компонент, но не SDK. Поэтому `nil`/пустой desired-набор не публикует
  новых refs и оставляет текущий набор нетронутым.
- **Набор детей ЗАМОРОЖЕН на барьере 1 (`ErrChildrenSetFrozen`).** Как только узел на `phase>=Planned` (включая
  терминальный `Failed`), `EnsureChildren` отклоняет любую попытку **вырастить** объявленный набор (или изменить
  excluded-набор) с `snapshotsdk.ErrChildrenSetFrozen` — fail-closed и **до** создания любого дочернего CR, так
  что отклонённый вызов не имеет сайд-эффектов. Идемпотентная перепубликация того же набора (desired ⊆ published,
  excluded не изменился) остаётся no-op при любой фазе. Заморозка зеркалит иммутабельный
  `SnapshotContent.status.childrenSnapshotContentRefs`; поздно добавленное ребро навсегда заклинило бы узел,
  поэтому рекомендуемая реакция — `sdk.Fail(GraphPlanningFailed)`:
  ```go
  if err := sdk.EnsureChildren(ctx, adapter, childSpecs, excludedRefs); err != nil {
  	if errors.Is(err, snapshotsdk.ErrChildrenSetFrozen) {
  		return ctrl.Result{}, sdk.Fail(ctx, adapter, snapshotsdk.Reason(storagev1alpha1.ReasonGraphPlanningFailed), err)
  	}
  	// Recoverable (конфликт усыновления / транзиентная API-ошибка): requeue с backoff, фаза остаётся до Planned.
  	return ctrl.Result{}, err
  }
  ```
- Имена детей должны быть **детерминированными** (одно и то же имя на тот же логический ребёнок), иначе при
  повторном reconcile наплодишь дубликаты. Используй `snapshotsdk.ChildSnapshotName`.

Reference: `virtualmachinesnapshot_controller.go` (родитель с детьми).

---

## Exclude-veto

Лейбл `state-snapshotter.deckhouse.io/exclude` (`snapshotsdk.ExcludeLabelKey`) — **абсолютный, всегда активный**
veto: любой объект, несущий его (значение игнорируется), выпадает из каждого снапшота, на каждом уровне дерева,
независимо от `spec.resourceSelector` рута.

Core вкладывает veto в собственный резолв ресурсов, но **доменный энумератор видит только собранные им
child-спеки — не лейблы объектов-источников** — поэтому он ОБЯЗАН применить veto сам:

- вызови `snapshotsdk.PartitionExcluded(sourceObjs)` → `(kept, excluded)`;
- строй детей из `kept`;
- передай `excluded`-refs в `EnsureChildren` 4-м аргументом.

SDK публикует эти excluded-refs в `status.captureState.domainSpecificController.excludedRefs` (транзиентный
INPUT, который core сворачивает в durable `SnapshotContent.status.excludedRefs` и зеркалит в top-level
`status.excludedRefs`). Домен никогда не пишет durable-агрегат или top-level-зеркало. Передавай пустой/`nil`
excluded-набор, когда ничего не завето­вано, — data-лист, никогда не перечисляющий детей, так и делает.
Завето­ванный ребёнок не получает дочернего снапшота (и, значит, ни VCR, ни MCR); неполный образ принимается
by design (нет consistency-group-механики; этот trade-off берёт на себя оператор).

---

## `DataRef` — что это и почему это ровно один PVC

`EnsureVolumeCapture(ctx, adapter, VolumeCaptureSpec{DataRef: ...})` описывает **захват данных** — захват
содержимого одного PVC. `Target` — это идентификация PVC, которую домен сам разрезолвил:

```go
type Target struct {
	UID        string
	APIVersion string // "v1"
	Kind       string // "PersistentVolumeClaim"
	Name       string
	Namespace  string
}
```

Домен сам находит свой PVC и сам принимает решения о готовности. **Отсутствующий PVC — recoverable, не
терминал**: домен сообщает об этом через `ReportProgress` (только message, фаза сохраняется) и делает requeue
через `ctrl.Result` — он **не должен** входить в терминальный `Failed`-sink, иначе появившийся позже PVC
никогда не был бы захвачен. Из `DataRef` SDK создаёт storage-foundation `VolumeCaptureRequest` (VCR) и публикует
его имя в `status.captureState.domainSpecificController.volumeCaptureRequestName`. Это только data-нога — она
**не** питает manifest-ногу; если YAML этого PVC тоже нужно сохранить, домен перечисляет его в manifest
`Targets`.

### Инвариант: данные снапшота — это РОВНО ОДИН (опциональный) data ref

```
GOOD: один snapshot-узел = максимум один захват данных (один PVC)

VM Snapshot
 ├── Disk Snapshot A -> PVC A
 └── Disk Snapshot B -> PVC B

BAD: несколько PVC на одном узле (моделью не предусмотрено)

VM Snapshot
 ├── PVC A
 └── PVC B
```

**Один snapshot-узел = максимум один захват данных (один PVC).** Если у домена несколько PVC — это **не**
несколько `DataRef`, а несколько **дочерних** snapshot-узлов (каждый со своим единственным PVC).

Каноническая модель — **один логический захват данных на снапшот** (Variant A, cardinality ≤1; см.
`api/storage/v1alpha1` `SnapshotContent.status.data` — там тоже одна опциональная привязка).
Поэтому поле — единичный указатель, а не слайс:

```go
type VolumeCaptureSpec struct {
	DataRef *Target // один PVC, либо nil
}
```

- **`DataRef == nil`** → snapshot manifest-only: SDK не создаёт VCR и не публикует имя (явный no-op).
- **`DataRef != nil`** → обычный захват данных одного PVC.

`DataRef == nil` доступен домену, чей собственный CRD/CSD-контракт разрешает manifest-only узел.
Текущий served CRD POC `DemoVirtualDisk` требует PVC и отклоняет manifest-only disk на admission,
поэтому его недостижимая controller-ветка nil-PVC не является референсом этой SDK-возможности.

> Слайс `[]Target` сюда невозможен **by design**: тип сам запрещает «несколько захватов данных на один
> снапшот». Множественность PVC выражается только через дочерние узлы. В foundation-CRD тоже один
> target: unstructured-обёртка в `internal/storagefoundation` пишет единичный
> `VolumeCaptureRequest.spec.target`.

### Как формировать (пример: диск → его PVC)

```go
pvcName := source.Spec.PersistentVolumeClaimName
if pvcName == "" {
	return nil, "", nil // только если CRD/CSD этого домена явно разрешает manifest-only узел
}
pvc := &corev1.PersistentVolumeClaim{}
if err := reader.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: pvcName}, pvc); err != nil {
	if apierrors.IsNotFound(err) {
		// RECOVERABLE: PVC может появиться позже → верни «pending»-сообщение; вызывающий сообщит его через
		// ReportProgress и сделает requeue (НЕ Fail/Reject, НЕ ошибка).
		return nil, fmt.Sprintf("waiting for PersistentVolumeClaim %q to exist", pvcName), nil
	}
	return nil, "", err
}
dataRef := &snapshotsdk.Target{
	UID:        string(pvc.UID),
	APIVersion: corev1.SchemeGroupVersion.String(),
	Kind:       "PersistentVolumeClaim",
	Name:       pvc.Name,
	Namespace:  pvc.Namespace,
}
```

Reference: `virtualdisksnapshot_controller.go` (лист с захватом данных PVC).

---

## Публикация захваченного source (`status.sourceRef`)

`PublishSnapshotSource(ctx, adapter, SnapshotSource{...})` публикует полную ссылку на захваченный живой source
в top-level `status.sourceRef`. Она самодостаточна (`apiVersion`, `kind`, `name`, `namespace`, `uid`), поэтому
`d8`-cli читает её как единый блок, чтобы пересобрать import-source. В формулу readiness она **не** входит.
Публикуют её только доменные снапшоты, захватывающие живой source; nil/zero source — no-op.

```go
if err := sdk.PublishSnapshotSource(ctx, adapter, snapshotsdk.SnapshotSource{
	APIVersion: demov1alpha1.SchemeGroupVersion.String(),
	Kind:       "DemoVirtualDisk",
	Name:       source.Name,
	Namespace:  source.Namespace,
	UID:        source.UID,
}); err != nil {
	return ctrl.Result{}, err
}
```

---

## Барьер 2 — ожидание core через `CoreCaptureOutcome`

После барьера 1 core захватывает ноги и переключает per-leg success-latch-и на
`status.captureState.commonController` (`manifestCaptured`, `dataCaptured`; каждый — `*bool`: nil = такой ноги
нет, false = объявлена, но не захвачена, true = захвачена). Домен их никогда не пишет — он **читает** их через
`CoreCaptureOutcome`, который выводит tri-state из latch-ей плюс терминального `Ready`-reason:

```go
switch outcome := snapshotsdk.CoreCaptureOutcome(adapter); outcome.Outcome {
case snapshotsdk.CaptureOutcomeCaptured:
	// Все объявленные ноги захвачены и Ready не терминальный → подтверждаем consistency (барьер 2).
	return ctrl.Result{}, sdk.ConfirmConsistent(ctx, adapter)
case snapshotsdk.CaptureOutcomeFailed:
	// Core выставил терминальный Ready-reason (своя manifest/volume-нога или всплывший child-fail).
	// Домен НЕ re-drive-ит это в phase=Failed — превращение core-owned отказа ноги в терминал — работа core
	// (Variant A). Останавливаемся; requeue только крутил бы. outcome.Reason / outcome.Message несут детали.
	return ctrl.Result{}, nil
default: // CaptureOutcomeCapturing
	return ctrl.Result{RequeueAfter: retry}, nil
}
```

`CaptureOutcomeFailed` проверяется первым: терминальный `Ready`-reason (`IsReasonTerminal`) побеждает
success-latch-и (они success-only и никогда не выражают отказ).

## Барьер 2 у агрегатора — `childrenSettled` + `CoreCaptureOutcome`

Агрегатор (например VM, чьи дочерние диски каждый владеет data-ногой) объявляет барьер 2 не по грубому
rollup-у child `Ready`, а по core-derived исходу захвата **своего** узла и по латчу `childrenSettled` (все
прямые дети терминальны — captured-OK ЛИБО провалились). Единственный безопасный паттерн — свитч на
`CoreCaptureOutcome` (`Failed` проверяется ПЕРВЫМ), а действие согласованности гейтится на `childrenSettled`
ЛИБО на собственном freeze-deadline домена:

**CURRENT (51eb6c2):** core считает child terminal по `Ready=True`, terminal `Ready=False` либо
direct fallback `phase=Finished|Failed`. **TARGET (здесь не реализован):** `childrenSettled` читает
только Ready — `Ready=True` означает success, terminal `Ready=False` означает failure, а
absent/pending Ready не settled. Namespaced domain failure становится
`Ready=False/DomainCaptureFailed`; исходные domain reason и message встраиваются в
`Condition.Message`. Прямого phase fallback нет.

```go
switch outcome := snapshotsdk.CoreCaptureOutcome(adapter); outcome.Outcome {
case snapshotsdk.CaptureOutcomeFailed:
	// ПЕРВЫМ. Терминал (в т.ч. всплывший ChildrenFailed от упавшего ребёнка) принадлежит ядру — домен его
	// не re-drive-ит. Но остановка НЕ освобождает от КОМПЕНСАЦИИ действия согласованности: если ФС была
	// заморожена — unfreeze обязан отработать именно здесь (иначе заморозка утечёт), ветка Captured тут
	// недостижима.
	return ctrl.Result{}, r.thawIfFrozen(ctx, adapter) // компенсация
case snapshotsdk.CaptureOutcomeCaptured:
	// Все СВОИ ноги захвачены. Действие согласованности (напр. unfreeze гостевой ФС) гейтится на собственном
	// childrenSettled ЛИБО на domain freeze-deadline: у childrenSettled нет core-TTL, зависший ребёнок его
	// никогда не флипнет, поэтому живость закрывает domain-side deadline, а не ядро.
	settled := adapter.CoreCaptureState().ChildrenSettled
	if (settled == nil || !*settled) && !r.freezeDeadlineExceeded(adapter) {
		return ctrl.Result{RequeueAfter: retry}, nil // ждём, пока дети устаканятся
	}
	if err := r.thawIfFrozen(ctx, adapter); err != nil { // consistency-действие
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, sdk.ConfirmConsistent(ctx, adapter)
default: // CaptureOutcomeCapturing
	return ctrl.Result{RequeueAfter: retry}, nil
}
```

> **Почему домен всё равно читает `childrenSettled`, а не пересобирает его сам.** В текущей
> реализации ребёнок может упасть с free-form Ready reason вне `TerminalReadyReasons`; в TARGET core
> канонизирует такой отказ как `DomainCaptureFailed`. В обоих случаях классификацией и latch-ем
> владеет core. Домен использует `childrenSettled` вместе со своим freeze-deadline, а не поддерживает
> второй каталог terminality.

Для **тонкой** диагностики отдельных детей есть `ChildrenCaptureStates(ctx, adapter)`: резолвит объявленные
child-refs и возвращает для каждого его `Ready` status/reason/message и `AllLegsCaptured` (захвачены ли все
его объявленные ноги). Дети читаются как unstructured по GVK их ref-а (любой доменный child-kind); ещё не
найденный ребёнок — пустой `Ready`, `AllLegsCaptured=false`. Это read-only инспекция, НЕ замена гейта выше.

## CURRENT manifest-exclude compatibility — `SubtreeManifestIdentities`

В 51eb6c2 эта capability используется **агрегаторами**, чья собственная manifest-нога покрывает объекты, которые их
снапшоты-потомки уже захватывают (например namespace-root Snapshot, или VM, чьи дочерние диски захватывают
часть его объектов). Она строит MCR агрегатора как `EnsureManifestCapture(base − exclude)`, где exclude-набор —
всё, что поддерево уже захватило.

Сейчас эти идентичности появляются только после bind и персистирования content детей, поэтому текущий
порядок намеренно разделён: `EnsureChildren` → `MarkPlanned` → ожидание
`SubtreeManifestIdentities` → `EnsureManifestCapture(base − exclude)` → outcome барьера 2. Это
единственное текущее исключение из обычного правила «все Ensure до MarkPlanned»; оно предназначено
для замены, а не для повторного использования.

`SubtreeManifestIdentities(ctx, adapter)` возвращает union идентичностей объектов, захваченных по поддеревьям
ПРЯМЫХ детей узла. Ей нужен subresource REST-клиент (`WithSubresourceREST`; без него вызов вернёт ошибку
конфигурации). Она **fail-closed**: если любое поддерево не полностью персистировано или ребёнок ещё не забайндил
свой content, она возвращает `snapshotsdk.ErrSubtreeIdentitiesPending` и вызывающий делает requeue — частичный
exclude никогда не возвращается. Узел без детей возвращает пустой набор. Лист/родитель, не агрегирующий
перекрывающиеся манифесты, в этом вообще не нуждается.

Этот persisted-only порядок — **документация CURRENT реализации, а не рецепт для новых
агрегаторов**. Активный TARGET сохраняет внешнюю capability/endpoint, но читает опубликованные
manifest-планы до `Planned`; persisted MCP используется только как fallback после reap.

**Встроенный pre-gate.** Перед обращением к сабресурсу метод сверяется с полем
`CoreCaptureState().ChildSubtreesManifestsPersisted` **своего** узла — core-computed латчем «поддеревья ВСЕХ
объявленных прямых детей полностью персистированы» (children-only: манифесты самого узла в него НЕ входят,
поэтому он может стать `true` ещё до создания собственного MCR). Если он явно `false`, метод сразу возвращает
`ErrSubtreeIdentitiesPending` **без единого REST-вызова** — экономя обращения к эндпоинту и 409-requeue-циклы,
пока потомки ещё захватываются. `nil` (адаптер не мапит поле или оно ещё не вычислено) выключает pre-gate и
сохраняет прежнее поведение; корректность в любом случае держит fail-closed 409 сабресурса.

---

## Failure и progress-пути

- `Fail(ctx, adapter, reason, cause)` — быстрая терминальная форма: ставит `phase=Failed` с
  machine-readable `reason` и message из cause. Используй для нарушения доменного контракта: `ErrChildrenSetFrozen`
  / `ErrManifestTargetsDrift` / `ErrEmptyManifest` (рекомендуемый reason `GraphPlanningFailed`).
- `Reject(ctx, adapter, FailSpec{Reason, Message, Cause})` — структурная терминальная форма (например
  невалидный `sourceRef`). Тот же эффект: `phase=Failed`. Legacy-поле `FailSpec.Requeue` не действует
  и использоваться не должно; requeue-политикой владеет вызывающий.
- `ReportProgress(ctx, adapter, message)` — **нетерминальная**, только-message диагностика, записываемая в
  `status.captureState.domainSpecificController.message`. Она сохраняет фазу и reason и никогда не трогает
  `Ready`. Используй для recoverable-ожидания («жду PVC X») и продолжай requeue-ить; она идемпотентна, а пустой
  message очищает прежнюю диагностику. Она отказывается перезаписывать терминальный (`Failed`) объект.

Ключевое различие: `Fail`/`Reject` — **терминальны** (SDK никогда не покидает `Failed`), поэтому применяй их
только к действительно неустранимым ошибкам. Всё, что может разрешиться позже (ещё не появившийся source или
PVC), использует `ReportProgress` + requeue — Pod-модель: остаться `Pending` с диагностикой. Отказы core-owned
ног выставляет **core** на `Ready`; домен наблюдает их через `CoreCaptureOutcome=Failed` и просто
останавливается, он не re-drive-ит их в `phase=Failed`.

## Гарантии, на которые можно полагаться

- **Идемпотентность / restart-safe.** Любой `Ensure*` можно звать каждый reconcile; повторный вызов
  ничего не ломает и не плодит дубликаты (детерминированные имена VCR/MCR/детей).
- **Per-leg suppression по core-latch-ам.** Как только **core** переключает success-latch ноги на
  `captureState.commonController`, `Ensure*` этой ноги становится no-op: `EnsureVolumeCapture` подавляется, когда
  `dataCaptured` = true, а `EnsureManifestCapture` — когда `manifestCaptured` = true (так запрос, удалённый
  байндером после захвата, не пересоздаётся). Это **по ноге**, а не единый глобальный «после барьера всё
  заморожено» переключатель. У набора детей своя заморозка (`phase>=Planned`, `ErrChildrenSetFrozen`), а
  изменённый набор manifest-таргетов **сигнализируется** как drift (`ErrManifestTargetsDrift`) после усыновления
  имени MCR — саму иммутабельность `spec.targets` обеспечивает apiserver-CEL.
- **Граница domain/SDK.** Домен владеет: валидацией источника, планированием детей, exclude-veto, доменными
  объектами. SDK владеет: ownerRefs, оркестрацией capture, lifecycle запросов, доменными полями статуса и фазой
  жизненного цикла. **Core** владеет условием `Ready` и leg-latch-ами `commonController`.

## С чего начать практически

Возьми demo-реализацию как отправную точку и адаптируй под свой тип:
1. `internal/controllers/demo/snapshot_adapter.go` — адаптер;
2. `virtualdisksnapshot_controller.go` — текущий leaf-скелет.

Demo-контроллеры живут в `sds-unified-snapshots-poc`. Текущий
`virtualmachinesnapshot_controller.go` полезен только для понимания заменяемого split-barrier
поведения; не копируй этот aggregator ordering в новую интеграцию. Активный план обновляет
executable reference под TARGET-контракт.
