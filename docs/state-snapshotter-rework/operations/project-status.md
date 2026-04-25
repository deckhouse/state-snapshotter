# Project status (high-level)

Краткий статус дорожной карты. Детали задач и таблицы — [`design/implementation-plan.md`](../design/implementation-plan.md). Тесты — [`testing/e2e-testing-strategy.md`](../testing/e2e-testing-strategy.md).

**Итог по треку R2 phase 2b + R3 (ядро):** в репозитории реализованы additive unified watches после reconcile DSC, явный **`LayeredGVKState`**, монотонные **active** keys, интеграционный proof (**`unified_runtime_hot_add_test.go`**), Prometheus gauges и **Info**-лог при **stale** active (см. `pkg/unifiedruntime/`). Симметричный **unwatch** без рестарта по-прежнему **не** обещается.

**Документация D1–D3:** точка входа [`../README.md`](../README.md); runbook [`runbook-degraded-and-unified-runtime.md`](runbook-degraded-and-unified-runtime.md); DSC/RBAC/MCR [`dsc-rbac-and-mcr.md`](dsc-rbac-and-mcr.md).

| Область | Статус |
|---------|--------|
| S1–S2 (optional CRD, bootstrap resolve) | ✅ |
| T1 (integration + unit bootstrap) | ✅ |
| R1 (DSC API + CRD YAML в репозитории) | ✅ |
| R2 phase 1 (DSC reconciler, Accepted/Ready, KindConflict/InvalidSpec; **без** подмены unified watch) | ✅ |
| R4 (KindConflict в reconciler, без panic) | ✅ |
| R2 phase 2a (eligible DSC → merge с bootstrap на старте процесса; не hot reload) | ✅ |
| R2 phase 2b (additive unified watches после DSC reconcile, без clean unwatch) | ✅ |
| R3 — **явный слой state** (`LayeredGVKState`, `activeSnapshotGVKKeys`, `LastLayeredState` / `ActiveSnapshotGVKKeys`) | ✅ |
| R3 — **proof hot-add** (интеграция: DSC eligible → Sync → layered + active keys) | ✅ |
| R3 — **observability** (Prometheus gauges + лог при stale active; hint на restart pod) | ✅ |
| D1–D3 (обзор, runbook, DSC/RBAC/MCR) | ✅ |
| R5 (unified rollout: env + Helm) | ✅ |
| N2b generic **[`spec/system-spec.md`](../spec/system-spec.md) §3** — **E1–E6** | **E1–E5** ✅. **E6** ✅: строгие **`childrenSnapshotRefs`** (**`apiVersion`/`kind`/`name`**) → **`GetChildSnapshot`**; **`SummarizeChildrenSnapshotRefsForParentReadyE6`** без registry; **`PickParentReadyReasonE6`**; dynamic child watches + map child→parent; wiring **`namespacesnapshot_capture.go`**. Unit **`namespace_snapshot_parent_ready_e6_test.go`**, **`namespacesnapshot_child_snapshot_watches_test.go`**; integration **`namespacesnapshot_graph_e5_e6_integration_test.go`**; **PR5b**. **§3.8**. |
| N2b **PR5a** (demo **DemoVirtualDiskSnapshot** + DSC + parent-owned graph) | ✅ минимум — **`persistentVolumeClaimName`**, parent-owned top-level graph через **`NamespaceSnapshot`** DSC discovery, ref-only walk через **`WalkNamespaceSnapshotContentSubtreeWithRegistry`** + integration graph **`GVKRegistry`**; demo CRD в **`crds/`** |
| N2b **PR5b** (demo **DemoVirtualMachineSnapshot** + диск под VM через единый **`parentSnapshotRef`**) | ✅ **`DemoVirtualMachineSnapshotReconciler`** владеет disk child graph, VM/Disk имеют собственные MCR/MCP materialization; root/VM **`Ready`** агрегируется по **`childrenSnapshotRefs`** (без фиктивного leaf **NamespaceSnapshot** под root) |
| M* | ⬜ по плану |

**N2b — generic [`spec/system-spec.md`](../spec/system-spec.md) §3 в коде:** **E1–E5** — как в таблице и [`design/implementation-plan.md`](../design/implementation-plan.md) **§3-E1–E5**. **E6:** parent **`Ready`** по строгим refs + **`Get`** по GVK; registry только для **E5** / graph walk при непустых **`childrenSnapshotRefs`** на exclude-пути. Дерево run **namespace-local** (**§3.2** spec): форма ref — **`apiVersion/kind/name`** (без `namespace`), child namespace всегда берётся от parent. Parent controllers владеют graph edges: `NamespaceSnapshot` пишет top-level refs из DSC discovery, VM пишет refs на свои Disk children; child controllers не self-register. Пустые **`childrenSnapshotRefs`** — полный namespace **list для manifest capture** (не смешивать с графом E6). Кластерный smoke registry — **`hack/snapshot-graph-registry-smoke.sh`**.

**Для команды (одно предложение):** **E1–E6**: граф по **`children*Refs`**, E5 exclude через registry, E6 parent **`Ready`** через строгий ref + **`unstructured`**, child→parent через dynamic watches.

**Критерий «сделано до M-трека»:** ядро R2/R3, D1–D3, R5, точечные integration (RBAC / eligibility) — в коде; **все тесты зелёные**, включая `go test -tags=integration ./test/integration/...`.

**Текущий фокус:** **N1** по namespace-flow **формально закрыт** (см. [`design/implementation-plan.md`](../design/implementation-plan.md) §2.4 / §2.4.1). **N2a** — в коде и тестах; root OK — **`FollowObjectWithTTL`** + `spec.ttl`; ограничения (list без pagination и т.д.) — в плане и design. **N2b PR1–PR4** — в коде; PR4 SSOT — [`spec/namespace-snapshot-aggregated-manifests-pr4.md`](../spec/namespace-snapshot-aggregated-manifests-pr4.md); кластерный smoke — `hack/pr4-smoke.sh` (см. [`testing/e2e-testing-strategy.md`](../testing/e2e-testing-strategy.md)); **strict TTL cascade** — при необходимости с ObjectKeeper. **Generic §3:** **E1–E6** — в коде и интеграции (**§3-E6** в [`design/implementation-plan.md`](../design/implementation-plan.md), **§3.8** в [`spec/system-spec.md`](../spec/system-spec.md)); без параллельного раздувания **Ready**/TTL. **Demo domain (proposed):** **[§2.4.3](../design/implementation-plan.md)** / [`design/demo-domain-dsc/README.md`](../design/demo-domain-dsc/README.md). Продуктовое ТЗ — [`snapshot-rework/`](../../../snapshot-rework/). **M1/M2** — по gate плана §4. Обзор NS — [`design/namespace-snapshot-controller.md`](../design/namespace-snapshot-controller.md).

**Блокеры / rollout:** см. ADR §3 и plan §5; при критическом изменении обновляй этот файл и при необходимости `design/implementation-plan.md`.
