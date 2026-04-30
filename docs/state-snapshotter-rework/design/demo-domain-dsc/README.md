# Demo domain-specific nested snapshot (via DSC)

**Status:** implementation/design package for the current demo-domain-dsc work, non-normative.

Normative contracts live in:

- [`../../spec/system-spec.md`](../../spec/system-spec.md)
- [`../../spec/snapshot-aggregated-read.md`](../../spec/snapshot-aggregated-read.md)

This package explains the current work around:

- DSC registration and graph activation for demo snapshot types;
- parent/child snapshot tree materialization;
- dedicated demo controllers for VM and disk snapshots;
- aggregated manifest read validation for heterogeneous content graphs.

Some documents in this directory are historical design notes. Treat them as context for implementation, not as the source of implementable contract when they disagree with `spec/`.

### PR5a — что гарантирует / чего пока нет

| Гарантирует | Пока не делает (вне PR5a) |
|-------------|---------------------------|
| Привязка к parent snapshot через **`spec.parentSnapshotRef`**; обязательный **`spec.sourceRef`** на namespace-local **`DemoVirtualDisk`**; создание **`DemoVirtualDiskSnapshotContent`**; parent-owned публикация refs родительским controller’ом. | VolumeSnapshot/CSI и реальный data-path. |
| `sourceRef` описывает объект, который materializes текущий snapshot; `parentSnapshotRef` описывает только положение в дереве. | Cross-namespace source refs. |
| **Стадия 2** из **[`spec/system-spec.md`](../../spec/system-spec.md) §3.0:** обход **уже записанного** графа только по **`children*Refs`** (как aggregated/N2b); без list-based восстановления дерева. Доменные узлы (VM content → дети в PR5b; disk content как листья) имеют собственные MCP и **появляются в aggregated read**, потому что доменный контроллер **записал** refs на стадии capture/build (**§3.0** п. 1). | Реальный CSI/data-path остаётся вне PR5a. |

### PR5b (минимум в коде)

| Гарантирует | Пока не делает |
|-------------|----------------|
| **`DemoVirtualMachineSnapshot`** + **`DemoVirtualMachineSnapshotContent`** под parent NS/NSC; обязательный **`spec.sourceRef`** на **`DemoVirtualMachine`**; parent задаётся через **`parentSnapshotRef`**; root/VM `Ready` сходится через child aggregation. | Реальный CSI/data-path (VolumeSnapshot/VCR) в demo. |
| **`DemoVirtualDiskSnapshot.spec.parentSnapshotRef`** — универсальная ссылка на parent snapshot-узел в namespace-local graph. VM controller создаёт disk child snapshots с **`spec.sourceRef`** на соответствующий **`DemoVirtualDisk`** и сам пишет свой child graph. | Не валидирует, что у VM «достаточно» дисков. |

Поставка demo CRD: манифесты в **`crds/`**; образ **`bundle`** в **`.werf/bundle.yaml`** включает каталог **`crds`** в git-стадию модуля. Факт доставки на кластер проверяется **сборкой и деплоем** модуля (CI / релизный pipeline).

## Назначение

Reference для **heterogeneous** доменного дерева под **текущим** root **`NamespaceSnapshot`**, на базе **общей** snapshot-модели ([`08-universal-snapshot-tree-model.md`](08-universal-snapshot-tree-model.md)):

- дерево — **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`** для **любых** `XxxxSnapshot` / `XxxxSnapshotContent`, без отдельных `domainChild*` и без «особого» graph API только для namespace;
- **dedup** — **вычисляется** при reconcile из API, **не** хранится в `status`/аннотациях;
- **готовность и деградация** — единый condition **`Ready`**, каскад снизу вверх и обратная деградация с сохранением **`reason`/`message`**;
- **`NamespaceSnapshot`** — текущий верхний узел архитектуры, **не** отдельный класс правил дерева ([`08`](08-universal-snapshot-tree-model.md) §A.3).

**Слой PR5 vs generic §3-E*:** код и тесты **демо-доменного контроллера** (первый реальный writer **`children*Refs`** в heterogeneous flow) — это **PR5** таблицы **[`implementation-plan.md`](../implementation-plan.md) §2.4.2** и этот пакет документов; это **не** отдельный этап **§3-E1…E6** того же плана (**E1–E6** готовят **generic** контракт и реализацию в модуле, **PR5** впервые использует контракт в **domain flow**). Порядок и нарезка PR5a/PR5b — в **§2.4.4** плана.

**Граница startup / graph activation:** dedicated demo controllers стартуют всегда и могут reconciler'ить вручную созданные `DemoVirtualDiskSnapshot` / `DemoVirtualMachineSnapshot` без DSC. Это не активирует demo kinds в `NamespaceSnapshot` discovery. Graph registry built-in содержит только `NamespaceSnapshot`→`NamespaceSnapshotContent`; demo VM/Disk пары появляются в discovery только через eligible DSC.

**Граница generic / demo (имплементация):** reconciler **`NamespaceSnapshot`**, E5 exclude и PR4 aggregate traversal **не** импортируют demo CRD и **не** содержат веток по именам **`Demo*Snapshot`**. Они используют **`pkg/snapshotgraphregistry.Provider`** (merge graph built-ins ∪ eligible DSC, **refresh после reconcile DSC**, не startup-static снимок) и **`unstructured`** для любых зарегистрированных snapshot/content пар. Демо-типы — **пример consumer'а** DSC и доменной логики (вложенные disk snapshots под VM создаёт **доменный** контроллер, не generic).

**Reference controller contract:** a demo domain snapshot controller owns validation of `parentSnapshotRef` / `sourceRef`, creation of its own `*SnapshotContent`, creation of an MCR for its own source object, linking `content.status.manifestCheckpointName`, and its own `Ready` condition. A domain parent controller also owns child snapshot creation for nested resources, its own `childrenSnapshotRefs`, its own content `childrenSnapshotContentRefs`, and Ready aggregation over children. It does **not** own root/parent refs, `RBACReady`, RBAC creation, or parent status. Invalid user spec is reported as `Ready=False` and must not create content, MCR, or child snapshots.

**Reference RBAC model:** demo/domain controllers intentionally omit kubebuilder RBAC markers. Required permissions are documented as contract and granted externally by the Deckhouse RBAC controller/hook before DSC `RBACReady=True`; they are not generated from controller code comments.

**Контекст:** N2a/N2b + PR4. Целевая модель этого README — **heterogeneous** дерево и контракты ниже; generic **E5/E6** и доменные **PR5a/PR5b** в коде опираются на registry и **`children*Refs`**, без отдельного временного child-**NamespaceSnapshot** scaffold в runtime (см. **[`spec/system-spec.md`](../../spec/system-spec.md) §3.8** и **§2.4.2**/**§2.4.4** [`implementation-plan.md`](../implementation-plan.md)).

## ADR (кратко)

| | |
|--|--|
| **Решение** | Demo kinds подключены через **DSC**; те же pipeline **MCR→MCP** / **VCR→VolumeSnapshot**; **без** вложенного **`NamespaceSnapshot`** под root (**INV-T1** — политика трека и heterogeneous дети, **не** «особый» kind). PR5 — **реальный** heterogeneous tree на **той же универсальной модели refs + `Ready`**, см. [`08`](08-universal-snapshot-tree-model.md). |
| **Инвариант** | Generic не повторно захватывает ресурс, покрытый subtree; **ownerRef** — только жизненный цикл/GC ([`08`](08-universal-snapshot-tree-model.md) часть B). |
| **Ограничения** | Код после апрува пакета; PR4 traversal может потребовать расширения под обход из **тех же** `children*Refs` — отдельный шаг в spec. |

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
| **09** | [`09-materialized-child-content-mcp-and-aggregated-read-checklist.md`](09-materialized-child-content-mcp-and-aggregated-read-checklist.md) | Implementation checklist and validation cases. |
| **90** | [`090-unified-snapshot-controller-lifecycle.md`](090-unified-snapshot-controller-lifecycle.md) | Design note for lifecycle `XxxSnapshotController`. |

**Минимальный API v1:** §0 в [`05-tree-and-graph-invariants.md`](05-tree-and-graph-invariants.md).

## Связь с существующими SSOT

- [`namespace-snapshot-controller.md`](../namespace-snapshot-controller.md), [`implementation-plan.md`](../implementation-plan.md) §2.4, [`spec/system-spec.md`](../../spec/system-spec.md).
- Aggregated read: [`spec/snapshot-aggregated-read.md`](../../spec/snapshot-aggregated-read.md).
- PR4 history: [`spec/namespace-snapshot-aggregated-manifests-pr4.md`](../../spec/namespace-snapshot-aggregated-manifests-pr4.md).
- DSC: [`operations/dsc-rbac-and-mcr.md`](../../operations/dsc-rbac-and-mcr.md).
