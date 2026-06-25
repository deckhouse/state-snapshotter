# Руководство: как доменной команде пользоваться snapshot SDK (capture)

> Статус: **developer-facing usage guide** для команд, интегрирующих свой домен со
> snapshot-контроллером через `pkg/snapshotsdk`. Это «как пользоваться», а не нормативный контракт.
> Нормативные источники: godoc в `pkg/snapshotsdk` (интерфейсы и инварианты) и
> [`domain-sdk-plan.md`](./domain-sdk-plan.md) (архитектурные решения Р*). Reference-реализация —
> demo-контроллеры в `images/domain-controller/internal/controllers/demo`.
>
> Скоуп SDK v1 — **capture-only** (планирование снапшота: дети + данные + манифест + барьер).
> Restore — отдельный sanctioned-boundary (`pkg/snapshotsdk/transform`), в этом гайде не рассматривается.

## TL;DR — что вообще от тебя требуется

Чтобы научить **свой** snapshot-CR сниматься, тебе нужно сделать ровно две вещи:

1. **Один раз** написать тонкий адаптер своего типа под интерфейс `snapshotsdk.SnapshotAdapter`
   (маппинг «generic-поле → твоё поле статуса», без side effects).
2. В `Reconcile` обернуть объект в этот адаптер и вызвать intent-глаголы SDK в фиксированном порядке.

Всю wire-механику (ownerRefs, создание capture-requests, optimistic-lock patch статуса, имя условия
барьера, idempotency, подавление повторной работы) делает SDK. Домен решает только **что** его источник,
**какие** дети и **какой** PVC образует линию данных.

## Где лежит контракт (карта интерфейсов)

Весь публичный контракт — в модуле `pkg/snapshotsdk`:

| Файл | Тип | Кто реализует |
|---|---|---|
| `capture.go` | `CaptureSDK` (= `Planning` + `PlanningBarrier` + `ReadinessFault`) | **SDK** (ты вызываешь) |
| `adapter.go` | `SnapshotAdapter` | **ты** (по одному на свой snapshot-тип) |
| `volumecapture.go` | `VolumeCaptureProvider` | SDK по умолчанию (`NewStorageFoundationProvider`) |
| `types.go` | `ChildSpec`, `VolumeCaptureSpec`, `ManifestCaptureSpec`, `NotReadySpec`, `SourceRef`, `DomainCaptureState`, `CoreCaptureState` | DTO, передаёшь в глаголы |

Интерфейсы объявлены **на стороне потребителя / на границе** (а не свалены в один `interfaces.go`) — это
осознанно: layout кодирует архитектуру.

## Что делает доменный контроллер: три линии планирования + барьер

Снапшот в канонической модели = **манифест + одна логическая линия данных + дети**. Доменный reconcile
выражает намерение для каждой линии и в конце опускает барьер:

1. **Дети** (`EnsureChildren`) — дочерние снапшоты (например VM-снапшот владеет снапшотами своих дисков).
2. **Линия данных** (`EnsureVolumeCapture`) — захват содержимого **одного** PVC (см. раздел про `DataRef`).
3. **Манифест** (`EnsureManifestCapture`) — захват манифеста источника (+ owned-PVC из линии данных).
4. **Барьер** (`MarkPlanningReady`) — «всё спланировано»; core-контроллер ждёт именно его, прежде чем
   забрать `SnapshotContent`.

---

## Шаг 1 — реализовать `SnapshotAdapter` для своего CRD

### Что это такое

`SnapshotAdapter` — **твой собственный тип**, который ты объявляешь в своём пакете. SDK его не даёт. Это
value-обёртка над указателем на твой снапшот, на которой висят методы маппинга. Имя любое; в demo это
`demoVirtualDiskSnapshotAdapter`:

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
	Object() client.Object                 // живой объект; SDK его рефрешит и патчит
	SourceRef() SourceRef                  // spec.sourceRef
	GetConditions() []metav1.Condition     // status.conditions (read)
	SetConditions([]metav1.Condition)      // status.conditions (write)
	GetDomainCaptureState() DomainCaptureState // durable-результат планирования (read)
	SetDomainCaptureState(DomainCaptureState)  // durable-результат планирования (write)
	CoreCaptureState() CoreCaptureState    // read-only handoff от core (SDK сам не пишет)
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
	return snapshotsdk.New(r.Client, r.APIReader, snapshotsdk.NewStorageFoundationProvider(r.Client))
}
```

- `Client` — записи и кэш-чтения.
- `APIReader` — некэшированное чтение для TOCTOU-safe обновления core-маркеров (`CoreCaptureState`).
- `VolumeCaptureProvider` — бэкенд линии данных; по умолчанию storage-foundation `VolumeCaptureRequest`.

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
		return ctrl.Result{}, sdk.MarkNotReady(ctx, adapter, snapshotsdk.NotReadySpec{Reason: ..., Message: ...})
	}

	// 2. Дети (лист без детей → nil).
	if err := sdk.EnsureChildren(ctx, adapter, childSpecs); err != nil {
		return ctrl.Result{}, err
	}

	// 3. Линия данных (нет PVC → DataRef: nil = no-op; артефакта ещё нет → MarkNotReady{Requeue:true}).
	if err := sdk.EnsureVolumeCapture(ctx, adapter, snapshotsdk.VolumeCaptureSpec{DataRef: dataRef}); err != nil {
		return ctrl.Result{}, err
	}

	// 4. Манифест (всегда).
	if err := sdk.EnsureManifestCapture(ctx, adapter, snapshotsdk.ManifestCaptureSpec{
		TargetAPIVersion: ..., TargetKind: ..., TargetName: ...,
	}); err != nil {
		return ctrl.Result{}, err
	}

	// 5. Барьер — последним.
	return ctrl.Result{}, sdk.MarkPlanningReady(ctx, adapter, "planning complete")
}
```

Порядок: планирующие линии (`EnsureChildren`/`EnsureVolumeCapture`/`EnsureManifestCapture`) — между собой в
любом порядке, но **`MarkPlanningReady` всегда последним**. На ошибку из любого `Ensure*` — просто
`return err`, reconcile повторится.

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

- проставляет на ребёнка **controller ownerRef** родителя;
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

- **Desired-set, а не инкремент.** Передаёшь полный целевой набор каждый reconcile; SDK приводит
  `status.childrenSnapshotRefs` к этому набору.
- **Delete-free (SDK v1, Р23).** SDK только создаёт/усыновляет и публикует refs. Он **не удаляет** детей.
  Ребёнок, которого больше нет в desired, **отвязывается** от графа (исчезает из refs), но остаётся в
  кластере — его подберёт ownerRef GC / будущий компонент очистки.
- `nil`/пустой набор — это валидно: публикует пустые refs (снапшот без детей). Для листа просто передавай
  `nil`.
- Имена детей должны быть **детерминированными** (одно и то же имя на тот же логический ребёнок), иначе
  при повторном reconcile наплодишь дубликаты.

Reference: `virtualmachinesnapshot_controller.go` (родитель с детьми).

---

## `DataRef` — что это и почему это ровно один PVC

`EnsureVolumeCapture(ctx, adapter, VolumeCaptureSpec{DataRef: ...})` описывает **линию данных** — захват
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
`ArtifactMissing` + requeue). SDK из `DataRef` создаёт storage-foundation `VolumeCaptureRequest` (VCR),
публикует его имя в `status.volumeCaptureRequestName`, а позже подмешивает owned-PVC в линию манифеста.

### Инвариант: линия данных — это РОВНО ОДИН (опциональный) data ref

Каноническая модель — **одна логическая линия данных на снапшот** (Variant A, cardinality ≤1; см.
`api/storage/v1alpha1` `SnapshotContent.status.dataRef` — там тоже `*SnapshotDataBinding`, единичный
указатель). У снапшота-узла может быть **максимум один** data ref (один PVC) либо ни одного. Несколько
разных data-артефактов на один узел моделью **не предусмотрены** — если у домена несколько дисков, это
несколько **дочерних** снапшотов (каждый со своей единственной линией данных), а не несколько ref у одного.

Поэтому поле — единичный указатель, а не слайс:

```go
type VolumeCaptureSpec struct {
	DataRef *Target // один PVC, либо nil
}
```

- **`DataRef == nil`** → snapshot manifest-only: SDK не создаёт VCR и не публикует имя (явный no-op).
- **`DataRef != nil`** → нормальная линия данных с одним PVC.
- В demo `resolveDemoVirtualDiskDataRef` возвращает `*snapshotsdk.Target` (PVC) либо `nil`.

> Слайс `[]Target` сюда невозможен **by design**: тип сам запрещает «несколько снимков данных на один
> снапшот». Множественность PVC выражается только через дочерние узлы. Единственное место, где список
> targets реально существует, — это unstructured-обёртка над foundation-CRD `VolumeCaptureRequest`
> (`spec.targets[]`) внутри `internal/storagefoundation`; SDK всегда кладёт туда ровно один элемент.

### Как формировать (пример: диск → его PVC)

```go
pvcName := source.Spec.PersistentVolumeClaimName
if pvcName == "" {
	// manifest-only диск: линии данных нет
	return nil // DataRef остаётся nil
}
pvc := &corev1.PersistentVolumeClaim{}
if err := reader.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: pvcName}, pvc); err != nil {
	if apierrors.IsNotFound(err) {
		// артефакт может появиться позже → surfaced Ready=False + requeue, НЕ ошибка
		return /* MarkNotReady{Reason: ArtifactMissing, Requeue: true} */
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

Reference: `virtualdisksnapshot_controller.go` (лист с линией данных).

---

## Failure / not-ready пути

- `MarkNotReady(ctx, adapter, NotReadySpec{Reason, Message, Cause, Requeue})` — публикует `Ready=False`
  (невалидный источник, ещё не появившийся артефакт). `Requeue: true` для «появится позже»; `false` —
  терминально-до-смены-spec.
- `MarkPlanningFailed(ctx, adapter, reason, cause)` — планирование заблокировано доменной причиной; пишет
  условие барьера в `False` (вместо `MarkPlanningReady`).
- Разница: `MarkNotReady` — про **источник/артефакт**; `MarkPlanningFailed` — про **сам барьер
  планирования**.

## Гарантии, на которые можно полагаться

- **Идемпотентность / restart-safe.** Любой `Ensure*` можно звать каждый reconcile; повторный вызов
  ничего не ломает и не плодит дубликаты (детерминированные имена VCR/MCR/детей).
- **Suppression по `CoreCaptureState`.** После того как core durably зафиксировал линию
  (`ManifestCaptured`/`DataCaptured`), соответствующий `Ensure*` становится no-op — SDK не пересоздаёт
  запросы. Для свежести SDK сам делает некэшированный refresh через `APIReader`.
- **Граница domain/SDK.** Домен владеет: валидацией источника, планированием детей, доменными объектами.
  SDK владеет: conditions, ownerRefs, оркестрацией capture, lifecycle запросов.

## С чего начать практически

Скопируй под свой тип:
1. `internal/controllers/demo/snapshot_adapter.go` — адаптер;
2. `virtualdisksnapshot_controller.go` (лист с линией данных) **или**
   `virtualmachinesnapshot_controller.go` (родитель с детьми, manifest-only) — reconcile-скелет.

Это и есть reference-реализация: demo-контроллеры намеренно держатся как executable-документация SDK.
