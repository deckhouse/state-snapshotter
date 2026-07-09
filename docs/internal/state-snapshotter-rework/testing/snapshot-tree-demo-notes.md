# Snapshot tree demo — notes (контракт, внутренности, troubleshooting-детали)

Вспомогательные заметки к [`snapshot-tree-demo-runbook.md`](snapshot-tree-demo-runbook.md).
Сюда вынесено всё, что **не нужно** во время live-показа: архитектура, контракт,
детали API, исторические blocker'ы, нарратив «что сказать». Нормативный контракт —
[`../spec/system-spec.md`](../spec/system-spec.md) §3,
[`../design/snapshot-controller.md`](../design/snapshot-controller.md).

## Картина целиком

```
DemoVirtualMachine vm-1 ─spec.virtualDiskName─► DemoVirtualDisk disk-vm   ConfigMap demo-snapshot-cm
   (disk-vm.spec.persistentVolumeClaimName = demo-pvc-disk)               PVC demo-pvc      (orphan, Bound)
DemoVirtualDisk disk-standalone                                          PVC demo-pvc-disk (nested, Bound)
   связи иерархии направлены ВНИЗ: VM ─► Disk ─► PVC (ownerRefs — только lifecycle)
   topology ≠ coverage (формула `INV-TOPO-COV`, spec §3.6.1):
     • topology source: spec links downward;
     • ownerReferences are not topology;
     • reference does not imply coverage;
     • top-level exclusion happens only after actual child snapshot coverage.
                         │  CustomSnapshotDefinition demo-live-vm-disk
                         ▼
              Snapshot demo-tree (spec: {})
                         │ controller: MCR + (VS | VCR) + child snapshots
        ┌────────────────┼───────────────────────────────────────────┐
        ▼                ▼                                             ▼
  childrenSnapshotRefs   manifest leg                              data leg
  ├─ DemoVirtualMachineSnapshot(vm-1)   MCR ─► ManifestCheckpoint ─► Chunk(s)
  │    └─ DemoVirtualDiskSnapshot(disk-vm) ── data: VCR ─► VolumeSnapshotContent  (nested demo-pvc-disk)
  ├─ DemoVirtualDiskSnapshot(disk-standalone)
  └─ VolumeSnapshot nss-vs-* (visibility leaf) ── data: VS ─► VolumeSnapshotContent (orphan demo-pvc)

каждый узел: Snapshot ─bound─► SnapshotContent (cluster) ─ownerRef─► ObjectKeeper (follow + TTL)
```

### Два пути захвата тома (демо проверяет оба)

| PVC | Где живёт | Data-путь | Где артефакт (VSC) | Манифест PVC |
|---|---|---|---|---|
| `demo-pvc` | standalone/orphan прямо в namespace | root создаёт **CSI VolumeSnapshot** как visibility-leaf (`nss-vs-*` в `Snapshot.status.childrenSnapshotRefs[]`); VCR **не** создаётся | `status.data` **root** SnapshotContent | в **root** MCP |
| `demo-pvc-disk` | вложена в `DemoVirtualDisk/disk-vm` (`spec.persistentVolumeClaimName`) | доменный диск создаёт **VolumeCaptureRequest** (как любой доменный узел), затем handoff VSC | `status.data` **disk-vm** SnapshotContent | в **disk-vm** MCP |

Инвариант: вложенная `demo-pvc-disk` является subtree-covered (через `status.data`/in-flight VCR
у disk-vm content), поэтому root **не** считает её сиротой и **не** создаёт для неё VS. Сам объект
`VolumeSnapshot` (nss-vs-*) — это только data-leg сироты, он **не** попадает в ManifestCheckpoint.

| Сущность | Роль |
|---|---|
| **Snapshot** (ns) | заявка на снимок узла; после готовности ссылается на content |
| **SnapshotContent** (cluster) | долговечный носитель узла: `manifestCheckpointName`, `status.data`, `childrenSnapshotContentRefs[]` |
| **ManifestCaptureRequest / ManifestCheckpoint / Chunk** | снапшот **манифестов** (payload в chunk, get-by-name) |
| **VolumeCaptureRequest / VolumeSnapshotContent** | снапшот **данных** тома |
| **ObjectKeeper** | держит content после удаления Snapshot; по TTL → каскадный GC |

## API-группы (сверка перед демо)

| Объект | Группа |
|---|---|
| `Snapshot`, `SnapshotContent` | `state-snapshotter.deckhouse.io` |
| `VolumeCaptureRequest`, `VolumeRestoreRequest` | `storage-foundation.deckhouse.io` |
| `ManifestCaptureRequest`, `ManifestCheckpoint`, `ManifestCheckpointContentChunk`, `CustomSnapshotDefinition` | `state-snapshotter.deckhouse.io` |
| `DemoVirtual*` | `demo.state-snapshotter.deckhouse.io` |
| `VolumeSnapshotContent` | `snapshot.storage.k8s.io` |

## Restore-контракт (volumes через VRR)

Volumes восстанавливаются **только** так (без `PVC dataSource`/`dataSourceRef`,
без временных `VolumeSnapshot`):

```
SnapshotContent.status.data  →  artifact = VolumeSnapshotContent
  →  VolumeRestoreRequest (sourceRef.kind=VolumeSnapshotContent, pvcTemplate)
  →  external-provisioner executor  →  CSI CreateVolume (snapshotHandle)
  →  PV  →  PVC (spec.volumeName)  →  storage-foundation видит PVC Bound  →  VRR Ready=True
```

- `snapshotHandle` живёт внутри VSC, пользователь с ним напрямую не работает;
- PVC из архива **не применяется** напрямую (ссылается на старый PV/namespace/status);
- PVC после restore создаёт executor, не пользовательский манифест.

English demo note:

- Pods may be present in the archived manifests, but namespace restore excludes
  them on purpose. The bind Pod is runtime consumer state, not durable
  application intent; recreate consumers after the restored PVC is Bound if
  needed.
- The restored PVC can become Bound without a Pod even when the StorageClass is
  `WaitForFirstConsumer`: VRR/executor creates the PV first and creates the PVC
  with `spec.volumeName`, so Kubernetes performs static pre-binding instead of
  dynamic provisioning.

## Что отдаёт агрегированный API (проверено по коду + замером 2026-06-01)

`AggregatedNamespaceManifests` отдаёт объекты так, как они лежат в архиве
`ManifestCheckpoint`. Capture хранит сырые манифесты как есть
(«include everything as-is»), поэтому API:

- **удаляет `metadata.namespace`** у каждого объекта;
- **отбрасывает cluster-scoped объекты** (namespace пуст);
- **НЕ** удаляет `uid`/`resourceVersion`/`creationTimestamp`/`ownerReferences`/`status`
  (и `managedFields`/`generation`, если были) — остаются «as-is»;
- PVC присутствует в архиве как обычный объект → при restore его надо исключить;
- `ownerReferences` сохраняются со **старыми UID** → применять как есть нельзя
  (k8s GC удалит объект с битым controller-ownerRef).

Поэтому `restore-namespace-from-snapshot.sh` чистит identity/runtime-поля +
`ownerReferences` + `status` и проставляет `metadata.namespace`. Это не «лишняя»
санитизация — API её не гарантирует (capture хранит сырьё, чистка — на restore
read-path).

### Агрегированные routes

Группа `subresources.state-snapshotter.deckhouse.io` отдаёт `/manifests` только
для `snapshots/manifests` (namespaced) и `manifestcheckpoints/manifests`
(cluster-scoped, без `namespaces/...`). Субресурса `demovirtualmachinesnapshots/manifests`
**нет** → `Forbidden`. VM-subtree читаем через MCP дочернего VM-снапшота.

## Известный blocker: restored PV без storageClassName

На текущем (непересобранном) образе provisioner restored PV создаётся **без**
`spec.storageClassName` (`null`), а PVC имеет `local-thin` → PVC висит `Pending`
(`VolumeMismatch: storageClassName does not match`), VRR не доходит до `Ready`.

Фикс есть в коде executor (`external-provisioner` `vrr_handler.go`,
`StorageClassName: sc.Name`), но **ещё не в образе**. Обход — пропатчить PV:

```bash
PVNAME=$(kubectl -n "$NS" get pvc "$PVC" -o jsonpath='{.spec.volumeName}')
kubectl patch pv "$PVNAME" --type merge -p '{"spec":{"storageClassName":"'"$STORAGE_CLASS"'"}}'
```

`restore-namespace-from-snapshot.sh` делает этот обход автоматически (отключается
`--no-pv-fix`). После выката образа с фиксом обход не нужен.

## CSD AccessGranted (production vs demo)

`AccessGranted=True` на CSD в production ставит внешний Deckhouse RBAC controller/hook
(в этом репозитории hook **не реализован**). Контроллер сам это условие не пишет,
только читает для eligibility (`Accepted=True && AccessGranted=True && observedGeneration совпадает`).
В демо/smoke `AccessGranted` выставляется вручную `kubectl replace --subresource=status`
(нужны права на `customsnapshotdefinitions/status`).

## Forced TTL expiry (только cluster-admin)

ObjectKeeper снапшота создаётся с `spec.ttl=1m0s`; естественной экспирации
достаточно. Принудительно ускорить (если есть `can-i patch objectkeepers = yes`):

```bash
for ok in $(kubectl get objectkeepers.deckhouse.io -o json | jq -r --arg ns "$DEMO_NS" \
              '.items[] | select(.spec.followObjectRef.namespace==$ns) | .metadata.name'); do
  kubectl patch objectkeepers.deckhouse.io "$ok" --type=merge -p '{"spec":{"ttl":"0s"}}'
done
```

Для обычного admin это `forbidden` — тогда просто дождаться естественного 1m TTL.

## Retained aggregated read после удаления Snapshot

После удаления namespaced Snapshot read по `snapshots/<name>/manifests` может
упасть (зависит от реализации route). Надёжный retained-read — по **MCP**
(cluster route, не зависит от namespaced Snapshot):

```bash
kubectl get --raw \
  "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/manifestcheckpoints/${MCP}/manifests" | jq 'length'
```

## Связанные файлы

| Файл | Назначение |
|---|---|
| [`snapshot-tree-demo/`](snapshot-tree-demo/) | Скачиваемые YAML + `restore-namespace-from-snapshot.sh` |
| `hack/snapshot-tree-demo-e2e.sh` | Staged-диагностический прогон дерева (00-preflight..11-chunk-missing) с артефактами; runbook §9 |
| [`snapshot-live-demo-runbook.md`](snapshot-live-demo-runbook.md) | Snapshot-only live demo, речь, pitfalls |
| [`snapshot-live-demo-commands.md`](snapshot-live-demo-commands.md) | Команды треков A/C/D/B |
| `hack/demo-e2e.sh` | Автоматический трек B; stage 08 = forced TTL GC |
| `hack/snapshot-graph.sh` | Граф дерева |
| `storage-foundation/hack/apply-vrr-rbac.sh` | RBAC provisioner SA для restore |
