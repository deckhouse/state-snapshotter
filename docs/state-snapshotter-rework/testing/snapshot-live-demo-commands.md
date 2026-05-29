# Snapshot live demo — команды для самостоятельного прогона

Копируйте блоки по порядку. Подробности и «что говорить» — в [`snapshot-live-demo-runbook.md`](snapshot-live-demo-runbook.md).

**Не показываем:** restore, VolumeSnapshot restore intent.

**Правила:**

- Трек **A** (manifest) и **C** (volume) — **разные** namespace.
- Трек A: **без PVC** в namespace.
- Трек C: Snapshot **только после** Bound PVC + consumer Pod.
- **Трек D (полный demo)** — отдельный namespace; CSD + `DemoVirtualMachine`/`DemoVirtualDisk` + PVC; один root Snapshot **после** `RBACReady` на CSD.
- Retain — только на **успешном** snapshot.

**Какой сценарий «основной»:**

| Трек | Что показывает |
|------|----------------|
| **C** | Manifest leg (MCP/chunk) + volume leg (`dataRefs` → VSC) — **без** demo domain |
| **D** | Всё из C + **CSD → child snapshots** (VM/disk) + aggregated subtree |
| **B** | Child Snapshot + 2 PVC (автомат `hack/demo-e2e.sh`) |

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

## Трек D — полный demo (CSD + Demo VM/Disk + volume + chunks)

Отдельный namespace. **Не** переиспользуйте namespace трека C: demo children и CSD нужны **до** первого root Snapshot (или удалите старый Snapshot и создайте новый после включения CSD).

### Переменные

```bash
export DEMO_NS=snapshot-demo-full
export SNAP=demo-full
export CSD_NAME=demo-live-vm-disk
export STORAGE_CLASS=local-thin
export PVC=demo-pvc
export BIND_IMAGE=registry.k8s.io/pause:3.9
export CTRL_NS=d8-state-snapshotter
export CTRL_SA=controller
```

### 0. RBAC (обязательно до трека D)

`kubernetes-admin` должен иметь права на demo CRD и CSD. Проверка:

```bash
kubectl auth can-i create demovirtualmachines.demo.state-snapshotter.deckhouse.io -n "$DEMO_NS"
kubectl auth can-i create customsnapshotdefinitions.state-snapshotter.deckhouse.io
```

Ожидание: все три `yes`. Если `no` — **redeploy модуля** с `templates/rbac-for-us.yaml` (в `d8:state-snapshotter:admin-kubeconfig` уже заложены demo CRD + CSD).

**Почему не починить kubectl'ом:** `kubernetes-admin` здесь не суперпользователь — нет `*/*`, нет `clusterroles` **escalate** и **bind**. Значит нельзя ни выдать себе новые API через `ClusterRole`, ни привязать отдельный role к SA. Расширение прав — только через Helm/module (или оператор с bind/escalate).

**Controller SA** для demo reconcile: production — Deckhouse RBAC hook → CSD `RBACReady`; в smoke — §4, но apply `ClusterRoleBinding` тоже требует **bind** у того, кто запускает команды (часто тот же redeploy/оператор, не текущий kubeconfig).

### 1. Namespace и preflight (как трек C)

```bash
kubectl delete namespace "$DEMO_NS" --ignore-not-found --wait=true --timeout=120s
kubectl create namespace "$DEMO_NS"

kubectl get storageclass "$STORAGE_CLASS"
export VSC_NAME=$(kubectl get storageclass "$STORAGE_CLASS" \
  -o jsonpath='{.metadata.annotations.storage\.deckhouse\.io/volumesnapshotclass}')
kubectl get volumesnapshotclass "$VSC_NAME"
kubectl get pods -n "$CTRL_NS" -l app=controller | grep Running
```

### 2. Demo inventory (источники для CSD graph)

```bash
kubectl -n "$DEMO_NS" create configmap demo-snapshot-cm \
  --from-literal=demo=full --dry-run=client -o yaml | kubectl apply -f -

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

### 3. Volume workload (Bound до Snapshot)

```bash
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

until [[ "$(kubectl -n "$DEMO_NS" get pvc "$PVC" -o jsonpath='{.status.phase}')" == "Bound" ]]; do sleep 2; done
kubectl -n "$DEMO_NS" get pvc "$PVC" -o wide
```

### 4. Test-only RBAC для demo controllers + CSD

Эмуляция внешнего Deckhouse RBAC hook (как в [`pre-e2e-smoke-validation.md`](pre-e2e-smoke-validation.md) §7). На production кластере может уже быть выдано — тогда `can-i` ниже должно быть `yes` без apply.

```bash
kubectl apply -f - <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: state-snapshotter-live-demo-domain-rbac
rules:
- apiGroups: ["demo.state-snapshotter.deckhouse.io"]
  resources:
  - demovirtualmachines
  - demovirtualdisks
  - demovirtualmachinesnapshots
  - demovirtualdisksnapshots
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
- apiGroups: ["demo.state-snapshotter.deckhouse.io"]
  resources:
  - demovirtualmachinesnapshots/status
  - demovirtualdisksnapshots/status
  verbs: ["get", "update", "patch"]
- apiGroups: ["demo.state-snapshotter.deckhouse.io"]
  resources:
  - demovirtualmachinesnapshots/finalizers
  - demovirtualdisksnapshots/finalizers
  verbs: ["update", "patch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: state-snapshotter-live-demo-domain-rbac
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: state-snapshotter-live-demo-domain-rbac
subjects:
- kind: ServiceAccount
  name: ${CTRL_SA}
  namespace: ${CTRL_NS}
EOF

kubectl auth can-i create demovirtualmachinesnapshots.demo.state-snapshotter.deckhouse.io \
  --as="system:serviceaccount:${CTRL_NS}:${CTRL_SA}" -n "$DEMO_NS"
```

### 5. CustomSnapshotDefinition (VM + Disk)

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

### 6. Root Snapshot (только после CSD eligible)

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

### 7. Полный чеклист (manifest + volume + demo tree + chunks)

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

# Volume leg на root
kubectl get snapshotcontents.storage.deckhouse.io "$BOUND" -o json | jq '.status.dataRefs'
export VSC=$(kubectl get snapshotcontents.storage.deckhouse.io "$BOUND" -o jsonpath='{.status.dataRefs[0].artifact.name}')
kubectl get volumesnapshotcontents.snapshot.storage.k8s.io "$VSC" -o jsonpath='readyToUse={.status.readyToUse}{"\n"}'

# Aggregated root subtree (CM + demo + PVC в одном ответе)
kubectl get --raw \
  "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/${DEMO_NS}/snapshots/${SNAP}/manifests" \
  | jq '[.[] | {kind, name: .metadata.name}] | {count: length, sample: .[0:12]}'

# Aggregated только VM child subtree
kubectl get --raw \
  "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/${DEMO_NS}/demovirtualmachinesnapshots/${CHILD_VM}/manifests" \
  | jq 'length'
```

Ожидание:

- `childrenSnapshotRefs`: `DemoVirtualMachineSnapshot` на `vm-1`; `DemoVirtualDiskSnapshot` под VM на `disk-vm`; `disk-standalone` — direct child root (не под VM).
- Root + child MCP: `status.chunks` не пустой, chunk `${MCP}-0` с `objectsCount > 0`.
- Root `dataRefs[]` → VSC `readyToUse=true`.
- Aggregated root `count` больше, чем у трека C (есть demo kinds).

### 8. Граф (полное дерево)

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

### 9. Очистка (после прогона)

```bash
kubectl delete customsnapshotdefinition "${CSD_NAME}" --ignore-not-found
kubectl delete namespace "$DEMO_NS" --wait=true
# ClusterRole demo RBAC оставить или удалить вручную, если создавали в §4
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
