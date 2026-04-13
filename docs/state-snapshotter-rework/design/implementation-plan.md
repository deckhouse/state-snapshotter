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
- [x] **R3 (часть 1 — state + proof):** слой **bootstrap / eligible / merged / resolved** в `pkg/unifiedruntime.LayeredGVKState` + `BuildLayeredGVKState`; **active** — `Syncer.activeSnapshotGVKKeys` (монотонно: ключ попадает, если оба `AddWatch*` успешны); `LastLayeredState()` / `ActiveSnapshotGVKKeys()` для отладки и тестов; unit — `pkg/unifiedruntime/layers_test.go`. Интеграция: `test/integration/unified_runtime_hot_add_test.go` — DSC становится watch-eligible (Accepted → RBACReady), затем проверяются `LastLayeredState` (resolved + eligible) и `ActiveSnapshotGVKKeys`; тест **Serial**, маппинг на **RegistrationTestSnapshot** (не `TestSnapshot`), чтобы не вешать глобальный watch на тип, с которым lifecycle-спеки делают прямой `Reconcile` (иначе два reconcile-потока и 409). В `BeforeSuite` интеграции — wiring как в production: unified controllers + `unifiedruntime.NewSyncer` + `AddDomainSpecificSnapshotControllerToManager(..., syncer.Sync)`.
- [x] **R3 (observability):** после каждого `Sync` обновляются Prometheus gauges (`sigs.k8s.io/controller-runtime/pkg/metrics`): `state_snapshotter_unified_runtime_resolved_snapshot_gvk_count`, `active_monotonic_snapshot_gvk_count`, `stale_active_snapshot_gvk_count`; сводка на `V(2)`; при `stale_active_snapshot_gvk_count > 0` — **Info**-лог со списком ключей и явным hint про restart pod (additive watches не снимаются). Регистрация метрик — `sync.Once` в `NewSyncer`. См. [`r2-phase-2b-r3-runtime-registry.md`](r2-phase-2b-r3-runtime-registry.md).
- [x] **R5:** `config.Options` + env (`STATE_SNAPSHOTTER_UNIFIED_ENABLED`, `STATE_SNAPSHOTTER_UNIFIED_BOOTSTRAP_PAIRS`); `cmd/main.go` ветка без unified; `NewSyncer` получает `EffectiveUnifiedBootstrapPairs()`; Helm/OpenAPI. Ошибка парсинга bootstrap → warning + дефолтный список.
- [ ] **R3 / integration (опционально):** два DSC при поломке одного, полный T5/T9 и т.д.

### 2.3 Manifest capture

| # | Задача | Зачем | Статус |
|---|--------|--------|--------|
| M1 | Расширение **MCR spec** | UX | ⬜ **отложено** до стабилизации **NamespaceSnapshot / NamespaceSnapshotContent / ObjectKeeper** (N1–N3) |
| M2 | Лимиты объёма, таймауты list | Защита apiserver/etcd | ⬜ **после M1** (тот же gate) |

### 2.4 Namespace snapshot + NamespaceSnapshotContent + ObjectKeeper

**Цель:** сразу целевая схема **без миграции** с промежуточного generic `SnapshotContent` для корня namespace — см. [`decisions/namespace-snapshot-content-decision.md`](decisions/namespace-snapshot-content-decision.md). Детали сценария — **только** [`snapshot-rework/`](../../../snapshot-rework/). Статус **`NamespaceSnapshot`**: **только `conditions`**, без `status.phase` — [`decisions/namespace-snapshot-status-surface.md`](decisions/namespace-snapshot-status-surface.md).

| # | Задача | Документ / примечание | Статус |
|---|--------|------------------------|--------|
| N0 | **Gate:** **Chosen option** в [`namespace-snapshot-scope.md`](decisions/namespace-snapshot-scope.md) ≠ TBD. Сверка **apiVersion/group** для `NamespaceSnapshot` / `NamespaceSnapshotContent` между ТЗ в `snapshot-rework` и фактическими CRD в репозитории (привести к одному). | [`namespace-snapshot-controller.md`](namespace-snapshot-controller.md) §13–§16 | ✅ (scope resolved; group `storage.deckhouse.io/v1alpha1` на этапе N1) |
| N1 | **CRD + API:** типы `NamespaceSnapshot`, `NamespaceSnapshotContent`, codegen, OpenAPI; **убрать** использование **generic `SnapshotContent`** как носителя результата для namespace root. | decision + design §14 | ✅ **закрыт:** skeleton reconciler; bind + delete (Retain/Delete); `status.boundSnapshotContentName`; integration: lifecycle, deletion, **ref mismatch**, **status recovery**; default exclusions до real capture — §4.1 design; без ObjectKeeper / real capture (**N2**). |
| N2 | **Bootstrap / unified / RBAC:** пара GVK NS/NSC в `unifiedbootstrap`, watches, шаблоны RBAC; reconciler: finalizer, bind NS↔NSC, **conditions**; зачатки **ObjectKeeper** (корневой NSC, FollowObjectWithTTL — по ТЗ). | design §4.3, §5, §14 | ⬜ |
| N3 | **Интеграция:** envtest — расширение: delete → recovery после **рестарта** контроллера; доп. негативные кейсы; политика по §15. Базовые mismatch/recovery/status уже в N1 (`namespacesnapshot_n1_boundary_test.go`). | design §15 | ⬜ |
| N4 | **Реальный capture:** Job runner, артефакт, лимиты большого namespace (§8.6 design). | design §16 N2 | ⬜ |
| N5 | **Полный ТЗ:** дочерние снимки, DSC priority, экспорт/импорт, restore — итерациями по [`snapshot-rework/2026-01-25-namespace-snapshot.md`](../../../snapshot-rework/2026-01-25-namespace-snapshot.md) без изменения ТЗ из `docs/`. | бэклог | ⬜ |

Трек **N*** и **M1–M2** не смешивать в одном PR без необходимости.

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
3. **N0 → N1 → N2 → N3 → N4** — **`NamespaceSnapshot` / `NamespaceSnapshotContent` / ObjectKeeper** по ТЗ `snapshot-rework`; затем **N5** (полный сценарий ТЗ итерациями).
4. **M1**, затем **M2** — только после стабилизации **N1–N3** (минимум bind + OK + fake capture + тесты).

*(R5 и M1–M2 не смешивать в одном PR без необходимости.)*

---

## 5. Открытые решения

- ~~Отдельный контроллер для DSC vs Runnable в manager~~ — для **R2 phase 1** выбран **reconciler в том же manager** (`SetupWithManager`); отдельный процесс при необходимости пересмотреть позже.
- Размещение **ValidatingWebhook** (только reject spec) — **пока не в приоритете**; брать после rollout/gates и при явной продуктовой потребности.
- Feature flag для unified целиком — в связке с **R5** (см. §4).

**Зафиксировано в ADR:** исчезновение CRD — degraded / fail-open; bootstrap до DSC; v1alpha1 только CRD-имена; cluster-only content.
