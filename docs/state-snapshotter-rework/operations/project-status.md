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
| N2b generic **[`spec/system-spec.md`](../spec/system-spec.md) §3** — срезы **E1–E4** (минимум перед PR5a) | ✅ см. блок ниже |
| N2b **PR5a** (demo **DemoVirtualDiskSnapshot** + DSC + merge **`children*Refs`** на root) | 🔄 минимум усилен — **`persistentVolumeClaimName`** в spec (идентичность PVC), ref-only walk с листьями **`DemoVirtualDiskSnapshotContent`** (**`WalkNamespaceSnapshotContentSubtreeWithDemoLeaves`** / skip в aggregated), проверка поставки CRD — **`hack/verify-module-bundle-includes-demo-crds.sh`**; **без** MCR/MCP/VolumeSnapshot, **без** PR5b/снятия synthetic |
| M* | ⬜ по плану |

**N2b — generic [`spec/system-spec.md`](../spec/system-spec.md) §3 в коде (минимум до PR5a, без E5/E6):** **E1** — merge-only **`children*Refs`** (snapshot + content), интеграция на неперетирание чужого ref (**INV-REF-M1**). **E2** — owned prune synthetic при выключенном дереве, merge-safe remove + **`RetryOnConflict`**, unit + integration (**INV-REF-M2**). **E3** — PR4 aggregated traversal только по **`childrenSnapshotContentRefs`**, без list-fallback (**INV-REF-C1**); godoc + unit «несвязанный NSC не обходится». **E4** — общий ref-only DFS **`usecase.WalkNamespaceSnapshotContentSubtree`** (сортировка детей, **`ErrNamespaceSnapshotContentCycle`**), тот же обход подключён к aggregated manifests; листья demo-content — **`WalkNamespaceSnapshotContentSubtreeWithDemoLeaves`** / skip в aggregated (PR5a). Модель графа — **дерево** (повторный вход в узел с разных родителей сейчас трактуется как цикл; **DAG / shared subtrees** — вне текущего контракта).

**Критерий «сделано до M-трека»:** ядро R2/R3, D1–D3, R5, точечные integration (RBAC / eligibility) — в коде; **все тесты зелёные**, включая `go test -tags=integration ./test/integration/...`.

**Текущий фокус:** **N1** по namespace-flow **формально закрыт** (см. [`design/implementation-plan.md`](../design/implementation-plan.md) §2.4 / §2.4.1). **N2a** — в коде и тестах; root OK — **`FollowObjectWithTTL`** + `spec.ttl`; ограничения (list без pagination и т.д.) — в плане и design. **N2b PR1–PR4** — в коде; PR4 SSOT — [`spec/namespace-snapshot-aggregated-manifests-pr4.md`](../spec/namespace-snapshot-aggregated-manifests-pr4.md); кластерный smoke — `hack/pr4-smoke.sh` (см. [`testing/e2e-testing-strategy.md`](../testing/e2e-testing-strategy.md)); **strict TTL cascade** — при необходимости с ObjectKeeper. **Generic §3 E1–E4** — минимум в коде (блок выше). **Следующий шаг:** при необходимости добить **PR5a** (MCR/MCP, кластерный smoke aggregated) или перейти к **PR5b** / **§3-E5/E6** и снятию synthetic по **[§2.4.2](../design/implementation-plan.md)** после merge-gate — **[§2.4.4](../design/implementation-plan.md)**; без параллельного раздувания **Ready**/TTL. **Demo domain (proposed):** **[§2.4.3](../design/implementation-plan.md)** / [`design/demo-domain-dsc/README.md`](../design/demo-domain-dsc/README.md). Продуктовое ТЗ — [`snapshot-rework/`](../../../snapshot-rework/). **M1/M2** — по gate плана §4. Обзор NS — [`design/namespace-snapshot-controller.md`](../design/namespace-snapshot-controller.md).

**Блокеры / rollout:** см. ADR §3 и plan §5; при критическом изменении обновляй этот файл и при необходимости `design/implementation-plan.md`.
