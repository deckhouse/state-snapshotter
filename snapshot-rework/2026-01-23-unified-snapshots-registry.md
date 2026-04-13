## DomainSpecificSnapshotController: регистрация snapshot-типов и Deckhouse-managed RBAC

> **Модуль `state-snapshotter`:** нормативные выдержки и статусы реализации — [`docs/state-snapshotter-rework/spec/system-spec.md`](../docs/state-snapshotter-rework/spec/system-spec.md), [`design/implementation-plan.md`](../docs/state-snapshotter-rework/design/implementation-plan.md).

**Статус:** draft  
**Дата:** 2026-01-23  
**Контекст:** `2025-11-30-unified-snapshots.md`

### Scope документа
Данный ADR описывает, как **DomainSpecificSnapshotController (DSC)** становится источником истины
для watch/RBAC общего snapshot‑контроллера (registry‑подсистемы).

Далее под registry‑подсистемой понимается registry‑подсистема общего snapshot‑контроллера,
ответственная за:
- обработку DSC;
- регистрацию типов;
- управление watch и синхронизацию с RBAC через `RBACReady`.

## Design decision
Выбран DomainSpecificSnapshotController как единый platform‑level реестр snapshot‑типов
и точка синхронизации registry/RBAC для общего snapshot‑контроллера (через `RBACReady`).

### Мотивация и требования к дизайну
При проектировании snapshot‑подсистемы необходимо сразу заложить:
1. **Декларативную расширяемость snapshot‑типов** без изменения кода контроллера
   и без статических списков GVK.
2. **Детерминированную генерацию RBAC** для зарегистрированных типов
   с автоматическим пересчётом при изменении набора типов.
3. **Явный platform‑level контракт API**: какие ресурсы internal,
   какие создаются только контроллерами, какие доступны пользователям.

### Цели
- Ввести DSC как registry snapshot‑типов.
- Сделать RBAC детерминированным и автоматическим.
- Формализовать границу internal/user API.

### Bootstrap до появления DSC (не steady-state)

До развёртывания DSC система может работать в **bootstrap‑режиме**:

- **пустой** registry state для unified‑типов (нормально для rollout и dev);
- либо **ограниченный** список GVK за **feature flag** (переход от legacy `main.go`).

Этот режим **не** является целевой моделью; steady-state — **только** DSC как источник истины после внедрения.

---

## 1. DomainSpecificSnapshotController как реестр типов

### Обзор
DSC — platform‑level источник истины о snapshot‑типах платформы.
CRD без регистрации в DSC не делает snapshot‑тип поддерживаемым общим контроллером.

DSC создаётся модулями Deckhouse и не предназначен для
пользовательских инструментов/CLI/сторонних расширений.
Registry‑подсистема общего snapshot‑контроллера читает DSC как единственный источник
информации о поддерживаемых типах.

### Registry state vs runtime watch activation

**Registry state** (желаемый набор типов, GVK/GVR из discovery, учёт конфликтов) может обновляться **динамически** при изменении DSC.

**Runtime activation** (подключение watch/controller-runtime к `*Snapshot` / `*SnapshotContent`) в рамках одного процесса:

**Единая формула решения об активации watch:** `Accepted=True`, `RBACReady=True`, и у **обоих** conditions выполняется `observedGeneration == metadata.generation`. Condition **`Ready` в эту формулу не входит** — он только **отражает** готовность для операторов (и при пересчёте получает актуальное поколение записи).

- **Безопасно динамичны** в первую очередь: **add** новых типов и изменения **состояния активации** (например появление `RBACReady=True`, обновление `Accepted`/поколения) при **неизменном** resolved mapping для уже известного типа.
- **Update `spec`**, меняющий **resolved** GVK/GVR или семантику маппинга (новый snapshot/content kind для той же декларации), может требовать **рестарта** pod для полностью консистентного применения; не следует рассчитывать, что любой такой апдейт безопасен без рестарта.
- **полное снятие** watch для исчезнувшего типа без рестарта pod **не гарантируется** (ограничения controller-runtime); для консистентного cleanup может потребоваться **рестарт**.

Документ **не** требует симметрии «динамически добавили = так же динамически убрали» на уровне watch.

### API: Spec
#### Поля и семантика
- `spec.ownerModule` — идентификатор владельца DSC; immutable; используется для контроля прав и аудита.
- `spec.snapshotResourceMapping[]` — декларация поддерживаемых snapshot‑типов и правила оркестрации.
  Для каждого маппинга задаются ресурс (resource), snapshot и content‑тип, а также `priority`.
  Этот список является **входным контрактом** для registry/watch/RBAC.
- `spec.manifestTransformation` — **единственный механизм** трансформации манифестов для snapshot‑типов данного DSC.

#### Пример
```yaml
apiVersion: state-snapshotter.deckhouse.io/v1alpha1
kind: DomainSpecificSnapshotController
metadata:
  name: virtualization
spec:
  ownerModule: virtualization
  snapshotResourceMapping:
    - resourceCRDName: virtualmachines.virtualization.deckhouse.io
      snapshotCRDName: virtualmachinesnapshots.virtualization.deckhouse.io
      contentCRDName: virtualmachinesnapshotcontents.virtualization.deckhouse.io
      priority: 0
    - resourceCRDName: virtualdisks.virtualization.deckhouse.io
      snapshotCRDName: virtualdisksnapshots.virtualization.deckhouse.io
      contentCRDName: virtualdisksnapshotcontents.virtualization.deckhouse.io
      priority: 1
  manifestTransformation:
    serviceRef:
      name: virtualization-snapshot-webhook
      namespace: d8-virtualization
```

**Формат v1alpha1:** используется **только** представление через **три CRD‑имени** на элемент маппинга: `resourceCRDName`, `snapshotCRDName`, `contentCRDName`. Иные представления (GVK‑поля и т.п.) **сознательно отложены** до следующих версий API — не входят в v1alpha1 (меньше ветвлений валидации, defaulting и совместимости).

#### Правила валидации spec
- Каждый элемент `snapshotResourceMapping[]` обязан задавать тройку **CRD‑имён** (см. выше).
- Контроллер выполняет discovery и вычисляет GVK/GVR и **scope** для каждого типа.
- `manifestTransformation` опционален; иных механизмов трансформации нет.

#### Модель доверия и владение
DSC — platform‑level ресурс. Создавать и изменять его могут только
сервис‑аккаунты модулей платформы (или cluster‑admin).
Чтение и интерпретация DSC разрешены только platform‑level компонентам Deckhouse;
использование пользовательскими инструментами/CLI/сторонними расширениями не поддерживается.

`ownerModule` — идентификатор владельца DSC и не влияет на поведение:
- обязателен в v1alpha1 и immutable;
- используется для контроля прав и аудита;
- не используется в логике приоритетов, RBAC или создании snapshot’ов.

Правило владения: разрешённые SA/группы (например, `system:serviceaccounts:d8-<module>`)
маппятся на `ownerModule`. Несоответствие → reject.

Enforcement обеспечивается RBAC + admission; в v1alpha1 используется **ValidatingWebhookConfiguration**.

**ValidatingWebhook:** только **reject** невалидных изменений `spec` / нарушений владения; **не пишет** `status` DSC. При недоступности webhook поведение — стандартное для кластера (fail‑open vs fail‑closed задаётся политикой apiserver/webhook).

#### Platform‑level consistency (cross‑DSC)
- Платформа допускает несколько DSC при отсутствии конфликтов типов.
- Kind snapshot‑типа глобально уникален по платформе.
- Конфликт по `(group, kind)` в разных DSC → `Accepted=False (KindConflict)`;
  конфликтующие DSC полностью исключаются из registry/RBAC/watch (частичное принятие не допускается).
  DSC либо полностью принят, либо полностью отклонён; partial registration запрещён.

### API: Status
#### Schema
```yaml
status:
  conditions:
    - type: Ready # Общий кондишин Ready
      status: "True" | "False"
      reason: <string>
      message: <string>
      observedGeneration: <int>
    - type: Accepted
      status: "True" | "False"
      reason: <string>    # например KindConflict, InvalidSpec
      message: <string>
      observedGeneration: <int>
    - type: RBACReady
      status: "True" | "False"
      reason: <string>
      message: <string>
      observedGeneration: <int>
```
#### Spec vs Status
- `spec.snapshotResourceMapping[]` — входное намерение.
- `status.conditions` — результат проверки и готовности регистрации.

**Для runtime activation** учитываются **только** `Accepted` и `RBACReady`: оба `True` и у **каждого** `observedGeneration == metadata.generation`. Устаревшее поколение у любого из них ⇒ watch **не** активируется.

Производный **`Ready`** при пересчёте записывается с **`observedGeneration = metadata.generation`**; он **не** является дополнительным предикатом активации (см. *Conditions*).

#### Conditions (definitions)
- `Accepted` — результат валидации registry/reconcile DSC. Пишет **только** контроллер DSC / registry‑подсистема (не webhook).
- `RBACReady` — факт применения RBAC. Пишет **только** Deckhouse‑hook (write‑only для остальных компонентов); общий snapshot‑контроллер поле не меняет. Отсутствие condition трактуется как «RBAC ещё не применён».
- `Ready` — **производный** condition: **не** является точкой принятия решений и **не вводит** семантики сверх факта «`Accepted` и `RBACReady` валидны и актуальны по поколению». Назначение — **агрегированное отображение** для операторов и platform‑level компонентов.  
  `Ready=True` **iff** `Accepted=True`, `RBACReady=True`, и у **этих двух** conditions `observedGeneration == metadata.generation`; при записи `Ready=True` контроллер выставляет у **`Ready`** `observedGeneration = metadata.generation` (определение **без цикличности**: предикат опирается только на `Accepted`/`RBACReady`).  
  **Активация watch** использует **ту же** формулу, что и строка выше, **без чтения** `Ready` как входа; `Ready` лишь дублирует итог в status.  
  Если агрегат ещё не пересчитан после изменения spec/status, контроллер выставляет `Ready=False` с reason **`StaleStatus`**, чтобы не вводить в заблуждение.

**Пересчёт `Ready`:** любое изменение `spec` или `status.conditions`, влияющее на `Accepted` или `RBACReady` (в т.ч. запись hook’ом `RBACReady=True`), должно приводить к **повторному reconcile** DSC и **пересчёту** агрегированного `Ready` контроллером registry (watch на объект DSC целиком обеспечивает это).

#### Ownership записи status (единая модель)
| Condition / поле | Кто пишет |
|------------------|-----------|
| `Accepted`, агрегированный `Ready` | Контроллер DSC / registry |
| `RBACReady` | Deckhouse‑hook |
| `spec` | Модули платформы (по RBAC); webhook лишь отклоняет запрос |

#### Standardized reasons and priority rules (v1alpha1)

`Accepted=False` — **только** ошибки spec и кросс‑DSC согласованности (**не** использовать для «устаревшего поколения» — это не причина у `Accepted`):
- KindConflict
- InvalidSpec (в т.ч. snapshot content не **cluster**‑scoped — см. инвариант ниже)

`Ready=False` — производный condition; **единственный** standardized reason для «отставания пересчёта агрегата»:
- StaleStatus (у `Ready` устарело `observedGeneration` относительно текущего `metadata.generation`, пока контроллер не пересчитал)

Когда `Accepted=False` или `RBACReady=False` (или неактуально поколение у них), производный **`Ready`** тоже `False`; **детали** — в conditions `Accepted` / `RBACReady`, без дублирования их reason’ов как отдельных standardized значений под `Accepted`.

RBACReady=False:
- Pending
- ApplyFailed

`RBACReady=False (Pending)` выставляется Deckhouse‑hook’ом после `Accepted=True`
и сохраняется, пока hook не подтвердит применение RBAC. Отсутствие `RBACReady` или `False`
трактуется как «RBAC не готов».

### Поведение registry‑подсистемы
- Следит за `DomainSpecificSnapshotController`.
- На add/update:
  - Разрешает GVK из `spec.snapshotResourceMapping` через discovery.
  - Если тип **ранее** был разрешён, а **теперь** не резолвится через discovery (CRD/API исчезли или сменились), registry переводит соответствующий тип в **degraded / inactive**: не активирует для него новые reconcile/watch в рамках текущего steady-state; согласовано с политикой **исчезновения CRD** (§3).
  - Обновляет **registry state** (внутренний реестр типов).
  - **Runtime activation:** только формула из *Registry state vs runtime* (`Accepted` + `RBACReady` + их актуальные поколения); **`Ready` не читается** как вход решения.
  - При `RBACReady=False (Pending)` watch не запускаются (даже если `Accepted=True`) до подтверждения hook’ом.
- На delete:
  - Удаляет запись из registry state. Остановка watch — **best‑effort** и может потребоваться **рестарт** pod для полной гарантии.
  - RBAC пересчитывается Deckhouse.

#### Алгоритм reconcile DSC (псевдокод)
```
desiredMappings = spec.snapshotResourceMapping
for each mapping in desiredMappings:
  validate mapping format (только CRD‑имена, v1alpha1)
  ensure snapshot/content CRD exist
  discover/resolve GVK + GVR + scope для snapshot/content
  check content.scope == Cluster (иначе InvalidSpec)  # контракт API v1alpha1, см. §2
  check Kind conflicts across all DSC (при конфликте -> Accepted=False)
if any validation failed:
  set conditions Accepted=False (по причине), Ready=False
  set observedGeneration для Accepted и Ready (= metadata.generation)
else:
  set Accepted=True; Accepted.observedGeneration = metadata.generation
  recompute Ready: True iff RBACReady=True и RBACReady.observedGeneration == metadata.generation
  Ready.observedGeneration = metadata.generation  # при каждой записи производного condition
```

---

## 2. RBAC‑обновления (Deckhouse‑managed)

### Инвариант: scope snapshot content (контракт API)

В **v1alpha1** поддерживаются только типы **`*SnapshotContent` с cluster scope**. Тип content с **namespace** scope **не** поддерживается → `Accepted=False (InvalidSpec)`. Это часть **API‑контракта** unified‑снимков, а не только деталь reconcile; RBAC и генератор правил исходят из cluster‑scoped content.

### Требуемый RBAC для общего контроллера
Для каждого зарегистрированного типа снимка:
- `get/list/watch` на `snapshots` и `snapshotcontents`
- `update/patch` только на `snapshots/status` (если общий контроллер пишет conditions)
- CRUD на артефакты общего контроллера (SnapshotContent, ManifestCheckpoint, ObjectKeeper и т.п.)
> Примечание: доступ к `/manifests*` subresources нужен пользователям/CLI, а не контроллеру.

Ограничение:
- общий контроллер **не должен** иметь `create/update` на доменные `Snapshot` (только `status`).
- общий контроллер **не должен** иметь прав на изменение пользовательских ресурсов,
  даже если они участвуют в snapshot‑флоу (например, `VirtualMachine`).

### Предлагаемый механизм
1. **Deckhouse‑hook** (или контроллер модуля) читает все `DomainSpecificSnapshotController`.
2. Генерирует/обновляет **один ClusterRole**, привязанный к ServiceAccount общего контроллера:
   - Добавляет правила для каждого `snapshotResourceMapping[]` из DSC.
   - Держит правила минимальными и детерминированными.
3. Роли **реконсайлятся при изменении**, а не поддерживаются вручную доменными модулями.
RBAC пересчитывается при любом изменении `spec`
в любом `DomainSpecificSnapshotController`.
> Важно: общий snapshot‑контроллер **не применяет RBAC сам** и должен
> корректно переживать промежуток между регистрацией типа и фактическим применением RBAC.
> После успешного применения RBAC Deckhouse‑hook выставляет
> `RBACReady=True` в соответствующем DSC. При неуспехе — `RBACReady=False (ApplyFailed)`.

Это делает RBAC обновляемым, проверяемым и согласованным с регистрацией.
Использование одного ClusterRole упрощает аудит прав, снижает риск рассинхронизации ролей
и соответствует модели Deckhouse‑managed reconciliation.

> Генератор RBAC использует только DSC со статусами `Accepted=True`
> и актуальными `observedGeneration` в conditions.

### RBACReady handshake
- После `Accepted=True` состояние `RBACReady` остаётся `Pending`
  до фактического применения RBAC.
- После успешного apply hook выставляет `RBACReady=True`.
- При ошибке apply hook выставляет `RBACReady=False (ApplyFailed)`.

### Детерминированная генерация RBAC
Для каждого `snapshotResourceMapping[]`:
- snapshot GVR: `get`, `list`, `watch`
- snapshot `/status`: `patch`, `update` (если общий контроллер пишет status)
- content GVR: `get`, `list`, `watch`

Пример ClusterRole (фрагмент):
```yaml
rules:
  - apiGroups: ["virtualization.deckhouse.io"]
    resources: ["virtualmachinesnapshots"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["virtualization.deckhouse.io"]
    resources: ["virtualmachinesnapshots/status"]
    verbs: ["patch", "update"]
  - apiGroups: ["virtualization.deckhouse.io"]
    resources: ["virtualmachinesnapshotcontents"]
    verbs: ["get", "list", "watch"]
```

### Platform contracts
- `*CaptureRequest/*RestoreRequest` — internal ресурсы, создаются только соответствующими контроллерами.
- Пользовательский RBAC на эти ресурсы не выдаётся.
- Если кто‑то вручную выдал RBAC на internal ресурсы,
  это считается нарушением модели безопасности.
  Admission/контроллеры могут дополнительно защищать инварианты
  (например, запрещать операции над internal ресурсами).

## 3. Открытые вопросы и зафиксированные решения

### Конфликт Kind между DSC (runtime)
- **Решение:** оба конфликтующих DSC получают `Accepted=False (KindConflict)`, исключаются из registry/watch/RBAC; **процесс общего snapshot‑контроллера не завершается** из‑за этого. Лог / событие / метрика обязательны.
- **Panic** не используется как реакция на конфигурационную ошибку платформы. Panic допустим только для **внутренне невозможного** состояния кода (инвариант нарушен после успешной валидации), не для пользовательского/модульного неверного DSC.

### Исчезновение CRD после регистрации
**Решение (operational policy):** **fail‑open / degraded** — согласовано с философией «не падать из‑за отсутствующих API» и fail‑closed на уровне **конкретного типа/DSC**, а не всего процесса.

- Тип помечается **неактивным/невалидным** в registry; **новые** reconcile по нему не запускаются.
- **Процесс** общего контроллера **продолжает** работу; **остальные** зарегистрированные типы не блокируются из‑за одного пропавшего CRD.
- **Stale** watch могут сохраняться до **рестарта** pod — это **operational consequence**, а не отдельная «модель отказа» как вариант B.

### Прочее
- Допустимы ли multi‑version snapshot’ы, если Kind различается? (открыто)
