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

**NamespaceSnapshot** — namespaced root / intention; результат фиксируется в cluster-scoped **`NamespaceSnapshotContent`** (пара 1:1 с root по смыслу ТЗ). Generic **`SnapshotContent`** для этого потока **не** используется как root-carrier. Связка с **ObjectKeeper** — по правилам из `snapshot-rework`; **целевой** режим удержания persisted результата — **`FollowObjectWithTTL`** (как в ТЗ). **N2a в коде сейчас:** отдельный root OK в режиме **`FollowObject`** на **`NamespaceSnapshotContent`** (lifecycle helper: при удалении NSC OK удаляется вместе с ним); **`FollowObjectWithTTL`** — следующий шаг retention (см. §4.3.2, [`implementation-plan.md`](implementation-plan.md) §2.4.1).

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

Полная схема полей OK — в [`snapshot-rework/2026-01-25-namespace-snapshot.md`](../../../snapshot-rework/2026-01-25-namespace-snapshot.md) и ADR там же. Ниже — **design lock** для N2a/N2b, согласованный с текущим кодом manifest-линии (`ManifestCheckpointController`: OK `ret-mcr-*` в режиме **FollowObject** на MCR, **ManifestCheckpoint** с `ownerReference` на этот OK, **chunks** с `ownerReference` на MCP).

#### 4.3.1 Правило: ownerReference vs ObjectKeeper

- **`ownerReference`** используем там, где объекты в **совместимом scope** для Kubernetes GC (например оба cluster-scoped): **ManifestCheckpointContentChunk → ManifestCheckpoint** — chunks удаляются через ownerRef на MCP (как сейчас в коде).
- **ObjectKeeper** используем там, где нужен **cluster-scoped retention anchor** и/или связь проходит **границу scope** (namespaced ↔ cluster-scoped): логический bind **root ↔ NamespaceSnapshotContent** остаётся **spec/status**, не ownerRef; удержание результата снимка — отдельный OK.
- **ObjectKeeper нигде не подменяет** bind-контракт **`spec.namespaceSnapshotRef`** / **`status.boundSnapshotContentName`**.

#### 4.3.2 Два применения ObjectKeeper (не смешивать)

1. **Root / snapshot result:** **Целевой** режим — ObjectKeeper для **`NamespaceSnapshotContent`** в **`FollowObjectWithTTL`** — **retention anchor** после удаления root/namespace по policy (ТЗ). **N2a (реализация в `namespacesnapshot_capture.go`):** root OK создаётся в **`FollowObject`** на NSC по UID — **lifecycle helper**, не TTL-retention; при удалении NSC OK удаляется вместе с ним. Переход на **`FollowObjectWithTTL`** — отдельная доработка; код и этот абзац согласованы.
2. **Manifest capture execution path (уже в коде для MCR):** отдельный ObjectKeeper в режиме **`FollowObject`**, следующий за **ManifestCaptureRequest** — **технический** lifecycle для MCR→MCP: MCP держится через ownerRef на этот OK; при удалении MCR цепочка OK→MCP→chunks согласуется с GC (см. комментарии в `manifestcheckpoint_controller.go`). Это **не** тот же объект OK, что root OK для NSC (разные роли, разные режимы).

**Инвариант:** связь **NamespaceSnapshot → MCR** не обязана быть ownerRef; MCR создаётся/наблюдается **NamespaceSnapshot controller**; **ManifestCheckpointController** **только** исполняет **MCR → MCP** (+ chunks). Статусы **NS/NSC** пишет **NamespaceSnapshot controller** по наблюдению MCP/MCR.

**N2a technical debt (root OK vs retention):** root OK в **`FollowObject`** на NSC — **не** retention anchor для persisted манифеста; он живёт с NSC и исчезает вместе с ним. Удержание MCP/chunks в N2a опирается на цепочку **MCR→OK→MCP** и **явную политику delete/cancel** в `NamespaceSnapshot` controller (§5.2), а не на «root OK уже как в ТЗ». Полная семантика **Retain / независимость артефакта от root** в терминах **FollowObjectWithTTL** — отдельный этап; не интерпретировать N2a как «ObjectKeeper для результата уже завершён».

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

Проверено против `images/state-snapshotter-controller/internal/controllers/manifestcheckpoint_controller.go` (логика создания OK / MCP / chunks):

| Утверждение §4.3 | В коде |
|------------------|--------|
| ObjectKeeper для MCR в режиме **`FollowObject`** (следует за UID MCR) | Да: `ObjectKeeperSpec.Mode: "FollowObject"`, `FollowObjectRef` с UID MCR (см. комментарии ~L266–L291). |
| Имя OK **`ret-mcr-<namespace>-<mcrName>`** | Да: `retainerName` из namespace + имени MCR. |
| **ManifestCheckpoint** держится **ownerReference → ObjectKeeper** (controller) | Да: `OwnerReferences` на OK `ret-mcr-…` (~L373–L380). |
| **Chunks** держатся **ownerReference → ManifestCheckpoint** | Да: chunk `OwnerReferences` на MCP (~L946–L956 в потоке createChunks). |
| Отдельная модель удержания для NS-flow не смешивается | Корневой OK для NSC **не** создаётся в `ManifestCheckpointController`; он в **NamespaceSnapshot** controller (**N2a**): **`FollowObject`** на NSC (`ret-nssnap-…`), см. `namespacesnapshot_capture.go`. **`FollowObjectWithTTL`** — запланированное усиление retention, не блокирует N2a. |

Итог: **§4.3 для manifest execution path соответствует текущей реализации.** При смене именования OK/MCP в коде — обновлять §4.3 и этот подпункт.

---

### 4.7 N2a — корреляция NamespaceSnapshot ↔ MCR ↔ MCP (до wiring)

**Цель:** после рестарта и без полей MCR на root находить «свой» MCR/MCP детерминированно и без list по всему кластеру как основного пути.

1. **Имя `ManifestCaptureRequest`:** детерминированно от **`NamespaceSnapshot.metadata.uid`** и namespace root, например **`nss-{metadata.uid}`** (UID как в API, с дефисами — допустимое имя объекта Kubernetes). Namespace MCR = **`metadata.namespace` того же `NamespaceSnapshot`**. Тогда **`Get`** MCR по фиксированному ключу идемпотентен; коллизий с другими линиями не будет, если префикс/правило зарезервирован за namespace-snapshot flow.
2. **Имя `ManifestCheckpoint`:** после появления MCR вычисляется **та же формула**, что в `ManifestCheckpointController.generateCheckpointNameFromUID(mcrUID)` — префикс **`mcp-`** + **16 hex** от SHA256(UID MCR) (первые 8 байт хеша). **NamespaceSnapshot controller** должен вызывать **общую** функцию из пакета, разделяемого с manifest-контроллером (или вынести в `pkg/`), **без** дублирования строки алгоритма.
3. **Labels (дополнительно):** на создаваемом NS-контроллером MCR задать метки вида **`state-snapshotter.deckhouse.io/namespace-snapshot-uid=<root-uid>`** и при необходимости **`…/namespace-snapshot-content-name=<nsc-name>`** — для отладки и редкого recovery; **первичный** путь — `Get` по имени из п.1.
4. **Stale после рестарта:** если MCR с ожидаемым именем есть, но **`metadata.uid` не совпадает** с текущим `NamespaceSnapshot` (root пересоздан с тем же именем) — считать MCR **чужим**: **не** переиспользовать; выставить на NSC/NS терминальную ошибку или удалить MCR по политике (конкретное поведение — в коде + тест «пересоздание root с тем же именем»). Если UID root совпадает — продолжать observe существующего MCR/MCP. **Root OK** (`ret-nssnap-…` по namespace+имени root): при новом поколении root с тем же именем, но **новом** `NamespaceSnapshotContent` (другой UID), контроллер **удаляет** устаревший OK и создаёт новый, следующий за актуальным NSC (см. `ensureNamespaceSnapshotRootObjectKeeper`).
5. **Immutability `spec.targets` (N2a):** после создания MCR список targets **не** обновляется молча при расхождении с текущим list namespace; расхождение → терминальная ошибка **`CapturePlanDrift`** (оператор удаляет MCR и повторяет capture). Стабильный порядок targets в spec — отсортированный набор (см. `BuildManifestCaptureTargets`).

**Публичный статус root по-прежнему без имён MCR** (§4.4); корреляция — внутренняя для контроллера + опционально labels.

---

## 5. Reconcile model

### 5.1 Normal flow (логические шаги)

**Watch:** контроллер подписан на **`NamespaceSnapshot`** и на связанный **`NamespaceSnapshotContent`** (enqueue по `spec.namespaceSnapshotRef`), чтобы изменения content (в т.ч. ручной repair) снова прогоняли reconcile root без отдельного события на root.

1. Fetch; при удалении — §5.2.
2. Ensure finalizer на root.
3. Validation (namespace существует, class/policy валидна, spec консистентен) — §7.
4. Ensure **NamespaceSnapshotContent** + запись bind в status root и ссылки root↔content — §4.2; ensure **ObjectKeeper** по ТЗ — §4.3 (**N2a+**).
5. **Reconcile capture** через domain service: в **N2a** — внутренний **ManifestCaptureRequest → ManifestCheckpoint** (observe MCR/MCP, таймауты); при альтернативной реализации — Job/runner, согласованный с §8 и [`implementation-plan.md`](implementation-plan.md) §2.4.1.
6. По успеху: записать artifact metadata (в т.ч. ссылка на MCP) в **`NamespaceSnapshotContent.status`**, синхронизировать root (Ready и условия).
7. Не выполнять тяжёлую работу list/watch namespace в горячем пути reconcile root, если она перенесена в MCR/controller capture path (политика ресурсов apiserver).

### 5.2 Deletion flow

Учитывать **deletion policy** на content (Retain / Delete) и финализаторы root/content; ниже — **предлагаемый порядок для MVP** (уточняется при реализации, но фиксируется, чтобы снизить гонки и «висячие» артефакты).

**Proposed deletion order (MVP)**

1. **Отмена capture при удалении root (N2a, зафиксированная политика в коде):**
   - удалить **ManifestCaptureRequest** по детерминированному имени (§4.7);
   - **requeue**, пока MCR **исчез** из API;
   - затем **best-effort `Delete(ManifestCheckpoint)`** — осознанная стратегия контроллера (идемпотентно с цепочкой **MCR→OK→MCP** в production: deckhouse может убрать MCP через OK, но явный Delete не конфликтует и задаёт однозначный cancel);
   - **chunks** уходят через **ownerReference на MCP**, когда MCP удалён;
   - **Не** ждать бесконечно завершения capture на финализаторе root без этой отмены. Исключения — только при явном ADR.
2. Если **deletionPolicy = Delete** (или эквивалент для артефакта): инициировать **удаление объекта в backend**; дождаться подтверждения **или** зафиксировать best-effort + явное условие/событие **Warning** (не оставлять поведение неопределённым в коде без комментария в spec).
3. Довести **NamespaceSnapshotContent** до согласованного терминального состояния (артефакт удалён или помечен retained согласно политике), снять с content финализаторы, допускающие удаление; согласовать с **ObjectKeeper** по ТЗ.
4. Снять финализатор с **NamespaceSnapshot**, удалить root.

**Инвариант:** финализатор root **снимается только после** того, как NamespaceSnapshotContent достиг **согласованного с deletion policy терминального состояния**; не раньше.

**Скелет (Phase 2, API-объект content):** при **DeletionPolicy=Delete** на `NamespaceSnapshotContent` контроллер вызывает `Delete` на CR и **не** снимает финализатор с root, пока `Get(NamespaceSnapshotContent)` не вернёт **NotFound** (requeue до исчезновения объекта из API).

При **Retain:** артефакт и при необходимости **NamespaceSnapshotContent** переживают root — явный итог reconcile (**conditions** на content и ссылки orphaning). См. ТЗ в `snapshot-rework`.

### 5.3 Recovery after restart

- Восстановление связи root ↔ content по §4.2.
- Восстановление связи с MCR/MCP по **§4.7** (детерминированное имя MCR, формула имени MCP от UID MCR).
- Повторный observe capture без дублирования работы (идемпотентный ensure MCR / результата MCP).

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
- **N2b aggregated download:** новый маршрут или композиция нескольких вызовов к тому же механизму склейки — в реализации N2b; семантика ошибок та же (нет готового child MCP → не включать или fail whole — зафиксировать в N2b PR).

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

- **Источник правды:** `NamespaceSnapshot` + **`NamespaceSnapshotContent`** + artifact metadata (имя **ManifestCheckpoint** на NSC, conditions). Публичный контракт статусов — §4.4.
- **`ManifestCaptureRequest` / `ManifestCheckpoint`:** в **N2a** — **внутренний** execution path; MCR **не** в статусе root (§4.4).
- **Разделение ответственности (N2a):**
  - **`NamespaceSnapshot` controller:** ensure **NamespaceSnapshotContent**; ensure **root ObjectKeeper** (**`FollowObject`** на NSC в N2a; **`FollowObjectWithTTL`** — §4.3.2); ensure **MCR** (имя/namespace по **§4.7**); observe **MCR/MCP**; пишет **status** на **NamespaceSnapshot** и **NamespaceSnapshotContent** (в т.ч. зеркалирует терминальные ошибки capture на NSC **`conditions`**); выставляет **Ready** по persisted MCP.
  - **`ManifestCheckpointController`:** **только** исполняет **MCR → ManifestCheckpoint** (+ chunks); создаёт/использует **отдельный ObjectKeeper (FollowObject)** для MCR/MCP lifecycle (как в текущем коде); **не** пишет публичный статус NS/NSC.
- Публично наружу: статус root/content по §4.4; маркеры partial/warnings согласно §9. Статус Job — только если Job введён поверх MCR; **Ready** не выводить без persisted MCP (см. [`implementation-plan.md`](implementation-plan.md) §2.4.1).

Связь OK для MCR/MCP vs root NSC — §4.3. См. также [`../README.md`](../README.md).

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

- **`Ready=True` у parent** только если: **собственный** `NamespaceSnapshotContent` в состоянии готовности (как у листа, N2a) **и** все **required** дочерние snapshot (по graph из **`childrenSnapshotRefs`** / доменной логике) также **`Ready=True`**.
- **Child в процессе** (ещё не Ready, не Failed): parent **не** `Ready=True`; допускаются **Progressing** / незавершённый capture на parent (конкретные condition types — в CRD).
- **Child в терминальном сбое** (`Ready=False` / Failed): parent **`Ready=False`** с устойчивым reason, например **`ChildSnapshotFailed`** (имя согласовать с `pkg/snapshot`), с указанием какого child.
- Список **required** children vs optional — зафиксировать в spec/API при введении N2b (до кода агрегации).

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
4. ~~Политика при удалении во время capture~~ — для **N2a** зафиксировано в **§5.2** (**отмена через delete MCR**).
5. **Required vs optional** дочерние snapshot для агрегации **N2b** — §11.1.

---

## 14. Bootstrap / registry / RBAC impact

- Зарегистрировать пару **`NamespaceSnapshot` / `NamespaceSnapshotContent`** в `pkg/unifiedbootstrap` (и при необходимости DSC): resolve, watches, RBAC в шаблонах. **Убрать** использование **generic `SnapshotContent`** как носителя для namespace root (**без** миграции — целевая схема сразу NSC).
- Не смешивать в одном PR без необходимости с треками M1/M2 (manifest) и крупными изменениями R3 — см. [`implementation-plan.md`](implementation-plan.md).

---

## 15. Testing strategy

- **N1:** уже в коде — lifecycle, delete, mismatch, recovery (fake Ready).
- **N2a:** integration — MCR/MCP, OK, persisted Ready, download одного снимка, Retain; см. [`testing/e2e-testing-strategy.md`](../testing/e2e-testing-strategy.md).
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

SSOT: **§2.4.1** в [`implementation-plan.md`](implementation-plan.md).

- **N2a:** OK + **MCR→ManifestCheckpoint** + запись результата в **`NamespaceSnapshotContent.status`** + **Ready** только по persisted MCP + download **одного** снимка; data — placeholders.
- **N2b:** дочерние snapshot/content, **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`**, агрегированный **Ready** parent, **aggregated manifests download** subtree; всё ещё без data-flow.

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
