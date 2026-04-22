# Snapshot flow: root NamespaceSnapshot + heterogeneous snapshot tree

**Статус:** Proposed (исправлено по ревью: **без** вложенных `NamespaceSnapshot`).  
**Цель:** согласовать порядок reconcile, **ownerRef** (иерархия / GC / deletion) отдельно от **вычисляемого dedup/exclude**, и границу без special-case «если DemoVM» в generic reconcile.

**Три оси (кратко):** дерево — **`children*Refs`**; готовность — **`Ready`** по refs + зависимостям; lifecycle — **ownerRef**/финализаторы ([`05` §3](05-tree-and-graph-invariants.md) таблица «Три оси», [`08` B](08-universal-snapshot-tree-model.md)).

**Traversal (единый вход в логическое дерево):** обход для **`Ready`**, для **вычисляемого** dedup/exclude и для любого агрегированного чтения по **целевой** модели PR5 стартует с root **`NamespaceSnapshot`** и продолжается **только** по **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`**; **не** по **ownerRef**, **не** по DSC и **не** по «ожидаемой схеме» kinds ([`05`](05-tree-and-graph-invariants.md) §1–§2).

## Целевое дерево (логическое)

Под **одним** root **`NamespaceSnapshot`** (снимок namespace) живут **не** другие NS snapshot, а **разные kinds** snapshot/content:

**Дисклеймер к диаграмме ниже:** рисунок иллюстрирует **возможную** форму дерева в demo v1, а **не** фиксированную схему, обязательный порядок узлов и **не** allowlist kinds. Фактический граф — то, что контроллеры создали и отразили в **`children*Refs`**; см. [`05`](05-tree-and-graph-invariants.md) (нет compile-time списка детей в generic).

```text
NamespaceSnapshot (root)
├── NamespaceSnapshotContent          ← root namespace manifest result (N2a/N2b root)
├── DemoVirtualMachineSnapshot
│   ├── DemoVirtualMachineSnapshotContent
│   ├── DemoVirtualDiskSnapshot
│   │   └── DemoVirtualDiskSnapshotContent
│   └── …
├── DemoVirtualDiskSnapshot            ← при необходимости standalone disk под root
│   └── DemoVirtualDiskSnapshotContent
└── VolumeSnapshot                     ← где leaf данных = CSI
    └── VolumeSnapshotContent
```

**`VolumeSnapshot`** / **`VolumeSnapshotContent`** — **leaf** слоя данных (CSI); в дереве они появляются как узлы, которые **создаёт и ведёт доменный** контроллер (или согласованная цепочка под ним), **без** регистрации VS в DSC state-snapshotter там, где это не требуется ([`02`](02-dsc-wiring.md)). **Generic** не ветвится по «demo vs CSI»: взаимодействие с таким узлом — **только** через ref в **`children*Refs`** и **тип-агностичный** разбор CSI **status** (маппинг к **`Ready`** / dedup), **без** продуктовой ветки «наш особый VS».

Имена demo kinds — см. [`01-api.md`](01-api.md). **Смысл:** PR5 / real domain wiring — это **heterogeneous snapshot graph**, а не модель с **вложенным** `NamespaceSnapshot` под root вместо доменного графа (**INV-T1**).

## Участники

| Участник | Роль |
|----------|------|
| **Generic NamespaceSnapshot controller** | Только **root** namespace-level capture: bind **одного** root `NamespaceSnapshotContent`, MCR→MCP для root; **`Ready`** root — каскад по **`childrenSnapshotRefs`** (heterogeneous дети после расширения элементов refs в spec) и собственные зависимости root. **Не** создаёт **вложенный** `NamespaceSnapshot` под root (**INV-T1** — политика трека, не «особый» kind). **Не** содержит `if DemoVM`. |
| **Demo VM / Disk snapshot controllers** | Создают и ведут **DemoVirtualMachineSnapshot**, **DemoVirtualDiskSnapshot**, их **Content**, **VolumeSnapshot**/VCR, MCR/MCP для manifest leaf. |
| **Согласование с root** | **Пишут** в root **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`** те **доменные** контроллеры (или **опциональный** demo orchestrator — [`02`](02-dsc-wiring.md)), которые **создали** соответствующие дочерние snapshot-узлы: каждый отвечает за **свои** элементы refs (**аддитивный merge**, merge-safe патчи, **без** полной замены списка «чужими» writers — детали контракта PR5 в spec/коде). **Generic** root reconciler **не** заполняет доменные дети в refs «по схеме»; он **читает** refs для **`Ready`** / exclude. **`Ready`** root — каскад снизу вверх по refs ([`07`](07-ready-delete-matrix.md), [`08`](08-universal-snapshot-tree-model.md)). В spec элементы refs могут быть описаны минимально для PR1 — для PR5 **расширяется содержимое тех же полей**, без новых имён полей дерева. |

## Порядок (высокий уровень)

1. Пользователь создаёт **root `NamespaceSnapshot`** на namespace с demo workload.
2. Generic controller: bind root NSC, root MCR→MCP, с **exclude**, **вычисляемым** из API и дерева (см. [`04-coverage-dedup.md`](04-coverage-dedup.md)).
3. **Доменный** контроллер (или orchestrator из [`02`](02-dsc-wiring.md)), **наблюдающий** root **`NamespaceSnapshot`** / namespace по **политике** (label/selector, аннотация на NS и т.д. — фиксируется в spec), **инициирует** создание **`DemoVirtualMachineSnapshot`**; далее **те же** доменные reconciler’ы создают disk snapshots, **VolumeSnapshot**/VCR, MCR/MCP по **`spec`** — **не** generic **`NamespaceSnapshot`** reconciler.
4. Leaf disk: **VolumeSnapshot** для PVC + manifest path; **Ready** пропагируется к VM snapshot, затем к **политике root NS Ready** (расширение §11.1 под heterogeneous children — не дублировать здесь дословно).
5. **Aggregated manifests — граница контрактов:** в **текущем** shipping-контуре действует **PR4**: aggregated read и traversal завязаны на **существующий** обход (NSC-дерево / контракт в [`spec/namespace-snapshot-aggregated-manifests-pr4.md`](../../spec/namespace-snapshot-aggregated-manifests-pr4.md)). **В этом документе** flow **heterogeneous** графа для aggregation **не** нормативен до отдельного шага: поддержка **нескольких MCP** с доменных content и обход **по тем же** **`children*Refs`** потребует **явного** расширения traversal и обновления spec — иначе риск **двух несовместимых** моделей обхода (legacy PR4 vs полный PR5-граф). Пока не смержен контракт расширения — для aggregation опираться **только** на механизм PR4.

## Owner references (**не** дерево, **не** dedup, **не** `Ready`)

- **ownerRef** нужен для **lifecycle**: владение, **GC cascade**, согласованное удаление (например child demo snapshot → parent VM snapshot; при политике — привязка к root NS UID через label или отдельный ref).
- **ownerRef не определяет:** состав логического дерева (это **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`**), dedup/exclude (**вычисление** по API в границах run — [`06`](06-coverage-dedup-keys.md)), ни прямую агрегацию **`Ready`** (она идёт по refs + conditions).
- **ownerRef** может **расходиться** с логическим деревом refs (лишние/отсутствующие связи, ручные правки, гонки) и **не** должен использоваться для **восстановления** или **обхода** дерева — только **`children*Refs`** ([`05`](05-tree-and-graph-invariants.md)).
- **ownerRef не отвечает** на вопросы «этот PVC уже покрыт?» или «этот VirtualDisk уже в VM subtree?» — для этого только **вычисление** по API + дереву refs (см. [`04-coverage-dedup.md`](04-coverage-dedup.md), [`06`](06-coverage-dedup-keys.md), [`08` часть B](08-universal-snapshot-tree-model.md)).

## Удаление (cascade)

Удаление **root `NamespaceSnapshot`**:

- инициирует каскад через **`ownerReference`** и **финализаторы** для объектов, входящих в **текущий snapshot-run** (**run** здесь: объекты, достижимые по **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`** от root, плюс lifecycle-связи — **не** «всё с label» и **не** «всё по ownerRef» как определение run);
- **состав** удаляемых объектов определяется **фактическими связями в API** (**refs** как логическое дерево + **ownerRef** / финализаторы как lifecycle), а не заранее заданной «схемой дерева»;
- **`NamespaceSnapshotContent`**, **MCP** и связанные артефакты **root** namespace capture удаляются по **тем же** правилам модуля, что и сегодня (**N2a/N2b**), **без** отдельной ветки «только для demo»;
- модель **не** предполагает **вложенный** **`NamespaceSnapshot`** под root (**INV-T1**), поэтому **нет** отдельной ветки «каскад nested NS» — дочерние узлы PR5 это **heterogeneous** kinds, снимаемые вместе с run по **ownerRef**/политике модуля.

Сводка по kind и финализаторам — [`07-ready-delete-matrix.md`](07-ready-delete-matrix.md) §6–§7; ограничения **ownerRef** — [`08` часть B](08-universal-snapshot-tree-model.md).

## Открытые вопросы (для следующего раунда spec)

1. Точная форма **edges** root NS → `DemoVirtualMachineSnapshot` / `DemoVirtualDiskSnapshot` / `VolumeSnapshot` в API.
2. Как **aggregated** endpoint однозначно обходит heterogeneous граф без дублирования MCP (один узел — один MCP в merge).
