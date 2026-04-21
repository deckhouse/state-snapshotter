# DSC wiring для demo domain

**Статус:** Proposed.

## Принцип

**`DomainSpecificSnapshotController` (DSC)** — декларативная регистрация пар **snapshot / snapshot content** для доменных kinds, чтобы они участвовали в **unified registry** и runtime watches по существующим правилам (см. [`spec/system-spec.md`](../../spec/system-spec.md) §0, [`operations/dsc-rbac-and-mcr.md`](../../operations/dsc-rbac-and-mcr.md)).

Generic **`NamespaceSnapshot`** reconciler **не** получает `if demoKind`: он ведёт **root** namespace capture и читает только **обобщённые** сигналы (например **coverage / exclude list** на root объекте — см. [`04-coverage-dedup.md`](04-coverage-dedup.md)), без знания про конкретный demo GVK.

## Объекты DSC (черновик)

Минимум **два** DSC (или один DSC с двумя парами kinds — если схема и KindConflict это позволяют):

| DSC | Snapshot kind | SnapshotContent kind | Назначение |
|-----|---------------|----------------------|------------|
| A | `DemoVirtualMachineSnapshot` | `DemoVirtualMachineSnapshotContent` (имя на ревью) | VM |
| B | `DemoVirtualDiskSnapshot` | `DemoVirtualDiskSnapshotContent` | Disk leaf |

**VolumeSnapshot** / **VolumeSnapshotContent** — обычно CSI API group; участие в дереве под root NS не требует DSC state-snapshotter, если драйвер стандартный; связь **логического узла** дерева с VS — в [`03-snapshot-flow.md`](03-snapshot-flow.md).

## Кто что создаёт

| Компонент | Создаёт |
|-----------|---------|
| Пользователь / CI | **Root** `NamespaceSnapshot` (и далее стандартный bind **одного** root `NamespaceSnapshotContent`). |
| **Demo domain controllers** | `DemoVirtualMachineSnapshot`, `DemoVirtualDiskSnapshot`, их **Content**, **VolumeSnapshot** / VCR, **MCR/MCP** по политике leaf — **не** вложенные **`NamespaceSnapshot`**. |
| Generic NS controller | Root MCR→MCP pipeline, обновление root NSC/NS статусов, учёт **exclude/coverage** для generic capture. |

**Demo orchestrator** (если выделяется отдельным бинарником/пакетом) синхронизирует жизненный цикл доменного дерева с **root** `NamespaceSnapshot` (refs, readiness агрегация на root — по согласованной схеме), но **не** подменяет модель «снимок VM = ещё один NamespaceSnapshot».

## Маппинг «инвентарь → snapshot»

- Обнаружение **DemoVirtualMachine** / **DemoVirtualDisk** в namespace — **list/watch в demo reconciler** (или одном «orchestrator» пакете), **не** разрастание allowlist generic NS специальными ветками под demo GVK.
- Связь инвентаря с созданными **DemoVirtualMachineSnapshot** — поля `spec` + статусы demo CRD.

## Приоритет / порядок

- Disk subtree не обгоняет VM snapshot без явной машины состояний — [`03-snapshot-flow.md`](03-snapshot-flow.md).
- KindConflict / Accepted — как для любых DSC.

## RBAC

- Demo controllers: чтение VM/Disk, создание/обновление **demo snapshot** CRD, **VolumeSnapshot**, **ManifestCaptureRequest**, запись **coverage** на root `NamespaceSnapshot` (или согласованный канал). **Не** требуется право создавать **дочерние** `NamespaceSnapshot` для VM/disk (их нет в модели).

## Не делать

- Не описывать и не реализовывать **child NamespaceSnapshot** для VM/disk под root.
- Не добавлять `if demoKind` в generic DSC reconciler.
