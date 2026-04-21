# Snapshot flow: root NamespaceSnapshot + heterogeneous snapshot tree

**Статус:** Proposed (исправлено по ревью: **без** вложенных `NamespaceSnapshot`).  
**Цель:** согласовать порядок reconcile, **ownerRef** (иерархия / GC / deletion) отдельно от **coverage/dedup**, и границу без special-case «если DemoVM» в generic reconcile.

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
| **Generic NamespaceSnapshot controller** | Только **root** namespace-level capture: bind **одного** root `NamespaceSnapshotContent`, MCR→MCP для root, политика **Ready** root с учётом **агрегации по графу** (после расширения модели — не только дочерние NS). **Не** порождает child `NamespaceSnapshot`. **Не** содержит `if DemoVM`. |
| **Demo VM / Disk snapshot controllers** | Создают и ведут **DemoVirtualMachineSnapshot**, **DemoVirtualDiskSnapshot**, их **Content**, **VolumeSnapshot**/VCR, MCR/MCP для manifest leaf. |
| **Согласование с root** | Обновление на root `NamespaceSnapshot` / `NamespaceSnapshotContent` **refs на дочерние узлы** (heterogeneous) и/или агрегированные условия — механизм согласуется в spec после апрува дизайна (сейчас N2b `children*Refs` заточены под child **NamespaceSnapshot** — потребуется **расширение**). |

## Порядок (высокий уровень)

1. Пользователь создаёт **root `NamespaceSnapshot`** на namespace с demo workload.
2. Generic controller: bind root NSC, root MCR→MCP, с **exclude** из **coverage** (см. [`04-coverage-dedup.md`](04-coverage-dedup.md)).
3. Demo: по политике root (например label/selector на NS) создаётся **DemoVirtualMachineSnapshot** (и далее disk snapshots, VS, MCP).
4. Leaf disk: **VolumeSnapshot** для PVC + manifest path; **Ready** пропагируется к VM snapshot, затем к **политике root NS Ready** (расширение §11.1 под heterogeneous children — не дублировать здесь дословно).
5. **Aggregated manifests:** сегодняшний PR4 контракт завязан на обход через **NSC**-дерево; для чтения **нескольких MCP** с доменных content потребуется **расширение traversal** (heterogeneous kinds + те же MCP/chunks). Это **отдельный согласованный шаг** после дизайна графа.

## Owner references (**не** dedup)

- **ownerRef** задаёт: иерархию владения, **GC cascade**, модель удаления (например child demo snapshot → parent VM snapshot; при политике — привязка к root NS UID через label или отдельный ref).
- **ownerRef не отвечает** на вопросы «этот PVC уже покрыт?» или «этот VirtualDisk уже в VM subtree?» — для этого только **coverage / exclude** или эквивалент (см. [`04-coverage-dedup.md`](04-coverage-dedup.md)).

## Удаление (cascade)

- Удаление **root `NamespaceSnapshot`**: каскад по **ownerRef / финализаторам** для доменного дерева и стандартная уборка root NSC/MCP по существующим правилам; **нет** цепочки «удалить дочерние NamespaceSnapshot», если их не создавали.

## Открытые вопросы (для следующего раунда spec)

1. Точная форма **edges** root NS → `DemoVirtualMachineSnapshot` / `DemoVirtualDiskSnapshot` / `VolumeSnapshot` в API.
2. Как **aggregated** endpoint однозначно обходит heterogeneous граф без дублирования MCP (один узел — один MCP в merge).
