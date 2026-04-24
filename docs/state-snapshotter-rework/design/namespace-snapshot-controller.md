# NamespaceSnapshot controller — MVP design

## 1. Status of this document

**Тип:** design — план поставки и инженерные инварианты под реализацию. **Продуктовый сценарий и примеры YAML** — SSOT в [`snapshot-rework/`](../../../snapshot-rework/) (в т.ч. [`2026-01-25-namespace-snapshot.md`](../../../snapshot-rework/2026-01-25-namespace-snapshot.md)); эти файлы **не правятся** в рамках согласования через `docs/`. После стабилизации — выдержки в [`spec/system-spec.md`](../spec/system-spec.md) и тесты.

**Связанные документы:**

- Пара root/content + отказ от generic `SnapshotContent` для namespace root: [`decisions/namespace-snapshot-content-decision.md`](decisions/namespace-snapshot-content-decision.md)
- Поверхность статуса (без `status.phase`): [`decisions/namespace-snapshot-status-surface.md`](decisions/namespace-snapshot-status-surface.md)
- **Scope (resolved):** namespaced root — [`decisions/namespace-snapshot-scope.md`](decisions/namespace-snapshot-scope.md)

---

## 2. Scope and non-goals

### 2.1 Целевая семантика (согласовано с ТЗ)

**NamespaceSnapshot** — namespaced root / intention; результат фиксируется в cluster-scoped **`NamespaceSnapshotContent`** (пара 1:1 с root по смыслу ТЗ). Generic **`SnapshotContent`** для этого потока **не** используется как root-carrier. Связка с **ObjectKeeper** — по правилам из `snapshot-rework`. **N2a в коде сейчас:** cluster-scoped root OK **`ret-nssnap-<namespace>-<snapshotName>`** в режиме **`FollowObjectWithTTL`** на **root `NamespaceSnapshot`** (UID в `followObjectRef`); **root `NamespaceSnapshotContent.metadata.ownerReferences` → этот ObjectKeeper** (**controller: true**) — якорь TTL: после удаления root snapshot OK остаётся до истечения TTL; Deckhouse ObjectKeeper controller удаляет OK → GC снимает retained NSC и каскадно MCP/детей. TTL задаётся конфигом контроллера (`SnapshotRootOKTTL`). **Временный** `ManifestCaptureRequest` (MCR) для capture после успешного завершения **удаляется** NS-контроллером (см. §4.7).

**Поставка поэтапно** (см. §16 и [`implementation-plan.md`](implementation-plan.md)): **N1** — skeleton bind/delete без OK и без real capture; **N2a** — manifests-only один root + OK + внутренний **MCR→ManifestCheckpoint** + download; **N2b** — **дерево** manifests-only (дети, refs, aggregated Ready/download); data-layer, полный export/import/restore — после N2, **не** переписывая SSOT в `snapshot-rework`.

- **Capture** тяжёлой работы: для manifests в **N2a** — через **внутренний** путь **ManifestCaptureRequest → ManifestCheckpoint** (существующий контроллер/chunks); root-контроллер **не** обязан держать весь list/capture в одном reconcile, но **не** обязателен и отдельный Job, если исполнение согласовано с MCR-потоком.
- **Unified shared runtime** там, где применим общий паттерн Snapshot/SnapshotContent для **других** GVK; для пары namespace root используем **свой** content kind (`NamespaceSnapshotContent`), см. [`namespace-snapshot-content-decision.md`](decisions/namespace-snapshot-content-decision.md).

### 2.2 Out of scope на ранних milestones (N1–N2)

**N2** по [`implementation-plan.md`](implementation-plan.md) §2.4.1 — **только manifests-only**: **N2a** (один root) + **N2b** (дерево). Явно **вне N2:** volume/data snapshots, реальный VCR/VSC, restore с данными, полный export/import продукта, storage class remap, VM data restore, выдача поддерева с **data payloads**.

До полного сценария ТЗ в одном релизе (SSOT в `snapshot-rework`):

- **Child composition (manifests-only)** — целевая ось **N2b**, не «опциональный потом».
- DSC priority traversal **в полном объёме**, data-layer — после закрытого N2 / в **N5** по плану.
- Экспорт/импорт архива и CLI как в полном ТЗ — не обещать в рамках N2.
- **`ManifestCaptureRequest` как обязательный публичный контракт** для оператора — см. §10; публично: root + **NamespaceSnapshotContent** + artifact metadata; MCR/MCP — **внутренний** execution path в **N2a**.

---

## 3. Relationship to `snapshot-rework/` (SSOT)

Сценарий, иерархии, ObjectKeeper, примеры манифестов — **только** по [`snapshot-rework/`](../../../snapshot-rework/). Настоящий файл задаёт **порядок внедрения**, инварианты binding/deletion/Ready и ссылки на decisions; при противоречии сначала **уточняют ТЗ** в `snapshot-rework`, затем обновляют этот design и код.

---

## 4. API model

### 4.1 NamespaceSnapshot (черновик полей)

**Scope:** namespaced root — зафиксировано в [`decisions/namespace-snapshot-scope.md`](decisions/namespace-snapshot-scope.md). На текущем этапе целевой namespace совпадает с `metadata.namespace` объекта `NamespaceSnapshot`.

**Spec (логически):**

- Источник namespace для capture: тот же, что `metadata.namespace` root (расширение через отдельное поле — позже, если понадобится продуктово).
- Класс/политика: `snapshotClassName` / `className` (как в продуктовой модели unified snapshots).
- Опционально: include/exclude групп ресурсов (MVP — минимальный набор или фиксированный профиль).
- Опционально позже: `capturePolicy` (см. §9); в MVP допустимо заложить поле, но **выставить только fail-closed**.

**Default exclusions / GVR до реального capture (N1→N2):** в CRD на этапе N1 **нет** полей `includedResources` / `excludedResources` у root — пользовательский allow/deny **не задаётся**. Пока capture остаётся placeholder/fake, **исключения не применяются**. С включением **реального** capture (N2+) **по умолчанию fail-closed**: без явно разрешённого набора GVR (через class/profile или зафиксированный built-in минимум в коде — конкретизация в N2) **запускать** list/capture **нельзя**; произвольный «снимать всё» без политики не допускается.

**Status (логически):**

- **`conditions` — единственный** нормативный источник жизненного цикла и ошибок для операторов; поля **`status.phase` нет** (см. [`decisions/namespace-snapshot-status-surface.md`](decisions/namespace-snapshot-status-surface.md)).
- `conditions`: как минимум согласованный набор с unified-паттерном (например Ready, Bound, Progressing; при необходимости CaptureStarted, ArtifactStored; терминальные сбои — через Ready=False и reason).
- Имя привязанного content-объекта snapshot-линии — в CRD: **`status.boundSnapshotContentName`** (cluster-scoped имя; для этой линии фактический kind — **`NamespaceSnapshotContent`**, не выводится из имени поля).
- `observedGeneration`.
- `startedAt`, `completedAt` (опционально).
- Сводные поля прогресса/ошибки по необходимости (без дублирования детального состояния Job).

### 4.2 NamespaceSnapshotContent binding (контракт)

**Root → Content**

- В status root — **`status.boundSnapshotContentName`**: cluster-scoped имя привязанного **`NamespaceSnapshotContent`** (единое имя поля для всех snapshot root’ов в модуле).

**Content → Root**

- В `NamespaceSnapshotContent.spec`: ссылка **`namespaceSnapshotRef`**: `apiVersion`, `kind`, `name`, **`uid`**, `namespace` (root namespaced).

**Инварианты**

1. Пока NamespaceSnapshot **не** в согласованном терминальном состоянии по conditions для данного поколения spec, он связан **не более чем с одним** NamespaceSnapshotContent.
2. NamespaceSnapshotContent **не перепривязывается** к другому root.
3. После рестарта контроллер восстанавливает связь по status root, ref на content в spec/status content, или **детерминированному имени** content.

**Идемпотентность**

- Операции ensure: финализатор, создание/поиск content, ObjectKeeper, запуск capture, запись статуса — безопасны при повторной доставке события и после рестарта.

**Нейминг**

- Рекомендация: стабильное имя content от **UID** root (см. ТЗ и существующие паттерны в коде).

**Уникальность и коллизии**

- Новый UID root → новый content; совпадение `metadata.name` у пользователя **не** означает reuse content.

Точная схема полей — в CRD и в `system-spec.md` после выравнивания с ТЗ.

### 4.3 ObjectKeeper, ownerReference и границы scope

Полная схема полей OK — в [`snapshot-rework/2026-01-25-namespace-snapshot.md`](../../../snapshot-rework/2026-01-25-namespace-snapshot.md) и ADR там же. Ниже — **design lock** для N2a/N2b, согласованный с текущим кодом manifest-линии:

- **Общий (generic) путь MCR** в `ManifestCheckpointController`: при отсутствии привязки к NSC создаётся OK **`ret-mcr-<namespace>-<mcrName>`** в **FollowObject** на MCR; **ManifestCheckpoint** получает `ownerReference` → этот OK; **chunks** → MCP.
- **Namespace N2a-путь:** на MCR ставится аннотация **`state-snapshotter.deckhouse.io/bound-namespace-snapshot-content`** (имя root NSC); тогда **ManifestCheckpoint** создаётся с **`ownerReference` → `NamespaceSnapshotContent`** (**controller: true**), **без** OK `ret-mcr-*` для MCP. MCR после успешного capture удаляется (§4.7) — MCP **не** должен зависеть от дальнейшего существования MCR.

#### 4.3.1 Правило: ownerReference vs ObjectKeeper

- **`ownerReference`** используем там, где объекты в **совместимом scope** для Kubernetes GC (например оба cluster-scoped): **ManifestCheckpointContentChunk → ManifestCheckpoint** — chunks удаляются через ownerRef на MCP (как сейчас в коде).
- **ObjectKeeper** используем там, где нужен **cluster-scoped retention anchor** (TTL на follow root snapshot): логический bind **root ↔ NamespaceSnapshotContent** остаётся **spec.namespaceSnapshotRef** / **status.boundSnapshotContentName**; **удержание retained NSC** — через **`NamespaceSnapshotContent.ownerReferences` → root OK** (не наоборот).
- **ObjectKeeper нигде не подменяет** bind-контракт **`spec.namespaceSnapshotRef`** / **`status.boundSnapshotContentName`**.

#### 4.3.2 Два применения ObjectKeeper (не смешивать)

1. **Root / snapshot (N2a, `namespacesnapshot_capture.go`):** cluster-scoped OK **`ret-nssnap-…`**: **`FollowObjectWithTTL`** на **root `NamespaceSnapshot`**; **без** `metadata.ownerReferences` на NSC; **root `NamespaceSnapshotContent`** имеет **`ownerReferences` → этот OK** (**controller**). TTL из конфига. Это **не** follow на MCR и **не** generic `ret-mcr-*`. После удаления root снимка при **Retain** NSC (и MCP) остаются, пока жив OK; после TTL — удаление OK → GC NSC и каскад вниз — зона Deckhouse ObjectKeeper controller и политики модуля.
2. **Manifest capture (generic MCR-путь):** OK **`ret-mcr-*`** в **FollowObject** на **ManifestCaptureRequest**; MCP через ownerRef на этот OK. Для **namespace N2a** этот путь **не** используется для финального MCP (см. вводный абзац §4.3).

**Инвариант:** **ManifestCaptureRequest** в N2a имеет **`metadata.ownerReferences` → `NamespaceSnapshot`** (**controller: true**, тот же namespace), чтобы при **полном удалении** root из API garbage collector убрал «зависший» in-flight MCR без отдельного `Delete` из `reconcileDelete`. MCR по-прежнему создаётся **NamespaceSnapshot controller** на время capture; **ManifestCheckpointController** исполняет **MCR → MCP** (+ chunks). Статусы **NS/NSC** пишет **NamespaceSnapshot controller** по наблюдению MCP; после успешного завершения MCR **удаляется** явно контроллером (§4.7) — совместимо с ownerRef; публичная «истина» — **NSC + `manifestCheckpointName` + MCP**.

**Удержание MCP/chunks (N2a namespace path):** после успеха — **ownerRef MCP → NSC** и политика **Retain/Delete** на NSC; **не** цепочка MCR→OK→MCP. Generic путь с `ret-mcr-*` остаётся для других вызывающих MCR, не для финального namespace-bound MCP.

**N2a.x:** сверка с кодом зафиксирована в **§4.6**; при изменении `ManifestCheckpointController` обновлять §4.3 и §4.6.

---

### 4.4 N2a — публичный status surface (design lock)

Цель: оператор и автоматизация не зависят от **ManifestCaptureRequest** в API root.

**`NamespaceSnapshot.status` (N2a, публично):**

- **`boundSnapshotContentName`** (уже есть) + **`conditions`** (+ при необходимости **`observedGeneration`**, временные метки — по мере появления в CRD).
- **Без** `manifestCaptureRequestName` и любых полей MCR на root: MCR — **implementation detail**, создаётся в namespace capture, но **не** часть публичного контракта root.

**`NamespaceSnapshotContent.status` (N2a, публично):**

- **`manifestCheckpointName`** (cluster-scoped имя готового **ManifestCheckpoint**) — каноническая ссылка на persisted manifest-результат.
- **`conditions`** (в т.ч. готовность к download, ошибки capture).
- Опционально для UX/наблюдаемости: **`capturedAt`**, **`resourceCount`** (или эквивалент из metadata MCP), **`artifactFormatVersion`** / `formatVersion` — если нужны до расширения spec; имена полей — финализировать в CRD и `system-spec.md`.

**Источник `Ready=True` на root (N2a):** только после того, как на **NSC** зафиксированы persisted результат (MCP + согласованные chunks) и согласованные **conditions**, а не по факту «создали MCR».

**Минимальный первый PR по CRD (N2a, не раздувать):** в **`NamespaceSnapshotContent.status`** добавить **`manifestCheckpointName`** (string, omitempty) рядом с уже существующими **`conditions`**. Поля **`capturedAt`**, **`resourceCount`**, **`observedGeneration`** на NSC — **отложить** до отдельного PR/ADR, если достаточно conditions + `manifestCheckpointName`. На **`NamespaceSnapshot.status`** без изменений объёма: уже есть **`observedGeneration`**, **`boundSnapshotContentName`**, **`conditions`** — публично **без** MCR (§4.4).

---

### 4.5 N2a — built-in allowlist и исключения (первый набор, SSOT до кода)

**Включить в первый built-in profile (namespaced, list по GVR):**

| API group | Версия | Resource (plural) | Примечание |
|-----------|--------|---------------------|------------|
| `apps` | `v1` | `deployments`, `statefulsets`, `daemonsets` | |
| `batch` | `v1` | `jobs`, `cronjobs` | |
| | `v1` | `pods`, `services`, `configmaps`, `secrets`, `serviceaccounts`, `persistentvolumeclaims` | PVC — **только манифест** (metadata/spec), без данных тома |
| `networking.k8s.io` | `v1` | `ingresses` | |
| `networking.k8s.io` | `v1` | `networkpolicies` | Опционально: включать только явным решением в PR; иначе **вне** первого merge |
| `rbac.authorization.k8s.io` | `v1` | `roles`, `rolebindings` | |

**Явно исключить (не list / не target):**

- `events`, `leases`, `endpointslices` (core / coordination / discovery — по фактическим GVR в кластере).
- `replicasets`, `controllerrevisions` — derived/controller-owned; не дублировать workload.
- **PodDisruptionBudget** — в первом наборе **не включать**; включение — отдельное решение.
- Все **внутренние** объекты snapshotter: `NamespaceSnapshot`, `NamespaceSnapshotContent`, `ManifestCaptureRequest`, `ManifestCheckpoint`, `ManifestCheckpointContentChunk`, `ObjectKeeper` (и пр. CR модуля по списку), служебные объекты runner/MCR (по **labels/prefixes** — зафиксировать в коде рядом с allowlist).
- Любые GVR не из таблицы выше — **не** захватывать в N2a (fail-closed расширение только через изменение SSOT-списка).

Профиль должен быть **один** в коде (или генерироваться из одного источника); ad-hoc «снять всё» запрещён (см. [`implementation-plan.md`](implementation-plan.md) §2.4.1).

---

### 4.6 N2a.x — сверка manifest-линии с §4.3 (выполнено по коду репозитория)

Проверено против `images/state-snapshotter-controller/internal/controllers/manifestcheckpoint_controller.go` и `namespacesnapshot_capture.go`:

| Утверждение §4.3 | В коде |
|------------------|--------|
| Generic: ObjectKeeper для MCR в **`FollowObject`** (следует за UID MCR) | Да, если **нет** аннотации bound NSC на MCR (`ret-mcr-*`, см. ~L297–L363). |
| Generic: имя OK **`ret-mcr-<namespace>-<mcrName>`** | Да. |
| Generic: **ManifestCheckpoint** **ownerReference → ObjectKeeper** `ret-mcr-…` (controller) | Да на generic-пути. |
| **Namespace N2a:** при **`AnnotationBoundNamespaceSnapshotContent`** — **ManifestCheckpoint** **ownerReference → `NamespaceSnapshotContent`** (controller), **без** `ret-mcr-*` | Да (~L272–L296). |
| **Chunks** **ownerReference → ManifestCheckpoint** | Да (поток create chunks). |
| Root OK **`ret-nssnap-…`**: **`FollowObjectWithTTL`** на **`NamespaceSnapshot`**; **NSC `ownerReferences` → этот OK** | Да, `namespacesnapshot_capture.go` (`ensureNamespaceSnapshotRootObjectKeeper`). |

Итог: **два manifest-пути** (generic vs namespace-bound) и root OK — как в §4.3. При смене логики в коде — обновлять §4.3 и эту таблицу.

---

### 4.7 N2a — корреляция NamespaceSnapshot ↔ MCR ↔ MCP и жизненный цикл MCR

**Цель:** во время capture — детерминированно находить MCR/MCP без list по кластеру; **после успешного завершения** — не опираться на существование MCR: публичная и операционная опора на **NSC + MCP** (и root OK по §4.3.2).

**Во время capture (временный MCR):**

1. **Имя `ManifestCaptureRequest`:** детерминированно от **`NamespaceSnapshot.metadata.uid`**: **`nss-{metadata.uid}`** (UID с дефисами — допустимое имя). Namespace MCR = **`metadata.namespace` того же `NamespaceSnapshot`**. **`Get`** по этому ключу — основной путь ensure.
2. **Имя `ManifestCheckpoint`:** от **UID экземпляра MCR** (после `Create`), та же формула, что в `ManifestCheckpointController` / `pkg/namespacemanifest` — префикс **`mcp-`** + **16 hex** от SHA256(UID MCR) (первые 8 байт). Общая функция в коде — без дублирования строки алгоритма.
3. **Labels (дополнительно):** **`state-snapshotter.deckhouse.io/namespace-snapshot-uid=<root-uid>`**; аннотация **`state-snapshotter.deckhouse.io/bound-namespace-snapshot-content=<nsc-name>`** на MCR — для namespace-bound пути в manifest-контроллере. **`metadata.ownerReferences` → `NamespaceSnapshot`** (**controller: true**) — очистка in-flight MCR при полном удалении root из API (GC); у существующего MCR без ref — **patch** (merge) при reconcile; иной **`NamespaceSnapshot`** в ownerReferences того же MCR — ошибка.
4. **Stale / пересоздание root с тем же `metadata.name`:** если MCR **`nss-{uid}`** существует, но label **namespace-snapshot-uid** не совпадает с текущим root UID — MCR **чужой**; ошибка (см. тест recreate). **Root OK** `ret-nssnap-…`: при новом поколении root спецификация OK обновляется под актуальный `NamespaceSnapshot` (см. `ensureNamespaceSnapshotRootObjectKeeper`).

**После успешного capture (стабильное состояние N2a):**

5. **Порядок в коде:** MCP готов → на **NSC** записан **`status.manifestCheckpointName`**, условия NSC/NS согласованы, **`Ready=True`** на root (в т.ч. после **E6** при непустых **`childrenSnapshotRefs`**, см. §11.1 и **[`spec/system-spec.md`](../spec/system-spec.md) §3.8**) → **`Delete(ManifestCaptureRequest)`** по имени из п.1 (**NotFound** считается успехом). **MCP и chunks не удаляются** этим шагом. **`spec.manifestCaptureRequestRef`** на MCP может сохранять имя/uid **удалённого** MCR как исторический ref — на жизнь MCP это не влияет при ownerRef на NSC.
6. **Идемпотентность / гонки:** повторный reconcile не должен **пересоздавать** MCR, если на NSC уже зафиксирован готовый MCP (см. логику в `namespacesnapshot_capture.go`: быстрый путь только при **отсутствии** MCR в API; при `NotFound` MCR и уже сохранённом MCP — не создавать новый request).

**Поведение до завершения capture:**

7. **Immutability `spec.targets`:** расхождение снимаемого плана с live namespace → **`CapturePlanDrift`** (пока MCR ещё существует; см. integration-тест: drift проверяется **до** удаления MCR). Стабильный порядок targets — отсортированный набор (`BuildManifestCaptureTargets`).

**Публичный статус root** — без полей MCR (§4.4). **Каноническая ссылка на артефакт** после success — **`NamespaceSnapshotContent.status.manifestCheckpointName`**.

---

## 5. Reconcile model

### 5.1 Normal flow (логические шаги)

**Watch:** контроллер подписан на **`NamespaceSnapshot`** и на связанный **`NamespaceSnapshotContent`** (enqueue по `spec.namespaceSnapshotRef`), чтобы изменения content (в т.ч. ручной repair) снова прогоняли reconcile root без отдельного события на root.

1. Fetch; при удалении — §5.2.
2. Ensure finalizer на root.
3. Validation (namespace существует, class/policy валидна, spec консистентен) — §7.
4. Ensure **NamespaceSnapshotContent** + запись bind в status root и ссылки root↔content — §4.2; ensure **ObjectKeeper** по ТЗ — §4.3 (**N2a+**).
5. **Reconcile capture** (N2a): ensure временного **MCR** → observe **MCP** через manifest-линию; при альтернативной реализации — Job/runner по [`implementation-plan.md`](implementation-plan.md) §2.4.1.
6. По успеху: записать **`manifestCheckpointName`** и условия на **NSC**, **`Ready`** на root (включая агрегацию по **`childrenSnapshotRefs`**, §11.1), затем **удалить MCR** (§4.7).
7. Не выполнять тяжёлую работу list/watch namespace в горячем пути, если она перенесена в MCR/controller capture path (политика ресурсов apiserver).

### 5.2 Deletion flow

Учитывать **deletion policy** на content (Retain / Delete) и финализаторы root/content; ниже — **предлагаемый порядок для MVP** (уточняется при реализации, но фиксируется, чтобы снизить гонки и «висячие» артефакты).

**Proposed deletion order (MVP)**

1. **N2a и MCR при удалении root (текущая реализация `reconcileDelete`):**
   - После **успешного** capture MCR уже **удалён** контроллером (§4.7); при **Retain** типичный сценарий удаления root — **без** MCR в API.
   - **In-flight MCR** (пока не сработал явный delete после success): при **полном** исчезновении **`NamespaceSnapshot`** из API garbage collector удаляет MCR по **`ownerReferences` → NamespaceSnapshot** (§4.3, §4.7 п.3). **`reconcileDelete` по-прежнему не вызывает** явный `Delete(ManifestCaptureRequest)`.
   - **`reconcileDelete` не** удаляет MCP/chunks: снимается финализатор с root при политике на NSC; артефакты следуют жизненному циклу **NamespaceSnapshotContent** (см. комментарий в коде).
   - **Generic** отмена через цепочку **MCR→OK→MCP** относится к **не-namespace** пути manifest-линии.
2. Если **deletionPolicy = Delete** (или эквивалент для артефакта): инициировать **удаление объекта в backend**; дождаться подтверждения **или** зафиксировать best-effort + явное условие/событие **Warning** (не оставлять поведение неопределённым в коде без комментария в spec).
3. Довести **NamespaceSnapshotContent** до согласованного терминального состояния (артефакт удалён или помечен retained согласно политике), снять с content финализаторы, допускающие удаление; согласовать с **ObjectKeeper** по ТЗ.
4. Снять финализатор с **NamespaceSnapshot**, удалить root.

**Инвариант:** финализатор root **снимается только после** того, как NamespaceSnapshotContent достиг **согласованного с deletion policy терминального состояния**; не раньше.

**Скелет (Phase 2, API-объект content):** при **DeletionPolicy=Delete** на `NamespaceSnapshotContent` контроллер вызывает `Delete` на CR и **не** снимает финализатор с root, пока `Get(NamespaceSnapshotContent)` не вернёт **NotFound** (requeue до исчезновения объекта из API).

При **Retain:** артефакт и при необходимости **NamespaceSnapshotContent** переживают root — явный итог reconcile (**conditions** на content и ссылки orphaning). См. ТЗ в `snapshot-rework`.

### 5.3 Recovery after restart

- Восстановление связи root ↔ content по §4.2.
- Если capture **уже завершён:** MCR **может отсутствовать**; ориентир **`NamespaceSnapshotContent.status.manifestCheckpointName`** и готовый **MCP** (быстрый путь reconcile без пересоздания MCR — §4.7 п.6).
- Если capture **в процессе:** MCR по имени **§4.7 п.1**, MCP по формуле от UID MCR.
- Идемпотентный ensure без дублирования тяжёлой работы (см. код `reconcileCaptureN2a`).

---

## 6. Conditions (без `status.phase`)

- **Conditions** — **единственный** нормативный источник истины для операторов и автоматизации на `NamespaceSnapshot` (нет дублирующего `status.phase`). Решение: [`decisions/namespace-snapshot-status-surface.md`](decisions/namespace-snapshot-status-surface.md).

**Минимальный lifecycle** — выражается **только** через conditions и поля фактов (имя привязанного **NamespaceSnapshotContent**, `observedGeneration`). Полный перечень публичных полей статуса для **N2a** — §4.4; агрегация parent для **N2b** — §11.1.

```text
(нет Bound / Bound=False) → Bound=True → … capture … → Ready=True
                         └────────────────────────────→ Ready=False (терминальная ошибка)
Удаление root: финализатор + cleanup (§5.2); отдельного «phase Deleting» в API не требуется — достаточно conditions и deletionTimestamp.
```

- **Bound=True** — NamespaceSnapshotContent создан и согласован с root (поля ссылок по §4.2).
- **Ready=True** — на **N1**: placeholder/fake capture (скелет). На **N2a+**: **persisted** manifest-результат (в т.ч. готовый **ManifestCheckpoint** и согласованный статус на NSC), не промежуточное состояние MCR без MCP. На **N2b** для parent — см. §11 (агрегация детей).
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

- Жизненный цикл артефакта в backend **управляется через пару** NamespaceSnapshot → **NamespaceSnapshotContent** (политика удаления на content / class): root инициирует сценарии, content отражает фактическое состояние хранения.
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
- **Материализация:** для N2a и N2b по умолчанию **не** хранить отдельный заранее собранный архив в etcd/storage; **read-only агрегация на чтении** из существующих **MCP + chunks** (склейка через `ArchiveService` или эквивалент). Предматериализованный артефакт — только если отдельное ADR/этап.

#### 8.7.1 Практика API и ошибок (N2a, текущий код)

- **Endpoint одного снимка (уже есть):** HTTP **`GET`** … **`/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/manifestcheckpoints/<manifestCheckpointName>/manifests`** — реализация **`ArchiveHandler.HandleGetManifests`** (`internal/api/archive_handler.go`): загрузка MCP, проверка **Ready**, затем **`ArchiveService.GetArchiveFromCheckpoint`** (склейка chunks).
- **MCP не Ready:** ответ **409 Conflict** с телом Kubernetes **Status** (`checkpoint not ready`) — клиент не считает снимок готовым к выгрузке.
- **MCP не найден:** **404**.
- **Нет chunk / checksum mismatch / прочая поломка при склейке:** **500 InternalError** (как сейчас при ошибке `GetArchiveFromCheckpoint`); логирование с деталями. **N2a:** это **не** автоматически снимает **`Ready=True`** на `NamespaceSnapshot`/`NamespaceSnapshotContent` при одном неудачном запросе download (операционный сбой чтения ≠ откат capture). Отдельная condition уровня **ArtifactUnreadable** / reconcile, пересобирающий MCP — **после N2a**, если понадобится.
- **N2b aggregated download (PR4):** нормативный контракт — [`spec/namespace-snapshot-aggregated-manifests-pr4.md`](../spec/namespace-snapshot-aggregated-manifests-pr4.md) (endpoint `…/namespaces/{ns}/namespacesnapshots/{name}/manifests`, fail-whole, обход NSC, `ArchiveService`). **N2a** single-MCP путь — без изменений (строка выше).

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

- **Источник правды после успешного N2a:** `NamespaceSnapshot` + **`NamespaceSnapshotContent`** + **`manifestCheckpointName`** → **ManifestCheckpoint** (+ chunks); MCR **отсутствует** в API. Публичный контракт статусов — §4.4.
- **`ManifestCaptureRequest`:** **временный** внутренний объект на время capture; **не** в статусе root (§4.4). **`ManifestCheckpoint`** — persisted артефакт, для namespace-пути с **ownerRef на NSC** (§4.3, §4.6).
- **Разделение ответственности (N2a):**
  - **`NamespaceSnapshot` controller:** ensure **NamespaceSnapshotContent**; ensure **root OK** `ret-nssnap-…` (**`FollowObjectWithTTL`** на root snapshot, **NSC → ownerRef на OK**, §4.3.2); ensure **MCR** с **ownerRef** на root **`NamespaceSnapshot`** (§4.7 п.3) и при необходимости **удаление** **MCR** по **§4.7**; observe **MCP**; пишет **status** на **NamespaceSnapshot** и **NamespaceSnapshotContent**; **Ready** по persisted MCP.
  - **`ManifestCheckpointController`:** исполняет **MCR → ManifestCheckpoint** (+ chunks); на **generic** пути — OK **`ret-mcr-*`**; на **namespace-bound** пути — MCP сразу под **NSC**, без **`ret-mcr-*`** для MCP; **не** пишет публичный статус NS/NSC.
- Публично наружу: статус root/content по §4.4; **Ready** не выводить без persisted MCP (см. [`implementation-plan.md`](implementation-plan.md) §2.4.1).

Связь OK (root vs MCR) — §4.3. См. также [`../README.md`](../README.md).

---

## 11. Ready semantics (почти нормативно для MVP)

**`NamespaceSnapshot` Ready=True означает:**

- Root валиден (runtime validation прошла).
- `NamespaceSnapshotContent` создан и **корректно привязан** к root.
- Capture **завершён успешно** (в терминах §9 — без провала fail-closed).
- Артефакт манифестов **persisted** (для **N2a+** — готовый **ManifestCheckpoint**/chunks; для **N1** — допускается placeholder).
- Метаданные артефакта записаны в `NamespaceSnapshotContent.status` (или согласованное поле).
- Дальнейший reconcile **не ожидает** незавершённых операций capture для этого поколения spec.
- Для **N2b** (parent): см. **§11.1**.

### 11.1 N2b — политика агрегации Ready (design lock)

- **`Ready=True` у parent** только если: **собственный** persisted manifest-результат (N2a: **ManifestCheckpoint** на parent `NamespaceSnapshotContent`, как сегодня в коде) **и** все **required** дочерние snapshot (по graph из **`childrenSnapshotRefs`** / доменной логике) также **`Ready=True`**.
- **Child в процессе** (не bound, нет `Ready`, `Ready=False` с не-терминальной причиной N2a): parent **`Ready=False`**, reason **`ChildSnapshotPending`** (`pkg/snapshot.ReasonChildSnapshotPending`); message поясняет этап (ожидание bind child content / ожидание `Ready=True` у child; при не-терминальном **`Ready=False`** у child в message передаются **reason и при наличии message** child для observability).
- **Child в терминальном сбое** N2a (`Ready=False` с причиной из allowlist терминальных root-capture ошибок): parent **`Ready=False`**, reason **`ChildSnapshotFailed`** (`pkg/snapshot.ReasonChildSnapshotFailed`); message содержит имя child и reason/message child.
- Успешный parent после агрегации: **`Ready=True`**, reason **`Completed`** (`pkg/snapshot.ReasonCompleted`), как у N2a leaf после MCP.
- Список **required** children vs optional — зафиксировать в spec/API при полном N2b.

**Имплементация (generic):** parent **`Ready`** по **`status.childrenSnapshotRefs`** — **`reconcileChildrenRefsE6ParentReadyOrPatch`** + **`SummarizeChildrenSnapshotRefsForParentReadyE6`** / **`PickParentReadyReasonE6`** (резолв через **`GVKRegistry`**, **`unstructured`**); дочерний **`NamespaceSnapshot`** в графе — обычный зарегистрированный snapshot-тип, не отдельный «scaffold»-путь. Исторический временный in-repo scaffold (PR2–PR3 в старых версиях плана) **удалён**; контракт и тесты — **[`spec/system-spec.md`](../spec/system-spec.md) §3.8**, **`namespacesnapshot_graph_e5_e6_integration_test.go`**, **PR5b**.

| Child state (resolved child snapshot по ref) | Parent `Ready` | Parent `Ready` reason |
|-------------------------------------------|----------------|------------------------|
| Нет привязанного `NamespaceSnapshotContent` / child reconcile в процессе | `False` | `ChildSnapshotPending` |
| `Ready` отсутствует или `Unknown` | `False` | `ChildSnapshotPending` |
| `Ready=False` с не-терминальной причиной (например `ManifestCheckpointPending`) | `False` | `ChildSnapshotPending` |
| `Ready=False` с терминальной причиной N2a (whitelist в коде) | `False` | `ChildSnapshotFailed` |
| `Ready=True` и у parent уже persisted MCP | `True` | `Completed` |

**Ready=True не означает:**

- Что сохранены **данные томов**.
- Что собран «экспортный» архив в смысле полного продукта из vision-документа.
- Что restore / dry-run гарантированно пройдёт в любом кластере без дополнительных проверок.

---

## 12. Access model

**Выбрано:** namespaced root — [`decisions/namespace-snapshot-scope.md`](decisions/namespace-snapshot-scope.md).

- Проще делегирование прав и UX относительно владельцев namespace; целевой namespace на текущем этапе = `metadata.namespace` у `NamespaceSnapshot`.
- `NamespaceSnapshotContent` остаётся **cluster-scoped**; права на content и OK — отдельно от namespaced root (см. RBAC в шаблонах модуля и будущий N2).

---

## 13. Blocking decisions and open questions

### 13.1 Blocking (MUST до N1)

1. ~~**Cluster-scoped vs namespaced NamespaceSnapshot**~~ — **снято:** выбрано **namespaced** в [`decisions/namespace-snapshot-scope.md`](decisions/namespace-snapshot-scope.md).

### 13.2 Open (до финализации API / реализации)

1. Точные **имена condition types** и их соответствие существующим unified CRD (в т.ч. **`ChildSnapshotFailed`** для N2b — §11.1).
2. **Deletion policy** на уровне class vs spec (что наследуется от SnapshotClass аналога).
3. ~~Минимальный набор **GVR** для N2a~~ — зафиксирован в **§4.5**; остаётся перенос в CRD/code и при необходимости сужение/расширение через PR с обновлением §4.5.
4. **Политика удаления root при незавершённом capture** — **§5.2 п.1:** in-flight MCR убирается **GC** по ownerRef на **`NamespaceSnapshot`** после полного удаления root; явный `Delete(MCR)` из `reconcileDelete` **не** делается.
5. **Required vs optional** дочерние snapshot для агрегации **N2b** — §11.1.

---

## 14. Bootstrap / registry / RBAC impact

- Зарегистрировать пару **`NamespaceSnapshot` / `NamespaceSnapshotContent`** в `pkg/unifiedbootstrap` (и при необходимости DSC): resolve, watches, RBAC в шаблонах. **Убрать** использование **generic `SnapshotContent`** как носителя для namespace root (**без** миграции — целевая схема сразу NSC).
- Не смешивать в одном PR без необходимости с треками M1/M2 (manifest) и крупными изменениями R3 — см. [`implementation-plan.md`](implementation-plan.md).

---

## 15. Testing strategy

- **N1:** уже в коде — lifecycle, delete, mismatch, recovery (fake Ready).
- **N2a:** integration — временный MCR, MCP, root OK, persisted Ready (**после success MCR отсутствует**), download одного снимка, Retain; см. [`testing/e2e-testing-strategy.md`](../testing/e2e-testing-strategy.md).
- **N2b:** integration — дерево, refs, parent Ready, aggregated download (manifests-only).
- **N3:** envtest — recovery после рестарта контроллера поверх закрытого N2a (и при необходимости N2b).
- **N4+:** лимиты большого namespace, таймауты; затем кейсы **N5** (data и полный ТЗ).
- Идемпотентность ensure **NamespaceSnapshotContent**, OK, MCR/MCP и stable naming — отдельные кейсы.

---

## 16. Поставка (milestones N0–N5, не `status.phase`)

Имена **N0–N5** — этапы [`implementation-plan.md`](implementation-plan.md) §2.4; **не** поля API. Статус объектов — только **conditions** (§6). Детальный бэклог **N5** — только по ТЗ в `snapshot-rework/` (data-layer и полный сценарий вне manifests-only дерева). **Декомпозиция N2** (**N2a** / **N2b**, DoD, out-of-scope) — **[`implementation-plan.md`](implementation-plan.md) §2.4.1** (SSOT; этот §16 — краткий указатель).

### N0 — Contract / gate

1. **Chosen option** в [`decisions/namespace-snapshot-scope.md`](decisions/namespace-snapshot-scope.md) (≠ TBD) — §13.1.
2. Сверка apiVersion/group NS и NSC: ТЗ `snapshot-rework` ↔ CRD в репозитории.
3. Пара **`NamespaceSnapshot` / `NamespaceSnapshotContent`** и **conditions-only** — decisions; root/content binding §4.2; ObjectKeeper — §4.3 + ТЗ.

### N1 — CRD / API / skeleton lifecycle

✅ Закрыт: подготовительный слой без ObjectKeeper, без real manifest capture, без дочернего дерева — см. [`implementation-plan.md`](implementation-plan.md) §2.4 и §2.4.1 (граница N1). Код **не** считается «временным» — это база для N2.

### N2 — Manifests-only: N2a затем N2b

SSOT: **§2.4.1** и **§2.4.2** (декомпозиция N2b по PR) в [`implementation-plan.md`](implementation-plan.md).

- **N2a:** OK + **MCR→ManifestCheckpoint** + запись результата в **`NamespaceSnapshotContent.status`** + **Ready** только по persisted MCP + download **одного** снимка; data — placeholders.
- **N2b:** дочерние snapshot/content, **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`**, агрегированный **Ready** parent, **aggregated manifests download** subtree; всё ещё без data-flow. Вход в N2b — с **PR1** (только поля графа + docs/spec), см. **§2.4.2**.

§8 — логический контракт манифестов; физическое хранение N2a согласовать с MCP/chunks и download path в коде.

### N3 — Envtest / hardening

Расширение сценариев §15 (в т.ч. recovery после рестарта контроллера); негативные кейсы сверх уже закрытых в N1 и N2a.

### N4 — После N2 (N2a+N2b)

Углублённые лимиты большого namespace (§8.6), таймауты — см. [`implementation-plan.md`](implementation-plan.md) §2.4 и M2.

### N5 — За пределами manifests-only дерева

Data-layer (volume/VSC/VCR), полный export/import, restore с данными, DSC traversal в полном объёме — по [`snapshot-rework/2026-01-25-namespace-snapshot.md`](../../../snapshot-rework/2026-01-25-namespace-snapshot.md). **Дерево манифест-only** — в **N2b**, не в N5.

---

## 17. Layering (напоминание)

| Слой | Ответственность |
|------|-----------------|
| A. API / object lifecycle (NamespaceSnapshot controller) | Финализатор, content ensure, **conditions** (без `status.phase`), вызов domain capture, удаление |
| B. Domain capture | План захвата, сериализация, запись в backend, возврат metadata/result |
| C. Shared runtime / unified | Bind/sync для пары **NamespaceSnapshot / NamespaceSnapshotContent** (и при необходимости общие хелперы conditions с другими snapshot-типами) |
