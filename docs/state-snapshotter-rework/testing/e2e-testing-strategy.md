# Testing strategy (state-snapshotter)

Оркестрация уровней тестов и идентификаторы сценариев (T1–T11). Нормативные инварианты — в [`spec/system-spec.md`](../spec/system-spec.md).

## Расположение кода тестов (этот репозиторий)

| Уровень | Путь / команда |
|---------|----------------|
| Unit | `cd images/state-snapshotter-controller && go test ./pkg/... ./internal/...` |
| Integration (envtest) | `go test -tags integration ./test/integration/...` или `make test-integration` |
| E2E (envtest) | `go test -tags e2e ./test/e2e/...` или `make test-e2e` |
| Smoke (кластер) | `./test-smoke.sh` из корня репозитория |

Требуется `KUBEBUILDER_ASSETS` для integration/e2e (см. `.cursor/rules/controller-envtest-local.mdc`).

**Примечание:** каталога `tests/e2e-go` в корне нет; канонические тесты — под `images/state-snapshotter-controller/test/`. При необходимости удалённой прогонки на кластере — по согласованию с командой (smoke, CI).

### NamespaceSnapshot retained path: integration (envtest) vs real cluster

| Где | Что является контрактом |
|-----|-------------------------|
| **Integration** | Структура и поведение **этого модуля**: `ownerReferences` (root ObjectKeeper→NSC, **MCR→NamespaceSnapshot** на время capture, MCP→NSC, child NSC→parent NSC), снятие `snapshot.deckhouse.io/parent-protect` у дочернего NSC при удалении parent content (без `Client.Delete` детей), ручное удаление NSC после удаления root snapshot не блокируется финализаторами. **Не** требовать TTL Deckhouse ObjectKeeper или полного GC MCP/chunks как обязательный проход envtest. |
| **Real cluster** | `hack/pr4-smoke.sh`: retained read, aggregated manifests, **обязательный** контракт root ObjectKeeper (если не `PR4_SMOKE_SKIP_OK_CONTRACT=1`); фаза TTL — наблюдательная по умолчанию, **строгая** только с `PR4_SMOKE_REQUIRE_TTL=1` при реально настроенном TTL. |

Продуктовую модель удаления не менять ради ограничений envtest (см. также `.cursor/rules/controller-envtest-local.mdc`).

### PR4: проверка на реальном кластере (`hack/pr4-smoke.sh`)

**Команда:** из корня репозитория `bash hack/pr4-smoke.sh` (без `PR4_SMOKE_SKIP_OK_CONTRACT`, если в кластере есть `objectkeepers.deckhouse.io`). Лог прогона сохраняйте как артефакт ревью/PR.

**Подтверждено базовым прогоном** (read-path + retained + контракт модуля на живом API server):

- subresource **aggregated manifests** отвечает и отдаёт ожидаемый JSON-массив;
- **retained read** после удаления `NamespaceSnapshot` продолжает работать (тот же путь агрегации по retained `NamespaceSnapshotContent`);
- **root ObjectKeeper** (шаг 5b скрипта): `spec.followObjectRef` → `NamespaceSnapshot` (UID root snapshot), в `metadata.ownerReferences` есть ссылка на соответствующий `NamespaceSnapshotContent`;
- discovery субресурса, опциональный gzip, negative 404 — по сценарию скрипта.

**Не является частью базового прогона** (отдельно: интеграция с Deckhouse ObjectKeeper + GC, не unit/integration модуля):

- **strict TTL cascade:** `PR4_SMOKE_REQUIRE_TTL=1` — скрипт ждёт до `PR4_SMOKE_WAIT_SEC` исчезновения retained `NamespaceSnapshotContent`, затем отсутствия `ManifestCheckpoint`, затем **неуспешный** aggregated GET. Корневой OK создаётся контроллером всегда в **`FollowObjectWithTTL`**; `spec.ttl` — из `STATE_SNAPSHOTTER_SNAPSHOT_ROOT_OK_TTL` / алиас `STATE_SNAPSHOTTER_NS_ROOT_OK_TTL` или встроенный дефолт (`DefaultSnapshotRootOKTTL` в `pkg/config`; может быть временно уменьшен для отладки, например 1m). Для strict-прогона задайте `PR4_SMOKE_WAIT_SEC` с запасом относительно `spec.ttl`. Без strict-режима шаг 10 остаётся наблюдательным (`sleep` + INFO).

**Итоговая формулировка для PR / чата:** PR4 как **read-path** и **retained-path** на реальном кластере — рабочие; полное доказательство **TTL-удаления** retained артефактов — отдельный прогон на окружении с известным конфигом ObjectKeeper.

## Уже есть (поддерживать)

| Уровень | Назначение |
|---------|------------|
| Unit | Условия, GVK registry, `unifiedbootstrap`, `unifiedruntime` (`layers_test`, `metrics_test`, snapshot registry tests) |
| Integration | envtest, CRD из `crds/`; **DSC:** см. ниже |
| E2E (envtest) | Сборка manager |
| Smoke | Реальный кластер (`./test-smoke.sh`, `hack/pr4-smoke.sh` для NamespaceSnapshot retained) |

**Integration (DSC + unified runtime):** в `BeforeSuite` (`setup_test.go`) поднимаются DSC reconciler и **production-like** unified stack: resolve bootstrap ∪ eligible DSC на mapper → snapshot/content контроллеры на `mgr` → `unifiedruntime.Syncer` → `AddDomainSpecificSnapshotControllerToManager(..., syncer.Sync)` (как в `cmd/main.go`, без дублирования второго `SetupWithManager` для тех же имён контроллеров).

| Файл | Что проверяет |
|------|----------------|
| `dsc_api_smoke_test.go` | Схема + CRD Established; `Accepted=True` после resolve; `Ready` после симуляции `RBACReady`. Маппинг на **RegistrationTest**\* CRD (не TestSnapshot), чтобы не пересекаться с lifecycle-спеками и hot-add по одному snapshot kind. |
| `dsc_reconciler_kindconflict_test.go` | Два DSC → `KindConflict` |
| `dsc_reconciler_invalidspec_test.go` | Namespace-scoped content → `InvalidSpec`; дубликат snapshot kind в одном DSC → `InvalidSpec` |
| `unified_runtime_hot_add_test.go` | **R3 proof:** DSC создаётся после старта manager; после `RBACReady` — `unifiedSyncer.ActiveSnapshotGVKKeys` и `LastLayeredState()` (resolved + eligible). **`Serial`**; очистка конфликтующих DSC (в т.ч. rbac/eligibility/smoke). |
| `unified_runtime_rbac_eligibility_test.go` | **T4 + eligibility:** без RBACReady нет eligible-слоя для RegistrationTest; после снятия RBACReady resolved без пары, monotonic active сохраняет ключ. **`Serial`**; `AfterEach` чистит DSC. |
| `controller_registration_test.go` | Конструирование контроллеров как в production; **без** повторного `SetupWithManager` на общем `mgr` |
| `namespacesnapshot_lifecycle_test.go` | **N1 skeleton:** `NamespaceSnapshot` → `NamespaceSnapshotContent`, `status.boundSnapshotContentName` (unified root bind field), Ready через conditions (без ObjectKeeper / полного N2) |
| `namespacesnapshot_deletion_test.go` | **Delete flow:** Retain — snapshot gone, NSC остаётся; Delete policy — root finalizer только после `NotFound` на content; **retained unified** — после delete snapshot остаются NSC+MCP (MCR уже снят после capture); проверки контракта root OK (`followObjectRef`→NamespaceSnapshot, `ownerRef`→NSC) и MCP→NSC; **MCR `ownerRef`→NamespaceSnapshot** — после появления MCR удаление root даёт **NotFound** на MCR (GC); узкий сценарий — пользователь удаляет NSC после удаления snapshot (deletion завершается, без контракта GC артефактов) |
| `namespacesnapshot_n1_boundary_test.go` | **Формальное закрытие N1:** `ContentRefMismatch` при неверном `namespaceSnapshotRef` на NSC; **recovery** — после сброса `status` при валидном content снова `Bound`+`Ready`; короткая **стабильность** (Consistently) |
| `namespacesnapshot_recreate_test.go` | **§4.7 / отдельный lifecycle MCR:** после первого **Ready** MCR уже снят; удаление root; второй snapshot с тем же `metadata.name` — новый UID, новый NSC + новый MCR (`nss-{uid2}`), **Ready**; старый Retain NSC остаётся; имя MCR зависит от UID, коллизий нет |
| `namespacesnapshot_capture_plan_drift_test.go` | **N2a §4.7:** после **Ready** добавление allowlisted объекта в namespace → **CapturePlanDrift** на root и NSC (без молчаливого `Update` **MCR.spec.targets**); **MCR** остаётся в API для ручного удаления / retry; пока MCR жив — **`ownerRef`→`NamespaceSnapshot`** |
| `namespacesnapshot_synthetic_tree_test.go` | **Synthetic child tree (scaffold):** child NS; graph refs; parent ждёт child; **`ChildSnapshotPending`** → **`Completed`**; **контракт каскада:** child NSC имеет `ownerRef`→parent NSC, child с `FinalizerParentProtect`; при delete parent NSC контроллер снимает parent-protect с child (без `Delete` child из этого reconciler) |
| `namespacesnapshot_synthetic_child_failure_test.go` | **N2b PR3:** искусственный **CapturePlanDrift** только на **MCR child** (лишний target в `spec.targets`); child терминально **`Ready=False`**; parent остаётся с валидным N2a-планом → **`ChildSnapshotFailed`**, message с именем child + причиной child |

**N2a (integration — план минимума, см. [`design/implementation-plan.md`](../design/implementation-plan.md) §2.4.1 и [`design/namespace-snapshot-controller.md`](../design/namespace-snapshot-controller.md) §4.4–§4.7, §5.2, §8.7):** happy path (namespace → **MCR→ManifestCheckpoint** → persisted result → **Ready** только по MCP/chunks; на NSC **`manifestCheckpointName`**; root **без** MCR в status; MCR name по §4.7; **ownerRef MCR→NamespaceSnapshot**); fail-closed / allowlist; **Retain** с **root OK** + execution OK для MCR; провал MCR/MCP; **удаление root при живом MCR** — очистка MCR **GC** по ownerRef после исчезновения root из API (§5.2); download одного снимка (**§8.7.1**, 409 если MCP не Ready, 500 при битой склейке); smoke **pagination** при list в capture-потоке.  

**N2b:** дерево — дочерние NS/NSC, **childrenSnapshotRefs** / **childrenSnapshotContentRefs**, агрегированный **Ready** parent (**§11.1** design), **aggregated manifests download** на чтении; **PR4** — нормативный контракт HTTP/ошибок/обхода: [`spec/namespace-snapshot-aggregated-manifests-pr4.md`](../spec/namespace-snapshot-aggregated-manifests-pr4.md); поставка короткими PR — [`design/implementation-plan.md`](../design/implementation-plan.md) **§2.4.2**; **PR2** — `namespacesnapshot_synthetic_tree_test.go`; **PR3** — матрица parent reason (**`ChildSnapshotPending`** / **`ChildSnapshotFailed`** / **`Completed`**) — `namespacesnapshot_synthetic_tree_test.go` + `namespacesnapshot_synthetic_child_failure_test.go`; далее PR4 по спеке.

## Планируемые тесты

**Бэклог integration:** T5, T8–T11 и др. — по необходимости. R5 + T4/eligibility — см. [`design/implementation-plan.md`](../design/implementation-plan.md).

**Порядок с M-треком:** сценарии **T6** и расширение **MCR spec** — по gate в [`design/implementation-plan.md`](../design/implementation-plan.md) §2.4 / §4 (**N2a** или явное исключение); закрытие **N2b** не обязано блокировать M1, если так зафиксировано в плане.

| ID | Тест | Связь | Статус |
|----|------|--------|--------|
| T1 | Нет production unified CRD в API — wiring без ошибки, ноль watch | S1–S2 | ✅ `unified_bootstrap_t1_test.go`, `pkg/unifiedbootstrap/gvk_test.go` |
| T2 | DSC + маппинг; **статусы** Accepted/RBACReady/Ready; **активация watch** по формуле (eligible → Sync → layered + active) | R1–R3 | ✅ `dsc_api_smoke_test.go`, `unified_runtime_hot_add_test.go`, `unified_runtime_rbac_eligibility_test.go`. ⬜ расширение: T5 (spec/delete DSC) |
| T3 | Два DSC, конфликт kind | R4 | ✅ `dsc_reconciler_kindconflict_test.go` |
| T4 | Без `RBACReady` пара не в `EligibleFromDSC` / не eligible для merge (не проверяем monotonic `active`) | R3 | ✅ `unified_runtime_rbac_eligibility_test.go` |
| T5 | Декомпозиция update/delete DSC: смена desired GVK в spec, удаление DSC, смена поколения статуса, последовательные apply нескольких DSC | R2, spec | ⬜ |
| T6 | MCR расширенный выбор | M1–M2 | ⬜ |
| T7 | Только MCR, без unified | S1–S2 | ⬜ |
| T8 | Исчезновение CRD | D3, ADR | ⬜ |
| T9 | Устаревшее observedGeneration | R3 | ⬜ |
| T10 | RBAC drift / 403, изоляция по типу | — | ⬜ |
| T11 | Два DSC, изоляция при поломке одного | опционально | ⬜ |

## Нефункциональные

- CI: `go_checks`.
- **Метрики unified runtime** (controller metrics endpoint): `state_snapshotter_unified_runtime_resolved_snapshot_gvk_count`, `state_snapshotter_unified_runtime_active_monotonic_snapshot_gvk_count`, `state_snapshotter_unified_runtime_stale_active_snapshot_gvk_count` — см. `pkg/unifiedruntime/metrics.go`.
- Нагрузка: большие MCR после M1 (M1 — после N-трека по текущему плану).

## Demo / remote validation

- Автоматизированная проверка на кластере и подготовка окружения — по мере внедрения сценариев; не удалять диагностические тесты без замены.
- Детали деплоя контроллера и линтера: `.cursor/rules/controller-redeploy-and-remote-e2e.mdc`.
- Эксплуатация на кластере (CRD, метрики, stale, рестарт): [`../operations/runbook-degraded-and-unified-runtime.md`](../operations/runbook-degraded-and-unified-runtime.md).
