# Test plan: demo domain DSC nested snapshot

**Статус:** Proposed.  
**Связь:** [`design/demo-domain-dsc/README.md`](../design/demo-domain-dsc/README.md); инварианты v1 — [`05`](../design/demo-domain-dsc/05-tree-and-graph-invariants.md), [`06`](../design/demo-domain-dsc/06-coverage-dedup-keys.md), [`07`](../design/demo-domain-dsc/07-ready-delete-matrix.md).  
**Модель дерева:** под root **`NamespaceSnapshot`** — **heterogeneous** узлы (**DemoVirtualMachineSnapshot**, **DemoVirtualDiskSnapshot**, **VolumeSnapshot** + `*Content`); **без** вложенных **`NamespaceSnapshot`**.  
**Уровни:** unit / integration (`-tags integration`) / cluster smoke — см. [`e2e-testing-strategy.md`](e2e-testing-strategy.md).

## Обязательные сценарии

### 1. Tree creation

- **Given:** namespace с `DemoVirtualMachine` + `DemoVirtualDisk` + PVC.  
- **When:** создаётся root `NamespaceSnapshot`.  
- **Then:** появляются **DemoVirtualMachineSnapshot** (и далее disk snapshots / VS / content) с согласованными **refs с root**; **нет** дочерних `NamespaceSnapshot` под VM/disk. Граф на root NSC соответствует [`03-snapshot-flow.md`](../design/demo-domain-dsc/03-snapshot-flow.md).

### 2. Ready propagation

- Disk / VS / MCP leaf переходят в **Ready**; VM snapshot агрегирует; root `NamespaceSnapshot` получает **`Ready=True`** только при согласованной политике (расширение §11.1 под heterogeneous children — после spec).

### 3. Aggregated manifests

- Несколько MCP в поддереве (root NSC + domain contents) → один вызов **aggregated** по обновлённому контракту traversal; до мержи спека — сценарий помечается как **зависимый от расширения PR4**.

### 4. Delete cascade

- Удаление root `NamespaceSnapshot` → согласованная уборка доменного дерева (ownerRef/finalizers) + root NSC/MCP; **не** «каскад дочерних NamespaceSnapshot».

### 5. PVC / data dedup (**критично**)

- **Given:** PVC подключён к диску; ровно один **VolumeSnapshot** на PVC в рамках root run.  
- **When:** root namespace manifest / data path отрабатывает.  
- **Then:** **нет** второго VolumeSnapshot; проверка по [`04-coverage-dedup.md`](../design/demo-domain-dsc/04-coverage-dedup.md) (coverage, не ownerRef).

### 6. Resource dedup — VirtualDisk (**критично**)

- **Given:** один **DemoVirtualDisk** входит в состав **DemoVirtualMachine** и одновременно существует как объект в namespace (standalone «видимость» в list).  
- **When:** создаётся root snapshot и отрабатывает доменное дерево.  
- **Then:** **один** согласованный domain snapshot path для этого диска (нет двух противоречивых **DemoVirtualDiskSnapshot** за одним run без явной политики); в **aggregated** / root MCP **нет** дублирующего представления диска (см. resource dedup в [`04-coverage-dedup.md`](../design/demo-domain-dsc/04-coverage-dedup.md)).

## Негативные сценарии

- Частичный провал disk snapshot → VM и root **Ready=False** с ожидаемой причиной.
- DSC без RBACReady → demo kinds не активируются; без panic.

## Команды (после реализации)

- `go test -tags integration ./test/integration/...` — новые тесты с префиксом по согласованию ревью.
- Cluster smoke — отдельные env-флаги при необходимости.

## Критерии приёмки

Пункты 1–6; **merge gate** для реализации: **5** (PVC) и **6** (VirtualDisk resource dedup).
