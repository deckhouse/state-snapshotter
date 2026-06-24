# State-snapshotter rework — навигация по документам (D1)

Каталог **`docs/state-snapshotter-rework/`** — точка входа в дорожную карту переработки контроллера, unified snapshots и смежных контрактов. Код контроллера: `images/state-snapshotter-controller/`.

---

## Current Source Of Truth

The current implementation contract is maintained in:

- `docs/state-snapshotter-rework/spec/system-spec.md`
- `docs/state-snapshotter-rework/testing/pre-e2e-smoke-validation.md`
- `docs/state-snapshotter-rework/operations/project-status.md`

Older ADR/design notes may describe superseded stages of the architecture. When documents disagree, the files above take precedence.

Legacy terms are allowed only in explicitly historical documents. Do not use historical documents as implementation instructions without checking the current spec.

---

## Четыре «линии» продукта (сверху вниз)

Ниже — **логическое** разделение, а не обязательно отдельные бинарники. Так проще понять, где искать CRD, RBAC и документацию.

### 1. CSI / volume snapshot (вне этого каталога детально)

- Работа с томами через **Kubernetes VolumeSnapshot** / драйвер хранилища — это **другая ось**, чем «какие *Snapshot CRD* модуль регистрирует в API Deckhouse».
- State-snapshotter как продукт **соприкасается** с бэкапом/томами на уровне доменных сценариев, но **unified registry** в первую очередь про **собственные и модульные CRD** снимков состояния приложений, а не про замену CSI.

### 2. Группа `storage.deckhouse.io` (unified «родные» типы)

- Типовые ресурсы модуля: **Snapshot**, **SnapshotContent**, **CustomSnapshotDefinition**, **ManifestCaptureRequest**, **ManifestCheckpoint** и связанные artifact-ресурсы. Bootstrap-набор snapshot/content пар задаётся в `pkg/unifiedbootstrap`, пока CSD не расширяет набор.
- Эта линия — **опорная** для единого контроллера снимков в кластере Deckhouse.

### 3. Manifest line (MCR / ManifestCheckpoint / capture)

- **ManifestCaptureRequest**, **ManifestCheckpoint**, связанные сценарии захвата манифестов — **отдельный трек** от CSD и unified GVK registry.
- Документы плана: **M1 / M2** в [`design/implementation-plan.md`](design/implementation-plan.md). Не смешивать с механикой «какие snapshot kinds зарегистрированы» в одном PR без необходимости.

### 4. Unified line + CSD (реестр типов и динамические watches)

- **`CustomSnapshotDefinition` (CSD)** — кластерный объект: модуль декларирует, какие **CRD имёна** соответствуют паре snapshot / snapshot content.
- После формулы **Accepted + RBACReady** (+ поколения) CSD участвует в **merge** с bootstrap; `pkg/unifiedruntime` делает **additive** watches без рестарта pod для новых eligible типов.
- **Bootstrap (R5):** env `STATE_SNAPSHOTTER_UNIFIED_BOOTSTRAP_PAIRS` и Helm values — см. [`operations/runbook-degraded-and-unified-runtime.md`](operations/runbook-degraded-and-unified-runtime.md) §4. Unified/generic runtime в v0 always-on.
- Ограничения (unwatch, stale) — там же §1–§3.

---

## Куда смотреть дальше

| Документ | Назначение |
|----------|------------|
| [`operations/project-status.md`](operations/project-status.md) | Короткий статус дорожной карты |
| [`design/implementation-plan.md`](design/implementation-plan.md) | Задачи S/R/M/D и галочки |
| [`spec/system-spec.md`](spec/system-spec.md) | Нормативные выдержки для кода и тестов |
| [`spec/snapshot-aggregated-read.md`](spec/snapshot-aggregated-read.md) | Нормативный контракт aggregated snapshot read |
| [`api/snapshot-read.md`](api/snapshot-read.md) | HTTP API чтения aggregated snapshot manifests |
| [`operations/runbook-degraded-and-unified-runtime.md`](operations/runbook-degraded-and-unified-runtime.md) | **Runbook:** CRD, метрики, stale, рестарт (D3) |
| [`operations/csd-rbac-and-mcr.md`](operations/csd-rbac-and-mcr.md) | CSD, hook, RBAC, отличие от MCR (D2) |
| [`design/r2-phase-2b-r3-runtime-registry.md`](design/r2-phase-2b-r3-runtime-registry.md) | Техдизайн runtime registry |
| [`testing/e2e-testing-strategy.md`](testing/e2e-testing-strategy.md) | Уровни тестов и команды |
| [`design/demo-domain-csd/README.md`](design/demo-domain-csd/README.md) | **Proposed:** demo domain nested snapshot (CSD), PVC dedup — дизайн до кода |
| [`snapshot-rework/`](../../snapshot-rework/) | ADR и длинная история решений |

---

## Зависимости разработчика (кратко)

- Сборка и тесты контроллера: модуль Go под `images/state-snapshotter-controller/`, см. `Makefile` / CI `go_checks`.
- Интеграционные тесты: `go test -tags=integration ./test/integration/...`, нужен `KUBEBUILDER_ASSETS` (см. правила репозитория для envtest).

Подробный операционный контекст по деградации и метрикам — в runbook (D3), не дублировать длинные процедуры здесь.
