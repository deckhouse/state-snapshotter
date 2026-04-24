# Demo domain-specific nested snapshot (via DSC)

**Статус:** Proposed — **сначала документы → ревью → только потом код.** **PR5a/PR5b в репозитории:** группа **`demo.state-snapshotter.deckhouse.io`**, disk (**`DemoVirtualDiskSnapshot`**) и VM (**`DemoVirtualMachineSnapshot`**) + **`*Content`**, stubs **`DemoVirtualDisk`** / **`DemoVirtualMachine`** для DSC; контроллеры merge **`children*Refs`**; интеграции `demovirtualdisksnapshot_pr5a_test.go`, `demovirtualmachinesnapshot_pr5b_test.go` — см. [`operations/project-status.md`](../../operations/project-status.md).

### PR5a — что гарантирует / чего пока нет

| Гарантирует | Пока не делает (вне PR5a) |
|-------------|---------------------------|
| Привязка к root **`NamespaceSnapshot`** через **`spec.rootNamespaceSnapshotRef`**; создание **`DemoVirtualDiskSnapshotContent`**; merge **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`** на root NS и root NSC (идемпотентно). | **`Ready`/TTL**, VolumeSnapshot/CSI, реальный data-path, полный MCR/MCP для demo kinds. |
| Опционально в **`spec`**: **`persistentVolumeClaimName`** — имя PVC в том же namespace (только идентичность для доменной семантики «диск»; без reconcile PVC/VolumeSnapshot). | Не валидирует существование PVC в API-сервере; не пишет статус по PVC. |
| **Стадия 2** из **[`spec/system-spec.md`](../../spec/system-spec.md) §3.0:** обход **уже записанного** графа только по **`children*Refs`** (как aggregated/N2b); без list-based восстановления дерева. Доменные узлы (VM content → дети в PR5b; disk content как листья без MCP в aggregated) **появляются в обходе**, потому что доменный контроллер **записал** refs на стадии capture/build (**§3.0** п. 1). | Не смешивает demo-архив в aggregated JSON до отдельного контракта. |

### PR5b (минимум в коде)

| Гарантирует | Пока не делает |
|-------------|----------------|
| **`DemoVirtualMachineSnapshot`** + **`DemoVirtualMachineSnapshotContent`** под root NS/NSC; **`spec.virtualMachineName`** (идентификатор VM без inventory CRD). | **`Ready`**-каскад по детям VM, автосоздание disk snapshots контроллером VM. |
| **`DemoVirtualDiskSnapshot.spec.parentDemoVirtualMachineSnapshotRef`** — диск под VM при совпадении **`rootNamespaceSnapshotRef`** с родителем (**INV-T2**); merge **`children*Refs`** на VM snapshot и на VM content. | Не валидирует, что у VM «достаточно» дисков; оператор создаёт диск отдельно. |

Поставка demo CRD: манифесты в **`crds/`**; образ **`bundle`** в **`.werf/bundle.yaml`** включает каталог **`crds`** в git-стадию модуля. Факт доставки на кластер проверяется **сборкой и деплоем** модуля (CI / релизный pipeline).

## Назначение

Reference для **heterogeneous** доменного дерева под **текущим** root **`NamespaceSnapshot`**, на базе **общей** snapshot-модели ([`08-universal-snapshot-tree-model.md`](08-universal-snapshot-tree-model.md)):

- дерево — **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`** для **любых** `XxxxSnapshot` / `XxxxSnapshotContent`, без отдельных `domainChild*` и без «особого» graph API только для namespace;
- **dedup** — **вычисляется** при reconcile из API, **не** хранится в `status`/аннотациях;
- **готовность и деградация** — единый condition **`Ready`**, каскад снизу вверх и обратная деградация с сохранением **`reason`/`message`**;
- **`NamespaceSnapshot`** — текущий верхний узел архитектуры, **не** отдельный класс правил дерева ([`08`](08-universal-snapshot-tree-model.md) §A.3).

**Слой PR5 vs generic §3-E*:** код и тесты **демо-доменного контроллера** (первый реальный writer **`children*Refs`** в heterogeneous flow) — это **PR5** таблицы **[`implementation-plan.md`](../implementation-plan.md) §2.4.2** и этот пакет документов; это **не** отдельный этап **§3-E1…E6** того же плана (**E1–E6** готовят **generic** контракт и реализацию в модуле, **PR5** впервые использует контракт в **domain flow**). Порядок и нарезка PR5a/PR5b — в **§2.4.4** плана.

**Граница generic / demo (имплементация):** reconciler **`NamespaceSnapshot`**, E5 exclude и PR4 aggregate traversal **не** импортируют demo CRD и **не** содержат веток по именам **`Demo*Snapshot`**. Они используют **`pkg/snapshotgraphregistry.Provider`** (merge bootstrap ∪ eligible DSC, **refresh после reconcile DSC**, не startup-static снимок) и **`unstructured`** для любых зарегистрированных snapshot/content пар. Демо-типы — **пример consumer'а** DSC и доменной логики (вложенные disk snapshots под VM создаёт **доменный** контроллер, не generic).

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

**Минимальный API v1:** §0 в [`05-tree-and-graph-invariants.md`](05-tree-and-graph-invariants.md).

## Связь с существующими SSOT

- [`namespace-snapshot-controller.md`](../namespace-snapshot-controller.md), [`implementation-plan.md`](../implementation-plan.md) §2.4, [`spec/system-spec.md`](../../spec/system-spec.md).
- PR4: [`spec/namespace-snapshot-aggregated-manifests-pr4.md`](../../spec/namespace-snapshot-aggregated-manifests-pr4.md) — при heterogeneous обходе опираться на **ту же** модель refs после обновления spec.
- DSC: [`operations/dsc-rbac-and-mcr.md`](../../operations/dsc-rbac-and-mcr.md).
