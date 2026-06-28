# План: SDK для интеграции доменных команд со snapshot-контроллером

> Статус: **дизайн-документ / архитектурный аудит** перед реализацией SDK. Не нормативный.
> Предыдущие планы (`unified_snapshot_flows` и его волны C1–C9) — **завершены и больше не
> активны**; этот документ самодостаточен и от них не зависит.
>
> **Скоуп зафиксирован: SDK v1 = единый namespace, но capture-only implementation scope.**
> Командам показываем **одну** целевую картинку SDK (`pkg/snapshotsdk`), поэтому уже в v1
> агрегируем namespace: существующий restore-`Transformer` **механически переносим/ре-экспортируем**
> в `pkg/snapshotsdk/transform` (только абстрактный интерфейс + DTO, без развития restore).
> Это **не расширяет** implementation-скоуп v1 на restore. Вне v1: новая restore-материализация,
> aggregated apiserver framework, VRR/adopt, изменение restore-поведения, каркас-framework.
> Причина capture-only реализации — сознательный выбор **чистого первого SDK** (стабильный
> контракт), а не «moving target».

## 0. Что доменный контроллер делает: три planning legs + reconcile flow

Это **взгляд потребителя** (что должна делать команда, интегрирующая свой домен), а не внутреннее
устройство SDK. Доменный контроллер реконсайлит **свой** snapshot-CR (например `XxxSnapshot`) и на
каждом шаге выражает **намерение протокола** через SDK; всю wire-механику (conditions, merge-patch,
optimistic-lock, owner-graph) SDK делает за него (facade, §1 / Р15).

> Практическое «как этим пользоваться» (шаги, скелет reconcile, разбор `ChildSpec`/`Target`) вынесено в
> отдельный гайд: [`domain-sdk-usage-guide.md`](./domain-sdk-usage-guide.md).

### 0.1 Ментальная модель: capture узла = три независимые planning legs

> **From the domain controller point of view, capture of one snapshot node consists of three
> independent planning legs:**
> 1. **Manifest leg — always present.** Domain asks SDK to ensure **MCR** for this node.
> 2. **Data leg — optional, occupancy 0..1.** If the node has data, domain asks SDK to ensure
>    **exactly one VCR**. If the node is manifest-only, this leg is empty.
> 3. **Children leg — optional, 0..N.** Domain discovers children **privately** and publishes the
>    **complete desired child set** through SDK.
>
> Planning is complete only after all **applicable** legs are published. Then domain marks the node
> as **planning-ready**.

Это ровно **canonical graph model** (§1, §6): `node = manifest + one logical data slot + children[]`.
Три линии планирования — это три ортогональные части узла. Таблица в §0.3 — не «8 случайных шагов», а
**реализация этих трёх линий** + вход/барьер.

> **Ключевой инвариант ответственности:** `SDK does not own domain decisions. SDK owns publication of
> three protocol legs: manifest, data slot, children.`
> - `MCR` — это **manifest leg**; `VCR` — это **data leg**; `EnsureChildren` — это **children leg**;
> - `MarkPlanningReady` — **барьер после публикации всех применимых legs**.

### 0.2 Lifecycle узла: Ensure* (do planning) → Mark* (commit outcome)

Доменный контроллер планирует три leg'а, затем **обязан** завершить planning phase одним из **двух
terminal outcomes**:

```text
// sdk — инстанс snapshotsdk.CaptureSDK, инъектированный в reconciler (true facade, Р26)
1. Manifest leg → sdk.EnsureManifestCapture(...)   // ensure MCR
2. Data leg(0..1) → sdk.EnsureVolumeCapture(...)   // ensure exactly one VCR (если есть данные)
3. Children leg(0..N) → sdk.EnsureChildren(...)    // publish complete desired child set
                         ↓
   terminal: sdk.MarkPlanningReady(...)  ИЛИ  sdk.MarkPlanningFailed(...)
```

**Semantic contract `Mark*` (важно прописать явно, иначе у команды ложное ожидание):**
`MarkPlanningReady` **НЕ делает planning**. Он только утверждает:
*«I, domain controller, assert that all required planning for this node is complete and published.»*
Конкретно `MarkPlanningReady`:
- **не** создаёт MCR; **не** создаёт VCR; **не** пишет/пересчитывает children refs;
- только: (1) опционально fail-fast валидирует, что minimum invariants соблюдены;
  (2) ставит internal condition (`ChildrenSnapshotReady=True`, legacy-имя); (3) публикует planning barrier.

Чистое разделение (mental model):
- **`Ensure*` = perform planning work** (меняет protocol-state: refs / MCR / VCR);
- **`Mark*` = commit planning outcome** (только фиксирует исход, ничего не планирует).

**Planning outcome бинарен** с точки зрения SDK-протокола: `Ready` → planning completed successfully;
`Failed` → planning terminated with error. Других терминальных исходов у planning phase нет.

**Planning phase crash/restart-safe (инвариант, Р19/Р25):** flow — это **restart-safe recipe, НЕ linear
workflow**. Контроллер может перезапуститься между любыми `Ensure*`/`Mark*` вызовами; после рестарта он
пересчитывает desired и выполняет тот же recipe **сверху** заново. Каждый `Ensure*` — самостоятельный
idempotent checkpoint, сходящийся из **durable Kubernetes state** (никакой зависимости от in-memory
прогресса); до `MarkPlanningReady` published legs реконсилируются к **новому** observed desired (Р25).

> **NB про legacy-имя:** после этого core-condition `ChildrenSnapshotReady` выглядит ещё более
> misleading — фактически он означает уже не «children ready», а **«node planning completed»**.
> Поэтому rename в `MarkPlanningReady` — архитектурно корректный, не косметический; SDK прячет старое
> имя за точным intent (Р17).

### 0.3 Поток одного reconcile доменного snapshot-CR (capture)

**Доменная роль (что остаётся командой) vs роль SDK (что берёт на себя):**
- **домен решает:** что является source'ом снапшота; какие у узла дети; какие PVC/тома попадают в
  data-leg; имена/kind'ы домена; когда узел вообще нужно снапшотить.
- **SDK берёт:** создание `MCR`/`VCR`, построение/сортировку `childrenSnapshotRefs`, запись co-owned
  status/conditions по D4a, TOCTOU-гард, подавление пересоздания, ownerRef-граф, content-free инвариант.

Таблица ниже — реализация трёх legs (§0.1): шаги 4/5/6 = children/data/manifest leg, шаг 7 = барьер.

| # | Момент (когда) | Что делает доменный контроллер | Зачем | SDK intent-API (методы `CaptureSDK`, Р16) |
|---|---|---|---|---|
| 1 | вход в `Reconcile`, получен snapshot-CR | строит `SnapshotAdapter` поверх своего CR (одноразовый field-mapping seam) | дать SDK читать/писать контракт-поля, не зная доменной раскладки | `snapshotsdk.SnapshotAdapter` (root, Р10) |
| 2 | сразу после входа | резолвит и **проверяет source** своим `SourceValidator` (identity — доменная, Р22/Р28) | не планировать снапшот невалидного/чужого объекта | домен сам зовёт `validator.Validate(...)`; при ошибке — `MarkNotReady(...)` (Р28/Р30) |
| 3 | — (нет публичного шага) | ничего; **просто вызывает `Ensure*`** | TOCTOU-гард (не пересоздавать `MCR`/`VCR`) — **internal** механика SDK, не public API (Р20) | _(internal; `AlreadyCaptured` убран из facade)_ |
| 4 | узел имеет детей (parent, напр. VM) | отдаёт SDK **полный desired-set детей** одним вызовом (как собрал — приватно, Р17) | create/adopt дочерних snapshot-CR + публикация `childrenSnapshotRefs` (**delete-free**, Р23) | `EnsureChildren(ctx, adapter, desired)` (leaf: nil/empty = «детей нет», публикует пустой список; при сбое discovery — `MarkPlanningFailed`, Р23) |
| 5 | узел имеет data-leg (leaf, напр. Disk) | просит обеспечить **VCR** для своих томов | запустить захват данных тома (через backend-порт), occupancy слота = 1 | `sdk.EnsureVolumeCapture(...)` |
| 6 | всегда (у любого узла есть manifest) | отдаёт SDK **полный desired-set manifest targets** | создать один MCR для manifest targets узла; technical owned-PVC target добавляется из VCR при наличии data-leg | `sdk.EnsureManifestCapture(...)` |
| 7 | планирование узла завершено (refs + MCR/VCR published) | ставит **барьер планирования** (НЕ меняет список детей, Р17) | сигнал, что узел корректно спланирован; published-набор становится authoritative | `sdk.MarkPlanningReady(ctx, t)` |
| 7' | любой `Ensure*` на шагах 4–6 вернул ошибку | помечает **planning failed** с причиной | дать наблюдаемую причину, не «зависнуть» молча | `sdk.MarkPlanningFailed(ctx, t, cause)` |
| 8 | маркеры `manifestCaptured`/`dataCaptured` выставил core | ничего не пересоздаёт; reconcile идемпотентен | завершить capture без дублей; узел в steady state | (шаги 4–6 становятся no-op) |

**Ключевые правила, которые SDK гарантирует за домен (и которые команда НЕ должна писать сама):**
- доменный snapshot-реконсайлер **никогда не читает/пишет `SnapshotContent`** (content-free);
- запись co-owned полей — **только** optimistic-lock merge patch (D4a), без full-replace;
- `manifestCaptured`/`dataCaptured`/`boundSnapshotContentName` пишет **core**, домен их только читает;
- глубина дерева — доменная (рекурсия по узлам); SDK работает локально на любом узле: **ровно один
  logical data slot, occupancy 0..1**.

> Этот раздел — карта usage. Точные сигнатуры — §5.3; почему именно так — §3/§4/§4a/§4b (развилки Р1–Р28).
> **NB (нейминг):** MCR/VCR/children/capture — **язык домена**, его НЕ прячем (это internal platform
> SDK для инженерных команд, §1). Прячем только transport-механику (conditions/patch/retry/status-layout).
> **NB (форма):** методы вызываются на **`CaptureSDK`** — documented interface-facade (Р16, РЕШЕНО);
> в таблице записаны как `capture.X(...)` для краткости. Session/`Planner` не вводим.

## 1. Зачем это нужно (мотивация)

Сейчас единственная интеграция домена со state-snapshotter — это **демо-контроллер**
(`images/domain-controller`, модуль `demo`). Он одновременно:
- реализует демо-домен (VirtualDisk / VirtualMachine + их снапшоты), и
- «вручную» реализует **протокол общения со snapshot-контроллером**: создание
  `ManifestCaptureRequest` (MCR) и `VolumeCaptureRequest` (VCR), построение списка детей
  (`childrenSnapshotRefs`), запись status-полей и условий по контракту D4a, ownerRef-граф,
  TOCTOU-гард на устаревший кэш и т.д.

SDK проектируется как **domain-agnostic integration layer** между произвольными доменными
контроллерами и state-snapshotter. Без SDK каждая команда перепишет один и тот же низкоуровневый
протокол — с риском разойтись в тонких инвариантах:
- **content-free snapshot-реконсайлеры** (домен НЕ читает и НЕ пишет `SnapshotContent`);
- запись co-owned status-полей только через **optimistic-lock merge patch** (D4a), без full-replace;
- подавление пересоздания запросов по доменным маркерам (`manifestCaptured`/`dataCaptured`);
- детерминированные/отсортированные/дедуплицированные `childrenSnapshotRefs`;
- TOCTOU-гард: живое чтение маркеров перед планированием;
- модель графа: у узла **ровно один logical data slot** (occupancy 0..1, Вариант A).

> **Модель data slot (архитектурный инвариант, не просто ограничение реализации):**
> у каждого snapshot-узла существует **ровно один logical data slot** (cardinality = 1).
> Слот может быть:
> - **empty** — узел без data-leg, manifest-only (например VM): occupancy = 0;
> - **bound** — слот связан ровно с одним data-артефактом (например `VolumeSnapshotContent`): occupancy = 1.
>
> Формально: `cardinality(data_slot) = 1`, `occupancy(data_slot) ∈ {0,1}`.
> Несколько независимых линий данных на одном узле в SDK v1 **не поддерживаются** — такой случай
> (гипотетический multi-disk VM) декомпозируется в дочерние узлы (children), а не в один узел.
>
> Заметка по терминологии: канон — абстракция **data-leg slot**, а НЕ имя поля. Точное имя поля
> (`dataRef`/иное) живёт в `spec/` и может эволюционировать; SDK на него имя-в-имя не завязываем.

**Цель v1:** вынести из демо-домена **только повторяемые capture-инварианты** в SDK,
оставив доменной команде доменную бизнес-логику (какие дети, какие PVC-таргеты, какой source).
Демо-контроллер становится **референс-потребителем** SDK; его поведение не меняется (no-op refactor).

> **SDK neutrality principle (нормативно для этого SDK).**
> `snapshotsdk` MUST remain domain-agnostic. SDK НЕ кодирует предположений о:
> - конкретных именах доменных CRD;
> - соглашениях именования доменных полей / раскладке status;
> - глубине доменной иерархии;
> - порядке rollout'а конкретных адоптеров SDK.
>
> Конкретные потребители намеренно трактуются как **implementation detail, а не как
> архитектурный вход**. `demo` **не является привилегированным потребителем SDK-дизайна**; это
> **reference implementation / conformance fixture внутри репо**. **Привилегированный внешний
> потребитель недопустим**: downstream-домены — равноправные адоптеры без особого статуса в дизайне.

> **Кто потребитель: internal platform SDK, НЕ external consumer SDK.** Это SDK для **инженерных
> команд**, работающих в том же домене и **обязанных понимать snapshot-термины** (child snapshots,
> MCR, VCR, capture state, ready-барьеры). Цель API — **не спрятать всё**, а найти **правильный
> abstraction level**.

> **Центральный принцип: `SDK hides transport mechanics, not domain concepts`.**
> Три уровня абстракции:
> 1. **Слишком низко (плохо)** — команда видит wire-format и Kubernetes-механику:
>    `PatchCondition("ChildrenSnapshotReady", metav1.ConditionTrue)`, `PatchStatus(...)` — API leaking
>    implementation details.
> 2. **Слишком высоко (тоже плохо)** — black box, команда не понимает, что происходит:
>    `planner.Process(ctx)` / `planner.Advance()` — магия.
> 3. **Правильный уровень (target)** — команда видит **domain protocol concepts**, но НЕ serialization details.
>
> **Выставляем наружу (это язык домена, прятать НЕЛЬЗЯ):** child snapshots, **MCR**, **VCR**,
> capture state, ready-барьеры, source-валидность.
> **Прячем внутрь (transport/persistence mechanics):** `metav1.Condition` и точные condition-type
> строки, merge-patch strategy, `RetryOnConflict`/optimistic-lock, раскладка `status.*`
> (`childrenSnapshotRefs` и т.п.), ownerRef-quirks.
>
> Так абстракция не уезжает ни в «utility поверх k8s», ни в «framework black box»; структура conditions /
> co-owned полей / merge-strategy / даже storage backend могут меняться, не ломая доменные контроллеры.
>
> **Следствие для дизайна API (нейминг — на языке reconcile-intent, см. Р15):** публичные глаголы —
> `Ensure*` / `Mark*` / `Validate*` (idempotent reconcile-step / domain-transition / проверка), а НЕ
> `Set*`/`Patch*` (это field-mutation/persistence). Низкоуровневые `D4a-patch`/`optimistic-lock`/
> `condition-merge` допустимы, но это **внутренний слой**, а не «лицо» SDK.

## 2. Поверхность интеграции (что вообще есть) и что попадает в v1

Полная карта протокола ниже; **жирным** — то, что входит в **v1 (capture-only)**.

### 2.1 Регистрация домена (декларативно) — НЕ код SDK
- `CustomSnapshotDefinition` (CSD): `spec.snapshotResourceMapping[]` (`templates/domain-controller/demo-csd.yaml`).
- Хук `030-domain-rbac`, APIService, Deployment/SA/RBAC.
- В SDK v1 не входит (это helm/инфра), но попадёт в гайд для команд (S5/future).

### 2.2 Capture-планирование (SNAPSHOT-реконсайлеры, content-free) — **ЯДРО v1**
- **Валидация `spec.sourceRef`** (immutable, единый источник истины) — `source_identity.go`.
- **Создание per-snapshot MCR** (targets, ownerRef, детерминированное имя) — `materialization.go`, `manifest_targets.go`.
- **Создание data-leg VCR** на PVC через unstructured+GVK — `disk_volume_capture.go`,
  `pkg/volumecapture`, `internal/controllers/volumecapture/unstructured.go`.
- **Деривация manifest-targets из VCR** (не из `SnapshotContent`) — чтобы остаться content-free.
- **Построение списка детей**: ensure дочерних снапшот-CR + публикация `status.childrenSnapshotRefs`.
- **Барьер `ChildrenSnapshotReady`** (`api/storage/v1alpha1/conditions.go`) — SDK прячет это
  legacy-имя за intent `MarkPlanningReady` (Р17).
- **Подавление пересоздания** по маркерам `status.manifestCaptured`/`status.dataCaptured`.
- **TOCTOU-гард**: живое чтение маркеров через `APIReader` (`disk_controller.go`, `steady_state.go`).

### 2.3 Контракт status / условий (co-write с core, D4a) — **ЯДРО v1**
- Поля домена: `manifestCaptureRequestName`, `volumeCaptureRequestName`, `childrenSnapshotRefs`,
  условие `ChildrenSnapshotReady`, ранний `Ready=False` (валидация).
- Поля core (домен только читает): `boundSnapshotContentName`, `manifestCaptured`/`dataCaptured`, `Ready`.
- Запись — **optimistic-lock merge patch** под `RetryOnConflict`, read-modify-write только своих полей.

### 2.4 OwnerRef-граф и хелперы — **v1**
- `demoSnapshotOwnerReference`, `ensureDemoSnapshotOwnerRef` (запрет двойного controller-owner),
  `OwnerReferencesEqual`, `SnapshotChildRefsEqualIgnoreOrder`, `SortSnapshotChildRefs`.

### 2.5 Restore-материализация (RESOURCE-реконсайлеры) — **FUTURE (не v1)**
- `virtualdisk_restore.go`: dataRef-резолв, VRR build/ensure/delete, adopt PVC.

### 2.6 Restore-компиляция (доменный aggregated apiserver) — **FUTURE (не v1)**
- `domainsdk.Transformer`, `internal/domainapi`, `internal/usecase/restore`.

### 2.7 Что остаётся ЧИСТО доменным (никогда не в SDK)
- Конкретные CRD-типы и имена полей домена; решение «какие дети» и «что материализовать»;
  доменная restore-трансформация; содержимое CSD/helm.

## 3. Форма SDK v1 (единый namespace, capture-only реализация)

Идея: **library-first** — набор хелперов поверх `controller-runtime`. Domain-специфика подаётся
через узкие accessor-интерфейсы и коллбэки. Каркас-framework НЕ вводим в v1 (см. Р1).

Раскладка пакетов v1 (после арх-аудита ревью 3 и **ревью 4b: true facade, Р26**). SDK — **отдельный
shared-модуль** в корне репо `pkg/snapshotsdk` (Р2; модуль `…/pkg/snapshotsdk`, свой `go.mod`, подключается
потребителями через `replace`). Граница проведена по принципу **«публичный пакет — только если внешний
потребитель должен о нём рассуждать»** и усилена решением Р26 (true facade, а не layered library):

```
pkg/snapshotsdk/                   # КАНОНИЧЕСКИЙ root-модуль (Р2): весь type+protocol API виден здесь (litmus, §1)
  go.mod                           #   module github.com/deckhouse/state-snapshotter/pkg/snapshotsdk; replace …/api => ../../api
  *.go                             #   CaptureSDK (+ узкие роли/targets), SnapshotAdapter, SourceValidator,
                                   #   ChildSpec, ManifestCaptureSpec, VolumeCaptureSpec, VolumeCaptureProvider,
                                   #   SDK-owned Condition/ChildRef. Весь capture-lifecycle (source-identity,
                                   #   TOCTOU live-refresh, marker-suppression, MCR/VCR orchestration) — здесь.
  transform/       # ЕДИНСТВЕННЫЙ публичный подпакет: restore extension point (Transformer + DTO),
                   # механически перенесён из domainsdk; НИКАКОЙ новой restore-работы в v1
  internal/                        # low-level mechanics (Р15.6), НЕ публичный SDK surface
    children/        # ensure + build/sort/dedup/compare childrenSnapshotRefs, delete-free (transport mechanics)
    status/          # D4a merge-patch + optimistic-lock + condition-merge (бывш. public-advanced → internal, Р26)
    patch/           # D4a merge-patch + optimistic-lock + RetryOnConflict
    conditions/      # SDK Condition <-> metav1.Condition, condition-merge, type-strings
    ownerref/        # ownerRef-граф, запрет двойного controller-owner (зовётся из children/capture)
    storagefoundation/ # реализация VolumeCaptureProvider поверх storage-foundation VCR (unstructured+GVK)
```
Потребители: `images/domain-controller` (demo, сейчас) и др. команды (позже) — `require …/pkg/snapshotsdk`
+ `replace …/pkg/snapshotsdk => ../../pkg/snapshotsdk`.

**Уровни (Р15.6 + Р26):** публичны только **root `pkg/snapshotsdk`** (semantic protocol + type API) и
`transform/` (extension point). Всё остальное — `internal/*` (low-level/transport mechanics), домен не
импортирует. Гибрид «facade + куча публичных пакетов» снят: команды видят **одну** поверхность и не могут
bypass-нуть facade в transport-слой.

Ключевые решения аудита (детали — §4/§4b):
- **True facade (Р26)** — root-пакет каноничен; `children/` и `status/` ушли в `internal/` (это transport
  mechanics: sort/dedup/patch/condition-merge, а не доменный contract). Проходит litmus «открыл root — всё видно».
- **`status/` → `internal/`** — «public-advanced, но не трогай» — это smell и нарушение Р15
  (package boundary = semantic boundary). D4a/optimistic-lock/conditions — чисто механические.
- **`children/` → `internal/`** — sort/dedup/compare childrenSnapshotRefs (delete-free) = transport mechanics;
  домен выражает «какие дети» через `CaptureSDK.EnsureChildren`, а не через отдельный пакет.
- **`ownerref` → `internal/`** — implementation detail capture/children, не доменный API.
- **`volumecapture` = порт, а не VCR-схема** — тип `VolumeCaptureProvider` живёт в **root**; знание схемы
  `VolumeCaptureRequest` (storage-foundation) изолировано в `internal/storagefoundation`.
- **`transform/` в v1 — только перенос namespace, не реализация** — единый SDK для команд требует, чтобы
  restore extension point жил под тем же `pkg/snapshotsdk`. Механически переезжает абстрактный `Transformer`
  + DTO (`RestoreNode`/`NodeResult`); новой restore-логики не добавляем.

**Вне v1 (future):** `restore/` (материализация), `apiserver/` (framework), `scaffold/` (framework).

## 4. Развилки — РЕШЕНИЯ (зафиксированы на ревью)

### Р1. Стиль SDK — **РЕШЕНО: library-first, но НЕ low-level utility (facade-слой сверху)**
Каркас-реконсайлеры (framework) — future. В v1 — библиотека, которую домен вызывает из своего
`Reconcile`. **Но «library» здесь ≠ набор generic Kubernetes-хелперов** (см. SDK-as-protocol-facade
principle, §1): публичное «лицо» SDK — **intent-ориентированный facade** (намерения протокола),
а D4a-patch/optimistic-lock/condition-merge — **внутренний слой**, не выставляемый домену напрямую.

### Р2. Расположение/версионирование — **РЕШЕНО: репо-корневой `pkg/snapshotsdk` = отдельный shared-модуль**
**Цель (ревью 5):** SDK надо **вытащить из demo-контроллеров** в общий `pkg/snapshotsdk`, чтобы demo
(и другие команды) импортировали именно его — НЕ оставлять внутри `images/domain-controller`.

**Механика (по факту кода):** репозиторий — multi-module, репо-корневого `go.mod` нет; общий код
подключается как **отдельный модуль + `replace`** (так уже сделан `api`:
`replace github.com/deckhouse/state-snapshotter/api => ../../api`). Поэтому корневой `pkg/snapshotsdk`
**обязан быть собственным модулем**:
- путь модуля: `github.com/deckhouse/state-snapshotter/pkg/snapshotsdk` (свой `pkg/snapshotsdk/go.mod`);
- зависит от модуля `api/` (MCR/ManifestTarget, SnapshotContent/SnapshotChildRef, conditions/reasons) —
  через `require` + `replace … => ../../api`;
- потребители (`images/domain-controller` сейчас; virtualization и др. позже) добавляют
  `require …/pkg/snapshotsdk` + `replace …/pkg/snapshotsdk => ../../pkg/snapshotsdk`.

**Изменение прежней формулировки:** ранее тут стояло «отдельный `sdk/go.mod` — рано». **Отменяется:**
кросс-модульное переиспользование (demo + другие команды в отдельных модулях) **требует** модуль уже в v1.
Отдельный репозиторий — по-прежнему нет; модуль живёт в этом репо под `pkg/snapshotsdk`.

### Р3/Р4. Подача доменного типа — **РЕШЕНО: adapter-first, FULL read+write контракт, embeddable НЕ обязателен**
`SnapshotAdapter` is **intentionally consumer-agnostic**: он маппит произвольные доменные
snapshot-CRD на контракт SDK, не навязывая именование полей или раскладку status. Rationale — НЕ
«потому что такой-то домен», а «разные домены имеют разные CRD-conventions» (см. SDK neutrality
principle, §1). Embedded status НЕ требуется.
**Критично (см. Р10):** адаптер должен **полностью инкапсулировать контракт-поля — и read, и write**.
Тонкий read-only адаптер протёк бы: `status.PatchCondition` тогда лез бы в поля через
reflection/closures (магия). Адаптер владеет и чтением, и записью всех co-owned полей.
Embeddable `SnapshotStatusCommon` предоставляем как **optional helper**, но НЕ навязываем —
обязательный общий embedded-блок стал бы API/политическим блокером для команд со своими conventions.

### Р5. Объём v1 — **РЕШЕНО: capture-only implementation scope** (namespace — единый, см. Р14)
Реализуем только capture. `transform/` входит в v1 как **перенос namespace** существующего
restore-`Transformer` (без новой restore-логики), а не как реализация restore.

### Р6. VCR/VRR-контракт — **РЕШЕНО: unstructured+GVK как канон**
Go-зависимость на storage-foundation НЕ тащим (типобезопасность спеки VCR/VRR приносим в жертву
развязке репозиториев — осознанно).

### Р7. Каркас aggregated apiserver — **FUTURE** (отдельный restore-срез SDK)

### Р8. Координация с другими планами — **РЕШЕНО**
Старые планы (`unified_snapshot_flows`) завершены; этот SDK — **самостоятельный no-op refactor**,
ни от каких активных волн не зависит и ни в какой общий план не встраивается.

### Р9. Conformance — **РЕШЕНО: минимальный набор**
1. demo-домен, переписанный на SDK, проходит существующие e2e (no-op refactor);
2. unit/envtest на ownership D4a-патча (свой writer не затирает чужие поля);
3. content-free guard: snapshot-реконсайлер НЕ читает `SnapshotContent`;
4. childrenSnapshotRefs — детерминированы/отсортированы/дедуплицированы;
5. TOCTOU marker-refresh предотвращает дублирующий MCR/VCR;
6. crash/restart safety (Р19): повтор `Ensure*` после «рестарта» (новый reconcile из durable state) —
   no-op, без дублей MCR/VCR/child refs; `MarkPlanningReady` идемпотентен.
7. delete-free children (Р23): `EnsureChildren` с уменьшенным desired-set перестаёт публиковать ref'ы
   выбывших детей (они отвязываются от графа, реклейм — ownerRef GC), сами объекты НЕ удаляет; пустой set
   публикует пустой список refs; повтор — no-op.

## 4a. Архитектурный аудит (ревью 3) — решения по поверхности SDK

### Р10. SnapshotAdapter — полный read+write контракт (РЕШЕНО; форма уточнена в Р15)
Самое важное замечание: адаптер инкапсулирует **все** co-owned поля (и чтение, и запись), чтобы
`status/` не делал «магию». Это **field-mapping seam**, а не reconcile-time API (facade — §1, Р15).
Исходная цельная форма:
Целевая форма после уточнений Р15/Р21 (см. §5.3):
```go
type SnapshotAdapter interface {
    Object() client.Object                                              // мост к controller-runtime; seam-only (Р15.5)
    SourceRef() SourceRef
    GetDomainCaptureState() DomainCaptureState; SetDomainCaptureState(DomainCaptureState) // domain read+write (Р21)
    CoreCaptureState() CoreCaptureState                                 // core-owned, ТОЛЬКО getter (Р21)
    GetConditions() []Condition; SetConditions([]Condition)             // SDK-owned Condition, НЕ metav1 (Р15.5)
}
```

**Уточнение Р15 (Go-идиомы):** вместо одного «жирнеющего» интерфейса — **capability-split** на
маленькие роли (`ObjectAccessor`, `SourceProvider`, `DomainStateAccessor`, `CoreStateReader`,
`ConditionWriter`); на границе — **SDK-owned** `Condition`/`ConditionType`/`Status`, а не `metav1.Condition`.
**Уточнение Р21 (ownership):** co-owned поля разведены на два DTO — `DomainCaptureState` (домен read+write)
и `CoreCaptureState` (домен ТОЛЬКО читает, сеттера нет → misuse невозможен по типам). `client.Object`
остаётся только в seam — в capture intent-глаголы он не протекает.

### Р11. Граница write-механики vs capture-lifecycle (РЕШЕНО; пути уточнены в Р26)
Write-механика (D4a patch, condition-merge, optimistic-lock) — **внутренний слой** (facade-principle, §1):
её зовёт capture intent API, домен напрямую не вызывает. После Р26 она живёт в **`internal/status`**
(+`internal/patch`/`internal/conditions`), а не в публичном `status/`. Всё, что про **capture-lifecycle**
(`RefreshMarkersLive`/TOCTOU, marker-suppression, MCR/VCR orchestration), живёт в **root** `pkg/snapshotsdk`
за capture intent-глаголами `Ensure*`/`Mark*`. (Source-identity validation — у домена, Р28.) Иначе write-слой
превратится в свалку «всё про status», а домен — в Kubernetes-native код.

### Р12. `ownerref` — НЕ публичный пакет (РЕШЕНО, internal)
ownerRef-граф — implementation detail `capture`/`children`, а не доменный API: внешний потребитель
не вызывает `EnsureOwnerRef` напрямую. Переезжает в `internal/ownerref` (или `children/owner.go`).
Правило: публичный пакет — только если потребитель должен о нём рассуждать.

### Р13. `volumecapture` — порт-абстракция, а не VCR-схема (РЕШЕНО)
`pkg/snapshotsdk` НЕ должен знать схему `VolumeCaptureRequest`. Публичный — интерфейс:
```go
type VolumeCaptureProvider interface {
    Ensure(ctx context.Context, in VolumeCaptureInput) (VolumeCaptureRef, error)
    ParseTargets(obj any) ([]Target, error)
}
```
Реализация поверх storage-foundation VCR (unstructured+GVK) изолирована в `internal/storagefoundation`.
Так появление другого backend (VCRv2/иной CRD) не меняет публичный SDK API.

### Р14. Единый SDK namespace уже в v1 (РЕШЕНО — изменение прежнего решения)
Командам показываем **одну версию SDK**, поэтому namespace агрегируем сразу. Прежнее
«старый `domainsdk` не трогаем» **отменено**. В v1:
- `pkg/snapshotsdk` — **единственный** namespace SDK для доменных команд;
- существующий restore-only `domainsdk.Transformer` **механически переносится/ре-экспортируется**
  в `pkg/snapshotsdk/transform` — только абстрактный интерфейс + DTO, без restore-реализации;
- это **НЕ расширяет** implementation-скоуп v1 (нет новой restore-материализации/apiserver/VRR/adopt);
- **старый `images/domain-controller/pkg/domainsdk` полностью удаляется** после миграции всех
  внутренних импортов. **Compatibility layer / alias / shim НЕ создаём** — это **v0 / initial SDK
  version**, внешнего контракта совместимости ещё нет, тащить хвосты незачем.

**Фактические импортёры — все в репо, внешних нет:**
`images/domain-controller/internal/domainapi/restore.go`,
`images/domain-controller/internal/controllers/demo/restore_transform.go` (+ `_test`). В
`images/state-snapshotter-controller/.../restore/delegate.go` — только **комментарий** (не импорт),
его тоже поправить на новый путь. → миграция импортов + удаление старого пакета, без shim.

NB по модулю: `domainsdk` сейчас в Go-модуле `images/domain-controller`, а `snapshotsdk` — в общем
модуле репо; при переносе `transform` переезжает в общий модуль (модуль `images/domain-controller`
уже зависит от `api/`, так что новый импорт доступен).

**Цель переноса:** (1) единый SDK для команд; (2) заранее подготовить место для будущего restore-SDK;
(3) не держать публичный SDK внутри demo/domain-controller.

> **Ключевой тезис:** `transform/` — часть **SDK namespace**, но НЕ часть **capture implementation
> scope**. Перенос namespace входит в v1; restore-реализация — нет. Это архитектурно чисто.

### Р15. Go API-идиомы: семантика читается из exported API (ревью 4 — уточняет Р10/facade)
**Тезис:** в Go семантика живёт в **package boundary + type system + method names**, а не в
комментариях/design-doc. Если правильный usage понятен только из ADR — API слишком низкоуровневый.
Это переводит facade-принцип (§1) в конкретные идиомы и **уточняет Р10** (жирный single-adapter →
capability-split + SDK-owned типы на границе).

1. **Package boundary = первый semantic boundary.** Домен импортирует **только root** `snapshotsdk`
   (intent), а НЕ подпакеты `status`/`patch`/`condition` + `metav1`. Если наружу торчит low-level — его
   начнут использовать как public API. Поэтому (true facade, Р26) всё low-level — `internal/*`; публичны
   только root + `transform/`. «public-advanced mid-level» как идея отвергнут (Р26): это был leak.
2. **Никаких raw strings / raw `metav1.Condition` на границе.** Плохо:
   `SetCondition("ChildrenSnapshotReady", metav1.ConditionTrue)` / `PatchCondition(..., cond)` —
   caller обязан знать wire-format. Наружу — typed concepts и semantic operations.
3. **Capability-oriented small interfaces** (Go-way) вместо одного жирного `SnapshotAdapter`:
   ```go
   type ConditionWriter interface { SetConditions([]Condition) }
   type DomainStateAccessor interface { GetDomainCaptureState() DomainCaptureState; SetDomainCaptureState(DomainCaptureState) }
   ```
   тогда функция явно объявляет, что ей нужно: `func MarkPlanningReady(obj ConditionWriter)` — семантика читается сразу.
4. **Value objects / iota-enums вместо «голого» `bool`/`string`**, когда у значения есть фиксированный
   домен вариантов и его читаемость важна на границе. (NB: для planning-исхода мы сознательно НЕ
   вводим enum-состояние — terminal outcome выражается **глаголами** `MarkPlanningReady`/
   `MarkPlanningFailed`, Р17; это пример, где «intent-verb» уместнее value-object'а.)
5. **Прятать Kubernetes-типы на границе.** Если public API требует `metav1.Condition` /
   `unstructured.Unstructured` — leakage слишком сильный. Внутри SDK — ок; на границе — **SDK-owned**
   value objects:
   ```go
   type Condition struct { Type ConditionType; Status Status; Reason Reason; Message string }
   ```
   conversion в `metav1.Condition` — внутри. (Прагматика: `client.Object` неизбежен как мост к
   controller-runtime — он остаётся в **adapter-seam** (Р10), но НЕ в reconcile-time глаголах.)
6. **Layered API (после Р26 — две границы, не три).** Public semantic: методы `CaptureSDK` в root
   (`sdk.MarkPlanningReady(...)`, `sdk.EnsureManifestCapture(...)`). Low-level mechanics — **только**
   `internal/*` (`internal/status`, `internal/patch`, `internal/conditions`, `internal/ownerref`,
   `internal/children`). Промежуточного «public-advanced» слоя нет (был leak, Р26).
7. **Глаголы — на языке reconcile-intent, не persistence.** Имя должно отвечать на вопрос
   «о чём думает доменный контроллер» («мне нужен VCR / нужен MCR / есть дети / capture завершён /
   source невалиден»), а не «какое поле я мутирую». Каноны:
   - `Ensure*` — **idempotent reconcile-step** (естественно для controller-runtime): `EnsureChildren`,
     `EnsureVolumeCapture`, `EnsureManifestCapture`;
   - `Mark*` — **domain-transition / барьер / publication исхода**: `MarkPlanningReady`,
     `MarkPlanningFailed`, `MarkNotReady` (общий `Ready=False`-исход, Р30).
   (Source-identity `Validate*` в facade **нет** — Р28: проверку делает домен своим `SourceValidator`.)
   **Анти-паттерн — `Set*`/`Patch*`** в публичном API: в Go `Set*` читается как field-mutation
   («у объекта есть поле Ready»), а не как domain-transition («состояние узла изменилось»). Поэтому
   `MarkPlanningReady`, а НЕ `SetPlanningReady`; `Set*`/`Patch*` живут только во внутреннем слое.

> **Litmus-тест SDK:** может ли новая команда, **не читая ADR**, открыть `pkg/snapshotsdk` и интуитивно
> понять usage? Если нет — abstraction boundary недостаточно сильная. Этот тест — приёмочный критерий
> формы публичного API на S1.

### Р16-rationale. Почему форма public API важна (мотивация, НЕ выбор интерфейса)
Этот блок фиксирует **почему вопрос формы API вообще важен** (вход для решения Р16), а не навязывает
конкретный interface/service/free-functions — из этой мотивации потом обосновывается выбор.

Capture SDK — это **не набор утилитарных helper-функций**, а **семантический протокол** взаимодействия
между доменным snapshot-контроллером и state-snapshotter. Доменный контроллер проходит фиксированный
**lifecycle capture-планирования** (manifest leg → optional data leg → optional children leg → terminal
planning outcome, §0), поэтому публичная поверхность SDK должна **отражать именно этот протокол**.

**Ключевое требование:** семантика usage должна читаться из public API **без обращения к ADR или
внутренней реализации** (Litmus, Р15). Новая доменная команда, открыв пакет SDK, должна интуитивно понять:
- какие шаги capture-протокола существуют;
- в каком порядке они обычно выполняются;
- какие операции — **planning work** (`Ensure*`), а какие — **terminal outcome** (`Mark*`).

Это работает, потому что SDK намеренно **скрывает transport mechanics** (conditions, optimistic-lock
patching, ownerRef quirks, status layout), но **не скрывает domain concepts** (MCR, VCR, child snapshots,
planning barriers) — §1.

**Отсюда требование к форме API:** public surface должен давать **единый, хорошо документируемый
semantic boundary**, на котором можно зафиксировать: (а) общий lifecycle протокола; (б) invariants
(single logical data slot occupancy 0..1; desired-set semantics для children, Р17); (в) контракт
terminal transitions (planning ready / planning failed); (г) guarantees и ограничения реализации.

**Документируемость:** каждая публичная операция должна иметь явный контракт, но **не дублировать весь
lifecycle** в доке каждой функции. Желательна **единая точка входа** с canonical documentation всего
capture-протокола, а доку отдельных операций ограничить локальными semantic guarantees.

Таким образом, форма public API должна способствовать:
1. **Явности semantic contract** — API сам объясняет usage.
2. **Минимизации cognitive load** — команда не изучает внутреннюю механику SDK.
3. **Удобной документации** — общий protocol contract фиксируется один раз.
4. **Mockability / testability** — доменные контроллеры тестируются изолированно.
5. **Стабильности контракта** — внутренняя реализация SDK эволюционирует без изменения доменного usage.

> **Итоговое архитектурное требование (вход для Р16):** при выборе формы public API приоритет —
> решению, которое **лучше выражает semantic protocol contract**, а не просто минимизирует количество
> кода/объектов. По этому критерию форма **выбрана — interface-facade `CaptureSDK`** (Р16, РЕШЕНО):
> единая godoc-точка контракта + mockability + сокрытие deps выражают protocol-boundary сильнее, чем
> набор free-функций.

### Р16. Форма public API — **РЕШЕНО: documented interface-facade (`CaptureSDK`)**
SDK v1 выставляет **документируемый interface-facade**, а НЕ free functions и НЕ session/`Planner`.
**Причина:** capture SDK — это **protocol boundary**, а не набор helper-функций. Доменным командам нужна
**одна точка входа**, где виден весь lifecycle capture-планирования: manifest leg → optional data leg →
children leg → terminal outcome `MarkPlanningReady`/`MarkPlanningFailed` (§0).

**Фасад нужен для:**
- единой **godoc-точки** всего capture-протокола (Р18);
- **mockability** в unit-тестах доменных reconciler'ов;
- **constructor injection** в контроллеры;
- **сокрытия deps**: `client`, `APIReader`, `VolumeCaptureProvider`, ownerRef/patch mechanics;
- **стабилизации** публичного контракта при изменении внутренней реализации SDK.

**Форма** (composition of narrow roles, Р24/Р27; `AlreadyCaptured` убран в internal, Р20; `ValidateSource`
убран из facade, Р28; точные сигнатуры — §5.3):
```go
type CaptureSDK interface { ReadinessFault; Planning; PlanningBarrier }
// ReadinessFault:  MarkNotReady(...)        // общий Ready=False (source/artifact), Р30; ValidateSource убран (Р28)
// Planning:        EnsureChildren / EnsureVolumeCapture / EnsureManifestCapture
// PlanningBarrier: MarkPlanningReady / MarkPlanningFailed
// методы берут узкий target (Р27), не весь SnapshotAdapter
```

**Что НЕ делаем:**
- **Session/`Planner` не вводим** — нет настоящего mutable session-state, это framework smell.
- **Free functions** допустимы только как **internal/private implementation detail** (если удобно),
  но НЕ как основное публичное лицо SDK.

**Godoc на `CaptureSDK` MUST** (Р18) описывать весь lifecycle и инварианты: три planning legs;
crash/restart safety (Р19); desired-set semantics для children (Р17); ≤1 logical data slot (occupancy
0..1); content-free invariant; `MarkPlanningReady` как **barrier, а не planning operation**; и что
transport mechanics скрыты.

> Прежний «крен ~60/40» снят: выбор зафиксирован в пользу interface-facade. Базис — Р16-rationale
> (форма должна выражать semantic protocol contract) + Р18 (единая godoc-точка) + Р19/Р17 (durable,
> desired-set). `interface сам по себе boundary не создаёт` — поэтому фасад идёт **вместе** с package +
> SDK-owned types + godoc (Р15/Р18), а не вместо них.

**Подвопрос Р16a — `RejectInvalidSource` смешивает validation + mutation + error.** По смыслу это
**validation outcome**, а не action. Go-идиома: валидация выглядит как валидация, решение принимает caller.
**Финал (Р28):** `ValidateSource` вообще убран из facade (SDK ничего не добавлял поверх валидатора), домен
зовёт `SourceValidator` сам, а SDK публикует только исход:
```go
if err := validator.Validate(adapter.SourceRef()); err != nil {  // доменный SourceValidator (Р22)
    _ = sdk.MarkNotReady(ctx, adapter, NotReadySpec{Reason: "InvalidSourceRef", Cause: err})  // SDK: publish исход (Р30)
}
```
То есть identity-проверка — чисто domain concern, `MarkNotReady` — transport/publication SDK (Р30).

### Р17. Capture = три planning legs; children desired-set; publish ≠ barrier (РЕШЕНО)
**Ментальная модель (см. §0.1):** capture одного узла = **три независимые planning legs** —
**manifest** (always → ensure MCR), **data slot** (0..1 → ensure ≤1 VCR), **children** (0..N →
publish desired set). Это `node = manifest + one logical data slot + children[]`. Инвариант:
`SDK does not own domain decisions. SDK owns publication of three protocol legs.`

**Граница ответственности (children leg):** `Domain owns child planning (discovery + building child
objects). SDK owns child create/adopt and publication.` SDK **не предписывает**, как домен находит,
собирает и **строит** дочерние объекты (`ChildSpec.Object`, Р17.5) — это его внутренняя кухня. SDK
определяет только, **как дети публикуются в snapshot-протокол** (create/adopt + ownerRef + refs;
**delete-free**, Р23), и публикация — **desired-set based и атомарна** с точки зрения протокола.

**Lifecycle / semantic contract (см. §0.2):** `Ensure* = perform planning work` (меняет protocol-state),
`Mark* = commit planning outcome` (только фиксирует исход). После публикации всех применимых legs домен
обязан завершить planning phase **бинарным** terminal outcome: `MarkPlanningReady` (completed) или
`MarkPlanningFailed` (terminated with error). `MarkPlanningReady` ничего не планирует (не создаёт
MCR/VCR, не пишет refs) — максимум fail-fast валидация + выставление барьера.

**1) Контракт публикации — bulk desired-set, НЕ incremental.** Домен на текущем reconcile вычисляет
**полный** набор детей узла и отдаёт его SDK одним вызовом:
```go
children, err := planChildren(ctx, source)       // []ChildSpec: domain planning строит дочерние объекты (Р17.5)
sdk.EnsureChildren(ctx, adapter, children)       // SDK: create/adopt + ownerRef + sort/dedup + write childrenSnapshotRefs (delete-free, Р23)
```
**Не выставляем** `AddChild`/`AppendChild` в public API v1 — это закрепило бы incremental-status модель
и сделало бы `childrenSnapshotRefs` зависимыми от порядка вызовов / partial progress. Почему bulk:
- **детерминизм** — SDK видит весь набор, стабильно сортирует/дедуплицирует/пишет refs;
- **replace semantics** — status родителя отражает актуальный desired-set, а не историю добавлений
  (исчез ребёнок из доменной модели → SDK перестаёт публиковать его ref, объект отвязывается от графа;
  delete-free, реклейм — ownerRef GC, Р23);
- **идемпотентность** — естественная controller-runtime модель «вычисли desired целиком и ensure»;
- **меньше partial state** — нет «один ребёнок записан, второй ещё нет».

**2) Фаза планирования приватна до барьера.** Мы НЕ настаиваем, *как* домен собирает детей.
Настаиваем только на моменте барьера:
- *before readiness barrier* — domain-private planning phase (собирай/создавай/проверяй как угодно);
- *at readiness barrier* — SDK требует **полный консистентный desired child set** уже опубликованным;
- *after readiness barrier* — опубликованный набор детей **authoritative**.

**3) Publish ≠ barrier — две разные операции.** `EnsureChildren` меняет protocol-state (refs/CR),
`Mark*` — только фиксирует барьер и НЕ трогает список детей:
- `EnsureChildren(ctx, t, desired)` — create/adopt дочерних snapshot-CR, sort/dedup, запись
  `childrenSnapshotRefs`; **delete-free** (Р23): дети вне desired отвязываются от графа (выпадают из refs),
  не удаляются. Для leaf без детей — `EnsureChildren(ctx, t, nil)` (пустой set = позитивное «детей нет» →
  публикует пустой список refs). **MUST (Р23):** звать только после успешного discovery; при сбое discovery —
  `MarkPlanningFailed`, а НЕ `EnsureChildren(nil)` (nil ≠ «не смог собрать»);
- `MarkPlanningReady(ctx, t)` — выставляет барьер (reason/message), опционально проверяет, что
  refs уже опубликованы/консистентны, **НЕ меняет** список детей и ничего не создаёт.

**4) Переименование: `MarkChildrenReady` → `MarkPlanningReady` (SDK прячет legacy-имя condition).**
Семантика барьера шире, чем «дети готовы»: это *«домен закончил планирование узла: children refs +
MCR/VCR published»*. Публичный intent — `sdk.MarkPlanningReady(...)`; **внутри** SDK выставляет
исторический `ChildrenSnapshotReady=True` (хороший пример сокрытия legacy/wire naming, §1/Р15).
Симметрично провал — `MarkPlanningFailed(ctx, t, cause)`.

**5) Форма `ChildSpec` — РЕШЕНО: child-builder seam (домен отдаёт готовый объект), НЕ opaque spec.**
Дочерний snapshot-CR домен-типизирован (`ensureDemoVirtualMachineDiskChild` создаёт `DemoVirtualDiskSnapshot`
со своим `spec.sourceRef`, §5.1a.2). Тип, spec, source identity и имя ребёнка — **доменные решения**, не
transport. Поэтому `ChildSpec` несёт **построенный доменом объект**, а не `map[string]any`:
```go
type ChildSpec struct {
    // Object — построенный доменом дочерний snapshot-CR (тип/spec/имя/namespace — доменное решение).
    // SDK трактует его как template: deep-copy перед мутацией, выставляет ownerRef, create-or-adopt.
    Object client.Object
}
```
Граница кристально чистая и совпадает с инвариантом `SDK hides transport, not domain decisions`:
- **домен владеет:** child type, child spec, source identity, naming;
- **SDK владеет:** create/adopt, ownerRef, деривация `SnapshotChildRef` из объекта (single source of truth →
  без drift «Ref сказал X, Object назван Y»), sort/dedup/публикация `childrenSnapshotRefs` (delete-free, Р23).

**Почему НЕ opaque spec (`map[string]any`):** (1) SDK перестал бы быть content-agnostic — взял бы на себя
metadata/ownerRef/spec-merge/create-update rules; (2) `map[string]any` теряет compile-time типобезопасность
(`sourceRef`→`sourceref` всплыло бы в runtime); (3) slippery slope — сложные доменные spec (restorePolicy,
topologyHints, …) превратили бы SDK в контейнер произвольного YAML. `client.Object` здесь — **намеренное
scoped-исключение** из «hide k8s types at boundary» (Р15.5): это собственный типизированный объект домена,
а не transport-механика; домен и так уже им оперирует.

**Канонический flow узла (capture):** (`sdk` — инстанс `CaptureSDK`; `adapter` удовлетворяет нужный target)
```go
if err := validator.Validate(adapter.SourceRef()); err != nil {      // source identity — domain (Р28)
    return sdk.MarkNotReady(ctx, adapter, NotReadySpec{Reason: "InvalidSourceRef", Cause: err}) // Ready=False, stop (Р30)
}
children, err := planChildren(ctx, source)                           // domain planning: строит []ChildSpec (Р17.5)
if err != nil {                                                      // planning упал → НЕ EnsureChildren(nil)
    return sdk.MarkPlanningFailed(ctx, adapter, err)
}
if err := sdk.EnsureChildren(ctx, adapter, children); err != nil {
    return sdk.MarkPlanningFailed(ctx, adapter, err)
}
if err := sdk.EnsureVolumeCapture(ctx, adapter, volumeSpec); err != nil { // только для data-leg
    return sdk.MarkPlanningFailed(ctx, adapter, err)
}
if err := sdk.EnsureManifestCapture(ctx, adapter, manifestSpec); err != nil {
    return sdk.MarkPlanningFailed(ctx, adapter, err)
}
return sdk.MarkPlanningReady(ctx, adapter)                             // только барьер
```

**Future (НЕ v1):** дорогой/стриминговый discovery детей (paging) — не усложняем v1; наружный контракт
остаётся bulk desired-set.

### Р18. Documentation requirement — godoc как часть контракта (РЕШЕНО, нормативно)
Форма API выбрана: **interface-facade `CaptureSDK`** (Р16). **Требование к документации зафиксировано**
(это и есть «single **well-documented** semantic public API», Р16-rationale). Godoc — **часть контракта**,
не украшение.

**MUST:**
- **Каждый** exported type/interface/function/method/const в `pkg/snapshotsdk` имеет godoc.
- Godoc описывает **semantic contract**, а НЕ implementation details. Конкретно объясняет:
  - **intent** операции (что она значит на уровне протокола);
  - **protocol side effects** (что меняется в snapshot-протоколе);
  - **ownership boundaries** (что делает домен vs что делает SDK);
  - **idempotency / replace semantics**, где применимо (напр. children desired-set, Р17);
  - что функция **явно НЕ делает** (anti-expectations).
- Godoc **НЕ** раскрывает лишнюю transport-механику (точные condition-строки, merge-patch,
  retry-internals), **кроме** символов, явно помеченных как low-level/internal-facing.

**Top-level doc capture-пакета MUST** описывать **полный planning lifecycle** (§0): manifest leg →
optional data leg → children leg → terminal planning outcome — как **единую точку** canonical
documentation. Godoc отдельных методов MUST описывать **только локальные** guarantees и side effects
(не дублировать весь lifecycle).

**Анти-паттерн (запрещено):** `// EnsureChildren ensures children.` — формально есть, бесполезно.

**Эталон (обязательный стиль), напр. `MarkPlanningReady`:**
```go
// MarkPlanningReady commits successful planning for the snapshot node.
// It assumes all applicable planning legs have already been published:
// manifest capture, optional volume capture, and desired children.
// It does not create MCR/VCR objects and does not change child refs.
```
Форма выбрана (Р16: interface-facade), поэтому конкретно: **godoc на типе `CaptureSDK` несёт весь
lifecycle и инварианты** (единая точка), а **method-docs — только локальные** guarantees и side effects.

### Р19. Crash/restart safety during planning (РЕШЕНО, инвариант SDK)
Доменный контроллер может быть перезапущен **в любой точке** planning phase: до/после `EnsureChildren`,
`EnsureVolumeCapture`, `EnsureManifestCapture`, и до/после `MarkPlanningReady`/`MarkPlanningFailed`.
SDK capture-операции **MUST** быть идемпотентными и **durable-state driven**; **никакая** корректность
не зависит от in-memory прогресса доменного контроллера.

**Required semantics:**
- `Ensure*` операции безопасны к повтору (repeat = no-op при уже опубликованном состоянии).
- `Ensure*` выводят текущее состояние из **Kubernetes API/status**, а не из памяти процесса.
- `MarkPlanningReady` допустим **только** после того, как все применимые legs **durably published**.
- Partial planning progress — валиден и восстановим.
- Reconcile после рестарта пересчитывает desired state и сходится к **тому же** published protocol state.
- После рестарта **не создаются** дублирующие MCR/VCR/child refs.

**Сценарии:** умер после `EnsureChildren`, но до `EnsureManifestCapture` → следующий reconcile повторит
`EnsureChildren` (no-op) и продолжит. Умер после `MarkPlanningReady` → следующий reconcile увидит
published state/markers (internal TOCTOU live-refresh внутри `Ensure*`, Р20) и не создаст дублей.

Crash/restart safety включает случай, когда **desired domain state изменился, пока контроллер был выключен**:
до `MarkPlanningReady` повторные `Ensure*` реконсилируют partial durable state к **заново вычисленному**
desired state (не к зафиксированному до сбоя) — детально см. Р25.

> **Это прямая опора для Р17/Р16:** desired-set publication (а не incremental `AddChild`/`AppendChild`)
> и `Ensure*`-семантика выбраны в т.ч. потому, что после рестарта домен пересобирает **полный** child
> set, а SDK **детерминированно** сходится к нему. Incremental-API не пережил бы рестарт без риска
> partial/duplicate state.

## 4b. Consistency-аудит (ревью 4) — устранение протечек facade

Точечные решения после консистентного ревью: где facade ещё чуть-чуть превращался обратно в
transport-aware API. Главные — Р20 (`AlreadyCaptured`) и Р22 (`ValidateSource`).

### Р20. `AlreadyCaptured` — убрать из public facade (РЕШЕНО)
`AlreadyCaptured() (manifest, data bool, …)` **протекал**: возвращал наружу **internal transport-model**
(маркеры `manifestCaptured`/`dataCaptured`), и домен начинал мыслить деталями core. Завтра маркеров
может стать больше (`childrenObserved`/`bindingCompleted`) или `dataCaptured` перестанет быть bool —
facade потёк бы. **Решение:** `AlreadyCaptured` **НЕ** входит в public `CaptureSDK`; это **internal**
TOCTOU-механика. Домен его не зовёт: `Ensure*` и так идемпотентны (Р19) и сами делают live-refresh +
suppression. Caller просто вызывает `Ensure*` — «спрашивать already captured?» не нужно. (Если когда-то
понадобится публичный прогресс — отдать **semantic object** `CaptureProgress`, НЕ два `bool`; но в v1 не вводим.)

### Р21. Split `CaptureStatus` по ownership (РЕШЕНО)
Один DTO на двух writers размывает ownership (домен теоретически мог бы выставить core-поле). Разводим
**типами**, чтобы ownership был enforceable:
```go
type DomainCaptureState struct {            // domain read+write
    ManifestCaptureRequestName string
    VolumeCaptureRequestName   string
    ChildrenSnapshotRefs       []SnapshotChildRef
}
type CoreCaptureState struct {              // core-owned; домен ТОЛЬКО читает
    ManifestCaptured         bool
    DataCaptured             bool
    BoundSnapshotContentName string
}
```
Адаптер: `GetDomainCaptureState()/SetDomainCaptureState()` (read+write) и `CoreCaptureState()` —
**только getter** (нет сеттера → misuse невозможен по типам). Заменяет прежний слитный `CaptureStatus`.

### Р22. `ValidateSource` не владеет identity-логикой (РЕШЕНО)
`ValidateSource(expect GVK)` хардкодил `source identity == GVK validation` — это противоречит
зафиксированному принципу «source identity MUST NOT assume `spec.sourceRef`/конкретную форму» (§1
neutrality). Identity у домена может включать annotations/labels/source-class/custom-ref. **Решение:**
SDK НЕ владеет identity-логикой — домен описывает её **валидатором**:
```go
type SourceValidator interface { Validate(SourceRef) error }   // domain-owned
```
**Уточнение Р28:** валидатор как **параметр facade не нужен** — домен зовёт `validator.Validate(...)` сам,
а SDK оставляет только publication исхода (`MarkNotReady`, Р30). Это консистентно с «SDK hides transport,
not domain decisions».

### Р23. `EnsureChildren` — publication-only, **delete-free** (ПЕРЕСМОТРЕНО для v1)
**Было (исходное решение):** `EnsureChildren` делал полную реконсиляцию desired-set **включая удаление
orphan'ов** (SDK-owned child-CR, исчезнувших из desired-set).

**Стало (v1, delete-free — действующее решение):** `EnsureChildren` выполняет **create/adopt + публикацию**
desired-set и **НЕ удаляет** детей. Ребёнок, исчезнувший из desired-set, просто перестаёт быть в
`status.childrenSnapshotRefs` и **отвязывается от snapshot-графа**; сам объект остаётся в кластере и
реклеймится **ownerRef GC** (parent владеет каждым child) либо будущим cleanup-компонентом — не SDK.

**Почему пересмотрено** (аудит-ревью): delete-free
- честнее как **no-op refactor** (демо orphan'ов и так не удаляло);
- убирает риск снести чужой объект из-за stale/кривого `status`;
- делает SDK **чистым publication-layer**, а не lifecycle-owner всех child-CR;
- убирает из SDK `List`/previous-diff и **unstructured delete** (`internal/children`);
- сохраняет restart-safety: после рестарта SDK просто перепубликует актуальный список refs.

**`nil`/empty теперь безопасны.** `EnsureChildren(nil)` = «опубликовать пустой список детей» (leaf), а НЕ
«удалить всех детей». Сбой discovery по-прежнему должен вести к `MarkPlanningFailed(cause)`, а не к молчаливой
публикации пустого набора (это исказит граф), но катастрофического удаления уже не происходит. Godoc
`EnsureChildren` (Р18) MUST отражать: create/adopt + publication, **deletion не входит в v1**, объекты вне
desired отвязываются (ownerRef GC), а не удаляются.

**Удаление — вне v1.** Реальная сборка мусора (если понадобится) — отдельный GC/cleanup-компонент или ownerRef
GC, не `EnsureChildren`. См. также Р29 (механика orphan-diff более не нужна в v1).

### Р24. Interface segregation для тестируемости (РЕШЕНО, nice-to-have)
`CaptureSDK` из 8 методов тяжело мокать целиком. Реализация — **одна struct**, но публично выставляем и
**узкие роли**, которые доменный reconciler потребляет по месту (depend on the narrow interface):
```go
type ReadinessFault  interface { MarkNotReady(...) }        // общий Ready=False (Р30); ValidateSource убран (Р28)
type Planning        interface { EnsureChildren(...); EnsureVolumeCapture(...); EnsureManifestCapture(...) }
type PlanningBarrier interface { MarkPlanningReady(...); MarkPlanningFailed(...) }
// CaptureSDK = композиция ролей (одна реализация удовлетворяет все).
type CaptureSDK interface { ReadinessFault; Planning; PlanningBarrier }
```
Доменный тест мокает только нужную роль (напр. `Planning`), а не весь facade. Точный набор ролей —
финализируется на S1 PoC; godoc lifecycle (Р18) живёт на `CaptureSDK`. **Усиление Р27:** методы берут не
весь `SnapshotAdapter`, а узкий target-интерфейс (`ManifestCaptureTarget`/`ChildrenTarget`/…).

### Р25. Restart-safe recipe, НЕ linear workflow (РЕШЕНО)
Публичный flow SDK — это **рекомендуемый порядок идемпотентных reconcile-шагов**, а **не** транзакционный
workflow. Усиление Р19: crash/restart safety — это не только «повтор = no-op», но и **converge-to-current-desired**.

**Нормативно (Р25):** доменный контроллер **MAY** restart between any two calls; after restart it **MUST**
recompute domain desired state и снова вызвать ту же последовательность `Ensure*` **сверху** (не продолжать
«с памяти»). Until `MarkPlanningReady` is durably published, desired children/data/manifest **MAY** be
reconciled to the newly observed domain state.

**Каждый `Ensure*` — самостоятельный idempotent checkpoint:** заново читает durable state, сверяет desired,
дообеспечивает недостающее (children → create/adopt + publish, delete-free, Р23; ровно один VCR; MCR) и **безопасен после рестарта
между любыми двумя шагами**. Контроллер не несёт прогресс в памяти — recipe всегда выполняется заново:
```go
children, err := planChildren(ctx, source)                           // domain planning: строит []ChildSpec (Р17.5)
if err != nil {                                                      // planning упал → НЕ EnsureChildren(nil)
    return sdk.MarkPlanningFailed(ctx, barrierTarget, err)
}
if err := sdk.EnsureChildren(ctx, childrenTarget, children); err != nil {
    return sdk.MarkPlanningFailed(ctx, barrierTarget, err)
}
if err := sdk.EnsureVolumeCapture(ctx, volumeTarget, volumeSpec); err != nil {
    return sdk.MarkPlanningFailed(ctx, barrierTarget, err)
}
if err := sdk.EnsureManifestCapture(ctx, manifestTarget, manifestSpec); err != nil {
    return sdk.MarkPlanningFailed(ctx, barrierTarget, err)
}
return sdk.MarkPlanningReady(ctx, barrierTarget)
```
После рестарта мы **не продолжаем с памяти**, а снова выполняем весь recipe сверху. Если дети изменились до
`MarkPlanningReady` — `EnsureChildren` приводит published children к **новому** desired-set. Если planning
детей упал — ставим `MarkPlanningFailed`, а не `EnsureChildren(nil)` (Р23).

Сценарий (desired state изменился, пока контроллер был выключен):
```
T1  EnsureChildren([A,B])        // refs/CR опубликованы
T2  EnsureManifestCapture        // ещё НЕ вызван
T3  MarkPlanningReady            // ещё НЕ выставлен
T4  domain-controller выключен
T5  source изменился: дети [A,C]
T6  controller включён
T7  reconcile → discovery = [A,C]
T8  EnsureChildren([A,C]) MUST:  keep/reuse A; create/ensure C;
                                 detach B (drop from refs, no delete); refs → [A,C]
T9  EnsureManifestCapture/EnsureVolumeCapture — продолжают planning
T10 MarkPlanningReady — только после нового актуального desired-set
```
`MarkPlanningReady` — **commit point**: после него published planning-set становится authoritative. До
барьера partial planning state — **recoverable и replaceable** (не immutable). Это сильнее, чем «no-op
после рестарта»: SDK сходится к **актуальному** desired-set, а не к зафиксированному до сбоя.

### Р26. True facade, а не layered library (РЕШЕНО) — главное напряжение ревью
Было competing-abstraction: одновременно interface-facade `CaptureSDK` **и** набор публичных пакетов
(`capture/ status/ children/ contract/ volumecapture/`). Гибрид опасен — команды начинают bypass-ить
facade в transport-слой; litmus «открыл root — всё видно» частично провален (нужно прыгать по пакетам).
**Решение (Вариант A — true facade):** канонический **root `pkg/snapshotsdk`** несёт **весь** type+protocol
API (`CaptureSDK`, узкие роли/targets, `SnapshotAdapter`, `SourceValidator`, `ChildSpec`,
`ManifestCaptureSpec`, `VolumeCaptureSpec`, `VolumeCaptureProvider`, SDK-owned `Condition`/`ChildRef`).
Публичен только подпакет `transform/` (restore extension point). `children/`, `status/`, `ownerref/`,
`patch/`, `conditions/`, `storagefoundation/` → **`internal/*`** (transport mechanics, не доменный contract).
Это закрывает leaks A/B ревью и проходит litmus (§1). См. дерево §3.

### Р27. Узкие target-интерфейсы в сигнатурах методов (РЕШЕНО) — реальное исполнение Р24
Р24 был декларативен, пока методы принимали целый `SnapshotAdapter` (fat interface фактически вернулся).
**Решение:** методы принимают **узкий target-интерфейс**, отражающий реальную зависимость, а не весь
адаптер:
```go
type ManifestCaptureTarget interface { ObjectAccessor; DomainStateAccessor; CoreStateReader }
type VolumeCaptureTarget   interface { ObjectAccessor; DomainStateAccessor; CoreStateReader }
type ChildrenTarget        interface { ObjectAccessor; DomainStateAccessor }
type BarrierTarget         interface { ConditionWriter }
type ReadinessFaultTarget  interface { ObjectAccessor; ConditionWriter }
```
`SnapshotAdapter` (композиция всех ролей) удовлетворяет каждый target — домен реализует **один раз** и
передаёт свой адаптер везде; узкие типы документируют истинную зависимость и дают focused-моки. Точный
набор ролей в каждом target финализируется на S1 PoC.

### Р28. Убрать `ValidateSource` из facade (РЕШЕНО) — facade ради facade
`ValidateSource` в SDK сводился к `return validator.Validate(adapter.SourceRef())` — SDK ничего не
добавлял. **Решение:** `ValidateSource` **НЕ** входит в facade; identity-проверку домен делает сам, а SDK
оставляет только **publication** исхода — `MarkNotReady` (Р30). Тогда SDK действительно занимается только
publication/transport, а не доменными решениями.
```go
if err := validator.Validate(adapter.SourceRef()); err != nil {  // чистый domain concern
    _ = sdk.MarkNotReady(ctx, adapter, NotReadySpec{Reason: "InvalidSourceRef", Cause: err})  // SDK: publish (Р30)
}
```
`SourceValidator` остаётся доменным типом (Р22), но как параметр facade больше не нужен.
**Уточнено Р30:** `MarkInvalidSource` обобщён в `MarkNotReady` (см. ниже) — domain validation остаётся
у домена, SDK публикует любой `Ready=False`-исход (source / artifact-missing), не только source.

### Р29. Orphan-GC без desired GVK: diff против durable `childrenSnapshotRefs` (НЕ ПРИМЕНЯЕТСЯ В v1 — снято Р23)
> **Статус:** этот механизм был решением для orphan-**deletion**. После пересмотра Р23 (v1 delete-free)
> SDK orphan'ов **не удаляет**, поэтому diff `previous − desired` и unstructured-delete в `internal/children`
> в v1 **не существуют**. Раздел сохранён как обоснование на случай, если delete вернётся в отдельный
> GC/cleanup-компонент вне `EnsureChildren`.

**Проблема (вскрыто сверкой кода, §5.1a.3):** `EnsureChildren(nil)` не несёт ни одного `ChildSpec.Object`,
значит SDK не может вывести child GVK из desired-set. Orphan-GC **не должен** зависеть от перечисления
kind'ов или list-by-ownerRef как основного механизма.

**Решение — diff против durable status:**
```
previous = adapter.<...>.ChildrenSnapshotRefs   // status.childrenSnapshotRefs (durable, apiVersion/kind/name)
desired  = refs, derived from each ChildSpec.Object
orphans  = previous - desired
```
SDK удаляет orphan child-CR по `apiVersion/kind/name` из durable `childrenSnapshotRefs`. Это:
- **crash-safe** — источник истины durable в status, переживает рестарт (Р19/Р25);
- **работает для empty desired-set** — `previous` известен из status, GVK брать неоткуда не нужно;
- **не требует заранее знать все child kinds** — никакого enumeration GVK / list-всех-видов.

`SnapshotChildRef` (api) несёт `APIVersion/Kind/Name` → достаточно для адресной delete. **List-by-ownerRef
допустим только как optional hardening**, не как основной алгоритм.

### Р30. Publication `Ready=False`: обобщить `MarkInvalidSource` → `MarkNotReady` (РЕШЕНО)
**Проблема (вскрыто сверкой кода, §5.1a.1):** фактический код публикует `Ready=False` НЕ только для invalid
source. Минимум три blocking-сценария:
- invalid source / source not found → `Ready=False`, **stop** (`patchDemo*SnapshotReady`/`NotReady`, reasons
  `InvalidSourceRef`/`SourceNotFound`);
- missing PVC / artifact missing → `Ready=False`, **requeue** (`ReasonArtifactMissing`);
- graph/children planning failure → `ChildrenSnapshotReady=False` (это `MarkPlanningFailed`, отдельный барьер).

`MarkInvalidSource` покрывает только первый. **Решение — заменить узкий `SourceFault.MarkInvalidSource` на
общий `ReadinessFault.MarkNotReady`:**
```go
type ReadinessFault interface {
    // MarkNotReady publishes Ready=False with a domain-chosen reason. It performs NO domain validation:
    // source identity validation and PVC/artifact resolution stay domain logic; the SDK only publishes.
    MarkNotReady(ctx context.Context, t ReadinessFaultTarget, in NotReadySpec) error
}
type NotReadySpec struct {
    Reason  Reason  // domain-chosen, e.g. "InvalidSourceRef"/"SourceNotFound"/"ArtifactMissing"
    Message string
    Cause   error   // optional, for logging/aggregation
    Requeue bool    // intent hint: artifact-missing → true; invalid source → false
}
```
`MarkNotReady` публикует только outcome. `MarkPlanningFailed` остаётся отдельно (барьер
`ChildrenSnapshotReady=False`, а не `Ready=False`). Source validation и artifact resolution — доменные.

## 5. Implementation-plan S1 (SDK v1 capture-only)

**Цель v1:** вынести из демо-домена только повторяемые capture-инварианты; не трогать restore;
не трогать aggregated apiserver; не вводить обязательный embeddable status; сохранить поведение
демо как **no-op refactor** (все существующие тесты демо зелёные).

### 5.1 Inventory: функции демо, переносимые в `snapshotsdk`

| Источник (demo) | Что | Целевой пакет |
|---|---|---|
| `disk_controller.go` / `vm_controller.go` `patch*ChildrenSnapshotReady`, `patch*Ready`, `patch*ManifestCaptureRequestName`, `patch*VolumeCaptureRequestName`, `patch*ChildrenRefs` | D4a optimistic-lock write-механика → `internal/patch` (merge-patch+lock) + `internal/conditions` (Condition-merge); обёртка пишет через `SnapshotAdapter`. Домен зовёт не их, а capture intent-глаголы `Ensure*/Mark*` (Р15.6–15.7) | `internal/patch`+`internal/conditions`+`internal/status` (Р26) |
| TOCTOU-блок live-refresh маркеров (`disk_controller.go` ~122–132, `vm_controller.go` ~128–137), `steady_state.go::demoReconcilerReader` | живое чтение `APIReader` перед планированием (capture-lifecycle, Р11) | root `pkg/snapshotsdk` |
| `materialization.go`: `ensureDemoSnapshotManifestCaptureRequest`, `demoSnapshotManifestCaptureRequestName`, `demoManifestCaptureTargetsFromVCR` | MCR ensure + имя + деривация targets из VCR | root `pkg/snapshotsdk` |
| `manifest_targets.go`: `manifestTargetsFromVolumeTargets`, `appendOwnedPVCManifestTargets`, `sortManifestTargets`, `manifestTargetDedupKey` | manifest-targets из VCR-targets | root `pkg/snapshotsdk` |
| `disk_volume_capture.go`: `ensureDemoDiskVolumeCaptureRequest`, `demoDiskVCRHasOwnerRef` | реализация `VolumeCaptureProvider.Ensure` (Р13) | `internal/storagefoundation` |
| `pkg/volumecapture` (`names.go`, `types.go`) + `internal/controllers/volumecapture/unstructured.go` | тип `Target`/`VolumeCaptureProvider` (порт) → **root** (Р26); VCR-схема (`*GVK`, `New…Object`, `ParseVolumeCaptureTargets`, `…Equal`) → `internal/storagefoundation` | root (порт) + `internal/storagefoundation` (impl) |
| `materialization.go`: `demoSnapshotOwnerReference`, `demoSnapshotOwnerRefMatches`, `ensureDemoSnapshotOwnerRef`, `isSnapshotParentOwnerRef` (параметризовать parent-kind'ами) | ownerRef-граф (implementation detail, Р12) | `internal/ownerref` |
| `common/glue.go`: `OwnerReferencesEqual` → `internal/ownerref`; `SortSnapshotChildRefs`, `SnapshotChildRefsEqualIgnoreOrder` → `internal/children` | сравнение/сортировка (transport mechanics, Р26) | `internal/ownerref` + `internal/children` |
| `vm_controller.go`: паттерн `ensure дочернего снапшота` + публикация refs (логика «какие дети» остаётся в демо) | generic ensure-child + build refs (delete-free, Р23) | `internal/children` |
| `source_identity.go::resolveDemoSnapshotSource` | становится доменным `SourceValidator` (Р22/Р28); SDK-сторона — только `MarkNotReady` (root, Р30) | демо-домен (validator) + root (publication) |
| `images/domain-controller/pkg/domainsdk` (`transformer.go`, `doc.go`): `Transformer`, `RestoreNode`, `NodeResult` | механический move интерфейса + DTO (БЕЗ restore-логики); внутренние импорты мигрируем, старый пакет удаляем целиком, без shim (Р14) | `transform/` |

**Остаётся в демо (доменное):** оркестрация `Reconcile`, выбор детей (VM→диски,
`demoDiskOwnedByVM`), PVC-таргеты для VCR, имена/kind'ы домена, всё из 2.5/2.6/2.7.

### 5.1a Consistency-аудит vs фактический код (ревью 5)

Сверка плана с реальным кодом `images/domain-controller/internal/controllers/demo` + `api/`.

**Подтверждено (план совпадает с кодом):**
- статус-поля `api/demo/v1alpha1`: `DomainCaptureState{ManifestCaptureRequestName, VolumeCaptureRequestName,
  ChildrenSnapshotRefs}` и `CoreCaptureState{ManifestCaptured, DataCaptured, BoundSnapshotContentName}` —
  **1:1** с реальным `DemoVirtualDiskSnapshotStatus`/`...MachineSnapshotStatus` (Р21 split корректен);
- TOCTOU live-refresh (`!DataCaptured||!ManifestCaptured` → `APIReader.Get`), D4a
  `MergeFromWithOptimisticLock`, барьер `ConditionChildrenSnapshotReady`, MCR/VCR ensure, ownerRef-хелперы,
  volumecapture-порт (`vcpkg.Target`/`VolumeCaptureRequestGVK` + `vcctrl.*` impl), `domainsdk.Transformer` —
  всё соответствует §5.1; reasons (`ReasonCompleted`/`ReasonCreateChildFailed`/`ReasonGraphPlanningFailed`/
  `ReasonArtifactMissing`) есть в `api/storage/v1alpha1/conditions.go`.

**Уточнения, которые код навязывает (решить на S1, иначе план расходится с реальностью):**
1. **Publication `Ready=False` — обобщить в `MarkNotReady` — РЕШЕНО (Р30).** Демо публикует `Ready=False`
   не только для source: invalid/not-found source (`InvalidSourceRef`/`SourceNotFound`, stop) И missing PVC
   (`ReasonArtifactMissing`, requeue), всё как `ConditionReady=False`+reason (а НЕ отдельным condition).
   Узкий `MarkInvalidSource` заменён общим `MarkNotReady(NotReadySpec{Reason,Message,Cause,Requeue})`.
   `MarkPlanningFailed` остаётся отдельно (барьер `ChildrenSnapshotReady=False`). Детали — Р30.
2. **Child-материализация домен-типизирована — РЕШЕНО (Р17.5): child-builder seam.**
   `ensureDemoVirtualMachineDiskChild` создаёт **`DemoVirtualDiskSnapshot`** с доменным `spec.sourceRef` +
   parent ownerRef. Domain-agnostic SDK не строит домен-типизированный child CR сам. Решение:
   `ChildSpec{Object client.Object}` — домен отдаёт **готовый объект** (тип/spec/source/имя — доменное
   решение), SDK владеет create/adopt + ownerRef + дериваця `SnapshotChildRef` + sort/dedup (delete-free).
   Opaque spec (`map[string]any`) отвергнут (architectural leak; теряет типобезопасность; slippery slope) —
   обоснование в Р17.5.
3. **Delete-free children (Р23) — для demo это no-op.** Текущее демо orphan child-CR не удаляет, а VM
   ссылается на один диск (`vm.Spec.VirtualDiskName`), поэтому orphan вообще не возникает. В v1 SDK тоже
   **не удаляет** детей: ребёнок вне desired выпадает из `status.childrenSnapshotRefs` (отвязка от графа),
   реклейм — ownerRef GC. Это держит S1 строго **no-op refactor** (никакого нового delete-поведения);
   механика orphan-diff (Р29) в v1 не используется.
4. **`VolumeCaptureSpec` несёт единственный data ref (`DataRef *Target`), посчитанный доменом; missing-PVC —
   доменный.** Узел связывает ≤1 data-артефакт (Variant A, как `SnapshotContent.status.dataRef`); несколько
   PVC = несколько дочерних узлов, а не список. `EnsureVolumeCapture` = чистый ensure VCR. Резолв PVC
   (`APIReader`), сборка единичного `Target`, терминальный `ReasonArtifactMissing` + requeue — остаются в
   домене (это «что захватывать»).
5. **`ManifestCaptureSpec` несёт полный desired-set manifest targets.** Домен решает, какие namespaced
   объекты принадлежат manifest capture текущего snapshot-узла; SDK превращает этот набор в один MCR и
   добавляет technical owned-PVC target из VCR при наличии линии данных. Это важно для subtree exclude:
   объекты, captured в MCP дочернего/доменного узла, затем исключаются из root/parent MCR.

### 5.2 Целевые пакеты — см. раздел 3 (только v1-пакеты).

### 5.3 Минимальные публичные интерфейсы (черновик)

```go
// ───────────── SDK-OWNED value objects на границе (Р15.5): НЕ metav1/k8s типы наружу ─────────────
type ConditionType string
type Status        string   // ConditionTrue/False/Unknown как SDK-константы
type Reason        string
type Condition struct { Type ConditionType; Status Status; Reason Reason; Message string }
// conversion Condition <-> metav1.Condition — внутри SDK (internal/conditions), не у домена.

// Field-mapping SEAM (Р10), НЕ reconcile-time API. Адаптер — единственный мост между доменным CR и SDK;
// internal-механика SDK не лезет в поля мимо него. Идиома Go (Р15.3): не один жирный интерфейс,
// а capability-split на маленькие роли, которые функции требуют по отдельности (узкие targets — Р27).
//
// Invariant: each snapshot node owns exactly one logical data slot (occupancy 0..1;
// manifest-only node => 0; data node => 1). Multiple independent data legs per node — unsupported in v1.
type ObjectAccessor  interface { Object() client.Object }        // мост к controller-runtime (seam-only)
type SourceProvider  interface { SourceRef() SourceRef }
// Split по ownership (Р21): domain-state — read+write; core-state — ТОЛЬКО getter (нет сеттера →
// домен не может выставить core-поле; misuse невозможен по типам).
type DomainStateAccessor interface { GetDomainCaptureState() DomainCaptureState; SetDomainCaptureState(DomainCaptureState) }
type CoreStateReader     interface { CoreCaptureState() CoreCaptureState }
type ConditionWriter     interface { GetConditions() []Condition; SetConditions([]Condition) } // SDK-owned Condition

// SnapshotAdapter — композиция ВСЕХ ролей (домен реализует один раз). Методы facade принимают НЕ его,
// а узкие target-интерфейсы (Р27) — реальную зависимость. Адаптер удовлетворяет каждый target, поэтому
// домен передаёт свой адаптер везде; узкие типы документируют истинную зависимость и дают focused-моки.
type SnapshotAdapter interface {
    ObjectAccessor; SourceProvider; DomainStateAccessor; CoreStateReader; ConditionWriter
}

// Узкие targets (Р27): что РЕАЛЬНО нужно методу, а не весь адаптер. Набор ролей финализируется на S1.
type ManifestCaptureTarget interface { ObjectAccessor; DomainStateAccessor; CoreStateReader }
type VolumeCaptureTarget   interface { ObjectAccessor; DomainStateAccessor; CoreStateReader }
type ChildrenTarget        interface { ObjectAccessor; DomainStateAccessor }
type BarrierTarget         interface { ConditionWriter }
type ReadinessFaultTarget  interface { ObjectAccessor; ConditionWriter }

// Ownership разведён типами (Р21): два DTO, не один слитный.
type DomainCaptureState struct {           // domain read+write
    ManifestCaptureRequestName string
    VolumeCaptureRequestName   string
    ChildrenSnapshotRefs       []SnapshotChildRef
}
type CoreCaptureState struct {             // core-owned; домен ТОЛЬКО читает
    ManifestCaptured         bool
    DataCaptured             bool
    BoundSnapshotContentName string
}

// ───────────── ПУБЛИЧНОЕ ЛИЦО SDK: documented interface-facade в КАНОНИЧЕСКОМ root (Р16/Р26) ─────────────
// Нейминг — на языке reconcile-intent (Ensure/Mark, Р15.7); термины домена (MCR/VCR/children) НЕ прячем;
// прячем только transport-механику (conditions/patch/retry/status-layout). True facade (Р26): весь этот
// type+protocol API живёт в root pkg/snapshotsdk; transport-пакеты — internal/*.
//
// CaptureSDK — единая точка входа capture-протокола. Godoc на типе (Р18) несёт весь lifecycle и
// инварианты: 3 legs / crash-restart safety + converge-to-current-desired (Р19/Р25) / desired-set
// children publication, delete-free (Р17/Р23) / ≤1 data slot / content-free / MarkPlanningReady = barrier (не
// planning) / transport mechanics скрыты. deps (client, APIReader, VolumeCaptureProvider, ownerRef/patch)
// уходят в конструктор реализации. Session/Planner НЕ вводим; free-функции — только internal impl detail.
//
// Interface segregation (Р24/Р27): facade = композиция узких ролей, методы берут узкий target (не весь
// адаптер) → focused-моки. `ValidateSource` НЕ в facade (Р28: SDK ничего не добавлял; identity — домен
// сам). `AlreadyCaptured` НЕ выставляется (Р20: internal TOCTOU, Ensure* идемпотентны).
type SourceValidator interface { Validate(SourceRef) error }   // domain-owned identity (Р22); домен зовёт сам

// NotReadySpec — доменно-выбранный Ready=False исход; SDK только публикует, валидацию НЕ делает (Р30).
type NotReadySpec struct {
    Reason  Reason  // напр. "InvalidSourceRef"/"SourceNotFound"/"ArtifactMissing" (домен выбирает)
    Message string
    Cause   error   // optional — для логов/агрегации
    Requeue bool    // intent-hint: artifact-missing → true; invalid source → false
}

type ReadinessFault interface {
    // MarkNotReady publishes Ready=False with a domain-chosen reason. It performs NO domain validation:
    // source identity validation and PVC/artifact resolution stay domain logic (Р28/Р30); the SDK only
    // publishes the outcome. MarkPlanningFailed is separate (it drives the ChildrenSnapshotReady barrier,
    // not Ready).
    MarkNotReady(ctx context.Context, t ReadinessFaultTarget, in NotReadySpec) error
}

type Planning interface {
    // EnsureChildren publishes the complete desired child set for the snapshot node.
    // Each ChildSpec carries a domain-built child object (ChildSpec.Object); the domain owns
    // child type, spec, source identity and naming. The SDK treats the object as a template
    // (deep-copies it, sets the parent ownerRef, create-or-adopts) and derives the published
    // SnapshotChildRef from it. It is delete-free (SDK v1): it ensures every desired child and
    // publishes the desired refs, but never deletes children — a child no longer desired drops out of
    // status.childrenSnapshotRefs (detached from the graph, reclaimed by ownerRef GC). It does not mark
    // planning ready.
    //
    // NOTE: a nil/empty desired set means "this node has no children" (leaf) and publishes an empty
    // ref list. It does NOT mean "discovery failed". Callers MUST call EnsureChildren only after
    // successful child discovery; on discovery failure call MarkPlanningFailed instead and never pass
    // nil/empty as a failure signal (it would wrongly publish an empty graph, though it deletes nothing).
    EnsureChildren(ctx context.Context, t ChildrenTarget, desired []ChildSpec) error
    // EnsureVolumeCapture ensures the optional data leg for the snapshot node.
    // It creates or reuses exactly one VCR when data is present.
    EnsureVolumeCapture(ctx context.Context, t VolumeCaptureTarget, in VolumeCaptureSpec) error
    // EnsureManifestCapture ensures the manifest leg for the snapshot node.
    // It creates or reuses the MCR and does not mark planning as ready.
    EnsureManifestCapture(ctx context.Context, t ManifestCaptureTarget, in ManifestCaptureSpec) error
}

type PlanningBarrier interface {
    // MarkPlanningReady commits successful planning for the snapshot node.
    // It does not create MCR/VCR objects and does not change child refs.
    MarkPlanningReady(ctx context.Context, t BarrierTarget) error
    // MarkPlanningFailed commits failed planning for the snapshot node.
    // It does not roll back already published planning legs.
    MarkPlanningFailed(ctx context.Context, t BarrierTarget, cause error) error
}

type CaptureSDK interface { ReadinessFault; Planning; PlanningBarrier }

// VolumeCaptureProvider: ПОРТ backend'а (Р13), тип живёт в root. Реализация — internal/storagefoundation.
type VolumeCaptureProvider interface {
    Ensure(ctx context.Context, in VolumeCaptureInput) (VolumeCaptureRef, error)
    ParseTargets(obj any) ([]Target, error)
}

// ───────────── ВНУТРЕННИЙ СЛОЙ (НЕ лицо SDK): wire-механика (Р15.6 + Р26: всё internal) ─────────────
// internal/children:  ensure/sort/dedup/compare childrenSnapshotRefs, delete-free (Р23).
// internal/status:    D4a merge-patch + optimistic-lock + condition-merge (бывш. public-advanced).
// internal/patch:     D4a merge-patch + optimistic-lock + RetryOnConflict.
// internal/conditions:SDK Condition <-> metav1.Condition, condition-merge, type-strings.
// internal/ownerref (Р12): SnapshotOwnerReference, EnsureOwnerRef, OwnerReferencesEqual.
//   Всё это зовётся ТОЛЬКО из capture intent API (root), домен сюда не импортирует.
```
Точные сигнатуры уточняются на S1. Ключевое: **доменный код выражает intent через `CaptureSDK` (root)**, не
зная про `metav1.Condition`/condition-strings/merge-patch/optimistic-lock (Р15: семантика читается
из exported API, не из ADR). `SnapshotAdapter` (§Р10) — **не reconcile-time API**, а одноразовый
**field-mapping seam** из маленьких capability-ролей; методы facade берут **узкий target** (Р27), а не весь
адаптер. На границе фигурируют **SDK-owned** типы (`Condition`/`ConditionType`/`Status`), а не k8s-типы.
True facade (Р26): публичны только root `pkg/snapshotsdk` + `transform/`; `children`/`status`/`patch`/
`conditions`/`ownerref`/`storagefoundation` — `internal/*`, не публичное лицо.

### 5.4 Тесты: переносимые / добавляемые
- **Переносим/адаптируем** существующие демо-unit-тесты capture-плеча: `source_identity_test.go`,
  `source_ref_test.go`, `disk_volume_capture_test.go` (и capture-куски из
  `virtualdisk_controller_test.go` / `virtualmachine_controller_test.go`) → в соответствующие
  `snapshotsdk/*`-пакеты как табличные тесты на чистых функциях.
- **Оставляем в демо** интеграционные тесты как **conformance**: демо на SDK проходит текущие
  e2e/envtest (`test/integration/demovirtualdisksnapshot_pr5a_test.go`,
  `demovirtualmachinesnapshot_pr5b_test.go` и пр.) без изменений в ожиданиях.
- **Добавляем** минимальный conformance (Р9): D4a ownership, content-free guard, детерминизм refs, TOCTOU.

### 5.5 Места в демо, которые меняются
- `disk_controller.go`, `vm_controller.go` — `Reconcile` выражает intent через инъектированный
  `CaptureSDK` (Р16): `EnsureChildren`/`EnsureVolumeCapture`/`EnsureManifestCapture`/`MarkPlanningReady`/
  `MarkPlanningFailed`/`MarkNotReady` (Р30). Source-валидацию домен делает сам своим `SourceValidator`
  (Р28). Children — desired-set одним вызовом (Р17). Локальные `patch*`/`ensure*` и прямые
  `metav1.Condition`/condition-strings удаляются. Доменный код больше НЕ знает про
  conditions/merge-patch/optimistic-lock — только про намерения протокола.
- `materialization.go`, `manifest_targets.go` — capture-логика уезжает в **root** `pkg/snapshotsdk`,
  остаются тонкие доменные обёртки (sourceRef→targets, какие дети).
- `disk_volume_capture.go` — становится реализацией `VolumeCaptureProvider` в `internal/storagefoundation`.
- `source_identity.go` — становится доменным `SourceValidator` (Р22/Р28), домен зовёт его сам; при ошибке —
  `sdk.MarkNotReady` (Р30). `steady_state.go` (live-refresh маркеров) — internal TOCTOU внутри `Ensure*` (Р20).
- `common/glue.go` → split: ownerRef-часть в `internal/ownerref`, child-refs — в `internal/children`.
- `pkg/volumecapture` + `internal/controllers/volumecapture/unstructured.go` → split: тип-порт в
  **root** `pkg/snapshotsdk`, VCR-схема в `internal/storagefoundation`.
- Демо реализует `SnapshotAdapter` для `DemoVirtualDiskSnapshot` / `DemoVirtualMachineSnapshot`
  (полный read+write маппинг своих status-полей).
- `images/domain-controller/pkg/domainsdk` — `Transformer`/`RestoreNode`/`NodeResult` переезжают в
  `pkg/snapshotsdk/transform`; импорты в `internal/domainapi/restore.go` и
  `internal/controllers/demo/restore_transform.go` (+`_test`) мигрируют на новый путь, старый пакет
  `pkg/domainsdk` удаляем целиком (без shim). Комментарий в
  `state-snapshotter-controller/.../restore/delegate.go` поправить на новый путь.

### 5.6 Explicit non-goals (v1)
- **Нет** restore-материализации (dataRef-резолв, VRR, adopt PVC).
- **Нет** доменного aggregated apiserver и его каркаса.
- **Нет** новой restore-логики при переносе `Transformer` — в `transform/` едет только абстрактный
  интерфейс + DTO (механический move/re-export); restore-поведение не меняется. *(Перенос namespace
  входит в v1; restore-реализация — нет.)*
- **Нет** каркас-framework (`scaffold/`) — только library-first.
- **Нет** обязательного embeddable status (только optional helper).
- **Нет** отдельного `sdk/go.mod` / отдельного репозитория.
- **Нет** Go-зависимости на storage-foundation (VCR/VRR — unstructured+GVK).
- **Нет** изменения поведения демо (строго no-op refactor).
- **Нет** поддержки нескольких независимых линий данных на одном узле — *multiple independent data
  legs per node are explicitly unsupported* (фиксируем Вариант A: один logical data slot,
  occupancy 0..1). **Глубину дерева SDK НЕ ограничивает** — это доменная логика; capture/children
  работают рекурсивно/локально на любом узле любой глубины.

## 6. Canonical snapshot graph model → вынести в `spec/`
Модель data slot (§1) — это не SDK-деталь, а **core domain model**. Предлагается зафиксировать в
`spec/system-spec.md` как канон формы графа (отдельно от SDK):

> **Snapshot graph node** состоит ровно из трёх ортогональных частей:
> 1. **Manifest component** — всегда присутствует (ровно 1 `ManifestCheckpoint`);
> 2. **Single logical data slot** — occupancy 0..1 (manifest-only ⇒ 0; data-узел ⇒ 1);
> 3. **Child references** — 0..N (рёбра дерева).

SDK и доменные контроллеры — потребители этой модели, а не её владельцы.

## 7. Принято / решено
- **SDK domain-agnostic** (см. SDK neutrality principle, §1): проектируем **adapter-first**, БЕЗ
  обязательного embedded status; конкретные адоптеры — implementation detail, не вход дизайна.
- **Единый SDK namespace `pkg/snapshotsdk`** (Р14): restore-`Transformer` уже в v1 механически
  переносим в `pkg/snapshotsdk/transform`; старый `domainsdk` удаляется после миграции внутренних
  импортов, compatibility shim не создаётся (v0, внешней совместимости нет). Это унифицирует
  публичную поверхность, но НЕ добавляет restore-реализацию в v1.
- **SDK v1 = самостоятельный no-op refactor**, не зависит от завершённых планов и ни в какой общий
  план не встраивается; делается отдельным набором коммитов.
- Арх-аудит (ревью 3): Р10 (full adapter), Р11 (status/capture), Р12 (ownerref→internal),
  Р13 (volumecapture-порт) — приняты, см. §4a.
- Арх-направление (ревью 4): **internal platform SDK** для инженерных команд (§1). Принцип —
  **`hide transport mechanics, not domain concepts`**: MCR/VCR/children/capture-state — язык домена,
  НЕ прячем; прячем conditions/merge-patch/retry/optimistic-lock/status-layout/ownerRef-quirks.
  Цель — правильный abstraction level (не «utility поверх k8s», не «framework black box»).
- Нейминг (ревью 4, Р15.7): публичные глаголы — `Ensure*`/`Mark*` (reconcile-intent), НЕ `Set*`/`Patch*`
  (persistence).   `EnsureChildren`/`EnsureVolumeCapture`/`EnsureManifestCapture`/`MarkPlanningReady`/
  `MarkNotReady` (Р30). (`Validate*` в facade нет — Р28: source-проверка у домена.)
- Children API (ревью 4, Р17, РЕШЕНО): **desired-set based, НЕ incremental** — домен отдаёт полный
  набор детей одним вызовом `EnsureChildren` (replace-семантика), `Add/AppendChild` в public API НЕТ.
  `Domain owns child discovery; SDK owns materialization+publication`. Publish (`EnsureChildren`) ≠
  barrier (`MarkPlanningReady`): барьер только фиксирует состояние, не меняет refs/детей. Переименование
  `MarkChildrenReady`→`MarkPlanningReady` (+`MarkPlanningFailed`); SDK прячет legacy `ChildrenSnapshotReady`.
- Go API-идиомы (ревью 4, Р15): семантика читается из exported API (package/типы/имена), не из ADR.
  Capability-split вместо жирного `SnapshotAdapter` (уточняет Р10); SDK-owned value objects/iota-enums
  вместо raw strings/`bool`/`metav1.Condition` на границе; layered API (public root semantic / `internal/*`,
  без mid-level — Р26). Приёмочный litmus: новая команда понимает `pkg/snapshotsdk` без чтения ADR.
- Форма public API (ревью 4, Р16, **РЕШЕНО**): **documented interface-facade `CaptureSDK`** —
  единая точка входа capture-протокола; НЕ free functions (допустимы только как internal impl detail)
  и НЕ session/`Planner` (нет mutable session-state → framework smell). Причина — protocol boundary, а
  не utils: единая godoc-точка (Р18), mockability, constructor-injection, сокрытие deps, стабильный
  контракт. Р16a: `RejectInvalidSource` → разведено; финал Р28 — `ValidateSource` убран; Р30 — публикация
  исхода обобщена в `MarkNotReady` (общий `Ready=False`, не только source).
- Documentation requirement (ревью 4, Р18, РЕШЕНО, нормативно): godoc — часть контракта. Каждый
  exported символ `pkg/snapshotsdk` имеет godoc (intent / protocol side effects / ownership / idempotency /
  что явно НЕ делает), без transport-механики; top-level doc capture несёт полный planning lifecycle,
  method-docs — только локальные guarantees. Запрещён пустой `// EnsureChildren ensures children.`
- Crash/restart safety (ревью 4, Р19, РЕШЕНО, инвариант): контроллер может рестартовать между любыми
  `Ensure*`/`Mark*`; `Ensure*` идемпотентны и durable-state driven (из K8s API/status, не из памяти),
  повторный reconcile сходится к тому же published state без дублей MCR/VCR/child refs. Прямая опора
  выбора desired-set (Р17) и `Ensure*`-семантики.
- Consistency-аудит (ревью 4b, устранение протечек facade):
  - Р20 (**РЕШЕНО**): `AlreadyCaptured` убран из public `CaptureSDK` — он протекал internal-маркеры
    (`manifestCaptured`/`dataCaptured`); TOCTOU-гард теперь internal внутри идемпотентных `Ensure*`.
  - Р21 (**РЕШЕНО**): split `CaptureStatus` по ownership на `DomainCaptureState` (read+write) и
    `CoreCaptureState` (только getter) — ownership enforceable by type system.
  - Р22 (**РЕШЕНО**): `ValidateSource(expect GVK)` → `ValidateSource(..., SourceValidator)`; identity-логику
    владеет домен (не хардкод GVK), консистентно с «source identity MUST NOT assume `spec.sourceRef`».
  - Р23 (**ПЕРЕСМОТРЕНО → delete-free v1**): `EnsureChildren` = **create/adopt + публикация** desired-set,
    **без удаления**. Ребёнок вне desired отвязывается от графа (выпадает из `childrenSnapshotRefs`) и
    реклеймится ownerRef GC / будущим cleanup-компонентом, не SDK. `nil`/empty = «детей нет» (публикует пустой
    список), теперь безопасно; при сбое discovery — `MarkPlanningFailed`. Убирает unstructured-delete и риск
    снести чужой объект. Отражено в godoc (Р18). (Исходно было full-reconciliation incl. orphan-deletion.)
  - Р24 (**РЕШЕНО, nice-to-have**): interface segregation — `CaptureSDK` = композиция узких ролей
    (`ReadinessFault`/`Planning`/`PlanningBarrier`); одна реализация, доменный тест мокает только нужную роль.
  - Р25 (**РЕШЕНО**): публичный flow — **restart-safe recipe, НЕ linear workflow**. Каждый `Ensure*` —
    самостоятельный idempotent checkpoint (читает durable state, сверяет desired, дообеспечивает недостающее,
    безопасен после рестарта между любыми двумя шагами). Контроллер MAY restart between calls и MUST
    пересчитать desired и выполнить recipe **сверху** заново; до `MarkPlanningReady` children/data/manifest
    реконсилируются к **новому** observed desired (converge-to-current-desired). `MarkPlanningReady` = commit point.
- Consistency-аудит (ревью 4b, facade vs layered — устранение competing-abstraction):
  - Р26 (**РЕШЕНО**): **true facade**, а не layered library. Канонический root `pkg/snapshotsdk` несёт весь
    type+protocol API; публичен только подпакет `transform/`; `children`/`status`/`ownerref`/`patch`/
    `conditions`/`storagefoundation` → `internal/*`. Снимает leaks A/B (children/status были public),
    закрывает competing-abstraction (facade + куча публичных пакетов) и проходит litmus «открыл root — всё видно».
  - Р27 (**РЕШЕНО**): реальное исполнение Р24 — методы facade берут **узкий target** (`ManifestCaptureTarget`/
    `ChildrenTarget`/`BarrierTarget`/…), а не весь `SnapshotAdapter` (иначе fat interface возвращался).
  - Р28 (**РЕШЕНО**): `ValidateSource` убран из facade (SDK сводился к `validator.Validate(SourceRef())` —
    ничего не добавлял). Identity-проверку домен делает сам, SDK оставляет только publication.
- Consistency-аудит (ревью 5, design vs фактический код — §5.1a):
  - Р29 (**НЕ ПРИМЕНЯЕТСЯ В v1 — снято Р23**): был механизмом orphan-**deletion** (diff против durable
    `status.childrenSnapshotRefs`). После delete-free Р23 SDK orphan'ов не удаляет — diff/unstructured-delete
    в v1 отсутствуют. Сохранено как обоснование на случай возврата delete в отдельный GC-компонент.
  - Р30 (**РЕШЕНО**): publication `Ready=False` обобщена — узкий `MarkInvalidSource` заменён на
    `ReadinessFault.MarkNotReady(NotReadySpec{Reason,Message,Cause,Requeue})` (покрывает source +
    artifact-missing). `MarkPlanningFailed` остаётся отдельно (барьер `ChildrenSnapshotReady=False`).
- Терминология (ревью 4b): везде `facade` (ASCII), без диакритики — проще grep/чтение в godoc/коде.

## 8. Implementation checklist S1

Короткий порядок реализации SDK v1. Цель — **no-op refactor** demo-контроллера без изменения поведения.

### S1.1 Создать публичный SDK surface
- создать модуль `pkg/snapshotsdk/go.mod` (`module github.com/deckhouse/state-snapshotter/pkg/snapshotsdk`,
  `require …/api` + `replace …/api => ../../api`) — отдельный shared-модуль (Р2);
- в `images/domain-controller/go.mod` добавить `require …/pkg/snapshotsdk` + `replace …/pkg/snapshotsdk => ../../pkg/snapshotsdk`;
- создать `pkg/snapshotsdk/doc.go` с lifecycle godoc для `CaptureSDK` (Р18: полный planning lifecycle, §0);
- добавить публичные типы:
  - `CaptureSDK`;
  - `ReadinessFault`, `Planning`, `PlanningBarrier`;
  - `SnapshotAdapter` и capability-интерфейсы (`ObjectAccessor`/`SourceProvider`/`DomainStateAccessor`/`CoreStateReader`/`ConditionWriter`);
  - narrow targets: `ManifestCaptureTarget`, `VolumeCaptureTarget`, `ChildrenTarget`, `BarrierTarget`, `ReadinessFaultTarget`;
  - `DomainCaptureState`, `CoreCaptureState`;
  - SDK-owned `Condition`, `ConditionType`, `Status`, `Reason`;
  - `NotReadySpec` (Р30: publication `Ready=False`);
  - `ChildSpec` (= `{Object client.Object}`, Р17.5: child-builder seam), `ManifestCaptureSpec`, `VolumeCaptureSpec`;
  - `VolumeCaptureProvider`, `SourceValidator` (доменный, Р22/Р28).

### S1.2 Создать internal implementation packages
- `pkg/snapshotsdk/internal/status` — status/condition patch orchestration;
- `pkg/snapshotsdk/internal/patch` — D4a merge-patch + optimistic-lock;
- `pkg/snapshotsdk/internal/conditions` — SDK condition ↔ `metav1.Condition`;
- `pkg/snapshotsdk/internal/children` — ensure child CRs, sort/dedup refs (delete-free);
- `pkg/snapshotsdk/internal/ownerref` — ownerRef helpers;
- `pkg/snapshotsdk/internal/storagefoundation` — VCR backend implementation.

### S1.3 Перенести restore extension point
- создать `pkg/snapshotsdk/transform`;
- перенести туда `Transformer`, `RestoreNode`, `NodeResult`;
- обновить импорты в `domain-controller`;
- удалить старый `images/domain-controller/pkg/domainsdk`;
- **не** добавлять shim/alias (Р14: v0, внешней совместимости нет).

### S1.4 Реализовать capture facade
- реализовать struct, удовлетворяющую `CaptureSDK`;
- constructor принимает deps: `client`, `APIReader`, scheme/rest mapper при необходимости, `VolumeCaptureProvider`;
- `EnsureChildren`: принимает полный desired-set `[]ChildSpec` (Р17.5: `ChildSpec.Object` — домен-built);
  deep-copy объекта → set parent ownerRef → create-or-adopt; деривация `SnapshotChildRef` из объекта;
  sort/dedup refs; **delete-free** (Р23) — выбывшие из desired дети не удаляются, только выпадают из refs;
  `nil`/empty = «leaf/no children» (НЕ «discovery failed»);
- `EnsureVolumeCapture`: internal TOCTOU/live-refresh; reuse existing VCR when already published; enforce single data slot;
- `EnsureManifestCapture`: internal TOCTOU/live-refresh; reuse existing MCR; derive manifest targets from volume targets when needed;
- `MarkPlanningReady`/`MarkPlanningFailed`: только publish outcome; не создают MCR/VCR; не меняют child refs;
- `MarkNotReady(NotReadySpec)`: publish `Ready=False`+reason (source/artifact), без domain validation (Р30).

### S1.5 Переписать demo на SDK
- реализовать `SnapshotAdapter` для `DemoVirtualDiskSnapshot`;
- реализовать `SnapshotAdapter` для `DemoVirtualMachineSnapshot`;
- оставить source validation в demo-домене (свой `SourceValidator`, Р28);
- заменить локальные `patch*`/`ensure*` на вызовы `sdk.Ensure*`/`sdk.Mark*`;
- убрать прямые `metav1.Condition`/condition strings из reconcile-кода;
- сохранить доменную логику: выбор детей + **построение child-объектов** (`DemoVirtualDiskSnapshot`) для
  `ChildSpec.Object` (Р17.5), и резолв единственного PVC (`DataRef *Target`) для `VolumeCaptureSpec`.

### S1.6 Тесты первыми
1. `internal/conditions`: merge condition без потери чужих условий.
2. `internal/patch`: D4a ownership — свой writer не затирает чужие поля.
3. `internal/children`: sort/dedup refs.
4. `internal/children`: `EnsureChildren([A,C])` после `[A,B]` — refs → `[A,C]`; `B` **detached** (выпал из
    refs), но в кластере **не удалён** (delete-free, Р23).
4a. `internal/children`: builder seam (Р17.5) — из `ChildSpec.Object` SDK ставит parent ownerRef,
    create-or-adopt, деривирует `SnapshotChildRef`; объект-template не мутируется у caller (deep-copy).
5. `internal/children`: `EnsureChildren(nil)` публикует пустой список refs; прежние SDK-owned children
    **не удаляются** (delete-free, Р23).
6. `snapshotsdk`: crash/restart recipe (Р25) — повтор `Ensure*` после partial progress не создаёт дублей.
7. `snapshotsdk`: desired changed before `MarkPlanningReady` — `[A,B]` → restart → `[A,C]` сходится к `[A,C]`.
8. `snapshotsdk`: content-free guard — facade не требует и не читает `SnapshotContent`.
9. `snapshotsdk` (Р30): `MarkNotReady` — source invalid → `Ready=False` (no requeue); artifact-missing →
   `Ready=False` + `Requeue=true` intent; planning failed → `ChildrenSnapshotReady=False` (`MarkPlanningFailed`).
10. demo envtest/e2e — поведение до/после SDK совпадает (no-op refactor).

### S1.7 Definition of Done
- demo-контроллер использует только root `pkg/snapshotsdk` и `pkg/snapshotsdk/transform`;
- публичных `children`/`status`/`patch`/`conditions`/`ownerref`/`volumecapture` подпакетов нет (Р26);
- все exported symbols в `pkg/snapshotsdk` имеют godoc (Р18);
- старый `domainsdk` удалён;
- существующие demo tests зелёные;
- добавлены conformance-тесты: D4a ownership, content-free, deterministic refs, TOCTOU/restart, delete-free children.

## 9. Риски / заметки
- Форма public API (Р16/Р26, **РЕШЕНО**): documented interface-facade `CaptureSDK` в **каноническом root**
  `pkg/snapshotsdk` (true facade, не layered). Деталь реализации (deps в конструкторе, internal free-функции) — на S1 PoC.
- Связность с `api/` неизбежна (контрактные типы) — приемлемо в рамках одного репо/модуля.
- Ключевая ценность SDK — защитить **content-free инвариант**; в library-first это гарантируется
  тем, что capture-хелперы вообще не принимают доступ к `SnapshotContent`.
- Открытый микро-вопрос (S1): внутри `DomainCaptureState` — агрегат vs explicit per-field accessors в
  `SnapshotAdapter` (Р10/Р21); и точный набор ролей в каждом target (Р27) — выбрать при реализации.
- По правилам репозитория (delivery-gating) — оформить как явный набор самодостаточных коммитов
  с зелёными build/test и ревью; обновить `operations/project-status.md` при необходимости.
