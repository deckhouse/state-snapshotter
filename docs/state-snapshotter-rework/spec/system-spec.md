# System spec (normative excerpts — state-snapshotter)

Нормативный контракт для реализации и тестов. Полная детализация DSC / registry / RBAC — в ADR [`snapshot-rework/2026-01-23-unified-snapshots-registry.md`](../../../snapshot-rework/2026-01-23-unified-snapshots-registry.md); не дублировать длинные фрагменты здесь без необходимости — обновлять этот файл при изменении контракта.

Нумерация разделов ниже совпадает с бывшим указателем `snapshot-rework/plan/dorabotki-i-testy.md`: **§0** — registry/runtime, **§1** — контекст.

## §0. Registry state и runtime (watch)

Две подсистемы:

| Подсистема | Роль | Динамичность |
|------------|------|----------------|
| **Registry state** | Желаемый набор типов из DSC + discovery GVK/GVR | DSC reconciler + `pkg/unifiedruntime.BuildLayeredGVKState` (eligible → merge → resolve); снимок в `Syncer.lastState` |
| **Runtime watch activation** | Подписки controller-runtime на `*Snapshot` / `*SnapshotContent` | Quasi-dynamic; **снятие watch без рестарта** не гарантируется |

**Правило:** отключение или удаление типа может потребовать **рестарта pod** для консистентного cleanup watch. См. ADR *Registry state vs runtime*.

## §1. Контекст продукта (кратко)

- **S1–S2:** optional unified CRD могут отсутствовать; процесс не падает. Пары GVK отфильтровываются через `pkg/unifiedbootstrap` + `RESTMapper` (см. код и план в design/testing).
- **R1 ✅:** типы и CRD **`DomainSpecificSnapshotController`** (`api/v1alpha1`, `crds/`).
- **R2 phase 1 ✅:** reconciler в manager (`internal/controllers/domainspecificsnapshot_controller.go`): resolve маппинга по CRD, `Accepted` / производный `Ready`, `KindConflict` и `InvalidSpec` (в т.ч. namespace-scoped content); `RBACReady` пишет только hook.
- **R2 phase 2a ✅:** на старте процесса merge eligible DSC ∪ bootstrap → resolve mapper → начальные GVK для unified контроллеров (`cmd/main.go`, `pkg/dscregistry`, `pkg/unifiedbootstrap`).
- **R2 phase 2b ✅:** после успешного reconcile DSC — `pkg/unifiedruntime.Syncer.Sync`: пересчёт слоёв (`LayeredGVKState`) и additive `AddWatch*` для новых resolved пар; **clean unwatch не гарантируется** (ADR).
- **R3 ✅ (ядро):** явный слой state в `pkg/unifiedruntime`; интеграционный proof hot-add — `test/integration/unified_runtime_hot_add_test.go`; Prometheus gauges + лог при «stale» active (ключ есть в monotonic active, но выпал из resolved). **Опционально:** доп. proof-сценарии — по плану.
- **Цель (ядро):** регистрация типов через DSC + **RBACReady** + активация watch без рестарта для новых eligible типов — реализовано для additive-пути; симметричное снятие watch — нет.
- **Manifest / MCR / ManifestCheckpoint** — отдельный трек от unified registry snapshot-типов; не смешивать с DSC.
- **NamespaceSnapshot manifests-only path (N2):** этапы **N2a** / **N2b** — [`design/implementation-plan.md`](../design/implementation-plan.md) **§2.4.1**; **декомпозиция поставки N2b по PR** — **§2.4.2** (тот же файл). Публичные поля статуса N2a, allowlist, корреляция MCR/MCP, delete во время capture, download API/ошибки, агрегация N2b, OK vs ownerRef — [`design/namespace-snapshot-controller.md`](../design/namespace-snapshot-controller.md) **§4.3–§4.7**, **§5.2**, **§8.7**, **§10–§11**. Data-layer и полный export/restore — за пределами N2. При стабильном контракте в API — дополнять этот spec, не дублируя design.
- **N2b — форма графа в статусе (PR1):** опциональные поля **`status.childrenSnapshotRefs`** на **`NamespaceSnapshot`** (элементы JSON: **`name`**, **`namespace`**) и **`status.childrenSnapshotContentRefs`** на **`NamespaceSnapshotContent`** (элемент: **`name`**). В Go типы элементов — **`NamespaceSnapshotChildRef`** / **`NamespaceSnapshotContentChildRef`** (N2b child graph, не универсальные cross-kind refs). Семантика заполнения, orchestration и агрегированный **Ready** — не в PR1; см. **§2.4.2** плана и design **§11**.
- **N2b PR2 (scaffold в коде):** при аннотации **`state-snapshotter.deckhouse.io/n2b-pr2-synthetic-tree`** на parent контроллер обеспечивает одного synthetic child и политику **parent `Ready=True` только после child `Ready=True`** с записью graph refs; детали и ограничения (без aggregated download / без domain) — **§11.1** design и **§2.4.2** плана.

## §2. Ссылки

- Обзор линий продукта и навигация: [`README.md`](../README.md)
- Runbook (CRD, метрики, stale, рестарт): [`operations/runbook-degraded-and-unified-runtime.md`](../operations/runbook-degraded-and-unified-runtime.md)
- DSC, RBAC hook, MCR: [`operations/dsc-rbac-and-mcr.md`](../operations/dsc-rbac-and-mcr.md)
- Архитектурные паттерны контроллеров: [`docs/architecture/controller-pattern.md`](../../architecture/controller-pattern.md)
- План внедрения и статусы задач: [`design/implementation-plan.md`](../design/implementation-plan.md)
- Тесты и команды: [`testing/e2e-testing-strategy.md`](../testing/e2e-testing-strategy.md)
- Прогресс стадий: [`operations/project-status.md`](../operations/project-status.md)
