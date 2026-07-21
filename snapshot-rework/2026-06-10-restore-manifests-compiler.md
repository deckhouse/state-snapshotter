## Restore manifests compiler (`/manifests-with-data-restoration` rework)

- **Status:** Proposed (2026-06-10). Design/ADR draft. Code и тесты — после согласования.
- **Scope:** read-path API (`internal/api` + `internal/usecase`, `internal/usecase/restore`). Аггрегированное чтение манифестов snapshot-дерева и компиляция apply-ready манифестов для восстановления (в т.ч. в другой namespace).
- **Не трогает (capture-side):** материализацию дерева, `ManifestCheckpoint`/MCR/VCR, публикацию `dataRefs[]`/`children*Refs` — это уже описано в `spec/system-spec.md` §3 и ADR `2026-06-09-orphan-pvc-csi-volumesnapshot.md`. ADR — только про **чтение/компиляцию**.
- **Скоуп этого этапа (подтверждено):** логика создания снапшотов и построения дерева **не меняется**. Реализуется **только** генерация готовых (apply-ready) манифестов поверх **уже существующего** дерева/артефактов. Никаких новых полей в `dataRefs[]`/`children*Refs`, никаких изменений в контроллерах capture/graph. Компилятор только **читает** дерево, манифесты и существующие refs и собирает выдачу.
- **Зависимость (удовлетворена; не изменение):** orphan-PVC restore (PVC → `dataSourceRef` на VS) опирается на capture-поведение orphan-PVC (VS как `childrenSnapshotRefs` visibility-leaf + VSC в root `dataRefs[]` с `deletionPolicy=Retain`). Это **уже реализовано** в коде (`internal/controllers/snapshot/orphan_pvc_volume_snapshot.go`: `reconcileOrphanPVCVolumeSnapshotChildLeaves`, `PublishSnapshotContentDataRefs`, `ensureVolumeSnapshotContentRetain`; spec §3.9.11) и покрыто кластерным e2e (исторически — `hack/snapshot-tree-demo-e2e.sh`, стадии `orphan-pvc-vs`, `orphan-pvc-cleanup`, `referenced-pvc-without-disk-csd`; скрипт удалён вместе с выносом demo-домена — сценарии живут в e2e-наборе модуля `sds-unified-snapshots-poc`). Этот ADR capture не реализует и не меняет — только читает уже публикуемые refs. **Зависимость orphan-PVC capture удовлетворена текущим кодом.** Если при реализации обнаружится, что в конкретной тест-фикстуре отсутствует требуемый VS visibility-leaf / VSC `dataRef`, — это баг фикстуры/setup, а **не** повод добавлять новые поля или менять capture-логику.
- **Связанные нормативные документы:** `spec/snapshot-aggregated-read.md`, `api/snapshot-read.md`, `spec/system-spec.md` §3.4 (INV-REF-C1), `design/demo-domain-csd/08-universal-snapshot-tree-model.md`. Нормативные выдержки этого ADR после согласования переносятся в `spec/snapshot-aggregated-read.md` (раздел restore) и/или `spec/system-spec.md`.

### Контекст

Сегодня есть два namespaced subresource поверх одного snapshot-дерева:

- `…/namespaces/{ns}/snapshots/{name}/manifests` — агрегированный read-only слепок дерева
  (`usecase.AggregatedNamespaceManifests`);
- `…/namespaces/{ns}/snapshots/{name}/manifests-with-data-restoration` — «манифесты для
  восстановления» (`usecase/restore.Service`).

Текущее состояние и его проблемы:

1. **Два независимых конвейера.** Обход дерева продублирован: `AggregatedNamespaceManifests.walkContent`
   (через общий `WalkSnapshotContentSubtree`, top-down DFS) и `restore.resolver.buildTree`
   (собственная рекурсия). Это два места с расходящейся семантикой (namespace, ошибки, типы).
2. **Restore умеет слишком мало.** `restore.Transformer` делает только локальную замену
   `PVC → VolumeRestoreRequest` внутри одного узла. Он:
   - не обходит дерево снизу вверх (нет post-order);
   - не умеет доменные restore-преобразования (`DemoVirtualDisk`, `DemoVirtualMachine` и пр.);
   - не переписывает namespace (жёстко `400`, если `targetNamespace != snapshotNamespace`).
3. **Фильтрация полей привязана к capture-time.** `common.CleanObjectForSnapshot` работает только
   при `EnableFiltering=true` (дефолт `false`). На read-path общий `ArchiveService.createJSONArchive`
   режет только `status` и `metadata.managedFields`. Значит при дефолтной конфигурации в выдачу
   попадают `uid`, `resourceVersion`, `ownerReferences`, `creationTimestamp` — поля, мешающие
   `kubectl apply` при восстановлении.

Целевой сценарий (тестовый стенд): namespace с `DemoVirtualMachine` (+ `DemoVirtualDisk` + PVC),
отдельным «одиночным» `DemoVirtualDisk`, одиночными PVC и обычными объектами (`ConfigMap` и т.п.).
Сделали `Snapshot`. Хотим получить через `manifests-with-data-restoration` готовый к применению
набор манифестов **в другом namespace / другом кластере**, где тома привязаны к своим снапшотам
данных.

**Реальный сценарий — перенос (migration) в другой кластер.** Снапшот делается в namespace
исходного кластера, затем переносится в целевой кластер. Перенос выполняется в две части,
**вне** этого эндпоинта:

1. **Данные** — переносятся артефакты (`VolumeSnapshotContent` и соответствующие
   `VolumeSnapshot`); в целевом кластере VS/VSC уже существуют после переноса.
2. **Дерево снапшотов** — переносятся snapshot-узлы (`DemoVirtualDiskSnapshot` и т.п.) как часть
   дерева.

Поэтому restore-эндпоинту **не нужно** ничего материализовать или пересоздавать: VS/VSC и
доменные snapshot-узлы уже на месте. Задача эндпоинта — отдать целевые манифесты ресурсов с
проставленным `dataSource`/`dataSourceRef`, который ссылается на уже перенесённый артефакт по
идентичности (имя). Никакой логики создания VS из VSC в эндпоинте нет.

### Решение

#### D1. Два эндпоинта остаются; новый внешний HTTP subresource НЕ вводится

- `/manifests` = **read-only view**: те же сохранённые манифесты, опциональный output-фильтр,
  без restore-семантики. Отвечает на вопрос «что было сохранено?».
- `/manifests-with-data-restoration` = **restore compiler**: всегда restore-safe фильтр,
  rewrite namespace, post-order трансформация снизу вверх. Отвечает на вопрос «что нужно
  применить для восстановления?».

Доменные преобразования реализуются **внутренними** transformers/adapters в коде, а не вызовом
других HTTP-эндпоинтов. Модель «endpoint → вызывает другой endpoint» отвергается в пользу
«endpoint → общий tree loader → internal restore transformers».

#### D2. Общий core для обоих эндпоинтов

Выделить общий пайплайн (новый пакет, напр. `internal/usecase/snapshotgraph`):

```
resolve Snapshot root
  -> build SnapshotContent tree (ref-only, INV-REF-C1; cycle-safe)
  -> load manifests for every node (ManifestCheckpoint archive)
  -> normalize/filter output metadata (sanitize mode)
  -> dedup/merge (apiVersion|kind|namespace|name)
```

Оба эндпоинта используют один резолвер дерева и один загрузчик манифестов. Устраняется дубль
`walkContent` vs `resolver.buildTree`. Обход — строго по `status.childrenSnapshotContentRefs`
(не list, не `childrenSnapshotRefs`), с защитой от циклов — как сейчас в `WalkSnapshotContentSubtree`.

Дальше — разные режимы:

```
/manifests
  -> sanitize mode: none | restore-safe | namespace-relative (по умолчанию как сейчас)
  -> return normalized raw manifests
/manifests-with-data-restoration
  -> sanitize mode: restore-safe (всегда)
  -> targetNamespace rewrite
  -> post-order restore transform (children first, parent after)
  -> return apply-ready manifests
```

#### D3. Output-санитайзер на read-path (вынести фильтр «на выход»)

Ввести режим очистки на выходе, не на входе:

```go
type OutputSanitizationMode string
const (
    OutputRaw               OutputSanitizationMode = "raw"
    OutputRestoreSafe       OutputSanitizationMode = "restore-safe"
    OutputNamespaceRelative OutputSanitizationMode = "namespace-relative"
)
```

Для `manifests-with-data-restoration` режим `restore-safe` **обязателен** и удаляет:

- `metadata`: `uid`, `resourceVersion`, `generation`, `creationTimestamp`, `deletionTimestamp`,
  `deletionGracePeriodSeconds`, `managedFields`, `ownerReferences`, `selfLink`. **`finalizers` НЕ
  удаляются** (политика сохранения intent, см. ниже); `ownerReferences` удаляются намеренно (висячий
  ownerRef → мгновенный GC восстановленного объекта = потеря данных);
- `metadata.namespace` → переписать на `targetNamespace` (для namespaced объектов);
- `metadata.annotations`: `kubectl.kubernetes.io/last-applied-configuration`,
  `pv.kubernetes.io/bind-completed`, `pv.kubernetes.io/bound-by-controller`,
  `volume.kubernetes.io/selected-node` (stale/bind-аннотации исходного кластера; пустая map после
  чистки удаляется);
- `status`;
- kind-specific: `PVC.spec.volumeName`, `PVC.spec.dataSource`, `PVC.spec.dataSourceRef` (до
  restore-трансформации); `Service.spec.clusterIP/clusterIPs/ipFamilies/ipFamilyPolicy/healthCheckNodePort/loadBalancerIP`
  и `Service.spec.ports[].nodePort`;
- in-spec namespace rewrite: `RoleBinding.subjects[].namespace` для `kind: ServiceAccount` →
  `targetNamespace` (namespace переезжает целиком, иначе RBAC после restore ссылается на исходный
  namespace). Прочие in-spec namespace-ссылки доменных CR — ответственность `DomainRestoreTransformer`.

Для `/manifests` сохраняется текущее поведение (`namespace-relative`: убрать `metadata.namespace`,
отбросить cluster-scoped; плюс уже всегда режутся `status`/`managedFields` в `ArchiveService`).
Режим `none`/`raw` — опция на будущее, не обязателен в MVP.

**Cluster-scoped в restore MVP:** namespace restore compiler **не эмитит** cluster-scoped объекты
(как и `/manifests`). Иначе restore в другой namespace мог бы случайно вернуть `CRD`/`ClusterRole`
/`StorageClass` и т.п. В MVP — drop; «include cluster-scoped as-is» — возможная будущая опция, не
этот этап.

**Финализаторы сохраняются (не режутся на restore).** Класс 1 (машинные, напр.
`kubernetes.io/pvc-protection`) целевой кластер навесит заново; Класс 3 (кастомные) кодируют intent
пользователя и обязаны пережить restore; Класс 2 (self-induced wedge
`snapshot.storage.kubernetes.io/pvc-as-source-protection`) до restore не доходит — он срезан **на
захвате** (единственное field-level исключение verbatim-захвата). Cross-cluster import: кастомный
финализатор без контроллера в целевом кластере оставит объект неудаляемым без ручного вмешательства —
осознанная intent-семантика.

Замечание: capture теперь имеет **ровно одно** field-level исключение из verbatim — срез транзиентного
`pvc-as-source-protection`. Read-path санитайзер — независимый слой; restore не должен зависеть от того,
был ли включён `EnableFiltering` при захвате.

#### D4. `targetNamespace != sourceNamespace` становится рабочим для restore

Снять ограничение `400`. Логика:

```
if targetNamespace == "" { targetNamespace = snapshotNamespace }
for namespaced objects { metadata.namespace = targetNamespace }
```

Только для restore-эндпоинта. Для `/manifests` остаётся namespace-relative (namespace убирается).

#### D5. Post-order трансформация (restore compiler)

`manifests-with-data-restoration` обходит дерево **снизу вверх** (post-order): сначала дети,
потом родитель. Это позволяет родительскому узлу видеть уже преобразованные манифесты детей.

Пример целевого дерева:

```
VM SnapshotContent
└── Disk SnapshotContent
    └── PVC / VSC data artifact (dataRefs)
```

Порядок обработки:

1. **Orphan PVC / data leaf:** `PVC -> PVC` с `spec.dataSourceRef -> VolumeSnapshot` — **только**
   для orphan PVC; VS резолвится через `childrenSnapshotRefs` visibility-leaf (см. D7). VRR не
   эмитится. Domain-covered PVC здесь как отдельный PVC не эмитится.
2. **DemoVirtualDisk leaf:** raw-манифест диска + его data artifact / результат ребёнка
   `-> DemoVirtualDisk` с `spec.dataSource` на снапшот диска (поле уже есть в API:
   `DemoVirtualDiskSpec.DataSource -> DemoVirtualDiskSnapshot`).
3. **DemoVirtualMachine parent:** raw-манифест VM + уже восстановленный манифест диска
   `-> DemoVirtualMachine` (в demo — почти без изменений, no-op кроме sanitize + namespace).
4. **namespace root:** `ConfigMap`/`Secret`/`Service`/обычные объекты как normalized raw +
   трансформированные доменные манифесты.

Аналогично для второго диска/нескольких детей.

Алгоритм `BuildManifestsWithDataRestoration`:

```
1. resolve root SnapshotContent tree
2. ensure every node Ready=True
3. load manifests for every node
4. walk post-order:
   a. children first
   b. sanitize (restore-safe) + rewrite namespace -> targetNamespace
   c. transform PVC/data leaves using node.dataRefs
   d. transform domain nodes via registered restore transformer
   e. generic objects pass-through (после sanitize)
5. merge transformed manifests
6. dedup by apiVersion|kind|namespace|name (дубль -> 409 Conflict)
7. return JSON (apply-ready)
```

#### D6. Внутренний контракт restore-трансформера (расширяемость)

Ввести Go-интерфейс (без новых HTTP subresources):

```go
type NodeRestoreTransformer interface {
    Supports(node SnapshotContentNode) bool
    Transform(ctx context.Context, input RestoreTransformInput) ([]unstructured.Unstructured, error)
}
```

`RestoreTransformInput` несёт: манифесты узла (после sanitize), `node.DataBindings`
(`status.dataRefs[]`), уже восстановленные манифесты детей, `targetNamespace`, source-identity
узла. Встроенные реализации:

- `PVCRestoreTransformer`;
- `DemoVDiskRestoreTransformer`;
- `DemoVMRestoreTransformer` (no-op кроме sanitize/namespace);
- `GenericPassthroughTransformer`.

**Ограничение generic passthrough.** `GenericPassthroughTransformer` применяется только к
captured application-объектам после restore-safe санитайзера и **не** пропускает
snapshot/control-plane kinds. Явный exclude (никогда не эмитятся в выдачу):
`Snapshot`, `SnapshotContent`, `ManifestCheckpoint` (+ chunks), `VolumeSnapshot`,
`VolumeSnapshotContent`, `VolumeRestoreRequest`, `VolumeCaptureRequest`, `ManifestCaptureRequest`,
доменные `*Snapshot` CR. (VS/VSC и snapshot-узлы переносятся отдельно как data + tree; в
apply-ready выдаче их быть не должно — они только резолвятся для ссылок.) Иначе passthrough мог бы
случайно вернуть старые доменные snapshot CR / internal-объекты.

> **Направление будущего контракта доменной трансформации** (AdmissionReview-подобный, versioned,
> per-node, generic владеет набором/sanitize/dedup, domain возвращает mutation+suppress, transport-
> independent, no additions) зафиксировано в отдельном ADR:
> `snapshot-rework/2026-06-13-domain-restore-transform-contract.md`. Описанный ниже in-process
> `DomainRestoreTransformer` — это **v0 этого контракта** (самостоятельный вариант, без миграций);
> точная схема и внешний транспорт — отдельное возможное будущее направление.

**Как реализовано (вместо inline-хелперов MVP).** Изначально планировались inline demo-хелперы
(`transformDemoVDisk`/`transformDemoVM`) прямо в restore-пайплайне с TODO на вынос. Вместо этого
сразу введён зарегистрированный in-process Go-интерфейс (нового HTTP subresource по-прежнему нет),
чтобы доменная логика жила рядом со своими типами, а generic `internal/usecase/restore` оставался
domain-free (см. repo guard `TestProductionSourcesDoNotNameDemoSnapshotKinds`):

```go
type DomainRestoreTransformer interface {
    // PVC, которые доменный объект пересоздаёт на restore (covered) — их generic-слой не эмитит как
    // отдельный PVC и не считает orphan.
    CoveredPVCNames(node *RestoreNode, objects []unstructured.Unstructured) map[string]struct{}
    // Доводит один уже-санитайзенный доменный объект до apply-ready. children — уже скомпилированные
    // (restore-ready) объекты дочерних снапшотов этого узла (post-order, снизу вверх), чтобы родитель
    // мог ссылаться на восстановленных детей. Возвращает handled=true, если объект его.
    TransformObject(node *RestoreNode, obj *unstructured.Unstructured, children []NodeResult) (bool, error)
}
```

- **Bottom-up:** `compileNode` обходит дерево post-order, сперва компилирует детей и передаёт их
  `NodeResult` в `TransformObject` родителя (закрепляет ADR-требование «parent can use restored
  manifests of children», даже если текущий demo VM — no-op).
- **Один владелец объекта:** если объект пометили `handled=true` два и более трансформера — это
  contract violation (неоднозначный restore), а не «последний победил».
- Реализация demo: `internal/controllers/demo/restore_transform.go` (`DemoVirtualDisk` →
  `spec.dataSource` на свой `DemoVirtualDiskSnapshot`; covered PVC диска подавляется; `DemoVirtualMachine`
  и прочее — no-op). Регистрируется в `restore.Service` в `internal/api/server.go`.

#### D7. Эндпоинты НИКОГДА не возвращают `VolumeRestoreRequest`

Ключевое закреплённое решение: ни `/manifests`, ни `/manifests-with-data-restoration` не должны
возвращать `VolumeRestoreRequest` (VRR), хотя текущая реализация restore это делает. Это
поведение убирается.

- **VRR — вспомогательный (execution) ресурс доменных контроллеров.** Позже доменный контроллер
  МОЖЕТ использовать VRR, чтобы создать реальный `PersistentVolumeClaim`. Это деталь
  restore-reconcile, а **НЕ** выходной артефакт read/compile эндпоинтов.
- Выход эндпоинтов — это **apply-ready манифесты целевых ресурсов**, а не запросы на их создание.

Контракт PVC/диск restore (целевые манифесты, без VRR):

- **Доменный диск (`DemoVirtualDisk`):** вернуть `DemoVirtualDisk`, у которого
  `spec.dataSource -> DemoVirtualDiskSnapshot` (поле уже есть в API: `DemoVirtualDiskSpec.DataSource`).
  Имя snapshot-объекта берётся из **доменной snapshot-ссылки узла** (узел дерева /
  `childrenSnapshotRefs` / source-identity снапшота), **не** из имени source-объекта: диск `disk-a`
  и его снапшот `snapshot-a` — разные объекты; для `dataSource` нужен именно snapshot-объект.
- **Orphan PVC:** вернуть сам `PersistentVolumeClaim` с `spec.dataSourceRef -> VolumeSnapshot`
  (разрешение имени VS — ниже).

Никакого `restoreStrategy: pvcDataSource | vrr` не вводится: VRR-режим исключён из контракта
эндпоинтов полностью.

**Orphan vs domain-covered PVC (ключевое различие).** PVC как самостоятельный restore-target
эмитится **только** для orphan PVC (root residual data-leg, не покрытый доменным
snapshot-контроллером). PVC, принадлежащий subtree `DemoVirtualDisk`/`DemoVirtualMachine`, как
отдельный PVC **не** эмитится — он восстанавливается через доменный ресурс
(`DemoVirtualDisk.spec.dataSource -> DemoVirtualDiskSnapshot`).

Orphan-ность определяется **покрытием дерева / доменным владением**, а **не** наличием VS/VSC:
наличие `VolumeSnapshot` для PVC НЕ делает его orphan. Критерий: PVC-манифест принадлежит текущему
узлу и НЕ покрыт доменным child/root restore-трансформом. (VSC создаётся и для домен­но-покрытых
PVC тоже, но VS как restore-facing объект нужен только на orphan-пути.)

**Implementation note (классификация PVC обязательна перед трансформацией).** Реализация ДОЛЖНА
сначала классифицировать каждый PVC, и только потом трансформировать:

- **orphan PVC** → emit `PVC` с `spec.dataSourceRef` на VS (резолв через leaf, см. ниже);
- **domain-covered PVC** → **suppress** PVC, восстановить через доменный объект (`DemoVirtualDisk` и т.п.);
- **PVC без `dataRef` и не покрытый доменным объектом** → **contract violation** (fail-closed).

Хелпер `transformPVCsToDataSources` **не** должен слепо обрабатывать все PVC в манифестах узла:
без классификации он рискует превратить domain-covered PVC в orphan-PVC restore (двойное
восстановление одного тома).

**Семантика PVC без `dataRef` (MVP, зафиксировано: fail-closed).** Эмитить namespaced-PVC без
`dataSourceRef` опасно: restore создаст PVC **без данных** (молчаливая потеря данных). Поэтому
любой PVC в namespace snapshot, который **не** покрыт доменным объектом и для которого **не** найден
`dataRefs` → `VolumeSnapshot` binding, даёт `ErrContractViolation` (409) и обваливает весь ответ.
Инвариант: **любой emitted PVC обязан иметь `spec.dataSourceRef`**. Поведение зафиксировано
guard-тестом (`TestTransformNode_PVCWithoutDataRefFailsClosed`).
**TODO/follow-up:** ввести явный признак «stateless / пустой PVC, данные не бэкапим и пустое
восстановление безопасно» (аннотация на PVC при capture или политика узла), чтобы разрешить
осознанный data-less passthrough; до тех пор passthrough PVC без данных запрещён.

**Разрешение имени `VolumeSnapshot` для orphan PVC (без дублирования в `dataRefs[]`).**
`dataRefs[]` остаётся data-layer контрактом и хранит durable артефакт = `VolumeSnapshotContent`
(VSC); имя VS в `dataRefs[]` **не** добавляется. Compiler резолвит VS так: orphan PVC покрыт
`dataRef` на VSC `X`; соответствующий `VolumeSnapshot` — это visibility-leaf узла в
`Snapshot.status.childrenSnapshotRefs[]` (`IsVolumeSnapshotVisibilityLeaf`), у которого
`spec.source.volumeSnapshotContentName == X`. `metadata.name` этого VS подставляется в
`PVC.spec.dataSourceRef.name` (в `targetNamespace`). Если для VSC найдено 0 или >1 VS — contract
violation.

> **Согласованность с ADR `2026-06-09-orphan-pvc-csi-volumesnapshot.md`.** VS **не** попадает в
> MCP / manifest inventory / aggregated manifests (INV-ORPHAN3), поэтому compiler **не** ищет VS
> среди сохранённых манифестов — он резолвит его через `childrenSnapshotRefs` visibility-leaf
> (INV-ORPHAN4). Это требует **живого** `Snapshot` (leaf VS живёт пока жив run; durable артефакт —
> VSC). Для retained-пути (после удаления `Snapshot`) разрешение VS — открытый вопрос (см. ниже).

**Допущение о переносе (migration).** Эндпоинт НЕ создаёт и не материализует артефакты данных:
`VolumeSnapshot` / `VolumeSnapshotContent` и доменные snapshot-узлы (`DemoVirtualDiskSnapshot`)
переносятся в целевой кластер отдельно (data + tree). Эндпоинт лишь проставляет ссылку
`dataSource`/`dataSourceRef` на уже существующий (перенесённый) артефакт **по идентичности**
(имя/kind). Имя VS/snapshot для ссылки берётся из узла дерева (`dataRefs[]` / source-identity
узла), а не вычисляется заново. Если в целевом кластере артефакта нет — это ответственность
процесса переноса, а не эндпоинта.

#### D8. Source identity — не хардкодить `spec.sourceRef`

Доменный source узла извлекается через generic-контракт, а не по имени поля в доменном CR.
Источник истины — framework-аннотация `state-snapshotter.deckhouse.io/source-ref`
(`AnnotationKeySourceRef`, см. `internal/controllers/demo/source_identity.go`); `spec.sourceRef` —
demo/manual API-compat поле, выводимое из аннотации. Generic-логика restore не должна предполагать
конкретное имя поля доменного snapshot CR. Это сохраняет ранее зафиксированный review-инвариант.

#### D9. `scope=node` и фильтр объекта (`kind`/`name`) — сужение выдачи без нового субресурса

Компилятор умеет отдавать не только всё поддерево, но и один узел или один объект — через
**query-параметры** того же `manifests-with-data-restoration`. Новых HTTP subresource'ов не вводим,
форма ответа не меняется: тот же GET, тот же JSON-массив apply-ready (санитизированных) объектов —
параметры лишь **сужают** набор. Параметры единообразны для namespaced `snapshots` и для VS-коннектора
(`subresources.snapshot.storage.k8s.io`) — один парсер query-опций на оба пути.

- **`scope=node|subtree`** — опционален; отсутствие ⇒ `subtree` (полная обратная совместимость с
  D2/D5). `subtree` — прежний рекурсивный post-order (не-Ready ребёнок валит весь запрос, `409`).
  `scope=node` резолвит и валидирует **только адресуемый узел** (его Ready-гейт, `Ready` привязанного
  контента, anti-spoofing back-ref корня → `403`, непустой MCP) и **детей не читает вовсе** —
  не-Ready ребёнок запрос не валит. Выдаются только собственные манифесты узла, прогнанные через тот
  же restore-safe путь (D3/D4).
- **Degraded-корень при `scope=node`.** Для user-addressed КОРНЯ при `scope=node` Ready-гейт мягко
  ослаблен: `Ready=False` с reason из каталога `DegradedReadyReasons` (SSOT — Go
  `api/storage/v1alpha1`; единственный член `ChildSnapshotDeleted`: удалён namespaced-ребёнок, а
  собственный манифест-лег корня цел) **не отклоняется** `409`. Собственные проверки узла сохраняются
  полностью (`Ready` привязанного `SnapshotContent`, anti-spoofing back-ref → `403`, непустой MCP), а
  дети при `scope=node` и так не читаются — поэтому per-node/per-object restore корня обязан работать.
  `scope=subtree` (и дефолт без параметра) остаётся fail-closed `Ready=True` — деградировавший снимок
  целиком не компилируется. Relax применяется **только** к user-addressed узлу при `scope=node`; узлы,
  достигнутые tree-walk'ом, и VS-коннектор не меняются.
- **`kind=<Kind>&name=<name>`** (+ опц. `apiVersion=<group[/version]>` — полный `group/version` **или**
  голая `group`) — фильтр одного объекта, допустим **только при `scope=node`**; применяется **после**
  трансформации по точному совпадению. Исходы: одно совпадение → массив из одного объекта (`200`);
  ноль (в т.ч. объект, **выпиленный санитайзером**) → `404`; ≥2 совпадений `kind`+`name` в разных
  API-группах без `apiVersion` → `400` fail-closed (в сообщении — конкурирующие `apiVersion`);
  заданный `apiVersion` снимает неоднозначность (если и с ним осталось >1 — тоже `400`; контракт
  гарантирует ровно один объект, множественный матч не отдаётся).
- **Валидация → `400`** (существующий маппинг restore-ошибок `ErrBadRequest`): неизвестный `scope`;
  `kind` без `name` или `name` без `kind`; `apiVersion` без `kind`; любой из `kind`/`name`/`apiVersion`
  при `scope≠node` (фильтр объекта при `subtree` запрещён).
- `targetNamespace` **ортогонален** обоим параметрам (namespace-rewrite работает и для одного узла, и
  для одного объекта). Каталог conditions/reasons/фаз новыми параметрами **не меняется**.

**SSOT контракта** — первичный ADR `architecture-decision-records:
dkp/storage/state-snapshotter/2026-06-29-unified-snapshots-overview.md`, подраздел
«`manifests-with-data-restoration`: `scope` и фильтр объекта (`kind`/`name`)». Здесь — только
нормативная выжимка read-path; при расхождении источник истины — первичный ADR (правило
SSOT / document-boundaries).

### Инварианты

- **INV-RC1 (общий core):** оба эндпоинта используют один резолвер дерева и один загрузчик
  манифестов; дублирующий обход (`walkContent` vs `resolver.buildTree`) устраняется.
- **INV-RC2 (ref-only обход):** обход дерева идёт только по `status.childrenSnapshotContentRefs`,
  без list/`childrenSnapshotRefs`, cycle-safe (сохранение INV-REF-C1, `spec/system-spec.md` §3.4).
- **INV-RC3 (post-order для restore):** restore-компиляция обрабатывает детей раньше родителя;
  родитель может использовать восстановленные манифесты детей.
- **INV-RC4 (restore-safe обязателен):** выдача `manifests-with-data-restoration` всегда очищена
  от runtime-полей (D3) и namespaced-объекты переписаны на `targetNamespace`.
- **INV-RC5 (dedup):** дубль по `apiVersion|kind|namespace|name` (после трансформации) — `409`,
  без молчаливого merge/overwrite (как в `spec/snapshot-aggregated-read.md`).
- **INV-RC6 (Ready gating):** restore-компиляция требует **явного** `Ready=True` на каждом узле
  дерева — и на Snapshot/доменном snapshot-CR, и на его SnapshotContent. **Отсутствующее** условие
  `Ready` трактуется как not-ready (`ErrNotReady`), а не как успех (mid-reconcile узел не должен
  попадать в restore). При не-Ready/missing/failed узле — fail-whole (`409`/`404`/`400`).
  **Единственное исключение (D9 degraded-relax):** user-addressed КОРЕНЬ при `scope=node` с
  `Ready=False` и reason из `DegradedReadyReasons` (сейчас — только `ChildSnapshotDeleted`) не
  отклоняется; остальные проверки узла (`Ready` контента, anti-spoofing, непустой MCP) сохраняются.
  `scope=subtree`/дефолт и tree-walk дети — по-прежнему строгий `Ready=True`.
- **INV-RC7 (no name-coupling):** source identity извлекается через generic-контракт
  (annotation/adapter), а не через хардкод `spec.sourceRef` доменного CR.
- **INV-RC8 (no new endpoint):** доменные restore-преобразования — внутренние Go-трансформеры;
  новые HTTP subresources не вводятся.
- **INV-RC9 (no VRR в выдаче):** ни один эндпоинт не возвращает `VolumeRestoreRequest`. VRR —
  внутренний execution-ресурс доменных контроллеров (restore-reconcile), не выходной артефакт
  read/compile. PVC restore выражается как **целевой** ресурс: доменный диск с `spec.dataSource`
  на свой snapshot; orphan PVC с `spec.dataSourceRef` на свой `VolumeSnapshot`.
- **INV-RC10 (orphan-PVC VS restore):** `VolumeSnapshot`-based PVC restore применяется **только**
  к orphan PVC data-артефактам. Для orphan PVC с `dataRef` на VSC `X` compiler ОБЯЗАН найти ровно
  один `VolumeSnapshot` (visibility-leaf в `Snapshot.status.childrenSnapshotRefs[]`) с
  `spec.source.volumeSnapshotContentName == X` и выставить `PVC.spec.dataSourceRef` на этот VS в
  `targetNamespace`; 0 или >1 VS на VSC — contract violation. **Fail-closed по состоянию leaf:**
  endpoint собирает apply-ready PVC со ссылкой на этот VS, поэтому VS leaf, который удаляется
  (`metadata.deletionTimestamp != nil`), не `status.readyToUse`, или с пустым
  `status.boundVolumeSnapshotContentName`, — это ошибка (not-ready / contract violation), а не
  пропуск. Domain-covered PVC **не** порождают
  PVC `dataSourceRef`-манифест (восстанавливаются через доменный трансформер). Orphan-ность — по
  покрытию дерева/доменному владению, **не** по наличию VS/VSC.
- **INV-RC11 (target references valid in target namespace):** compiler эмитит только ссылки,
  валидные в целевом namespace. `PVC.spec.dataSourceRef` ОБЯЗАН ссылаться на namespaced
  `VolumeSnapshot`; доменные ресурсы — на свой доменный snapshot-объект по его restored identity.
  Cluster-scoped и internal/control-plane snapshot-артефакты в выдаче **не** эмитятся (MVP).

### Структура кода (целевая)

```
internal/usecase/snapshotgraph/   общий resolver + loader + post-order walk (новый)
internal/usecase/restore/
  service.go        orchestration
  sanitizer.go      restore-safe output cleanup (новый)
  transformer.go    orchestration of node transforms
  pvc_helpers.go    PVC restore transform (PVC -> PVC c spec.dataSourceRef; без VRR)
  demo_helpers.go   временные DemoVDisk/DemoVM transforms (TODO: -> domain adapter)
```

`/manifests` (`AggregatedNamespaceManifests`) переключается на общий `snapshotgraph`-резолвер,
сохраняя текущий namespace-relative режим и семантику ошибок (`AggregatedStatusError`).

### Lifecycle / зависимости rollout

- Restore-reconcile (фактическое создание объектов) не входит в этот ADR — здесь только
  компиляция apply-ready манифестов.
- **Перенос данных и дерева — внешний шаг.** Эндпоинт предполагает, что `VolumeSnapshot`/`VSC` и
  доменные snapshot-узлы уже перенесены в целевой кластер. Эндпоинт только проставляет ссылку на
  них; материализация артефактов — ответственность процесса переноса, не эндпоинта.
- PVC restore (`spec.dataSourceRef -> VolumeSnapshot`) и диск restore (`spec.dataSource ->
  DemoVirtualDiskSnapshot`) на стороне применения зависят от наличия перенесённых артефактов и
  доменной restore-поддержки (`restore-rollout-guard`). Эта зависимость **не** возвращает VRR в
  выдачу и **не** требует от эндпоинта создавать VS/VSC.
- **Жизненный цикл VS/VSC (orphan-путь).** `VolumeSnapshot` живёт, переносится и удаляется
  **вместе со всем snapshot-деревом** (visibility-leaf run; ownerRef → root Snapshot/ObjectKeeper,
  GC с run — `2026-06-09-orphan-pvc-csi-volumesnapshot.md`). Данные защищены durable `VSC` с
  `deletionPolicy=Retain`. Отдельное удаление VS безвредно: PVC восстанавливается вместе с
  namespace (через перенесённый артефакт). Поэтому endpoint резолвит VS через leaf и **не** обязан
  делать VS-identity durable — durable-ность гарантируется на уровне VSC, а не VS.

### Открытые вопросы

1. **Разрешение имени `VolumeSnapshot` — не блокер.** На живом дереве VS резолвится через
   `childrenSnapshotRefs` visibility-leaf (D7, INV-RC10), и этого достаточно: VS живёт/переносится
   /удаляется вместе с деревом, а durability данных держит VSC (`Retain`). Делать VS-identity
   durable отдельно **не** требуется. Единственный edge — чистый retained-read **после** удаления
   всего дерева (когда остался только VSC); это сворачивается в общий retained-read TODO (3), а не
   в отдельную проблему restore.
2. **Точное поле/маппинг имени `DemoVirtualDiskSnapshot`** для `DemoVirtualDisk.spec.dataSource`:
   решено — берётся из доменной snapshot-ссылки узла (`childrenSnapshotRefs` / source-identity
   снапшота), не из имени source-объекта (D7). Остаётся уточнить конкретное поле при реализации.
3. **Retained read после удаления Snapshot** для restore-пути: сейчас resolve идёт через живой
   `Snapshot`. Долгосрочно — durable `/snapshotcontents/{name}/manifests` (TODO в
   `api/snapshot-read.md`). Нужно ли restore поверх retained content в этом этапе — открыто.
4. **Where lives `snapshotgraph`-core** (новый пакет vs расширение существующего
   `pkg/snapshot` walker) — уточнить при реализации, чтобы не ломать `WalkSnapshotContentSubtree`.

### Alternatives considered

- **Новый внешний demo restore-endpoint (HTTP), вызываемый из restore-пайплайна.** Отвергнуто на
  этом этапе: лишняя сетевая граница и RBAC-поверхность; вместо этого — внутренние трансформеры с
  чёткой точкой расширения (D6). Внешние adapters возможны позже без смены HTTP-контракта.
- **Оставить `manifests-with-data-restoration` как PVC-replacer.** Недостаточно: не покрывает
  доменные узлы (`DemoVDisk`/`DemoVM`) и не делает bottom-up компиляцию; целевой сценарий
  (VM+Disk+PVC, restore в другой namespace) не достигается.
- **Возвращать `VolumeRestoreRequest` из эндпоинтов (текущее поведение).** Отвергнуто (D7/INV-RC9):
  VRR — execution-ресурс доменного слоя для последующего создания PVC, а не целевой apply-ready
  манифест. Эндпоинты должны отдавать целевой ресурс (PVC с `dataSourceRef` / диск с `dataSource`),
  а материализацию через VRR оставить доменным контроллерам на этапе restore-reconcile.
- **Фильтрация только на capture (`EnableFiltering`).** Недостаточно для restore: при дефолтном
  `EnableFiltering=false` выдача содержит мешающие apply поля. Нужен обязательный read-path
  restore-safe режим (D3).
- **Слить два эндпоинта в один с флагом.** Отвергнуто: разные ответы на разные вопросы
  («что сохранено» vs «что применить»); раздельные subresources сохраняют обратную совместимость
  `/manifests`. Объединяется только код (core), не контракт.

### Scope / stage / out of scope

- **Stage:** новый слайс **read-path/restore-compile** поверх N2b-aggregated read; объявить в
  `operations/project-status.md` после согласования ADR. **Только генерация манифестов** —
  capture/tree-building не входит.
- **Out of scope:** restore-reconcile (создание объектов), **процесс переноса данных/дерева в
  другой кластер** (перенос `VolumeSnapshot`/`VSC` и snapshot-узлов — внешний шаг), изменение
  capture-pipeline, durable content read route, материализация VS из VSC, внешние HTTP
  restore-adapters.
- **Не нарушать:** `spec/snapshot-aggregated-read.md` (dedup `409`, namespace-relative для
  `/manifests`, ref-only обход), `restore-rollout-guard`, generic source-identity (INV-RC7),
  ADR `2026-06-09-orphan-pvc-csi-volumesnapshot.md` (INV-ORPHAN3 — VS не в manifests; INV-ORPHAN4
  — VS как `childrenSnapshotRefs` visibility-leaf; restore резолвит VS через leaf, не через MCP).

### Tests (план)

Unit / envtest на restore compiler:

- **orphan PVC + VSC + VS (visibility-leaf):** вернулся `PersistentVolumeClaim` с
  `spec.dataSourceRef.name == VS.metadata.name`;
- **domain-covered PVC** (есть VSC, возможно есть VS): PVC **не** эмитится; вместо него
  `DemoVirtualDisk` с `spec.dataSource -> DemoVirtualDiskSnapshot`;
- **orphan PVC + VSC, VS не найден** → contract violation;
- **orphan PVC + VSC, два VS на один VSC** → contract violation / `409`;
- **orphan-ность по покрытию, не по VS:** PVC под доменным диском, у которого есть VS, всё равно
  не эмитится как самостоятельный PVC;
- `DemoVirtualDisk.spec.dataSource.name` равен имени **snapshot-объекта**, а не source-объекта;
- **no-VRR guard:** ни в одной выдаче (`/manifests` и `/manifests-with-data-restoration`) нет
  объектов `VolumeRestoreRequest`;
- одиночный `DemoVirtualDisk` (вернулся `DemoVirtualDisk` с `spec.dataSource -> DemoVirtualDiskSnapshot`);
- `DemoVirtualMachine` + `DemoVirtualDisk` (post-order: диск восстановлен до VM);
- namespace root: `ConfigMap` + orphan PVC + одиночный disk + VM-disk вместе;
- restore в другой namespace (`targetNamespace != sourceNamespace`, namespace переписан);
- **cluster-scoped объект в дереве** → restore compiler его **не** эмитит (MVP drop);
- **generic passthrough exclude:** `VolumeSnapshot`/`VSC`/`*Snapshot`/control-plane kinds
  отсутствуют в выдаче;
- duplicate после трансформации → `409`;
- missing `dataRef` для orphan PVC → ошибка;
- child not Ready → fail-whole;
- deleted/failed child инвалидирует restore родителя;
- sanitize: проверка отсутствия `uid`/`resourceVersion`/`ownerReferences`/`status`/`managedFields`
  и `PVC.spec.volumeName/dataSource/dataSourceRef` в выдаче;
- `/manifests` регрессия: namespace-relative и cluster-scoped drop не изменились после перевода на
  общий core.

Cluster smoke — при необходимости в рамках `hack/demo-e2e.sh`; demo-domain сценарии удалённого
`hack/snapshot-tree-demo-e2e.sh` живут в e2e-наборе модуля `sds-unified-snapshots-poc`
(см. `testing/e2e-testing-strategy.md`).
