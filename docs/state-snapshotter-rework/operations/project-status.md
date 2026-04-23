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
| N2b generic **[`spec/system-spec.md`](../spec/system-spec.md) §3** — **E1–E6** | **E1–E5** ✅ как ранее. **E6** ✅ (finish line этого среза): единый fail-closed parent **`Ready`** по **`childrenSnapshotRefs`** (`usecase` summarize + **`PickParentReadyReasonE6`**: **`ChildSnapshotFailed`** > **`SubtreeManifestCapturePending`** > **`ChildSnapshotPending`** > **`Completed`**); wiring в **`namespacesnapshot_capture.go`** + synthetic subtree scaffold (без удаления synthetic); unit **`namespace_snapshot_parent_ready_e6_test.go`**; integration synthetic tree / synthetic child failure / **PR5b** root **`Completed`** после child. Детали — **§3-E6** в [`design/implementation-plan.md`](../design/implementation-plan.md), **§3.8** в [`spec/system-spec.md`](../spec/system-spec.md). |
| N2b **PR5a** (demo **DemoVirtualDiskSnapshot** + DSC + merge **`children*Refs`** на root) | ✅ минимум — **`persistentVolumeClaimName`**, ref-only walk через **`WalkNamespaceSnapshotContentSubtreeWithRegistry`** + integration graph **`GVKRegistry`**; demo CRD в **`crds/`** |
| N2b **PR5b** (demo **DemoVirtualMachineSnapshot** + диск под VM через **`parentDemoVirtualMachineSnapshotRef`**) | ✅ минимум в коде — **`DemoVirtualMachineSnapshotReconciler`**, registry-based ref-walk, интеграция **`demovirtualmachinesnapshot_pr5b_test.go`** (листовой **NamespaceSnapshot** `vm-run` под ref домена + root **`Ready`/`Completed`** по **E6**); **без** снятия synthetic |
| M* | ⬜ по плану |

**N2b — generic [`spec/system-spec.md`](../spec/system-spec.md) §3 в коде:** **E1–E5** — как в таблице и [`design/implementation-plan.md`](../design/implementation-plan.md) **§3-E1–E5**. **E6** — finish line: единая агрегация parent **`Ready`** по **`childrenSnapshotRefs`** и фиксированный приоритет reason (**§3-E6** / **§3.8** spec). Ограничения: пустые **`childrenSnapshotRefs`** — прежний полный list; без registry при непустых refs — fail-closed. Кластерный smoke registry — **`hack/snapshot-graph-registry-smoke.sh`**.

**Для команды (одно предложение):** **E6** закрыт на согласованном finish line (generic parent **`Ready`** + тесты + spec **§3.8**); **снятие synthetic** — отдельный cleanup PR по **§2.4.2** после merge-gate на demo flow.

**Критерий «сделано до M-трека»:** ядро R2/R3, D1–D3, R5, точечные integration (RBAC / eligibility) — в коде; **все тесты зелёные**, включая `go test -tags=integration ./test/integration/...`.

**Текущий фокус:** **N1** по namespace-flow **формально закрыт** (см. [`design/implementation-plan.md`](../design/implementation-plan.md) §2.4 / §2.4.1). **N2a** — в коде и тестах; root OK — **`FollowObjectWithTTL`** + `spec.ttl`; ограничения (list без pagination и т.д.) — в плане и design. **N2b PR1–PR4** — в коде; PR4 SSOT — [`spec/namespace-snapshot-aggregated-manifests-pr4.md`](../spec/namespace-snapshot-aggregated-manifests-pr4.md); кластерный smoke — `hack/pr4-smoke.sh` (см. [`testing/e2e-testing-strategy.md`](../testing/e2e-testing-strategy.md)); **strict TTL cascade** — при необходимости с ObjectKeeper. **Generic §3:** **E1–E6** — в коде и интеграции (**§3-E6** в [`design/implementation-plan.md`](../design/implementation-plan.md), **§3.8** в [`spec/system-spec.md`](../spec/system-spec.md)); снятие synthetic по **[§2.4.2](../design/implementation-plan.md)** после merge-gate — **[§2.4.4](../design/implementation-plan.md)**; без параллельного раздувания **Ready**/TTL. **Demo domain (proposed):** **[§2.4.3](../design/implementation-plan.md)** / [`design/demo-domain-dsc/README.md`](../design/demo-domain-dsc/README.md). Продуктовое ТЗ — [`snapshot-rework/`](../../../snapshot-rework/). **M1/M2** — по gate плана §4. Обзор NS — [`design/namespace-snapshot-controller.md`](../design/namespace-snapshot-controller.md).

**Блокеры / rollout:** см. ADR §3 и plan §5; при критическом изменении обновляй этот файл и при необходимости `design/implementation-plan.md`.
