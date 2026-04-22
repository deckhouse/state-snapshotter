# Test plan: demo domain DSC nested snapshot

**Статус:** Proposed.  
**Связь:** [`design/demo-domain-dsc/README.md`](../design/demo-domain-dsc/README.md); универсальная модель дерева и **`Ready`** — [`08-universal-snapshot-tree-model.md`](../design/demo-domain-dsc/08-universal-snapshot-tree-model.md); инварианты v1 — [`05`](../design/demo-domain-dsc/05-tree-and-graph-invariants.md), [`06`](../design/demo-domain-dsc/06-coverage-dedup-keys.md), [`07`](../design/demo-domain-dsc/07-ready-delete-matrix.md).

**Модель:** **heterogeneous** дерево через общие **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`**; **один** condition **`Ready`** (каскад успеха и деградации); **dedup вычисляется** из API, **без** persisted `domainCoverage` / **`SubtreeReady`**.

**Уровни:** `go test -tags integration ./test/integration/...` / при необходимости cluster smoke — [`e2e-testing-strategy.md`](e2e-testing-strategy.md).

---

## Обязательные сценарии

### 1. Tree creation

- **Given:** namespace с demo workload (VM + диски / standalone disk по дизайну).  
- **When:** создаётся root `NamespaceSnapshot`.  
- **Then:** строится **heterogeneous** дерево; все связи родитель→ребёнок отражены через **`childrenSnapshotRefs`** и **`childrenSnapshotContentRefs`** на соответствующих `*Snapshot` / `*SnapshotContent` ([`05`](../design/demo-domain-dsc/05-tree-and-graph-invariants.md) §2). **Нет** дочернего `NamespaceSnapshot` под VM/disk.

### 2. Ready propagation

- **`Ready`** выставляется **снизу вверх**: leaf (disk + VS + MCP) → VM snapshot → root NS.  
- Родитель **`Ready=True`** только когда все обязательные дети **`Ready=True`** и собственные зависимости готовы ([`07`](../design/demo-domain-dsc/07-ready-delete-matrix.md) §1).

### 3. PVC / data dedup

- **Given:** PVC подключён к диску домена.  
- **When:** отрабатывают доменный путь и generic root capture (**вычисляемый** exclude по [`06`](../design/demo-domain-dsc/06-coverage-dedup-keys.md) §4).  
- **Then:** **нет** второго `VolumeSnapshot` на тот же PVC; проверка по фактам API, не по полю в CR.

### 4. Resource dedup (VirtualDisk / logical disk)

- **Given:** диск участвует в VM и потенциально виден как standalone.  
- **Then:** один согласованный snapshot-path; нет двойного доменного пути; в aggregated / root MCP нет дублирующего представления ([`04`](../design/demo-domain-dsc/04-coverage-dedup.md)).

### 5. Degradation after success (`Ready` каскадом вверх)

Подсценарии (все **обязательны** для полного закрытия поведения `Ready`):

**5a. Chunk / MCP**

- После успеха удалена **chunk** или сломан **MCP** → reconcile → ближайший узел **`Ready=False`**, `reason` вроде **`ManifestChunkMissing`** / **`ManifestCheckpointMissing`** → каскад до root, **причина сохраняется** ([`07`](../design/demo-domain-dsc/07-ready-delete-matrix.md) §4.1).

**5b. Удалён дочерний Snapshot**

- Удалён дочерний **`DemoVirtualDiskSnapshot`** (или другой обязательный child) → родитель и выше **`Ready=False`**, **`ChildSnapshotMissing`** (или согласованный код) ([`07`](../design/demo-domain-dsc/07-ready-delete-matrix.md) §4.2).

**5c. Удалён дочерний SnapshotContent**

- Удалён **`DemoVirtualDiskSnapshotContent`** (или другой обязательный content) → родительский snapshot **`Ready=False`**, **`ChildSnapshotContentMissing`** → каскад вверх ([`07`](../design/demo-domain-dsc/07-ready-delete-matrix.md) §4.3).

**INV:** root **не** остаётся **`Ready=True`**, если subtree физически повреждён (**[`07`](../design/demo-domain-dsc/07-ready-delete-matrix.md) INV-R0**).

### 6. Delete cascade

- Удаление root `NamespaceSnapshot` → согласованный каскад по **ownerRef** / финализаторам; согласование с [`07`](../design/demo-domain-dsc/07-ready-delete-matrix.md) §6 и [`08`](../design/demo-domain-dsc/08-universal-snapshot-tree-model.md) часть B.

## Негативные сценарии (дополнительно)

- Частичный провал disk → VM и root **`Ready=False`** с ожидаемым **`reason`**.  
- DSC без RBACReady → корректная деградация без panic.

## Команды (после реализации)

- `go test -tags integration ./test/integration/...`  
- При необходимости — cluster smoke по согласованию.

## Критерии приёмки

**Merge gate (PR5 / demo-domain):** сценарии **1–4** и **6** — обязательны при закрытии трека. **Каскад `Ready` и деградация после успеха — часть DoD PR5, не «потом»:** подсценарии **5a**, **5b**, **5c** (**chunk/MCP**, **удалён дочерний Snapshot**, **удалён дочерний SnapshotContent**) — каждый **merge-gate**; закрытие PR5 без зелёных **5a–5c** недопустимо при принятой модели единого **`Ready`** ([`07`](../design/demo-domain-dsc/07-ready-delete-matrix.md) §3–§4, **INV-R2**).
