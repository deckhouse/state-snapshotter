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
| M* | ⬜ по плану |

**Критерий «сделано до M-трека»:** ядро R2/R3, D1–D3, R5, точечные integration (RBAC / eligibility) — в коде; **все тесты зелёные**, включая `go test -tags=integration ./test/integration/...`.

**Текущий фокус:** **N1** по namespace-flow **формально закрыт** (см. [`design/implementation-plan.md`](../design/implementation-plan.md) §2.4 / §2.4.1). **N2a** (один root, manifests-only, MCR→MCP, download одного снимка, негативные integration: delete during capture, recreate root, CapturePlanDrift) — в коде и тестах в состоянии, близком к merge/review; ограничения (root OK без TTL-retention, list без pagination) задокументированы в плане и design. **N2b PR1** (форма графа в API/CRD) — в коде. **N2b PR2** (synthetic one-child tree, graph refs) и **N2b PR3** (агрегированный **Ready**: **`ChildSnapshotPending`** / **`ChildSnapshotFailed`** / **`Completed`**, helper + integration **`namespacesnapshot_synthetic_tree_test.go`**, **`namespacesnapshot_synthetic_child_failure_test.go`**) — в коде; **следующий шаг — N2b PR4** (aggregated download): нормативный контракт — [`spec/namespace-snapshot-aggregated-manifests-pr4.md`](../spec/namespace-snapshot-aggregated-manifests-pr4.md), план — **[§2.4.2](../design/implementation-plan.md)**. Продуктовое ТЗ — [`snapshot-rework/`](../../../snapshot-rework/). **M1/M2 (manifest)** — по gate в плане §4. Обзор NS — [`design/namespace-snapshot-controller.md`](../design/namespace-snapshot-controller.md).

**Блокеры / rollout:** см. ADR §3 и plan §5; при критическом изменении обновляй этот файл и при необходимости `design/implementation-plan.md`.
