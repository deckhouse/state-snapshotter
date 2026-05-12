# Project status (high-level)

Краткий статус дорожной карты. Детали задач и таблицы — [`design/implementation-plan.md`](../design/implementation-plan.md). Тесты — [`testing/e2e-testing-strategy.md`](../testing/e2e-testing-strategy.md).

**Итог по треку R2 phase 2b + R3 (ядро):** в репозитории реализованы additive unified watches после reconcile CSD, явный **`LayeredGVKState`**, монотонные **active** keys, интеграционный proof (**`unified_runtime_hot_add_test.go`**), Prometheus gauges и **Info**-лог при **stale** active (см. `pkg/unifiedruntime/`). Симметричный **unwatch** без рестарта по-прежнему **не** обещается.

**Документация D1–D3:** точка входа [`../README.md`](../README.md); runbook [`runbook-degraded-and-unified-runtime.md`](runbook-degraded-and-unified-runtime.md); CSD/RBAC/MCR [`csd-rbac-and-mcr.md`](csd-rbac-and-mcr.md).

| Область | Статус |
|---------|--------|
| S1–S2 (optional CRD, bootstrap resolve) | ✅ |
| T1 (integration + unit bootstrap) | ✅ |
| R1 (CSD API + CRD YAML в репозитории) | ✅ |
| R2 phase 1 (CSD reconciler, Accepted/Ready, KindConflict/InvalidSpec; **без** подмены unified watch) | ✅ |
| R4 (KindConflict в reconciler, без panic) | ✅ |
| R2 phase 2a (eligible CSD → merge с bootstrap на старте процесса; не hot reload) | ✅ |
| R2 phase 2b (additive unified watches после CSD reconcile, без clean unwatch) | ✅ |
| R3 — **явный слой state** (`LayeredGVKState`, `activeSnapshotGVKKeys`, `LastLayeredState` / `ActiveSnapshotGVKKeys`) | ✅ |
| R3 — **proof hot-add** (интеграция: CSD eligible → Sync → layered + active keys) | ✅ |
| R3 — **observability** (Prometheus gauges + лог при stale active; hint на restart pod) | ✅ |
| D1–D3 (обзор, runbook, CSD/RBAC/MCR) | ✅ |
| R5 (unified rollout: env + Helm) | ✅ |
| N2b generic **[`spec/system-spec.md`](../spec/system-spec.md) §3** — **E1–E6** | **E1–E5** ✅. **E6** ✅: строгие **`childrenSnapshotRefs`** (**`apiVersion`/`kind`/`name`**) → child snapshot `Get` → bound child content; parent content `Ready` агрегируется `SnapshotContentController`, snapshot `Ready` зеркалит bound content; dynamic child watches + map child→parent. Integration **`snapshot_graph_e5_e6_integration_test.go`** / **PR5b**. **§3.8**. |
| N2b **PR5a** (demo disk snapshot + CSD + parent-owned graph) | ✅ минимум — обязательный **`spec.sourceRef`** на source **`DemoVirtualDisk`**, parent-owned top-level graph через **`Snapshot`** CSD discovery, ref-only walk через common content-tree walker; demo snapshot CRD в **`crds/`** |
| N2b **PR5b** (demo VM snapshot + disk child через **`ownerReference`** + `childrenSnapshotRefs`) | ✅ VM reconciler владеет disk child graph, child snapshot получает ownerRef на VM snapshot, полный planned graph публикуется в `status.childrenSnapshotRefs`; VM/Disk имеют собственные MCR/MCP materialization; root/VM final **`Ready`** зеркалит bound common content `Ready`; content-level aggregation по child contents + own MCP проверяется integration |
| M* | ⬜ по плану |

**N2b — generic [`spec/system-spec.md`](../spec/system-spec.md) §3 в коде:** **E1–E5** — как в таблице и [`design/implementation-plan.md`](../design/implementation-plan.md) **§3-E1–E5**. **E5:** root own MCR содержит только namespace-scoped allowlist objects и исключает объекты из descendant MCPs, включая common SnapshotContent nodes; Kubernetes **Namespace** object не захватывается. Если own scope пустой, создаётся empty MCP (0 objects), и `manifestCheckpointName` всё равно заполняется. **E6:** parent content `Ready` по строгим refs + **`Get`** по GVK; snapshot `Ready` только зеркалит bound content. Registry только для **E5** / graph walk при непустых **`childrenSnapshotRefs`** на exclude-пути. Дерево run **namespace-local** (**§3.2** spec): форма ref — **`apiVersion/kind/name`** (без `namespace`), child namespace всегда берётся от parent. Parent controllers владеют graph edges: `Snapshot` пишет top-level refs из CSD discovery, VM пишет refs на свои Disk children; child controllers не self-register. Кластерный smoke registry — **`hack/snapshot-graph-registry-smoke.sh`**.

**Always-on runtime:** `Snapshot`, common `SnapshotContent`, CSD reconciler, graph registry, `GenericSnapshotBinderController` / `unifiedruntime.Syncer`, and dynamic CSD hot-add path initialize on the single v0 runtime path.

**CSD-gated demo activation:** graph registry built-in по умолчанию содержит только `Snapshot`→ common content. Demo VM/Disk resources входят в `Snapshot` tree только через eligible CSD. Hot-add CSD покрыт для новых root snapshots; requeue уже существующих root после активации CSD остаётся follow-up при необходимости.

**Common `SnapshotContent` ownership:** `SnapshotContent` is now retained/self-contained: it has no live reverse reference to Snapshot, its spec is immutable after creation, and its durable result graph lives in status. Snapshot-domain controllers own planning/execution requests, write their own `XxxxSnapshot.status` (`boundSnapshotContentName`, request names, `childrenSnapshotRefs`, `GraphReady`, mirrored snapshot `Ready`), and publish result refs into bound `SnapshotContent.status` (`manifestCheckpointName`, future `dataRef`, `childrenSnapshotContentRefs`). `SnapshotContentController` validates those persisted refs, ensures artifact ownerRef handoff, and owns only content readiness conditions. MCR/VCR `Ready=True` means the artifact is ready and owned by `SnapshotContent`; cleanup request runs only after that explicit chain. `SnapshotContent.status.dataRef` points to a cluster final artifact (`apiVersion/kind/name`, v0 — `VolumeSnapshotContent`), not an execution request.

**Manifest payload security:** `ManifestCheckpointContentChunk` RBAC is internal-only: module templates grant chunks only to the controller/API service account with `create/get/delete`; user-facing read access is through `/manifests`; the API assembles payloads by reading chunks via the internal direct client, while direct chunk `get/list/watch` is not granted to users.

**Custom snapshot controller barrier:** CSD-registered custom snapshot controllers publish `status.conditions[type=HandledByCustomSnapshotController]=True` before `GenericSnapshotBinderController` binds common `SnapshotContent`. The previous `HandledByDomainSpecificController` condition name is superseded and not part of the active contract.

**Lifecycle ownerRef model:** any Snapshot kind can be a root run, including standalone demo Snapshot kinds. Root `SnapshotContent` has `ownerRef -> root ObjectKeeper`. Root ObjectKeeper follows root Snapshot via `spec.followObjectRef`. Root Snapshot itself is not owned by ObjectKeeper because Snapshot is namespaced and ObjectKeeper is cluster-scoped. Child Snapshot ownerRef points to parent Snapshot; child `SnapshotContent` ownerRef points to parent `SnapshotContent`; MCP / future data artifacts ownerRef points to the owning `SnapshotContent`. `SnapshotContent` must not be owned by short-lived Snapshot and must not be ownerless after reconcile convergence.

**Same-name root recreate:** retained root `ObjectKeeper` identity is bound to one snapshot UID through `ObjectKeeper.spec.followObjectRef.UID`. Same namespace/name root Snapshot cannot reuse retained root ObjectKeeper if that ObjectKeeper follows an old Snapshot UID. A new root Snapshot with the same namespace/name must fail closed or wait until old root ObjectKeeper expires/is removed.

**Latest pre-e2e smoke:** 2026-04-29 пройден на реальном кластере с test-only domain RBAC до `RBACReady=True` (no-CSD root, disk-only CSD, VM+Disk CSD, content tree, MCP/chunks, namespace-relative aggregated API output, negative API и cleanup). 2026-05-06 PR4 smoke после UID-aware MCR OK и retained-read hardening прошёл на текущем временном поведении `/snapshots/{name}/manifests` после root delete. **TODO:** долгосрочно retained manifests должны читаться через durable `/snapshotcontents/{contentName}/manifests`; deleted Snapshot name через root ObjectKeeper не является целевым retained read API.

**Для команды (одно предложение):** **E1–E6**: граф по **`children*Refs`**, E5 exclude через registry, E6 parent content **`Ready`** агрегируется `SnapshotContentController`, snapshot **`Ready`** только зеркалит bound content, child→parent через dynamic watches.

**Критерий «сделано до M-трека»:** ядро R2/R3, D1–D3, R5, точечные integration (RBAC / eligibility) — в коде; **все тесты зелёные**, включая `go test -tags=integration ./test/integration/...`.

**Текущий фокус:** **N1** по namespace-flow **формально закрыт** (см. [`design/implementation-plan.md`](../design/implementation-plan.md) §2.4 / §2.4.1). **N2a** — в коде и тестах; root OK — **`FollowObjectWithTTL`** + `spec.ttl`; ограничения (list без pagination и т.д.) — в плане и design. **N2b PR1–PR4** — в коде; PR4 SSOT — [`spec/snapshot-aggregated-manifests-pr4.md`](../spec/snapshot-aggregated-manifests-pr4.md); кластерный smoke — `hack/pr4-smoke.sh` (см. [`testing/e2e-testing-strategy.md`](../testing/e2e-testing-strategy.md)); **strict TTL cascade** — при необходимости с ObjectKeeper. **Generic §3:** **E1–E6** — в коде и интеграции (**§3-E6** в [`design/implementation-plan.md`](../design/implementation-plan.md), **§3.8** в [`spec/system-spec.md`](../spec/system-spec.md)); без параллельного раздувания **Ready**/TTL. **Demo domain (proposed):** **[§2.4.3](../design/implementation-plan.md)** / [`design/demo-domain-csd/README.md`](../design/demo-domain-csd/README.md). Продуктовое ТЗ — [`snapshot-rework/`](../../../snapshot-rework/). **M1/M2** — по gate плана §4. Обзор NS — [`design/snapshot-controller.md`](../design/snapshot-controller.md).

**Блокеры / rollout:** см. ADR §3 и plan §5; при критическом изменении обновляй этот файл и при необходимости `design/implementation-plan.md`.
