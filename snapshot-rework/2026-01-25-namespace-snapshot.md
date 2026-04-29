# Полный флоу NamespaceSnapshot: создание, экспорт, импорт, восстановление

> **Продуктовое ТЗ (полный сценарий):** этот документ — описание целевого end-to-end потока, включая **`NamespaceSnapshotContent`**, дочерние снимки, экспорт/импорт и ObjectKeeper. В дереве [`docs/state-snapshotter-rework/`](../docs/state-snapshotter-rework/) может параллельно вестись **суженный** инженерный трек (MVP); при расхождении приоритет определяется явным ADR/обновлением спеки, а не «тихой» подменой ТЗ снизу.
>
> Ссылки на модуль: [`spec/system-spec.md`](../docs/state-snapshotter-rework/spec/system-spec.md), [`design/implementation-plan.md`](../docs/state-snapshotter-rework/design/implementation-plan.md). Диаграмма связей: [`unified-snapshot-detailed.png`](unified-snapshot-detailed.png).

Этот документ описывает жизненный цикл снимка namespace на примере namespace `demo`, содержащего различные типы ресурсов.
В документе опущены детали реализации ownerReferences и ObjectKeeper как удержания артефактов. Общее правило такое:

- ObjectKeeper с FollowObjectWithTTL создаётся общим контроллером только для корневых SnapshotContent'ов (тех, которые не имеют ownerReference на другой SnapshotContent)
- ObjectKeeper с FollowObject создаётся request контроллерами для ресурсов ManifestCheckpoint и VolumeSnapshotContent.
- Общий контроллер при обработке снимка ставит дополнительный ownerReference на ManifestCheckpoint и VolumeSnapshotContent, который указывает владельцем соответствующий SnapshotContent, к которому они принадлежат.

## 1. Исходное состояние namespace

Namespace `demo` содержит следующие ресурсы:

| Ресурс | Имя | Описание |
|--------|-----|----------|
| VirtualMachine | `my-vm` | VM с 2 дисками и зарезервированным IP |
| VirtualDisk | `root-disk` | Системный диск VM (10 GiB, StorageClass: ceph-ssd) |
| VirtualDisk | `data-disk` | Диск данных VM (50 GiB, StorageClass: ceph-hdd) |
| VirtualMachineIPAddress | `my-vm-ip` | Зарезервированный IP для VM |
| VirtualDisk | `standalone-disk` | Диск, не привязанный к VM (20 GiB) |
| StatefulSet | `my-sts` | 2 реплики с PVC |
| PVC | `data-my-sts-0` | PVC первой реплики StatefulSet (5 GiB) |
| PVC | `data-my-sts-1` | PVC второй реплики StatefulSet (5 GiB) |
| Deployment | `my-deploy` | 3 реплики |
| Pod | `standalone-pod` | Pod без ownerReference |
| PVC | `standalone-pvc` | PVC в статусе Bound, не используется Pod'ом (10 GiB) |
| Service | `my-svc` | ClusterIP сервис |
| Ingress | `my-ingress` | Ingress для внешнего доступа |

---

## 2. DomainSpecificSnapshotController и priority

В кластере зарегистрирован доменный контроллер виртуализации:

```yaml
apiVersion: state-snapshotter.deckhouse.io/v1alpha1
kind: DomainSpecificSnapshotController
metadata:
  name: virtualization
spec:
  snapshotResourceMapping:
    - resourceCRDName: virtualmachines.virtualization.deckhouse.io
      snapshotCRDName: virtualmachinesnapshots.virtualization.deckhouse.io
      priority: 0    # Обрабатывается первой
    - resourceCRDName: virtualdisks.virtualization.deckhouse.io
      snapshotCRDName: virtualdisksnapshots.virtualization.deckhouse.io
      priority: 1    # Обрабатывается после VM
```

Примечание:
- типы snapshot/content и RBAC/watch определяются через `spec.snapshotTypes` DSC — см. ADR
  `2026-01-23-unified-snapshots-registry-and-restore-rework.md`;
- `snapshotResourceMapping` использует только `snapshotCRDName`, объявленные в `spec.snapshotTypes`.

### Логика priority

1. Общий контроллер сортирует ресурсы namespace по priority (0 → 1 → 2 → ...)
2. Ресурсы с priority: 0 обрабатываются первыми (VirtualMachine)
3. При создании VirtualMachineSnapshot доменный контроллер создаёт дочерние VirtualDiskSnapshot для привязанных дисков
4. Затем обрабатываются ресурсы с priority: 1 (VirtualDisk)
5. Если VirtualDisk уже попал в дочерний снимок VirtualMachineSnapshot — он пропускается
6. VirtualDisk, не попавший в вышестоящий снимок, создаёт отдельный VirtualDiskSnapshot

---

## 3. Создание NamespaceSnapshot

### 3.1 Пользователь создаёт NamespaceSnapshot

```yaml
apiVersion: state-snapshotter.deckhouse.io/v1alpha1
kind: NamespaceSnapshot  # cluster-scoped
metadata:
  name: demo-snapshot
  # namespace отсутствует — NamespaceSnapshot cluster-scoped
spec:
  namespaceName: demo  # целевой namespace для снимка
```

### 3.2 Алгоритм обработки общим контроллером

```text
┌─────────────────────────────────────────────────────────────────┐
│                    Общий контроллер                             │
├─────────────────────────────────────────────────────────────────┤
│ 1. Получить список всех ресурсов в namespace demo               │
│                                                                 │
│ 2. Исключить ресурсы с ownerReference на ресурсы в namespace:   │
│    - Pod/my-sts-0, Pod/my-sts-1 (owner: StatefulSet/my-sts)     │
│    - Pod/my-deploy-xxx (owner: ReplicaSet/my-deploy-xxx)        │
│    - ReplicaSet/my-deploy-xxx (owner: Deployment/my-deploy)     │
│                                                                 │
│ 3. Сгруппировать по DomainSpecificSnapshotController:           │
│    - VirtualMachine/my-vm → priority: 0                         │
│    - VirtualDisk/root-disk, data-disk, standalone-disk → p: 1   │
│    - Остальные → общий контроллер                               │
│                                                                 │
│ 4. Обработать по priority (создавая соответствующие Snapshot):  │
│    priority: 0 → VirtualMachine/my-vm                           │
│    priority: 1 → VirtualDisk (проверить, не в снимке ли уже)    │
│                                                                 │
│ 5. Обработать остальные ресурсы через MCR/VCR                   │
└─────────────────────────────────────────────────────────────────┘
```

**Определение "ресурс уже в снимке":** При последовательной обработке ресурсов по priority (0 → 1 → 2 → ...) общий контроллер для каждого ресурса проверяет, существует ли уже снимок соответствующего типа с ownerRef, указывающим на текущий NamespaceSnapshot или его дочерние снимки. Например, для VirtualDisk проверяется наличие VirtualDiskSnapshot. Если такой снимок найден — ресурс пропускается, так как он уже был заснапшочен на предыдущем шаге.
> Примечание: для быстрых проверок можно использовать индекс покрытия:
> детерминированные имена ожидаемых snapshot’ов (GET по имени) или
> in‑memory set покрытых имён (например, `coveredDiskNames`) после `Ready=True`.

### 3.3 Шаг 1: Обработка VirtualMachine (priority: 0)

Общий контроллер создаёт `VirtualMachineSnapshot`:

```yaml
apiVersion: virtualization.deckhouse.io/v1alpha1
kind: VirtualMachineSnapshot
metadata:
  name: demo-snapshot-my-vm
  namespace: demo
  ownerReferences:
    - apiVersion: state-snapshotter.deckhouse.io/v1alpha1
      kind: NamespaceSnapshot
      name: demo-snapshot
      uid: <namespace-snapshot-uid>
spec:
  sourceRef:
    apiVersion: virtualization.deckhouse.io/v1alpha1
    kind: VirtualMachine
    name: my-vm
```

Доменный контроллер виртуализации:

1. Обрабатывает VirtualMachineSnapshot:
   - создаёт `ManifestCaptureRequest` для VirtualMachine и VirtualMachineIPAddress
   - создаёт дочерние `VirtualDiskSnapshot` для root-disk и data-disk с ownerRef на `VirtualMachineSnapshot`
2. Обрабатывает VirtualDiskSnapshot'ы:
   - создаёт `VolumeCaptureRequest` для VirtualDisk'ов root-disk и data-disk
   - создаёт `ManifestCaptureRequest` для VirtualDisk'ов root-disk и data-disk

**Дочерние VirtualDiskSnapshot:**

```yaml
apiVersion: virtualization.deckhouse.io/v1alpha1
kind: VirtualDiskSnapshot
metadata:
  name: demo-snapshot-my-vm-root-disk
  namespace: demo
  ownerReferences:
    - apiVersion: virtualization.deckhouse.io/v1alpha1
      kind: VirtualMachineSnapshot
      name: demo-snapshot-my-vm
spec:
  virtualDiskName: root-disk
---
apiVersion: virtualization.deckhouse.io/v1alpha1
kind: VirtualDiskSnapshot
metadata:
  name: demo-snapshot-my-vm-data-disk
  namespace: demo
  ownerReferences:
    - apiVersion: virtualization.deckhouse.io/v1alpha1
      kind: VirtualMachineSnapshot
      name: demo-snapshot-my-vm
spec:
  virtualDiskName: data-disk
```

### 3.4 Шаг 2: Обработка VirtualDisk (priority: 1)

Общий контроллер проверяет каждый VirtualDisk:

| VirtualDisk       | В снимке? | Действие                    |
|-------------------|-----------|-----------------------------|
| `root-disk`       | Да        | Пропустить                  |
| `data-disk`       | Да        | Пропустить                  |
| `standalone-disk` | Нет       | Создать VirtualDiskSnapshot |


Общий контроллер находит `VirtualDiskSnapshot` с `spec.virtualDiskName: root-disk` и проверяет цепочку ownerRef до текущего `NamespaceSnapshot`. Если цепочка найдена — диск уже в снимке.

Цепочка ownerReference для `root-disk` и `data-disk`:

```text
VirtualDiskSnapshot (root-disk)
    └─ ownerRef → VirtualMachineSnapshot (my-vm)
                      └─ ownerRef → NamespaceSnapshot (demo-snapshot)

VirtualDiskSnapshot (data-disk)
    └─ ownerRef → VirtualMachineSnapshot (my-vm)
                      └─ ownerRef → NamespaceSnapshot (demo-snapshot)

```

**VirtualDiskSnapshot для standalone-disk:**

```yaml
apiVersion: virtualization.deckhouse.io/v1alpha1
kind: VirtualDiskSnapshot
metadata:
  name: demo-snapshot-standalone-disk
  namespace: demo
  ownerReferences:
    - apiVersion: state-snapshotter.deckhouse.io/v1alpha1
      kind: NamespaceSnapshot
      name: demo-snapshot
spec:
  virtualDiskName: standalone-disk
```

Доменный контроллер виртуализации:

1. Обрабатывает VirtualDiskSnapshot:
   - создаёт `VolumeCaptureRequest` для standalone-disk
   - создаёт `ManifestCaptureRequest` для standalone-disk

### 3.5 Шаг 3: Обработка остальных ресурсов (общий контроллер)

**Пропущенные ресурсы:**

| Ресурс | Причина пропуска |
|--------|------------------|
| `VirtualMachine/my-vm` | Обработан доменным контроллером (VirtualMachineSnapshot) |
| `VirtualDisk/root-disk` | Обработан как дочерний VirtualMachineSnapshot |
| `VirtualDisk/data-disk` | Обработан как дочерний VirtualMachineSnapshot |
| `VirtualDisk/standalone-disk` | Обработан доменным контроллером (VirtualDiskSnapshot) |
| `VirtualMachineIPAddress/my-vm-ip` | ресурс уже захвачен в MCR (см. ниже) |
| `Pod/my-sts-0`, `Pod/my-sts-1` | Имеют ownerRef на StatefulSet — восстановятся автоматически |
| `Pod/my-deploy-*` | Имеют ownerRef на ReplicaSet — восстановятся автоматически |
| `ReplicaSet/my-deploy-*` | Имеет ownerRef на Deployment — восстановится автоматически |
| `PVC/standalone-disk` | Имеет ownerRef на VirtualDisk — восстановится автоматически |
| `PVC/root-disk` | Имеет ownerRef на VirtualDisk — восстановится автоматически |
| `PVC/data-disk` | Имеет ownerRef на VirtualDisk — восстановится автоматически |
| `EndpointSlice/my-svc-xxx` | Имеет ownerRef на Service — восстановится автоматически |

**Определение "ресурс уже захвачен в MCR":** Общий контроллер проверяет `status.capturedResources` во всех ManifestCheckpoint, связанных с текущим NamespaceSnapshot (напрямую или через дочерние снимки). Если ресурс найден — он пропускается.

> **TODO:** Для работы этого механизма необходимо добавить `status.capturedResources` в ManifestCheckpoint. См. [2025-11-30-unified-snapshots.md, раздел 7. TODO, пункт 3](./2025-11-30-unified-snapshots.md#7-todo).

**Ресурсы для обработки общим контроллером:**

| Ресурс | Тип обработки |
|--------|---------------|
| `StatefulSet/my-sts` | MCR (манифест) |
| `Deployment/my-deploy` | MCR (манифест) |
| `Pod/standalone-pod` | MCR (манифест) — нет ownerRef |
| `Service/my-svc` | MCR (манифест) |
| `Ingress/my-ingress` | MCR (манифест) |
| `PVC/standalone-pvc` | Дочерний VolumeSnapshot (см. ниже) |
| `PVC/data-my-sts-0` | Дочерний VolumeSnapshot (см. ниже) |
| `PVC/data-my-sts-1` | Дочерний VolumeSnapshot (см. ниже) |

**ManifestCaptureRequest для Kubernetes-ресурсов (без PVC):**

```yaml
apiVersion: state-snapshotter.deckhouse.io/v1alpha1
kind: ManifestCaptureRequest
metadata:
  name: demo-snapshot-k8s-mcr
  namespace: demo
spec:
  targets:
    - apiVersion: apps/v1
      kind: StatefulSet
      name: my-sts
    - apiVersion: apps/v1
      kind: Deployment
      name: my-deploy
    - apiVersion: v1
      kind: Pod
      name: standalone-pod
    - apiVersion: v1
      kind: Service
      name: my-svc
    - apiVersion: networking.k8s.io/v1
      kind: Ingress
      name: my-ingress
    # PVC не включаются — они обрабатываются через дочерние VolumeSnapshot
status:
  checkpointName: mcp-k8s-b2c3d4e5-6f7g
  completionTimestamp: "2026-01-25T12:00:08Z"
  conditions:
    - type: Ready
      status: "True"
```

### 3.6 Шаг 4: Обработка PVC (дочерние VolumeSnapshot)

Для каждого PVC, который не попал в снимки доменных контроллеров, общий контроллер создаёт дочерний `VolumeSnapshot` с ownerRef на `NamespaceSnapshot`.

> **Вариант B:** Согласно [2025-11-30-volume-snapshot-to-unified.md](./2025-11-30-volume-snapshot-to-unified.md), snapshot-controller создаёт `VolumeCaptureRequest` (VCR) и MCR для манифеста PVC.

**VolumeSnapshot для PVC:**

```yaml
# standalone-pvc
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata:
  name: demo-snapshot-standalone-pvc
  namespace: demo
  ownerReferences:
    - apiVersion: state-snapshotter.deckhouse.io/v1alpha1
      kind: NamespaceSnapshot
      name: demo-snapshot
spec:
  source:
    # PVC source name omitted in this historical sketch.
  volumeSnapshotClassName: ceph-ssd-snapclass
status:
  boundVolumeSnapshotContentName: vsc-standalone-pvc-s4t5u6
  readyToUse: true
  # Расширение unified-snapshots (Вариант B):
  manifestCheckpointName: mcp-pvc-standalone-...
---
# data-my-sts-0
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata:
  name: demo-snapshot-data-my-sts-0
  namespace: demo
  ownerReferences:
    - apiVersion: state-snapshotter.deckhouse.io/v1alpha1
      kind: NamespaceSnapshot
      name: demo-snapshot
spec:
  source:
    # PVC source name omitted in this historical sketch.
  volumeSnapshotClassName: ceph-hdd-snapclass
status:
  boundVolumeSnapshotContentName: vsc-data-my-sts-0-v7w8x9
  readyToUse: true
  manifestCheckpointName: mcp-pvc-sts-0-...
---
# data-my-sts-1
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata:
  name: demo-snapshot-data-my-sts-1
  namespace: demo
  ownerReferences:
    - apiVersion: state-snapshotter.deckhouse.io/v1alpha1
      kind: NamespaceSnapshot
      name: demo-snapshot
spec:
  source:
    # PVC source name omitted in this historical sketch.
  volumeSnapshotClassName: ceph-hdd-snapclass
status:
  boundVolumeSnapshotContentName: vsc-data-my-sts-1-y9z0a1
  readyToUse: true
  manifestCheckpointName: mcp-pvc-sts-1-...
```

Snapshot-controller (Вариант B) для каждого VolumeSnapshot:
1. Создаёт `VolumeCaptureRequest` → получает `VolumeSnapshotContent`
2. Создаёт `ManifestCaptureRequest` для PVC → получает `ManifestCheckpoint`
3. Записывает `status.manifestCheckpointName` в VolumeSnapshot

---

## 4. Иерархия снимков

После создания снимка получается следующая иерархия:

```text
NamespaceSnapshot/demo-snapshot (cluster-scoped)
│
├── NamespaceSnapshotContent/demo-snapshot-content (cluster-scoped)
│   └── ManifestCheckpoint/mcp-k8s-b2c3d4e5-6f7g (без PVC — только K8s ресурсы)
│       └── ManifestCheckpointContentChunk/mcp-k8s-b2c3d4e5-6f7g-0
│
├── VirtualMachineSnapshot/demo-snapshot-my-vm (namespaced, child, priority: 0)
│   ├── VirtualMachineSnapshotContent (cluster-scoped)
│   │   └── ManifestCheckpoint/mcp-vm-a1b2c3d4-5e6f
│   │
│   ├── VirtualDiskSnapshot/demo-snapshot-my-vm-root-disk (namespaced, child)
│   │   └── VirtualDiskSnapshotContent (cluster-scoped)
│   │       ├── ManifestCheckpoint/mcp-vd-root-...
│   │       └── VolumeSnapshotContent/vsc-root-disk-x7y8z9
│   │
│   └── VirtualDiskSnapshot/demo-snapshot-my-vm-data-disk (namespaced, child)
│       └── VirtualDiskSnapshotContent (cluster-scoped)
│           ├── ManifestCheckpoint/mcp-vd-data-...
│           └── VolumeSnapshotContent/vsc-data-disk-...
│
├── VirtualDiskSnapshot/demo-snapshot-standalone-disk (namespaced, child, priority: 1)
│   └── VirtualDiskSnapshotContent (cluster-scoped)
│       ├── ManifestCheckpoint/mcp-vd-standalone-...
│       └── VolumeSnapshotContent/vsc-standalone-disk-p1q2r3
│
├── VolumeSnapshot/demo-snapshot-standalone-pvc (namespaced, child, K8s PVC)
│   └── VolumeSnapshotContent/vsc-standalone-pvc-s4t5u6 (cluster-scoped)
│       └── ManifestCheckpoint/mcp-pvc-standalone-... (манифест PVC)
│
├── VolumeSnapshot/demo-snapshot-data-my-sts-0 (namespaced, child, K8s PVC)
│   └── VolumeSnapshotContent/vsc-data-my-sts-0-v7w8x9 (cluster-scoped)
│       └── ManifestCheckpoint/mcp-pvc-sts-0-...
│
└── VolumeSnapshot/demo-snapshot-data-my-sts-1 (namespaced, child, K8s PVC)
    └── VolumeSnapshotContent/vsc-data-my-sts-1-y9z0a1 (cluster-scoped)
        └── ManifestCheckpoint/mcp-pvc-sts-1-...
```

### 4.1 Статус NamespaceSnapshot

```yaml
apiVersion: state-snapshotter.deckhouse.io/v1alpha1
kind: NamespaceSnapshot  # cluster-scoped
metadata:
  name: demo-snapshot
spec:
  namespaceName: demo
status:
  contentName: demo-snapshot-content
  manifestCaptureRequestName: demo-snapshot-k8s-mcr
  childrenSnapshotRefs:
    - kind: VirtualMachineSnapshot
      name: demo-snapshot-my-vm
      namespace: demo
    - kind: VirtualDiskSnapshot
      name: demo-snapshot-standalone-disk
      namespace: demo
    # Дочерние VolumeSnapshot для PVC (Вариант B)
    - kind: VolumeSnapshot
      name: demo-snapshot-standalone-pvc
      namespace: demo
    - kind: VolumeSnapshot
      name: demo-snapshot-data-my-sts-0
      namespace: demo
    - kind: VolumeSnapshot
      name: demo-snapshot-data-my-sts-1
      namespace: demo
  conditions:
    - type: ManifestsReady
      status: "True"
    - type: ChildrenSnapshotsReady
      status: "True"
    - type: Ready
      status: "True"
```

### 4.2 NamespaceSnapshotContent (cluster-scoped)

```yaml
apiVersion: state-snapshotter.deckhouse.io/v1alpha1
kind: NamespaceSnapshotContent
metadata:
  name: demo-snapshot-content
  ownerReferences:
    - apiVersion: state-snapshotter.deckhouse.io/v1alpha1
      kind: ObjectKeeper
      name: ok-demo-snapshot
      uid: <...>
spec:
  namespaceSnapshotRef:
    name: demo-snapshot  # cluster-scoped, namespace не указывается
    uid: <...>
status:
  manifestCheckpointName: mcp-k8s-b2c3d4e5-6f7g
  # dataRefs отсутствует — данные PVC находятся в дочерних VolumeSnapshotContent
  childrenSnapshotContentRefs:
    - kind: VirtualMachineSnapshotContent
      name: demo-snapshot-my-vm-content
    - kind: VirtualDiskSnapshotContent
      name: demo-snapshot-standalone-disk-content
    # VolumeSnapshotContent для PVC
    - kind: VolumeSnapshotContent
      name: vsc-standalone-pvc-s4t5u6
    - kind: VolumeSnapshotContent
      name: vsc-data-my-sts-0-v7w8x9
    - kind: VolumeSnapshotContent
      name: vsc-data-my-sts-1-y9z0a1
  conditions:
    - type: Ready
      status: "True"
```

---

## 5. Экспорт (скачивание) снимка

### 5.1 Создание DataExport

```yaml
apiVersion: storage.deckhouse.io/v1alpha1
kind: DataExport
metadata:
  name: demo-snapshot-export
  namespace: demo
spec:
  targetRef:
    kind: NamespaceSnapshot
    name: demo-snapshot
  publish: true  # Создать Ingress для внешнего доступа
```

### 5.2 Структура архива

```
demo-snapshot-2026-01-25.tar
├── manifests.tar.gz
│   ├── index.yaml
│   └── snapshots/
│       ├── NamespaceSnapshot--demo--demo-snapshot/
│       │   ├── snapshot.yaml
│       │   ├── snapshot-content.yaml
│       │   └── objects/
│       │       ├── StatefulSet.apps.v1--demo--my-sts.yaml
│       │       ├── Deployment.apps.v1--demo--my-deploy.yaml
│       │       ├── Pod.v1--demo--standalone-pod.yaml
│       │       ├── Service.v1--demo--my-svc.yaml
│       │       └── Ingress.networking.k8s.io.v1--demo--my-ingress.yaml
│       │       # PVC не включены — они в дочерних VolumeSnapshot
│       ├── VirtualMachineSnapshot--demo--demo-snapshot-my-vm/
│       │   ├── snapshot.yaml
│       │   ├── snapshot-content.yaml
│       │   └── objects/
│       │       ├── VirtualMachine...yaml
│       │       └── VirtualMachineIPAddress...yaml
│       ├── VirtualDiskSnapshot--demo--demo-snapshot-my-vm-root-disk/
│       │   ├── snapshot.yaml
│       │   ├── snapshot-content.yaml
│       │   └── objects/
│       │       └── VirtualDisk.virtualization.deckhouse.io.v1alpha1--demo--root-disk.yaml
│       ├── VirtualDiskSnapshot--demo--demo-snapshot-my-vm-data-disk/
│       │   ├── snapshot.yaml
│       │   ├── snapshot-content.yaml
│       │   └── objects/
│       │       └── VirtualDisk.virtualization.deckhouse.io.v1alpha1--demo--data-disk.yaml
│       ├── VirtualDiskSnapshot--demo--demo-snapshot-standalone-disk/
│       │   ├── snapshot.yaml
│       │   ├── snapshot-content.yaml
│       │   └── objects/
│       │       └── VirtualDisk.virtualization.deckhouse.io.v1alpha1--demo--standalone-disk.yaml
│       # Дочерние VolumeSnapshot для PVC (Вариант B)
│       ├── VolumeSnapshot--demo--demo-snapshot-standalone-pvc/
│       │   ├── snapshot.yaml
│       │   ├── snapshot-content.yaml
│       │   └── objects/
│       │       └── PersistentVolumeClaim.v1--demo--standalone-pvc.yaml
│       ├── VolumeSnapshot--demo--demo-snapshot-data-my-sts-0/
│       │   └── objects/
│       │       └── PersistentVolumeClaim.v1--demo--data-my-sts-0.yaml
│       └── VolumeSnapshot--demo--demo-snapshot-data-my-sts-1/
│           └── objects/
│               └── PersistentVolumeClaim.v1--demo--data-my-sts-1.yaml
│
└── data/
    ├── VolumeSnapshot--demo--demo-snapshot-standalone-pvc.tar.gz
    ├── VolumeSnapshot--demo--demo-snapshot-data-my-sts-0.tar.gz
    ├── VolumeSnapshot--demo--demo-snapshot-data-my-sts-1.tar.gz
    ├── VirtualDiskSnapshot--demo--demo-snapshot-my-vm-root-disk.tar.gz
    ├── VirtualDiskSnapshot--demo--demo-snapshot-my-vm-data-disk.tar.gz
    └── VirtualDiskSnapshot--demo--demo-snapshot-standalone-disk.tar.gz
```

### 5.3 index.yaml

```yaml
version: "v1"
exportedAt: "2026-01-25T12:00:00Z"
sourceCluster:
  name: prod-cluster
# apiServerURL: https://api.prod.example.com
# clusterUUID: 0db7574a-86d4-4d90-b79f-2d99585fcb31 #k -n kube-system get cm d8-cluster-uuid ???
rootSnapshot:
  id: "NamespaceSnapshot--demo--demo-snapshot"
  kind: NamespaceSnapshot  # cluster-scoped в K8s
  sourceNamespace: demo    # namespace, который был заснапшочен
  name: demo-snapshot
snapshots:
  - id: "NamespaceSnapshot--demo--demo-snapshot"
    kind: NamespaceSnapshot  # cluster-scoped в K8s
    sourceNamespace: demo    # namespace, который был заснапшочен
    name: demo-snapshot
    hasData: false  # данные в дочерних VolumeSnapshot
    children:
      - "VirtualMachineSnapshot--demo--demo-snapshot-my-vm"
      - "VirtualDiskSnapshot--demo--demo-snapshot-standalone-disk"
      # Дочерние VolumeSnapshot для PVC (Вариант B)
      - "VolumeSnapshot--demo--demo-snapshot-standalone-pvc"
      - "VolumeSnapshot--demo--demo-snapshot-data-my-sts-0"
      - "VolumeSnapshot--demo--demo-snapshot-data-my-sts-1"
      
  - id: "VirtualMachineSnapshot--demo--demo-snapshot-my-vm"
    kind: VirtualMachineSnapshot
    namespace: demo
    name: demo-snapshot-my-vm
    parentId: "NamespaceSnapshot--demo--demo-snapshot"
    hasData: false
    children:
      - "VirtualDiskSnapshot--demo--demo-snapshot-my-vm-root-disk"
      - "VirtualDiskSnapshot--demo--demo-snapshot-my-vm-data-disk"
      
  - id: "VirtualDiskSnapshot--demo--demo-snapshot-my-vm-root-disk"
    kind: VirtualDiskSnapshot
    namespace: demo
    name: demo-snapshot-my-vm-root-disk
    parentId: "VirtualMachineSnapshot--demo--demo-snapshot-my-vm"
    hasData: true
    storageClassName: "ceph-ssd"
    dataFile: "data/VirtualDiskSnapshot--demo--demo-snapshot-my-vm-root-disk.tar.gz"
    dataSize: 10737418240
    dataType: block

  - id: "VirtualDiskSnapshot--demo--demo-snapshot-my-vm-data-disk"
    kind: VirtualDiskSnapshot
    namespace: demo
    name: demo-snapshot-my-vm-data-disk
    parentId: "VirtualMachineSnapshot--demo--demo-snapshot-my-vm"
    hasData: true
    storageClassName: "ceph-hdd"
    dataFile: "data/VirtualDiskSnapshot--demo--demo-snapshot-my-vm-data-disk.tar.gz"
    dataSize: 53687091200
    dataType: block

  - id: "VirtualDiskSnapshot--demo--demo-snapshot-standalone-disk"
    kind: VirtualDiskSnapshot
    namespace: demo
    name: demo-snapshot-standalone-disk
    parentId: "NamespaceSnapshot--demo--demo-snapshot"
    hasData: true
    storageClassName: "ceph-ssd"
    dataFile: "data/VirtualDiskSnapshot--demo--demo-snapshot-standalone-disk.tar.gz"
    dataSize: 21474836480
    dataType: block

  # Дочерние VolumeSnapshot для PVC (Вариант B)
  - id: "VolumeSnapshot--demo--demo-snapshot-standalone-pvc"
    kind: VolumeSnapshot
    namespace: demo
    name: demo-snapshot-standalone-pvc
    parentId: "NamespaceSnapshot--demo--demo-snapshot"
    hasData: true
    storageClassName: "ceph-ssd"
    dataFile: "data/VolumeSnapshot--demo--demo-snapshot-standalone-pvc.tar.gz"
    dataSize: 10737418240
    dataType: Filesystem

  - id: "VolumeSnapshot--demo--demo-snapshot-data-my-sts-0"
    kind: VolumeSnapshot
    namespace: demo
    name: demo-snapshot-data-my-sts-0
    parentId: "NamespaceSnapshot--demo--demo-snapshot"
    hasData: true
    storageClassName: "ceph-hdd"
    dataFile: "data/VolumeSnapshot--demo--demo-snapshot-data-my-sts-0.tar.gz"
    dataSize: 5368709120
    dataType: Filesystem

  - id: "VolumeSnapshot--demo--demo-snapshot-data-my-sts-1"
    kind: VolumeSnapshot
    namespace: demo
    name: demo-snapshot-data-my-sts-1
    parentId: "NamespaceSnapshot--demo--demo-snapshot"
    hasData: true
    storageClassName: "ceph-hdd"
    dataFile: "data/VolumeSnapshot--demo--demo-snapshot-data-my-sts-1.tar.gz"
    dataSize: 5368709120
    dataType: Filesystem
```

---

## 6. Импорт (загрузка) снимка

### 6.1 Загрузка архива в новый кластер

```shell
d8 snapshot import ./demo-snapshot-2026-01-25.tar --namespace demo-restored
```

### 6.2 Сущности в архиве и их маппинг при импорте

**Структура архива:**

| Путь в архиве | Что содержит | Что создаётся при импорте |
|---------------|--------------|---------------------------|
| `index.yaml` | Иерархия снимков, размеры данных, storageClassName | Используется для планирования импорта |
| `snapshots/<id>/snapshot.yaml` | Манифест XxxxSnapshot | XxxxSnapshot в целевом кластере |
| `snapshots/<id>/snapshot-content.yaml` | Манифест XxxxSnapshotContent | XxxxSnapshotContent в целевом кластере |
| `snapshots/<id>/objects/*.yaml` | Манифесты объектов из ManifestCheckpoint | ManifestCheckpoint + ManifestCheckpointContentChunk |
| `data/<id>.tar.gz` | Данные тома (block/filesystem) | Временный PVC → VolumeSnapshotContent |

### 6.3 Порядок импорта

#### Шаг 1. Распаковка и анализ архива

CLI распаковывает `manifests.tar.gz` и читает `index.yaml`:

- Определяет иерархию снимков (rootSnapshot, children)
- Извлекает список снимков с данными (`hasData: true`)
- Получает `storageClassName` и `dataSize` для каждого снимка с данными

#### Шаг 2. Создание DataImport и валидация StorageClass

```yaml
apiVersion: storage.deckhouse.io/v1alpha1
kind: DataImport
metadata:
  name: demo-snapshot-import
  namespace: demo-restored
spec:
  sourceRef:
    kind: NamespaceSnapshot
    name: demo-snapshot
  storageClassMapping:           # Опционально
    ceph-ssd: local-ssd
    ceph-hdd: nfs-storage
```

Контроллер DataImport:

- Проверяет наличие StorageClass для каждого снимка с данными
- Если SC не найден и маппинг не указан — DataImport переходит в `Failed`
- Задает поле `status.manifestsUrl` для загрузки манифестов

#### Шаг 3. Загрузка манифестов

CLI отправляет архив `manifests.tar.gz` на `status.manifestsUrl`

#### Шаг 4. Подготовка к загрузке данных

Контроллер для каждого снимка с данными:

1. Создаёт временный PVC с размером из `index.yaml[].dataSize` и `volumeMode` из `dataType`
2. Поднимает http-сервер для приёма данных
3. Добавляет URL в `status.dataUrls[]`

#### Шаг 5. Загрузка данных

CLI параллельно для каждого `data/<id>.tar.gz`:

1. Находит соответствующий URL в `status.dataUrls[]`
2. Стримит данные на `/api/v1/blocks` или `/api/v1/files`
3. Вызывает `POST /api/v1/finished` по завершении

#### Шаг 6. Создание ресурсов в кластере

После загрузки всех данных контроллер создаёт ресурсы **снизу вверх** по иерархии:

1. **Для снимков с данными (leaf-снимки):**
   - Делает VCR от временного PVC → получает `VolumeSnapshotContent`
   - Удаляет временный PVC
   - Создаёт `ManifestCheckpoint` и `ManifestCheckpointContentChunk` для `objects/*.yaml`
   - Создаёт `XxxxSnapshotContent` (`VirtualDiskSnapshotContent` или `VolumeSnapshotContent`) со ссылками на `ManifestCheckpoint`, `VolumeSnapshotContent` и `childrenSnapshotContentRefs[]` если такие есть

2. **Для снимков без данных (parent-снимки):**
   - Создаёт `ManifestCheckpoint` и `ManifestCheckpointContentChunk` для `objects/*.yaml`
   - Создаёт `XxxxSnapshotContent` со ссылкой на `ManifestCheckpoint` и `childrenSnapshotContentRefs[]` если такие есть

3. **Создаёт ObjectKeeper** для корневого SnapshotContent

4. **Создаёт XxxxSnapshot** со ссылками на XxxxSnapshotContent, ставит `Ready=True`

**Порядок создания SnapshotContent'ов для примера demo-snapshot:**

```text
1. VolumeSnapshotContent для standalone-pvc, data-my-sts-0, data-my-sts-1
2. VirtualDiskSnapshotContent для root-disk, data-disk, standalone-disk
3. VirtualMachineSnapshotContent для my-vm
4. NamespaceSnapshotContent для demo-snapshot (корневой, с ObjectKeeper)
```

**Порядок создания Snapshot'ов:**

```text
1. VolumeSnapshot для standalone-pvc, data-my-sts-0, data-my-sts-1
2. VirtualDiskSnapshot для root-disk, data-disk, standalone-disk
3. VirtualMachineSnapshot для my-vm
4. NamespaceSnapshot для demo-snapshot
```

---

## 7. Восстановление из снимка

### 7.1 Получение манифестов

```shell
d8 snapshot restore demo-snapshot --namespace demo-restored
```

CLI вызывает:

```http
GET /apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/demo-restored/namespacesnapshots/demo-snapshot/manifests-with-data-restoration
```

### 7.2 Обработка общим контроллером

Общий контроллер:

1. Загружает манифесты из `ManifestCheckpoint`
2. Для каждого типа ресурса проверяет `DomainSpecificSnapshotController`
3. Отправляет манифесты доменному контроллеру для модификации через mTLS webhook, если тип ресурса есть в `DomainSpecificSnapshotController`.

### 7.3 Запрос к доменному контроллеру

**POST /api/v1/manifests:modify** (mTLS)

```json
{
  "snapshotRef": {
    "apiVersion": "virtualization.deckhouse.io/v1alpha1",
    "kind": "VirtualDiskSnapshot",
    "name": "demo-snapshot-my-vm-root-disk",
    "namespace": "demo-restored"
  },
  "manifests": {
    "disk-uid-root": {
      "apiVersion": "virtualization.deckhouse.io/v1alpha1",
      "kind": "VirtualDisk",
      "metadata": {
        "name": "root-disk",
        "namespace": "demo",
        "uid": "disk-uid-root"
      },
      "spec": {
        "persistentVolumeClaim": {
          "storageClassName": "ceph-ssd",
          "size": "10Gi"
        }
      }
    }
  }
}
```

### 7.4 Ответ доменного контроллера

```json
{
  "results": {
    "disk-uid-root": {
      "manifest": {
        "apiVersion": "virtualization.deckhouse.io/v1alpha1",
        "kind": "VirtualDisk",
        "metadata": {
          "name": "root-disk",
          "namespace": "demo-restored"
        },
        "spec": {
          "persistentVolumeClaim": {
            "storageClassName": "ceph-ssd",
            "size": "10Gi"
          },
          "dataSource": {
            "type": "ObjectRef",
            "objectRef": {
              "kind": "VirtualDiskSnapshot",
              "name": "demo-snapshot-my-vm-root-disk"
            }
          }
        }
      }
    }
  }
}
```

**Что изменилось:**

- `metadata.namespace` → `demo-restored`
- Добавлен `spec.dataSource` → ссылка на `VirtualDiskSnapshot`
- Удалены runtime поля

### 7.5 PVC с dataSource для восстановления данных

Для PVC (StatefulSet, standalone) в манифест добавляется `dataSource`, указывающий на `VolumeSnapshot`:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: data-my-sts-0
  namespace: demo-restored
spec:
  accessModes: ["ReadWriteOnce"]
  storageClassName: ceph-ssd
  resources:
    requests:
      storage: 5Gi
  dataSource:
    kind: VolumeSnapshot
    name: demo-snapshot-data-my-sts-0
    apiGroup: snapshot.storage.k8s.io
```

### 7.6 Конфликты и их разрешение

| Конфликт | Причина | Решение пользователя |
|----------|---------|----------------------|
| `AlreadyExists` | Ресурс с таким именем уже есть | Переименовать в манифесте |
| `StorageClass not found` | В целевом кластере другой StorageClass | Заменить на доступный |
| `VirtualMachineClass not found` | VMClass отличается | Заменить на доступный |
| `IP address conflict` | IP уже занят | Удалить IP lease или изменить |

**Пример: StorageClass не существует**

```shell
$ d8 snapshot restore demo-snapshot --namespace demo-restored

# Автоматически открывается редактор с манифестами
# Пользователь видит ошибку dry-run:
#   Error: VirtualDisk/root-disk: StorageClass "ceph-ssd" not found

# Пользователь меняет storageClassName на local-ssd в редакторе
# Сохраняет и закрывает редактор

# dry-run успешен → манифесты применяются автоматически
Applied 12 resources to namespace demo-restored
```

---

## 8. Sequence Diagram

```text
┌─────────┐     ┌─────────────┐     ┌──────────────┐     ┌─────────────────┐
│   CLI   │     │ Aggregated  │     │    Общий     │     │    Доменный     │
│         │     │     API     │     │  контроллер  │     │   контроллер    │
└────┬────┘     └──────┬──────┘     └──────┬───────┘     └────────┬────────┘
     │                 │                   │                      │
     │ GET /manifests-with-data-restoration│                      │
     │────────────────>│                   │                      │
     │                 │                   │                      │
     │                 │ Загрузить         │                      │
     │                 │ ManifestCheckpoint│                      │
     │                 │──────────────────>│                      │
     │                 │                   │                      │
     │                 │                   │ POST /manifests:modify (mTLS)
     │                 │                   │─────────────────────>│
     │                 │                   │                      │
     │                 │                   │   Модифицированные   │
     │                 │                   │<─────────────────────│
     │                 │                   │      манифесты       │
     │                 │                   │                      │
     │                 │   Готовые YAML    │                      │
     │<────────────────│<──────────────────│                      │
     │                 │                   │                      │
     │ dry-run         │                   │                      │
     │────────────────>│                   │                      │
     │                 │                   │                      │
     │ Ошибки (если есть)                  │                      │
     │<────────────────│                   │                      │
     │                 │                   │                      │
     │ [Пользователь редактирует]          │                      │
     │                 │                   │                      │
     │ dry-run         │                   │                      │
     │────────────────>│                   │                      │
     │                 │                   │                      │
     │ OK (dry-run успешен)                │                      │
     │<────────────────│                   │                      │
     │                 │                   │                      │
     │ apply (автоматически)               │                      │
     │────────────────>│                   │                      │
     │                 │                   │                      │
     │ Success         │                   │                      │
     │<────────────────│                   │                      │
```

## Пример продолжения выполнения снимка, если контроллер упал в момент выполнения снимка

1. Trigger → reconcile `NamespaceSnapshot`.
2. Определить текущую фазу по статусу:
   - `Ready=True` → ничего не делаем (кроме валидаций/финалайзеров).
   - `Ready!=True` → продолжаем сборку.
3. Сформировать список “должны быть дети” по правилам DSC/priority
   (детерминированные имена?).
   Опционально (будущее улучшение): фиксировать “план снапшота” (hash списка целей)
   и проверять его неизменность до Ready=True.
4. Сначала записать ссылки на детей, потом создавать:
   - обновить `status.childrenSnapshotRefs[]` ожидаемыми ссылками.
5. Создать/досоздать дочерние snapshot’ы:
   - GET по ссылке;
   - если нет → CREATE (ownerRef → parent);
   - если есть → проверка ownerRef/labels.
6. Дождаться готовности детей (`Ready=True`).
7. Собрать/создать `SnapshotContent` и ссылки на артефакты
   (`childrenSnapshotContentRefs[]`, `manifestCheckpointName`, data refs).
8. Выставить `Ready=True` у родителя, когда все дети готовы и контент собран.
9. После рестарта продолжать по `status.childrenSnapshotRefs[]`.

## Минимальный контракт доступа

- `GET /manifests-with-data-restoration` разрешён только субъектам,
  имеющим права на `spec.namespaceName` соответствующего `NamespaceSnapshot`.
- Проверка выполняется через admission и/или `SubjectAccessReview` на целевой namespace.

---

## TODO

- [ ] **RBAC для пользователей NamespaceSnapshot**: проработать модель прав, при которой:
  - Пользователь может создавать/просматривать/восстанавливать снимки только для namespace'ов, к которым у него есть доступ
  - Пользователь не может видеть и восстанавливать снимки чужих namespace'ов
  - Учесть, что NamespaceSnapshot — cluster-scoped, но доступ должен быть ограничен по `spec.namespaceName`
  - Возможные подходы:
    - Admission webhook для проверки прав на целевой namespace при создании/чтении NamespaceSnapshot
    - Использование `SubjectAccessReview` для проверки прав пользователя на namespace
    - Label-based RBAC с автоматической простановкой label'ов на NamespaceSnapshot
- [ ] **План снапшота для консистентности**:
  - Фиксировать hash плана (набор целей snapshotting) при старте.
  - При несоответствии hash до `Ready=True` — fail или restart.