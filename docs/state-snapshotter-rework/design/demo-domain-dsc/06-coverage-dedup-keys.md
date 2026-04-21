# Единица дедупликации и ключи coverage (v1)

**Статус:** Proposed — **зафиксировано для согласования перед кодом.**  
**Цель:** однозначно определить «тот же PVC» / «тот же VirtualDisk» и формат **`status.domainCoverage`** (черновое имя), чтобы generic и demo не разъехались.

---

## 1. Два слоя (напоминание)

| Слой | Вопрос |
|------|--------|
| **Data dedup** | Один и тот же **PVC** не получает два **VolumeSnapshot** в одном root run. |
| **Resource dedup** | Один и тот же **логический диск** / объект инвентаря не оказывается в двух ветках snapshot и не дублируется в root MCP. |

---

## 2. Что считается «одним и тем же» для PVC

| Понятие | Определение «тот же PVC» |
|---------|---------------------------|
| **Канонический ключ coverage** | **`pvcUID`** — `metadata.uid` объекта **PersistentVolumeClaim**. |
| **Допустимый дубликат ключа в записи** | Если UID ещё недоступен (редкий гон): временно **`namespace/name`** того же namespace, что root NS; при появлении UID — **заменить** в coverage на UID при следующем reconcile. |
| **Инвариант** | **INV-P1:** В множестве `domainCoverage.pvcUIDs` каждый UID **уникален**. Любой **VolumeSnapshot**, созданный demo для PVC, **обязан** привести к добавлению этого UID в `pvcUIDs` **до** того, как generic начнёт строить список volume-targets (или в одной транзакции policy — порядок фиксируется тестом). |

---

## 3. Что считается «одним и тем же» для VirtualDisk (логический диск без inventory CRD в v1)

В v1 нет объекта **`DemoVirtualDisk`**, поэтому «диск» = запись в **`DemoVirtualMachineSnapshot.spec.disks[]`** или standalone-описание на **`DemoVirtualDiskSnapshot.spec`**.

| Понятие | Определение «тот же диск» |
|---------|----------------------------|
| **Канонический ключ coverage (под VM)** | **`vmSnapshotUID` + `diskIndex`** где `diskIndex` — индекс в неизменяемом списке дисков в spec VM snapshot **на момент создания** дисковых snapshot (или **стабильный `diskId`** string в spec, если добавят — предпочтительнее индекса). |
| **Канонический ключ coverage (standalone disk snapshot)** | **`demoVirtualDiskSnapshotUID`** самого `DemoVirtualDiskSnapshot` **или** **`pvcUID`** диска (достаточно **pvcUID** — тогда диск = PVC one-to-one в v1). |
| **Инвариент** | **INV-D1:** Два активных `DemoVirtualDiskSnapshot` в одном root run **не** могут иметь один и тот же **`pvcUID`**. |
| **INV-D2:** Standalone vs VM | Если **`pvcUID`** уже покрыт диском под VM, **standalone** `DemoVirtualDiskSnapshot` на тот же PVC **не** создаётся (ошибка / отказ reconcile demo). |

**Рекомендация v1 (упрощение):** использовать **`pvcUID`** как **единственный** ключ resource-dedup для диска (достаточно для «диск = один PVC»). Тогда **`domainCoverage.resourceKeys`** = список объектов `{ "type": "pvcUID", "uid": "..." }` без отдельного типа VirtualDisk.

Если позже появится inventory CRD **DemoVirtualDisk**, добавить тип **`demoVirtualDiskUID`**.

---

## 4. Схема поля `status.domainCoverage` (v1 черновик)

Единый JSON-совместимый фрагмент на **`NamespaceSnapshot.status`** (имя поля финализировать в CRD):

```yaml
domainCoverage:
  version: 1
  pvcUIDs: []           # union: все PVC, для которых уже создан путь данных (VS) доменом
  excludedFromRootManifestRefs: []   # опционально: {apiGroup,kind,namespace,name} уже в domain MCP
```

| Подполе | Кто пишет | Кто читает |
|---------|-----------|------------|
| `pvcUIDs` | Demo disk reconciler (после успешного create VS или по политике — после bind VS) | Generic: **исключить** PVC из любого generic volume snapshot path. |
| `excludedFromRootManifestRefs` | Demo (объекты, чей манифест уже в domain MCP) | Generic: **исключить** из allowlist root MCR. |

**INV-C1.** Generic **только читает** `domainCoverage`; **никогда** не пересчитывает dedup логику домена.

**INV-C2.** Demo **только дополняет** множества (merge идемпотентный); удаление ключей — только при откате снимка / явной политике отмены (описать в delete matrix).

---

## 5. Связь с ownerRef

Наличие ownerRef **не** добавляет и **не** удаляет ключи в `domainCoverage` — только явная логика demo + generic чтение списков.
