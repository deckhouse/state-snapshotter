# План доработок (roadmap)

Детальный план работ и таблицы статусов. Высокоуровневый прогресс — в [`operations/project-status.md`](../operations/project-status.md). Обзор линий продукта и ссылки на runbook — [`../README.md`](../README.md).

**Продуктовое ТЗ (SSOT сценария):** [`snapshot-rework/`](../../../snapshot-rework/) — контрактные примеры и потоки **не правятся** из `docs/` в пользу «упрощённого MVP»; план ниже только задаёт поставку кода под ТЗ.

**Техдизайн R2 2b / R3 (runtime registry, diff, additive watch):** [`r2-phase-2b-r3-runtime-registry.md`](r2-phase-2b-r3-runtime-registry.md).

**Сверка с удалённым указателем `snapshot-rework/plan/dorabotki-i-testy.md`:** таблица § → файлы — в [`snapshot-rework/README.md`](../../../snapshot-rework/README.md).

---

## 2. План доработок

### 2.1 Baseline: устойчивый процесс (обязательно первым)

**Отсутствие CRD в кластере не должно валить процесс.** Пустой реестр unified-типов — **нормальный режим**, а не «деградация безымянная».

| # | Задача | Зачем | Статус |
|---|--------|--------|--------|
| S1 | На старте: **discovery** — учитывать только реально существующие в API GVK (из bootstrap-конфига до DSC, позже из DSC) | Operational hygiene | ✅ сделано |
| S2 | Логировать предупреждение / событие и **не** вешать watch на отсутствующие типы | Нет CrashLoop из-за частично выключенных модулей | ✅ сделано |

**Как сделано (S1–S2):**

- Пакет `images/state-snapshotter-controller/pkg/unifiedbootstrap/`: `DefaultDesiredUnifiedSnapshotPairs()`, `ResolveAvailableUnifiedGVKPairs(mapper, pairs, log)`.
- `cmd/main.go`: resolve после `manager.New`; при нуле типов — сообщение в logrus.
- **Динамика после старта:** новые eligible типы из DSC подхватываются **без рестарта** через `pkg/unifiedruntime.Syncer.Sync` (R2 2b/R3). **Снятие** watch при выпадении типа из resolved — по-прежнему не гарантируется; см. gauges `state_snapshotter_unified_runtime_*` и лог при stale.

Опционально (не сделано): **feature gate** в values для всего unified трека.

### 2.2 Реестр типов (после S1–S2)

| # | Задача | Зачем | Статус |
|---|--------|--------|--------|
| R1 | **`DomainSpecificSnapshotController`:** типы `api/v1alpha1`, CRD `crds/state-snapshotter.deckhouse.io_domainspecificsnapshotcontrollers.yaml`, регистрация схемы | Единый контракт API | ✅ |
| R2 | **DSC reconciler** в manager + пересчёт статусов; **phase 1** — см. блок ниже; **phase 2a** — merge на старте; **phase 2b** — `pkg/unifiedruntime.Sync` после reconcile DSC: additive `mgr.Add` для новых пар | Замена статического bootstrap как единственного источника пар GVK | ✅ phase 1 / 2a / 2b *(additive add; без clean unwatch)* |
| R3 | **Runtime (без рестарта pod):** подписка по формуле `Accepted`+`RBACReady`+поколения; `Ready` не в предикате; **отписка не гарантируется** | Нет обязательности watch на stale тип; новые eligible — без рестарта | ✅ *(additive + layered state + proof-тест + gauges/log stale↔resolved; symmetric unwatch — ⬜)* |
| R4 | Конфликт kind между DSC → `Accepted=False (KindConflict)`; процесс не падает | Fail-closed на уровне DSC | ✅ *(reconciler; см. `domainspecificsnapshot_controller.go`)* |
| R5 | Опциональный bootstrap-список GVK + отключение unified wiring (env / Helm values) | Rollout | ✅ см. `pkg/config` (`STATE_SNAPSHOTTER_*`), `openapi/config-values.yaml`, `templates/controller/deployment.yaml` |

**R2 phase 1 — сделано (status-only, без runtime activation):**

- [x] `internal/controllers/domainspecificsnapshot_controller.go`: на каждый reconcile — полный `List` DSC, resolve CRD-имён через `CustomResourceDefinition`, инвариант **cluster-scoped content**, дубликат snapshot kind в одном DSC → `InvalidSpec`, cross-DSC → `KindConflict`.
- [x] Статусы **`Accepted`**, производный **`Ready`** по ADR; **`RBACReady`** не пишет контроллер (копия из объекта).
- [x] `RetryOnConflict`: внутри retry повторный `List` + пересчёт global state (актуальный spec).
- [x] Подключение в `cmd/main.go`; RBAC: `templates/controller/rbac-for-us.yaml` (DSC + status + read CRD).
- [x] `hack/generate_code.sh`: как в модуле `backup` — пин `controller-gen` v0.18.0, вызов из `$(go env GOPATH)/bin/controller-gen`.
- [x] **Phase 2a (только на старте процесса):** `pkg/dscregistry` — eligible DSC → пары GVK; `unifiedbootstrap.MergeBootstrapAndDSCPairs`; в `cmd/main.go` после `manager.New` — прямой client + `ResolveAvailableUnifiedGVKPairs` на merged списке. Ошибка `List` DSC → fallback на bootstrap-only + warning в logrus.
- [x] **Phase 2b (additive):** `pkg/unifiedruntime.Syncer` после успешного `reconcileAll` DSC: merge + `ResolveAvailableUnifiedGVKPairs` → `SnapshotController.AddWatchForPair` / `SnapshotContentController.AddWatchForContent` (`mgr.Add` после `Start` поддерживается controller-runtime). Идемпотентность по GVK; один сбой add не валит остальные пары.
- [x] **R3 (часть 1 — state + proof):** слой **bootstrap / eligible / merged / resolved** в `pkg/unifiedruntime.LayeredGVKState` + `BuildLayeredGVKState`; **active** — `Syncer.activeSnapshotGVKKeys` (монотонно: ключ попадает, если оба `AddWatch*` успешны); `LastLayeredState()` / `ActiveSnapshotGVKKeys()` для отладки и тестов; unit — `pkg/unifiedruntime/layers_test.go`. Интеграция: `test/integration/unified_runtime_hot_add_test.go` — DSC становится watch-eligible (Accepted → RBACReady), затем проверяются `LastLayeredState` (resolved + eligible) и `ActiveSnapshotGVKKeys`; тест **Serial**, маппинг на **RegistrationTestSnapshot** (не `TestSnapshot`), чтобы не вешать глобальный watch на тип, с которым lifecycle-спеки делают прямой `Reconcile` (иначе два reconcile-потока и 409). В `BeforeSuite` интеграции — wiring как в production: unified controllers + `unifiedruntime.NewSyncer` + `AddDomainSpecificSnapshotControllerToManager(..., syncer.Sync, graphRegistryRefresh)`.
- [x] **R3 (observability):** после каждого `Sync` обновляются Prometheus gauges (`sigs.k8s.io/controller-runtime/pkg/metrics`): `state_snapshotter_unified_runtime_resolved_snapshot_gvk_count`, `active_monotonic_snapshot_gvk_count`, `stale_active_snapshot_gvk_count`; сводка на `V(2)`; при `stale_active_snapshot_gvk_count > 0` — **Info**-лог со списком ключей и явным hint про restart pod (additive watches не снимаются). Регистрация метрик — `sync.Once` в `NewSyncer`. См. [`r2-phase-2b-r3-runtime-registry.md`](r2-phase-2b-r3-runtime-registry.md).
- [x] **R5:** `config.Options` + env (`STATE_SNAPSHOTTER_UNIFIED_ENABLED`, `STATE_SNAPSHOTTER_UNIFIED_BOOTSTRAP_PAIRS`); `cmd/main.go` ветка без unified; `NewSyncer` получает `EffectiveUnifiedBootstrapPairs()`; Helm/OpenAPI. Ошибка парсинга bootstrap → warning + дефолтный список.
- [ ] **R3 / integration (опционально):** два DSC при поломке одного, полный T5/T9 и т.д.

### 2.3 Manifest capture

| # | Задача | Зачем | Статус |
|---|--------|--------|--------|
| M1 | Расширение **MCR spec** | UX | ⬜ **отложено** до стабилизации **NamespaceSnapshot / NamespaceSnapshotContent / ObjectKeeper** по поставке **N2a** (и при необходимости N3); не смешивать с закрытием **N2b** без явного gate |
| M2 | Лимиты объёма, таймауты list | Защита apiserver/etcd | ⬜ **после M1** (тот же gate) |

### 2.4 Namespace snapshot + NamespaceSnapshotContent + ObjectKeeper

**Цель:** сразу целевая схема **без миграции** с промежуточного generic `SnapshotContent` для корня namespace — см. [`decisions/namespace-snapshot-content-decision.md`](decisions/namespace-snapshot-content-decision.md). Детали сценария — **только** [`snapshot-rework/`](../../../snapshot-rework/). Статус **`NamespaceSnapshot`**: **только `conditions`**, без `status.phase` — [`decisions/namespace-snapshot-status-surface.md`](decisions/namespace-snapshot-status-surface.md).

| # | Задача | Документ / примечание | Статус |
|---|--------|------------------------|--------|
| N0 | **Gate:** **Chosen option** в [`namespace-snapshot-scope.md`](decisions/namespace-snapshot-scope.md) ≠ TBD. Сверка **apiVersion/group** для `NamespaceSnapshot` / `NamespaceSnapshotContent` между ТЗ в `snapshot-rework` и фактическими CRD в репозитории (привести к одному). | [`namespace-snapshot-controller.md`](namespace-snapshot-controller.md) §13–§16 | ✅ (scope resolved; group `storage.deckhouse.io/v1alpha1` на этапе N1) |
| N1 | **API + lifecycle skeleton (завершённый подготовительный слой, код не откатывается):** типы `NamespaceSnapshot`, `NamespaceSnapshotContent`, codegen, OpenAPI; **убрать** generic `SnapshotContent` как носитель root; bind + delete; integration (lifecycle, deletion, mismatch, recovery). **В N1 намеренно нет:** ObjectKeeper, реального manifest capture, дочернего дерева. | decision + design §14–§16 | ✅ |
| N2 | **Manifests-only snapshot path** (без data-layer), в два подэтапа — **§2.4.1:** **N2a** — первый рабочий снимок манифестов одного root (OK + MCR→ManifestCheckpoint + статус на NSC + download одного снимка); **N2b** — дерево манифест-only снимков (дети, refs на graph, агрегированный Ready, aggregated download). | [`namespace-snapshot-controller.md`](namespace-snapshot-controller.md) + §2.4.1 | ⬜ |
| N3 | **Интеграция / hardening:** envtest — recovery после **рестарта** контроллера; доп. негативные кейсы; политика по §15. Базовые mismatch/recovery/status уже в N1 (`namespacesnapshot_n1_boundary_test.go`). | design §15 | ⬜ |
| N4 | **После закрытого N2 (N2a+N2b):** углублённые лимиты большого namespace, политики таймаутов list/apiserver (пересечение с §8.6 design, M2). | design §8.6, §16 | ⬜ |
| N5 | **За пределами manifests-only дерева:** data-layer (volume/VSC/VCR и т.д.), DSC priority traversal в полном объёме, экспорт/импорт/restore с данными — итерациями по [`snapshot-rework/2026-01-25-namespace-snapshot.md`](../../../snapshot-rework/2026-01-25-namespace-snapshot.md) без изменения ТЗ из `docs/`. Дочерняя **композиция манифест-only** — в **N2b**, не в N5. | бэклог | ⬜ |

Трек **N*** и **M1–M2** не смешивать в одном PR без необходимости.

#### 2.4.1 N2 — manifests-only путь (N2a / N2b), SSOT декомпозиции

**Граница N1 ↔ N2 (код N1 не пересматривается как «неудачная работа»):** **N1** — **завершённый** слой **API + lifecycle skeleton**: CRD/типы, bind root↔`NamespaceSnapshotContent`, delete (Retain/Delete), integration (lifecycle, mismatch, recovery). **В N1 намеренно нет:** **ObjectKeeper**, **реального** manifest capture, **дочернего дерева**. Дальнейшие этапы опираются на этот скелет без отката CRD/bind/delete.

**Зафиксированные договорённости для N2:**

- **ObjectKeeper** нужен уже в **первом рабочем** проходе (**N2a**), как **retention anchor** для корневого content/артефакта; OK **не** заменяет bind (**`spec.namespaceSnapshotRef`** / **`status.boundSnapshotContentName`**).
- Рабочий scope до data-layer — **manifests-only**; **VolumeCaptureRequest**, **VolumeSnapshotContent**, dataRefs и прочие data-ветки — **не реализуются** в N2; в API допускаются только **placeholders**, если они уже заложены в модели.
- **Внутренний** путь исполнения manifest capture — **ManifestCaptureRequest → ManifestCheckpoint** (+ существующие **ManifestCheckpointContentChunk** в коде модуля); публичный lifecycle и статусы — **`NamespaceSnapshot` / `NamespaceSnapshotContent`** (+ при необходимости те же поля связи, что у unified content с MCP, по аналогии со `SnapshotContent`).
- **Дерево snapshot-ов и child composition** — **целевая ось продукта**, закрывается в **N2b** (manifests-only), а не откладывается как «дальний optional».

**Цель N2 целиком:** кратчайший путь к **первым рабочим** снимкам манифестов (**N2a**), затем к **рабочему дереву** manifests-only снимков (**N2b**). Полный vision из [`snapshot-rework/2026-01-25-namespace-snapshot.md`](../../../snapshot-rework/2026-01-25-namespace-snapshot.md) **не** тащить в один этап.

**Вне N2 (явный out-of-scope для всего N2a+N2b):** volume/data snapshots; реальный поток **VCR/VSC**; **restore с данными**; полный **export/import** продукта; **storage class remap**; **VM data restore**; выдача поддерева **с data payloads** (агрегированный download в N2b — **только манифесты**).

---

**N2a — первый рабочий manifests-only snapshot (один root, без дерева)**

**Definition of Done (N2a):**

1. **Два ObjectKeeper не смешивать:** корневой OK (**`ret-nssnap-…`**: **`FollowObjectWithTTL`** на **`NamespaceSnapshot`**, `spec.ttl` из env или дефолта в `pkg/config`) + **root `NamespaceSnapshotContent.metadata.ownerReferences` → этот OK** (TTL-якорь; не наоборот) — отдельно от **generic** execution OK **`ret-mcr-*`** (**`FollowObject`** на MCR) в `ManifestCheckpointController`. Для **namespace N2a** финальный **MCP** крепится к **NSC** (`ownerReference`), **без** `ret-mcr-*` для MCP. Chunks → **ownerRef на MCP**. Детали — [`namespace-snapshot-controller.md`](namespace-snapshot-controller.md) §4.3 / §4.6.  
2. Реальный manifest capture через цепочку **MCR → ManifestCheckpoint** (chunks), управляемый из потока **NamespaceSnapshot** (ensure временного MCR с **ownerRef** на root snapshot для GC in-flight, observe MCP; после успешного persisted результата — **удаление MCR**, §4.7 design; без публичной обязанности MCR для оператора — §10).  
3. Запись результата в **`NamespaceSnapshotContent.status`** по **§4.4** design (как минимум **`manifestCheckpointName`**, conditions; опционально `capturedAt`, `resourceCount`).  
4. **`Ready=True`** на root **только** после **persisted** manifest-результата (MCP Ready + консистентные chunks / статус MCP), **не** из «промежуточного» события вроде одного лишь факта создания MCR без готового checkpoint.  
5. Рабочий **read/download path** манифестов **одного** снимка (на базе существующей склейки chunks / archive path в модуле, без обязательности нового формата хранения).  
6. **Без** агрегации детей и **без** data-flow; поля data-related — только **placeholders**, если уже есть в CRD.

**Design lock до кода N2a (подробности в design, не дублировать здесь):**

- Публичный **status surface** N2a — [`namespace-snapshot-controller.md`](namespace-snapshot-controller.md) **§4.4** (root без MCR в status; NSC — `manifestCheckpointName` + conditions + опциональные счётчики/время).  
- **Allowlist / exclusions** — **§4.5**.  
- **Download** (один снимок, без предматериализации) — **§8.7**.  
- **N2b агрегация Ready** — **§11.1**.  
- **Контроллеры** (NS vs `ManifestCheckpointController`) — **§10**.  
- **OK vs ownerRef** — **§4.3**.

**N2a.x:** выполнено — см. [`namespace-snapshot-controller.md`](namespace-snapshot-controller.md) **§4.6** (сверка с `ManifestCheckpointController`).

**Порядок работ N2a (ориентир):** (0) прочитать design lock + §4.6–§4.7; (1) CRD **`manifestCheckpointName`** на NSC (§4.4.1); (2) allowlist §4.5 в коде; (3) NS reconciler: NSC, **root OK**, MCR по §4.7, observe MCP, статусы NS/NSC, Ready, **delete MCR** после success; (4) download §8.7.1; (5) integration.

**Нормативно:** набор GVR — **§4.5** + один SSOT в коде; ad-hoc «снять всё подряд» **запрещён**.

**Known N2a limits (не маскировать как «готово»):** фактическое срабатывание TTL retained NSC — политика Deckhouse ObjectKeeper controller; политика **удаления root при незавершённом capture** и явный cancel MCP — [`namespace-snapshot-controller.md`](namespace-snapshot-controller.md) §5.2; list targets без pagination — только N2a, hardening позже (§4.5 / §4.3.2 design).

**Хранилище манифестов N2a:** **ManifestCheckpoint + gzip/json chunks**; выдача — склейка на читании (см. `ArchiveService`, §8.7). Отдельный заранее материализованный **`bundle.tar.gz`** **не обязателен** для N2a.

---

**N2b — дерево manifests-only snapshot-ов**

**Definition of Done (N2b):**

1. Создание **дочерних** snapshot **доменными контроллерами** (по ТЗ), дочерние **`NamespaceSnapshotContent`**.  
2. На **`NamespaceSnapshot`**: **`childrenSnapshotRefs`** — **observability** / намерения.  
3. На **`NamespaceSnapshotContent`**: **`childrenSnapshotContentRefs`** — **канонический graph** результата.  
4. **`Ready=True`** у parent — по политике **[`namespace-snapshot-controller.md`](namespace-snapshot-controller.md) §11.1** (собственный result + required children; child failed → parent `Ready=False` / `ChildSnapshotFailed`).  
5. **Aggregated manifests download** для **subtree / root** (только манифесты, без data payloads; на чтении из MCP/chunks — §8.7 design).  
6. По-прежнему **без** data-flow (volume и т.д.).

#### 2.4.2 N2b — поставка короткими PR (инвариант на PR)

Цель: **не** смешивать форму графа в API, wiring parent/child, политику **Ready**, read-path aggregated download и доменный traversal в один коммит. Каждый PR замыкает **один** новый инвариант; после каждого — зелёные тесты и понятный критерий остановки.

| PR | Фокус | Включить | Не включать (отложить) | Критерий остановки |
|----|--------|----------|-------------------------|-------------------|
| **PR1** | Только **форма графа** в API | JSON: `childrenSnapshotRefs` / `childrenSnapshotContentRefs`; в Go элементы — **`NamespaceSnapshotChildRef`** / **`NamespaceSnapshotContentChildRef`** (узкие имена, не путать с полем **`snapshotRef`** у generic **`SnapshotContent.spec`**). Обновление **design** и при необходимости **spec**; unit / envtest на **сериализацию** | Aggregated download; полный parent **Ready**; несколько типов детей; domain traversal | «API графа стабилен, дерево в поведении ещё не оживлено» |
| **PR2** | **Один** synthetic child **end-to-end** (**временный scaffold в коде, не целевая модель PR5**) | **Реализация:** opt-in аннотация **`n2b-pr2-synthetic-tree`** на parent; имя child = **`parentName + "-child"`**; label **`n2b-synthetic-child`** (без рекурсии); parent пишет graph refs; parent **Ready** только после child **Ready**; интеграция **`namespacesnapshot_synthetic_tree_test.go`**. **Временный scaffold** до замены heterogeneous flow и **снятия** по правилу **§2.4.2** (не доменный продуктовый flow). | Subtree download; несколько детей; domain wiring | «Дерево на **одном** ребёнке работает» |
| **PR3** | **Политика агрегации Ready** отдельно (**временный scaffold в коде, не целевая модель PR5** — тот же слой, что PR2) | **Реализовано:** helper **`evaluateSyntheticRequiredChildState`**; parent **`ChildSnapshotPending`** vs **`ChildSnapshotFailed`** vs успех **`Completed`**; allowlist терминальных N2a-причин у child; **§11.1** design (таблица); integration **`namespacesnapshot_synthetic_tree_test.go`** (явные reason на pending/success) + **`namespacesnapshot_synthetic_child_failure_test.go`** (child terminal fail → parent **`ChildSnapshotFailed`**). | Aggregated download (PR4); несколько детей; optional/required; domain wiring | «Матрица success / pending / failed зафиксирована тестами» |
| **PR4** | **Aggregated manifests download** (без data) | **SSOT:** [`spec/namespace-snapshot-aggregated-manifests-pr4.md`](../spec/namespace-snapshot-aggregated-manifests-pr4.md) — endpoint, read-path по **сохранённому** NSC-графу ([`spec/system-spec.md`](../spec/system-spec.md) **§3.0** ст. 2), fail-whole, merge, циклы/дубликаты. Integration: parent + 1 child, затем parent + 2 children | Data payloads; export/import; restore | «N2b manifests-only **для пользователя** замкнут на чтении subtree». **Real cluster:** `hack/pr4-smoke.sh` (без skip OK) — aggregated до/после удаления root snapshot, retained read, шаг 5b (root OK: followRef→`NamespaceSnapshot`, **NSC ownerRef→OK**). **Strict TTL** (`PR4_SMOKE_REQUIRE_TTL=1`) — на кластере с рабочим Deckhouse ObjectKeeper; TTL снимка задаётся контроллером (`FollowObjectWithTTL` + `spec.ttl`, env/дефолт); см. [`testing/e2e-testing-strategy.md`](../testing/e2e-testing-strategy.md). |
| **PR5** | Первый **реальный** domain wiring | **Heterogeneous** дерево на базе **общей** snapshot-модели (целевая модель PR5 — **без** synthetic; **временный scaffold в коде** — только до **cleanup** по правилу ниже): **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`** для **любых** дочерних kinds, единый **`Ready`** (каскад), **dedup вычисляемый** из API; **без** **вложенного** **`NamespaceSnapshot`** под root (**INV-T1**), **без** отдельных `domainChild*` / persisted coverage / `SubtreeReady`. Детали — [`design/demo-domain-dsc/08-universal-snapshot-tree-model.md`](demo-domain-dsc/08-universal-snapshot-tree-model.md). **Имена полей дерева не множатся:** расширяется **семантика элементов** существующих **`children*Refs`** под heterogeneous children; PR4 read-path (§3.0 ст. 2) — в spec вместе с кодом. **Включить снятие** временного synthetic scaffold по правилу ниже (**в этом PR5 или следующем cleanup PR**). | До PR5 не начинать, пока **PR1–PR4** не зелёные — иначе неясно, баг в графе, агрегации или домене | «Один реальный доменный сценарий на базе стабильного N2b-скелета»; **synthetic scaffold снят** с кода и тестов по правилу снятия scaffold (этот PR или немедленно следующий cleanup PR) |

**Рекомендуемый порядок:** **PR1 → PR2 → PR3 → PR4**; **PR5** — после стабильности PR1–PR4.

**Целевая архитектура vs текущий код.** (1) В **целевой модели** PR5 / demo-domain **synthetic не фигурирует** — только **heterogeneous** flow ([`demo-domain-dsc/README.md`](demo-domain-dsc/README.md)). (2) **PR2–PR3** в таблице выше и аннотация **`n2b-pr2-synthetic-tree`** / synthetic integration tests / helper’ы в коде — **временный scaffold в коде, не целевая модель PR5** (граф и матрица **Ready**, пока их не заменил реальный flow); это **не** часть архитектурного narrative demo-domain (см. тот же README).

**Снятие synthetic scaffold (обязательно):** как только **одновременно** верно: **(i)** heterogeneous (PR5 / demo-domain) flow покрывает **тот же контракт**, который раньше доказывал synthetic; **(ii)** integration и **merge-gate** на demo-domain flow переведены на этот контракт и зелёные — scaffold **удаляется** из кода и тестов **в том же PR5 или немедленно следующим cleanup PR** (без паузы и без «полумёртвого» legacy при уже ушедшей целевой архитектуре).

After merge-gate on demo-domain flow, synthetic scaffold must be removed from code and tests in the same PR or immediately following cleanup PR.

**Первый минимальный вход в N2b:** только **PR1** (поля графа + docs/spec + тесты сериализации; **без** изменения семантики **Ready** N2a-leaf и без orchestration).

#### 2.4.3 Demo domain (virtualization-shaped) через DSC — **Proposed**

Целевой референс в пакете demo-domain — **heterogeneous** дерево на **универсальной** модели **[`08-universal-snapshot-tree-model.md`](demo-domain-dsc/08-universal-snapshot-tree-model.md)** — те же **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`**, один **`Ready`**, dedup **только вычисление**; **не** отдельный namespace-only graph API, **не** `domainChild*`, **не** persisted `domainCoverage`, **не** `SubtreeReady`. (Synthetic в narrative здесь **не** фигурирует — только **временный scaffold в коде**, см. таблицу **§2.4.2** выше.) Подробности demo v1: [`demo-domain-dsc/README.md`](demo-domain-dsc/README.md).

- Пакет: **[`design/demo-domain-dsc/README.md`](demo-domain-dsc/README.md)** + [`testing/demo-domain-dsc-test-plan.md`](../testing/demo-domain-dsc-test-plan.md).
- Фиксация: [`05`](demo-domain-dsc/05-tree-and-graph-invariants.md), [`06`](demo-domain-dsc/06-coverage-dedup-keys.md), [`07`](demo-domain-dsc/07-ready-delete-matrix.md), **[`08`](demo-domain-dsc/08-universal-snapshot-tree-model.md)**.
- **Тесты (после кода):** [`testing/demo-domain-dsc-test-plan.md`](../testing/demo-domain-dsc-test-plan.md); уровни — [`testing/e2e-testing-strategy.md`](../testing/e2e-testing-strategy.md) (раздел Demo domain).
- **Нормативный каркас PR5+** (логическое дерево по **`children*Refs`**, **INV-REF1** / **INV-REF-C1** / **INV-REF-M1** / **INV-REF-M2**, **INV-S0** / **INV-E1**, запрет DSC/ownerRef как SoT дерева/dedup) — **[`spec/system-spec.md`](../spec/system-spec.md) §3** (две стадии — **§3.0**). Расширение **формы элементов** `children*Refs` (GVK+…) и полная фиксация PR4 read-path по NSC в OpenAPI+коде — вместе с реализацией PR5; мотивы, таблицы **`Ready`**/delete — [`demo-domain-dsc/`](demo-domain-dsc/README.md).

#### 2.4.4 Порядок имплементации **[`spec/system-spec.md`](../spec/system-spec.md) §3** (execution plan)

Короткая **нарезка атомарных PR** под контракт **§3**: **что** делать — только в spec; **как** и матрицы — в design / test-plan. Таблица **§2.4.2** выше — поставка N2b по полю API (**PR1**), **временному scaffold в коде** (**PR2–PR3**, до **снятия synthetic** по правилу сразу после **merge-gate** на demo-domain flow), aggregated (**PR4**), heterogeneous gate (**PR5**); подраздел ниже — **порядок работ внутри линии generic + граф + dedup + Ready** (может частично пересекаться уже закрытыми N2b PR — тогда этап помечается в PR как выполненный / no-op). **§3-E1** опирается на **факты кода** (в т.ч. временный scaffold), пока он есть; **целевой результат PR5** — **без** synthetic.

**Граница §3-E* vs PR5 (demo-domain).** Срезы **§3-E1…E6** ниже — **подготовка generic-инфраструктуры** в модуле (refs, merge, read-path по **content refs** (§3.0 ст. 2), dedup, **`Ready`**) поверх нормативного **[`spec/system-spec.md`](../spec/system-spec.md) §3**; они **не** являются этапом «написать демо-доменный контроллер». Первый реальный **domain consumer** контракта — **PR5** ([`demo-domain-dsc/README.md`](demo-domain-dsc/README.md)): DSC, **Demo*Snapshot** / **Content**, запись **`children*Refs`** (§3.0 ст. 1), прохождение generic **Ready** / dedup / обход сохранённого графа и **снятие** synthetic scaffold по правилу таблицы **§2.4.2**. Минимальный spike одного kind **возможен** уже после **E1/E2**; **практически комфортная** точка входа в PR5 — после **E3/E4**, когда не «плавает» слой read-path / **content refs**. Рекомендуемая нарезка внутри PR5: **PR5a** — один доменный kind (например **DemoVirtualDiskSnapshot**), короткий proof (один DSC, один child path, refs); **PR5b** — при необходимости второй kind и вложенность (например **DemoVirtualMachineSnapshot** → Disk) для проверки промежуточного узла.

**Практичный переход (не ждать «идеального» E1–E6).** Не обязательно закрывать все срезы **E1–E6** до конца перед доменом; разумная очередность:
1. **E3/E4 до рабочего минимума** — зафиксировать поведение вокруг **`childrenSnapshotContentRefs`**; дожать read-path / aggregation (§3.0 ст. 2) настолько, чтобы **domain consumer** не упирался в неясность, **как generic читает сохранённый граф**; **без** лишнего раздувания матрицы **`Ready`** в этих же PR (широкий **Ready** — **E6** / отдельные изменения).
2. **PR5a** — один реальный demo kind (**DemoVirtualDiskSnapshot**), один **DSC**, один простой child-path, запись **`children*Refs`**; проверить, что generic читает граф **без** synthetic-магии.
3. **PR5b** — **DemoVirtualMachineSnapshot** (и при необходимости цепочка VM → Disk), промежуточный узел и каскад **`Ready`**; **снятие** synthetic scaffold — **после** merge-gate на demo-domain flow, по правилу **§2.4.2** (тот же **PR5** или немедленно следующий cleanup PR).

**§3-E1 — базовый graph (write + read)**  
Запись **`childrenSnapshotRefs`** с **merge-only** (**INV-REF-M1** / **INV-REF-M2**); generic читает только refs (**INV-REF1**, без list-достройки); в этом срезе **не** опираться на **`childrenSnapshotContentRefs`** для обхода; **без** dedup. **Тест:** один child, happy-path.

**§3-E2 — multi-writer / merge correctness**  
Несколько writers на refs; **RetryOnConflict** / согласованная стратегия patch. **Тесты:** concurrent writers; **нельзя** удалить чужой ref (**INV-REF-M2**).

**§3-E3 — content refs (частично)**  
Использование **`childrenSnapshotContentRefs`** там, где нужно по spec traversal; generic: **без** list-обхода (**INV-REF-C1**); при выбранном варианте — **явный fallback** только по цепочке **snapshot refs**. **Тест:** отсутствие / пустые content refs → **fail-closed** или задокументированный fallback.

**§3-E4 — traversal / aggregation (если выносится отдельным PR)**  
Обход дерева **по refs**; подготовка к aggregated операциям (download / restore по политике N2b); **без** расширения матрицы **Ready** в том же PR (если иначе — разнести). Общий DFS по **`childrenSnapshotContentRefs`** (сортировка детей, циклы) — в коде **`usecase.WalkNamespaceSnapshotContentSubtree`** (только узлы **`NamespaceSnapshotContent`**) и **`usecase.WalkNamespaceSnapshotContentSubtreeWithRegistry`** (heterogeneous: зарегистрированные **`XxxxSnapshotContent`** под теми же refs, **`unstructured`**, без импорта доменных CRD в generic). Агрегатор PR4 и интеграции PR5a/PR5b используют **один** ref-only walk; листья dedicated content **без** MCP на aggregated path — как в **`namespacesnapshot_content_graph.go`**.

**§3-E5 — dedup / exclude**  
**INV-S0** / **INV-E1**: вычисление только по дереву текущего run; **fail-closed** при неполных данных. **В коде (root capture):** `usecase.BuildRootNamespaceManifestCaptureTargets` + `collectRunSubtreeManifestExcludeKeys` (обход **`childrenSnapshotContentRefs`** + dedicated content через registry; **без** list subtree); **`ResolveChildSnapshotToBoundContentName`** по **`GVKRegistry`**; **`namespacemanifest.FilterManifestTargets`**; wiring в **`namespacesnapshot_capture.go`** + **`ArchiveService`** на **`NamespaceSnapshotReconciler`**. **Ограничения среза:** при пустых **`childrenSnapshotRefs`** exclude не применяется (как раньше — полный namespace list); при непустых **`childrenSnapshotRefs`** registry **обязателен** (иначе fail-closed). **Живой registry:** `pkg/snapshotgraphregistry` + refresh после reconcile DSC (не startup-static снимок). **Finish line (текущая поставка):** integration proof в **`namespacesnapshot_synthetic_tree_test.go`** (root **MCR** не должен содержать **ConfigMap**, уже попавший в child **MCP**); при **CapturePlanDrift** и **непустых `status.childrenSnapshotRefs`** контроллер **удаляет** устаревший **MCR** и **requeue** (гонка первого MCR vs exclude); иначе drift остаётся **terminal** (**N2a**). Тесты: **`snapshot_graph_registry_dynamic_dsc_test.go`**, ambiguous / heterogeneous без registry — unit.

**Finish line §3-E5 (инженерно честный, рекомендуемый gate перед E6):** (1) **integration proof** — см. выше; (2) **детерминированный первый MCR** — см. выше (ветка только при subtree refs); (3) негативные ветки registry/графа — расширяются по мере необходимости. **§3-E6** — отдельный этап после закрепления read-path; актуальный статус — [`operations/project-status.md`](../operations/project-status.md).

**§3-E6 — Ready (когда нормы перенесены в spec / закреплены тестами)**  
Единый контракт чтения состояния дочернего узла; каскад **Ready**; **INV-R4** / согласованный выбор **reason** на родителе (см. [`demo-domain-dsc/07-ready-delete-matrix.md`](demo-domain-dsc/07-ready-delete-matrix.md)) — без дублирования таблиц здесь. **Рекомендуемый порядок:** после finish line **§3-E5** выше.

**Фактический прогресс срезов §3-E в коде** (объём «сделано / не сделано» без повторения таблиц Must) — в [`operations/project-status.md`](../operations/project-status.md) (строка таблицы N2b generic §3 и блок под ней).

---

**Definition of Done (N2 целиком = N2a ∧ N2b)**

Выполнены DoD **N2a** и **N2b**; out-of-scope выше **не** смешан с закрытием N2 без явного расширения этапа.

---

**Практический task-list (копипаст backlog)**

**N2a:** CRD §4.4.1 + allowlist §4.5 + §4.7 → NS reconciler → download §8.7.1 → integration.  

**N2b:** по шагам **§2.4.2** (таблица PR в подразделе **2.4.2** выше в этом документе; PR1→PR4, затем PR5 при необходимости). Порядок имплементации контракта **[`spec/system-spec.md`](../spec/system-spec.md) §3** — **§2.4.4**.

**В этой задаче (только план):** не переписывать CRD N1 без отдельного решения; не менять bind/delete skeleton без необходимости; не включать data snapshots и полный export/import/restore.

### 2.5 Документация и операционка

| # | Задача | Статус |
|---|--------|--------|
| D1 | README: зависимости, CSI vs storage.deckhouse.io, manifest vs unified | ✅ [`../README.md`](../README.md) |
| D2 | RBAC из DSC + hook; DSC vs MCR | ✅ [`../operations/dsc-rbac-and-mcr.md`](../operations/dsc-rbac-and-mcr.md) |
| D3 | Runbook: исчезновение CRD (degraded / fail-open); unified runtime: метрики stale/resolved/active, рестарт pod | ✅ [`../operations/runbook-degraded-and-unified-runtime.md`](../operations/runbook-degraded-and-unified-runtime.md) |

---

## 4. Порядок внедрения

1. ~~S1–S2~~ ✅; тест **T1** ✅.
2. ~~**R1**~~ ✅.
3. ~~**R2 phase 1**~~ ✅ (DSC reconciler + статусы + тесты); ~~**R4**~~ ✅ в части reconciler (`KindConflict`).
4. ~~**R2 phase 2a**~~ ✅; ~~**R2 phase 2b (additive watches)**~~ ✅ (`unifiedruntime` + `AddWatch*`). ~~**R3 (ядро)**~~ ✅ — layered state, proof hot-add, Prometheus + лог stale↔resolved. Опционально — доп. proof-сценарии из design note.
5. ~~**D1–D3**~~ ✅ — обзор ([`README.md`](../README.md)), runbook ([`operations/runbook-degraded-and-unified-runtime.md`](../operations/runbook-degraded-and-unified-runtime.md)), DSC/RBAC/MCR ([`operations/dsc-rbac-and-mcr.md`](../operations/dsc-rbac-and-mcr.md)). При эволюции кода — синхронизировать эти три файла.

**Рекомендуемый порядок дальше** (после закрытого ядра R2/R3 + D1–D3): не смешивать manifest с rollout-unified в одном PR.

1. ~~**R5 + feature gates**~~ ✅ — `STATE_SNAPSHOTTER_UNIFIED_ENABLED`, `STATE_SNAPSHOTTER_UNIFIED_BOOTSTRAP_PAIRS`; Helm `unifiedSnapshotEnabled`, `unifiedBootstrapPairs`.
2. ~~**Точечные integration-тесты**~~ ✅ — `unified_runtime_rbac_eligibility_test.go`: без RBACReady нет записи в `EligibleFromDSC`; после снятия RBACReady resolved теряет пару, monotonic active сохраняет ключ.
3. ~~**N0 → N1**~~ ✅; **N2** — по **§2.4.1**: **N2a** (первый manifests-only snapshot + OK + download), затем **N2b** (дерево + aggregated download); далее **N3** (restart/hardening), **N4** (лимиты после N2), **N5** (data-layer и полный ТЗ вне manifests-only дерева).
4. **M1**, затем **M2** — только после стабилизации namespace-flow (**N2a** или явный gate в плане; расширение MCR spec не блокируется закрытием **N2b**, если так зафиксировано); manifest-трек не смешивать с N2 без необходимости.

*(R5 и M1–M2 не смешивать в одном PR без необходимости.)*

---

## 5. Открытые решения

- ~~Отдельный контроллер для DSC vs Runnable в manager~~ — для **R2 phase 1** выбран **reconciler в том же manager** (`SetupWithManager`); отдельный процесс при необходимости пересмотреть позже.
- Размещение **ValidatingWebhook** (только reject spec) — **пока не в приоритете**; брать после rollout/gates и при явной продуктовой потребности.
- Feature flag для unified целиком — в связке с **R5** (см. §4).

**Зафиксировано в ADR:** исчезновение CRD — degraded / fail-open; bootstrap до DSC; v1alpha1 только CRD-имена; cluster-only content.
