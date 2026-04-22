# Demo domain-specific nested snapshot (via DSC)

**Статус:** Proposed — **сначала документы → ревью → только потом код.**

## Назначение

Reference для **heterogeneous** доменного дерева под **текущим** root **`NamespaceSnapshot`**, на базе **общей** snapshot-модели ([`08-universal-snapshot-tree-model.md`](08-universal-snapshot-tree-model.md)):

- дерево — **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`** для **любых** `XxxxSnapshot` / `XxxxSnapshotContent`, без отдельных `domainChild*` и без «особого» graph API только для namespace;
- **dedup** — **вычисляется** при reconcile из API, **не** хранится в `status`/аннотациях;
- **готовность и деградация** — единый condition **`Ready`**, каскад снизу вверх и обратная деградация с сохранением **`reason`/`message`**;
- **`NamespaceSnapshot`** — текущий верхний узел архитектуры, **не** отдельный класс правил дерева ([`08`](08-universal-snapshot-tree-model.md) §A.3).

**Контекст:** N2a/N2b + PR4. Целевая модель этого README — **heterogeneous** дерево и контракты ниже; временный тестовый scaffold в коде (исторически «synthetic») **не** является частью архитектурной модели здесь — см. [`implementation-plan.md`](../implementation-plan.md) **§2.4.2** (блок **«Целевая архитектура vs текущий код»** / **«Снятие synthetic scaffold»**): после **merge-gate** на demo-domain flow scaffold **обязан** быть удалён из кода и тестов **в том же PR5 или следующем cleanup PR**.

## ADR (кратко)

| | |
|--|--|
| **Решение** | Demo kinds подключены через **DSC**; те же pipeline **MCR→MCP** / **VCR→VolumeSnapshot**; **без** вложенного **`NamespaceSnapshot`** под root (**INV-T1** — политика трека и heterogeneous дети, **не** «особый» kind). PR5 — **реальный** heterogeneous tree на **той же универсальной модели refs + `Ready`**, см. [`08`](08-universal-snapshot-tree-model.md). |
| **Инвариант** | Generic не повторно захватывает ресурс, покрытый subtree; **ownerRef** — только жизненный цикл/GC ([`08`](08-universal-snapshot-tree-model.md) часть B). |
| **Ограничения** | Код после апрува пакета; PR4 traversal может потребовать расширения под обход из **тех же** `children*Refs` — отдельный шаг в spec. Временный synthetic scaffold в репо до merge-gate — только **implementation-plan §2.4.2** (миграция/снятие), не целевая модель здесь. |

## Документы этапа 1 (архитектурный обзор)

| # | Файл | Содержание |
|---|------|------------|
| 1 | [`01-api.md`](01-api.md) | Demo CRD; **v1** без inventory CRD (**self-contained** `spec`); связь с root через **`children*Refs`**; без вложенного NS под root (**INV-T1**). |
| 2 | [`02-dsc-wiring.md`](02-dsc-wiring.md) | DSC; demo controllers. |
| 3 | [`03-snapshot-flow.md`](03-snapshot-flow.md) | Поток reconcile; ownerRef ≠ dedup. |
| 4 | [`04-coverage-dedup.md`](04-coverage-dedup.md) | Dedup (вычисляемый): data + resource; граница run (**INV-S0** в `06`); ownerRef ≠ dedup. |
| — | [`../../testing/demo-domain-dsc-test-plan.md`](../../testing/demo-domain-dsc-test-plan.md) | Сценарии тестов. |

## Документы этапа 2–3 (фиксация + универсальная модель)

| # | Файл | Содержание |
|---|------|------------|
| 5 | [`05-tree-and-graph-invariants.md`](05-tree-and-graph-invariants.md) | Таблица kinds; **общие** `children*Refs`; generic vs domain. |
| 6 | [`06-coverage-dedup-keys.md`](06-coverage-dedup-keys.md) | Ключи вычисления; без persisted coverage. |
| 7 | [`07-ready-delete-matrix.md`](07-ready-delete-matrix.md) | Единый **`Ready`**; каскады; сценарии деградации. |
| **8** | [`08-universal-snapshot-tree-model.md`](08-universal-snapshot-tree-model.md) | **Универсальная** модель дерева, `Ready`, dedup, **ownerRef** (части A и B). |

**Минимальный API v1:** §0 в [`05-tree-and-graph-invariants.md`](05-tree-and-graph-invariants.md).

## Связь с существующими SSOT

- [`namespace-snapshot-controller.md`](../namespace-snapshot-controller.md), [`implementation-plan.md`](../implementation-plan.md) §2.4, [`spec/system-spec.md`](../../spec/system-spec.md).
- PR4: [`spec/namespace-snapshot-aggregated-manifests-pr4.md`](../../spec/namespace-snapshot-aggregated-manifests-pr4.md) — при heterogeneous обходе опираться на **ту же** модель refs после обновления spec.
- DSC: [`operations/dsc-rbac-and-mcr.md`](../../operations/dsc-rbac-and-mcr.md).
