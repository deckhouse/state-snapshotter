# ADR: модель conditions снапшота (PlanningReady / ManifestsReady / VolumesReady / ChildrenReady / Ready)

- **Дата:** 2026-06-03
- **Статус:** Accepted — реализовано в PR2a–PR2c. **Update 2026-06-14:** единый `RequestsReady` разнесён на две публичные линии `ManifestsReady` + `VolumesReady` (см. §2.2); формула и инварианты обновлены ниже. Нормативные выдержки — в `docs/internal/state-snapshotter-rework/spec/system-spec.md` §3.8 / §3.9.7 (INV-COND1..6, INV-FAIL1). Этот документ — long-form запись решения (почему), не текущая спецификация.
- **Область:** `storage.deckhouse.io/v1alpha1` `Snapshot` / `SnapshotContent`, domain `XxxxSnapshot` (demo VM/Disk), `GenericSnapshotBinderController`, `SnapshotContentController`.

> Канон для кода и тестов — `spec/system-spec.md`; этот ADR держим в синхроне с ним (cross-doc-consistency). Предыстория (какой набор conditions был до этой модели) — в Appendix A.

## 1. Контекст и проблема

Готовность снапшота должна вычисляться в **одном** месте и выражаться **минимальным** набором публичных conditions. До этой модели набор был перегружен и частично мёртв, а итоговая готовность вычислялась в нескольких местах одновременно (на стороне `Snapshot` и на стороне `SnapshotContent`), что создавало риск двух источников истины и теряло диагностику при failure-propagation. Детали «до» — Appendix A.

Цель: один агрегатор готовности на `SnapshotContent`, `Snapshot.Ready` — только зеркало, и понятный для заказчика набор имён.

## 2. Решение: 5 публичных condition'ов

```go
const (
    ConditionReady          = "Ready"          // external aggregate: ManifestsReady && VolumesReady && ChildrenReady
    ConditionPlanningReady  = "PlanningReady"  // domain/custom controller завершил планирование (gate)
    ConditionManifestsReady = "ManifestsReady" // манифестная линия узла: MCP/checkpoint опубликован и Ready
    ConditionVolumesReady   = "VolumesReady"   // волюмная линия узла: все required dataRefs[] артефакты Ready
    ConditionChildrenReady  = "ChildrenReady"  // все child SnapshotContent.Ready=True (нет детей → True)
)
```

Семантика и последовательность (хэндофф между контроллерами):

1. **`PlanningReady`** — доменный/snapshot-контроллер первым берётся за объект и выставляет `PlanningReady=True`, когда закончил планирование: дети созданы и опубликованы как `status.childrenSnapshotRefs`, свои `MCR`/`VCR`/иные requests созданы. Это **gate** для общего контроллера и **барьер волны** для родителя.
2. **`ManifestsReady` / `VolumesReady`** — common/content-path вступает **после** `PlanningReady` (увидел, что requests созданы) и публикует durable refs в `SnapshotContent`: `status.manifestCheckpointName` (манифестная линия) и `status.dataRefs[]` (волюмная линия). `ManifestsReady=True`, когда MCP опубликован и `Ready=True`; `VolumesReady=True`, когда все required `dataRefs[]` артефакты `Ready`.
3. **`ChildrenReady`** — `True`, когда у всех дочерних `SnapshotContent` `Ready=True`. Нет детей (leaf) → `True` вакуумно.
4. **`Ready`** — агрегат **на `SnapshotContent`**: `Ready = ManifestsReady && VolumesReady && ChildrenReady`. На `Snapshot` — **только mirror** `Content.Ready` (snapshot-контроллер не пересчитывает дерево).

`SelfReady` намеренно **не вводим**: он эквивалентен `ManifestsReady && VolumesReady` (т.к. обе линии ⇒ `PlanningReady`) и не несёт новой информации.

### 2.1. Финальная формула

```text
SnapshotContent.Ready = ManifestsReady && VolumesReady && ChildrenReady
Snapshot.Ready        = mirror(SnapshotContent.Ready)   // НЕ локальный пересчёт
```

### 2.2. Почему две линии вместо единого `RequestsReady` (Update 2026-06-14)

`RequestsReady` скрывал **две разные причины ожидания/отказа**: манифестную (MCR → `ManifestCheckpoint`) и волюмную (VCR → `dataRefs[]`/`VolumeSnapshotContent`). Для эксплуатации это плохо: видно «снапшот не готов», но не видно — застрял ли он на capture манифестов или на data-артефактах. Линии имеют **разную природу и таймскейл** (манифесты — секунды; снятие тома — потенциально часы), и алертить/диагностировать их нужно раздельно. После сплита причина видна **сразу по типу condition**, без парсинга reason.

Это **новое осознанное решение по публичному API**, а не возврат старых мёртвых констант `ManifestsReady`/`DataReady` из Appendix A (те никогда не выставлялись и были удалены в PR2c). Принцип «минимальный набор conditions» сохраняется: `RequestsReady` оказался **слишком грубым** для ops-диагностики и **заменяется** двумя доменными линиями, а не расширяется набором технических `MCR`/`VCR`-conditions. На первом шаге sequencing сохраняется (волюмная линия оценивается после манифестной); пока манифесты не готовы — `VolumesReady=Unknown/ManifestCapturePending` (линия ещё не оценивалась, это не отказ тома). Независимая оценка линий — отдельный follow-up. Это **дизайн v0** — никакого `RequestsReady` в модели нет вовсе (не пишем, никаких миграций/совместимости со старым условием не предусматриваем).

### 2.3. Residual-gate первого `Ready=True` (Update 2026-06-28)

Для namespace-root `SnapshotContent` со смешанным деревом (доменные дети + orphan-PVC) три ноги формулы (`ManifestsReady && VolumesReady && ChildrenReady`) могли стать `True` **после** готовности доменных детей, но **до** запуска финальной residual/orphan-PVC-волны (волна стартует только когда все доменные дети готовы, затем линкует orphan child-node в дерево). Это давало флап `Ready` True→False→True (линковка orphan-ребёнка роняет `ChildrenReady`), а потребитель, увидевший первый `True`, получал на `manifests-with-data-restoration` `ErrNotReady`/409.

Контракт: корневой content (и зеркало `Snapshot.Ready`) не имеет права **впервые** выдать `Ready=True`, пока residual-волна не завершена (нет orphan-таргетов **или** все orphan child-nodes слинкованы и готовы). Реализовано **fail-closed**-гейтом — дополнительной **низшей по приоритету** ногой `Ready`:

- Сигнал завершения волны — поле `SnapshotContent.status.residualVolumeCapture.phase` (латч `Complete`), а **не** condition: conditions на content — эксклюзивная зона агрегатора (INV-COND2), а «волна завершена» знает только snapshot-reconciler (владелец PVC-скоупа namespace). Reconciler — **единственный писатель** латча (status-поле, как `dataRef`/`manifestCheckpointName`; MergeFrom-патч, идемпотентно), штампует `Complete` во всех root-путях: capture (после финальной orphan-волны), import (точка успеха), static-bind (steady-state).
- Агрегатор — **чистый локальный читатель**: для root-кандидата (`!leaf(LabelChildVolumeNode) && spec.snapshotRef.kind==Snapshot` в группе `storage.deckhouse.io/v1alpha1`) при `phase != Complete` выставляет `Ready=False/ResidualVolumeCapturePending`; leaf / доменные / non-root content'ы и `phase==Complete` ногу не гейтят. Владельца агрегатор **не читает** (дискриминатор «это root» берётся из собственного `spec.snapshotRef.kind`).
- **Монотонность + upgrade-guard:** латч `Complete` назад не откатывается; если `Ready` **уже** персистнут `True`, гейт не применяется (блокируется только **первый** `Ready=True`, как монотонный `ManifestsArchived`), что исключает флап на rollout контроллера на уже-готовые корни.
- import/static-bind получают первый `Ready=True` чуть позже штампа (до него — `False/ResidualVolumeCapturePending`); это `False→True`, не флап.

`ManifestsArchived` (subtree-latch завершения доменной волны по **не-листовым** детям) этим гейтом **не затрагивается**: его ранний `True` — необходимый precondition запуска orphan-волны (по нему root понимает, что доменные дети сняли манифесты), а манифест orphan-PVC принадлежит листовому child-node, исключённому из subtree-latch. Гейтить `ManifestsArchived` на `phase==Complete` инвертировало бы конвейер (precondition зависел бы от своего же downstream-эффекта) и задержало teardown per-namespace capture-RBAC. Гарантию «первый `Ready=True` ⟹ данные готовы» даёт только residual-gate. Детали реализации — `api/storage/v1alpha1` `ResidualVolumeCaptureStatus` и `SnapshotContentController.computeResidualSweepGate`.

## 3. Размещение conditions (Snapshot vs SnapshotContent)

Асимметрия следует из архитектуры (§3.2.2 / §3.8 spec): `SnapshotContent` — долговечный SoT (переживает удаление `Snapshot`), `Snapshot.Ready` его зеркалит; барьер волны читает дочерний **`Snapshot`** (контента ребёнка может ещё не быть в момент планирования верхнего priority-слоя).

| Объект | Conditions | Кто пишет | Срок жизни |
|---|---|---|---|
| **`XxxxSnapshot`** (live) | `PlanningReady` (своё, барьер) и `Ready` (mirror `Content.Ready`) | доменный / snapshot-контроллер | live |
| **`SnapshotContent`** (durable) | `ManifestsReady`, `VolumesReady`, `ChildrenReady`, `Ready` | `SnapshotContentController` | durable, SoT для restore |

- На live `Snapshot` несём только `PlanningReady` (своё) и `Ready` (mirror контента). `ManifestsReady`/`VolumesReady`/`ChildrenReady` живут **только** на `SnapshotContent` и на `Snapshot` **не зеркалятся** (иначе появился бы второй контракт под-кондишинов на live-объекте).
- `PlanningReady` осмыслен только в live-фазе; на `SnapshotContent` он вырожден (implied самим фактом существования контента), поэтому на контенте не хранится.
- **Один писатель на объект**: snapshot-контроллер пишет все snapshot-side conditions; `SnapshotContentController` — все content-side. `SnapshotContentController` по §3.8 не читает/не пишет live `Snapshot`.
- Mirror на `Snapshot` приходит через content→snapshot watch (field-index `status.boundSnapshotContentName`); generic binder поллит (5s).

## 4. Инварианты (MUST)

- **INV-COND1 (gate-импликация):** `ManifestsReady=True ⇒ PlanningReady=True` и `VolumesReady=True ⇒ PlanningReady=True`. Requests не появляются без планирования. Этот инвариант держит совпадение `Snapshot.Ready == Content.Ready` при том, что `PlanningReady` не входит в формулу `Ready`.
- **INV-COND2 (один агрегатор):** `Ready` вычисляется ровно в одном месте — на `SnapshotContent` (`ManifestsReady && VolumesReady && ChildrenReady`). Везде остальное — mirror. Запрещён локальный пересчёт `Ready` по mirror-копиям под-кондишинов (избегаем двойной агрегации и stale-гонок).
- **INV-COND3 (generation-gating):** `PlanningReady` и `Ready` MUST нести `condition.observedGeneration == object.metadata.generation`. Без этого parent wave-barrier может принять устаревший `True`. Рекомендуется gen-gating и для `ManifestsReady`/`VolumesReady`/`ChildrenReady` (единообразие).
- **INV-COND4 (mirror, не пересчёт):** `Snapshot.Ready := mirror(SnapshotContent.Ready)` (status/reason/message копируются). Snapshot-контроллер не вычисляет собственный alternative reason.
- **INV-COND5 (well-defined legs):** линии узла определены, потому что на логический узел приходится **максимум один MCR + один VCR** (spec §3.9.5). `ManifestsReady = (MCR done|нет)`; `VolumesReady = (VCR done|нет)`.
- **INV-COND6 (вырождения):** leaf без детей → `ChildrenReady=True`; пустой MCP (0 объектов, но `manifestCheckpointName` присутствует и MCP `Ready=True`) → `ManifestsReady=True`; пустой `dataRefs[]` → `VolumesReady=True`.

## 5. Failure propagation (INV-FAIL1: ancestor-chain, без заражения siblings)

**INV-FAIL1.** Терминальный failure листа поднимается по **ancestor-chain** к root и **не** заражает sibling-ветки.

### 5.1. Целевое поведение

1. **Leaf** `SnapshotContent` с терминальным failure собственной линии (нет `ManifestCheckpoint`; MCP `Ready=False`; нет chunk; нет/сломан data artifact, напр. `VolumeSnapshotContent`; иной терминальный failure в манифестной или волюмной линии):
   - `ManifestsReady=False` или `VolumesReady=False` (и, как следствие, `Ready=False`) c конкретным reason;
   - `message` содержит конкретный affected object: `kind/name` или `targetUID`/`dataRef` id;
   - `Ready=False`.
2. **Все ancestor** `SnapshotContent` по пути к root:
   - `ChildrenReady=False`, `Ready=False`, reason `ChildrenFailed`;
   - `message` указывает конкретного failed-потомка (минимум: имя failed child `SnapshotContent`; имя failed leaf, если глубже одного уровня; исходный reason/message листа), а не «child is not ready».
3. **Sibling-ветки не меняют состояние**: sibling `SnapshotContent` остаётся `Ready=True`; sibling `Snapshot`/domain snapshot остаётся `Ready=True`, если его requests и дети готовы. Propagation идёт **только** по ancestor-chain от failed leaf к root.
4. **Root `Snapshot`** получает `Ready=False` как **mirror** root `SnapshotContent.Ready`. Snapshot-контроллер не пересчитывает tree readiness самостоятельно.
5. **Нет отдельного глобального condition** «subtree failed». Выражается только через: leaf `ManifestsReady=False`/`VolumesReady=False`/`Ready=False`; ancestors `ChildrenReady=False`/`Ready=False`.

### 5.2. Терминальный vs pending (классификация ребёнка)

Failure поднимается как **терминальный** (`ChildrenFailed`), pending — как `ChildrenPending`. Множество терминальных content-reason'ов (по аналогии с `usecase.ChildSnapshotTerminalReadyReasons`):

- `ManifestCheckpointFailed` (MCP `Ready=False/Failed`);
- `DataArtifactInvalid`, `DataArtifactNotSupported`;
- `ArtifactMissing` (durable artifact удалён/не найден — по политике трактуем как терминальный для уже опубликованного `dataRefs[]`);
- `ChildrenFailed` (унаследованный failure ниже по дереву).

`ArtifactNotReady` (VSC ещё не `readyToUse`) — **pending**, не терминал.

**Bridge-исключение для `Snapshot.Ready`.** Единственный не-mirror writer `Snapshot.Ready` — мост для терминального capture-failure дочернего **`Snapshot`**, который не может быть отражён ни одним `SnapshotContent` (ребёнок упал на планировании раньше, чем появился его контент). Терминальные child-Snapshot failure'ы вычисляет `usecase.SummarizeChildSnapshotTerminalFailures` (без pending-агрегации). Pending-состояния исключением **не** являются — они зеркалятся из `Content.Ready`.

## 6. Минимальная reason-модель

```text
PlanningReady:  True/Completed | False/Planning | False/PlanningFailed
ManifestsReady:         True/Completed | False/ManifestCapturePending | False/ManifestCheckpointFailed
VolumesReady:           True/Completed | Unknown/ManifestCapturePending | False/DataCapturePending | False/<DataArtifact*|ArtifactMissing>
ChildrenReady:          True/Completed | False/ChildrenPending | False/ChildrenFailed
Ready:                  True/Completed | False/<manifest|data leg reason> | False/ChildrenPending | False/ChildrenFailed | False/ResidualVolumeCapturePending
```

Приоритет reason у `Ready` (несёт один reason при нескольких упавших ногах):

```text
manifestsFailed > volumesFailed > childrenFailed > manifestsPending > volumesPending > childrenPending > residualVolumeCapturePending > Completed
```

(терминальные провалы первыми — actionable; свой узел перед детьми при равной тяжести; при сохранённом sequencing `volumesPending` выставляется только после готовности манифестной линии. `residualVolumeCapturePending` — низший по приоритету, **fail-closed** gate первого `Ready=True` только на namespace-root content (§2.3): любая обычная/терминальная нога его перекрывает.)

**Фаза «до контента»:** пока bound `SnapshotContent` ещё не создан (или у него ещё нет `Ready`), `Snapshot.Ready = False/ContentBindingPending` (локальный transitional pre-bind). После bind — только mirror.

### 6.1. Progress / degradation visibility

Дизайн: [`docs/internal/state-snapshotter-rework/design/status-propagation-and-visibility.md`](../docs/internal/state-snapshotter-rework/design/status-propagation-and-visibility.md). Одна модель, фазы: **Phase 1** — progress-aware `Ready=False` (богатые reason/message, счётчики прогресса, leaf-chain в `ChildrenFailed`, mirror на `Snapshot`); **Phase 2a** — MCP/VSC wake-up/revalidation watches для деградации артефактов после `Ready=True`. Pending и degradation используют один и тот же pipeline и таксономию reason'ов.

- **Pending reasons:** `ManifestCapturePending`, `DataCapturePending` (наружу для not-ready data; `ArtifactNotReady` остаётся внутренним/compat), `ChildrenPending`, `ResidualVolumeCapturePending` (только namespace-root, fail-closed gate первого `Ready=True` до латча `residualVolumeCapture.phase==Complete`, §2.3), `ContentBindingPending` (только pre-bind на `Snapshot`).
- **Failed reasons:** `ManifestCheckpointFailed`, `ArtifactMissing`, `DataArtifactInvalid`/`DataArtifactNotSupported`, `ArtifactFailed` (deferred — не используется в Phase 2a), `ChildrenFailed`.
- **Прогресс-сообщения:** `"<ready>/<total> ready"` + capped pending list (max 5, затем `" (+N more)"`).
- **Leaf-chain (только в content-агрегации):** канонический parseable формат `ChildrenFailed`:

```text
child SnapshotContent <direct-child> failed: leaf=<failed-leaf> reason=<original-reason> message=<original-message>
```

Родитель, видя терминального ребёнка с reason `ChildrenFailed`, переиспользует `leaf`/`reason`/`message` из его message и подставляет своего direct child; иначе ребёнок и есть failed leaf. `Snapshot` ничего не анализирует — только verbatim mirror (исключение — child-Snapshot bridge, §5.2).
- **Revalidation без watch (Phase 1):** на каждом reconcile `SnapshotContentController` пересчитывает линии; если опубликованный data artifact ref перестал резолвиться (был `Ready=True`, стал missing/failed) → `VolumesReady=False`/`ArtifactMissing` (для manifest-линии — `ManifestsReady=False`/`ManifestCheckpointFailed`) → `Ready=False` с kind/name в message.
- **Phase 2a — wake-up (truth=refs, ownerRef=routing, watch=enqueue-only):** события `ManifestCheckpoint`/`VolumeSnapshotContent` будят владельца **только** по `ownerRef → SnapshotContent`; handler не пишет conditions; broken/missing ownerRef логируется и дропается (correctness — на reconcile, `INV-RECONCILE-TRUTH`). **Без** reverse-index по `dataRefs[]` и **без** shortcut `chunk → SnapshotContent` (chunk, если когда-либо, — только `chunk → MCP → SnapshotContent`, `INV-OWNCHAIN`). **Без watermark:** классификация только по текущему состоянию артефакта (MCP NotFound → `ManifestCapturePending`; VSC NotFound → `ArtifactMissing`; VSC `readyToUse=false` → `DataCapturePending`; `ArtifactFailed` отложен). Content переустанавливает ownerRef VSC из `dataRefs[]` (self-heal, idempotent, best-effort, deleting VSC не патчится). Watches регистрируются c guard по RESTMapping.
- **Chunk existence (get-only, без list/watch):** при MCP `Ready=True` — exact GET по `MCP.status.chunks[]`; первый `NotFound` → `ManifestCheckpointFailed` (terminal, message `ManifestCheckpoint <mcp> references missing chunk <chunk>`). Только **existence**, без чтения/декода `.spec.data` и без проверки checksum (content-валидация — на read/download/archive, не на каждом reconcile); GET — metadata-only (`PartialObjectMetadata`), payload не тянется. GET через uncached reader (cached Get создал бы chunk-informer = неявный list/watch — запрещено); транзиентная ошибка → requeue, не terminal. **Chunk deletion не будит reconcile** (нет chunk-watch): correctness — на следующем reconcile; read/download/archive ловит ошибку немедленно. Content/consistency (checksum в conditions) и `chunk → MCP → content` wake-up — вне scope (отдельный ADR/RBAC).

## 7. Тест-план

### A. Unit (SnapshotContentController aggregation, depth ≥ 2)
- Дерево `root -> child-a -> leaf-broken` и `root -> child-ok`.
- `leaf-broken` имеет терминальный failure в artifact/request-линии.
- reconcile `leaf-broken`, `child-a`, `root`; assert:
  - `leaf-broken` `Ready=False` с исходным reason;
  - `child-a` `ChildrenReady=False`, `Ready=False`, reason `ChildrenFailed`;
  - `root` `ChildrenReady=False`, `Ready=False`, reason `ChildrenFailed`;
  - `message` root содержит имя `leaf-broken` и исходный reason;
  - `child-ok` `Ready=True`, его conditions не изменились.

### B. Unit (missing data artifact)
- leaf с `dataRefs[]` на VSC; VSC удалён/не найден;
- leaf `VolumesReady=False`/`Ready=False` reason `ArtifactMissing`;
- failure поднимается до root через `ChildrenReady=False`.

### C. Unit (missing ManifestCheckpoint/chunk)
- leaf с `manifestCheckpointName`; MCP отсутствует или `Ready=False/Failed`;
- leaf `ManifestsReady=False`/`Ready=False`;
- ancestor-chain `ChildrenReady=False`/`Ready=False`;
- sibling-ветка остаётся `Ready=True`.

### D. Mirror-test (Snapshot)
- root `Snapshot` bound на root `SnapshotContent`; контент `Ready=False/ChildrenFailed`;
- reconcile `Snapshot`; assert `Snapshot.Ready == mirror(content.Ready)` (reason/message совпадают);
- assert snapshot-контроллер не вычисляет собственный alternative reason.

### Acceptance criteria
- failure одного листа делает `Ready=False` только у самого leaf и всех его ancestors;
- sibling-ветки остаются `Ready=True`;
- root `Ready=False` содержит диагностически полезный путь/ID до failed leaf;
- нет нового condition сверх `PlanningReady` / `ManifestsReady` / `VolumesReady` / `ChildrenReady` / `Ready`;
- `Ready` вычисляется только на `SnapshotContent`, `Snapshot` — только mirror;
- тесты покрывают минимум: broken data artifact, broken manifest artifact, propagation глубже одного уровня (depth ≥ 2), неаффектнутый sibling.

## 8. Последствия

- Меньше conditions, понятнее UX, один источник истины готовности.
- Закрывается баг барьера для demo и потеря диагностики при failure-propagation.
- Потребовалась миграция тестов, ссылавшихся на удалённые conditions, и синхронизация spec/доков.

## Appendix A. Historical note (pre-PR2c)

Краткая фиксация состояния «до» этой модели. Здесь — единственное место, где допустимы старые имена; в основном контракте (выше и в spec) их нет.

- **Старый набор conditions** (`pkg/snapshot/conditions.go`): `GraphReady`, `Ready`, `Bound`, `InProgress`, `HandledByCustomSnapshotController`, `HandledByCommonController`, `ManifestsReady`, `DataReady`.
  - `ManifestsReady` / `DataReady` — константы, нигде не выставлялись.
  - `HandledByCustomSnapshotController` — только читался (барьер binder'а); demo-контроллеры шли мимо барьера (dedicated path), т.е. барьер для demo де-факто не работал.
  - `Bound` дублировал поле `status.boundSnapshotContentName`.
  - `InProgress` нёс нулевую информацию (`!Ready`).
  - Имя `GraphReady` не прошло демо у заказчика.
- **Два источника готовности**: классификация на стороне `Snapshot` (бывший «E6» в `internal/usecase/`) и агрегация на `SnapshotContent` — риск двух контрактов.
- **Выполнено в PR2a–PR2c:**
  - PR2a: `GraphReady → DomainReady`; барьер binder'а — `DomainReady=True` (вместо `HandledByCustomSnapshotController`).
  - PR2b: введены `RequestsReady`/`ChildrenReady`; единственный агрегатор `Ready` — на `SnapshotContent`; `Snapshot.Ready` — mirror.
  - PR2c: удалены `Bound`/`InProgress`/`HandledByCustomSnapshotController`/`HandledByCommonController`/`ManifestsReady`/`DataReady`; reasons `ChildSnapshotPending/Failed → ChildrenPending/ChildrenFailed`; idempotency binder'а — структурная (`status.boundSnapshotContentName`); бывшая E6 priority-matrix удалена, для bridge оставлен узкий helper терминальных child-failure'ов.
- **Позже: косметические rename'ы имени condition'а: `DomainReady → ChildrenSnapshotReady → PlanningReady`** (только имя condition'а, без изменения семантики/формул/инвариантов). Причина: имя не должно «протекать» внутренней (бизнес-)логикой исполнителя наружу, а `ChildrenSnapshotReady` вводило в заблуждение («дети готовы» вместо «узел спланирован»). Везде выше по тексту и в spec используется уже финальное имя `PlanningReady` (бывш. `ChildrenSnapshotReady`).
