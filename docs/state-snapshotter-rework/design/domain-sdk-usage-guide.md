# Руководство: как доменной команде пользоваться snapshot SDK (capture)

> Статус: **developer-facing usage guide** для команд, интегрирующих свой домен со
> snapshot-контроллером через `pkg/snapshotsdk`. Это «как пользоваться», а не нормативный контракт.
> Нормативные источники: godoc в `pkg/snapshotsdk` (интерфейсы и инварианты) и
> [`domain-sdk-plan.md`](./domain-sdk-plan.md) (архитектурные решения Р*). Reference-реализация —
> demo-контроллеры в `images/domain-controller/internal/controllers/demo`.
>
> Скоуп SDK v1 — **capture-only** (планирование снапшота: дочерние снапшоты + захват данных + захват
> манифестов + два барьера фазы).
> Restore — отдельный sanctioned-boundary (`pkg/snapshotsdk/transform`), в этом гайде не рассматривается.

**В одном абзаце:** `snapshotsdk` позволяет доменному контроллеру **описать намерение снимка** (дочерние
снапшоты, опциональный data-PVC, манифесты), не реализуя оркестрацию снапшота. Домен решает, **что**
снимать; SDK решает, **как** управляются capture-запросы, ownership и restart-safe планирование, и публикует
результат в `status.captureState.domainSpecificController`. **`Ready` пишет не SDK, а core** — на каждом
snapshot-объекте; домен читает его обратно как свой канал ошибок через `CoreCaptureOutcome`.

## Жизненный цикл снапшота за одну минуту

1. Пользователь создаёт доменный snapshot-CR (`MySnapshot`).
2. Доменный контроллер валидирует объект-источник.
3. Контроллер решает три вещи:
   - какие **манифесты** сохранить;
   - нужно ли сохранять **данные PVC**;
   - какие нужны **дочерние снапшоты**.
4. Контроллер передаёт это в SDK и публикует ссылку на источник (`PublishSnapshotSource`).
5. SDK создаёт capture-запросы (MCR / VCR / дочерние снапшоты) и публикует их в доменный статус.
6. Контроллер ставит **барьер 1** — `MarkPlanned` (всё создано и опубликовано).
7. Core-контроллер материализует `SnapshotContent`, захватывает леги и защёлкивает
   `captureState.commonController`, затем выводит `Ready`.
8. Контроллер смотрит `CoreCaptureOutcome` и ставит **барьер 2** — `ConfirmConsistent` (после своих действий
   консистентности, напр. разморозки ФС) либо `Fail`/`Reject` при терминальной ошибке.

```
User
  |
  v
Domain Snapshot CR
  |
  v
Domain Controller
  |-- validate source
  |-- discover children
  |-- resolve PVC
  |-- choose manifests
  |
  v
Snapshot SDK (intent verbs)
  |-- PublishSnapshotSource
  |-- EnsureChildren / EnsureVolumeCapture / EnsureManifestCapture
  |-- MarkPlanned                 (barrier 1: phase=Planned)
  |
  v
Core snapshot controller
  |-- materializes SnapshotContent
  |-- captures legs -> captureState.commonController latches
  |-- derives Ready                (core-owned; SDK never writes it)
  |
  v
Domain Controller (barrier 2)
  |-- switch CoreCaptureOutcome:
  |     Captured  -> ConfirmConsistent (phase=Finished)
  |     Failed    -> Fail / Reject     (phase=Failed)
  |     Capturing -> wait / requeue
```

Это весь поток. Дальше в гайде — детали каждого шага.

## Что такое snapshot SDK и зачем он нужен

`pkg/snapshotsdk` — доменно-нейтральная библиотека, которая стандартизирует **capture-фазу** снапшота
(планирование: дочерние снапшоты + захват данных + захват манифестов + барьеры фазы). Доменная команда
**описывает намерение** снимка («что снимаем»), а оркестрацию («как разложить это в Kubernetes») берёт на
себя SDK.

До SDK каждая доменная команда была вынуждена реализовывать всё это сама:

- ownerRefs на capture-объектах;
- create/adopt capture-запросов;
- идемпотентность;
- восстановление после рестарта;
- lifecycle-фазу;
- optimistic-lock патчинг статуса.

Результат был предсказуемый: дублирование кода, расхождение (drift) поведения между доменами, тонкие race
conditions и несогласованная семантика снапшотов.

SDK введён, чтобы:

- стандартизировать lifecycle capture-запросов;
- убрать boilerplate;
- одинаково enforce-ить инварианты во всех доменах.

**SDK берёт на себя:**

- lifecycle capture-запросов (VCR/MCR/дочерних снапшотов);
- патчинг доменного статуса (optimistic-lock);
- два барьера фазы (`Planned` / `Finished`);
- restart-safe поведение;
- suppression по core-защёлкам (после захвата лега core-контроллером `Ensure*` становится no-op).

**Доменный контроллер оставляет у себя:**

- валидацию источника (`sourceRef`);
- discovery топологии (какие дочерние снапшоты нужны);
- резолв PVC для захвата данных;
- публикацию ссылки на источник (`PublishSnapshotSource`);
- доменные ошибки/причины (reasons) и решение о consistency-барьере;
- собственный reconcile-цикл (когда и как requeue).

## Модель статуса: `Ready` пишет core, а не SDK

Ключевое отличие от прежних версий: **SDK никогда не пишет condition `Ready`.** `Ready` выводит core на
каждом snapshot-объекте (единый агрегатор). SDK пишет только:

- `status.captureState.domainSpecificController` (имена MCR/VCR, `phase`, `reason`, `message`) —
  через `Get/SetDomainCaptureState`;
- `status.childrenSnapshotRefs` — там же;
- `status.snapshotSource` — через `Get/SetSnapshotSource`.

А core-написанное **читает** (никогда не пишет): `captureState.commonController` (защёлки легов) через
`CoreCaptureState`, и `conditions[Ready]` через `ReadyReason`/`ReadyMessage`.

Свой **канал ошибок** домен получает обратно из core через `CoreCaptureOutcome(t)` — три состояния:

- **`CaptureOutcomeCapturing`** — какой-то объявленный лег ещё не захвачен, терминального `Ready` нет → ждать;
- **`CaptureOutcomeCaptured`** — все объявленные леги захвачены, `Ready` не терминальный → `ConfirmConsistent`;
- **`CaptureOutcomeFailed`** — core выставил терминальную причину `Ready` (свои манифесты/данные или отказ
  ребёнка) → `Fail`/`Reject`.

## Два барьера фазы (planning barrier)

Барьеры — это durable-маркеры на `status.captureState.domainSpecificController.phase`
(`Planning|Planned|Finished|Failed`), а не runtime-примитивы синхронизации; они переживают рестарты.

- **Барьер 1 — `MarkPlanned()` → `phase=Planned`.** «Всё спланировано»: дочерние снапшоты созданы и
  опубликованы, MCR/VCR созданы и их имена опубликованы. Пока `phase < Planned`, core-контроллер **не
  трогает** `SnapshotContent`.
- **Барьер 2 — `ConfirmConsistent()` → `phase=Finished`.** «Домен закончил свою сторону», включая действия
  консистентности (например разморозку ФС после того, как снимки дисков реально сняты).
- **Отказ — `Fail()`/`Reject()` → `phase=Failed`** (+ `reason`/`message`). Core зеркалит `phase=Failed` в
  `Ready=False`.

Между барьерами ответственность за снапшот переходит доменный контроллер → core → (обратно к домену для
consistency-подтверждения).

## Где лежит контракт (карта интерфейсов)

Весь публичный контракт — в модуле `pkg/snapshotsdk`:

| Файл | Тип | Кто реализует |
|---|---|---|
| `capture.go` | `CaptureSDK` (= `Planning` + `CaptureBarrier` + `CaptureFault` + `SourcePublisher`) | **SDK** (ты вызываешь) |
| `capture.go` | `CoreCaptureOutcome(t)` — свободная функция (канал ошибок домена) | **SDK** (ты вызываешь) |
| `adapter.go` | `SnapshotAdapter` | **ты** (по одному на свой snapshot-тип) |
| `volumecapture.go` | `VolumeCaptureProvider` | SDK по умолчанию (`NewStorageFoundationProvider`) |
| `types.go` | `ChildSpec`, `VolumeCaptureSpec`, `ManifestCaptureSpec`, `FailSpec`, `SourceRef`, `SnapshotSource`, `DomainCaptureState`, `CoreCaptureState`, `CaptureOutcome*` | DTO, передаёшь/читаешь в глаголах |

Интерфейсы объявлены **на стороне потребителя (consumer side)** — на *boundary*, то есть на **шве интеграции
(integration seam)** между доменным контроллером и доменно-нейтральным SDK, — а не свалены в один
`interfaces.go`. Это осознанно: layout кодирует архитектуру.

## Что делает доменный контроллер: три вещи + источник + два барьера

Для каждого snapshot-узла доменный контроллер определяет три вещи:

- какие **манифесты** сохранить → `EnsureManifestCapture` (один базовый target — сам источник);
- нужно ли сохранять **данные PVC** (0 или 1) → `EnsureVolumeCapture`;
- какие нужны **дочерние снапшоты** (0..N) → `EnsureChildren`.

В канонической модели это `захват манифестов + захват данных одного PVC (0..1) + дочерние снапшоты (0..N)`.
Контроллер выражает намерение по каждой, публикует ссылку на источник и опускает барьеры:

1. **Публикация источника** (`PublishSnapshotSource`) — полная ссылка на снятый live-объект в
   `status.snapshotSource` (для import-mode восстановления; d8-cli читает её одним блоком).
2. **Дочерние снапшоты** (`EnsureChildren`) — например VM-снапшот владеет снапшотами своих дисков.
3. **Захват данных** (`EnsureVolumeCapture`) — захват содержимого **одного** PVC (см. раздел про `DataRef`).
4. **Захват манифестов** (`EnsureManifestCapture`) — захват манифеста источника (+ owned-PVC из захвата
   данных, который SDK подмешивает сам).
5. **Барьер 1** (`MarkPlanned`) — «всё спланировано».
6. **Барьер 2** (`ConfirmConsistent`) — после того как `CoreCaptureOutcome` вернул `Captured` и домен
   выполнил свои действия консистентности.

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
**не имеет права** импортировать `MyDomainSnapshot`. Если бы SDK писал `s.Status.VolumeCaptureRequestName`
напрямую, ему пришлось бы импортировать каждый доменный CRD — и он перестал бы быть generic. Адаптер
разворачивает зависимость: **домен зависит от SDK, а не наоборот.** SDK знает только интерфейс; маппинг
«generic-понятие → конкретное поле статуса» живёт в домене.

Почему не обходные пути:
- **Сырой `client.Object` + рефлексия/`unstructured`** — это запрещённая demo-контрактом «магия»:
  стрингли-типизированный доступ к `status.*`, падения в рантайме вместо компиляции.
- **Дженерик `New[T]`** — не помогает: дженерик всё равно не знает, *как* достать `sourceRef` или *куда*
  положить имя VCR; нужна функция-маппинг, т.е. тот же адаптер в другой форме.

### Интерфейс (что реализовать)

```go
type SnapshotAdapter interface {
	Object() client.Object                       // живой объект; SDK его патчит
	SourceRef() SourceRef                        // spec.sourceRef

	// доменная половина статуса, которую SDK ПИШЕТ:
	GetDomainCaptureState() DomainCaptureState    // captureState.domainSpecificController + childrenSnapshotRefs (read)
	SetDomainCaptureState(DomainCaptureState)     // (write)
	GetSnapshotSource() *SnapshotSource           // status.snapshotSource (read)
	SetSnapshotSource(*SnapshotSource)            // (write)

	// core-написанное, которое SDK только ЧИТАЕТ:
	CoreCaptureState() CoreCaptureState           // captureState.commonController (защёлки легов)
	ReadyReason() string                          // conditions[Ready].reason  (канал ошибок домена)
	ReadyMessage() string                         // conditions[Ready].message
}
```

Правила контракта:
- **Без side effects.** Никаких API-вызовов в методах — только чтение/запись полей структуры. Все
  обращения к кластеру делает SDK.
- `Object()` возвращает **тот же указатель**, что читают/пишут остальные методы (то самое `s`).
- **Writer discipline.** Адаптер даёт SDK писать **только** `domainSpecificController` +
  `childrenSnapshotRefs` + `snapshotSource`. `CoreCaptureState`/`ReadyReason`/`ReadyMessage` —
  **read-only**: SDK их не пишет (их владелец — core).
- В теле `Reconcile` ты эти методы напрямую **не зовёшь** — только `Ensure*`/`Mark*`/`Confirm*`/`Fail`/
  `Reject`/`PublishSnapshotSource` и свободную `CoreCaptureOutcome`.

Образец 1:1 — `images/domain-controller/internal/controllers/demo/snapshot_adapter.go` (там же — хелперы
`coreCaptureStateFrom`, `readyReason`/`readyMessage`, `ensureDomainSpecificController`).

## Шаг 2 — собрать `CaptureSDK` (один раз на reconciler)

```go
func (r *MySnapshotReconciler) capture() snapshotsdk.CaptureSDK {
	return snapshotsdk.New(r.Client, r.APIReader, snapshotsdk.NewStorageFoundationProvider(r.Client))
}
```

- `Client` — записи и кэш-чтения.
- `APIReader` — live-чтение без кэша: SDK им освежает snapshot перед проверкой захваченных маркеров
  (TOCTOU-safe suppression).
- `VolumeCaptureProvider` — бэкенд захвата данных; по умолчанию storage-foundation `VolumeCaptureRequest`.

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
	// Import mode: domain does NO capture planning — core материализует SnapshotContent из загруженных
	// манифестов. Домен тривиально «спланирован».
	if s.IsImportMode() {
		return ctrl.Result{}, nil
	}

	adapter := myDomainSnapshotAdapter{snap: s} // ← вот и весь "получить адаптер"
	sdk := r.capture()

	// 1. Валидация источника — твоя логика. Невалидно/не найдено → Reject (phase=Failed) и выходим.
	if /* source invalid / not found */ {
		return ctrl.Result{}, sdk.Reject(ctx, adapter, snapshotsdk.FailSpec{Reason: ..., Message: ...})
	}

	// 2. Публикация ссылки на источник (для import-mode восстановления; не входит в формулу Ready).
	if err := sdk.PublishSnapshotSource(ctx, adapter, snapshotsdk.SnapshotSource{...}); err != nil {
		return ctrl.Result{}, err
	}

	// 3. Дети (лист без детей → nil).
	if err := sdk.EnsureChildren(ctx, adapter, childSpecs); err != nil {
		if perr := sdk.Fail(ctx, adapter, snapshotsdk.Reason(storagev1alpha1.ReasonCreateChildFailed), err); perr != nil {
			return ctrl.Result{}, perr
		}
		return ctrl.Result{}, err
	}

	// 4. Захват данных (нет PVC → DataRef: nil = no-op; артефакта ещё нет → Reject{Requeue:true} + requeue).
	if err := sdk.EnsureVolumeCapture(ctx, adapter, snapshotsdk.VolumeCaptureSpec{DataRef: dataRef}); err != nil {
		return ctrl.Result{}, err
	}

	// 5. Манифест (всегда): один базовый target — сам источник; owned-PVC SDK подмешает сам.
	if err := sdk.EnsureManifestCapture(ctx, adapter, snapshotsdk.ManifestCaptureSpec{
		TargetAPIVersion: myGV.String(),
		TargetKind:       "MyResource",
		TargetName:       source.Name,
	}); err != nil {
		return ctrl.Result{}, err
	}

	// 6. Барьер 1.
	if err := sdk.MarkPlanned(ctx, adapter); err != nil {
		return ctrl.Result{}, err
	}

	// 7. Барьер 2 — по core-исходу.
	switch o := snapshotsdk.CoreCaptureOutcome(adapter); o.Outcome {
	case snapshotsdk.CaptureOutcomeCaptured:
		// (при необходимости — свои действия консистентности, напр. разморозка ФС)
		return ctrl.Result{}, sdk.ConfirmConsistent(ctx, adapter)
	case snapshotsdk.CaptureOutcomeFailed:
		return ctrl.Result{}, sdk.Reject(ctx, adapter, snapshotsdk.FailSpec{Reason: snapshotsdk.Reason(o.Reason), Message: o.Message})
	default: // CaptureOutcomeCapturing
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}
}
```

Порядок: планирующие вызовы (`EnsureChildren`/`EnsureVolumeCapture`/`EnsureManifestCapture`) — между собой в
любом порядке, но **`MarkPlanned` всегда после них**, а `CoreCaptureOutcome`-переключение — **после**
`MarkPlanned`. На ошибку из `Ensure*` — `return err`, reconcile повторится.

---

## `ManifestCaptureSpec` — какие манифесты попадут в MCR

`EnsureManifestCapture(ctx, adapter, ManifestCaptureSpec{...})` принимает **один базовый target** — идентити
самого снапшотируемого источника:

```go
snapshotsdk.ManifestCaptureSpec{
	TargetAPIVersion: demov1alpha1.SchemeGroupVersion.String(),
	TargetKind:       "DemoVirtualDisk",
	TargetName:       source.Name,
}
```

SDK превращает это в **один** `ManifestCaptureRequest` и сам подмешивает technical **owned-PVC target**,
обнаруженный из data-лега (`VolumeCaptureRequest`). Дедупликация по `(apiVersion|kind|namespace|name)` и
детерминированная сортировка — внутри SDK.

SDK отвечает только за transport-механику: создать (если ещё нет) один MCR, проставить ownerRef,
опубликовать `status.captureState.domainSpecificController.manifestCaptureRequestName`, сохранить
restart-safe поведение и подмешать owned-PVC target. Он **не решает** за домен, какой ресурс снимать — базовый
target обязан задать домен.

> **Suppression.** Как только core защёлкнул manifest-лег (`CoreCaptureState().ManifestCaptured == true`),
> `EnsureManifestCapture` становится no-op: SDK не пересоздаёт и не перепроверяет MCR. Это защёлка core, а не
> барьер фазы.

---

## `childSpecs` — что это и как формировать

`EnsureChildren(ctx, adapter, desired []ChildSpec)` принимает **желаемый набор** дочерних снапшотов и
делает так, чтобы кластер ему соответствовал.

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
- выводит из GVK+name дочерний `SnapshotChildRef` и публикует набор в
  `status.childrenSnapshotRefs` (через `Set/GetDomainCaptureState`).

SDK **никогда не сочиняет** доменные поля spec ребёнка — это делаешь только ты.

### Как формировать (пример: VM → снапшоты дисков)

```go
var childSpecs []snapshotsdk.ChildSpec
for _, diskName := range vm.Spec.DiskNames {
	child := &demov1alpha1.DemoVirtualDiskSnapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, Name: childSnapshotName(s, diskName)},
		Spec: demov1alpha1.DemoVirtualDiskSnapshotSpec{
			SourceRef: &demov1alpha1.SnapshotSourceRef{
				APIVersion: demov1alpha1.SchemeGroupVersion.String(),
				Kind:       "DemoVirtualDisk",
				Name:       diskName,
			},
		},
	}
	childSpecs = append(childSpecs, snapshotsdk.ChildSpec{Object: child})
}
```

### Важные инварианты `EnsureChildren`

- **Полный desired-set.** Передаёшь полный целевой набор детей каждый reconcile (а не инкремент). SDK
  публикует ровно то, что ты дал.
- **Delete-free (SDK v1, Р23).** SDK только создаёт/усыновляет и публикует refs. Он **не удаляет** дочерние
  объекты: ребёнок, которого больше нет в desired, просто **выпадает** из `status.childrenSnapshotRefs` и
  становится отсоединённым от графа; leftover-объект убирает ownerRef GC (родитель владеет каждым ребёнком)
  или будущий cleanup-компонент.
- **Fail-closed только на adoption-конфликт.** Если целевой объект уже принадлежит **другому** родителю, SDK
  его **не крадёт** — `EnsureChildren` возвращает ошибку и не трогает чужой объект.
- **Recompute-and-republish, без SDK-level drift-детекции.** В wave3 SDK **не** фризит топологию и **не**
  возвращает `ErrTopologyDrift`: он молча пересчитывает desired из живого состояния и переопубликует refs.
  Если между планированием и захватом набор членов изменился (член удалён/добавлен), это **не** валит снимок
  на уровне SDK. Detection drift, если он нужен, — **core-owned** (терминальная причина `Ready` в узких
  путях), а полноценная семантика частичного набора — открытый проектный вопрос (см. план `final_wave`,
  `fw-namespace-membership-drift`).
- **Детерминированные имена.** Имена детей должны быть **детерминированными** (одно и то же имя на тот же
  логический ребёнок), иначе при повторном reconcile наплодишь дубликаты.

Reference: `virtualmachinesnapshot_controller.go` (родитель с детьми): `planDemoVirtualMachineChildren` строит
`[]ChildSpec`, `EnsureChildren` их усыновляет; на ошибку — `Fail(ReasonCreateChildFailed)`.

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

Домен сам находит свой PVC и сам принимает решения о готовности (нет PVC → `Reject` с `ArtifactMissing` и
`Requeue: true`, а повторную проверку домен организует сам через `ctrl.Result{RequeueAfter: ...}`). SDK из
`DataRef` создаёт storage-foundation `VolumeCaptureRequest` (VCR), публикует его имя в
`status.captureState.domainSpecificController.volumeCaptureRequestName`, а позже подмешивает owned-PVC в
захват манифестов.

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
`api/storage/v1alpha1` `SnapshotContent.status.dataRef` — там тоже единичный указатель). У снапшота-узла может
быть **максимум один** data ref (один PVC) либо ни одного. Поэтому поле — единичный указатель, а не слайс:

```go
type VolumeCaptureSpec struct {
	DataRef *Target // один PVC, либо nil
}
```

- **`DataRef == nil`** → snapshot manifest-only: SDK не создаёт VCR и не публикует имя (явный no-op).
- **`DataRef != nil`** → обычный захват данных одного PVC.
- В demo `resolveDemoVirtualDiskDataRef` возвращает `*snapshotsdk.Target` (PVC) либо `nil`.

> **Suppression.** Как только core защёлкнул data-лег (`CoreCaptureState().DataCaptured == true`),
> `EnsureVolumeCapture` становится no-op.
>
> Слайс `[]Target` сюда невозможен **by design**: тип сам запрещает «несколько захватов данных на один
> снапшот». Множественность PVC выражается только через дочерние узлы.

### Как формировать (пример: диск → его PVC)

```go
pvcName := source.Spec.PersistentVolumeClaimName
if pvcName == "" {
	return nil, "", "", nil // manifest-only диск: DataRef остаётся nil
}
pvc := &corev1.PersistentVolumeClaim{}
if err := reader.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: pvcName}, pvc); err != nil {
	if apierrors.IsNotFound(err) {
		// артефакт может появиться позже → Reject{Reason: ArtifactMissing, Requeue: true},
		// а requeue делает контроллер через ctrl.Result{RequeueAfter: ...}
		return nil, storagev1alpha1.ReasonArtifactMissing, "PVC not found for disk data leg", nil
	}
	return nil, "", "", err
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

## Failure / not-ready пути

Единый канал отказа — **фаза `Failed`** на `captureState.domainSpecificController`; core зеркалит её в
`Ready=False` (пользователь смотрит только в `Ready`).

- `Fail(ctx, adapter, reason, cause)` — **быстрая форма**: `phase=Failed` + `reason` + `message` из `cause`.
  Удобно для «не смог создать детей / спланировать» (`Fail(ReasonCreateChildFailed, err)`).
- `Reject(ctx, adapter, FailSpec{Reason, Message, Cause, Requeue})` — **структурированная форма** того же
  исхода. Используется для невалидного/ненайденного источника и ещё не появившегося артефакта
  (`Reason: ArtifactMissing, Requeue: true`).

`FailSpec.Requeue` — это **подсказка домену**, а не управление reconcile-циклом: SDK не делает requeue сам,
его организует контроллер своим `ctrl.Result`. Терминальность/повтор определяет домен.

Отказы, пришедшие **от core** (свои манифесты/данные упали или упал ребёнок), домен получает не как ошибку
`Ensure*`, а через `CoreCaptureOutcome(adapter).Outcome == CaptureOutcomeFailed` — и переотражает их своим
`Reject(FailSpec{Reason: o.Reason, Message: o.Message})`.

## Барьер 2 и `CoreCaptureOutcome` (consistency)

После `MarkPlanned` домен на каждом reconcile переключается по `CoreCaptureOutcome(adapter)`:

```go
switch o := snapshotsdk.CoreCaptureOutcome(adapter); o.Outcome {
case snapshotsdk.CaptureOutcomeCaptured:
	// все объявленные леги ЭТОГО узла захвачены; Ready не терминальный.
	// Здесь домен делает свои consistency-действия (напр. разморозку ФС) и подтверждает:
	return ctrl.Result{}, sdk.ConfirmConsistent(ctx, adapter)
case snapshotsdk.CaptureOutcomeFailed:
	return ctrl.Result{}, sdk.Reject(ctx, adapter, snapshotsdk.FailSpec{Reason: snapshotsdk.Reason(o.Reason), Message: o.Message})
default: // Capturing
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}
```

Агрегатор (например VM-снапшот) может подождать **не только свой лег, но и захват данных всех детей**, прежде
чем `ConfirmConsistent`: в demo VM читает fine-grained per-child защёлку `dataCaptured` (через
`CoreCaptureState().AllLegsCaptured()` на каждом ребёнке), а не грубый child-Ready rollup — это позволяет
точно тайминговать разморозку ФС по факту снятия дисков. См. `allChildrenCaptured` в
`virtualmachinesnapshot_controller.go`.

## Гарантии, на которые можно полагаться

- **Идемпотентность / restart-safe.** Любой `Ensure*` можно звать каждый reconcile; повторный вызов
  ничего не ломает и не плодит дубликаты (детерминированные имена VCR/MCR/детей; refresh через APIReader
  для TOCTOU-safe проверки захваченных маркеров).
- **Suppression по core-защёлкам.** После того как core защёлкнул лег в `captureState.commonController`,
  соответствующий `Ensure*` становится no-op (SDK не пересоздаёт запрос, даже если его удалили по TTL после
  durable-хэндоффа).
- **`Ready` — core-owned.** SDK его не пишет; домен читает его как канал ошибок через `CoreCaptureOutcome`.
- **Граница domain/SDK.** Домен владеет: валидацией источника, планированием детей, доменными объектами,
  reconcile-циклом и consistency-решением. SDK владеет: ownerRefs, оркестрацией capture, lifecycle запросов,
  публикацией доменного статуса/фазы.

## С чего начать практически

Возьми demo-реализацию как отправную точку и адаптируй под свой тип:
1. `images/domain-controller/internal/controllers/demo/snapshot_adapter.go` — адаптер;
2. `images/domain-controller/internal/controllers/demo/virtualdisksnapshot_controller.go` (лист с захватом
   данных PVC) **или**
   `images/domain-controller/internal/controllers/demo/virtualmachinesnapshot_controller.go` (родитель с
   детьми, manifest-only + consistency-барьер по детям) — reconcile-скелет.

Это и есть reference-реализация: demo-контроллеры намеренно держатся как executable-документация SDK.
