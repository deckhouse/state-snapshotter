# Testing strategy (state-snapshotter)

Оркестрация уровней тестов и идентификаторы сценариев (T1–T11). Нормативные инварианты — в [`spec/system-spec.md`](../spec/system-spec.md) (в т.ч. **§3** — граф snapshot-run, merge refs, dedup для PR5+).

## Расположение кода тестов (этот репозиторий)

| Уровень | Путь / команда |
|---------|----------------|
| Unit | `cd images/state-snapshotter-controller && go test ./pkg/... ./internal/...` |
| Integration (envtest) | `go test -tags integration ./test/integration/...` или `make test-integration` |
| E2E (envtest) | `go test -tags e2e ./test/e2e/...` или `make test-e2e` |
| Smoke (кластер) | `./test-smoke.sh` из корня репозитория; **`hack/snapshot-graph-registry-smoke.sh`** — health модуля + опционально create/delete CSD (demo CRD), scan логов на panic/fatal |
| Ручной демо N2a (кластер) | [`snapshot-manual-demo.md`](snapshot-manual-demo.md) — YAML + `kubectl` для показа создания снимка, SnapshotContent/OK/MCP и aggregated |

Требуется `KUBEBUILDER_ASSETS` для integration/e2e (см. `.cursor/rules/controller-envtest-local.mdc`).

**Примечание:** каталога `tests/e2e-go` в корне нет; канонические тесты — под `images/state-snapshotter-controller/test/`. При необходимости удалённой прогонки на кластере — по согласованию с командой (smoke, CI).

### Snapshot retained path: integration (envtest) vs real cluster

| Где | Что является контрактом |
|-----|-------------------------|
| **Integration** | Структура и поведение **этого модуля**: `ownerReferences` (**root SnapshotContent→ObjectKeeper** для retained TTL-якоря, **MCR→Snapshot** на время capture, MCP→SnapshotContent, child SnapshotContent→parent SnapshotContent), снятие `snapshot.deckhouse.io/parent-protect` у дочернего SnapshotContent при удалении parent content (без `Client.Delete` детей), ручное удаление SnapshotContent после удаления root snapshot не блокируется финализаторами. **Не** требовать TTL Deckhouse ObjectKeeper или полного GC MCP/chunks как обязательный проход envtest. |
| **Real cluster** | **`hack/demo-e2e.sh`** (main): manifest + volume + retained + aggregated + forced ObjectKeeper TTL/GC (`08-forced-ttl-gc`); staged artifacts under `artifacts/<run-id>/`. |

Продуктовую модель удаления не менять ради ограничений envtest (см. также `.cursor/rules/controller-envtest-local.mdc`).

### Demo domain CSD (Proposed)

Пока трек в дизайне — [`design/demo-domain-csd/README.md`](../design/demo-domain-csd/README.md); универсальная модель дерева и **`Ready`** — [`design/demo-domain-csd/08-universal-snapshot-tree-model.md`](../design/demo-domain-csd/08-universal-snapshot-tree-model.md).

**После реализации кода** — **[`demo-domain-csd-test-plan.md`](demo-domain-csd-test-plan.md)**:

- heterogeneous дерево через общие **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`**;
- **один** condition **`Ready`** (каскад успеха и деградации снизу вверх, **`reason`/`message`** с первопричиной; **без** `SubtreeReady`);
- **dedup** — проверка **вычисляемой** логики по фактам API (**без** persisted `domainCoverage`);
- сценарии удаления chunk/MCP, дочернего snapshot и дочернего content (**§5** test-plan: **5a.1**/**5a.2** по путям первичной классификации при поддержке обоих, **5b**, **5c**); для **PR5** они — **merge-gate** (деградация `Ready` после успеха — DoD, не откладывается).

**Минимум:** `go test -tags integration ./test/integration/...`; **опционально** — cluster smoke. Закрытие трека без зелёных тестов по плану — **недопустимо** ([`implementation-plan.md`](../design/implementation-plan.md) §2.4.3).

### Unified demo/e2e (main cluster path): `hack/demo-e2e.sh`

**Команда:** `bash hack/demo-e2e.sh` из корня репозитория.

**Один сценарий** для **state-snapshotter + storage-foundation**: manifest capture (ConfigMap + MCP), retained lifecycle + aggregated read, bulk **VCR→VSC→`dataRefs[]`**, two-PVC subtree (child **pvc-a**, root residual **pvc-b**).

**Артефакты:** `artifacts/<run-id>/{00-preflight … 09-cleanup}/` — YAML/JSON dumps, events, `summary.txt`, graph (`hack/snapshot-graph.sh`). Граф: volume-рёбра (`status.dataRefs[]`, `status.volumeCaptureRequestName` → VCR), **fail** если `MCP.status.chunks[]` не читаются (`get manifestcheckpointcontentchunks` в `templates/rbac-for-us.yaml` admin-kubeconfig); fixture: `hack/test-snapshot-graph-fixture.sh`, `hack/test-snapshot-graph-chunk-verify.sh`.

**Storage:** **local-thin** only. **TODO:** Rook/Ceph not supported in this script yet.

**Retained read:** временный путь `GET …/namespaces/{ns}/snapshots/{rootName}/manifests` после удаления root Snapshot (см. TODO в скрипте). Целевой API — `/snapshotcontents/{contentName}/manifests`.

**Опции:** `DEMO_E2E_SKIP_CLEANUP=1`, `DEMO_E2E_SKIP_OK_CONTRACT=1`, `DEMO_E2E_SKIP_FORCED_TTL=1` (debug skip stage 08), `DEMO_E2E_REQUIRE_FORCED_TTL=1` (fail if stage 08 cannot patch ObjectKeeper), `DEMO_E2E_STALL_SEC`, `DEMO_E2E_ARTIFACT_DIR` (или `DEMO_E2E_ARTIFACTS_ROOT`), `DEMO_E2E_WAIT_SEC`, `DEMO_E2E_GC_WAIT_SEC`. Preflight logs user, `can-i patch objectkeepers`, and planned stage-08 RUN/SKIP.

**Legacy:** отдельные PR-4/PR-8 cluster scripts удалены; единственный канонический путь — `hack/demo-e2e.sh`.

**RBAC (три роли, см. `templates/controller/rbac-for-us.yaml` vs `templates/rbac-for-us.yaml`):** (1) controller SA — create/patch ObjectKeeper, MCR/MCP, VCR, SnapshotContent status; **без** `delete` ObjectKeeper; (2) admin-kubeconfig — Snapshots + aggregated read + read-only MCR/MCP/OK; (3) forced TTL — **не** в admin-kubeconfig: stage `08-forced-ttl-gc` **SKIP** по умолчанию, если caller не может `patch objectkeepers.deckhouse.io`; скрипт может поставить временный `demo-e2e-objectkeeper-patcher-<run-id>` или нужен cluster-admin. `DEMO_E2E_REQUIRE_FORCED_TTL=1` — fail вместо skip. Перед smoke: `bash hack/rbac-can-i-audit.sh`.

**Зависимости:** `d8-state-snapshotter`, `d8-storage-foundation`, Deckhouse ObjectKeeper controller, RBAC create на `snapshots.storage.deckhouse.io` в smoke-namespace.

**Forced TTL (replaces PR4_SMOKE_REQUIRE_TTL):** тестовый патч `ObjectKeeper.spec.ttl=0s` из скрипта (не production controller); ожидание GC до `DEMO_E2E_GC_WAIT_SEC`.

## Уже есть (поддерживать)

| Уровень | Назначение |
|---------|------------|
| Unit | Условия, GVK registry, `unifiedbootstrap`, `unifiedruntime` (`layers_test`, `metrics_test`, snapshot registry tests) |
| Integration | envtest, CRD из `crds/`; **CSD:** см. ниже |
| E2E (envtest) | Сборка manager |
| Smoke | Реальный кластер: pre-e2e [`pre-e2e-smoke-validation.md`](pre-e2e-smoke-validation.md), **`hack/demo-e2e.sh`** (canonical unified e2e) |

**Integration (CSD + unified runtime):** в `BeforeSuite` (`setup_test.go`) поднимаются CSD reconciler и **production-like** unified stack: resolve bootstrap ∪ eligible CSD на mapper → snapshot/content контроллеры на `mgr` → `unifiedruntime.Syncer` → `AddCustomSnapshotDefinitionControllerToManager(..., syncer.Sync, graphRegistryRefresh)` (как в `cmd/main.go`, без дублирования второго `SetupWithManager` для тех же имён контроллеров).

Envtest integration не проверяет реальный Kubernetes RBAC enforcement: `RBACReady` в этих тестах симулирует handshake внешнего RBAC controller/hook. Real-cluster smoke/e2e должны явно применять test-only RBAC для domain resources до `RBACReady=True` (см. [`pre-e2e-smoke-validation.md`](pre-e2e-smoke-validation.md)).

Latest manual pre-e2e smoke status: passed on 2026-04-29 with test-only domain RBAC, namespace-relative aggregated API output, and expected retained SnapshotContent/ObjectKeeper artifacts after cleanup. Non-blocking findings to keep visible in reports: transient `ObjectKeeper already exists` can appear on repeated runs with retained artifacts; Kubernetes warns that the current `Snapshot` finalizer name should include a path.

**CSD-gated demo activation:** graph registry built-ins содержат только `Snapshot`→`SnapshotContent`. Demo VM/Disk controllers стартуют в harness всегда, но demo resources входят в `Snapshot` tree только через eligible CSD. Integration покрывает три границы: без demo CSD нет demo children; после hot-add CSD новый `Snapshot` создаёт demo child; manual `DemoVirtualDiskSnapshot` materializes без CSD.

**Custom snapshot controller status contract:** custom snapshot controllers set `status.conditions[type=HandledByCustomSnapshotController]=True` before `GenericSnapshotBinderController` binds `SnapshotContent`. Tests and smoke must not use the superseded `HandledByDomainSpecificController` condition name.

| Файл | Что проверяет |
|------|----------------|
| `csd_api_smoke_test.go` | Схема + CRD Established; `Accepted=True` после resolve; `Ready` после симуляции `RBACReady`. Маппинг на **RegistrationTest**\* CRD (не TestSnapshot), чтобы не пересекаться с lifecycle-спеками и hot-add по одному snapshot kind. |
| `csd_reconciler_kindconflict_test.go` | Два CSD → `KindConflict` |
| `csd_reconciler_invalidspec_test.go` | Namespace-scoped content → `InvalidSpec`; дубликат snapshot kind в одном CSD → `InvalidSpec` |
| `unified_runtime_hot_add_test.go` | **R3 proof:** CSD создаётся после старта manager; после `RBACReady` — `unifiedSyncer.ActiveSnapshotGVKKeys` и `LastLayeredState()` (resolved + eligible). **`Serial`**; очистка конфликтующих CSD (в т.ч. rbac/eligibility/smoke). |
| `unified_runtime_rbac_eligibility_test.go` | **T4 + eligibility:** без RBACReady нет eligible-слоя для RegistrationTest; после снятия RBACReady resolved без пары, monotonic active сохраняет ключ. **`Serial`**; `AfterEach` чистит CSD. |
| `csd_gated_domain_activation_test.go` | Demo domain graph activation: without CSD → no demo children; hot-add CSD → new root sees demo child; manual demo snapshot works without CSD. |
| `controller_registration_test.go` | Конструирование контроллеров как в production; **без** повторного `SetupWithManager` на общем `mgr` |
| `snapshot_lifecycle_test.go` | **N1 skeleton:** `Snapshot` → `SnapshotContent`, `status.boundSnapshotContentName` (unified root bind field), Ready через conditions (без ObjectKeeper / полного N2) |
| `snapshot_deletion_test.go` | **Delete flow:** Retain — snapshot gone, SnapshotContent остаётся; Delete policy — root finalizer только после `NotFound` на content; **retained unified** — после delete snapshot остаются SnapshotContent+MCP (MCR уже снят после capture); проверки контракта root OK (`followObjectRef`→Snapshot; root **SnapshotContent** controller `ownerRef`→OK) и MCP→SnapshotContent; **MCR `ownerRef`→Snapshot** — сценарий «удаление root при живом MCR»: delete с **`DeletePropagationBackground`** (foreground без kube-controller-manager зависает); ожидается **NotFound** на snapshot; MCR — **NotFound** на кластере с GC или (plain envtest) объект может остаться с тем же **`ownerRef`** до появления GC; узкий сценарий — пользователь удаляет SnapshotContent после удаления snapshot (deletion завершается, без контракта GC артефактов) |
| `snapshot_n1_boundary_test.go` | **Формальное закрытие N1:** **recovery** — после сброса `status` при существующем deterministic content снова `Bound`+`Ready`; короткая **стабильность** (Consistently) |
| `snapshot_recreate_test.go` | **§4.7 / отдельный lifecycle MCR:** после первого **Ready** MCR уже снят; удаление root; второй snapshot с тем же `metadata.name` — новый UID, новый SnapshotContent + новый MCR (`nss-{uid2}`), **Ready**; старый Retain SnapshotContent остаётся; имя MCR зависит от UID, коллизий нет |
| `snapshot_capture_plan_drift_test.go` | **N2a §4.7:** при frozen **MCR.spec.targets** добавление allowlisted объекта в namespace → **CapturePlanDrift** на root snapshot (без молчаливого `Update` **MCR.spec.targets**); **MCR** остаётся в API для ручного удаления / retry; пока MCR жив — **`ownerRef`→`Snapshot`** |
| `snapshot_graph_e5_e6_integration_test.go` | **§3-E5 + §3-E6** на **реальном** графе: test fixture/controller path заполняет parent-owned **`childrenSnapshotRefs`**, а snapshot controller публикует **`childrenSnapshotContentRefs`**; child берётся как **registered snapshot kind fixture** (Snapshot kind, без synthetic-семантики). **E5** — первый root **MCR** не создаётся, пока exclude по subtree нельзя посчитать (child **MCP** / **`manifestCheckpointName`**); root **MCR** не листит объект, уже в descendant **MCP**; common SnapshotContent MCPs учитываются тем же content graph traversal; **E6** — **`ChildSnapshotPending`**, приоритет **`SubtreeManifestCapturePending`** vs child pending, **`ChildSnapshotFailed`** при терминальном **`CapturePlanDrift`** на child **MCR**, каскад **`FinalizerParentProtect`** / снятие при удалении parent SnapshotContent (см. сценарии в файле) |
| Unit: `namespace_snapshot_parent_ready_e6_test.go`, `child_snapshot_resolve_test.go`; `snapshot_child_snapshot_watches_test.go` | **[`spec/system-spec.md`](../spec/system-spec.md) §3.2:** дерево run **namespace-local**; форма ref — **`apiVersion/kind/name`** (без `namespace`), child namespace всегда от parent; relay **child→parent** без cluster-wide **`Snapshot` list** | N2b E6 / watches | ✅ |

**N2a (integration — план минимума, см. [`design/implementation-plan.md`](../design/implementation-plan.md) §2.4.1 и [`design/snapshot-controller.md`](../design/snapshot-controller.md) §4.4–§4.7, §5.2, §8.7):** happy path (namespace → **MCR→ManifestCheckpoint** → persisted result → **Ready**; root MCP may be empty but always exists; root MCP does not contain Kubernetes **Namespace** object; on SnapshotContent **`manifestCheckpointName`** is always set; root **без** MCR в status; MCR name по §4.7; **ownerRef MCR→Snapshot**); fail-closed / allowlist; **Retain** с **root OK** + execution OK для MCR; провал MCR/MCP; **удаление root при живом MCR** — на реальном кластере MCR убирает **kube-controller GC** после исчезновения snapshot из API (§5.2); в envtest без controller-manager интеграционный тест проверяет **NotFound** на snapshot при **background** delete и контракт **`ownerRef`**, а не обязательный **NotFound** на MCR; download одного снимка (**§8.7.1**, 409 если MCP не Ready, 500 при битой склейке); smoke **pagination** при list в capture-потоке.

**N2b:** дерево — **зарегистрированные snapshot-типы** и typed **`Snapshot`** как возможный child в refs, **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`**, snapshot controllers публикуют durable content graph, parent content **Ready** агрегируется `SnapshotContentController`, snapshot **Ready** только зеркалит bound content (**§11.1** design; нормативный минимум **§3.8** [`spec/system-spec.md`](../spec/system-spec.md)), **aggregated manifests download** на чтении; **PR4** — нормативный контракт HTTP/ошибок/обхода: [`spec/snapshot-aggregated-manifests-pr4.md`](../spec/snapshot-aggregated-manifests-pr4.md); generic HTTP read API — [`api/snapshot-read.md`](../api/snapshot-read.md): `curl .../{resource}/{name}/manifests` должен возвращать полный subtree для root snapshot и только child subtree для child snapshot, а duplicate object identity должен завершаться ошибкой, без silent dedup. Поставка и история PR — [`design/implementation-plan.md`](../design/implementation-plan.md) **§2.4.2**; контракт **E6** и приоритетов reason — интеграция **`snapshot_graph_e5_e6_integration_test.go`** + unit **`namespace_snapshot_parent_ready_e6_test.go`**; **PR5b** — `demovirtualmachinesnapshot_pr5b_test.go` (root **`Completed`** после готовности child contents по **E6**, **без** искусственного leaf **Snapshot** под root; own MCP separation + read from root/VM/disk content). Для demo API parent/back-reference задаётся через ownerRef, а persisted graph — через parent-owned `status.childrenSnapshotRefs`. Далее PR4 по спеке.

## Планируемые тесты

**Бэклог integration:** T5, T8–T11 и др. — по необходимости. R5 + T4/eligibility — см. [`design/implementation-plan.md`](../design/implementation-plan.md).

**Порядок с M-треком:** сценарии **T6** и расширение **MCR spec** — по gate в [`design/implementation-plan.md`](../design/implementation-plan.md) §2.4 / §4 (**N2a** или явное исключение); закрытие **N2b** не обязано блокировать M1, если так зафиксировано в плане.

| ID | Тест | Связь | Статус |
|----|------|--------|--------|
| T1 | Нет production unified CRD в API — wiring без ошибки, ноль watch | S1–S2 | ✅ `unified_bootstrap_t1_test.go`, `pkg/unifiedbootstrap/gvk_test.go` |
| T2 | CSD + маппинг; **статусы** Accepted/RBACReady/Ready; **активация watch** по формуле (eligible → Sync → layered + active) | R1–R3 | ✅ `csd_api_smoke_test.go`, `unified_runtime_hot_add_test.go`, `unified_runtime_rbac_eligibility_test.go`. ⬜ расширение: T5 (spec/delete CSD) |
| T3 | Два CSD, конфликт kind | R4 | ✅ `csd_reconciler_kindconflict_test.go` |
| T4 | Без `RBACReady` пара не в `EligibleFromCSD` / не eligible для merge (не проверяем monotonic `active`) | R3 | ✅ `unified_runtime_rbac_eligibility_test.go` |
| T5 | Декомпозиция update/delete CSD: смена desired GVK в spec, удаление CSD, смена поколения статуса, последовательные apply нескольких CSD | R2, spec | ⬜ |
| T6 | MCR расширенный выбор | M1–M2 | ⬜ |
| T7 | Только MCR, без unified | S1–S2 | ⬜ |
| T8 | Исчезновение CRD | D3, ADR | ⬜ |
| T9 | Устаревшее observedGeneration | R3 | ⬜ |
| T10 | RBAC drift / 403, изоляция по типу | — | ⬜ |
| T11 | Два CSD, изоляция при поломке одного | опционально | ⬜ |

## Нефункциональные

- CI: `go_checks`.
- **Метрики unified runtime** (controller metrics endpoint): `state_snapshotter_unified_runtime_resolved_snapshot_gvk_count`, `state_snapshotter_unified_runtime_active_monotonic_snapshot_gvk_count`, `state_snapshotter_unified_runtime_stale_active_snapshot_gvk_count` — см. `pkg/unifiedruntime/metrics.go`.
- Нагрузка: большие MCR после M1 (M1 — после N-трека по текущему плану).

## Demo / remote validation

- Автоматизированная проверка на кластере и подготовка окружения — по мере внедрения сценариев; не удалять диагностические тесты без замены.
- Актуальный артефакт pre-e2e smoke (kubectl checklist, namespace-local refs, `sourceRef`, root/demo readiness, namespace-relative aggregated API): [`pre-e2e-smoke-validation.md`](pre-e2e-smoke-validation.md) — статус `pre-e2e-passed` на 2026-04-29.
- Детали деплоя контроллера и линтера: `.cursor/rules/controller-redeploy-and-remote-e2e.mdc`.
- Эксплуатация на кластере (CRD, метрики, stale, рестарт): [`../operations/runbook-degraded-and-unified-runtime.md`](../operations/runbook-degraded-and-unified-runtime.md).
