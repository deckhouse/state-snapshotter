# Historical design notes

This directory contains historical design notes and ADR drafts from earlier snapshot architecture iterations.
These documents may mention superseded concepts such as:

- `NamespaceSnapshot`
- `NamespaceSnapshotContent`
- `Demo*SnapshotContent`
- `contentCRDName`
- `SnapshotContent.spec.snapshotRef`
- `SnapshotContentController` owning all `SnapshotContent.status`

They are kept for context only and are not the current implementation contract.
Current source of truth:

- `docs/state-snapshotter-rework/spec/system-spec.md`
- `docs/state-snapshotter-rework/testing/pre-e2e-smoke-validation.md`
- `docs/state-snapshotter-rework/operations/project-status.md`

Legacy terms are allowed only in explicitly historical documents. Do not use historical documents as implementation instructions without checking the current spec.

---

# snapshot-rework

Здесь лежат **расширенные ADR и черновики** по unified snapshots, DSC и смежным темам. Нормативные выдержки для реализации и тестов — в **[`docs/state-snapshotter-rework/`](../docs/state-snapshotter-rework/)** (spec, design, testing, operations).

Визуальная схема unified snapshots (ownerRef vs логика, scope, ObjectKeeper): [`unified-snapshot-detailed.png`](unified-snapshot-detailed.png) · исходник [`unified-snapshot-detailed.drawio`](unified-snapshot-detailed.drawio). Ссылка также в шапке [`unified-origin.md`](unified-origin.md).

**Этапы и текущий фокус** — в [`design/implementation-plan.md`](../docs/state-snapshotter-rework/design/implementation-plan.md) и [`operations/project-status.md`](../docs/state-snapshotter-rework/operations/project-status.md).

| Тема в старом указателе `snapshot-rework/plan/dorabotki-i-testy.md` (удалён) | Канонический документ |
|------------------------------------------------------------------------------|------------------------|
| §0–1 registry/runtime, контекст продукта | [`spec/system-spec.md`](../docs/state-snapshotter-rework/spec/system-spec.md) |
| §2, §4–5 дорожная карта, открытые вопросы | [`design/implementation-plan.md`](../docs/state-snapshotter-rework/design/implementation-plan.md) |
| §3 тесты, команды | [`testing/e2e-testing-strategy.md`](../docs/state-snapshotter-rework/testing/e2e-testing-strategy.md) |
| Прогресс / стадии | [`operations/project-status.md`](../docs/state-snapshotter-rework/operations/project-status.md) |

При смене контракта обновляй **`docs/state-snapshotter-rework/spec/system-spec.md`** и при необходимости соответствующий ADR в этом каталоге.

**N2b PR4 (aggregated manifests download):** current normative contract — [`spec/snapshot-aggregated-read.md`](../docs/state-snapshotter-rework/spec/snapshot-aggregated-read.md) and [`api/snapshot-read.md`](../docs/state-snapshotter-rework/api/snapshot-read.md); разбиение по PR — [`design/implementation-plan.md`](../docs/state-snapshotter-rework/design/implementation-plan.md) §2.4.2; кластерный smoke — [`testing/e2e-testing-strategy.md`](../docs/state-snapshotter-rework/testing/e2e-testing-strategy.md).
