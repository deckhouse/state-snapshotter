# NamespaceSnapshot controller — MVP design

## 1. Status of this document

**Тип:** design (целевая модель MVP, не нормативный контракт). После стабилизации API и поведения — вынести инварианты в [`spec/system-spec.md`](../spec/system-spec.md) и тесты.

**Связанные документы:**

- Решение по виду content: [`decisions/namespace-snapshot-content-decision.md`](decisions/namespace-snapshot-content-decision.md)
- **Gate до Phase 2:** cluster-scoped vs namespaced — [`decisions/namespace-snapshot-scope.md`](decisions/namespace-snapshot-scope.md)
- Расширенное видение (orchestration, дочерние снимки, тома и т.д.): [`../../../snapshot-rework/2026-01-25-namespace-snapshot.md`](../../../snapshot-rework/2026-01-25-namespace-snapshot.md)

---

## 2. Scope and non-goals

### 2.1 MVP semantics (главное решение)

В MVP **NamespaceSnapshot** — это **root / intention** для снимка **состояния и конфигурации** namespace (манифестный срез), с такими ограничениями:

- **Не** оркестрирует автоматически volume data (PVC data, CSI VolumeSnapshot и т.п.).
- **Не** ведёт полную иерархию **child domain snapshots** (VirtualMachineSnapshot, …) внутри одного reconcile этого контроллера.
- Результат фиксируется в **общем `SnapshotContent`**, а не в отдельном `NamespaceSnapshotContent`.
- **Capture** выполняется отдельным **domain runner** (например Job); контроллер root orchestrates lifecycle, не обязан сам листать весь apiserver в процессе reconcile.
- **Shared runtime** (generic Snapshot / SnapshotContent контроллеры и общие хелперы) отвечает за **bind, статус, финализаторы, cleanup** в рамках единого паттерна unified snapshots.

### 2.2 Out of scope for MVP

- Child domain snapshots и priority traversal по DSC mappings внутри одного NamespaceSnapshot.
- Automatic PVC / VolumeSnapshot orchestration как часть этого контроллера.
- Отдельный CRD `NamespaceSnapshotContent`.
- Экспорт/импорт полного иерархического bundle и CLI-потоки (см. vision-документ).
- Data restore orchestration и «manifests + data» как единый сценарий здесь.
- **`ManifestCaptureRequest` как обязательный публичный контракт** — см. §10; в MVP источник правды — root + content + artifact metadata.
- Поля вроде `childrenSnapshotContentRefs` на root для дерева дочерних снимков.

---

## 3. Relationship to previous NamespaceSnapshot design

Документ [`snapshot-rework/2026-01-25-namespace-snapshot.md`](../../../snapshot-rework/2026-01-25-namespace-snapshot.md) описывает **целевое расширенное видение** NamespaceSnapshot как широкого **orchestration-root** для manifests, domain children и volume-related снимков.

**Настоящий документ** фиксирует **MVP-модель**, в которой NamespaceSnapshot **сужен** до snapshot namespace state/configuration и использует **единый `SnapshotContent`**.

Vision-документ остаётся полезным для долгосрочного направления; реализация и тесты MVP должны опираться на **этот** файл и будущие выдержки из `system-spec.md`, а не на полный оркестрационный сценарий из 2026-01-25.

---

## 4. API model

### 4.1 NamespaceSnapshot (черновик полей)

До **разрешённого** решения cluster-scoped vs namespaced ([`decisions/namespace-snapshot-scope.md`](decisions/namespace-snapshot-scope.md), §13) — логические поля без привязки к финальному CRD.

**Spec (логически):**

- Источник: `source.namespaceName` (или эквивалент).
- Класс/политика: `snapshotClassName` / `className` (как в продуктовой модели unified snapshots).
- Опционально: include/exclude групп ресурсов (MVP — минимальный набор или фиксированный профиль).
- Опционально позже: `capturePolicy` (см. §9); в MVP допустимо заложить поле, но **выставить только fail-closed**.

**Status (логически):**

- `phase` — краткое резюме; предикаты для операторов — **conditions**.
- `conditions`: как минимум согласованный набор с unified-паттерном (например Ready, Bound, Progressing, Failed; при необходимости CaptureStarted, ArtifactStored).
- `boundSnapshotContentName` — имя привязанного `SnapshotContent`.
- `observedGeneration`.
- `startedAt`, `completedAt` (опционально).
- Сводные поля прогресса/ошибки по необходимости (без дублирования детального состояния Job).

### 4.2 SnapshotContent binding (контракт)

**Root → Content**

- `NamespaceSnapshot.status.boundSnapshotContentName` указывает на созданный `SnapshotContent`.

**Content → Root**

- В `SnapshotContent.spec` (или эквивалентном блоке ссылок): **двусторонняя** ссылка на root: `apiVersion`, `kind`, `name`, **`uid`**.

**Инварианты**

1. Один NamespaceSnapshot в активной фазе связан **не более чем с одним** SnapshotContent (смысловой «активный» content для данного root).
2. SnapshotContent, привязанный к NamespaceSnapshot, **не перепривязывается** к другому root.
3. После рестарта контроллер восстанавливает связь по: `status` root, или `rootRef` на content, или **детерминированному имени** content.

**Идемпотентность**

- Операции ensure: финализатор, создание/поиск content, запуск capture, запись статуса — безопасны при повторной доставке события и после рестарта.

**Нейминг (рекомендация MVP)**

- Стабильное имя content от **UID** root (не от `metadata.name` пользователя), например префикс + короткий идентификатор на основе UID, чтобы снизить гонки и коллизии при переименовании/повторном создании.

**Уникальность и коллизии**

- Уникальность имени content **производна от UID** root: повторное создание объекта с тем же `metadata.name` получает **другой UID** → ожидается **новый** SnapshotContent; совпадение имени пользователя **не** означает reuse content.
- Если reconcile не успел записать `boundSnapshotContentName` в status root, восстановление связи — по **`rootRef` на SnapshotContent** и/или детерминированному имени по UID.

Точное имя и схема полей — в CRD и в последующей выдержке `system-spec.md`.

---

## 5. Reconcile model

### 5.1 Normal flow (логические шаги)

1. Fetch; при удалении — §5.2.
2. Ensure finalizer на root.
3. Validation (namespace существует, class/policy валидна, spec консистентен) — §7.
4. Ensure SnapshotContent + запись bind в status root и rootRef на content — §4.2.
5. **Reconcile capture** через domain service: старт Job/runner при необходимости, observe прогресса, таймауты/heartbeat — §8 и Phase 3 плана.
6. По успеху: записать artifact metadata в `SnapshotContent.status`, синхронизировать root (Ready и условия).
7. Не выполнять тяжёлую работу list/watch namespace в горячем пути reconcile root, если это перенесено в runner (политика ресурсов apiserver).

### 5.2 Deletion flow

Учитывать **deletion policy** на content (Retain / Delete) и финализаторы root/content; ниже — **предлагаемый порядок для MVP** (уточняется при реализации, но фиксируется, чтобы снизить гонки и «висячие» артефакты).

**Proposed deletion order (MVP)**

1. **Пока идёт capture:** по политике — попытка **отмены** runner (например delete Job / сигнал отмены) **или** **ожидание** завершения capture (конкретная политика — TBD, но должна быть одна на продукт и покрыта тестом).
2. Если **deletionPolicy = Delete** (или эквивалент для артефакта): инициировать **удаление объекта в backend**; дождаться подтверждения **или** зафиксировать best-effort + явное условие/событие **Warning** (не оставлять поведение неопределённым в коде без комментария в spec).
3. Довести **SnapshotContent** до согласованного терминального состояния (артефакт удалён или помечен retained согласно политике), снять с content финализаторы, допускающие удаление.
4. Снять финализатор с **NamespaceSnapshot**, удалить root.

**Инвариант:** финализатор root **снимается только после** того, как SnapshotContent достиг **согласованного с deletion policy терминального состояния** (deletion-consistent terminal state); не раньше.

При **Retain:** артефакт и при необходимости **SnapshotContent** переживают root — это **явный, согласованный итог reconcile** (условия/фазы content и ссылки orphaning), а не побочный эффект «root удалили — что-то осталось». Поведение должно совпадать с общим unified-паттерном orphaning, если он применим.

### 5.3 Recovery after restart

- Восстановление связи root ↔ content по §4.2.
- Повторный observe capture без дублирования работы (идемпотентный ensure Job/результата).

---

## 6. Conditions and phases

- **Conditions** — основной источник истины для операторов и автоматизации.
- **Phase** — краткое резюме, согласованное с conditions.

**Минимальный lifecycle (логически, MVP)**

```text
Pending → Bound → InProgress → Ready
                      └──────→ Failed
Deleting (при удалении root)
```

- **Pending** — root валидируется / ещё нет привязанного content.
- **Bound** — SnapshotContent создан, `rootRef` и `boundSnapshotContentName` согласованы.
- **InProgress** — вводится **только после** **CaptureStarted=True** (runner реально создан / принят к исполнению); не путать с «фаза пошла, а Job ещё нет».
- **Ready** / **Failed** — терминальные для поколения spec (см. §11, §7).
- **Deleting** — финализация и cleanup (см. §5.2).

**Conditions (минимум для интеграции с runner)**

Помимо Bound / Ready / Failed, в unified часто есть **Progressing**; для capture добавляются **CaptureStarted** / опционально **ArtifactStored**.

- **CaptureStarted=True** — runner принят к исполнению (Job создан или эквивалент). **Предикат для входа в фазу InProgress** (см. выше).
- **ArtifactStored=True** (опционально, если отличается от финального Ready на content) — данные записаны в backend; можно слить с Ready на root, если дублирование не нужно.

**Progressing vs CaptureStarted (избежать лишнего дубля в коде):** для MVP допустимо оба, но до жёсткой фиксации в CRD/`pkg/snapshot` стоит выбрать одну линию: либо **Progressing** как зонт для «идёт работа» (включая capture), либо **минимальный набор** без одновременного Progressing=True и CaptureStarted=True, если они семантически совпадают. Иначе возможны лишние условия при одной фазе InProgress.

Точный набор имён — согласовать с `pkg/snapshot` и CRD; важно, чтобы **фаза InProgress** имела опору в conditions, иначе observe runner «висит в воздухе».

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

- Жизненный цикл артефакта в backend **управляется через пару** NamespaceSnapshot → SnapshotContent (политика удаления на content / class): root инициирует сценарии, content отражает фактическое состояние хранения.
- **Deletion policy** определяет, удаляется ли артефакт при удалении root или сохраняется (Retain).
- **Backend / repository GC** не считается **основным** механизмом консистентности для MVP: он может существовать как вспомогательный, но оператор должен понимать гарантии из reconcile + policy, а не полагаться на неявный GC.
- **Повторное использование артефакта и шаринг ссылок** между несколькими root в MVP **не поддерживаются** (**unsupported**); не оформлять как «тихий» edge case без отдельного контракта.

### 8.6 Large-namespace capture constraints (MVP)

Явные ограничения для runner (нормативно для реализации Phase 3):

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

- **Источник правды в MVP:** `NamespaceSnapshot` + `SnapshotContent` + artifact metadata (+ при необходимости статус Job как implementation detail).
- **`ManifestCaptureRequest` / `ManifestCheckpoint`:** если runner внутри использует те же механики, что и manifest line, это **внутренний** implementation detail, **не** дублирующий публичный статус для оператора рядом с root/content.
- Публично наружу: статус root, статус content, artifact metadata, маркеры partial/warnings согласно §9.

См. также разделение линий в [`../README.md`](../README.md).

---

## 11. Ready semantics (почти нормативно для MVP)

**`NamespaceSnapshot` Ready=True означает:**

- Root валиден (runtime validation прошла).
- `SnapshotContent` создан и **корректно привязан** к root.
- Capture **завершён успешно** (в терминах §9 — без провала fail-closed).
- Артефакт **persisted** в backend.
- Метаданные артефакта записаны в `SnapshotContent.status` (или согласованное поле).
- Дальнейший reconcile **не ожидает** незавершённых операций capture/runner для этого поколения spec.

**Ready=True не означает:**

- Что сохранены **данные томов**.
- Что собран «экспортный» архив в смысле полного продукта из vision-документа.
- Что restore / dry-run гарантированно пройдёт в любом кластере без дополнительных проверок.

---

## 12. Access model

Зависит от **разрешённого** решения cluster-scoped vs namespaced — [`decisions/namespace-snapshot-scope.md`](decisions/namespace-snapshot-scope.md).

- **Cluster-scoped:** сильнее выраженные требования к RBAC, admission, SubjectAccessReview и ограничению доступа по `spec.source.namespaceName`. Если выбран этот вариант — добавить в этот раздел **explicit cost** (webhook, модель прав).
- **Namespaced:** проще делегирование прав и UX, но семантика «root относительно целевого namespace» требует явного описания в CRD (тот же namespace / другой namespace только через политику продукта).

---

## 13. Blocking decisions and open questions

### 13.1 Blocking (MUST до Phase 2)

1. **Cluster-scoped vs namespaced NamespaceSnapshot** — архитектурная развилка: RBAC, admission, restore access, форма CRD, тесты. Зафиксировать в [`decisions/namespace-snapshot-scope.md`](decisions/namespace-snapshot-scope.md). **Формальный критерий допуска Phase 2 — один:** пока **Chosen option = TBD**, Phase 2 не начинать (см. Gate в том файле).

### 13.2 Open (до финализации API / реализации)

1. Точные **имена condition types** и их соответствие существующим unified CRD.
2. **Deletion policy** на уровне class vs spec (что наследуется от SnapshotClass аналога).
3. Минимальный набор **GVR по умолчанию** для MVP capture и механизм include/exclude (простой allowlist vs профили).
4. Политика при удалении во время capture: **отмена runner vs ожидание завершения** (§5.2) — выбрать одну для MVP и отразить в spec.

---

## 14. Bootstrap / registry / RBAC impact

- Сегодня в коде bootstrap-пара может ссылаться на **`NamespaceSnapshotContent`** — переход на общий `SnapshotContent` потребует согласованных правок: [`decisions/namespace-snapshot-content-decision.md`](decisions/namespace-snapshot-content-decision.md), `pkg/unifiedbootstrap`, CRD, RBAC шаблоны, DSC `spec.snapshotTypes` при регистрации пары в API.
- Не смешивать в одном PR без необходимости с треками M1/M2 (manifest) и крупными изменениями R3 — см. [`implementation-plan.md`](implementation-plan.md).

---

## 15. Testing strategy

- **Phase 2:** envtest — create → bind → ready (fake capture) → delete → recovery после рестарта контроллера (имитация).
- **Phase 3:** интеграционные сценарии с Job, таймаутами, крупным namespace (пагинация, лимиты), классификация ошибок terminal vs retriable.
- Идемпотентность ensure content и stable naming — отдельные кейсы.

---

## 16. Implementation phases (кратко)

### Phase 1 — Contract

1. Зафиксировать scope MVP (этот документ + decision по content).
2. Зафиксировать root/content binding (§4.2).
3. **Chosen option** в [`decisions/namespace-snapshot-scope.md`](decisions/namespace-snapshot-scope.md) заполнен (≠ TBD) — **gate**; см. §13.1.
4. Зафиксировать Ready, Failed, deletion semantics (§5, §7, §11), включая политику cancel-vs-wait при delete (§13.2).
5. Зафиксировать artifact format, sanitization, ownership (§8).
6. Зафиксировать отказ от `NamespaceSnapshotContent` в MVP (decision doc).

**Результат:** утверждённые design + decision; затем выдержки в `system-spec.md`.

### Phase 2 — Runtime skeleton

1. CRD NamespaceSnapshot (и правки SnapshotContent при необходимости).
2. Bootstrap / registry / watches / RBAC.
3. Reconciler skeleton: finalizer, validation, ensure content, sync status, deletion flow.
4. Fake capture service.
5. Envtest по сценариям §15.

**Результат:** lifecycle без реального capture.

### Phase 3 — Real capture

1. Job runner, observe, heartbeat, timeout.
2. Persist artifact, error classification.
3. Соблюдение **§8.6** (pagination, streaming/chunked serialization, без загрузки всего namespace в память сразу), лимиты и deadlines.
4. Тесты на реалистичных наборах ресурсов.

**Результат:** end-to-end MVP.

---

## 17. Layering (напоминание)

| Слой | Ответственность |
|------|-----------------|
| A. API / object lifecycle (NamespaceSnapshot controller) | Финализатор, content ensure, фазы/conditions, вызов domain capture, удаление |
| B. Domain capture | План захвата, сериализация, запись в backend, возврат metadata/result |
| C. Shared runtime | Bind/sync между root и generic SnapshotContent, стандартный cleanup, общие хелперы conditions |
