# `Ready` / Failed / Delete: матрица по kinds (v1)

**Статус:** Proposed — **зафиксировано для согласования перед кодом.**  
**Базовая модель:** единый condition **`Ready`** — [`08-universal-snapshot-tree-model.md`](08-universal-snapshot-tree-model.md) §A.4.  
**Связь:** дерево — [`05-tree-and-graph-invariants.md`](05-tree-and-graph-invariants.md); dedup — [`06-coverage-dedup-keys.md`](06-coverage-dedup-keys.md).

**Не использовать:** отдельный condition **`SubtreeReady`**, поля **`domainSubtreeSummary`**, **`domainCoverage`** в CR.

---

## 1. Единый `Ready`: каскад успеха (снизу вверх)

1. **Лист** (например `DemoVirtualDiskSnapshot`): **`Ready=True`** только когда выполнены **все** его жёсткие зависимости (§2 таблица leaf).
2. **Промежуточный** узел (`DemoVirtualMachineSnapshot`): **`Ready=True`** только если **все обязательные дети** в **`childrenSnapshotRefs`** имеют **`Ready=True`** и собственные зависимости узла (если есть) готовы.
3. **Корень** (`NamespaceSnapshot`): **`Ready=True`** только если root namespace MCP (и пр.) по N2a **и** все прямые дети в **`childrenSnapshotRefs`** имеют **`Ready=True`**.

**INV-R2 (обязательный ребёнок из refs).** Если объект перечислен в **`childrenSnapshotRefs`** или в **`childrenSnapshotContentRefs`** как **обязательный** ребёнок (по политике узла) и **отсутствует в API**, родитель **обязан** иметь **`Ready=False`**, **даже если** все остальные зависимости узла ещё «зелёные».

Пока дети не готовы, родитель остаётся **`Ready=False`** с осмысленным **`reason`** (например ожидание детей), без отдельного «subtree» condition.

---

## 2. Leaf `DemoVirtualDiskSnapshot`: когда `Ready=True` (v1)

| Зависимость | Условие |
|-------------|---------|
| **Volume** | `VolumeSnapshot` **ReadyToUse** (или принятая политика CSI). |
| **Manifest** | `DemoVirtualDiskSnapshotContent`: MCP **Ready**, chunks консистентны (правила N2a). |
| **Итог** | **Оба** выполнены. **INV-R1 (fail-closed):** частичная готовность → **`Ready=False`**, `VolumeSnapshotNotReady` / `ManifestCheckpointNotReady` / аналог. |

---

## 3. Каскад деградации (снизу вверх), единый `Ready`

При любой поломке нижнего уровня:

1. **Ближайший** затронутый объект (leaf content, MCP, leaf snapshot, …) переходит в **`Ready=False`** с **`reason` / `message`**, отражающими **первопричину**.
2. Родитель при reconcile видит ребёнка **`Ready=False`** или **отсутствующего** ребёнка в API → выставляет **`Ready=False`** и **пробрасывает** причину снизу по правилу ниже.
3. Каскад **до корня** (`NamespaceSnapshot`).

**Политика `reason` / `message` (зафиксировано в дизайне, одна на всю систему).** Все контроллеры (generic и доменные) используют **одну** схему: **`reason`** на родителях и корне — **тот же стабильный код**, что выставил **первый** узел, где причина **классифицирована** (обычно leaf или ближайший узел с прямой ошибкой API: MCP, content и т.д.); **не** подменять `reason` на «ближайшую локальную» формулировку на промежуточных уровнях. В **`message`** допускается (и желательно) **контекст пути** (имена/GVK узлов), чтобы по корню было видно **где** случилась первопричина. См. согласование с [`08`](08-universal-snapshot-tree-model.md) §A.4.

**Обязательные примеры `reason` (черновик перечня):**

| `reason` | Когда |
|----------|--------|
| `ManifestChunkMissing` | Удалена / недоступна chunk, битая склейка MCP. |
| `ManifestCheckpointMissing` | MCP / checkpoint удалён или недоступен. |
| `ChildSnapshotMissing` | Дочерний snapshot из **refs** исчез из API (GC, ручное удаление). |
| `ChildSnapshotContentMissing` | Дочерний **SnapshotContent** исчез. |
| `VolumeSnapshotNotReady` | VS не Ready / удалён / ошибка драйвера. |

**INV-R0.** Родитель **не** может остаться **`Ready=True`**, если subtree **физически повреждён**: потерян child snapshot/content, сломан MCP/chunk, VS не готов.

**INV-R3.** Корень **`Ready=True`** только при выполнении §1 и отсутствии противоречий с API; иначе **`Ready=False`** с причиной снизу.

---

## 4. Сценарии деградации (обязательно отразить в тестах)

### 4.1. Успех, затем удалена chunk / сломан MCP

1. Дерево было **`Ready=True`**.  
2. Администратор удаляет **chunk** манифеста (или ломает MCP).  
3. Reconcile **MCP** / **SnapshotContent** обнаруживает проблему → **`Ready=False`**, `reason`: **`ManifestChunkMissing`** (или **`ManifestCheckpointMissing`**), `message` с деталями.  
4. Каскад: **disk content** → **disk snapshot** → **VM snapshot** → **root `NamespaceSnapshot`**: на каждом уровне **`Ready=False`**, причина **отражает первопричину** (не молчаливый False).

### 4.2. Успех, затем удалён дочерний **Snapshot**

1. После успеха удалён например **`DemoVirtualDiskSnapshot`** (ручное удаление / GC).  
2. Родитель (`DemoVirtualMachineSnapshot` или root) при reconcile: ребёнок отсутствует в API при том, что он **обязателен** по refs/политике → **`Ready=False`**, **`reason`**: **`ChildSnapshotMissing`**, `message` с именем/типом.

### 4.3. Успех, затем удалён дочерний **SnapshotContent**

1. Удалён **`DemoVirtualDiskSnapshotContent`** (или другой обязательный content).  
2. Snapshot-родитель при reconcile → **`Ready=False`**, **`reason`**: **`ChildSnapshotContentMissing`** (или согласованный код), каскад дальше вверх как в §3.

---

## 5. Таблица «сбой → кто первым `Ready=False`» (v1)

| Ситуация | Первый уровень сигнала | Каскад |
|----------|------------------------|--------|
| VS timeout / error | disk snapshot | → VM → root |
| MCP / chunk | MCP или disk content | → disk snapshot → … |
| Удалён child snapshot | родитель, ожидавший ребёнка | → вверх |
| Удалён child content | соответствующий snapshot | → вверх |

---

## 6. Delete / ownerRef / финализаторы

Без изменения смысла из предыдущей версии: **GC** по ownerRef, финализаторы — кто поставил, тот снимает; Retain — по правилам модуля. Связь **удаление GC → деградация `Ready`**: см. **[`08` B.8](08-universal-snapshot-tree-model.md#b8-взаимодействие-с-деградацией)**.

## 7. Сводная таблица по kind (`Ready` / delete)

| Kind | `Ready` | Удаление | Финализаторы |
|------|---------|----------|--------------|
| Root NS | §1–§3 | user | модуль |
| DemoVirtualMachineSnapshot | все обязательные дети `Ready` | owner root | demo |
| DemoVirtualDiskSnapshot | §2 | owner VM или root | demo |
| `*SnapshotContent` | MCP/артефакты | с snapshot | см. код |
| VolumeSnapshot | CSI | с disk / политика | demo + CSI |

Детали строк финализаторов — в коде константами.
