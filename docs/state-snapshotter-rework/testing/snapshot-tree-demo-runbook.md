# Snapshot tree demo — runbook

Короткий live-runbook: дерево снапшотов (root → child), снапшот **манифестов**,
снапшот **данных**, агрегированное чтение, opt. restore тома, opt. namespace
restore, opt. каскадный GC по TTL. Детали/контракт/внутренности — в
[`snapshot-tree-demo-notes.md`](snapshot-tree-demo-notes.md).

## 0. Что показываем

- **дерево**: root `Snapshot` → дочерние `DemoVirtualMachineSnapshot`/`DemoVirtualDiskSnapshot`;
- **manifest leg**: `ManifestCaptureRequest` → `ManifestCheckpoint` → chunk;
- **data leg**: `VolumeCaptureRequest` → `VolumeSnapshotContent` (`dataRefs[]`);
- каждый узел: `Snapshot` → `SnapshotContent` → `ObjectKeeper` (follow + TTL);
- restore тома идёт через VRR: `dataRefs[] → VSC → VRR → PV/PVC`, `PVC dataSource` **не используется**.

Части 5–7 **опциональны**; часть A (разделы 1–4) самодостаточна.

---

## 1. Переменные и preflight

```bash
cd /path/to/state-snapshotter/docs/state-snapshotter-rework/testing/snapshot-tree-demo
export MANIFEST_DIR="$(pwd)"
export DEMO_NS=snapshot-demo-tree SNAP=demo-tree CSD_NAME=demo-live-vm-disk
export STORAGE_CLASS=local-thin MOD_NS=d8-state-snapshotter

# контроллер Running + нужные CRD
kubectl get pods -n "$MOD_NS" -l app=controller
kubectl get crd snapshots.storage.deckhouse.io snapshotcontents.storage.deckhouse.io \
  objectkeepers.deckhouse.io \
  demovirtualmachines.demo.state-snapshotter.deckhouse.io \
  customsnapshotdefinitions.state-snapshotter.deckhouse.io

# сверка фактических API-групп (если отличаются — поправить команды; таблица в notes)
kubectl api-resources | grep -E 'manifestcapture|manifestcheckpoint|^snapshots|snapshotcontents|customsnapshot|volumecapture|volumerestore'

# RBAC через SA модуля (не свой kubeconfig) — все должны быть yes
kubectl auth can-i get demovirtualmachines.demo.state-snapshotter.deckhouse.io \
  --as=system:serviceaccount:${MOD_NS}:webhooks -n "$DEMO_NS"      # webhook читает demo inventory
kubectl auth can-i create manifestcapturerequests.state-snapshotter.deckhouse.io \
  --as=system:serviceaccount:${MOD_NS}:controller -n "$DEMO_NS"    # controller создаёт MCR
kubectl auth can-i get manifestcheckpointcontentchunks.state-snapshotter.deckhouse.io \
  --as=system:serviceaccount:${MOD_NS}:controller                 # chunks для графа: под controller SA (admin прямого доступа не имеет)
```

Если webhook `get` demo inventory = `no` → redeploy модуля (иначе дерево не
построится, `SubtreeManifestCapturePending`).

> Restore (части 5/6) требует выкаченного VRR executor — preflight в §5.

---

## 2. Подготовить source namespace

```bash
# ns + CM + PVC + bind Pod + VM + VM-linked disk + standalone disk
# VM->Disk link is spec.virtualMachineName, not ownerReferences.
kubectl apply -f "$MANIFEST_DIR/01-source.yaml"

# gate: PVC Bound до Snapshot (local-thin = WaitForFirstConsumer; bind Pod уже в source)
kubectl -n "$DEMO_NS" get pvc demo-pvc -w     # Ctrl+C на Bound
```

Зарегистрировать CSD и сделать eligible:

```bash
kubectl apply -f "$MANIFEST_DIR/02-csd.yaml"

# дождаться Accepted
until kubectl get customsnapshotdefinition "$CSD_NAME" -o json \
  | jq -e '.status.conditions[]?|select(.type=="Accepted" and .status=="True")' >/dev/null; do
  echo "waiting CSD Accepted..."; sleep 2
done

# RBACReady=True (smoke/manual; в production ставит Deckhouse hook — см. notes)
kubectl get customsnapshotdefinition "$CSD_NAME" -o json | jq \
  --arg now "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --argjson gen "$(kubectl get customsnapshotdefinition "$CSD_NAME" -o jsonpath='{.metadata.generation}')" \
  '.status.conditions = ((.status.conditions // []) | map(select(.type != "RBACReady")) + [{
    type: "RBACReady", status: "True", reason: "LiveDemo",
    message: "manual live demo approval", lastTransitionTime: $now, observedGeneration: $gen
  }])' | kubectl replace --subresource=status -f -

kubectl get customsnapshotdefinition "$CSD_NAME" -o json | jq '[.status.conditions[]?|{type,status}]'
```

Ожидание: `Accepted`, `RBACReady`, `Ready` = True.

---

## 3. Создать Snapshot и снять дерево

```bash
kubectl apply -f "$MANIFEST_DIR/03-root-snapshot.yaml"
kubectl -n "$DEMO_NS" get snapshots.storage.deckhouse.io "$SNAP" -w   # Ctrl+C на Ready=True

# имена один раз
export BOUND=$(kubectl -n "$DEMO_NS" get snapshots.storage.deckhouse.io "$SNAP" -o jsonpath='{.status.boundSnapshotContentName}')
export MCP=$(kubectl get snapshotcontents.storage.deckhouse.io "$BOUND" -o jsonpath='{.status.manifestCheckpointName}')
export OK_NAME="ret-snap-${DEMO_NS}-${SNAP}"
echo "BOUND=$BOUND MCP=$MCP OK=$OK_NAME"
```

Дерево (root → дочерние, спуск VM → его Disk):

```bash
kubectl -n "$DEMO_NS" get snapshots.storage.deckhouse.io "$SNAP" -o json | jq '{
  ready: (.status.conditions[]|select(.type=="Ready")),
  children: .status.childrenSnapshotRefs, vcr: .status.volumeCaptureRequestName }'

export CHILD_VM=$(kubectl -n "$DEMO_NS" get snapshots.storage.deckhouse.io "$SNAP" -o json \
  | jq -r '.status.childrenSnapshotRefs[]?|select(.kind=="DemoVirtualMachineSnapshot")|.name')
export VM_CONTENT=$(kubectl -n "$DEMO_NS" get demovirtualmachinesnapshots.demo.state-snapshotter.deckhouse.io "$CHILD_VM" -o jsonpath='{.status.boundSnapshotContentName}')
export VM_MCP=$(kubectl get snapshotcontents.storage.deckhouse.io "$VM_CONTENT" -o jsonpath='{.status.manifestCheckpointName}')
echo "CHILD_VM=$CHILD_VM VM_CONTENT=$VM_CONTENT VM_MCP=$VM_MCP"
```

Ожидание: `childrenSnapshotRefs` содержит `DemoVirtualMachineSnapshot(vm-1)` и
`DemoVirtualDiskSnapshot(disk-standalone)`.

**Manifest leg** (root MCP → chunk; у каждого узла свой MCP):

```bash
kubectl get manifestcheckpoints.state-snapshotter.deckhouse.io "$MCP" -o json | jq '{
  ready: (.status.conditions[]|select(.type=="Ready")), totalObjects: .status.totalObjects, chunks: .status.chunks }'
kubectl get manifestcheckpointcontentchunks.state-snapshotter.deckhouse.io "${MCP}-0" \
  -o jsonpath='checkpoint={.spec.checkpointName} objects={.spec.objectsCount}{"\n"}'
```

**Data leg** (VCR после handoff исчезает; артефакт — VSC):

```bash
# собрать content дерева (root + children + content дочерних demo-снапшотов)
{
  kubectl get snapshotcontents.storage.deckhouse.io "$BOUND" -o json \
    | jq -r '.metadata.name, (.status.childrenSnapshotContentRefs[]?.name)'
  kubectl -n "$DEMO_NS" get demovirtualmachinesnapshots.demo.state-snapshotter.deckhouse.io,demovirtualdisksnapshots.demo.state-snapshotter.deckhouse.io \
    -o jsonpath='{range .items[*]}{.status.boundSnapshotContentName}{"\n"}{end}' 2>/dev/null
} | sort -u | grep -v '^$' > /tmp/tree-contents.txt

# первый VSC из dataRefs любого content дерева
# (while read, чтобы работало и в bash, и в zsh; done < file — чтобы VSC_NAME сохранился)
export VSC_NAME=""
while IFS= read -r c; do
  [ -n "$c" ] || continue
  v=$(kubectl get snapshotcontents.storage.deckhouse.io "$c" -o jsonpath='{.status.dataRefs[0].artifact.name}' 2>/dev/null)
  if [ -n "$v" ] && [ -z "$VSC_NAME" ]; then export VSC_NAME="$v"; echo "VSC on $c: $VSC_NAME"; break; fi
done < /tmp/tree-contents.txt
[ -n "$VSC_NAME" ] || echo "WARN: VSC не найден — data leg не готов или PVC не Bound"

kubectl get volumesnapshotcontents.snapshot.storage.k8s.io "$VSC_NAME" -o json | jq '{
  readyToUse: .status.readyToUse,
  snapshotHandle: (.status.snapshotHandle // .spec.source.snapshotHandle) }'
```

Ожидание: `readyToUse=true`, непустой `snapshotHandle`.

---

## 4. Граф и агрегированные манифесты

```bash
# граф: ~2 мин на реальном кластере — лучше сгенерировать ЗАРАНЕЕ, не «вживую»
cd /path/to/state-snapshotter
bash hack/snapshot-graph.sh --namespace "$DEMO_NS" --snapshot "$SNAP" \
  --output-dir /tmp/snapshot-tree --name tree --mode logical --title "Snapshot tree" \
  --chunk-as system:serviceaccount:d8-state-snapshotter:controller
open /tmp/snapshot-tree/tree.logical.svg
grep -c 'MISSING' /tmp/snapshot-tree/tree.logical.dot   # ожидание: 0

# агрегированное чтение (снять ДО удаления Snapshot)
kubectl get --raw \
  "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/${DEMO_NS}/snapshots/${SNAP}/manifests" \
  | jq '[.[] | {kind, name: .metadata.name}] | {count: length, sample: .[0:12]}'
```

На SVG видно `Snap → SC → MCP → Chunk` (манифесты), `SC → VSC` (данные), зелёные
дуги `childrenSnapshot*` (дерево), без `MISSING`.

---

## 5. (Опц.) Restore одного тома

Preflight executor (нужен только для частей 5/6):

```bash
export CSI_NS=d8-sds-local-volume
kubectl get crd volumerestorerequests.storage.deckhouse.io
kubectl auth can-i list volumerestorerequests.storage.deckhouse.io \
  --as="system:serviceaccount:${CSI_NS}:csi"      # yes
```
Если `no` или executor не выкачен — **пропустить** части 5/6.

```bash
# VSC_NAME из §3
envsubst < "$MANIFEST_DIR/04-volumerestorerequest.template.yaml" | kubectl apply -f -

kubectl -n "$DEMO_NS" get pvc demo-pvc-restored
kubectl -n "$DEMO_NS" get volumerestorerequests.storage.deckhouse.io restore-demo \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}{"\n"}{end}'
```

Ожидание: `demo-pvc-restored` → `Bound`, затем VRR `Ready=True`.

> На текущем образе provisioner restored PV создаётся без `storageClassName` → PVC
> висит `Pending` (`VolumeMismatch`). Обход (детали в notes):
> PVNAME=$(kubectl -n "$DEMO_NS" get pvc demo-pvc-restored -o jsonpath='{.spec.volumeName}');
> kubectl patch pv "$PVNAME" --type merge -p '{"spec":{"storageClassName":"'"$STORAGE_CLASS"'"}}'

---

## 6. (Опц.) Restore целого namespace

Один helper делает весь happy-path (manifests + volumes через VRR):

```bash
cd "$MANIFEST_DIR"
bash restore-namespace-from-snapshot.sh \
  --source-namespace "$DEMO_NS" --snapshot "$SNAP" \
  --target-namespace snapshot-demo-restored --storage-class "$STORAGE_CLASS" --timeout 180
```

Скрипт: создаёт target ns → apply non-PVC (PVC/Pod/`default` SA/`kube-root-ca.crt`
исключает, `ownerReferences`/runtime-поля чистит) → VRR на каждый snapshot'нутый
PVC → ждёт `Bound`+`Ready` → печатает `RESULT: SUCCESS`. PV-обход делает сам
(отключается `--no-pv-fix`). Контракт/детали — в notes.

> Note: Pods may be present in the archive, but the helper does not restore them.
> The restored PVC is statically pre-bound by VRR (`spec.volumeName`), so it can
> become `Bound` without a consumer Pod even with a `WaitForFirstConsumer`
> StorageClass.

```bash
export RESTORE_NS=snapshot-demo-restored
kubectl -n "$RESTORE_NS" get cm,demovirtualmachines.demo.state-snapshotter.deckhouse.io,demovirtualdisks.demo.state-snapshotter.deckhouse.io,pvc
kubectl -n "$RESTORE_NS" get volumerestorerequests.storage.deckhouse.io
```

Ожидание: demo-объекты в `$RESTORE_NS`; restored PVC `Bound`; VRR `Ready=True`.

---

## 7. (Опц.) Каскадный GC по TTL (controlled/debug only)

> Только на controlled-кластере. Удаляем **root Snapshot** — дерево остаётся
> (держит ObjectKeeper) и удаляется само по `1m` TTL.

```bash
# сохранить список дерева ДО удаления
kubectl get snapshotcontents.storage.deckhouse.io "$BOUND" -o json \
  | jq -r '.metadata.name, (.status.childrenSnapshotContentRefs[]?.name)' | tee /tmp/${SNAP}-tree.txt

# удалить root Snapshot — content retained
kubectl -n "$DEMO_NS" delete snapshots.storage.deckhouse.io "$SNAP" --wait=true
kubectl get snapshotcontents.storage.deckhouse.io "$BOUND" -o wide   # ещё жив

# подождать ~1.5 мин (естественный TTL ObjectKeeper) и наблюдать каскад
# watch -n3 '
#  echo "== SnapshotContents (ns-*) =="; kubectl get snapshotcontents.storage.deckhouse.io | grep -E "ns-" || echo none
#  echo "== ManifestCheckpoints ==";     kubectl get manifestcheckpoints.state-snapshotter.deckhouse.io 2>/dev/null | tail -n +2 || echo none
#'   # Ctrl+C когда пусто - не работает на маке

while true; do
  clear
  echo "== SnapshotContents (ns-*) =="
  kubectl get snapshotcontents.storage.deckhouse.io | grep -E "ns-" || echo none

  echo
  echo "== ManifestCheckpoints =="
  kubectl get manifestcheckpoints.state-snapshotter.deckhouse.io 2>/dev/null | tail -n +2 || echo none

  sleep 3
done

kubectl get snapshotcontents.storage.deckhouse.io "$BOUND" 2>&1 || echo "root content удалён"
kubectl get manifestcheckpoints.state-snapshotter.deckhouse.io "$MCP" 2>&1 || echo "root MCP удалён"
```

Критерий: все `SnapshotContent`/`ManifestCheckpoint`/chunks/`ObjectKeeper` дерева
удалены. Принудительный `ttl=0s` (только cluster-admin) — см. notes.

Очистка демо:

```bash
#kubectl delete customsnapshotdefinition "$CSD_NAME" --ignore-not-found
kubectl delete namespace "$DEMO_NS" snapshot-demo-restored --ignore-not-found --wait=true
```

---

## 8. Траблшутинг

| Симптом | Причина | Что делать |
|---|---|---|
| root `Ready=False` + `SubtreeManifestCapturePending`, MCR нет | webhook SA не читает demo inventory | redeploy RBAC модуля; см. §1 |
| `childrenSnapshotRefs` пусто | CSD не Ready / не RBACReady | проверить §2; логи controller |
| `dataRefs` пусто, VCR `PVC … is not bound` | PVC не Bound (WFFC без Pod) | дождаться Bound до Snapshot |
| restore PVC `Pending`, `VolumeMismatch` | PV без `storageClassName` (старый образ) | обход `patch pv` (§5/notes); пересобрать executor |
| `patch objectkeepers` forbidden | нет прав | дождаться естественного `1m` TTL |

```bash
kubectl logs -n d8-state-snapshotter -l app=controller --tail=300
kubectl logs -n d8-storage-foundation -l app=controller --tail=200   # restore lifecycle
```

Подробности, контракт и внутренности — [`snapshot-tree-demo-notes.md`](snapshot-tree-demo-notes.md).
