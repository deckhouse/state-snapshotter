> Язык: [English](./README.md) · **Русский**

# Руководство: как доменной команде пользоваться snapshot SDK (capture)

> Статус: **developer-facing usage guide** для команд, интегрирующих свой домен со
> snapshot-контроллером через `pkg/snapshotsdk`. Это «как пользоваться», а не нормативный контракт.
> Нормативные источники: godoc в `pkg/snapshotsdk` (интерфейсы и инварианты) и
> [`CLAUDE.md`](./CLAUDE.md) (контракт качества). Reference-реализация —
> demo-контроллеры в `images/domain-controller/internal/controllers/demo`.
>
> Скоуп SDK v1 — **capture-only** (планирование снапшота: дочерние снапшоты + захват данных + захват
> манифестов + барьер).
> Restore — отдельный sanctioned-boundary (`pkg/snapshotsdk/transform`), в этом гайде не рассматривается.

**В одном абзаце:** `snapshotsdk` позволяет доменному контроллеру **описать намерение снимка** (дочерние
снапшоты, опциональный data-PVC, манифесты), не реализуя оркестрацию снапшота. Домен решает, **что**
снимать; SDK решает, **как** управляются capture-запросы, conditions, ownership и restart-safe планирование.

## Жизненный цикл снапшота за одну минуту

1. Пользователь создаёт доменный snapshot-CR (`MySnapshot`).
2. Доменный контроллер валидирует объект-источник.
3. Контроллер решает три вещи:
   - какие **манифесты** сохранить;
   - нужно ли сохранять **данные PVC**;
   - какие нужны **дочерние снапшоты**.
4. Контроллер передаёт это в SDK.
5. SDK создаёт capture-запросы (MCR / VCR / дочерние снапшоты) и публикует статус.
6. Контроллер ставит planning-ready (барьер планирования).
7. Core-контроллер материализует `SnapshotContent`.

```
User
  |
  v
Domain Snapshot CR
  |
  v
Domain Controller
  |-- discover children
  |-- resolve PVC
  |-- choose manifests
  |
  v
Snapshot SDK
  |-- EnsureChildren
  |-- EnsureVolumeCapture
  |-- EnsureManifestCapture
  |
  v
Planning barrier = Ready
  |
  v
Core snapshot controller
  |
  v
SnapshotContent
```

Это весь поток. Дальше в гайде — детали каждого шага.

## Что такое snapshot SDK и зачем он нужен

`pkg/snapshotsdk` — доменно-нейтральная библиотека, которая стандартизирует **capture-фазу** снапшота
(планирование: дочерние снапшоты + захват данных + захват манифестов + барьер). Доменная команда
**описывает намерение** снимка («что снимаем»), а оркестрацию («как разложить это в Kubernetes») берёт на
себя SDK.

До SDK каждая доменная команда была вынуждена реализовывать всё это сама:

- ownerRefs на capture-объектах;
- status conditions;
- create/adopt capture-запросов;
- идемпотентность;
- восстановление после рестарта;
- семантику барьера планирования;
- optimistic-lock патчинг статуса.

Результат был предсказуемый: дублирование кода, расхождение (drift) поведения между доменами, тонкие race
conditions и несогласованная семантика снапшотов.

SDK введён, чтобы:

- стандартизировать lifecycle capture-запросов;
- убрать boilerplate;
- одинаково enforce-ить инварианты во всех доменах.

**SDK берёт на себя:**

- lifecycle capture-запросов (VCR/MCR/дочерних снапшотов);
- патчинг статуса (optimistic-lock);
- барьер планирования;
- restart-safe поведение;
- детектирование drift (дочерние снапшоты, захват данных, захват манифестов).

**Доменный контроллер оставляет у себя:**

- валидацию источника (`sourceRef`);
- discovery топологии (какие дочерние снапшоты нужны);
- резолв PVC для захвата данных;
- доменные ошибки/причины (reasons).

## TL;DR — что от тебя требуется

**Концептуально:** SDK позволяет доменной команде **описывать намерение снимка**, а не реализовывать его
оркестрацию.

**Практически** домен предоставляет всего четыре вещи:

- **адаптер** (`SnapshotAdapter`) — тонкую обёртку над твоим snapshot-CR;
- **топологию дочерних снапшотов** (child topology);
- **опциональный PVC** для захвата данных;
- **манифест-таргеты**.

Всё остальное (ownerRefs, создание capture-запросов, optimistic-lock patch статуса, имя условия барьера,
идемпотентность, restart-safe, drift) делает SDK.

## Что SDK снимает с твоего кода

Доменная команда **больше не реализует вручную**:

- управление ownerRef;
- именование capture-запросов;
- логику create-or-adopt;
- optimistic-lock патчинг статуса;
- обработку condition-барьера;
- проверки drift топологии;
- restart-safe реконсиляцию capture-запросов.

## Что такое «барьер планирования» (planning barrier)

Барьер планирования — это status-маркер, разделяющий две фазы:

- **фаза доменного планирования** — доменный контроллер решает, что снимать;
- **фаза core-обработки** — core-контроллер материализует `SnapshotContent`.

Когда вызван `MarkPlanningReady()`, ответственность за снапшот **переходит от доменного контроллера к
core-контроллеру**.

Технические детали (можно пропустить при первом чтении): барьер — это **durable** status-условие
(`ChildrenSnapshotReady`), а не runtime-примитив синхронизации; он переживает рестарты и поднимается ровно
одним вызовом `MarkPlanningReady`. Пока он не поднят, core-контроллер не трогает `SnapshotContent`.

## Где лежит контракт (карта интерфейсов)

Весь публичный контракт — в модуле `pkg/snapshotsdk`:

| Файл | Тип | Кто реализует |
|---|---|---|
| `capture.go` | `CaptureSDK` (= `Planning` + `PlanningBarrier` + `ReadinessFault`) | **SDK** (ты вызываешь) |
| `adapter.go` | `SnapshotAdapter` | **ты** (по одному на свой snapshot-тип) |
| `volumecapture.go` | `VolumeCaptureProvider` | SDK по умолчанию (`NewStorageFoundationProvider`) |
| `types.go` | `ChildSpec`, `VolumeCaptureSpec`, `ManifestCaptureSpec`, `NotReadyStatus`, `SourceRef`, `DomainCaptureState` | DTO, передаёшь в глаголы |

Интерфейсы объявлены **на стороне потребителя (consumer side)** — на *boundary*, то есть на **шве интеграции
(integration seam)** между доменным контроллером и доменно-нейтральным SDK, — а не свалены в один
`interfaces.go`. Это осознанно: layout кодирует архитектуру.

## Что делает доменный контроллер: решает три вещи + ставит барьер

Для каждого snapshot-узла доменный контроллер определяет три вещи:

- какие **манифесты** сохранить → `EnsureManifestCapture`;
- нужно ли сохранять **данные PVC** (0 или 1) → `EnsureVolumeCapture`;
- какие нужны **дочерние снапшоты** (0..N) → `EnsureChildren`.

В канонической модели это `захват манифестов + захват данных одного PVC (0..1) + дочерние снапшоты (0..N)`.
Контроллер выражает намерение по каждой из них и в конце опускает барьер:

1. **Дочерние снапшоты** (`EnsureChildren`) — например VM-снапшот владеет снапшотами своих дисков.
2. **Захват данных** (`EnsureVolumeCapture`) — захват содержимого **одного** PVC (см. раздел про `DataRef`).
3. **Захват манифестов** (`EnsureManifestCapture`) — захват манифеста источника (+ owned-PVC из захвата
   данных).
4. **Барьер** (`MarkPlanningReady`) — «всё спланировано»; core-контроллер ждёт именно его, прежде чем
   забрать `SnapshotContent`.

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
	Object() client.Object                 // живой объект; SDK его патчит
	SourceRef() SourceRef                  // spec.sourceRef
	GetConditions() []metav1.Condition     // status.conditions (read; барьер планирования)
	SetConditions([]metav1.Condition)      // status.conditions (write)
	GetDomainCaptureState() DomainCaptureState // durable-результат планирования (read)
	SetDomainCaptureState(DomainCaptureState)  // durable-результат планирования (write)
}
```

Правила контракта:
- **Без side effects.** Никаких API-вызовов в методах — только чтение/запись полей структуры. Все
  обращения к кластеру делает SDK.
- `Object()` возвращает **тот же указатель**, что читают/пишут остальные методы (то самое `s`).
- Это **единственное место**, где `client.Object` и `metav1.Condition` пересекают границу домен↔SDK. В
  теле `Reconcile` ты эти методы напрямую **не зовёшь** — только `Ensure*`/`Mark*`.

Образец 1:1 — `internal/controllers/demo/snapshot_adapter.go`.

## Шаг 2 — собрать `CaptureSDK` (один раз на reconciler)

```go
func (r *MySnapshotReconciler) capture() snapshotsdk.CaptureSDK {
	return snapshotsdk.New(r.Client, snapshotsdk.NewStorageFoundationProvider(r.Client))
}
```

- `Client` — записи и кэш-чтения.
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

	adapter := myDomainSnapshotAdapter{snap: s} // ← вот и весь "получить адаптер"
	sdk := r.capture()

	// 1. Валидация источника — твоя логика. Невалидно/не найдено → Ready=False и выходим.
	if /* source invalid / not found */ {
		return ctrl.Result{}, sdk.MarkNotReady(ctx, adapter, snapshotsdk.NotReadyStatus{Reason: ..., Message: ...})
	}

	// 2. Дети (лист без детей → nil).
	if err := sdk.EnsureChildren(ctx, adapter, childSpecs); err != nil {
		return ctrl.Result{}, err
	}

	// 3. Захват данных (нет PVC → DataRef: nil = no-op; артефакта ещё нет → MarkNotReady + requeue через
	//    ctrl.Result, см. ниже).
	if err := sdk.EnsureVolumeCapture(ctx, adapter, snapshotsdk.VolumeCaptureSpec{DataRef: dataRef}); err != nil {
		return ctrl.Result{}, err
	}

	// 4. Манифест (всегда).
	if err := sdk.EnsureManifestCapture(ctx, adapter, snapshotsdk.ManifestCaptureSpec{
		Targets: manifestTargets,
	}); err != nil {
		return ctrl.Result{}, err
	}

	// 5. Барьер — последним.
	return ctrl.Result{}, sdk.MarkPlanningReady(ctx, adapter, "planning complete")
}
```

Порядок: планирующие вызовы (`EnsureChildren`/`EnsureVolumeCapture`/`EnsureManifestCapture`) — между собой в
любом порядке, но **`MarkPlanningReady` всегда последним**. На ошибку из любого `Ensure*` — просто
`return err`, reconcile повторится (drift-ошибки дополнительно маппятся в `MarkPlanningFailed`, см. разделы
ниже).

---

## `manifestTargets` — какие манифесты попадут в один MCR

`EnsureManifestCapture(ctx, adapter, ManifestCaptureSpec{Targets: ...})` принимает **полный desired-set**
манифестных объектов, которые доменный контроллер считает принадлежащими этому snapshot-узлу. SDK превращает
этот список в **один** `ManifestCaptureRequest`.

```go
manifestTargets := []snapshotsdk.ManifestTarget{{
	APIVersion: demov1alpha1.SchemeGroupVersion.String(),
	Kind:       "DemoVirtualDisk",
	Name:       source.Name,
}}
```

Если домен решает, что вместе с основным source object нужно захватить дополнительные namespaced-объекты, он
добавляет их в этот же список. После capture эти объекты окажутся в MCP этого узла, и root/parent capture
сможет исключить их из своего MCR через существующий subtree exclude-механизм.

SDK не решает за домен, какие доменные манифесты принадлежат узлу. Он отвечает только за transport-механику:
создать/проверить один MCR, проставить ownerRef, опубликовать `status.manifestCaptureRequestName`,
сохранить restart-safe поведение и при необходимости добавить technical owned-PVC target из захвата данных.

### Захват манифестов — fail-closed на drift (симметрично детям/данным)

После первой публикации MCR его target-set **иммутабелен** так же, как топология детей и data-слот. Если на
последующем reconcile desired target-set разъехался с уже опубликованным MCR (сравнение **множеств** по
`(apiVersion, kind, name)`; порядок и дубликаты не важны), `EnsureManifestCapture` возвращает
`snapshotsdk.ErrManifestDrift`: он **не** патчит/не пересоздаёт/не удаляет MCR и не трогает status. Домен
публикует исход через `MarkPlanningFailed(ReasonManifestDrift)`:

```go
if err := sdk.EnsureManifestCapture(ctx, adapter, snapshotsdk.ManifestCaptureSpec{Targets: manifestTargets}); err != nil {
	if errors.Is(err, snapshotsdk.ErrManifestDrift) {
		_ = sdk.MarkPlanningFailed(ctx, adapter, snapshotsdk.Reason(storagev1alpha1.ReasonManifestDrift), err)
	}
	return ctrl.Result{}, err
}
```

Технический owned-PVC target (из захвата данных) добавляется в desired-set **до** сравнения, поэтому он не
даёт false-positive drift.

> ⚠️ **Захват манифестов не может быть пустым.** Финальный target-set (твои таргеты + augmentation owned-PVC
> из захвата данных) обязан содержать **минимум один** target. Нюансы:
> - домен **может** передать пустой начальный набор `Targets`;
> - owned-PVC augmentation **может** сделать финальный набор валидным (≥1) даже при пустом входе;
> - SDK проверяет именно **финальный** набор; если он пуст — `EnsureManifestCapture` возвращает
>   `snapshotsdk.ErrEmptyManifest` **до** любых обращений к кластеру (MCR не создаётся, status не трогается).
>
> SDK **не подставляет** сам снапшотируемый ресурс за тебя — передать хотя бы один manifest target (как
> минимум сам ресурс) обязан домен. Пустой `ErrEmptyManifest` — это сигнал бага планирования в контроллере,
> а не временное состояние.

---

## `childSpecs` — что это и как формировать

`EnsureChildren(ctx, adapter, desired []ChildSpec)` принимает **желаемый набор** дочерних снапшотов.

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
			SourceRef: demov1alpha1.SourceRef{Kind: "DemoVirtualDisk", Name: diskName},
		},
	}
	childSpecs = append(childSpecs, snapshotsdk.ChildSpec{Object: child})
}
```

### Важные инварианты `EnsureChildren`

- **Полный desired-set.** Передаёшь полный целевой набор детей каждый reconcile (а не инкремент). Дубликат
  в наборе (две спеки с одним `(apiVersion, kind, name)`) — ошибка планирования, `EnsureChildren` упадёт
  (это не drift).
- **Delete-free (SDK v1, Р23).** SDK только создаёт/усыновляет и публикует refs. Он **не удаляет** дочерние
  объекты.
- **Топология иммутабельна после коммита барьера (fail-closed).** Маркер коммита — `ChildrenSnapshotReady=
  True` (его ставит `MarkPlanningReady`), а не «refs непустые». **До** коммита набор ещё может сходиться к
  заново наблюдаемому desired. **После** коммита desired обязан **совпасть** с опубликованным по
  идентичности `(apiVersion, kind, name)` — сравнение **множеств, не длины** (`[A,B] → [A,C]` при равном
  count тоже расхождение; закоммиченный лист `[] → [A]` — тоже). Если набор разъехался (например, после
  рестарта discovery увидел другой набор), `EnsureChildren` возвращает `snapshotsdk.ErrTopologyDrift`,
  ничего не создаёт и не трогает опубликованные refs. Обрабатывай так:
  ```go
  if err := sdk.EnsureChildren(ctx, adapter, childSpecs); err != nil {
  	reason := snapshotsdk.Reason(storagev1alpha1.ReasonCreateChildFailed)
  	if errors.Is(err, snapshotsdk.ErrTopologyDrift) {
  		reason = snapshotsdk.Reason(storagev1alpha1.ReasonTopologyDrift)
  	}
  	_ = sdk.MarkPlanningFailed(ctx, adapter, reason, err)
  	return ctrl.Result{}, err
  }
  ```
- `nil`/пустой набор валиден до коммита (лист без дочерних снапшотов публикует пустые refs); после коммита
  непустого набора `nil` — это drift, как и появление нового дочернего объекта у закоммиченного пустого
  листа.
- Имена детей должны быть **детерминированными** (одно и то же имя на тот же логический ребёнок), иначе
  при повторном reconcile наплодишь дубликаты.

Reference: `virtualmachinesnapshot_controller.go` (родитель с детьми).

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

Домен сам находит свой PVC и сам принимает решения о готовности (нет PVC → `MarkNotReady` с
`ArtifactMissing`, а повторную проверку домен организует сам через `ctrl.Result`). SDK из `DataRef` создаёт
storage-foundation `VolumeCaptureRequest` (VCR), публикует его имя в `status.volumeCaptureRequestName`, а
позже подмешивает owned-PVC в захват манифестов.

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
`api/storage/v1alpha1` `SnapshotContent.status.dataRef` — там тоже `*SnapshotDataBinding`, единичный
указатель). У снапшота-узла может быть **максимум один** data ref (один PVC) либо ни одного. Несколько
разных data-артефактов на один узел моделью **не предусмотрены** — если у домена несколько дисков, это
несколько **дочерних** снапшотов (каждый со своим единственным PVC), а не несколько ref у одного.

Поэтому поле — единичный указатель, а не слайс:

```go
type VolumeCaptureSpec struct {
	DataRef *Target // один PVC, либо nil
}
```

- **`DataRef == nil`** → snapshot manifest-only: SDK не создаёт VCR и не публикует имя (явный no-op).
- **`DataRef != nil`** → обычный захват данных одного PVC.
- В demo `resolveDemoVirtualDiskDataRef` возвращает `*snapshotsdk.Target` (PVC) либо `nil`.

> Слайс `[]Target` сюда невозможен **by design**: тип сам запрещает «несколько захватов данных на один
> снапшот». Множественность PVC выражается только через дочерние узлы. Единственное место, где список
> targets реально существует, — это unstructured-обёртка над foundation-CRD `VolumeCaptureRequest`
> (`spec.targets[]`) внутри `internal/storagefoundation`; SDK всегда кладёт туда ровно один элемент.

### Как формировать (пример: диск → его PVC)

```go
pvcName := source.Spec.PersistentVolumeClaimName
if pvcName == "" {
	// manifest-only диск: данные не снимаются
	return nil // DataRef остаётся nil
}
pvc := &corev1.PersistentVolumeClaim{}
if err := reader.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: pvcName}, pvc); err != nil {
	if apierrors.IsNotFound(err) {
		// артефакт может появиться позже → surfaced Ready=False (через MarkNotReady), а requeue делает
		// контроллер через ctrl.Result{RequeueAfter: ...}, НЕ ошибка
		return /* MarkNotReady{Reason: ArtifactMissing} + ctrl.Result{RequeueAfter: ...} */
	}
	return err
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

- `MarkNotReady(ctx, adapter, NotReadyStatus{Reason, Message, Cause})` — публикует `Ready=False`
  (невалидный источник, ещё не появившийся артефакт). SDK только публикует условие; нужен ли повтор и когда
  — решает контроллер своим `ctrl.Result` (SDK не управляет reconcile-циклом).
- `MarkPlanningFailed(ctx, adapter, reason, cause)` — планирование заблокировано (доменной причиной,
  `ReasonTopologyDrift` при расхождении топологии детей или `ReasonManifestDrift` при расхождении
  manifest-таргетов); пишет условие барьера в `False` (вместо `MarkPlanningReady`).
- Разница: `MarkNotReady` — про **источник/артефакт**; `MarkPlanningFailed` — про **сам барьер
  планирования**.

## Гарантии, на которые можно полагаться

- **Идемпотентность / restart-safe.** Любой `Ensure*` можно звать каждый reconcile; повторный вызов
  ничего не ломает и не плодит дубликаты (детерминированные имена VCR/MCR/детей).
- **Suppression по барьеру планирования.** После того как ты вызвал `MarkPlanningReady`
  (`ChildrenSnapshotReady=True`), каждый `Ensure*` становится no-op — SDK ничего не создаёт, не переиспользует
  и не валидирует (фаза планирования зафиксирована, владение перешло core-контроллеру). Если запрос потом
  удалят (например, по TTL после durable-хэндоффа), SDK его **не** пересоздаёт. До барьера действует
  per-artifact иммутабельность: уже опубликованный артефакт, разошедшийся с desired, — это drift
  (fail-closed), а не молчаливая перезапись.
- **Граница domain/SDK.** Домен владеет: валидацией источника, планированием детей, доменными объектами.
  SDK владеет: conditions, ownerRefs, оркестрацией capture, lifecycle запросов.

## С чего начать практически

Возьми demo-реализацию как отправную точку и адаптируй под свой тип:
1. `internal/controllers/demo/snapshot_adapter.go` — адаптер;
2. `virtualdisksnapshot_controller.go` (лист с захватом данных PVC) **или**
   `virtualmachinesnapshot_controller.go` (родитель с детьми, manifest-only) — reconcile-скелет.

Это и есть reference-реализация: demo-контроллеры намеренно держатся как executable-документация SDK.
