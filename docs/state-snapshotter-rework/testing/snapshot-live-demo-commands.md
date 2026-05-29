# Snapshot live demo — команды для самостоятельного прогона

Копируйте блоки по порядку. Подробности и «что говорить» — в [`snapshot-live-demo-runbook.md`](snapshot-live-demo-runbook.md).

**Не показываем:** restore, VolumeSnapshot restore intent.

**Правила:**

- **Live demo = трек C** (`snapshot-demo-volume`). Не смешивать с D в одном namespace.
- Трек **A** (manifest) и **C** (volume) — **разные** namespace.
- Трек A: **без PVC** в namespace.
- Трек C: Snapshot **только после** Bound PVC + consumer Pod.
- **Трек D** — optional/full demo; **сначала D0 без PVC** на чистом ns, потом (опционально) D1 с volume.
- Retain — только на **успешном** snapshot.

**Какой сценарий когда:**

| Трек | Назначение |
|------|------------|
| **C** | **Основной live demo** — MCP/chunks + `dataRefs` → VSC |
| **D0** | Диагностика CSD + demo children + MCR/MCP **без** volume leg |
| **D1** | Полный D (+ PVC) — только после успешного D0 и redeploy controller RBAC |
| **B** | Child + 2 PVC (`hack/demo-e2e.sh`) |

---

## 0. Проверка кластера (один раз)

```bash
kubectl config current-context
kubectl get pods -n d8-state-snapshotter -l app=controller
kubectl auth can-i get snapshots/manifests.subresources.state-snapshotter.deckhouse.io -A
kubectl auth can-i get manifestcheckpointcontentchunks.state-snapshotter.deckhouse.io
```

Ожидание: controller `Running`, оба `can-i` → `yes`.

---

## Трек C — volume + manifest (базовый, без demo domain)

### Переменные

```bash
export DEMO_NS=snapshot-demo-volume
export SNAP=demo-volume
export STORAGE_CLASS=local-thin
export PVC=demo-pvc
export BIND_IMAGE=registry.k8s.io/pause:3.9
```

### 1. Чистый namespace (опционально, с нуля)

```bash
kubectl delete namespace "$DEMO_NS" --ignore-not-found --wait=true --timeout=120s
kubectl create namespace "$DEMO_NS"
```

### 2. Preflight

```bash
kubectl get storageclass "$STORAGE_CLASS"
export VSC_NAME=$(kubectl get storageclass "$STORAGE_CLASS" \
  -o jsonpath='{.metadata.annotations.storage\.deckhouse\.io/volumesnapshotclass}')
echo "VolumeSnapshotClass=$VSC_NAME"
kubectl get volumesnapshotclass "$VSC_NAME"

kubectl -n "$DEMO_NS" get volumecapturerequests.storage.deckhouse.io
# ожидание: пусто или только заголовок таблицы
```

### 3. Workload (CM + PVC + bind Pod)

```bash
kubectl -n "$DEMO_NS" create configmap demo-snapshot-cm \
  --from-literal=demo=volume --dry-run=client -o yaml | kubectl apply -f -

kubectl -n "$DEMO_NS" apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ${PVC}
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: ${STORAGE_CLASS}
  resources:
    requests:
      storage: 1Gi
EOF

kubectl -n "$DEMO_NS" apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: bind-${PVC}
spec:
  restartPolicy: Never
  containers:
    - name: hold
      image: ${BIND_IMAGE}
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: ${PVC}
EOF
```

### 4. Gate: PVC Bound (не переходить дальше, пока не Bound)

```bash
kubectl -n "$DEMO_NS" get pvc "$PVC" -w
# Ctrl+C когда PHASE=Bound

kubectl -n "$DEMO_NS" get pvc "$PVC" -o jsonpath='{.status.phase}{"\n"}'
# ожидание: Bound
```

### 5. Создать Snapshot

```bash
kubectl -n "$DEMO_NS" apply -f - <<EOF
apiVersion: storage.deckhouse.io/v1alpha1
kind: Snapshot
metadata:
  name: ${SNAP}
spec: {}
EOF

kubectl -n "$DEMO_NS" get snapshots.storage.deckhouse.io "$SNAP" -w
# Ctrl+C когда Ready=True
```

### 6. Happy path — чеклист

```bash
export BOUND=$(kubectl -n "$DEMO_NS" get snapshots.storage.deckhouse.io "$SNAP" \
  -o jsonpath='{.status.boundSnapshotContentName}')
export MCP=$(kubectl get snapshotcontents.storage.deckhouse.io "$BOUND" \
  -o jsonpath='{.status.manifestCheckpointName}')
export OK_NAME="ret-snap-${DEMO_NS}-${SNAP}"

echo "BOUND=$BOUND MCP=$MCP OK=$OK_NAME"

# Snapshot
kubectl -n "$DEMO_NS" get snapshots.storage.deckhouse.io "$SNAP" -o json | jq '{
  ready: (.status.conditions[]|select(.type=="Ready")),
  vcr: .status.volumeCaptureRequestName,
  bound: .status.boundSnapshotContentName
}'

# SnapshotContent
kubectl get snapshotcontents.storage.deckhouse.io "$BOUND" -o json | jq '{
  ready: (.status.conditions[]|select(.type=="Ready")),
  mcp: .status.manifestCheckpointName,
  dataRefs: .status.dataRefs
}'

# MCP
kubectl get manifestcheckpoints.state-snapshotter.deckhouse.io "$MCP" -o json | jq \
  '.status.conditions[]|select(.type=="Ready")'

# VCR — должно быть пусто
kubectl -n "$DEMO_NS" get volumecapturerequests.storage.deckhouse.io

# VolumeSnapshotContent
export VSC=$(kubectl get snapshotcontents.storage.deckhouse.io "$BOUND" \
  -o jsonpath='{.status.dataRefs[0].artifact.name}')
kubectl get volumesnapshotcontents.snapshot.storage.k8s.io "$VSC" -o jsonpath='readyToUse={.status.readyToUse}{"\n"}'

# ObjectKeeper
kubectl get objectkeepers.deckhouse.io "$OK_NAME" -o json | jq '{mode: .spec.mode, ttl: .spec.ttl, follow: .spec.followObjectRef}'

# Aggregated manifests
kubectl get --raw \
  "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/${DEMO_NS}/snapshots/${SNAP}/manifests" \
  | jq '[.[] | {kind, name: .metadata.name}] | {count: length, sample: .[0:5]}'
```

Ожидание: `Ready=True`, `vcr` пусто, `dataRefs` с `VolumeSnapshotContent`, `readyToUse=true`, aggregated `count >= 1`.

### 6b. Manifest leg: chunks созданы (обязательно для показа)

Payload манифестов лежит в **chunk**, не в MCP. Показать три уровня: MCP → chunk ref → get chunk.

```bash
# MCP: список chunk-имён и счётчик объектов
kubectl get manifestcheckpoints.state-snapshotter.deckhouse.io "$MCP" -o json | jq '{
  ready: (.status.conditions[]|select(.type=="Ready")),
  totalObjects: .status.totalObjects,
  chunks: .status.chunks
}'

# Ожидание: chunks не пустой, например ["mcp-....-0"]
export CHUNK="${MCP}-0"
kubectl get manifestcheckpointcontentchunks.state-snapshotter.deckhouse.io "$CHUNK" -o json | jq '{
  checkpointName: .spec.checkpointName,
  objectsCount: .spec.objectsCount,
  ownerRefs: .metadata.ownerReferences
}'

# RBAC: get chunk (для графа и ручной проверки)
kubectl auth can-i get manifestcheckpointcontentchunks.state-snapshotter.deckhouse.io

# Архив через MCP subresource (без прямого list chunk)
kubectl get --raw \
  "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/manifestcheckpoints/${MCP}/manifests" \
  | jq '{count: length, kinds: ([.[].kind] | unique)}'
```

На графе должны быть рёбра `status.chunks` (orange) от SnapshotContent к Chunk, без `[MISSING]` на chunk.

### 7. Граф (из корня репозитория state-snapshotter)

```bash
cd /path/to/state-snapshotter

bash hack/snapshot-graph.sh \
  --namespace "$DEMO_NS" \
  --snapshot "$SNAP" \
  --output-dir /tmp/snapshot-graph \
  --name live \
  --mode logical \
  --title "Volume demo"

open /tmp/snapshot-graph/live.logical.svg

grep -E 'status\.dataRefs|VolumeSnapshotContent|status\.chunks' /tmp/snapshot-graph/live.logical.dot
```

**Если уже создали Snapshot (трек C) и нужно только показать chunks** — выполните блок **6b** с вашими `BOUND`/`MCP`/`SNAP`, затем шаг **7**.

### 8. Retain (опционально, в конце показа)

```bash
kubectl -n "$DEMO_NS" delete snapshots.storage.deckhouse.io "$SNAP" --wait=true

# сразу после delete — content и read ещё живы
kubectl get snapshotcontents.storage.deckhouse.io "$BOUND"
kubectl get objectkeepers.deckhouse.io "$OK_NAME"
kubectl get --raw \
  "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/${DEMO_NS}/snapshots/${SNAP}/manifests" \
  | jq 'length'
```

Через TTL ObjectKeeper (часто 1m в debug) content/MCP исчезнут сами — на сцене не ждать долго.

### 9. Очистка namespace (после прогона)

```bash
kubectl delete namespace "$DEMO_NS" --wait=true
```

---

## Трек D — optional full demo (CSD + Demo VM/Disk)

Отдельный namespace (`snapshot-demo-full`). **Не** смешивать с треком C.

**Порядок отладки:**

1. **D0** — без PVC (изолирует child/MCR/CSD от volume leg).
2. **Redeploy** модуля: `templates/webhooks/rbac-for-us.yaml` (demo inventory `get/list/watch`) + `templates/controller/rbac-for-us.yaml` (demo snapshots/MCR).
3. Если D0 OK → **D1** с PVC (отдельный прогон); если D1 ломается — баг/гонка root volume + subtree pending.

### Переменные

```bash
export DEMO_NS=snapshot-demo-full
export SNAP=demo-full
export CSD_NAME=demo-live-vm-disk
export CTRL_NS=d8-state-snapshotter
export CTRL_SA=controller
```

### 0. RBAC preflight (admin + **controller SA** + webhook SA)

Проверять **только** с `--as=system:serviceaccount:…` — `can-i` от своего kubeconfig не отражает права контроллера.

**Admin** (ручной `kubectl apply` inventory/CSD):

```bash
kubectl auth can-i create demovirtualmachines.demo.state-snapshotter.deckhouse.io -n "$DEMO_NS"
kubectl auth can-i create demovirtualdisks.demo.state-snapshotter.deckhouse.io -n "$DEMO_NS"
kubectl auth can-i create customsnapshotdefinitions.state-snapshotter.deckhouse.io
```

**Controller SA** — матрица для Track D (`templates/controller/rbac-for-us.yaml`; на dev-кластере demo-права могут идти ещё из `state-snapshotter-smoke-demo-domain-rbac`):

```bash
export WH_SA=webhooks
AS_CTRL="--as=system:serviceaccount:${CTRL_NS}:${CTRL_SA}"
AS_WH="--as=system:serviceaccount:${CTRL_NS}:${WH_SA}"

# Ключевые gate (достаточно для быстрого preflight)
kubectl auth can-i create manifestcapturerequests.state-snapshotter.deckhouse.io $AS_CTRL -n "$DEMO_NS"
kubectl auth can-i update demovirtualmachinesnapshots.demo.state-snapshotter.deckhouse.io/status $AS_CTRL -n "$DEMO_NS"
kubectl auth can-i update demovirtualdisksnapshots.demo.state-snapshotter.deckhouse.io/status $AS_CTRL -n "$DEMO_NS"
kubectl auth can-i create manifestcheckpoints.state-snapshotter.deckhouse.io $AS_CTRL
kubectl auth can-i create objectkeepers.deckhouse.io $AS_CTRL

# Webhook (blocker MCR admission): get/list/watch на demo inventory
kubectl auth can-i get demovirtualdisks.demo.state-snapshotter.deckhouse.io $AS_WH -n "$DEMO_NS"
kubectl auth can-i get demovirtualmachines.demo.state-snapshotter.deckhouse.io $AS_WH -n "$DEMO_NS"
kubectl auth can-i list demovirtualdisks.demo.state-snapshotter.deckhouse.io $AS_WH -n "$DEMO_NS"
kubectl auth can-i watch demovirtualmachines.demo.state-snapshotter.deckhouse.io $AS_WH -n "$DEMO_NS"
```

Полная матрица (опционально):

```bash
can_ctrl() { kubectl auth can-i "$1" "$2" $AS_CTRL ${3:+-n "$3"} 2>/dev/null | awk -v v="$1" -v r="$2" '{print v, r, $1}'; }
for v in get list watch; do
  can_ctrl $v demovirtualmachines.demo.state-snapshotter.deckhouse.io "$DEMO_NS"
  can_ctrl $v demovirtualdisks.demo.state-snapshotter.deckhouse.io "$DEMO_NS"
done
for v in get list watch create update patch delete; do
  can_ctrl $v demovirtualmachinesnapshots.demo.state-snapshotter.deckhouse.io "$DEMO_NS"
  can_ctrl $v demovirtualdisksnapshots.demo.state-snapshotter.deckhouse.io "$DEMO_NS"
done
can_ctrl update demovirtualmachinesnapshots.demo.state-snapshotter.deckhouse.io/finalizers "$DEMO_NS"
can_ctrl update demovirtualdisksnapshots.demo.state-snapshotter.deckhouse.io/finalizers "$DEMO_NS"
for v in get list watch; do can_ctrl $v customsnapshotdefinitions.state-snapshotter.deckhouse.io; done
can_ctrl update customsnapshotdefinitions.state-snapshotter.deckhouse.io/status
for v in get list watch create update patch delete; do
  can_ctrl $v manifestcapturerequests.state-snapshotter.deckhouse.io "$DEMO_NS"
  can_ctrl $v manifestcheckpoints.state-snapshotter.deckhouse.io
done
for v in get list watch create update patch; do can_ctrl $v objectkeepers.deckhouse.io; done
for v in get list watch create update patch delete; do
  can_ctrl $v snapshots.storage.deckhouse.io "$DEMO_NS"
  can_ctrl $v snapshotcontents.storage.deckhouse.io
done
```

| Ресурс | Ожидание для D | Примечание |
|--------|----------------|------------|
| demo inventory + snapshots + `/status` + `/finalizers` | все **yes** | child/MCR reconcile |
| `manifestcapturerequests` (+ `/status`) | **yes** | create MCR |
| `manifestcheckpoints`, chunks | **yes** | MCP |
| `snapshots`, `snapshotcontents` (+ `/status`) | **yes** | unified + demo content |
| `objectkeepers` create/patch | **yes**; delete **no** | по шаблону — OK |
| `customsnapshotdefinitions/status` update | желательно **yes** | registry; для live D часто хватает ручного `RBACReady` |
| **webhook** `get/list/watch` `demovirtualmachines`, `demovirtualdisks` | **yes** | иначе MCR **denied** (`not found in namespace`) |

Проверить, что в chart задеплоен `d8:state-snapshotter:controller` с demo rules (не только smoke CR):

```bash
kubectl get clusterrole d8:state-snapshotter:controller -o yaml | grep -c demovirtual
kubectl get clusterrolebinding -o json | jq -r --arg ns "$CTRL_NS" --arg sa "$CTRL_SA" \
  '.items[] | select(.subjects[]? | .namespace==$ns and .name==$sa) | .metadata.name'
```

Расширить ClusterRole вручную **нельзя** без escalate у kubeconfig — только redeploy модуля.

### 1. Чистый namespace

```bash
kubectl delete namespace "$DEMO_NS" --ignore-not-found --wait=true --timeout=120s
kubectl create namespace "$DEMO_NS"
kubectl get pods -n "$CTRL_NS" -l app=controller | grep Running
```

### 2. Demo inventory (источники для CSD graph)

```bash
kubectl -n "$DEMO_NS" create configmap demo-snapshot-cm \
  --from-literal=demo=d0 --dry-run=client -o yaml | kubectl apply -f -

kubectl -n "$DEMO_NS" apply -f - <<'EOF'
apiVersion: demo.state-snapshotter.deckhouse.io/v1alpha1
kind: DemoVirtualMachine
metadata:
  name: vm-1
spec: {}
EOF

VM_UID=$(kubectl -n "$DEMO_NS" get demovirtualmachine vm-1 -o jsonpath='{.metadata.uid}')

kubectl -n "$DEMO_NS" apply -f - <<EOF
apiVersion: demo.state-snapshotter.deckhouse.io/v1alpha1
kind: DemoVirtualDisk
metadata:
  name: disk-vm
  ownerReferences:
  - apiVersion: demo.state-snapshotter.deckhouse.io/v1alpha1
    kind: DemoVirtualMachine
    name: vm-1
    uid: ${VM_UID}
spec: {}
---
apiVersion: demo.state-snapshotter.deckhouse.io/v1alpha1
kind: DemoVirtualDisk
metadata:
  name: disk-standalone
spec: {}
EOF

kubectl -n "$DEMO_NS" get demovirtualmachine,demovirtualdisk
```

### 3. D0 gate: без PVC

```bash
kubectl -n "$DEMO_NS" get pvc
# ожидание: No resources found — иначе volume leg смешает диагностику
```

### 4. CustomSnapshotDefinition (VM + Disk)

```bash
kubectl apply -f - <<EOF
apiVersion: state-snapshotter.deckhouse.io/v1alpha1
kind: CustomSnapshotDefinition
metadata:
  name: ${CSD_NAME}
spec:
  ownerModule: live-demo
  snapshotResourceMapping:
  - resourceCRDName: demovirtualmachines.demo.state-snapshotter.deckhouse.io
    snapshotCRDName: demovirtualmachinesnapshots.demo.state-snapshotter.deckhouse.io
  - resourceCRDName: demovirtualdisks.demo.state-snapshotter.deckhouse.io
    snapshotCRDName: demovirtualdisksnapshots.demo.state-snapshotter.deckhouse.io
EOF

until kubectl get customsnapshotdefinition "${CSD_NAME}" -o json \
  | jq -e '.status.conditions[]? | select(.type=="Accepted" and .status=="True")' >/dev/null; do
  echo "waiting CSD Accepted..."
  sleep 2
done
kubectl get customsnapshotdefinition "${CSD_NAME}" -o yaml | grep -A2 'type: Accepted'
```

Пометить `RBACReady=True` (smoke/manual; в production — Deckhouse hook):

```bash
kubectl get customsnapshotdefinition "${CSD_NAME}" -o json | jq \
  --arg now "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --argjson gen "$(kubectl get customsnapshotdefinition "${CSD_NAME}" -o jsonpath='{.metadata.generation}')" \
  '.status.conditions = ((.status.conditions // []) | map(select(.type != "RBACReady")) + [{
    type: "RBACReady", status: "True", reason: "LiveDemo",
    message: "manual live demo approval", lastTransitionTime: $now, observedGeneration: $gen
  }])' \
  | kubectl replace --subresource=status -f -
```

### 5. Root Snapshot (после CSD eligible, D0 без PVC)

```bash
kubectl -n "$DEMO_NS" apply -f - <<EOF
apiVersion: storage.deckhouse.io/v1alpha1
kind: Snapshot
metadata:
  name: ${SNAP}
spec: {}
EOF

kubectl -n "$DEMO_NS" get snapshots.storage.deckhouse.io "$SNAP" -w
```

### 6. D0 чеклист (один проход, без долгого wait)

```bash
kubectl -n "$DEMO_NS" get manifestcapturerequests.state-snapshotter.deckhouse.io
kubectl -n "$DEMO_NS" get snap "$SNAP" -o jsonpath='Ready={.status.conditions[?(@.type=="Ready")].status} reason={.status.conditions[?(@.type=="Ready")].reason}{"\n"}'
kubectl -n "$DEMO_NS" get demovirtualmachine,demovirtualdisk
```

Если `mcr` пусто или `Ready=False` + `SubtreeManifestCapturePending` — сразу логи (часто **webhook**, не volume):

```bash
kubectl logs -n "$CTRL_NS" -l app=controller --tail=120 \
  | grep -iE "mcr-validation|denied the request|DemoVirtual|not found in namespace" \
  | grep -v 'nss child relay' | tail -20
```

Типичная ошибка до redeploy webhook RBAC: `Target 0: resource …/DemoVirtualDisk not found in namespace` при существующем `disk-standalone` — webhook SA не может `get` demo inventory.

### 7. D0/D1 чеклист (demo tree + chunks)

```bash
export BOUND=$(kubectl -n "$DEMO_NS" get snapshots.storage.deckhouse.io "$SNAP" \
  -o jsonpath='{.status.boundSnapshotContentName}')
export MCP=$(kubectl get snapshotcontents.storage.deckhouse.io "$BOUND" \
  -o jsonpath='{.status.manifestCheckpointName}')

# Root: demo children (имена из status, не угадывать)
kubectl -n "$DEMO_NS" get snapshots.storage.deckhouse.io "$SNAP" -o json | jq '{
  ready: (.status.conditions[]|select(.type=="Ready")),
  children: .status.childrenSnapshotRefs,
  vcr: .status.volumeCaptureRequestName
}'

CHILD_VM=$(kubectl -n "$DEMO_NS" get snapshots.storage.deckhouse.io "$SNAP" -o json \
  | jq -r '.status.childrenSnapshotRefs[]? | select(.kind=="DemoVirtualMachineSnapshot") | .name')
echo "CHILD_VM=$CHILD_VM"
kubectl -n "$DEMO_NS" get demovirtualmachinesnapshots.demo.state-snapshotter.deckhouse.io "$CHILD_VM" -o json | jq \
  '{ready: (.status.conditions[]|select(.type=="Ready")), sourceRef: .spec.sourceRef, children: .status.childrenSnapshotRefs}'

CHILD_DISK=$(kubectl -n "$DEMO_NS" get demovirtualmachinesnapshots.demo.state-snapshotter.deckhouse.io "$CHILD_VM" -o json \
  | jq -r '.status.childrenSnapshotRefs[]? | select(.kind=="DemoVirtualDiskSnapshot") | .name')
echo "CHILD_DISK=$CHILD_DISK"

# Standalone disk может быть direct child root (без ownerRef на VM)
kubectl -n "$DEMO_NS" get snapshots.storage.deckhouse.io "$SNAP" -o json \
  | jq -r '.status.childrenSnapshotRefs[]? | select(.kind=="DemoVirtualDiskSnapshot") | .name'

# Chunks: root MCP
kubectl get manifestcheckpoints.state-snapshotter.deckhouse.io "$MCP" -o jsonpath='chunks={.status.chunks}{" total="}{.status.totalObjects}{"\n"}'
kubectl get manifestcheckpointcontentchunks.state-snapshotter.deckhouse.io "${MCP}-0" \
  -o jsonpath='objectsCount={.spec.objectsCount}{"\n"}'

# Chunks: child VM MCP (если есть bound content)
VM_CONTENT=$(kubectl -n "$DEMO_NS" get demovirtualmachinesnapshots.demo.state-snapshotter.deckhouse.io "$CHILD_VM" \
  -o jsonpath='{.status.boundSnapshotContentName}')
VM_MCP=$(kubectl get snapshotcontents.storage.deckhouse.io "$VM_CONTENT" -o jsonpath='{.status.manifestCheckpointName}')
echo "VM_MCP=$VM_MCP"
kubectl get manifestcheckpointcontentchunks.state-snapshotter.deckhouse.io "${VM_MCP}-0" \
  -o jsonpath='vmChunkObjects={.spec.objectsCount}{"\n"}' 2>/dev/null || echo "no VM chunk yet"

# D0: volume leg на root должен быть пуст (нет PVC в ns)
kubectl get snapshotcontents.storage.deckhouse.io "$BOUND" -o json | jq '.status.dataRefs'

# Aggregated root subtree (CM + demo; без PVC на D0)
kubectl get --raw \
  "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/${DEMO_NS}/snapshots/${SNAP}/manifests" \
  | jq '[.[] | {kind, name: .metadata.name}] | {count: length, sample: .[0:12]}'

# Aggregated только VM child subtree
kubectl get --raw \
  "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/${DEMO_NS}/demovirtualmachinesnapshots/${CHILD_VM}/manifests" \
  | jq 'length'
```

Ожидание **D0** (без PVC; короткая проверка после redeploy):

```bash
kubectl get customsnapshotdefinition "$CSD_NAME" -o json | jq '[.status.conditions[]?|{type,status}]'
kubectl -n "$DEMO_NS" get mcr
CHILD_SC=$(kubectl -n "$DEMO_NS" get demovirtualdisksnapshot -o jsonpath='{.items[0].status.boundSnapshotContentName}')
kubectl get snapshotcontents.storage.deckhouse.io "$CHILD_SC" -o jsonpath='mcp={.status.manifestCheckpointName} ready={.status.conditions[?(@.type=="Ready")].status}{"\n"}'
```

- CSD Accepted + RBACReady + Ready; VM/Disk на месте.
- **MCR** в ns (хотя бы на время capture).
- Child **SnapshotContent** → `manifestCheckpointName` set; MCP **Ready**.
- Root `Ready=True`; `dataRefs` пусто (нет PVC).

**D1** (отдельный прогон с PVC): см. §8; ожидается `dataRefs[]` → VSC. Если D0 OK, а D1 снова `SubtreeManifestCapturePending` — отдельный баг volume + subtree.

### 8. D1 — volume leg (только после успешного D0)

Новый чистый namespace или удалите Snapshot и PVC, затем:

```bash
export STORAGE_CLASS=local-thin
export PVC=demo-pvc
export BIND_IMAGE=registry.k8s.io/pause:3.9

# ... preflight SC/VSC как в треке C, затем PVC + bind pod ...
# Snapshot создавать только после PVC Bound
```

### 9. Граф (D0 или D1)

```bash
cd ~/GolandProjects/state-snapshotter

bash hack/snapshot-graph.sh \
  --namespace "$DEMO_NS" \
  --snapshot "$SNAP" \
  --output-dir /tmp/snapshot-graph-full \
  --name demo-full \
  --mode logical \
  --title "Full demo: CSD + VM/Disk + volume"

open /tmp/snapshot-graph-full/demo-full.logical.svg

grep -E 'status\.chunks|DemoVirtual|status\.dataRefs|childrenSnapshot' \
  /tmp/snapshot-graph-full/demo-full.logical.dot | head -20
```

### 10. Очистка (после прогона)

```bash
kubectl delete customsnapshotdefinition "${CSD_NAME}" --ignore-not-found
kubectl delete namespace "$DEMO_NS" --wait=true
```

---

## Продолжение трека C: только chunks (уже есть `demo-volume`)

Если Snapshot `demo-volume` уже **Ready**, не пересоздавайте — покажите manifest leg:

```bash
export DEMO_NS=snapshot-demo-volume
export SNAP=demo-volume
export BOUND=$(kubectl -n "$DEMO_NS" get snapshots.storage.deckhouse.io "$SNAP" \
  -o jsonpath='{.status.boundSnapshotContentName}')
export MCP=$(kubectl get snapshotcontents.storage.deckhouse.io "$BOUND" \
  -o jsonpath='{.status.manifestCheckpointName}')

kubectl get manifestcheckpoints.state-snapshotter.deckhouse.io "$MCP" -o json | jq \
  '{chunks: .status.chunks, totalObjects: .status.totalObjects, ready: (.status.conditions[]|select(.type=="Ready"))}'

kubectl get manifestcheckpointcontentchunks.state-snapshotter.deckhouse.io "${MCP}-0" -o yaml

kubectl get --raw \
  "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/manifestcheckpoints/${MCP}/manifests" \
  | jq 'length'
```

Для **demo domain** в том же прогоне нужен **новый** namespace и трек **D** (CSD до Snapshot). Существующий `demo-volume` demo-children не получит.

---

## Быстрый сценарий: трек A (только manifest, без PVC)

```bash
export DEMO_NS=snapshot-demo-manifest
export SNAP=demo-manifest

kubectl delete namespace "$DEMO_NS" --ignore-not-found --wait=true --timeout=60s
kubectl create namespace "$DEMO_NS"

kubectl -n "$DEMO_NS" create configmap demo-snapshot-cm \
  --from-literal=demo=live --dry-run=client -o yaml | kubectl apply -f -

# gate: PVC не должно быть
kubectl -n "$DEMO_NS" get pvc
# ожидание: No resources found

kubectl -n "$DEMO_NS" apply -f - <<EOF
apiVersion: storage.deckhouse.io/v1alpha1
kind: Snapshot
metadata:
  name: ${SNAP}
spec: {}
EOF

kubectl -n "$DEMO_NS" get snapshots.storage.deckhouse.io "$SNAP" -w

export BOUND=$(kubectl -n "$DEMO_NS" get snapshots.storage.deckhouse.io "$SNAP" \
  -o jsonpath='{.status.boundSnapshotContentName}')

kubectl -n "$DEMO_NS" get snapshots.storage.deckhouse.io "$SNAP" -o json | jq \
  '{ready: (.status.conditions[]|select(.type=="Ready")), vcr: .status.volumeCaptureRequestName}'

kubectl get snapshotcontents.storage.deckhouse.io "$BOUND" -o json | jq \
  '{ready: (.status.conditions[]|select(.type=="Ready")), dataRefs: .status.dataRefs}'

kubectl get --raw \
  "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/${DEMO_NS}/snapshots/${SNAP}/manifests" \
  | jq 'length'
```

Ожидание: `Ready=True`, `vcr` пусто, `dataRefs` null, aggregated `length >= 1`.

---

## Полный N5: трек B (готовый e2e namespace)

Не создавать вручную на live — использовать подготовленный прогон:

```bash
# подготовка (долго, ~15+ мин)
cd /path/to/state-snapshotter
DEMO_E2E_SKIP_CLEANUP=1 bash hack/demo-e2e.sh

# или уже есть namespace:
export E2E_NS=demo-e2e-20260525-163832

kubectl -n "$E2E_NS" get snap,pvc
kubectl -n "$E2E_NS" get snapshots.storage.deckhouse.io demo-root -o json | jq '.status.childrenSnapshotRefs'

export BOUND=$(kubectl -n "$E2E_NS" get snapshots.storage.deckhouse.io demo-root \
  -o jsonpath='{.status.boundSnapshotContentName}')
kubectl get snapshotcontents.storage.deckhouse.io "$BOUND" -o json | jq \
  '{children: .status.childrenSnapshotContentRefs, dataRefs: .status.dataRefs}'

# готовый SVG
ls artifacts/*/06-root-ready/graph/*.svg
```

---

## Полезные one-liner'ы

```bash
# логи контроллера
kubectl logs -n d8-state-snapshotter -l app=controller --tail=200

# все demo namespace
kubectl get ns | grep -E 'snapshot-demo|demo-e2e'

# красные VCR в ns
kubectl -n "$DEMO_NS" get volumecapturerequests.storage.deckhouse.io -o wide

# Pending PVC
kubectl -n "$DEMO_NS" get pvc
```

---

## Не делать во время demo

- PVC без bind Pod на `WaitForFirstConsumer`
- Snapshot при PVC `Pending`
- Manifest-only и volume в одном namespace
- Retain на failed snapshot (orphan VCR)
- Случайный старый namespace без `kubectl get snap,vcr,pvc`

---

## Связанные файлы

| Файл | Назначение |
|------|------------|
| [`snapshot-live-demo-runbook.md`](snapshot-live-demo-runbook.md) | Полный runbook, речь, pitfalls |
| [`pre-e2e-smoke-validation.md`](pre-e2e-smoke-validation.md) | Детальный CSD/graph smoke (эталон трека D) |
| [`snapshot-manual-demo.md`](snapshot-manual-demo.md) | Минимум в `default` |
| `hack/demo-e2e.sh` | Автоматический трек B |
| `hack/snapshot-graph.sh` | Граф |
