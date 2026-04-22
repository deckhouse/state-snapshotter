# Test plan: demo domain DSC nested snapshot

**Статус:** Proposed.  
**Связь:** [`design/demo-domain-dsc/README.md`](../design/demo-domain-dsc/README.md); универсальная модель дерева и **`Ready`** — [`08-universal-snapshot-tree-model.md`](../design/demo-domain-dsc/08-universal-snapshot-tree-model.md); инварианты v1 — [`05`](../design/demo-domain-dsc/05-tree-and-graph-invariants.md), [`06`](../design/demo-domain-dsc/06-coverage-dedup-keys.md), [`07`](../design/demo-domain-dsc/07-ready-delete-matrix.md).

**Модель:** **heterogeneous** дерево через общие **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`**; **один** condition **`Ready`** (каскад успеха и деградации); **dedup вычисляется** из API, **без** persisted `domainCoverage` / **`SubtreeReady`**.

**Уровни:** `go test -tags integration ./test/integration/...` / при необходимости cluster smoke — [`e2e-testing-strategy.md`](e2e-testing-strategy.md).

**Покрытие инвариантов [`07`](../design/demo-domain-dsc/07-ready-delete-matrix.md) и смежных:**

| Правило | Где проверяется |
|---------|-----------------|
| **INV-R2a** (обязательность только по **`children*Refs`**) | **1**, **2**, негатив «объект в API, refs ещё нет» |
| **INV-R2** (ребёнок из refs пропал из API) | **5b**, **5c**, **6** |
| **INV-R0** (родитель не **`Ready=True`** при (a)–(d) из [`07`](../design/demo-domain-dsc/07-ready-delete-matrix.md)) | **5a–5c** и строка **INV** под §5 |
| **INV-R4** (fail-closed: нельзя **`Ready=True`**, если состояние ребёнка неопределимо) | **2** (переходы), негатив при необходимости |
| **INV-R5** (несколько детей с разными **`reason`** → детерминированный выбор на родителе) | негатив «несколько поломок» |
| **INV-S0** / граница run ([`06`](../design/demo-domain-dsc/06-coverage-dedup-keys.md)) | **3**, негатив «чужой run» |
| **INV-E1** (fail-closed dedup, [`06`](../design/demo-domain-dsc/06-coverage-dedup-keys.md)) | **3** при сценариях неполных данных |
| **Первичная классификация** (§3 [`07`](../design/demo-domain-dsc/07-ready-delete-matrix.md): приоритет MCP → content → snapshot) | **5a.1**, **5a.2** (если архитектурно оба пути поддерживаются) |
| **Политика `reason` / `message`** (стабильный **`reason`**, контекст пути в **`message`**, §3 [`07`](../design/demo-domain-dsc/07-ready-delete-matrix.md)) | **5a–5c** (ассерты на **`message`**) |
| **INV-REF-M1** / **INV-REF-M2** (merge-only запись в **`children*Refs`**, удаление только владельцем / политикой родителя, [`05`](../design/demo-domain-dsc/05-tree-and-graph-invariants.md) §1) | **1** + интеграционные ассерты на patch; при появлении двух writers — отдельный сценарий/негатив |
| **INV-REF-C1** (нет самовольного восстановления content из list API при пустых **content refs**, [`05`](../design/demo-domain-dsc/05-tree-and-graph-invariants.md) §1) | **1** или негатив после фиксации поведения в spec |

---

## Обязательные сценарии

### 1. Tree creation

- **Given:** namespace с demo workload (VM + диски / standalone disk по дизайну).  
- **When:** создаётся root `NamespaceSnapshot`.  
- **Then:** строится **heterogeneous** дерево; **ассерты (конкретно):**
  - root **`NamespaceSnapshot.status`** содержит **`childrenSnapshotRefs`** / при политике — **`childrenSnapshotContentRefs`** **только** на **прямых** детей **этого** run (нет «лишних» ссылок вне фактического графа сценария);
  - **`DemoVirtualMachineSnapshot`** содержит refs на **свои** disk snapshot’ы (и согласованный граф под политику **INV-T2** / [`05`](../design/demo-domain-dsc/05-tree-and-graph-invariants.md));
  - disk snapshot / content и **`VolumeSnapshot`** появляются **без** лишних узлов относительно сценария (нет неожиданного второго пути на тот же PVC в рамках run);
  - объект, **существующий** в API, но **не** попавший в **`children*Refs`** дерева этого run, **не** участвует в обходе как узел дерева (**INV-REF1** / [`05`](../design/demo-domain-dsc/05-tree-and-graph-invariants.md), [`08`](../design/demo-domain-dsc/08-universal-snapshot-tree-model.md) A.2).
  - при наблюдаемости в API: запись в **`children*Refs`** согласована с **merge-only** по элементу (**INV-REF-M1**); generic **не** обогащает граф content’ами через list/search при отсутствии нормативного правила (**INV-REF-C1**).
  - **Нет** **вложенного** `NamespaceSnapshot` под root для VM/disk (**INV-T1**).

### 2. Ready propagation

- **`Ready`** выставляется **снизу вверх**: leaf (disk + VS + MCP / content по §3 [`07`](../design/demo-domain-dsc/07-ready-delete-matrix.md)) → VM snapshot → root NS.  
- Родитель **`Ready=True`** только когда **все обязательные** узлы из **`childrenSnapshotRefs`** и **`childrenSnapshotContentRefs`** в состоянии, достаточном для §1, и собственные зависимости готовы ([`07`](../design/demo-domain-dsc/07-ready-delete-matrix.md) §1, **INV-R2a**). Для типов без своего **`Ready`** на snapshot — учёт через owning **`*SnapshotContent`** / правило первичной классификации (**§3** того же документа).

**Дополнительно ассертить переходы (не только финал):**

- root **не** переходит в **`Ready=True`** **раньше**, чем leaf-зависимости, необходимые для §1, стали достаточно готовы;
- **`DemoVirtualMachineSnapshot`** **не** **`Ready=True`**, пока **все обязательные** disk-children из refs **не** в состоянии, достаточном для §1;
- при уже **записанных** refs на обязательного ребёнка, но ребёнок ещё **не** **`Ready=True`**, root остаётся **`Ready=False`** (нет «зелёного» root при жёлтых детях);
- при **неопределённости** чтения состояния обязательного ребёнка — предок **`Ready=False`** (**INV-R4**), а не **`Ready=True`** из-за пропуска проверки.

### 3. PVC / data dedup

- **Given:** PVC подключён к диску домена.  
- **When:** отрабатывают доменный путь и generic root capture (**вычисляемый** exclude по [`06`](../design/demo-domain-dsc/06-coverage-dedup-keys.md) §4).  
- **Then:** **нет** второго `VolumeSnapshot` на тот же PVC **в этом run**; проверка по фактам API, **не** по полю в CR.  
- **Граница run (INV-S0):** в namespace присутствует **другой** snapshot / VS / артефакт, **не** связанный через **`children*Refs`** с **текущим** root run — dedup/exclude **этого** run **не** меняется из‑за «чужого» объекта (generic **не** учитывает внешние деревья).

### 4. Resource dedup (VirtualDisk / logical disk)

- **Given:** диск участвует в VM и потенциально виден как standalone.  
- **Then (источник истины — фактические refs + API):**
  - по **`childrenSnapshotRefs`** / **`childrenSnapshotContentRefs`** виден **один** согласованный snapshot-path на logical disk / **`pvcUID`** в рамках run (**INV-T2** доменная политика + [`04`](../design/demo-domain-dsc/04-coverage-dedup.md));
  - **нет** второго доменного пути данных на тот же **`pvcUID`** в противоречии с политикой;
  - в **aggregated** / root MCP **нет** дублирующего представления одного и того же объекта манифеста (**INV-P1b** / [`06`](../design/demo-domain-dsc/06-coverage-dedup-keys.md) применительно к манифестам — как только контракт PR4/PR5 зафиксирован в spec).

### 5. Degradation after success (`Ready` каскадом вверх)

Подсценарии (все **обязательны** для полного закрытия поведения `Ready`):

**5a. Chunk / MCP (первичная классификация)**

Покрыть **оба** архитектурно допустимых пути из [`07`](../design/demo-domain-dsc/07-ready-delete-matrix.md) §3 (если в реализации поддерживаются оба); иначе явно задокументировать в PR, какой путь единственный, и оставить один подвариант:

- **5a.1** — первичный сигнал с **MCP / ManifestCheckpoint** с **conditions** (приоритет §3 п.1): после успеха удалена **chunk** или сломан **MCP** → reconcile → **`Ready=False`**, `reason` **`ManifestChunkMissing`** / **`ManifestCheckpointMissing`** → каскад до root, **тот же** **`reason`** ([`07`](../design/demo-domain-dsc/07-ready-delete-matrix.md) §4.1).  
- **5a.2** — сценарий, где первичная классификация идёт через **owning `*SnapshotContent`** (§3 п.2), если тип **не** публикует публичный слой conditions на MCP — те же коды **`reason`** и каскад.

**На 5a–5c дополнительно:** на родителях и корне **`message`** содержит **достаточный контекст пути** (минимум: имя дочернего узла **или** GVK **или** идентификатор MCP/chunk — по тому, что применимо), чтобы по корню было видно **где** первопричина ([`07`](../design/demo-domain-dsc/07-ready-delete-matrix.md) §3 политика **`message`**).

**5b. Удалён дочерний Snapshot**

- Удалён дочерний **`DemoVirtualDiskSnapshot`** (или другой обязательный child) → родитель и выше **`Ready=False`**, **`ChildSnapshotMissing`** (или согласованный код) ([`07`](../design/demo-domain-dsc/07-ready-delete-matrix.md) §4.2). Проверить **`message`** (контекст пути) как выше.

**5c. Удалён дочерний SnapshotContent**

- Удалён **`DemoVirtualDiskSnapshotContent`** (или другой обязательный content) → родительский snapshot **`Ready=False`**, **`ChildSnapshotContentMissing`** → каскад вверх ([`07`](../design/demo-domain-dsc/07-ready-delete-matrix.md) §4.3). Проверить **`message`** (контекст пути) как выше.

**INV:** root **не** остаётся **`Ready=True`**, если выполняется **INV-R0** ((a)–(d) в [`07`](../design/demo-domain-dsc/07-ready-delete-matrix.md)); отсутствие в API обязательного узла **из refs** — **INV-R2** (**5b**, **5c**).

### 6. Delete cascade

- **When:** удаление root `NamespaceSnapshot`.  
- **Then (ассерты):**
  - объекты **текущего** run очищаются по **lifecycle** (**ownerRef** / финализаторы / Retain-Delete) согласно [`07`](../design/demo-domain-dsc/07-ready-delete-matrix.md) §6 и [`08`](../design/demo-domain-dsc/08-universal-snapshot-tree-model.md) часть B;
  - артефакты **root** namespace capture (**`NamespaceSnapshotContent`**, root MCP и т.д.) удаляются по **обычным** правилам модуля (**N2a/N2b**), **без** отдельной ветки «только для demo» в generic;
  - при политике без retain **не** остаётся orphan demo snapshot/content/VS, если модуль этого требует;
  - удаление root **не** требует **special-case** «demo branch» в generic reconcile вне общих правил.

## Негативные сценарии (дополнительно)

- Частичный провал disk → VM и root **`Ready=False`** с ожидаемым **`reason`**.  
- DSC без RBACReady → корректная деградация без panic.  
- Объект уже в API, но **ещё не** записан в **`children*Refs`** родителя → **не** считается обязательным ребёнком для **`Ready`** родителя (**INV-R2a**; согласованность с **1**, **2**).  
- Второй snapshot-run / VS в том же namespace **без** связи **refs** с текущим root → **не** влияет на dedup текущего run (**INV-S0**, стык с **3**).  
- **Несколько** одновременных поломок у детей с **разными** **`reason`** → на родителе/корне **`reason`** выбирается **детерминированно** по **INV-R5** ([`07`](../design/demo-domain-dsc/07-ready-delete-matrix.md) §3).

## Команды (после реализации)

- `go test -tags integration ./test/integration/...`  
- При необходимости — cluster smoke по согласованию.

## Критерии приёмки

**Merge gate (PR5 / demo-domain):** обязательны сценарии **1**, **2**, **3**, **4**, **6** **и** каждый из подпунктов деградации **`Ready`**: **5a** (включая **5a.1** / **5a.2** по фактической поддержке путей первичной классификации), **5b**, **5c**. Закрытие PR5 без зелёных перечисленных сценариев недопустимо ([`07`](../design/demo-domain-dsc/07-ready-delete-matrix.md) §1–§4, **INV-R0**, **INV-R2**, **INV-R2a**, **INV-S0**, **INV-R4**, **INV-R5** — по таблице покрытия выше).
