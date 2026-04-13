# snapshot-rework

Здесь лежат **расширенные ADR и черновики** по unified snapshots, DSC и смежным темам. Нормативные выдержки для реализации и тестов — в **[`docs/state-snapshotter-rework/`](../docs/state-snapshotter-rework/)** (spec, design, testing, operations).

Визуальная схема unified snapshots (ownerRef vs логика, scope, ObjectKeeper): [`unified-snapshot-detailed.png`](unified-snapshot-detailed.png) · исходник [`unified-snapshot-detailed.drawio`](unified-snapshot-detailed.drawio). Ссылка также в шапке [`unified-origin.md`](unified-origin.md).

Актуальные **галочки по R1 / R2 phase 1 / R4** и следующий фокус (R2 phase 2 + R3) — в [`design/implementation-plan.md`](../docs/state-snapshotter-rework/design/implementation-plan.md) и [`operations/project-status.md`](../docs/state-snapshotter-rework/operations/project-status.md).

| Тема в старом указателе `snapshot-rework/plan/dorabotki-i-testy.md` (удалён) | Канонический документ |
|------------------------------------------------------------------------------|------------------------|
| §0–1 registry/runtime, контекст продукта | [`spec/system-spec.md`](../docs/state-snapshotter-rework/spec/system-spec.md) |
| §2, §4–5 дорожная карта, открытые вопросы | [`design/implementation-plan.md`](../docs/state-snapshotter-rework/design/implementation-plan.md) |
| §3 тесты, команды | [`testing/e2e-testing-strategy.md`](../docs/state-snapshotter-rework/testing/e2e-testing-strategy.md) |
| Прогресс / стадии | [`operations/project-status.md`](../docs/state-snapshotter-rework/operations/project-status.md) |

При смене контракта обновляй **`docs/state-snapshotter-rework/spec/system-spec.md`** и при необходимости соответствующий ADR в этом каталоге.

**N2b PR4 (aggregated manifests download):** человекочитаемое объяснение для митинга — [`2026-04-09-namespace-snapshot-pr4-aggregated-manifests.md`](2026-04-09-namespace-snapshot-pr4-aggregated-manifests.md); нормативный контракт — [`spec/namespace-snapshot-aggregated-manifests-pr4.md`](../docs/state-snapshotter-rework/spec/namespace-snapshot-aggregated-manifests-pr4.md).
