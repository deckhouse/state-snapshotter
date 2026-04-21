# Ready / Failed / Delete: матрица по kinds (v1)

**Статус:** Proposed — **зафиксировано для согласования перед кодом.**  
**Связь:** дерево и ownerRef — [`05-tree-and-graph-invariants.md`](05-tree-and-graph-invariants.md); coverage — [`06-coverage-dedup-keys.md`](06-coverage-dedup-keys.md).

---

## 1. Когда leaf `DemoVirtualDiskSnapshot` считается **Ready** (v1)

| Компонент | Условие для `Ready=True` |
|-----------|---------------------------|
| **Volume (данные)** | `VolumeSnapshot` в состоянии **ReadyToUse** (или эквивалент принятой политики CSI в модуле). |
| **Manifest (MCP)** | `DemoVirtualDiskSnapshotContent`: **`manifestCheckpointName`** выставлен и MCP **Ready** по правилам N2a. |
| **Итог leaf** | **Оба** выполнены. **INV-R1 (v1 fail-closed):** если MCP **Ready**, а VS **не** Ready — leaf **`Ready=False`**, reason **`VolumeNotReady`** (или симметрично **`ManifestNotReady`**). Частичный «полуготовый» leaf **не** считается Ready. |

---

## 2. Поведение при сбоях

| Ситуация | `DemoVirtualDiskSnapshot` | `DemoVirtualMachineSnapshot` | Root **`NamespaceSnapshot`** |
|----------|---------------------------|------------------------------|--------------------------------|
| VS не стал Ready (timeout / error) | `Ready=False`, reason диска | `Ready=False`, **`ChildSnapshotFailed`** или аналог домена на VM snapshot | `Ready=False`, агрегированная причина с **идентификатором** упавшего диска/VS |
| MCP не Ready | `Ready=False`, manifest reason | то же | то же |
| Один диск упал, остальные ОК | упавший — Failed | **INV-R2:** VM snapshot **`Ready=False`** (в v1 **нет** «частичного успеха» VM) | root **`Ready=False`** |

Политика **v1.1** (частичный VM snapshot) — только после явного ADR; в v1 — таблица выше.

---

## 3. Root `NamespaceSnapshot` Ready

| Условие | Обязательно |
|---------|-------------|
| Root MCP (namespace manifest) | **Ready** по N2a. |
| Все прямые дети из **`domainChildSnapshotRefs`** | Каждый в **Ready** (для VM snapshot — см. §2 каскад с дисками). |
| **`domainCoverage`** | Консистентен (нет противоречий — опциональная валидация). |

**INV-R3.** Generic выставляет root **`Ready=True`** только если получен **`domainSubtreeSummary.ready=true`** от агрегатора **или** эквивалентное условие в `conditions` (один канал — см. [`05`](05-tree-and-graph-invariants.md) §3 G3).

---

## 4. Delete / cascade / финализаторы

### 4.1 OwnerRef и GC

| Объект | Удаление при удалении root NS |
|--------|-------------------------------|
| `DemoVirtualMachineSnapshot` | Да (ownerRef на root NS). |
| `DemoVirtualDiskSnapshot` (standalone) | Да. |
| `DemoVirtualDiskSnapshot` (под VM) | Каскадно после удаления VM snapshot (ownerRef на VM snapshot) **или** прямой GC от root — достаточно **одной** схемы; **v1:** цепочка **VM snapshot → disk snapshots** (удаление VM snapshot триггерит удаление дисков). |
| `*SnapshotContent` | За ownerRef на snapshot. |
| `VolumeSnapshot` | Зависит от ownerRef на `DemoVirtualDiskSnapshot`; если только лейблы — **demo** удаляет VS в defer hook при удалении disk snapshot. |

### 4.2 Финализаторы (черновик v1)

| Объект | Финализатор | Кто ставит | Кто снимает |
|--------|-------------|------------|-------------|
| Root `NamespaceSnapshot` | существующий модульный | generic | generic, когда subtree готов к release |
| `DemoVirtualMachineSnapshot` | `demo.../protect-vm-snapshot` | demo VM controller | demo, после дисков очищены или по политике |
| `DemoVirtualDiskSnapshot` | `demo.../protect-disk-snapshot` | demo disk controller | demo, после VS удалён / retain завершён |
| `VolumeSnapshot` | *(опционально)* модульный | demo при создании | demo или CSI driver политика |

**INV-DEL1.** Снимать финализатор **только** тот контроллер, который его поставил (или documented handoff).

**INV-DEL2.** При **Retain** на root NS — поведение VS/MCP по существующим правилам модуля + явные строки в тест-плане.

---

## 5. Сводная таблица по kind (v1)

| Kind | Ready | Failed | Delete trigger | Финализатор |
|------|-------|--------|----------------|-------------|
| Root NS | §3 | любой child / root MCP | user | модуль |
| DemoVirtualMachineSnapshot | все disk leaf Ready | любой disk Failed | owner root NS удалён | demo |
| DemoVirtualMachineSnapshotContent | MCP VM готов | MCP error | с объектом VM snap | demo/generic по split |
| DemoVirtualDiskSnapshot | §1 | §2 | owner VM или root | demo |
| DemoVirtualDiskSnapshotContent | MCP disk готов | MCP error | с disk snap | demo |
| VolumeSnapshot | CSI ReadyToUse | CSI error | с disk snap / retain policy | demo + CSI |

Детали имён финализаторов — в коде константами; этот документ задаёт **обязанности**, не строковые литералы.
