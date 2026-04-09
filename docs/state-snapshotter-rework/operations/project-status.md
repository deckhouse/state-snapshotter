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

**Текущий фокус:** **M1 → M2** (manifest); опционально расширение integration (полный T5, T9, …). **ValidatingWebhook** — не в ближайшем плане. Детали — [`design/implementation-plan.md`](../design/implementation-plan.md). При смене контракта обновляй [`README.md`](../README.md), [`runbook-degraded-and-unified-runtime.md`](runbook-degraded-and-unified-runtime.md), [`dsc-rbac-and-mcr.md`](dsc-rbac-and-mcr.md).

**Блокеры / rollout:** см. ADR §3 и plan §5; при критическом изменении обновляй этот файл и при необходимости `design/implementation-plan.md`.
