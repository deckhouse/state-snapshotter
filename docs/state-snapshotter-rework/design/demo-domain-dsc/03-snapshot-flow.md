# Snapshot flow: root NamespaceSnapshot + heterogeneous snapshot tree

**Статус:** Proposed (исправлено по ревью: **без** вложенных `NamespaceSnapshot`).  
**Цель:** согласовать порядок reconcile, **ownerRef** (иерархия / GC / deletion) отдельно от **вычисляемого dedup/exclude**, и границу без special-case «если DemoVM» в generic reconcile.

**Три оси (кратко):** дерево — **`children*Refs`**; готовность — **`Ready`** по refs + зависимостям; lifecycle — **ownerRef**/финализаторы ([`05` §3](05-tree-and-graph-invariants.md) таблица «Три оси», [`08` B](08-universal-snapshot-tree-model.md)).

## Целевое дерево (логическое)

Под **одним** root **`NamespaceSnapshot`** (снимок namespace) живут **не** другие NS snapshot, а **разные kinds** snapshot/content:

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

Имена demo kinds — см. [`01-api.md`](01-api.md). **Смысл:** PR5 / real domain wiring — это **heterogeneous snapshot graph**, а не «заменить synthetic на child `NamespaceSnapshot`».

## Участники

| Участник | Роль |
|----------|------|
| **Generic NamespaceSnapshot controller** | Только **root** namespace-level capture: bind **одного** root `NamespaceSnapshotContent`, MCR→MCP для root; **`Ready`** root — каскад по **`childrenSnapshotRefs`** (heterogeneous дети после расширения элементов refs в spec) и собственные зависимости root. **Не** порождает child `NamespaceSnapshot`. **Не** содержит `if DemoVM`. |
| **Demo VM / Disk snapshot controllers** | Создают и ведут **DemoVirtualMachineSnapshot**, **DemoVirtualDiskSnapshot**, их **Content**, **VolumeSnapshot**/VCR, MCR/MCP для manifest leaf. |
| **Согласование с root** | Обновление на root **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`** на heterogeneous детей; **`Ready`** root — каскад снизу вверх ([`07`](07-ready-delete-matrix.md), [`08`](08-universal-snapshot-tree-model.md)). В spec элементы refs могут быть описаны минимально для PR1 — для PR5 **расширяется содержимое тех же полей**, без новых имён полей дерева. |

## Порядок (высокий уровень)

1. Пользователь создаёт **root `NamespaceSnapshot`** на namespace с demo workload.
2. Generic controller: bind root NSC, root MCR→MCP, с **exclude**, **вычисляемым** из API и дерева (см. [`04-coverage-dedup.md`](04-coverage-dedup.md)).
3. Demo: по политике root (например label/selector на NS) создаётся **DemoVirtualMachineSnapshot** (и далее disk snapshots, VS, MCP).
4. Leaf disk: **VolumeSnapshot** для PVC + manifest path; **Ready** пропагируется к VM snapshot, затем к **политике root NS Ready** (расширение §11.1 под heterogeneous children — не дублировать здесь дословно).
5. **Aggregated manifests:** сегодняшний PR4 контракт завязан на обход через **NSC**-дерево; для чтения **нескольких MCP** с доменных content потребуется **расширение traversal** (heterogeneous kinds + те же MCP/chunks). Это **отдельный согласованный шаг** после дизайна графа.

## Owner references (**не** дерево, **не** dedup, **не** `Ready`)

- **ownerRef** нужен для **lifecycle**: владение, **GC cascade**, согласованное удаление (например child demo snapshot → parent VM snapshot; при политике — привязка к root NS UID через label или отдельный ref).
- **ownerRef не определяет:** состав логического дерева (это **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`**), dedup/exclude (**вычисление** по API в границах run — [`06`](06-coverage-dedup-keys.md)), ни прямую агрегацию **`Ready`** (она идёт по refs + conditions).
- **ownerRef не отвечает** на вопросы «этот PVC уже покрыт?» или «этот VirtualDisk уже в VM subtree?» — для этого только **вычисление** по API + дереву refs (см. [`04-coverage-dedup.md`](04-coverage-dedup.md), [`06`](06-coverage-dedup-keys.md), [`08` часть B](08-universal-snapshot-tree-model.md)).

## Удаление (cascade)

- Удаление **root `NamespaceSnapshot`**: каскад по **ownerRef / финализаторам** для доменного дерева и стандартная уборка root NSC/MCP по существующим правилам; **нет** цепочки «удалить дочерние NamespaceSnapshot», если их не создавали.

## Открытые вопросы (для следующего раунда spec)

1. Точная форма **edges** root NS → `DemoVirtualMachineSnapshot` / `DemoVirtualDiskSnapshot` / `VolumeSnapshot` в API.
2. Как **aggregated** endpoint однозначно обходит heterogeneous граф без дублирования MCP (один узел — один MCP в merge).
