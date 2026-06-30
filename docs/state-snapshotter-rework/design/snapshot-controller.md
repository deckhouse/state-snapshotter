# Snapshot controller — MVP design

## 1. Status of this document

**Тип:** design — план поставки и инженерные инварианты под реализацию. **Продуктовый сценарий и примеры YAML** — SSOT в [`snapshot-rework/`](../../../snapshot-rework/) (в т.ч. [`2026-01-25-snapshot.md`](../../../snapshot-rework/2026-01-25-snapshot.md)); эти файлы **не правятся** в рамках согласования через `docs/`. После стабилизации — выдержки в [`spec/system-spec.md`](../spec/system-spec.md) и тесты.

**Связанные документы:**

- Пара root/content + отказ от generic `SnapshotContent` для namespace root: [`decisions/snapshot-content-decision.md`](decisions/snapshot-content-decision.md)
- Поверхность статуса (без `status.phase`): [`decisions/snapshot-status-surface.md`](decisions/snapshot-status-surface.md)
- **Scope (resolved):** namespaced root — [`decisions/snapshot-scope.md`](decisions/snapshot-scope.md)

---

## 2. Scope and non-goals

### 2.1 Целевая семантика (согласовано с ТЗ)

**Snapshot** — namespaced root / intention; результат фиксируется в cluster-scoped **`SnapshotContent`** (пара 1:1 с root по смыслу ТЗ). Generic **`SnapshotContent`** для этого потока **не** используется как root-carrier. Связка с **ObjectKeeper** — по правилам из `snapshot-rework`. **N2a в коде сейчас:** cluster-scoped root OK **`ret-snap-<namespace>-<snapshotName>`** в режиме **`FollowObjectWithTTL`** на **root `Snapshot`** (UID в `followObjectRef`); **root `SnapshotContent.metadata.ownerReferences` → этот ObjectKeeper** (**controller: true**); root Snapshot is not owned by cluster-scoped OK. Это TTL-якорь: после удаления root snapshot OK остаётся до истечения TTL; Deckhouse ObjectKeeper controller удаляет OK → GC снимает retained SnapshotContent и каскадно MCP/детей. TTL задаётся конфигом контроллера (`SnapshotRootOKTTL`). **Временный** `ManifestCaptureRequest` (MCR) для capture после успешного завершения **и handoff MCP на `SnapshotContent`** получает `Ready=True` и только затем **удаляется** NS-контроллером (см. §4.7).

**Поставка поэтапно** (см. §16 и [`implementation-plan.md`](implementation-plan.md)): **N1** — skeleton bind/delete без OK и без real capture; **N2a** — manifests-only один root + OK + внутренний **MCR→ManifestCheckpoint** + download; **N2b** — **дерево** manifests-only (дети, refs, aggregated Ready/download); data-layer, полный export/import/restore — после N2, **не** переписывая SSOT в `snapshot-rework`.

- **Capture** тяжёлой работы: для manifests в **N2a** — через **внутренний** путь **ManifestCaptureRequest → ManifestCheckpoint** (существующий контроллер/chunks); root-контроллер **не** обязан держать весь list/capture в одном reconcile, но **не** обязателен и отдельный Job, если исполнение согласовано с MCR-потоком.
- **Unified shared runtime** там, где применим общий паттерн Snapshot/SnapshotContent для **других** GVK; для пары namespace root используем **свой** content kind (`SnapshotContent`), см. [`snapshot-content-decision.md`](decisions/snapshot-content-decision.md).

### 2.2 Out of scope на ранних milestones (N1–N2)

**N2** по [`implementation-plan.md`](implementation-plan.md) §2.4.1 — **только manifests-only**: **N2a** (один root) + **N2b** (дерево). Явно **вне N2:** volume/data snapshots, реальный VCR/VSC, restore с данными, полный export/import продукта, storage class remap, VM data restore, выдача поддерева с **data payloads**.

До полного сценария ТЗ в одном релизе (SSOT в `snapshot-rework`):

- **Child composition (manifests-only)** — целевая ось **N2b**, не «опциональный потом».
- CSD priority traversal **в полном объёме**, data-layer — после закрытого N2 / в **N5** по плану.
- Экспорт/импорт архива и CLI как в полном ТЗ — не обещать в рамках N2.
- **`ManifestCaptureRequest` как обязательный публичный контракт** для оператора — см. §10; публично: root + **SnapshotContent** + artifact metadata; MCR/MCP — **внутренний** execution path в **N2a**.

---

## 3. Relationship to `snapshot-rework/` (SSOT)

Сценарий, иерархии, ObjectKeeper, примеры манифестов — **только** по [`snapshot-rework/`](../../../snapshot-rework/). Настоящий файл задаёт **порядок внедрения**, инварианты binding/deletion/Ready и ссылки на decisions; при противоречии сначала **уточняют ТЗ** в `snapshot-rework`, затем обновляют этот design и код.

---

## 4. API model

### 4.1 Snapshot (черновик полей)

**Scope:** namespaced root — зафиксировано в [`decisions/snapshot-scope.md`](decisions/snapshot-scope.md). Resolved target namespace совпадает с `metadata.namespace` объекта `Snapshot`; `Snapshot` остаётся namespaced built-in snapshot type.

**Spec (логически):**

- Источник namespace для capture: resolved target namespace; сейчас это `metadata.namespace` root (расширение через отдельное поле — позже, если понадобится продуктово).
- Опционально: include/exclude групп ресурсов (MVP — минимальный набор или фиксированный профиль).
- Опционально позже: `capturePolicy` (см. §9); в MVP допустимо заложить поле, но **выставить только fail-closed**.

**Default exclusions / GVR до реального capture (N1→N2):** в CRD на этапе N1 **нет** полей `includedResources` / `excludedResources` у root — пользовательский allow/deny **не задаётся**. Пока capture остаётся placeholder/fake, **исключения не применяются**. С включением **реального** capture (N2+) **по умолчанию fail-closed**: без явно разрешённого набора GVR (через class/profile или зафиксированный built-in минимум в коде — конкретизация в N2) **запускать** list/capture **нельзя**; произвольный «снимать всё» без политики не допускается.

**Status (логически):**

- **`conditions` — единственный** нормативный источник жизненного цикла и ошибок для операторов; поля **`status.phase` нет** (см. [`decisions/snapshot-status-surface.md`](decisions/snapshot-status-surface.md)).
- `conditions`: как минимум согласованный набор с unified-паттерном (например Ready, Bound, Progressing; при необходимости CaptureStarted, ArtifactStored; терминальные сбои — через Ready=False и reason).
- Имя привязанного content-объекта snapshot-линии — в CRD: **`status.boundSnapshotContentName`** (cluster-scoped имя; для этой линии фактический kind — **`SnapshotContent`**, не выводится из имени поля).
- `observedGeneration`.
- `startedAt`, `completedAt` (опционально).
- Сводные поля прогресса/ошибки по необходимости (без дублирования детального состояния Job).

### 4.2 SnapshotContent binding (контракт)

**Root → Content**

- В status root — **`status.boundSnapshotContentName`**: cluster-scoped имя привязанного **`SnapshotContent`** (единое имя поля для всех snapshot root’ов в модуле).

**Content → Root**

- В `Snapshot.status`: **`boundSnapshotContentName`** хранит имя cluster-scoped `SnapshotContent`; `SnapshotContent.spec` не содержит reverse-ссылки на root snapshot.

**Инварианты**

1. Пока Snapshot **не** в согласованном терминальном состоянии по conditions для данного поколения spec, он связан **не более чем с одним** SnapshotContent.
2. SnapshotContent **не перепривязывается** к другому root.
3. После рестарта контроллер восстанавливает связь по status root, ref на content в spec/status content, или **детерминированному имени** content.

**Идемпотентность**

- Операции ensure: финализатор, создание/поиск content, ObjectKeeper, запуск capture, запись статуса — безопасны при повторной доставке события и после рестарта.

**Нейминг**

- Рекомендация: стабильное имя content от **UID** root (см. ТЗ и существующие паттерны в коде).

**Уникальность и коллизии**

- Новый UID root → новый content; совпадение `metadata.name` у пользователя **не** означает reuse content.

Точная схема полей — в CRD и в `system-spec.md` после выравнивания с ТЗ.

### 4.3 ObjectKeeper, ownerReference и границы scope

Полная схема полей OK — в [`snapshot-rework/2026-01-25-snapshot.md`](../../../snapshot-rework/2026-01-25-snapshot.md) и ADR там же. Ниже — **design lock** для N2a/N2b, согласованный с текущим кодом manifest-линии:

- **Единый путь MCR** в `ManifestCheckpointController` (INV-EXECUTION-OK): для **любого** MCR создаётся UID-aware execution OK **`ret-mcr-<hash>`** в **FollowObject** на MCR; **ManifestCheckpoint** создаётся с `ownerReference` → этот execution OK (**controller: true**); **chunks** → MCP. Контроллер запроса **не** зависит от существования SnapshotContent и **не** создаёт артефакты, сразу принадлежащие SnapshotContent.
- **Handoff (INV-HANDOFF):** передачу владения MCP с execution OK на **SnapshotContent** делает **только** `SnapshotContentController` (`validateCommonContentManifestCheckpoint` → `ensureManifestCheckpointOwnedByContent`), когда опубликован `status.manifestCheckpointName` и MCP Ready. После handoff MCP принадлежит SnapshotContent и переживает удаление MCR; execution OK (FollowObject → MCR) собирается внешним ObjectKeeper-контроллером после удаления MCR. Прежняя namespace-N2a ветка с аннотацией `state-snapshotter.deckhouse.io/bound-snapshot-content` (MCP сразу `ownerReference → SnapshotContent`, без execution OK) **удалена** — поведение унифицировано.

#### 4.3.1 Правило: ownerReference vs ObjectKeeper

- **`ownerReference`** используем там, где объекты в **совместимом scope** для Kubernetes GC (например оба cluster-scoped): **ManifestCheckpointContentChunk → ManifestCheckpoint** — chunks удаляются через ownerRef на MCP (как сейчас в коде).
- **ObjectKeeper** используем там, где нужен **cluster-scoped retention anchor** (TTL на follow root snapshot): логический bind **root → SnapshotContent** остаётся в **status.boundSnapshotContentName**; **удержание retained SnapshotContent** — через **`SnapshotContent.ownerReferences` → root OK** (не наоборот).
- **ObjectKeeper нигде не подменяет** bind-контракт **`status.boundSnapshotContentName`**.

#### 4.3.2 Два применения ObjectKeeper (не смешивать)

1. **Root / snapshot (N2a, `snapshot_capture.go`):** cluster-scoped OK **`ret-snap-…`**: **`FollowObjectWithTTL`** на **root `Snapshot`**; сам root `Snapshot` не owned by OK; **root `SnapshotContent`** имеет **`ownerReferences` → этот OK** (**controller**). TTL из конфига. Это **не** follow на MCR и **не** generic `ret-mcr-*`. После удаления root снимка при **Retain** SnapshotContent (и MCP) остаются, пока жив OK; после TTL — удаление OK → GC SnapshotContent и каскад вниз — зона Deckhouse ObjectKeeper controller и политики модуля.
2. **Manifest capture (generic MCR-путь):** UID-aware OK **`ret-mcr-*`** в **FollowObject** на **ManifestCaptureRequest**; MCP через ownerRef на этот OK. Для **namespace N2a** этот путь **не** используется для финального MCP (см. вводный абзац §4.3).

**Инвариант:** **ManifestCaptureRequest** в N2a имеет **`metadata.ownerReferences` → `Snapshot`** (**controller: true**, тот же namespace), чтобы при **полном удалении** root из API garbage collector убрал «зависший» in-flight MCR без отдельного `Delete` из `reconcileDelete`. MCR по-прежнему создаётся **Snapshot controller** на время capture; **ManifestCheckpointController** исполняет **MCR → MCP** (+ chunks) и ставит `MCR Ready=True` только после того, как MCP готов и имеет ownerRef на `SnapshotContent`. **Snapshot controller** публикует `SnapshotContent.status.manifestCheckpointName` и пишет статус root snapshot, а **SnapshotContentController** валидирует refs и выставляет `Ready`; после `MCR Ready=True` MCR **удаляется** явно контроллером (§4.7) — совместимо с ownerRef; публичная «истина» — **SnapshotContent + `manifestCheckpointName` + MCP**.

**Удержание MCP/chunks (N2a namespace path):** после успеха — **ownerRef MCP → SnapshotContent** и политика **Retain/Delete** на SnapshotContent; **не** цепочка MCR→OK→MCP. Generic путь с `ret-mcr-*` остаётся для других вызывающих MCR, не для финального namespace-bound MCP.

**N2a.x:** сверка с кодом зафиксирована в **§4.6**; при изменении `ManifestCheckpointController` обновлять §4.3 и §4.6.

---

### 4.4 N2a — публичный status surface (design lock)

Цель: финальный результат не зависит от **ManifestCaptureRequest**; request name остаётся временным orchestration status.

**`Snapshot.status` (N2a, публично):**

- **`boundSnapshotContentName`** (уже есть) + **`manifestCaptureRequestName`** (temporary execution request) + **`conditions`** (+ при необходимости **`observedGeneration`**, временные метки — по мере появления в CRD).
- MCR — implementation detail execution object; после публикации `SnapshotContent.status.manifestCheckpointName` и ownerRef handoff request удаляется, а `manifestCaptureRequestName` очищается.

**`SnapshotContent.status` (N2a, публично):**

- **`manifestCheckpointName`** (cluster-scoped имя готового **ManifestCheckpoint**) — каноническая ссылка на persisted manifest-результат.
- **`conditions`** (в т.ч. готовность к download, ошибки capture).
- Опционально для UX/наблюдаемости: **`capturedAt`**, **`resourceCount`** (или эквивалент из metadata MCP), **`artifactFormatVersion`** / `formatVersion` — если нужны до расширения spec; имена полей — финализировать в CRD и `system-spec.md`.

**Источник `Ready=True` на root (N2a):** root snapshot зеркалит **SnapshotContent Ready**. SnapshotContent становится Ready только после persisted результата (MCP + согласованные chunks + ownerRef на SnapshotContent) и согласованных **conditions**, а не по факту «создали MCR».

**Минимальный первый PR по CRD (N2a, не раздувать):** в **`SnapshotContent.status`** добавить **`manifestCheckpointName`** (string, omitempty) рядом с уже существующими **`conditions`**. Поля **`capturedAt`**, **`resourceCount`**, **`observedGeneration`** на SnapshotContent — **отложить** до отдельного PR/ADR, если достаточно conditions + `manifestCheckpointName`. На **`Snapshot.status`** без изменений объёма: уже есть **`observedGeneration`**, **`boundSnapshotContentName`**, **`conditions`** — публично **без** MCR (§4.4).

---

### 4.5 Namespace capture — discovery + единое правило включения (SSOT)

**Перечисление видов — через discovery, не статический allowlist.** Захват больше не опирается на захардкоженный список GVR (`n2aNamespacedGVR` удалён). `BuildManifestCaptureTargets` перечисляет **все** namespaced-виды через `discovery.ServerPreferredNamespacedResources` (preferred version каждого ресурса, verbs ⊇ {`list`,`get`,`watch`}, без subresource) и листит каждый в целевом namespace динамическим клиентом. Это гарантирует, что произвольные CR (например, оператор-провижененные ресурсы без CSD-маппинга) не теряются.

**Требование verb `watch` исключает виртуальные/вычисляемые ресурсы.** Признак `watch` — это явный discovery-сигнал «настоящий хранимый desired-state vs computed-on-read». Любой etcd-backed ресурс (все CRD, встроенные виды, агрегированные API с реальным хранилищем) поддерживает `watch`; виртуальные ресурсы aggregation-слоя (`metrics.k8s.io`/`PodMetrics`/`NodeMetrics`, `custom/external.metrics.k8s.io`) отдают только `get,list` — их нельзя восстановить, и они racy при capture (имя, видимое в LIST на этапе планирования, может отсутствовать при GET-by-name в MCR-валидации → reject всего MCR). Признак режет именно по оси persisted/virtual, а не по «Local vs aggregated»: агрегированные, но реально хранимые ресурсы поддерживают `watch` и **остаются** в снимке. Verbs у CRD задаёт apiserver фиксированным набором (включая `watch`) — автор CR **не может** отключить `watch`, поэтому пользовательские манифесты этим фильтром не теряются.

**Единое правило включения — `ShouldIncludeNamespaceObject` (default-include).** Объект попадает в снимок, если нет доказуемого сигнала исключения. Сигналы исключения (единственное место — `pkg/namespacemanifest/targets.go`):

- **controller-`ownerReference`** (`controller: true`) — derived/runtime-состояние, которое владелец пересоздаёт после restore (backing Pod демо-VM, ReplicaSet/Pod от Deployment, domain-owned PVC — последняя захватывается отдельным volume-node путём);
- **control-plane noise denylist** (регенерируется контрол-плейном, не пользовательский desired-state):
  - kind-level: `Event` (`v1` + `events.k8s.io`), `v1` `Endpoints`, `coordination.k8s.io` `Lease`, `cilium.io` `CiliumEndpoint` (эфемерный per-pod runtime-объект, имя = имя пода; cilium-агент пересоздаёт его при churn подов — бессмыслен для restore и racy при capture, как и виртуальные метрики);
  - object-level: `ConfigMap` `kube-root-ca.crt`; `ServiceAccount` `default`; `Secret` типа `kubernetes.io/service-account-token`;
  - примечание: виртуальные ресурсы aggregation-слоя (`metrics.k8s.io` и т. п.) исключаются **не** здесь, а раньше — на этапе перечисления через отсутствие verb `watch` (см. абзац про discovery выше);
- **снапшотная машинерия (механизм 1, kind-level)** — виды, которые снапшоттер создаёт сам: CSI `VolumeSnapshot` плюс все зарегистрированные в живом `snapshot.GVKRegistry` snapshot/content-виды (встроенные + выведенные из CSD, напр. `VirtualMachineSnapshot`; root `Snapshot` тоже входит сюда). Набор GVK прокидывается из Snapshot-контроллера; **fail-closed**, если реестр ещё не построен (`ErrGraphRegistryNotReady` → requeue, чтобы не захватить собственные снапшот-объекты);
- **собственная machinery state-snapshotter** — вся API-группа `state-snapshotter.deckhouse.io` (MCR/MCP/chunk) и машинерные виды `state-snapshotter.deckhouse.io` (`VolumeCaptureRequest`, `VolumeRestoreRequest`, `DataExport`, `DataImport`);
- **Deckhouse-managed объекты по лейблу `heritage=deckhouse`** — модульная/платформенная machinery (реконсилится модулями Deckhouse), не пользовательский desired-state. Это широкий group-agnostic сигнал (`isDeckhouseManagedObject`), который ловит транзиентный per-namespace capture `RoleBinding` (`d8-state-snapshotter-capture`, чужая API-группа `rbac.authorization.k8s.io`, поэтому group-фильтр выше его не видит), а также любые другие Deckhouse-managed helper-объекты. Хук `040-namespace-capture-rbac` создаёт capture `RoleBinding` на время capture и удаляет его после `ManifestsArchived=True`; оба RBAC-хука (`030-domain-rbac`, `040-namespace-capture-rbac`) ставят `heritage=deckhouse` на создаваемые объекты. Пользовательские `RoleBinding` без этого лейбла остаются desired-state.

**Дедуп объектов, уже снятых дочерними доменными снимками (механизм 2, object-identity)** — `root_capture_run_exclude.go` вычитает ключи `apiVersion|kind|ns|name` из MCP дочерних content-узлов (+ predictive owned-PVC exclude). Это owner-независимо (обычный ConfigMap, попавший в дочерний VM-снимок, вычитается из root MCR). Обход поддерева идёт по **опубликованным** рёбрам `SnapshotContent.status.childrenSnapshotContentRefs`, поэтому корректность exclude зависит от того, что к моменту root capture поддерево **полностью связано рёбрами** (иначе непривязанный внук не будет посещён и его объект попадёт в root MCR повторно → `409 duplicate object`).

**Wave-барьер по `ManifestsArchived` (надёжность дедупа).** Чтобы exclude всегда строился по полному поддереву, root MCR создаётся только после того, как каждый **прямой** ребёнок root-снимка имеет `SnapshotContent.ManifestsArchived=True` (`requireContentManifestsArchived`). Если ребёнок ещё не заархивирован → `ErrSubtreeManifestCapturePending` (requeue); терминальный `ManifestsArchiveFailed` → `ErrSubtreeManifestCaptureFailed`. Собственный `ManifestsArchived` рута в барьер **не** входит: он становится `True` только после создания и обработки root MCR (own `ManifestsReady`) и архивации детей, т. е. это **выход** root capture, а не вход — иначе цикл/дедлок. Рут не особенный: его латч считается тем же путём content-контроллера, что и у любого узла.

**Надёжность латча `ManifestsArchived` (fail-closed против declared-but-unlinked).** `ManifestsArchived` — однонаправленный subtree-латч (не переоткрывается), поэтому он обязан стать `True` только когда поддерево действительно полно. `childrenSnapshotContentRefs` публикуется **атомарно-целиком** (`PublishSnapshotContentChildrenFromSnapshotRefs`: либо все заявленные дети резолвятся в bound content и публикуется полный набор, либо ничего), поэтому до публикации view «дочерних рёбер» пуст и наивная агрегация по опубликованным рёбрам могла бы выдать `True` преждевременно. `aggregateChildrenManifestsArchived` сверяет **заявленный** набор детей (через `spec.snapshotRef` → owning snapshot → `status.childrenSnapshotRefs`, без visibility-leaf) с **опубликованными** рёбрами: латч может стать `True` только если каждый заявленный non-leaf ребёнок зарезолвлен, привязан, **слинкован** в `childrenSnapshotContentRefs` И сам `ManifestsArchived=True`. Любой нерезолвленный/непривязанный/неслинкованный заявленный ребёнок = pending (не failure). Тем самым «прямой ребёнок `ManifestsArchived=True`» транзитивно гарантирует, что всё его поддерево заархивировано и связано рёбрами — что и делает wave-барьер выше корректным.

**Fail-closed на неполноту (transient).** Если какой-то namespaced-вид нельзя прочитать из-за `Forbidden` (транзиентный per-namespace `RoleBinding` ещё не распространился) или из-за частичного отказа discovery (битый aggregated APIService), его GVR возвращается в `unreadable`. Контроллер **не** создаёт root MCR с неполным планом: ставит `Ready=False`/`NamespaceCaptureIncomplete` и **requeue** (ждёт RBAC), не terminal. Три класса ошибок листинга: Forbidden/partial-discovery → `NamespaceCaptureIncomplete` (transient); транзиентные ошибки apiserver (429/503/500/timeout/EOF) → requeue; только структурные/неожиданные → terminal `ListFailed`. RBAC модель — §RBAC / Phase 5.

#### 4.5.0 Raw capture policy (источник истины — MCP)

Snapshot-capture stores manifests **as-is** in `ManifestCheckpoint` (MCP), **including `status`** and runtime fields. MCP is the source of truth for import/export: `DataImport`/`DataExport` read original fields (e.g. `status.capacity`, `spec.storageClassName`, `spec.volumeMode`) directly from the stored manifest.

- **Object selection** (`ShouldIncludeNamespaceObject`: dropping controller-owned dependents, control-plane noise, snapshot/own machinery) is a **separate** layer applied on capture; it does **not** mutate object fields.
- **Field-level sanitization** (stripping `status`, `metadata.managedFields`, `resourceVersion`, `uid`, `creationTimestamp`, etc.) happens **only on the restore read-path** (`internal/usecase/restore`), independent of capture.
- There are **no** field-level exceptions on capture — see §4.5.1.

#### 4.5.1 Secret handling in ManifestCheckpoint

`Secret` objects are captured **verbatim**, like every other object: their `.data`/`.stringData` are stored as-is in `ManifestCheckpoint`. There is no secret-specific selection or field stripping on the capture path.

- All selected `Secret`s (any `type`) are stored raw with their bytes intact.
- `Secret`s excluded by the unified inclusion rule (`ShouldIncludeNamespaceObject`) — e.g. `kubernetes.io/service-account-token` secrets — are not captured at all; this is object selection, not field stripping.

> At-rest encryption of the snapshot store is a separate, future concern. The legacy opt-in annotations (`state-snapshotter.deckhouse.io/include-secret`, `…/include-secret-data`) and the secure-by-default stripping have been removed.

#### 4.5.2 Что попадает в снимок namespace (сводка)

Модель — **default-include**: в снимок попадает **любой** namespaced-объект целевого namespace, кроме явно исключённого. Отдельного allowlist «снимать только это» нет — захватывается весь пользовательский desired-state, включая произвольные CR без CSD-маппинга.

**Попадает в снимок (примеры):**

- встроенные пользовательские ресурсы: `ConfigMap`, `Secret` (любого типа, кроме service-account-token), `Service`, `PersistentVolumeClaim`, `ServiceAccount` (кроме `default`), `Pod` без controller-владельца (standalone) и т. п.;
- workload-объекты верхнего уровня, создаваемые пользователем: `Deployment`, `StatefulSet`, `DaemonSet`, `Job`, `CronJob` и т. п. (их производные — `ReplicaSet`/`Pod` — исключаются как controller-owned, владелец пересоздаст их при restore);
- любые **CRD-ресурсы** в namespace (операторные/вендорные CR), в т. ч. агрегированные API с реальным хранилищем (поддерживают `watch`);
- сетевые/политики/прочее: `Ingress`, `NetworkPolicy`, `Role`, `RoleBinding` и др.

**НЕ попадает в снимок:**

| Категория | Что именно | Почему |
|-----------|------------|--------|
| Производные (controller-owned) | объекты с `ownerReference.controller: true` (`ReplicaSet`/`Pod` от Deployment, backing-Pod демо-VM, domain-owned PVC) | владелец пересоздаёт их при restore; PVC данных захватывается отдельным volume-путём |
| Control-plane noise | `Event`, `Endpoints`, `Lease`, `CiliumEndpoint`, `ConfigMap/kube-root-ca.crt`, `ServiceAccount/default`, service-account-token `Secret` | регенерируется контрол-плейном/CNI, не пользовательский desired-state |
| Виртуальные/computed | `metrics.k8s.io` (`PodMetrics`/`NodeMetrics`), `custom/external.metrics.k8s.io` | не хранятся (только `get,list`, без `watch`), невосстановимы |
| Снапшотная машинерия | CSI `VolumeSnapshot`, все snapshot/content-виды из `snapshot.GVKRegistry` (вкл. root `Snapshot`, `VirtualMachineSnapshot` и пр.) | создаются самим снапшоттером (self-referential) |
| Deckhouse-managed (`heritage=deckhouse`) | capture `RoleBinding` `d8-state-snapshotter-capture` и прочие объекты с `heritage=deckhouse` | модульная machinery Deckhouse (вкл. транзиентный RBAC снапшоттера), не desired-state |
| Собственная машинерия | вся группа `state-snapshotter.deckhouse.io` (MCR/MCP/chunk) + `state-snapshotter.deckhouse.io` (`VolumeCaptureRequest`, `VolumeRestoreRequest`, `DataExport`, `DataImport`) | внутренние execution/transfer-объекты |
| Уже снятое дочерними снимками | объекты, попавшие в MCP дочерних доменных content-узлов (механизм 2, object-identity) | дедуп, чтобы не дублировать в root |

> Примечание про «ресурсы, создаваемые Deckhouse»: специального исключения «по владельцу Deckhouse» нет. Deckhouse-managed объекты отсекаются теми же общими сигналами: либо они controller-owned (пересоздаются модулем), либо это control-plane noise, либо это наша/снапшотная машинерия. Всё остальное в namespace — в том числе ресурсы, которые лишь *настроены* пользователем поверх модулей, — считается desired-state и попадает в снимок.

---

### 4.6 N2a.x — сверка manifest-линии с §4.3 (выполнено по коду репозитория)

Проверено против `images/state-snapshotter-controller/internal/controllers/manifestcheckpoint_controller.go` и `snapshot_capture.go`:

| Утверждение §4.3 | В коде |
|------------------|--------|
| Единый путь: execution ObjectKeeper для MCR в **`FollowObject`** (следует за UID MCR) для **любого** MCR | Да (`ret-mcr-*`). |
| Имя OK **`ret-mcr-<hash(namespace/name/uid)>`** | Да. |
| **ManifestCheckpoint** **ownerReference → execution ObjectKeeper** `ret-mcr-…` (controller) на создании | Да. |
| Handoff MCP **ownerReference → `SnapshotContent`** делает только `SnapshotContentController` (после Ready + опубликованного `status.manifestCheckpointName`) | Да (`ensureManifestCheckpointOwnedByContent`). |
| **Chunks** **ownerReference → ManifestCheckpoint** | Да (поток create chunks). |
| Root OK **`ret-snap-…`**: **`FollowObjectWithTTL`** на **`Snapshot`**; **SnapshotContent `ownerReferences` → этот OK** | Да, `snapshot_capture.go` (`ensureSnapshotRootObjectKeeper`). |

Итог: **единый manifest-путь** (execution OK → MCP → handoff на SnapshotContent) и root OK — как в §4.3. При смене логики в коде — обновлять §4.3 и эту таблицу.

---

### 4.7 N2a — корреляция Snapshot ↔ MCR ↔ MCP и жизненный цикл MCR

**Цель:** во время capture — детерминированно находить MCR/MCP без list по кластеру; **после успешного завершения** — не опираться на существование MCR: публичная и операционная опора на **SnapshotContent + MCP** (и root OK по §4.3.2).

**Во время capture (временный MCR):**

1. **Имя `ManifestCaptureRequest`:** детерминированно от **`Snapshot.metadata.uid`**: **`snap-{metadata.uid}`** (UID с дефисами — допустимое имя). Namespace MCR = **`metadata.namespace` того же `Snapshot`**. **`Get`** по этому ключу — основной путь ensure.
2. **Имя `ManifestCheckpoint`:** от **UID экземпляра MCR** (после `Create`), та же формула, что в `ManifestCheckpointController` / `pkg/namespacemanifest` — префикс **`mcp-`** + **16 hex** от SHA256(UID MCR) (первые 8 байт). Общая функция в коде — без дублирования строки алгоритма.
3. **Labels (дополнительно):** **`state-snapshotter.deckhouse.io/snapshot-uid=<root-uid>`**. **`metadata.ownerReferences` → `Snapshot`** (**controller: true**) — очистка in-flight MCR при полном удалении root из API (GC); у существующего MCR без ref — **patch** (merge) при reconcile; иной **`Snapshot`** в ownerReferences того же MCR — ошибка. (Аннотация `bound-snapshot-content` удалена вместе со снапшот-bound веткой MCP — связывание идёт через `status.manifestCheckpointName` на SnapshotContent и handoff.)
4. **Stale / пересоздание root с тем же `metadata.name`:** если MCR **`snap-{uid}`** существует, но label **snapshot-uid** не совпадает с текущим root UID — MCR **чужой**; ошибка (см. тест recreate). **Root OK** `ret-snap-…`: same namespace/name root Snapshot cannot reuse retained root ObjectKeeper if that ObjectKeeper follows an old Snapshot UID; новый run должен fail closed или дождаться удаления/истечения старого OK.

**После успешного capture (стабильное состояние N2a):**

5. **Порядок в коде:** MCP готов → **Snapshot controller** публикует **`SnapshotContent.status.manifestCheckpointName`**, **SnapshotContentController** гарантирует **ownerRef MCP → SnapshotContent** и пишет `Ready`; **ManifestCheckpointController** после handoff ставит **`MCR Ready=True`**; root snapshot **`Ready`** зеркалит bound SnapshotContent → **`Delete(ManifestCaptureRequest)`** по имени из п.1 (**NotFound** считается успехом). **MCP и chunks не удаляются** этим шагом. MCP может сохранять историческую ссылку на удалённый MCR; на жизнь MCP это не влияет после ownerRef на SnapshotContent.
6. **Идемпотентность / гонки:** повторный reconcile не должен **пересоздавать** MCR, если на SnapshotContent уже зафиксирован готовый MCP (см. логику в `snapshot_capture.go`: быстрый путь только при **отсутствии** MCR в API; при `NotFound` MCR и уже сохранённом MCP — не создавать новый request).

**Поведение до завершения capture:**

7. **Point-in-time / frozen `spec.targets`:** снимок — это point-in-time. Полный list namespace выполняется **ровно один раз** — только когда рабочего MCR ещё нет (gate `OR(capture done, MCR существует)` в `reconcileCaptureN2a`, существование MCR читается через `APIReader`). Как только MCR создан, его `spec.targets` **заморожены** и **не сравниваются** с live namespace и **не переписываются** (никакого continuous drift). Прежнее состояние **`CapturePlanDrift`** для корня **больше не возникает** (логика дрейфа удалена). Пересоздание плана происходит только через транзиентные delete-пути (`unreadable`/subtree-pending удалили MCR → на следующем reconcile свежий list). Стабильный порядок targets — отсортированный набор (`BuildManifestCaptureTargets`, параллельный list-sweep). Перед единственным list — RBAC-гейт `SelfSubjectAccessReview(verb=list, group=*, resource=*, ns)`, который убирает гонку с асинхронной выдачей per-namespace capture RoleBinding.

**Публичный статус root** — без полей MCR (§4.4). **Каноническая ссылка на артефакт** после success — **`SnapshotContent.status.manifestCheckpointName`**.

---

## 5. Reconcile model

### 5.1 Normal flow (логические шаги)

**Watch/requeue:** контроллер подписан на **`Snapshot`**; `SnapshotContent` не содержит reverse-ссылки на root, поэтому mirror `Snapshot Ready` поддерживается reconcile/requeue логикой root controller, пока bound content pending.

1. Fetch; при удалении — §5.2.
2. Ensure finalizer на root.
3. Validation (namespace существует, class/policy валидна, spec консистентен) — §7.
4. Ensure **SnapshotContent** + запись bind в status root и ссылки root↔content — §4.2; ensure **ObjectKeeper** по ТЗ — §4.3 (**N2a+**).
5. **Reconcile capture** (N2a): ensure временного **MCR** → observe **MCP** через manifest-линию; при альтернативной реализации — Job/runner по [`implementation-plan.md`](implementation-plan.md) §2.4.1.
6. По успеху: записать **`manifestCheckpointName`** и условия на **SnapshotContent**, root **`Ready`** зеркалит bound content, затем **удалить MCR** только после `MCR Ready=True` (§4.7).
7. Не выполнять тяжёлую работу list/watch namespace в горячем пути, если она перенесена в MCR/controller capture path (политика ресурсов apiserver).

### 5.2 Deletion flow

Учитывать **deletion policy** на content (Retain / Delete) и финализаторы root/content; ниже — **предлагаемый порядок для MVP** (уточняется при реализации, но фиксируется, чтобы снизить гонки и «висячие» артефакты).

**Proposed deletion order (MVP)**

1. **N2a и MCR при удалении root (текущая реализация `reconcileDelete`):**
   - После **успешного** capture MCR уже **удалён** контроллером (§4.7); при **Retain** типичный сценарий удаления root — **без** MCR в API.
   - **In-flight MCR** (пока не сработал явный delete после success): при **полном** исчезновении **`Snapshot`** из API garbage collector удаляет MCR по **`ownerReferences` → Snapshot** (§4.3, §4.7 п.3). **`reconcileDelete` по-прежнему не вызывает** явный `Delete(ManifestCaptureRequest)`.
   - **`reconcileDelete` не** удаляет MCP/chunks: снимается финализатор с root при политике на SnapshotContent; артефакты следуют жизненному циклу **SnapshotContent** (см. комментарий в коде).
   - **Generic** отмена через цепочку **MCR→OK→MCP** относится к **не-namespace** пути manifest-линии.
2. Если **deletionPolicy = Delete** (или эквивалент для артефакта): инициировать **удаление объекта в backend**; дождаться подтверждения **или** зафиксировать best-effort + явное условие/событие **Warning** (не оставлять поведение неопределённым в коде без комментария в spec).
3. Довести **SnapshotContent** до согласованного терминального состояния (артефакт удалён или помечен retained согласно политике), снять с content финализаторы, допускающие удаление; согласовать с **ObjectKeeper** по ТЗ.
4. Снять финализатор с **Snapshot**, удалить root.

**Инвариант:** финализатор root **снимается только после** того, как SnapshotContent достиг **согласованного с deletion policy терминального состояния**; не раньше.

**Скелет (Phase 2, API-объект content):** при **DeletionPolicy=Delete** на `SnapshotContent` контроллер вызывает `Delete` на CR и **не** снимает финализатор с root, пока `Get(SnapshotContent)` не вернёт **NotFound** (requeue до исчезновения объекта из API).

При **Retain:** root `ObjectKeeper` with TTL является lifecycle anchor для root Snapshot и root `SnapshotContent`; retained content tree переживает обычное удаление Snapshot до TTL/удаления ObjectKeeper, но не становится ownerless. См. ТЗ в `snapshot-rework`.

### 5.3 Recovery after restart

- Восстановление связи root ↔ content по §4.2.
- Если capture **уже завершён:** MCR **может отсутствовать**; ориентир **`SnapshotContent.status.manifestCheckpointName`** и готовый **MCP** (быстрый путь reconcile без пересоздания MCR — §4.7 п.6).
- Если capture **в процессе:** MCR по имени **§4.7 п.1**, MCP по формуле от UID MCR.
- Идемпотентный ensure без дублирования тяжёлой работы (см. код `reconcileCaptureN2a`).

---

## 6. Conditions (без `status.phase`)

- **Conditions** — **единственный** нормативный источник истины для операторов и автоматизации на `Snapshot` (нет дублирующего `status.phase`). Решение: [`decisions/snapshot-status-surface.md`](decisions/snapshot-status-surface.md).

**Минимальный lifecycle** — выражается **только** через conditions и поля фактов (имя привязанного **SnapshotContent**, `observedGeneration`). Полный перечень публичных полей статуса для **N2a** — §4.4; агрегация parent для **N2b** — §11.1.

```text
(нет Bound / Bound=False) → Bound=True → … capture … → Ready=True
                         └────────────────────────────→ Ready=False (терминальная ошибка)
Удаление root: финализатор + cleanup (§5.2); отдельного «phase Deleting» в API не требуется — достаточно conditions и deletionTimestamp.
```

- **Bound=True** — SnapshotContent создан и согласован с root (поля ссылок по §4.2).
- **Ready=True** — на **N1**: placeholder/fake capture (скелет). На **N2a+**: mirror bound **SnapshotContent Ready**; content Ready означает **persisted** manifest-результат (готовый **ManifestCheckpoint**, ownerRef на SnapshotContent и согласованный статус на SnapshotContent), не промежуточное состояние MCR без MCP. На **N2b** parent snapshot также только зеркалит parent content.
- **Ready=False** с устойчивым reason — терминальная ошибка пользователя/конфигурации (см. §7.1).
- **Progressing** / **CaptureStarted** / **ArtifactStored** — по мере появления реального manifest capture (**N2a**); смысл «идёт capture» задаётся **condition**, не отдельным enum в status.

**Progressing vs CaptureStarted:** до фиксации в CRD/`pkg/snapshot` выбрать одну линию, чтобы не дублировать «идёт работа» двумя типами с True одновременно.

Согласование имён и правил вывода с существующими unified константами — в коде (`pkg/snapshot` и др.) и позже в `system-spec.md`.

---

## 7. Error model

### 7.1 Terminal errors

Примеры: namespace не найден, невалидный class/policy, невалидные include/exclude, unsupported configuration, конфликт иммутабельных полей.

Поведение: **Failed=True** (или эквивалент), нормализованная причина, **без** бесконечного requeue только из-за пользовательской ошибки.

### 7.2 Retriable errors

Примеры: временные ошибки API, backend недоступен, runner ещё не готов, кратковременные ошибки сериализации/записи.

Поведение: backoff / requeue по политике контроллера.

---

## 8. Artifact contract

### 8.1 Payload layout (минимум MVP)

- Логически: метаданные + сериализованный набор манифестов. Для **N2a** физическое хранение может совпадать с manifest-линией: **ManifestCheckpoint + chunks** (gzip/json); выдача клиенту — склейка в архив (см. `ArchiveService`).  
- Для «файлового» контракта (restore/export позже): `metadata.json` — обязателен; `warnings.json` — опционален; **`bundle.tar.gz`** (или аналог) — при необходимости как формат **download**, не обязательно как отдельный объект в etcd на этапе N2a.

### 8.2 metadata.json (минимум полей)

- `formatVersion`
- `snapshotKind` (логический тип снимка, например namespace-state)
- `sourceNamespace`, при необходимости `sourceNamespaceUID`
- `capturedAt`
- `resourceCount`
- `includedResources` / `excludedResources` (или GVR списки)
- `sanitizationProfile`
- `partial`, `warningsCount` (в MVP при fail-closed — см. §9)

### 8.3 Sanitization rules (по умолчанию MVP)

Удалять/не сохранять в bundle (явный перечень):

- `status`
- `managedFields`
- `resourceVersion`
- `uid`
- `generation`
- `creationTimestamp`
- прочие server-side / system поля, не нужные для последующего apply/import (перечень уточнять при реализации, без размытого «и т.п.» в коде и тестах)

**Отдельно решить до restore/import (restore-sensitive):** судьба **`ownerReferences`** (стрип / перезапись / сохранение под профиль), **частей `annotations`** (служебные префиксы, ссылки), при необходимости **`finalizers`** в сохранённых объектах — иначе apply в другой namespace или другой кластер даст неожиданные эффекты. В MVP достаточно явно зафиксировать выбранное поведение в коде и в тестах.

### 8.4 Versioning

- Поле `formatVersion` в metadata обязательно для эволюции формата и restore-friendly контрактов.

### 8.5 Ownership (MVP)

- Жизненный цикл артефакта в backend **управляется через пару** Snapshot → **SnapshotContent** (политика удаления на content / class): root инициирует сценарии, content отражает фактическое состояние хранения.
- **Deletion policy** определяет, удаляется ли артефакт при удалении root или сохраняется (Retain).
- **Backend / repository GC** не считается **основным** механизмом консистентности для MVP: он может существовать как вспомогательный, но оператор должен понимать гарантии из reconcile + policy, а не полагаться на неявный GC.
- **Повторное использование артефакта и шаринг ссылок** между несколькими root в MVP **не поддерживаются** (**unsupported**); не оформлять как «тихий» edge case без отдельного контракта.

### 8.6 Large-namespace capture constraints (MVP)

Явные ограничения для capture (нормативно для **N2a** и далее):

- Capture **не должен** загружать **всё** состояние namespace в память **одним** чтением: потоковая обработка или **chunking** (в т.ч. существующий split по `MaxChunkSizeBytes` в MCP) обязательны для больших объёмов.
- **Пагинация** (`continue`) при list по GVR **обязательна**, если list выполняется в worker/MCR-потоке.
- **Сериализация** при выдаче download (tar и т.д.) — **streaming** или по частям, чтобы не держать целый архив в RAM без лимитов.

### 8.7 Download semantics (N2a / N2b, design lock)

- **N2a — download одного снимка:** отдаёт **только** манифесты **этого** root/content (один MCP / его chunks), **без** дочерних snapshot и без data payloads.
- **N2b — aggregated download:** отдаёт манифесты **parent + subtree** (обход по **`childrenSnapshotContentRefs`** / согласованному graph), **только манифесты**, **без** data payloads.
- **Материализация:** каждый content-node пишет свой **`status.manifestCheckpointName`** на MCP собственного scope. Parent MCP **не** содержит child manifests; дочерние MCP участвуют в E5 exclude и в aggregated read только через content graph.
- **Own scope `Snapshot`:** root MCR/MCP содержит только namespace-scoped allowlist resources. Kubernetes **`Namespace`** object не захватывается. Если после exclude root own scope пустой, создаётся empty MCP (0 objects), а `SnapshotContent.status.manifestCheckpointName` всё равно заполняется.
- **Read-path:** для N2a и N2b по умолчанию **не** хранить отдельный заранее собранный архив в etcd/storage; **read-only агрегация на чтении** из существующих **MCP + chunks** (склейка через `ArchiveService` или эквивалент). Предматериализованный артефакт — только если отдельное ADR/этап.

#### 8.7.1 Практика API и ошибок (N2a, текущий код)

- **Endpoint одного снимка (уже есть):** HTTP **`GET`** … **`/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/manifestcheckpoints/<manifestCheckpointName>/manifests`** — реализация **`ArchiveHandler.HandleGetManifests`** (`internal/api/archive_handler.go`): загрузка MCP, проверка **Ready**, затем **`ArchiveService.GetArchiveFromCheckpoint`** (склейка chunks).
- **MCP не Ready:** ответ **409 Conflict** с телом Kubernetes **Status** (`checkpoint not ready`) — клиент не считает снимок готовым к выгрузке.
- **MCP не найден:** **404**.
- **Нет chunk / checksum mismatch / прочая поломка при склейке:** **500 InternalError** (как сейчас при ошибке `GetArchiveFromCheckpoint`); логирование с деталями. **N2a:** это **не** автоматически снимает **`Ready=True`** на `Snapshot`/`SnapshotContent` при одном неудачном запросе download (операционный сбой чтения ≠ откат capture). Отдельная condition уровня **ArtifactUnreadable** / reconcile, пересобирающий MCP — **после N2a**, если понадобится.
- **N2b aggregated download (PR4):** нормативный контракт — [`spec/snapshot-aggregated-manifests-pr4.md`](../spec/snapshot-aggregated-manifests-pr4.md) (endpoint `…/namespaces/{ns}/snapshots/{name}/manifests`, fail-whole, обход SnapshotContent, `ArchiveService`). Generic usecase умеет стартовать от произвольного registered content-node; HTTP route для curl от любого snapshot/content node — отдельное расширение API. **N2a** single-MCP путь — без изменений (строка выше).

---

## 9. Partial snapshot policy

**MVP по умолчанию: fail-closed.**

Если нельзя **консистентно** собрать целевой набор ресурсов (согласно политике capture), снимок считается **Failed**, а не «тихим» частичным успехом.

Расширение на будущее (не обязательно в первом CRD):

```yaml
spec:
  capturePolicy:
    partialMode: Fail | Allow   # в MVP допустимо только Fail
```

---

## 10. MCR and manifest track

- **Источник правды после успешного N2a:** `Snapshot` + **`SnapshotContent`** + **`manifestCheckpointName`** → **ManifestCheckpoint** (+ chunks); MCR **отсутствует** в API. Публичный контракт статусов — §4.4.
- **`ManifestCaptureRequest`:** **временный** внутренний объект на время capture; **не** в статусе root (§4.4). **`ManifestCheckpoint`** — persisted артефакт, для namespace-пути с **ownerRef на SnapshotContent** (§4.3, §4.6).
- **Разделение ответственности (N2a):**
  - **`Snapshot` controller:** ensure **SnapshotContent**; ensure **root OK** `ret-snap-…` (**`FollowObjectWithTTL`** на root snapshot, **SnapshotContent → ownerRef на OK**, §4.3.2); ensure **MCR** с **ownerRef** на root **`Snapshot`** (§4.7 п.3) и при необходимости **удаление** **MCR** по **§4.7**; observe **MCP**; пишет **status** на **Snapshot**; **Ready** root snapshot зеркалит bound `SnapshotContent Ready`.
  - **`ManifestCheckpointController`:** исполняет **MCR → ManifestCheckpoint** (+ chunks); на **generic** пути — OK **`ret-mcr-*`**; на **namespace-bound** пути — MCP сразу под **SnapshotContent**, без **`ret-mcr-*`** для MCP; **не** пишет публичный статус NS/SnapshotContent.
  - **`SnapshotContentController`:** validator/lifecycle controller, **не** planner/executor; **не** создаёт MCR/VCR/DataExport/VolumeSnapshot requests, **не** читает live Snapshot/MCR/VCR и **не** выбирает capture scope. Он валидирует persisted refs на `SnapshotContent.status` и владеет только readiness conditions; publisher fields (`manifestCheckpointName`, `dataRef`, `childrenSnapshotContentRefs`) пишут snapshot controllers.
- Публично наружу: статус root/content по §4.4; **Ready** не выводить без persisted MCP (см. [`implementation-plan.md`](implementation-plan.md) §2.4.1).

Связь OK (root vs MCR) — §4.3. См. также [`../README.md`](../README.md).

---

## 11. Ready semantics (почти нормативно для MVP)

**`Snapshot` Ready=True означает:**

- Root валиден (runtime validation прошла).
- `SnapshotContent` создан и **корректно привязан** к root.
- Capture **завершён успешно** (в терминах §9 — без провала fail-closed).
- Артефакт манифестов **persisted** (для **N2a+** — готовый **ManifestCheckpoint**/chunks; для **N1** — допускается placeholder).
- Метаданные артефакта записаны в `SnapshotContent.status` (или согласованное поле).
- Дальнейший reconcile **не ожидает** незавершённых операций capture для этого поколения spec.
- Для **N2b** (parent): см. **§11.1**.

### 11.1 N2b — политика агрегации Ready (design lock)

- **`Ready=True` у parent snapshot** только зеркалит **parent `SnapshotContent Ready=True`**.
- **`Ready=True` у parent content** только если: **собственный** persisted manifest-результат (N2a: **ManifestCheckpoint** на parent `SnapshotContent`, как сегодня в коде) **и** все **required** дочерние contents (резолв через **`childrenSnapshotRefs`** → child snapshot → `status.boundSnapshotContentName`) также **`Ready=True`**.
- **Child content в процессе** (нет bound content, нет `Ready`, `Ready=False` с не-терминальной причиной N2a): parent content **`Ready=False`**, reason **`ChildrenPending`** (`pkg/snapshot.ReasonChildrenPending`); message поясняет этап (ожидание bind child content / ожидание child content `Ready=True`; при не-терминальном **`Ready=False`** у child content в message передаются **reason и при наличии message** child для observability).
- **Child content в терминальном сбое** N2a (`Ready=False` с причиной из allowlist терминальных root-capture ошибок): parent content **`Ready=False`**, reason **`ChildrenFailed`** (`pkg/snapshot.ReasonChildrenFailed`); message содержит имя child и reason/message child.
- Успешный parent content после агрегации: **`Ready=True`**, reason **`Completed`** (`pkg/snapshot.ReasonCompleted`), как у N2a leaf после MCP.
- Список **required** children vs optional — зафиксировать в spec/API при полном N2b.

**Имплементация (generic):** parent snapshot controller строит durable content graph из **`status.childrenSnapshotRefs`**: child resolve — один `Get` по **`apiVersion/kind/name`** ref, затем `status.boundSnapshotContentName`; результат публикуется в **`SnapshotContent.status.childrenSnapshotContentRefs`**. `SnapshotContentController` валидирует уже опубликованный content graph; без domain kind-веток. Snapshot controllers не агрегируют child Ready и только зеркалят bound content Ready. Форма ref в API/CRD — только **`apiVersion/kind/name`** (без `namespace`), namespace child неявно равен namespace parent `Snapshot`. Parent/back-reference child-снимка задаётся **ownerReference** на parent snapshot; это lifecycle/requeue helper, не persisted graph. Snapshot controller ждёт `PlanningReady=True` перед публикацией `childrenSnapshotContentRefs` и ensures child content ownerRef → parent content для lifecycle/requeue. `GVKRegistry` на пути children-readiness не обязателен и используется в основном read/exclude-путях E5. Дочерний **`Snapshot`** в графе — лишь один из допустимых snapshot kinds, не отдельный «scaffold»-путь. Исторический временный in-repo scaffold (PR2–PR3 в старых версиях плана) **удалён**; контракт и тесты — **[`spec/system-spec.md`](../spec/system-spec.md) §3.8**, **`snapshot_graph_integration_test.go`**, **PR5b**.

| Child state (resolved child content by snapshot ref) | Parent content `Ready` | Parent content `Ready` reason |
|-------------------------------------------|----------------|------------------------|
| Нет привязанного `SnapshotContent` / child reconcile в процессе | `False` | `ChildrenPending` |
| `Ready` отсутствует или `Unknown` | `False` | `ChildrenPending` |
| `Ready=False` с не-терминальной причиной (например `ManifestCheckpointPending`) | `False` | `ChildrenPending` |
| `Ready=False` с терминальной причиной N2a (whitelist в коде) | `False` | `ChildrenFailed` |
| Child content `Ready=True` и у parent content уже persisted MCP | `True` | `Completed` |

**Ready=True не означает:**

- Что сохранены **данные томов**.
- Что собран «экспортный» архив в смысле полного продукта из vision-документа.
- Что restore / dry-run гарантированно пройдёт в любом кластере без дополнительных проверок.

---

## 12. Access model

**Выбрано:** namespaced root — [`decisions/snapshot-scope.md`](decisions/snapshot-scope.md).

- Проще делегирование прав и UX относительно владельцев namespace; resolved target namespace на текущем этапе = `metadata.namespace` у `Snapshot`.
- `SnapshotContent` остаётся **cluster-scoped**; права на content и OK — отдельно от namespaced root (см. RBAC в шаблонах модуля и будущий N2).

---

## 13. Blocking decisions and open questions

### 13.1 Blocking (MUST до N1)

1. ~~**Cluster-scoped vs namespaced Snapshot**~~ — **снято:** выбрано **namespaced** в [`decisions/snapshot-scope.md`](decisions/snapshot-scope.md).

### 13.2 Open (до финализации API / реализации)

1. Точные **имена condition types** и их соответствие существующим unified CRD (в т.ч. **`ChildrenFailed`** для N2b — §11.1).
2. **Deletion policy** на уровне class vs spec (что наследуется от SnapshotClass аналога).
3. ~~Минимальный набор **GVR** для N2a~~ — зафиксирован в **§4.5**; остаётся перенос в CRD/code и при необходимости сужение/расширение через PR с обновлением §4.5.
4. **Политика удаления root при незавершённом capture** — **§5.2 п.1:** in-flight MCR убирается **GC** по ownerRef на **`Snapshot`** после полного удаления root; явный `Delete(MCR)` из `reconcileDelete` **не** делается.
5. **Required vs optional** дочерние snapshot для агрегации **N2b** — §11.1.

---

## 14. Bootstrap / registry / RBAC impact

- Зарегистрировать пару **`Snapshot` / `SnapshotContent`** в `pkg/unifiedbootstrap` (и при необходимости CSD): resolve, watches, RBAC в шаблонах. **Убрать** использование **generic `SnapshotContent`** как носителя для namespace root (**без** миграции — целевая схема сразу SnapshotContent).
- Не смешивать в одном PR без необходимости с треками M1/M2 (manifest) и крупными изменениями R3 — см. [`implementation-plan.md`](implementation-plan.md).

---

## 15. Testing strategy

- **N1:** уже в коде — lifecycle, delete, mismatch, recovery (fake Ready).
- **N2a:** integration — временный MCR, MCP, root OK, persisted Ready (**после success MCR отсутствует**), download одного снимка, Retain; см. [`testing/e2e-testing-strategy.md`](../testing/e2e-testing-strategy.md).
- **N2b:** integration — дерево, refs, parent Ready, aggregated download (manifests-only).
- **N3:** envtest — recovery после рестарта контроллера поверх закрытого N2a (и при необходимости N2b).
- **N4+:** лимиты большого namespace, таймауты; затем кейсы **N5** (data и полный ТЗ).
- Идемпотентность ensure **SnapshotContent**, OK, MCR/MCP и stable naming — отдельные кейсы.

---

## 16. Поставка (milestones N0–N5, не `status.phase`)

Имена **N0–N5** — этапы [`implementation-plan.md`](implementation-plan.md) §2.4; **не** поля API. Статус объектов — только **conditions** (§6). Детальный бэклог **N5** — только по ТЗ в `snapshot-rework/` (data-layer и полный сценарий вне manifests-only дерева). **Декомпозиция N2** (**N2a** / **N2b**, DoD, out-of-scope) — **[`implementation-plan.md`](implementation-plan.md) §2.4.1** (SSOT; этот §16 — краткий указатель).

### N0 — Contract / gate

1. **Chosen option** в [`decisions/snapshot-scope.md`](decisions/snapshot-scope.md) (≠ TBD) — §13.1.
2. Сверка apiVersion/group NS и SnapshotContent: ТЗ `snapshot-rework` ↔ CRD в репозитории.
3. Пара **`Snapshot` / `SnapshotContent`** и **conditions-only** — decisions; root/content binding §4.2; ObjectKeeper — §4.3 + ТЗ.

### N1 — CRD / API / skeleton lifecycle

✅ Закрыт: подготовительный слой без ObjectKeeper, без real manifest capture, без дочернего дерева — см. [`implementation-plan.md`](implementation-plan.md) §2.4 и §2.4.1 (граница N1). Код **не** считается «временным» — это база для N2.

### N2 — Manifests-only: N2a затем N2b

SSOT: **§2.4.1** и **§2.4.2** (декомпозиция N2b по PR) в [`implementation-plan.md`](implementation-plan.md).

- **N2a:** OK + **MCR→ManifestCheckpoint** + запись результата в **`SnapshotContent.status`** + **Ready** только по persisted MCP + download **одного** снимка; data — placeholders.
- **N2b:** дочерние snapshot/content, **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`**, агрегированный **Ready** parent content + mirror на parent snapshot, **aggregated manifests download** subtree; всё ещё без data-flow. Вход в N2b — с **PR1** (только поля графа + docs/spec), см. **§2.4.2**.

§8 — логический контракт манифестов; физическое хранение N2a согласовать с MCP/chunks и download path в коде.

### N3 — Envtest / hardening

Расширение сценариев §15 (в т.ч. recovery после рестарта контроллера); негативные кейсы сверх уже закрытых в N1 и N2a.

### N4 — После N2 (N2a+N2b)

Углублённые лимиты большого namespace (§8.6), таймауты — см. [`implementation-plan.md`](implementation-plan.md) §2.4 и M2.

### N5 — За пределами manifests-only дерева

Data-layer (volume/VSC/VCR), полный export/import, restore с данными, CSD traversal в полном объёме — по [`snapshot-rework/2026-01-25-snapshot.md`](../../../snapshot-rework/2026-01-25-snapshot.md). **Дерево манифест-only** — в **N2b**, не в N5.

---

## 17. Layering (напоминание)

| Слой | Ответственность |
|------|-----------------|
| A. API / object lifecycle (Snapshot controller) | Финализатор, content ensure, **conditions** (без `status.phase`), вызов domain capture, удаление |
| B. Domain capture | План захвата, сериализация, запись в backend, возврат metadata/result |
| C. Shared runtime / unified | Bind/sync для пары **Snapshot / SnapshotContent** (и при необходимости общие хелперы conditions с другими snapshot-типами) |
