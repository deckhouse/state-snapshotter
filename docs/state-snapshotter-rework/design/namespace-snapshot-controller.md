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

**NamespaceSnapshot** — namespaced root / intention; результат фиксируется в cluster-scoped **`NamespaceSnapshotContent`** (пара 1:1 с root по смыслу ТЗ). Generic **`SnapshotContent`** для этого потока **не** используется как root-carrier. Связка с **ObjectKeeper** — по правилам из `snapshot-rework`; целевой режим для namespace-flow — **FollowObjectWithTTL** (полный OK lifecycle — N2+).

**Поставка поэтапно** (см. §16 и [`implementation-plan.md`](implementation-plan.md)): сначала bind NS↔NSC + OK + conditions и скелет capture; полная оркестрация дочерних снимков, экспорт/импорт и restore — наращиванием до полного ТЗ, **не** переписывая SSOT в `snapshot-rework`.

- **Capture** тяжёлой работы — отдельный runner (Job и т.п.), root-контроллер не обязан list-ить весь apiserver в горячем пути.
- **Unified shared runtime** там, где применим общий паттерн Snapshot/SnapshotContent для **других** GVK; для пары namespace root используем **свой** content kind (`NamespaceSnapshotContent`), см. [`namespace-snapshot-content-decision.md`](decisions/namespace-snapshot-content-decision.md).

### 2.2 Out of scope на ранних milestones (N1–N2)

До выхода на полный сценарий ТЗ в одном релизе **можно** отложить (но SSOT остаётся в `snapshot-rework`):

- Полная оркестрация child domain snapshots / priority traversal **в одном** reconcile (см. ТЗ — там это есть; в коде — отдельные этапы после N2).
- Экспорт/импорт архива и CLI как в ТЗ.
- **`ManifestCaptureRequest` как обязательный публичный контракт** для оператора — см. §10; публично: root + **NamespaceSnapshotContent** + artifact metadata.

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

### 4.3 ObjectKeeper

Правила создания **ObjectKeeper**, **FollowObjectWithTTL** для корневого content, **FollowObject** для ManifestCheckpoint / VolumeSnapshotContent, дополнительные **ownerReference** — **не дублировать** здесь; следовать [`snapshot-rework/2026-01-25-namespace-snapshot.md`](../../../snapshot-rework/2026-01-25-namespace-snapshot.md) и связанным ADR в том каталоге. В поставке N2/N3 — отдельные задачи на reconciler OK и интеграционные тесты.

---

## 5. Reconcile model

### 5.1 Normal flow (логические шаги)

1. Fetch; при удалении — §5.2.
2. Ensure finalizer на root.
3. Validation (namespace существует, class/policy валидна, spec консистентен) — §7.
4. Ensure **NamespaceSnapshotContent** + запись bind в status root и ссылки root↔content — §4.2; ensure **ObjectKeeper** по ТЗ — §4.3.
5. **Reconcile capture** через domain service: старт Job/runner при необходимости, observe прогресса, таймауты/heartbeat — §8 и milestone **N2** (реальный capture).
6. По успеху: записать artifact metadata в **`NamespaceSnapshotContent.status`**, синхронизировать root (Ready и условия).
7. Не выполнять тяжёлую работу list/watch namespace в горячем пути reconcile root, если это перенесено в runner (политика ресурсов apiserver).

### 5.2 Deletion flow

Учитывать **deletion policy** на content (Retain / Delete) и финализаторы root/content; ниже — **предлагаемый порядок для MVP** (уточняется при реализации, но фиксируется, чтобы снизить гонки и «висячие» артефакты).

**Proposed deletion order (MVP)**

1. **Пока идёт capture:** по политике — попытка **отмены** runner (например delete Job / сигнал отмены) **или** **ожидание** завершения capture (конкретная политика — TBD, но должна быть одна на продукт и покрыта тестом).
2. Если **deletionPolicy = Delete** (или эквивалент для артефакта): инициировать **удаление объекта в backend**; дождаться подтверждения **или** зафиксировать best-effort + явное условие/событие **Warning** (не оставлять поведение неопределённым в коде без комментария в spec).
3. Довести **NamespaceSnapshotContent** до согласованного терминального состояния (артефакт удалён или помечен retained согласно политике), снять с content финализаторы, допускающие удаление; согласовать с **ObjectKeeper** по ТЗ.
4. Снять финализатор с **NamespaceSnapshot**, удалить root.

**Инвариант:** финализатор root **снимается только после** того, как NamespaceSnapshotContent достиг **согласованного с deletion policy терминального состояния**; не раньше.

**Скелет (Phase 2, API-объект content):** при **DeletionPolicy=Delete** на `NamespaceSnapshotContent` контроллер вызывает `Delete` на CR и **не** снимает финализатор с root, пока `Get(NamespaceSnapshotContent)` не вернёт **NotFound** (requeue до исчезновения объекта из API).

При **Retain:** артефакт и при необходимости **NamespaceSnapshotContent** переживают root — явный итог reconcile (**conditions** на content и ссылки orphaning). См. ТЗ в `snapshot-rework`.

### 5.3 Recovery after restart

- Восстановление связи root ↔ content по §4.2.
- Повторный observe capture без дублирования работы (идемпотентный ensure Job/результата).

---

## 6. Conditions (без `status.phase`)

- **Conditions** — **единственный** нормативный источник истины для операторов и автоматизации на `NamespaceSnapshot` (нет дублирующего `status.phase`). Решение: [`decisions/namespace-snapshot-status-surface.md`](decisions/namespace-snapshot-status-surface.md).

**Минимальный lifecycle** — выражается **только** через conditions и поля фактов (имя привязанного **NamespaceSnapshotContent**, `observedGeneration`):

```text
(нет Bound / Bound=False) → Bound=True → … capture … → Ready=True
                         └────────────────────────────→ Ready=False (терминальная ошибка)
Удаление root: финализатор + cleanup (§5.2); отдельного «phase Deleting» в API не требуется — достаточно conditions и deletionTimestamp.
```

- **Bound=True** — NamespaceSnapshotContent создан и согласован с root (поля ссылок по §4.2).
- **Ready=True** — снимок для данного поколения spec **успешно** завершён (в т.ч. fake capture на N1).
- **Ready=False** с устойчивым reason — терминальная ошибка пользователя/конфигурации (см. §7.1).
- **Progressing** / **CaptureStarted** / **ArtifactStored** — по мере появления реального runner (N2); смысл «идёт capture» задаётся **condition**, не отдельным enum в status.

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

- `metadata.json` — обязателен.
- `warnings.json` — опционален (или пустой массив в metadata).
- `bundle.tar.gz` (или аналог) — сериализованный набор манифестов.

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

Явные ограничения для runner (нормативно для реализации **N2**):

- Capture **не должен** загружать **всё** состояние namespace в память **одним** чтением: потоковая обработка или **chunking** обязательны для больших объёмов.
- **Пагинация** (`continue`) при list по GVR **обязательна**; отсутствие лимитов на размер ответа — риск для apiserver и runner.
- **Сериализация** bundle (например tar.gz) — **streaming** или по частям, чтобы не держать целый архив в RAM.

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

- **Источник правды:** `NamespaceSnapshot` + **`NamespaceSnapshotContent`** + artifact metadata (+ при необходимости статус Job как implementation detail).
- **`ManifestCaptureRequest` / `ManifestCheckpoint`:** если runner внутри использует те же механики, что и manifest line, это **внутренний** implementation detail, **не** дублирующий публичный статус для оператора рядом с root/content.
- Публично наружу: статус root, статус content, artifact metadata, маркеры partial/warnings согласно §9.

См. также разделение линий в [`../README.md`](../README.md).

---

## 11. Ready semantics (почти нормативно для MVP)

**`NamespaceSnapshot` Ready=True означает:**

- Root валиден (runtime validation прошла).
- `NamespaceSnapshotContent` создан и **корректно привязан** к root.
- Capture **завершён успешно** (в терминах §9 — без провала fail-closed).
- Артефакт **persisted** в backend.
- Метаданные артефакта записаны в `NamespaceSnapshotContent.status` (или согласованное поле).
- Дальнейший reconcile **не ожидает** незавершённых операций capture/runner для этого поколения spec.

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

1. Точные **имена condition types** и их соответствие существующим unified CRD.
2. **Deletion policy** на уровне class vs spec (что наследуется от SnapshotClass аналога).
3. Минимальный набор **GVR по умолчанию** для MVP capture и механизм include/exclude (простой allowlist vs профили).
4. Политика при удалении во время capture: **отмена runner vs ожидание завершения** (§5.2) — выбрать одну для MVP и отразить в spec.

---

## 14. Bootstrap / registry / RBAC impact

- Зарегистрировать пару **`NamespaceSnapshot` / `NamespaceSnapshotContent`** в `pkg/unifiedbootstrap` (и при необходимости DSC): resolve, watches, RBAC в шаблонах. **Убрать** использование **generic `SnapshotContent`** как носителя для namespace root (**без** миграции — целевая схема сразу NSC).
- Не смешивать в одном PR без необходимости с треками M1/M2 (manifest) и крупными изменениями R3 — см. [`implementation-plan.md`](implementation-plan.md).

---

## 15. Testing strategy

- **N3:** envtest — create → bind → ready (fake capture) → delete → recovery после рестарта контроллера (имитация).
- **N4+:** сценарии с Job, таймаутами, крупным namespace (пагинация, лимиты), terminal vs retriable; затем кейсы из ТЗ **N5**.
- Идемпотентность ensure **NamespaceSnapshotContent**, OK и stable naming — отдельные кейсы.

---

## 16. Поставка (milestones N0–N5, не `status.phase`)

Имена **N0–N5** — этапы [`implementation-plan.md`](implementation-plan.md) §2.4; **не** поля API. Статус объектов — только **conditions** (§6). Детальный бэклог **N5** — только по ТЗ в `snapshot-rework/`.

### N0 — Contract / gate

1. **Chosen option** в [`decisions/namespace-snapshot-scope.md`](decisions/namespace-snapshot-scope.md) (≠ TBD) — §13.1.
2. Сверка apiVersion/group NS и NSC: ТЗ `snapshot-rework` ↔ CRD в репозитории.
3. Пара **`NamespaceSnapshot` / `NamespaceSnapshotContent`** и **conditions-only** — decisions; root/content binding §4.2; ObjectKeeper — §4.3 + ТЗ.

### N1 — CRD / API

CRD, типы, codegen; убрать generic `SnapshotContent` как носитель для namespace root.

### N2 — Bootstrap + reconciler skeleton

Пара NS/NSC в bootstrap/unified, RBAC, watches; reconciler: finalizer, bind, **ObjectKeeper** (зачатки по ТЗ), fake capture, **conditions**.

### N3 — Envtest

Сценарии §15 (lifecycle, recovery, негативные кейсы).

### N4 — Real capture

Job runner, артефакт, §8.6, лимиты.

### N5 — Полный ТЗ

Оркестрация детей, экспорт/импорт, restore — по [`snapshot-rework/2026-01-25-namespace-snapshot.md`](../../../snapshot-rework/2026-01-25-namespace-snapshot.md), без правок ТЗ из `docs/`.

---

## 17. Layering (напоминание)

| Слой | Ответственность |
|------|-----------------|
| A. API / object lifecycle (NamespaceSnapshot controller) | Финализатор, content ensure, **conditions** (без `status.phase`), вызов domain capture, удаление |
| B. Domain capture | План захвата, сериализация, запись в backend, возврат metadata/result |
| C. Shared runtime / unified | Bind/sync для пары **NamespaceSnapshot / NamespaceSnapshotContent** (и при необходимости общие хелперы conditions с другими snapshot-типами) |
