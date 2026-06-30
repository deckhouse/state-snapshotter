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
| Live demo runbook (snapshot, без restore) | [`snapshot-live-demo-runbook.md`](snapshot-live-demo-runbook.md) — порядок показа, речь, проверки; прогон на кластере 2026-05-29 |

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

**Артефакты:** `artifacts/<run-id>/{00-preflight … 09-cleanup}/` — YAML/JSON dumps, events, `summary.txt`, graph (`hack/snapshot-graph.sh`). Граф: volume-рёбра (`status.dataRefs[]`, `status.volumeCaptureRequestName` → VCR), **fail** если `MCP.status.chunks[]` не читаются (chunks читаются под controller SA через `hack/snapshot-graph.sh --chunk-as system:serviceaccount:d8-state-snapshotter:controller`; admin-kubeconfig прямого доступа к chunk payload не имеет — by design, см. `templates/rbac-for-us.yaml`); fixture: `hack/test-snapshot-graph-fixture.sh`, `hack/test-snapshot-graph-chunk-verify.sh`.

**Storage:** **local-thin** only. **TODO:** Rook/Ceph not supported in this script yet.

**Retained read:** временный путь `GET …/namespaces/{ns}/snapshots/{rootName}/manifests` после удаления root Snapshot (см. TODO в скрипте). Целевой API — `/snapshotcontents/{contentName}/manifests`.

**Опции:** `DEMO_E2E_SKIP_CLEANUP=1`, `DEMO_E2E_SKIP_OK_CONTRACT=1`, `DEMO_E2E_SKIP_FORCED_TTL=1` (debug skip stage 08), `DEMO_E2E_REQUIRE_FORCED_TTL=1` (fail if stage 08 cannot patch ObjectKeeper), `DEMO_E2E_STALL_SEC`, `DEMO_E2E_ARTIFACT_DIR` (или `DEMO_E2E_ARTIFACTS_ROOT`), `DEMO_E2E_WAIT_SEC`, `DEMO_E2E_GC_WAIT_SEC`. Preflight logs user, `can-i patch objectkeepers`, and planned stage-08 RUN/SKIP.

**Legacy:** отдельные PR-4/PR-8 cluster scripts удалены; единственный канонический путь — `hack/demo-e2e.sh`.

**RBAC (три роли, см. `templates/controller/rbac-for-us.yaml` vs `templates/rbac-for-us.yaml`):** (1) controller SA — create/patch ObjectKeeper, MCR/MCP, VCR, SnapshotContent status; **без** `delete` ObjectKeeper; (2) admin-kubeconfig — Snapshots + aggregated read + read-only MCR/MCP/OK; (3) forced TTL — **не** в admin-kubeconfig: stage `08-forced-ttl-gc` **SKIP** по умолчанию, если caller не может `patch objectkeepers.deckhouse.io`; скрипт может поставить временный `demo-e2e-objectkeeper-patcher-<run-id>` или нужен cluster-admin. `DEMO_E2E_REQUIRE_FORCED_TTL=1` — fail вместо skip. Перед smoke: `bash hack/rbac-can-i-audit.sh`.

**Зависимости:** `d8-state-snapshotter`, `d8-storage-foundation`, Deckhouse ObjectKeeper controller, RBAC create на `snapshots.state-snapshotter.deckhouse.io` в smoke-namespace.

**Forced TTL (replaces PR4_SMOKE_REQUIRE_TTL):** тестовый патч `ObjectKeeper.spec.ttl=0s` из скрипта (не production controller); ожидание GC до `DEMO_E2E_GC_WAIT_SEC`.

## Уже есть (поддерживать)

| Уровень | Назначение |
|---------|------------|
| Unit | Условия, GVK registry, `unifiedbootstrap`, `unifiedruntime` (`layers_test`, `metrics_test`, snapshot registry tests) |
| Integration | envtest, CRD из `crds/`; **CSD:** см. ниже |
| E2E (envtest) | Сборка manager |
| Smoke | Реальный кластер: pre-e2e [`pre-e2e-smoke-validation.md`](pre-e2e-smoke-validation.md), **`hack/demo-e2e.sh`** (canonical unified e2e) |

**Integration (CSD + unified runtime):** в `BeforeSuite` (`setup_test.go`) поднимаются CSD reconciler и **production-like** unified stack: resolve bootstrap ∪ eligible CSD на mapper → snapshot/content контроллеры на `mgr` → `unifiedruntime.Syncer` → `AddCustomSnapshotDefinitionControllerToManager(..., syncer.Sync, graphRegistryRefresh)` (как в `cmd/main.go`, без дублирования второго `SetupWithManager` для тех же имён контроллеров).

Envtest integration не проверяет реальный Kubernetes RBAC enforcement: `AccessGranted` в этих тестах симулирует handshake внешнего RBAC controller/hook. Real-cluster smoke/e2e должны явно применять test-only RBAC для domain resources до `AccessGranted=True` (см. [`pre-e2e-smoke-validation.md`](pre-e2e-smoke-validation.md)).

Latest manual pre-e2e smoke status: passed on 2026-04-29 with test-only domain RBAC, namespace-relative aggregated API output, and expected retained SnapshotContent/ObjectKeeper artifacts after cleanup. Non-blocking findings to keep visible in reports: transient `ObjectKeeper already exists` can appear on repeated runs with retained artifacts; Kubernetes warns that the current `Snapshot` finalizer name should include a path.

**CSD-gated demo activation:** graph registry built-ins содержат только `Snapshot`→`SnapshotContent`. Demo VM/Disk controllers стартуют в harness всегда, но demo resources входят в `Snapshot` tree только через eligible CSD. Integration покрывает три границы: без demo CSD нет demo children; после hot-add CSD новый `Snapshot` создаёт demo child; manual `DemoVirtualDiskSnapshot` materializes без CSD.

**Domain controller status contract:** snapshot controllers set `status.conditions[type=PlanningReady]=True` with `observedGeneration == metadata.generation` before `GenericSnapshotBinderController` binds `SnapshotContent`. Tests and smoke must not use the superseded `GraphReady` / `HandledByCustomSnapshotController` / `HandledByDomainSpecificController` / `ChildrenSnapshotReady` condition names. Публичная модель условий — `PlanningReady`, `ManifestsReady`, `VolumeReady`, `ChildrenReady`, `Ready` (spec §3.8/§3.9.7).

| Файл | Что проверяет |
|------|----------------|
| `csd_api_smoke_test.go` | Схема + CRD Established; `Accepted=True` после resolve; `Ready` после симуляции `AccessGranted`. Маппинг на **RegistrationTest**\* CRD (не TestSnapshot), чтобы не пересекаться с lifecycle-спеками и hot-add по одному snapshot kind. |
| `csd_reconciler_kindconflict_test.go` | Два CSD → `KindConflict` |
| `csd_reconciler_invalidspec_test.go` | Namespace-scoped content → `InvalidSpec`; дубликат snapshot kind в одном CSD → `InvalidSpec` |
| `unified_runtime_hot_add_test.go` | **R3 proof:** CSD создаётся после старта manager; после `AccessGranted` — `unifiedSyncer.ActiveSnapshotGVKKeys` и `LastLayeredState()` (resolved + eligible). **`Serial`**; очистка конфликтующих CSD (в т.ч. rbac/eligibility/smoke). |
| `unified_runtime_rbac_eligibility_test.go` | **T4 + eligibility:** без AccessGranted нет eligible-слоя для RegistrationTest; после снятия AccessGranted resolved без пары, monotonic active сохраняет ключ. **`Serial`**; `AfterEach` чистит CSD. |
| `csd_gated_domain_activation_test.go` | Demo domain graph activation: without CSD → no demo children; hot-add CSD → new root sees demo child; manual demo snapshot works without CSD. |
| `controller_registration_test.go` | Конструирование контроллеров как в production; **без** повторного `SetupWithManager` на общем `mgr` |
| `snapshot_lifecycle_test.go` | **N1 skeleton:** `Snapshot` → `SnapshotContent`, `status.boundSnapshotContentName` (unified root bind field), Ready через conditions (без ObjectKeeper / полного N2) |
| `snapshot_deletion_test.go` | **Delete flow:** Retain — snapshot gone, SnapshotContent остаётся; Delete policy — root finalizer только после `NotFound` на content; **retained unified** — после delete snapshot остаются SnapshotContent+MCP (MCR уже снят после capture); проверки контракта root OK (`followObjectRef`→Snapshot; root **SnapshotContent** controller `ownerRef`→OK) и MCP→SnapshotContent; **MCR `ownerRef`→Snapshot** — сценарий «удаление root при живом MCR»: delete с **`DeletePropagationBackground`** (foreground без kube-controller-manager зависает); ожидается **NotFound** на snapshot; MCR — **NotFound** на кластере с GC или (plain envtest) объект может остаться с тем же **`ownerRef`** до появления GC; узкий сценарий — пользователь удаляет SnapshotContent после удаления snapshot (deletion завершается, без контракта GC артефактов) |
| `snapshot_n1_boundary_test.go` | **Формальное закрытие N1:** **recovery** — после сброса `status` при существующем deterministic content снова `Bound`+`Ready`; короткая **стабильность** (Consistently) |
| `snapshot_recreate_test.go` | **§4.7 / отдельный lifecycle MCR:** после первого **Ready** MCR уже снят; удаление root; второй snapshot с тем же `metadata.name` — новый UID, новый SnapshotContent + новый MCR (`nss-{uid2}`), **Ready**; старый Retain SnapshotContent остаётся; имя MCR зависит от UID, коллизий нет |
| `snapshot_capture_plan_drift_test.go` | **N2a §4.7:** при frozen **MCR.spec.targets** добавление allowlisted объекта в namespace → **CapturePlanDrift** на root snapshot (без молчаливого `Update` **MCR.spec.targets**); **MCR** остаётся в API для ручного удаления / retry; пока MCR жив — **`ownerRef`→`Snapshot`** |
| `snapshot_graph_integration_test.go` | **subtree exclude + children-readiness propagation** на **реальном** графе: test fixture/controller path заполняет parent-owned **`childrenSnapshotRefs`**, а snapshot controller публикует **`childrenSnapshotContentRefs`**; child берётся как **registered snapshot kind fixture** (Snapshot kind, без synthetic-семантики). **Subtree exclude** — первый root **MCR** не создаётся, пока exclude по subtree нельзя посчитать (child **MCP** / **`manifestCheckpointName`**); root **MCR** не листит объект, уже в descendant **MCP**; common SnapshotContent MCPs учитываются тем же content graph traversal; **children readiness** — **`ChildrenPending`**, приоритет **`SubtreeManifestCapturePending`** vs child pending, **`ChildrenFailed`** при терминальном **`CapturePlanDrift`** на child **MCR** (terminal child-Snapshot failure bridge), каскад **`FinalizerParentProtect`** / снятие при удалении parent SnapshotContent (см. сценарии в файле) |
| Unit: `child_snapshot_terminal_failures_test.go`, `child_snapshot_resolve_test.go`; `snapshot_child_snapshot_watches_test.go` | **[`spec/system-spec.md`](../spec/system-spec.md) §3.2:** дерево run **namespace-local**; форма ref — **`apiVersion/kind/name`** (без `namespace`), child namespace всегда от parent; relay **child→parent** без cluster-wide **`Snapshot` list** | children readiness / watches | ✅ |

**N2a (integration — план минимума, см. [`design/implementation-plan.md`](../design/implementation-plan.md) §2.4.1 и [`design/snapshot-controller.md`](../design/snapshot-controller.md) §4.4–§4.7, §5.2, §8.7):** happy path (namespace → **MCR→ManifestCheckpoint** → persisted result → **Ready**; root MCP may be empty but always exists; root MCP does not contain Kubernetes **Namespace** object; on SnapshotContent **`manifestCheckpointName`** is always set; root **без** MCR в status; MCR name по §4.7; **ownerRef MCR→Snapshot**); fail-closed / allowlist; **Retain** с **root OK** + execution OK для MCR; провал MCR/MCP; **удаление root при живом MCR** — на реальном кластере MCR убирает **kube-controller GC** после исчезновения snapshot из API (§5.2); в envtest без controller-manager интеграционный тест проверяет **NotFound** на snapshot при **background** delete и контракт **`ownerRef`**, а не обязательный **NotFound** на MCR; download одного снимка (**§8.7.1**, 409 если MCP не Ready, 500 при битой склейке); smoke **pagination** при list в capture-потоке.

**N2b:** дерево — **зарегистрированные snapshot-типы** и typed **`Snapshot`** как возможный child в refs, **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`**, snapshot controllers публикуют durable content graph, parent content **Ready** агрегируется `SnapshotContentController`, snapshot **Ready** только зеркалит bound content (**§11.1** design; нормативный минимум **§3.8** [`spec/system-spec.md`](../spec/system-spec.md)), **aggregated manifests download** на чтении; **PR4** — нормативный контракт HTTP/ошибок/обхода: [`spec/snapshot-aggregated-manifests-pr4.md`](../spec/snapshot-aggregated-manifests-pr4.md); generic HTTP read API — [`api/snapshot-read.md`](../api/snapshot-read.md): `curl .../{resource}/{name}/manifests` должен возвращать полный subtree для root snapshot и только child subtree для child snapshot, а duplicate object identity должен завершаться ошибкой, без silent dedup. Поставка и история PR — [`design/implementation-plan.md`](../design/implementation-plan.md) **§2.4.2**; контракт children readiness и приоритетов reason — интеграция **`snapshot_graph_integration_test.go`** + unit **`child_snapshot_terminal_failures_test.go`**; **PR5b** — `demovirtualmachinesnapshot_pr5b_test.go` (root **`Completed`** после готовности child contents, **без** искусственного leaf **Snapshot** под root; own MCP separation + read from root/VM/disk content). Для demo API parent/back-reference задаётся через ownerRef, а persisted graph — через parent-owned `status.childrenSnapshotRefs`. Далее PR4 по спеке.

## Планируемые тесты

**Бэклог integration:** T5, T8–T11 и др. — по необходимости. R5 + T4/eligibility — см. [`design/implementation-plan.md`](../design/implementation-plan.md).

**Порядок с M-треком:** сценарии **T6** и расширение **MCR spec** — по gate в [`design/implementation-plan.md`](../design/implementation-plan.md) §2.4 / §4 (**N2a** или явное исключение); закрытие **N2b** не обязано блокировать M1, если так зафиксировано в плане.

| ID | Тест | Связь | Статус |
|----|------|--------|--------|
| T1 | Нет production unified CRD в API — wiring без ошибки, ноль watch | S1–S2 | ✅ `unified_bootstrap_t1_test.go`, `pkg/unifiedbootstrap/gvk_test.go` |
| T2 | CSD + маппинг; **статусы** Accepted/AccessGranted/Ready; **активация watch** по формуле (eligible → Sync → layered + active) | R1–R3 | ✅ `csd_api_smoke_test.go`, `unified_runtime_hot_add_test.go`, `unified_runtime_rbac_eligibility_test.go`. ⬜ расширение: T5 (spec/delete CSD) |
| T3 | Два CSD, конфликт kind | R4 | ✅ `csd_reconciler_kindconflict_test.go` |
| T4 | Без `AccessGranted` пара не в `EligibleFromCSD` / не eligible для merge (не проверяем monotonic `active`) | R3 | ✅ `unified_runtime_rbac_eligibility_test.go` |
| T5 | Декомпозиция update/delete CSD: смена desired GVK в spec, удаление CSD, смена поколения статуса, последовательные apply нескольких CSD | R2, spec | ⬜ |
| T6 | MCR расширенный выбор | M1–M2 | ⬜ |
| T7 | Только MCR, без unified | S1–S2 | ⬜ |
| T8 | Исчезновение CRD | D3, ADR | ⬜ |
| T9 | Устаревшее observedGeneration | R3 | ⬜ |
| T10 | RBAC drift / 403, изоляция по типу | — | ⬜ |
| T11 | Два CSD, изоляция при поломке одного | опционально | ⬜ |

## Status propagation and progress/degradation visibility

Дизайн — [`design/status-propagation-and-visibility.md`](../design/status-propagation-and-visibility.md); контракт — [`spec/system-spec.md` §3.9.10 / §3.9.10.1](../spec/system-spec.md). Фазы: **Phase 1** (progress-aware `Ready=False`) — реализована; **Phase 2a** (MCP/VSC damaged-artifact wake-up/revalidation) — реализована. Тяжёлый сквозной live-e2e (delete MCP/VSC у Ready-дерева на кластере) — gate ниже.

| ID | Сценарий | Уровень | Статус |
|----|----------|---------|--------|
| P1-U1 | content без `manifestCheckpointName` → `ManifestsReady=False`/`ManifestCapturePending`, `VolumeReady=Unknown`/`ManifestCapturePending`, `Ready=False` | unit | ✅ `controller_data_readiness_test.go` / `controller_test.go` |
| P1-U2 | data pending count → `DataCapturePending`, message `"<ready>/<total> ready"` + capped pending list | unit | ✅ `controller_data_readiness_test.go`, `progress_visibility_test.go` |
| P1-U3 | children pending count → `ChildrenPending`, message `"<ready>/<total> ready"` | unit | ✅ `controller_test.go` |
| P1-U4 | terminal child failure → `ChildrenFailed` leaf-chain (`leaf=/reason=/message=`), 3 уровня root→vm→disk | unit | ✅ `progress_visibility_test.go`, `controller_test.go` |
| P1-U5 | already `Ready=True` content, reconcile видит missing published artifact → `VolumeReady=False`/`ArtifactMissing`/`Ready=False`, kind/name в message (без watch) | unit | ✅ `progress_visibility_test.go` |
| P1-U6 | Snapshot mirror: pre-bind fallback → `ContentBindingPending`; после bind — verbatim mirror content.Ready | unit | ✅ `ready_mirror_test.go` |
| P1-I1 | bound Snapshot зеркалит content Ready после status-only обновления content | integration | ✅ `snapshot_content_ready_propagation_test.go` |
| P1-E1 | e2e: во время создания root `Ready=False` с осмысленным reason/message; на ожидании детей message содержит count; финал `Ready=True/Completed` | e2e | ⬜ gate с Phase 2a live |
| P2a-U1 | MCP NotFound → `ManifestCapturePending` (non-terminal); MCP `Ready=False/Failed` → `ManifestCheckpointFailed` (terminal); non-terminal false / no Ready cond → pending | unit | ✅ `phase2a_wakeup_test.go`, `content_aggregation_test.go` |
| P2a-U2 | wake-up map: artifact с ownerRef `SnapshotContent` → один reconcile.Request; без ownerRef или foreign-only (MCP и VSC) → nil (enqueue-only) | unit | ✅ `phase2a_wakeup_test.go` |
| P2a-U3 | VSC ownerRef self-heal: добавляет ownerRef, сохраняет foreign ref; already-correct → без Patch (idempotent); missing VSC — no-op; deleting VSC не патчится | unit | ✅ `phase2a_wakeup_test.go` |
| P2a-U4 | propagation: leaf `ArtifactMissing` → parent `ChildrenFailed` (terminal), ready-sibling нетронут; leaf `DataCapturePending` → parent `ChildrenPending` (non-terminal) | unit | ✅ `phase2a_wakeup_test.go` |
| P2a-U5 | MCP `Ready=True`, chunk из `MCP.status.chunks[]` missing → `ManifestCheckpointFailed`, message с mcp+chunk (exact GET, no list/watch); все chunks на месте → ready; transient chunk GET error → reconcile error (requeue), не терминал | unit | ✅ `phase2a_wakeup_test.go` |
| P2a-U6 | data leg: VSC NotFound → `ArtifactMissing`; `readyToUse=false`/поле отсутствует → `DataCapturePending` с `X/Y ready`; `readyToUse=true` → ready; прогресс по нескольким VSC | unit | ✅ `controller_data_readiness_test.go` |
| P2a-I1 | Ready-лист с Ready MCP; MCP флипает `Ready=False/Failed`; MCP-watch будит content → `ManifestsReady=False`/`ManifestCheckpointFailed`/`Ready=False` (без правки content); затем MCP назад `Ready=True` → recovery до `Ready=True/Completed` (I1R) | integration/envtest | ✅ `snapshotcontent_mcp_degradation_wakeup_test.go` |
| P2a-I2 | Ready-лист (MCP Ready + chunk); chunk удалён (сам не будит) + bump MCP → reconcile видит missing chunk → `ManifestCheckpointFailed` с именем chunk | integration/envtest | ✅ `snapshotcontent_mcp_degradation_wakeup_test.go` |
| P2a-I3 | Ready data-лист (Ready MCP + ready VSC); `readyToUse=false` → VSC-watch будит content → `DataCapturePending`/`Ready=False`; `readyToUse=true` → recovery до `Ready=True` | integration/envtest (guard: VSC CRD) | ✅ `snapshotcontent_vsc_degradation_wakeup_test.go` |
| P2a-I4 | Ready data-лист; VSC удалён → VSC-watch будит content → `ArtifactMissing`/`Ready=False` (terminal requests) | integration/envtest (guard: VSC CRD) | ✅ `snapshotcontent_vsc_degradation_wakeup_test.go` |
| P2a-I5 | VSC ownerRef self-heal на reconcile: снять `SnapshotContent` ownerRef (оставив foreign) + bump content → ownerRef восстановлен, foreign сохранён | integration/envtest (guard: VSC CRD) | ✅ `snapshotcontent_vsc_degradation_wakeup_test.go` |
| P2a-E1 | live: на реальном кластере (tree-demo, depth ≥ 2) повредить leaf-артефакт (delete VSC / fail MCP / delete chunk+bump) → flip `Ready=False` вверх по дереву (`ChildrenFailed`/`ChildrenPending`) вплоть до verbatim-mirror в root `Snapshot`; sibling isolation; recovery после восстановления. Runbook ниже | live-e2e | ⬜ gate (cluster) |
| P2b-* | chunk wake-up (chunk→MCP→content watch, чтобы delete сам будил reconcile) + chunk content/consistency (checksum) в conditions; `ArtifactFailed` для деградировавшего VSC | watch + e2e | ⬜ отдельный ADR/RBAC |

> Примечание по envtest и VSC: integration/envtest сервит `VolumeSnapshotContent` CRD только когда на CRD-path добавлены sibling `storage-foundation/crds` (см. `integrationResolveFoundationCRDDir`, override `STORAGE_FOUNDATION_CRDS`). P2a-I3/I4/I5 при отсутствии VSC API делают `Skip` с явной причиной (семантика покрыта unit-уровнем `controller_data_readiness_test.go` и остаётся за live-gate P2a-E1).

#### P2a-E1 live runbook (cluster gate)

Выполняется на кластере с задеплоенным контроллером и tree-demo (root `Snapshot` → VM child → Disk leaf с MCP/VSC), не в envtest. Корректность на reconcile уже доказана unit+integration; live-gate проверяет сквозной mirror до root `Snapshot` и sibling isolation на реальном GC/RBAC.

> Автоматический staged-прогон шагов ниже: `hack/snapshot-tree-demo-e2e.sh`
> (стадии `00-preflight`..`11-chunk-missing`, включая failure-propagation /
> parent-invalidation блок `12-child-snapshot-deleted`, `13-snapshotcontent-deleted`,
> `14-mcp-deleted`, `17-child-ready-false`, `18-recovery` (recoverable) и терминальные
> `15-chunk-deleted`, `16-orphan-vsc-deleted`; артефакты `artifacts/tree-demo-<run-id>/`;
> ядро инвариантов — hard-fail, гоночное — `SOFT`). Описание стадий —
> [`snapshot-tree-demo-runbook.md` §9](snapshot-tree-demo-runbook.md#9-staged-diagnostic-hacksnapshot-tree-demo-e2esh). Шаги 1–6 ниже — ручной эквивалент.
>
> Стадии 12–18 проверяют инвариант **INV-FAIL-PROP** (`spec/system-spec.md` §3.8): parent
> `Ready=True` IFF все обязательные потомки/артефакты present+healthy. Recoverable-удаления
> (12/13/14/17) инвалидируют дерево и восстанавливают `Ready=True`; терминальные (15/16)
> намеренно деградируют дерево и идут после `18-recovery`. Удаление orphan VS visibility-leaf
> само по себе **не** инвалидирует, пока retained VSC жив (durable artifact, §3.9.11); инвалидирует
> именно потеря VSC (стадия 16).

1. Baseline: `kubectl get snapshot -n <ns> <root> -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}'` == `True`; root/VM/Disk `SnapshotContent` и sibling — `Ready=True`; в графе нет `MISSING`. Зафиксировать имена leaf MCP, VSC(ы) и `MCP.status.chunks[]`.
2. MCP Failed: `kubectl patch manifestcheckpoint <mcp> --subresource=status --type=merge -p '{"status":{"conditions":[{"type":"Ready","status":"False","reason":"Failed","message":"e2e injected MCP failure","lastTransitionTime":"<now>"}]}}'` → ждать leaf `ManifestsReady=False/ManifestCheckpointFailed`, вверх `ChildrenFailed`, root `Snapshot Ready=False` (mirror); затем patch назад `Ready=True` → full recovery.
3. VSC `readyToUse=false` (если допускает admission): patch VSC status → leaf `DataCapturePending`, вверх `ChildrenPending`, root `Snapshot Ready=False`; назад `true` → recovery. Если status patch заблокирован — использовать сценарий 4.
4. VSC deleted: `kubectl delete volumesnapshotcontent <vsc>` → leaf `ArtifactMissing`, вверх `ChildrenFailed`, root mirror; sibling остаётся `Ready=True`; после теста пересоздать namespace (удаление VSC может быть необратимым).
5. Chunk missing (декларированное ограничение): `kubectl delete manifestcheckpointcontentchunk <chunk>` затем bump MCP (`kubectl annotate manifestcheckpoint <mcp> e2e/bump=$(date +%s) --overwrite`) → leaf `ManifestCheckpointFailed` с именем chunk. Без bump статус может остаться `Ready=True` (chunk-watch намеренно не реализован в 2a).
6. Прогресс при создании (best-effort, тайминг гоночный): сразу после создания root — `Ready=False` с pending-reason/`ContentBindingPending`; пока артефакты/дети не готовы — message содержит `X/Y ready`; финал — `Ready=True/Completed`.

## Нефункциональные

- CI: `go_checks`.
- **Метрики unified runtime** (controller metrics endpoint): `state_snapshotter_unified_runtime_resolved_snapshot_gvk_count`, `state_snapshotter_unified_runtime_active_monotonic_snapshot_gvk_count`, `state_snapshotter_unified_runtime_stale_active_snapshot_gvk_count` — см. `pkg/unifiedruntime/metrics.go`.
- Нагрузка: большие MCR после M1 (M1 — после N-трека по текущему плану).

## Demo / remote validation

- Автоматизированная проверка на кластере и подготовка окружения — по мере внедрения сценариев; не удалять диагностические тесты без замены.
- Актуальный артефакт pre-e2e smoke (kubectl checklist, namespace-local refs, `sourceRef`, root/demo readiness, namespace-relative aggregated API): [`pre-e2e-smoke-validation.md`](pre-e2e-smoke-validation.md) — статус `pre-e2e-passed` на 2026-04-29.
- Детали деплоя контроллера и линтера: `.cursor/rules/controller-redeploy-and-remote-e2e.mdc`.
- Эксплуатация на кластере (CRD, метрики, stale, рестарт): [`../operations/runbook-degraded-and-unified-runtime.md`](../operations/runbook-degraded-and-unified-runtime.md).
