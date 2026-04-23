# System spec (normative excerpts — state-snapshotter)

Нормативный контракт для реализации и тестов. Полная детализация DSC / registry / RBAC — в ADR [`snapshot-rework/2026-01-23-unified-snapshots-registry.md`](../../../snapshot-rework/2026-01-23-unified-snapshots-registry.md); не дублировать длинные фрагменты здесь без необходимости — обновлять этот файл при изменении контракта.

Нумерация разделов ниже совпадает с бывшим указателем `snapshot-rework/plan/dorabotki-i-testy.md`: **§0** — registry/runtime, **§1** — контекст, **§2** — ссылки, **§3** — граф snapshot-run (PR5+).

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
- **NamespaceSnapshot manifests-only path (N2):** этапы **N2a** / **N2b** — [`design/implementation-plan.md`](../design/implementation-plan.md) **§2.4.1**; **декомпозиция поставки N2b по PR** — **§2.4.2** (тот же файл). Публичные поля статуса N2a, allowlist, **временный MCR** (**ownerRef** на root `NamespaceSnapshot` для GC in-flight; удаляется контроллером после успешного capture), **каноническая ссылка на MCP** — `NamespaceSnapshotContent.status.manifestCheckpointName`, delete root / in-flight capture, download API/ошибки, агрегация N2b, OK vs ownerRef — [`design/namespace-snapshot-controller.md`](../design/namespace-snapshot-controller.md) **§4.3–§4.7**, **§5.2**, **§8.7**, **§10–§11**. Data-layer и полный export/restore — за пределами N2. При стабильном контракте в API — дополнять этот spec, не дублируя design.
- **N2b — форма графа в статусе (PR1):** опциональные поля **`status.childrenSnapshotRefs`** на **`NamespaceSnapshot`** (элементы JSON: **`name`**, **`namespace`**) и **`status.childrenSnapshotContentRefs`** на **`NamespaceSnapshotContent`** (элемент: **`name`**). В Go типы элементов — **`NamespaceSnapshotChildRef`** / **`NamespaceSnapshotContentChildRef`** (N2b child graph, не универсальные cross-kind refs). Семантика заполнения, orchestration и агрегированный **Ready** — не в PR1; см. **§2.4.2** плана и design **§11**. **Инварианты универсального дерева snapshot-run, merge и dedup при PR5+** — нормативно **§3**; дизайн-пакет и мотивы — [`design/demo-domain-dsc/README.md`](../design/demo-domain-dsc/README.md), [`design/demo-domain-dsc/08-universal-snapshot-tree-model.md`](../design/demo-domain-dsc/08-universal-snapshot-tree-model.md).
- **N2b PR2 (временный scaffold в коде, не целевая модель PR5):** при аннотации **`state-snapshotter.deckhouse.io/n2b-pr2-synthetic-tree`** на parent контроллер обеспечивает одного synthetic child и запись graph refs; **§11.1** design и **§2.4.2** плана. **Не** часть целевой heterogeneous-архитектуры; **снятие** из кода и тестов — сразу после **merge-gate** на demo-domain flow, см. **§2.4.2** того же плана (правило **«Снятие synthetic scaffold»**).
- **N2b PR3** (**временный scaffold в коде, не целевая модель PR5**): агрегированный **Ready** parent при использовании synthetic required child (тот же scaffold, что PR2): собственный persisted MCP **и** required child **`Ready=True`**; иначе **`ChildSnapshotPending`** или при терминальном провале child — **`ChildSnapshotFailed`** (`pkg/snapshot`); таблица и whitelist — **§11.1** design. **Снятие** scaffold — как для PR2.
- **N2b PR4 — aggregated manifests download (HTTP + read-path по сохранённому графу + errors):** нормативный контракт — **[`spec/namespace-snapshot-aggregated-manifests-pr4.md`](namespace-snapshot-aggregated-manifests-pr4.md)** (endpoint, обход **только** по опубликованным content-refs, fail-whole, merge, циклы, дубликаты); двухстадийная модель **§3.0** этого spec. Общие принципы N2a/N2b download — по-прежнему [`design/namespace-snapshot-controller.md`](../design/namespace-snapshot-controller.md) **§8.7** (ссылка на PR4 SSOT в **§8.7.1**).

## §2. Ссылки

- Обзор линий продукта и навигация: [`README.md`](../README.md)
- Runbook (CRD, метрики, stale, рестарт): [`operations/runbook-degraded-and-unified-runtime.md`](../operations/runbook-degraded-and-unified-runtime.md)
- DSC, RBAC hook, MCR: [`operations/dsc-rbac-and-mcr.md`](../operations/dsc-rbac-and-mcr.md)
- Архитектурные паттерны контроллеров: [`docs/architecture/controller-pattern.md`](../../architecture/controller-pattern.md)
- План внедрения и статусы задач: [`design/implementation-plan.md`](../design/implementation-plan.md)
- Тесты и команды: [`testing/e2e-testing-strategy.md`](../testing/e2e-testing-strategy.md)
- Прогресс стадий: [`operations/project-status.md`](../operations/project-status.md)

## §3. Граф snapshot-run: refs, generic, merge, dedup (нормативно для PR5+)

Ниже — **контракт реализации** (MUST / MUST NOT). Расширение полей элементов **`children*Refs`** до полного `{ apiGroup, kind, namespace, name }` — вместе с OpenAPI CRD и кодом; до этого действуют ключи слияния для текущих типов (**§3.2**). Поведение **`Ready`** и каскада — в [`design/demo-domain-dsc/07-ready-delete-matrix.md`](../design/demo-domain-dsc/07-ready-delete-matrix.md); при переносе норм в этот spec — без противоречий **§3** и **§1** (N2b).

### §3.0. Две стадии: capture-time domain expansion и обход сохранённого графа

Контракт **§3** разделяет **кто** определяет состав дерева и **как** по нему ходит общий код.

**1) Capture-time domain expansion (построение дерева снимка).** При создании пользователем **`XxxxSnapshot`** **доменный** контроллер соответствующего типа определяет **snapshot scope** для этого ресурса и то, **как** каждый охваченный объект представлен в дереве run, в частности:
- обычные Kubernetes-ресурсы → **manifest capture** данного узла (MCP / MCR и пр. — по нормативам N2a/N2b и согласованным design);
- ресурсы **volume / data** → отдельные snapshot/data-операции (вне manifests-only подграфа или по отдельным под-документам);
- объекты, для которых предусмотрен **другой** доменный snapshot (другой **`YyyySnapshot`** / контроллер) → оформляются как **явные дочерние** **`YyyySnapshot`** (и связанный **`YyyySnapshotContent`**), с публикацией рёбер в **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`** по политике родительского узла.

**Логическое дерево** snapshot-run материализуется **сверху вниз** на этом этапе. **Generic**-код **не** выводит состав дерева из инвентаря API и **не** подменяет решение домена; он **использует только уже записанные** в **`status`** refs (**§3.1**, **INV-REF1**).

**2) Traversal уже построенного snapshot-графа (read-path / generic).** После публикации refs **generic** и сценарии **aggregate / download** обходят узлы **только** по разрешённым цепочкам **`childrenSnapshotRefs`** и **`childrenSnapshotContentRefs`** (этот spec или согласованный под-документ). На этой стадии **MUST NOT:** заново **обнаруживать** (discovery), какие объекты кластера «входят в снимок», и **MUST NOT:** **восстанавливать** или **достраивать** дерево **list-перечислением**, фильтрами по namespace или иными эвристиками **вместо** или **в обход** недостающих или неполных refs.

**INV-REF-C1**, **§3.4** и **§3.5** относятся к стадии **(2)** и к обязанности считать границу run **замкнутой** по сохранённому графу.

### §3.1. Логическое дерево и источник истины

- **MUST:** логическое дерево snapshot-run задаётся **только** refs-полями **`status`** на **`XxxxSnapshot`** / **`XxxxSnapshotContent`**, **опубликованными** на пути от **root** **`NamespaceSnapshot`** этого run (материализация дерева — **§3.0** п. 1; обход без расширения scope — **§3.0** п. 2): **`childrenSnapshotRefs`** — **основной** носитель **ребёнка-узла** в дереве; **`childrenSnapshotContentRefs`** — **дополняющий** слой **только** там, где это **нормативно** требует этот spec или согласованный под-документ (read-path по графу, aggregation, политика этапа), **без** подмены SoT, задаваемого **snapshot** refs (см. [`05`](../design/demo-domain-dsc/05-tree-and-graph-invariants.md) §2, абзац **Snapshot refs vs content refs**).
- **Область:** **`XxxxSnapshot`** / **`XxxxSnapshotContent`** **могут** существовать в API и reconciler'иться **доменным** (или иным) контроллером **до** появления в **`children*Refs`** данного run или **вне** любого такого run — это **не** запрещает **§3** и не отменяет **DSC** / reconcile зарегистрированных типов (**§0**). **§3** задаёт **только** состав **логического дерева** конкретного run от root **`NamespaceSnapshot`** и обязанности **generic** (обход, dedup/exclude и т.д.) относительно **этого** дерева.
- **MUST NOT:** объект считаться узлом этого дерева, если он **не** представлен в **`children*Refs`** на пути от root (даже если существует в API). (**INV-REF1**, см. [`05`](../design/demo-domain-dsc/05-tree-and-graph-invariants.md) §1.)

### §3.2. Ключ merge для элементов refs (до расширения схемы PR5)

- **MUST:** запись в **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`** — **merge-only** по каноническому ключу элемента: для элементов вида **`NamespaceSnapshotChildRef`** — пара **`(namespace, name)`** дочернего snapshot; для **`NamespaceSnapshotContentChildRef`** — **`name`** дочернего **`XxxxSnapshotContent`** в **namespace** родительского **`NamespaceSnapshotContent`**. После расширения схемы элементов до **GVK + namespace + name** канонический ключ **MUST** совпадать с нормативным определением в OpenAPI этого репозитория.
- **MUST NOT:** заменять список **`children*Refs`** целиком одним write, если этим стираются элементы, записанные другим контроллером; удалять из списка элемент, за который пишущий контроллер **не** несёт ответственности. (**INV-REF-M1**, **INV-REF-M2**, [`05`](../design/demo-domain-dsc/05-tree-and-graph-invariants.md) §1.)

### §3.3. Удаление элемента из refs

- **MUST:** удаление записи о дочернем узле из **`children*Refs`** родителя выполнять **только** контроллер, который **владеет** соответствующим дочерним объектом (создал и ведёт его), **или** по **явной** политике reconcile родителя, задокументированной в коде модуля и не противоречащей **INV-REF-M2**.

### §3.4. Generic `NamespaceSnapshot` и read-path по API (только стадия 2)

Обязанности generic здесь — **не** domain expansion (**§3.0** п. 1), а **обход уже опубликованного** графа и сопутствующие правила fail-closed (**§3.0** п. 2).

- **MUST:** generic-код (**`NamespaceSnapshot`** reconcile, E5 exclude, PR4 aggregate read-path) резолвит дочерние **`XxxxSnapshot`** → **`status.boundSnapshotContentName`** и обходит дочерние **`XxxxSnapshotContent`** только через **`pkg/snapshot.GVKRegistry`**, наполняемый из merge(static bootstrap, eligible DSC) + **`RESTMapper`** (как в `cmd/main.go`), с **`unstructured`** Get по зарегистрированным GVK. **MUST NOT:** импортировать пакеты доменных CRD (в т.ч. demo) или хардкодить имена доменных snapshot/content kinds в generic слое.
- **MUST NOT:** reconciler **`NamespaceSnapshot`** (и общий код exclude/dedup для root capture) **достраивать** логическое дерево или множество узлов из **list/search по namespace** или эвристик без ref на пути от root. (**INV-REF1**.)
- **MUST NOT:** при отсутствии или пустоте **`childrenSnapshotContentRefs`** там, где поле предусмотрено схемой, **самостоятельно** находить **`*SnapshotContent`** через list API без нормативного правила в этом spec или в согласованном под-документе (например расширение PR4 read-path). **По умолчанию** поведение в такой ситуации — **fail-closed** (не продолжать этап). **Явный fallback** (в т.ч. только из цепочки **snapshot refs**) **допустим только** если он **отдельно** закреплён в этом spec или в согласованном под-документе; иначе list/search «для восстановления content» — **вне** контракта. Конкретный разрешённый вариант **MUST** быть указан в реализации и тестах до включения поведения в релиз. (**INV-REF-C1**.)

### §3.5. Граница run и fail-closed dedup

- **MUST:** вычисление dedup / exclude для root capture и **сопутствующий** подбор связанных объектов (обнаружение MCP/VS и т.п. **в контексте** покрытия и исключений) выполнять **только** в пределах дерева **текущего** snapshot-run (обход от root **`NamespaceSnapshot`** по **`children*Refs`**), **без** расширения множества узлов за пределы этого обхода под видом «подготовки данных». (**INV-S0**, [`design/demo-domain-dsc/06-coverage-dedup-keys.md`](../design/demo-domain-dsc/06-coverage-dedup-keys.md).)
- **MUST NOT:** при невозможности **надёжно** построить множества exclude расширять dedup или исключения «по догадке» по неполным данным; поведение **fail-closed** — как в **INV-E1** ([`06`](../design/demo-domain-dsc/06-coverage-dedup-keys.md) §4).

### §3.6. DSC и ownerRef (напоминание)

- **MUST NOT:** использовать **DSC** или **`ownerReference`** как источник истины **состава** логического дерева или для **dedup**; **DSC** — регистрация типов / runtime watches (**§0**); **`ownerReference`** — lifecycle / GC ([`design/demo-domain-dsc/08-universal-snapshot-tree-model.md`](../design/demo-domain-dsc/08-universal-snapshot-tree-model.md) часть B).

### §3.7. Ссылки на тесты и дизайн

- Порядок атомарных PR под имплементацию **§3** (без повторения MUST): [`design/implementation-plan.md`](../design/implementation-plan.md) **§2.4.4**.
- Концептуальный SSOT дерева / осей: [`design/demo-domain-dsc/08-universal-snapshot-tree-model.md`](../design/demo-domain-dsc/08-universal-snapshot-tree-model.md).
- Инварианты графа и generic vs domain: [`design/demo-domain-dsc/05-tree-and-graph-invariants.md`](../design/demo-domain-dsc/05-tree-and-graph-invariants.md).
- План сценариев: [`testing/demo-domain-dsc-test-plan.md`](../testing/demo-domain-dsc-test-plan.md).
