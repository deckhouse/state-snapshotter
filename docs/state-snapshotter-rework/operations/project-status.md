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
| N2b generic **[`spec/system-spec.md`](../spec/system-spec.md) §3** — **E1–E4** + **E5** (root MCR exclude по run-графу); **E6** не начат | ✅ E1–E5 см. блок ниже |
| N2b **PR5a** (demo **DemoVirtualDiskSnapshot** + DSC + merge **`children*Refs`** на root) | ✅ минимум — **`persistentVolumeClaimName`**, ref-only walk (**`WalkNamespaceSnapshotContentSubtreeWithDemoLeaves`**); demo CRD в **`crds/`** |
| N2b **PR5b** (demo **DemoVirtualMachineSnapshot** + диск под VM через **`parentDemoVirtualMachineSnapshotRef`**) | ✅ минимум в коде — **`DemoVirtualMachineSnapshotReconciler`**, расширенный ref-walk (**`WalkNamespaceSnapshotContentSubtreeWithAllDemoLeaves`**), интеграция **`demovirtualmachinesnapshot_pr5b_test.go`**; **без** Ready-каскада VM, **без** снятия synthetic |
| M* | ⬜ по плану |

**N2b — generic [`spec/system-spec.md`](../spec/system-spec.md) §3 в коде (минимум до PR5a; **E6** отдельно):** **E1** — merge-only **`children*Refs`** (snapshot + content), интеграция на неперетирание чужого ref (**INV-REF-M1**). **E2** — owned prune synthetic при выключенном дереве, merge-safe remove + **`RetryOnConflict`**, unit + integration (**INV-REF-M2**). **E3** — PR4 aggregated traversal только по **`childrenSnapshotContentRefs`**, без list-fallback (**INV-REF-C1**); godoc + unit «несвязанный NSC не обходится». **E4** — общий ref-only DFS **`usecase.WalkNamespaceSnapshotContentSubtree`** (сортировка детей, **`ErrNamespaceSnapshotContentCycle`**), тот же обход подключён к aggregated manifests; demo **content** узлы — **`WalkNamespaceSnapshotContentSubtreeWithDemoLeaves`** / **`WithAllDemoLeaves`** (PR5a/PR5b) / skip в aggregated. Модель графа — **дерево** (повторный вход в узел с разных родителей сейчас трактуется как цикл; **DAG / shared subtrees** — вне текущего контракта). **E5** — root **`NamespaceSnapshot`** manifest capture: при непустых **`status.childrenSnapshotRefs`** вычитание объектов из descendant **`NamespaceSnapshotContent`** MCP только по обходу **`childrenSnapshotContentRefs`** (+ demo leaves); валидация, что каждый child ref резолвится в узел, достижимый этим обходом; ошибки графа/MCP — fail-closed (**INV-S0** / **INV-E1**); unit — **`root_capture_run_exclude_test.go`**, **`pkg/namespacemanifest/filter_targets_test.go`**. **Integration proof** (полный reconcile → root MCR/MCP без объекта из child MCP) ещё не добавлен — **follow-up PR** на устранение гонки вокруг первого MCR (**CapturePlanDrift** / жизненный цикл MCR; см. **§3-E5** в **`implementation-plan.md`**). Ограничения среза: пустые **`childrenSnapshotRefs`** — прежний полный list; резолв child — пока три фиксированных типа (см. godoc **`root_capture_run_exclude.go`**).

**Критерий «сделано до M-трека»:** ядро R2/R3, D1–D3, R5, точечные integration (RBAC / eligibility) — в коде; **все тесты зелёные**, включая `go test -tags=integration ./test/integration/...`.

**Текущий фокус:** **N1** по namespace-flow **формально закрыт** (см. [`design/implementation-plan.md`](../design/implementation-plan.md) §2.4 / §2.4.1). **N2a** — в коде и тестах; root OK — **`FollowObjectWithTTL`** + `spec.ttl`; ограничения (list без pagination и т.д.) — в плане и design. **N2b PR1–PR4** — в коде; PR4 SSOT — [`spec/namespace-snapshot-aggregated-manifests-pr4.md`](../spec/namespace-snapshot-aggregated-manifests-pr4.md); кластерный smoke — `hack/pr4-smoke.sh` (см. [`testing/e2e-testing-strategy.md`](../testing/e2e-testing-strategy.md)); **strict TTL cascade** — при необходимости с ObjectKeeper. **Generic §3 E1–E5** — в коде (блок выше). **Следующий шаг:** **§3-E6** (Ready-каскад / fail-closed поверх E5), снятие synthetic по **[§2.4.2](../design/implementation-plan.md)** после merge-gate — **[§2.4.4](../design/implementation-plan.md)**; при необходимости MCR/MCP / aggregated smoke для demo; без параллельного раздувания **Ready**/TTL. **Demo domain (proposed):** **[§2.4.3](../design/implementation-plan.md)** / [`design/demo-domain-dsc/README.md`](../design/demo-domain-dsc/README.md). Продуктовое ТЗ — [`snapshot-rework/`](../../../snapshot-rework/). **M1/M2** — по gate плана §4. Обзор NS — [`design/namespace-snapshot-controller.md`](../design/namespace-snapshot-controller.md).

**Блокеры / rollout:** см. ADR §3 и plan §5; при критическом изменении обновляй этот файл и при необходимости `design/implementation-plan.md`.
