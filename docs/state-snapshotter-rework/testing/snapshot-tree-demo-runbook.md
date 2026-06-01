# Snapshot tree demo — cluster runbook (как создаются снапшоты)

Подробный сценарий показа **создания снапшотов** на кластере с акцентом на:

1. **как формируется дерево снапшотов** (root → child subtree);
2. **снапшоты манифестов** (ManifestCaptureRequest → ManifestCheckpoint → chunks);
3. **снапшоты данных** (VolumeCaptureRequest → VolumeSnapshotContent / `dataRefs`);
4. **restore** — один момент: восстанавливаем том из снапшота;
5. **финал** — вручную выставляем маленький TTL и ждём **каскадное удаление дерева контентов** по ObjectKeeper GC.

> Нормативный контракт — [`../spec/system-spec.md`](../spec/system-spec.md) §3,
> [`../design/snapshot-controller.md`](../design/snapshot-controller.md). Этот
> документ — runbook (как запустить демо), без копирования контракта.
> Snapshot-only сценарии без дерева/restore — в
> [`snapshot-live-demo-runbook.md`](snapshot-live-demo-runbook.md) /
> [`snapshot-live-demo-commands.md`](snapshot-live-demo-commands.md).

Используется **demo-domain (Track D1)** — самый наглядный «tree»: контроллер по
`CustomSnapshotDefinition` строит дочерние снапшоты из demo-инвентаря
(VM → Disk), плюс том даёт data-ветку.

## Структура демо (3 части — выполнять как отдельные блоки)

| Часть | Разделы | Когда | Зависит от |
|---|---|---|---|
| **A — основной демо (snapshot capture)** | 3–8 | всегда | только модуль `state-snapshotter` + demo RBAC |
| **B — optional restore** | 9 | только если **выкачен** VRR executor provisioner | новый provisioner image + RBAC provisioner-SA + storage-foundation VRR controller |
| **C — optional TTL-GC финал** | 10 | только на **controlled/debug** кластере | права `patch objectkeepers` (cluster-admin) |

Части B и C — **дополнительные эффектные шаги**. Если provisioner executor не
выкачен или нет прав на ObjectKeeper — **не запускать** их, чтобы не испортить
впечатление от рабочей части A. Часть A полностью самодостаточна.

### Сверка API-групп перед демо (обязательно)

CRD-группы зафиксированы так (подтверждено CRD-файлами репозитория и
`snapshot-live-demo-commands.md`):

| Объект | Группа |
|---|---|
| `Snapshot`, `SnapshotContent` | `storage.deckhouse.io` |
| `ManifestCaptureRequest`, `ManifestCheckpoint`, `ManifestCheckpointContentChunk` | `state-snapshotter.deckhouse.io` |
| `CustomSnapshotDefinition`, `DemoVirtual*` | `state-snapshotter.deckhouse.io` / `demo.state-snapshotter.deckhouse.io` |
| `VolumeCaptureRequest`, `VolumeRestoreRequest` | `storage.deckhouse.io` |
| `VolumeSnapshotContent` | `snapshot.storage.k8s.io` |

Перед прогоном сверить с фактическим кластером (если группа отличается —
заменить во всех командах):

```bash
kubectl api-resources | grep -E 'manifestcapture|manifestcheckpoint|^snapshots|snapshotcontents|customsnapshot|volumecapture|volumerestore'
```

---

## 0. Картина целиком

```
DemoVirtualMachine vm-1 ─owns─► DemoVirtualDisk disk-vm        ConfigMap demo-snapshot-cm
DemoVirtualDisk disk-standalone                                 PVC demo-pvc (Bound)
                         │  CustomSnapshotDefinition demo-live-vm-disk
                         ▼
              Snapshot demo-tree (spec: {})
                         │ controller: MCR + VCR + child snapshots
        ┌────────────────┼───────────────────────────────────────────┐
        ▼                ▼                                             ▼
  childrenSnapshotRefs   manifest leg                              data leg
  ├─ DemoVirtualMachineSnapshot(vm-1)   MCR ─► ManifestCheckpoint ─► Chunk(s)   VCR ─► VolumeSnapshotContent
  │    └─ DemoVirtualDiskSnapshot(disk-vm)        (снапшот манифестов)            (снапшот данных)
  └─ DemoVirtualDiskSnapshot(disk-standalone)

каждый узел: Snapshot ─bound─► SnapshotContent (cluster) ─ownerRef─► ObjectKeeper (follow + TTL)
```

Ключевые сущности:

| Сущность | Роль |
|---|---|
| **Snapshot** (ns) | заявка на снимок узла; после готовности ссылается на content |
| **SnapshotContent** (cluster) | долговечный носитель узла: `manifestCheckpointName`, `dataRefs[]`, `childrenSnapshotContentRefs[]` |
| **ManifestCaptureRequest / ManifestCheckpoint / Chunk** | снапшот **манифестов** (payload в chunk) |
| **VolumeCaptureRequest / VolumeSnapshotContent** | снапшот **данных** тома |
| **ObjectKeeper** | держит content после удаления Snapshot; по TTL → каскадный GC |

---

## 1. Предусловия

```bash
kubectl config current-context
kubectl get pods -n d8-state-snapshotter -l app=controller        # Running
kubectl get crd snapshots.storage.deckhouse.io snapshotcontents.storage.deckhouse.io
kubectl get crd objectkeepers.deckhouse.io                        # нужен для TTL-финала
```

### 1.1. RBAC для demo-domain (Track D)

Проверять **через SA контроллера/вебхука** (`--as=…`), не от своего kubeconfig:

```bash
export DEMO_NS=snapshot-demo-tree
export MOD_NS=d8-state-snapshotter

# webhook SA должен читать demo-инвентарь, иначе MCR admission denied
kubectl auth can-i get demovirtualmachines.demo.state-snapshotter.deckhouse.io \
  --as=system:serviceaccount:${MOD_NS}:webhooks -n "$DEMO_NS"
kubectl auth can-i get demovirtualdisks.demo.state-snapshotter.deckhouse.io \
  --as=system:serviceaccount:${MOD_NS}:webhooks -n "$DEMO_NS"

# controller SA: MCR + demo snapshot /status
kubectl auth can-i create manifestcapturerequests.state-snapshotter.deckhouse.io \
  --as=system:serviceaccount:${MOD_NS}:controller -n "$DEMO_NS"
kubectl auth can-i update demovirtualdisksnapshots.demo.state-snapshotter.deckhouse.io/status \
  --as=system:serviceaccount:${MOD_NS}:controller -n "$DEMO_NS"

# RBAC chunks (для графа)
kubectl auth can-i get manifestcheckpointcontentchunks.state-snapshotter.deckhouse.io
```

Все — `yes`. Если webhook `get` demo inventory = `no` → redeploy модуля
(`templates/webhooks/rbac-for-us.yaml`, `templates/controller/rbac-for-us.yaml`),
иначе дерево не построится (`SubtreeManifestCapturePending`). Детали —
[`snapshot-live-demo-commands.md`](snapshot-live-demo-commands.md) § «Трек D».

### 1.2. Для шага restore (раздел 9 / ЧАСТЬ B) — provisioner executor

Sidecar `provisioner` CSI-драйвера должен быть из ветки `d8-63742164-vrr`
(VRR executor) с применённым RBAC:

```bash
export CSI_NS=d8-sds-local-volume   # ваш CSI-модуль
kubectl get crd volumerestorerequests.storage.deckhouse.io
kubectl get pods -n d8-storage-foundation -l app=controller   # VRR lifecycle controller
# storage-foundation/hack/apply-vrr-rbac.sh "$CSI_NS" csi  — если ещё не применён
```

> Если restore показывать не планируется — раздел 9 / ЧАСТЬ B можно пропустить,
> дерево и TTL-финал от него не зависят.

---

## 2. Скачать манифесты

Каталог [`snapshot-tree-demo/`](snapshot-tree-demo/):

```bash
cd /path/to/state-snapshotter/docs/state-snapshotter-rework/testing/snapshot-tree-demo
ls -1
#  01-source.yaml                         ns + CM + PVC + bind Pod + VM + standalone Disk
#  02-csd.yaml                            CustomSnapshotDefinition
#  03-root-snapshot.yaml                  root Snapshot
#  04-volumerestorerequest.template.yaml  VRR (момент restore)
```

Переменные демо:

```bash
export DEMO_NS=snapshot-demo-tree
export SNAP=demo-tree
export CSD_NAME=demo-live-vm-disk
export STORAGE_CLASS=local-thin
export MANIFEST_DIR="$(pwd)"
```

---

# ЧАСТЬ A — Основной демо (snapshot capture)

Разделы 3–8. Самодостаточны; части B (restore) и C (TTL-GC) — опциональны.

## 3. Источник дерева: инвентарь + том + CSD

### 3.1. Применить source (ns, CM, PVC+Pod, VM, standalone disk)

```bash
kubectl apply -f "$MANIFEST_DIR/01-source.yaml"
```

### 3.2. Добавить **owned** диск (нужен UID VM → inline)

```bash
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
EOF

kubectl -n "$DEMO_NS" get demovirtualmachine,demovirtualdisk
```

### 3.3. Gate: PVC Bound (data-ветка)

`local-thin` — `WaitForFirstConsumer`; bind Pod уже в `01-source.yaml`.

```bash
kubectl -n "$DEMO_NS" get pvc demo-pvc -w     # Ctrl+C на Bound
kubectl -n "$DEMO_NS" get pvc demo-pvc -o jsonpath='{.status.phase}{"\n"}'   # Bound
```

### 3.4. CSD: зарегистрировать и сделать eligible

```bash
kubectl apply -f "$MANIFEST_DIR/02-csd.yaml"

# дождаться Accepted
until kubectl get customsnapshotdefinition "$CSD_NAME" -o json \
  | jq -e '.status.conditions[]?|select(.type=="Accepted" and .status=="True")' >/dev/null; do
  echo "waiting CSD Accepted..."; sleep 2
done

# RBACReady=True (smoke/manual; в production ставит Deckhouse hook)
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

> **Note:** ручной `kubectl replace --subresource=status` на CSD требует прав на
> `customsnapshotdefinitions/status`. Если `forbidden` — у текущего kubeconfig
> нет прав на CSD `/status` (Track D RBAC ещё не готов). Варианты: использовать
> kubeconfig с нужными правами, либо дождаться `RBACReady` от Deckhouse hook
> (production), не выставляя условие вручную.

---

## 4. Создать root Snapshot — и наблюдать формирование дерева

> **Что сказать:** `Snapshot` — заявка. Контроллер создаёт `ObjectKeeper`
> (TTL-якорь), `ManifestCaptureRequest` и `VolumeCaptureRequest` (временные),
> для demo-domain — **дочерние снапшоты** (VM → Disk), затем публикует
> результат каждого узла в `SnapshotContent`.

```bash
kubectl apply -f "$MANIFEST_DIR/03-root-snapshot.yaml"
kubectl -n "$DEMO_NS" get snapshots.storage.deckhouse.io "$SNAP" -w   # Ctrl+C на Ready=True
```

Снять имена один раз:

```bash
export BOUND=$(kubectl -n "$DEMO_NS" get snapshots.storage.deckhouse.io "$SNAP" \
  -o jsonpath='{.status.boundSnapshotContentName}')
export MCP=$(kubectl get snapshotcontents.storage.deckhouse.io "$BOUND" \
  -o jsonpath='{.status.manifestCheckpointName}')
export OK_NAME="ret-snap-${DEMO_NS}-${SNAP}"
echo "BOUND=$BOUND MCP=$MCP OK=$OK_NAME"
```

---

## 5. Дерево снапшотов (главное)

### 5.1. Root → дочерние снапшоты

```bash
kubectl -n "$DEMO_NS" get snapshots.storage.deckhouse.io "$SNAP" -o json | jq '{
  ready: (.status.conditions[]|select(.type=="Ready")),
  children: .status.childrenSnapshotRefs,
  vcr: .status.volumeCaptureRequestName
}'
```

Ожидание: `childrenSnapshotRefs` содержит `DemoVirtualMachineSnapshot` (vm-1) и
`DemoVirtualDiskSnapshot` (disk-standalone как direct child root).

### 5.2. Спуститься по дереву: VM → его Disk

```bash
export CHILD_VM=$(kubectl -n "$DEMO_NS" get snapshots.storage.deckhouse.io "$SNAP" -o json \
  | jq -r '.status.childrenSnapshotRefs[]?|select(.kind=="DemoVirtualMachineSnapshot")|.name')
echo "CHILD_VM=$CHILD_VM"

kubectl -n "$DEMO_NS" get demovirtualmachinesnapshots.demo.state-snapshotter.deckhouse.io "$CHILD_VM" -o json | jq '{
  ready: (.status.conditions[]|select(.type=="Ready")),
  sourceRef: .spec.sourceRef,
  children: .status.childrenSnapshotRefs
}'

CHILD_DISK=$(kubectl -n "$DEMO_NS" get demovirtualmachinesnapshots.demo.state-snapshotter.deckhouse.io "$CHILD_VM" -o json \
  | jq -r '.status.childrenSnapshotRefs[]?|select(.kind=="DemoVirtualDiskSnapshot")|.name')
echo "CHILD_DISK(owned by VM)=$CHILD_DISK"
```

### 5.3. То же на уровне content (`childrenSnapshotContentRefs`)

```bash
kubectl get snapshotcontents.storage.deckhouse.io "$BOUND" -o json | jq '{
  children: .status.childrenSnapshotContentRefs,
  mcp: .status.manifestCheckpointName,
  dataRefs: .status.dataRefs
}'
```

---

## 6. Снапшоты манифестов (manifest leg)

> **Что сказать:** манифесты узла архивируются через `ManifestCaptureRequest`
> (ephemeral) в `ManifestCheckpoint` (cluster), payload лежит в **chunk**
> (get-by-name, не list). Это и есть «снапшот манифестов».

### 6.1. MCR ephemeral (после успеха удаляется)

```bash
SNAP_UID=$(kubectl -n "$DEMO_NS" get snapshots.storage.deckhouse.io "$SNAP" -o jsonpath='{.metadata.uid}')
kubectl -n "$DEMO_NS" get manifestcapturerequests.state-snapshotter.deckhouse.io "snap-${SNAP_UID}" 2>&1 \
  || echo "OK: root MCR уже отработал и удалён"
```

### 6.2. Root MCP → chunks

```bash
kubectl get manifestcheckpoints.state-snapshotter.deckhouse.io "$MCP" -o json | jq '{
  ready: (.status.conditions[]|select(.type=="Ready")),
  totalObjects: .status.totalObjects,
  chunks: .status.chunks
}'
kubectl get manifestcheckpointcontentchunks.state-snapshotter.deckhouse.io "${MCP}-0" \
  -o jsonpath='checkpoint={.spec.checkpointName} objects={.spec.objectsCount}{"\n"}'
```

### 6.3. MCP дочернего узла (у каждого узла свой снапшот манифестов)

```bash
VM_CONTENT=$(kubectl -n "$DEMO_NS" get demovirtualmachinesnapshots.demo.state-snapshotter.deckhouse.io "$CHILD_VM" \
  -o jsonpath='{.status.boundSnapshotContentName}')
VM_MCP=$(kubectl get snapshotcontents.storage.deckhouse.io "$VM_CONTENT" -o jsonpath='{.status.manifestCheckpointName}')
echo "VM_CONTENT=$VM_CONTENT VM_MCP=$VM_MCP"
kubectl get manifestcheckpointcontentchunks.state-snapshotter.deckhouse.io "${VM_MCP}-0" \
  -o jsonpath='vmChunkObjects={.spec.objectsCount}{"\n"}' 2>/dev/null || echo "no VM chunk yet"
```

---

## 7. Снапшоты данных (data leg)

> **Что сказать:** для каждого **Bound** PVC создаётся `VolumeCaptureRequest`
> (bulk), а после publish в `SnapshotContent.status.dataRefs[]` VCR исчезает.
> Артефакт — `VolumeSnapshotContent` (cluster). Это «снапшот данных».

```bash
# VCR после handoff обычно отсутствует
kubectl -n "$DEMO_NS" get volumecapturerequests.storage.deckhouse.io 2>/dev/null || true

# dataRefs[] могут быть НЕ на root, а на дочернем content (например на content диска).
# Сначала посмотреть root:
kubectl get snapshotcontents.storage.deckhouse.io "$BOUND" -o json | jq '.status.dataRefs'
```

`VSC_NAME` ищем по всему дереву (root + дочерние content) — берём первый
`VolumeSnapshotContent` из `dataRefs[]` любого узла:

> **Совместимость shell (важно для live):** этот блок использует `while … read`,
> а не `for c in $VAR`. В **zsh** (shell по умолчанию на macOS) `for x in $PLAIN_VAR`
> НЕ разбивает значение по строкам/словам — цикл выполнится один раз со всей
> многострочной строкой и `kubectl` упадёт. Вариант с `while read` работает
> одинаково в bash и zsh.

```bash
# собрать список content дерева в один поток: root + childrenSnapshotContentRefs
# + content дочерних demo-снапшотов (VM/диски) — и сохранить во временный файл
{
  kubectl get snapshotcontents.storage.deckhouse.io "$BOUND" -o json \
    | jq -r '.metadata.name, (.status.childrenSnapshotContentRefs[]?.name)'
  kubectl -n "$DEMO_NS" get demovirtualmachinesnapshots.demo.state-snapshotter.deckhouse.io,demovirtualdisksnapshots.demo.state-snapshotter.deckhouse.io \
    -o jsonpath='{range .items[*]}{.status.boundSnapshotContentName}{"\n"}{end}' 2>/dev/null
} | sort -u | grep -v '^$' > /tmp/tree-contents.txt
echo "tree contents:"; cat /tmp/tree-contents.txt

# первый VSC из dataRefs любого content дерева
# (while … done < file — НЕ pipe, чтобы VSC_NAME сохранился в текущем shell)
export VSC_NAME=""
while IFS= read -r c; do
  [ -n "$c" ] || continue
  v=$(kubectl get snapshotcontents.storage.deckhouse.io "$c" -o jsonpath='{.status.dataRefs[0].artifact.name}' 2>/dev/null)
  if [ -n "$v" ] && [ -z "$VSC_NAME" ]; then
    export VSC_NAME="$v"; echo "VSC found on content $c: $VSC_NAME"; break
  fi
done < /tmp/tree-contents.txt
[ -n "$VSC_NAME" ] || echo "WARN: VSC в дереве не найден — data leg ещё не готов или PVC не Bound"

kubectl get volumesnapshotcontents.snapshot.storage.k8s.io "$VSC_NAME" -o json | jq '{
  readyToUse: .status.readyToUse,
  snapshotHandle: (.status.snapshotHandle // .spec.source.snapshotHandle),
  restoreSize: .status.restoreSize
}'
```

Ожидание: `dataRefs[]` с `artifact.kind=VolumeSnapshotContent`,
`readyToUse=true`, непустой `snapshotHandle`.

> Если PVC привязан не к root, а к disk-узлу — возьмите `dataRefs` с
> соответствующего child content (`VM_CONTENT` или content диска).

---

## 8. Диаграмма + агрегированное чтение

### 8.1. Граф дерева

> **Тайминг (важно для live):** скрипт делает много `kubectl`-запросов и на
> реальном кластере строит граф **~1.5–2 минуты** (замерено ~123 c). Вывод
> появляется только в конце. **Для показа сгенерируйте SVG заранее** (до
> начала demo) и просто откройте готовый файл; либо запускайте генерацию
> в отдельном окне в фоне, пока рассказываете теорию.

```bash
cd /path/to/state-snapshotter
# ~2 минуты на реальном кластере — заранее, не «вживую»
bash hack/snapshot-graph.sh \
  --namespace "$DEMO_NS" --snapshot "$SNAP" \
  --output-dir /tmp/snapshot-tree --name tree --mode logical \
  --title "Snapshot tree: VM/Disk + volume"

open /tmp/snapshot-tree/tree.logical.svg
# проверка целостности: не должно быть MISSING
grep -c 'MISSING' /tmp/snapshot-tree/tree.logical.dot   # ожидание: 0
grep -E 'status\.chunks|DemoVirtual|status\.dataRefs|childrenSnapshot' /tmp/snapshot-tree/tree.logical.dot | head
```

На SVG видно: `Snap → SC → MCP → Chunk` (манифесты), `SC → VSC` (данные),
зелёные дуги `childrenSnapshot*` (дерево), без `MISSING` на chunk.

### 8.2. Агрегированные манифесты (root + subtree)

> Снять агрегированное чтение **до** удаления root Snapshot (часть C). Здесь
> мы ещё имеем живой Snapshot, endpoint гарантированно отвечает.

Группа `subresources.state-snapshotter.deckhouse.io` отдаёт `/manifests`
**только** для двух видов: `snapshots/manifests` (namespaced) и
`manifestcheckpoints/manifests` (**cluster-scoped**, без `namespaces/...` в
пути). Субресурса `demovirtualmachinesnapshots/manifests` **нет** — обращение к
нему вернёт `Forbidden`. Поэтому root-дерево читаем через `snapshots`, а
VM-subtree — через MCP дочернего VM-снапшота.

```bash
# root: всё дерево объектов через namespaced snapshots/manifests
kubectl get --raw \
  "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/${DEMO_NS}/snapshots/${SNAP}/manifests" \
  | jq '[.[] | {kind, name: .metadata.name}] | {count: length, sample: .[0:12]}' \
  | tee /tmp/${SNAP}-aggregated-before-delete.json

# VM-subtree: через MCP дочернего VM-снапшота (cluster-scoped путь, БЕЗ namespaces/...)
VM_CONTENT=$(kubectl -n "$DEMO_NS" get demovirtualmachinesnapshots.demo.state-snapshotter.deckhouse.io "$CHILD_VM" -o jsonpath='{.status.boundSnapshotContentName}')
CHILD_MCP=$(kubectl get snapshotcontents.storage.deckhouse.io "$VM_CONTENT" -o jsonpath='{.status.manifestCheckpointName}')
echo "VM_CONTENT=$VM_CONTENT CHILD_MCP=$CHILD_MCP"
kubectl get --raw \
  "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/manifestcheckpoints/${CHILD_MCP}/manifests" \
  | jq '[.[] | {kind, name: .metadata.name}]'
```

На этом **основной демо (часть A) завершён**: показаны дерево, снапшоты
манифестов и данных, граф и агрегированное чтение. Части B и C ниже —
опциональные.

---

# ЧАСТЬ B — Optional restore (только если выкачен VRR executor)

> **Запускать только если** patched provisioner image выкачен и применён RBAC
> provisioner-SA. Иначе **пропустить** — часть A уже доказала рабочую
> snapshot-систему, а сырой restore-шаг может испортить показ.

## 9. Restore (один момент)

### 9.0. Пречеки готовности executor (обязательно перед restore)

```bash
export CSI_NS=d8-sds-local-volume   # ваш CSI-модуль

# label может отличаться между модулями — пробуем несколько, иначе выбираем вручную
POD=$(kubectl -n "$CSI_NS" get pods -l app=csi-controller -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
[ -n "$POD" ] || POD=$(kubectl -n "$CSI_NS" get pods -o name 2>/dev/null | grep -E 'csi-controller|controller' | head -1 | cut -d/ -f2)
echo "POD=$POD"
# Если $POD пустой — выбрать pod вручную:
#   kubectl -n "$CSI_NS" get pods
#   export POD=<имя-controller-pod-с-provisioner-sidecar>

# 1) какие контейнеры в pod (узнать реальное имя provisioner sidecar)
kubectl -n "$CSI_NS" get pod "$POD" -o jsonpath='{range .spec.containers[*]}{.name}{"  "}{.image}{"\n"}{end}'

# выбрать имя контейнера из вывода выше (обычно provisioner | csi-provisioner | external-provisioner)
export PROV_CONT=provisioner

# 2) в логах есть запуск VRR executor (признак патченого образа)
kubectl -n "$CSI_NS" logs "$POD" -c "$PROV_CONT" --tail=300 \
  | grep -iE 'Starting VRR executor|Resource=volumerestorerequests' | head

# 3) provisioner-SA имеет права на VRR
kubectl auth can-i list volumerestorerequests.storage.deckhouse.io \
  --as="system:serviceaccount:${CSI_NS}:csi"
```

Все три проверки должны быть положительными (контейнер найден, есть
`Starting VRR executor`, `can-i list … = yes`). Если нет — **не выполнять
restore**, перейти к части C или к очистке.

### 9.1. Создать VRR и наблюдать восстановление

```bash
# VSC_NAME уже найден в разделе 7
envsubst < "$MANIFEST_DIR/04-volumerestorerequest.template.yaml" | kubectl apply -f -

# executor (external-provisioner): CreateVolume → PV/PVC (имя контейнера из 9.0)
kubectl -n "$CSI_NS" logs "$POD" -c "$PROV_CONT" --tail=120 \
  | grep -iE 'restore-demo|restoreFromVSC|CreateVolume|created PV|created PVC' | tail

# PVC Bound + VRR Ready (статус ставит storage-foundation controller)
kubectl -n "$DEMO_NS" get pvc demo-pvc-restored
kubectl -n "$DEMO_NS" get volumerestorerequests.storage.deckhouse.io restore-demo \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}{"\n"}{end}'
```

Ожидание: `demo-pvc-restored` → `Bound`; затем VRR `Ready=True reason=Completed`
(статус ставит storage-foundation controller **после** того, как PVC стал
`Bound`; до этого conditions у VRR может вообще не быть — это нормально).

> **Известный blocker текущего образа (проверено на кластере 2026-06-01):**
> задеплоенный provisioner создаёт restored PV **без** `spec.storageClassName`
> (`null`), а PVC имеет `local-thin` → PVC висит `Pending` с событием
> `VolumeMismatch: storageClassName does not match`, и VRR **никогда** не дойдёт
> до `Ready`. Фикс есть в коде executor (`vrr_handler.go`, `StorageClassName:
> sc.Name`), но **ещё не собран в текущий образ** provisioner. Пока образ не
> пересобран — для демо нужен обход (выполнить сразу после создания VRR):
>
> ```bash
> PVNAME=$(kubectl -n "$DEMO_NS" get pvc demo-pvc-restored -o jsonpath='{.spec.volumeName}')
> kubectl patch pv "$PVNAME" --type merge -p '{"spec":{"storageClassName":"'"$STORAGE_CLASS"'"}}'
> ```
>
> После patch PVC связывается за ~15–20 c, и VRR переходит в `Ready=True`.
> Для чистого live-показа лучше **выкатить образ с фиксом** и обход не
> демонстрировать.

---

# ЧАСТЬ C — Optional TTL-GC финал (debug / controlled cluster only)

> **ВНИМАНИЕ:** этот раздел патчит `ObjectKeeper.spec.ttl` и принудительно
> запускает каскадное удаление контентов. Выполнять **только на
> controlled/debug-кластере** и **только** если понимаете последствия. На общем
> кластере **не выполнять**: можно удалить чужие retained-контенты.
>
> Если прав на `patch objectkeepers` нет — пропустить весь раздел; на реальном
> небольшом TTL (например `1m`) дерево удалится само без ручного патча.

## 10. Финал: маленький TTL → каскадное удаление дерева

> **Что сказать:** удаляем **только** root `Snapshot`. Дерево контентов
> остаётся живым — его держат `ObjectKeeper` (follow + TTL). Затем руками
> ставим **маленький TTL** и наблюдаем, как **всё дерево удаляется само**:
> SnapshotContent → ManifestCheckpoint → chunks → VolumeSnapshotContent.

### 10.1. Сохранить список объектов дерева ДО удаления (обязательно)

Снимок «что ожидаем удалить» — чтобы после GC было понятно, что именно исчезло.

Список контентов дерева строим **точно** из root `$BOUND` и его
`childrenSnapshotContentRefs` (а не широким `^ns-`, который может зацепить чужие
root-контенты):

```bash
TREE_SNAPSHOT=/tmp/${SNAP}-tree-before-gc.txt
{
  echo "== SnapshotContents дерева (root $BOUND + children refs) =="
  kubectl get snapshotcontents.storage.deckhouse.io "$BOUND" -o json | jq -r '
    .metadata.name, (.status.childrenSnapshotContentRefs[]?.name)'
  # content дочерних demo-снапшотов (VM/диски)
  kubectl -n "$DEMO_NS" get demovirtualmachinesnapshots.demo.state-snapshotter.deckhouse.io,demovirtualdisksnapshots.demo.state-snapshotter.deckhouse.io \
    -o jsonpath='{range .items[*]}{.status.boundSnapshotContentName}{"\n"}{end}' 2>/dev/null
  echo "== ManifestCheckpoints (root + дочерние) =="
  echo "root: $MCP"; echo "vm:   ${VM_MCP:-<none>}"
  echo "== ObjectKeepers (follow ns=$DEMO_NS) =="
  kubectl get objectkeepers.deckhouse.io -o json | jq -r --arg ns "$DEMO_NS" \
    '.items[] | select(.spec.followObjectRef.namespace==$ns) | "\(.metadata.name)\tfollow=\(.spec.followObjectRef.kind)/\(.spec.followObjectRef.name)\tttl=\(.spec.ttl)"'
  echo "== VolumeSnapshotContents (по dataRefs) =="
  echo "${VSC_NAME:-<none>}"
} | tee "$TREE_SNAPSHOT"
```

> Грубый fallback (если нужно «всё подозрительное») — список `^ns-*` контентов,
> но он может включать чужие root-контенты, поэтому для демо опираемся на
> `$BOUND` + `childrenSnapshotContentRefs`:
>
> ```bash
> kubectl get snapshotcontents.storage.deckhouse.io | grep -E '^ns-'   # rough, не точный список дерева
> ```

### 10.2. Удалить root Snapshot — дерево retained

```bash
kubectl -n "$DEMO_NS" delete snapshots.storage.deckhouse.io "$SNAP" --wait=true

# content остаётся живым (его держит ObjectKeeper)
kubectl get snapshotcontents.storage.deckhouse.io "$BOUND" -o wide
```

Агрегированное чтение после удаления namespaced Snapshot — **поведение зависит
от реализации route**. Retained-read по имени Snapshot работает, пока жив
ObjectKeeper/content, но если endpoint требует живой Snapshot — упадёт. Поэтому
эталон уже снят в §8.2 (до удаления). Надёжный retained-read — по **MCP**
(cluster route, не зависит от namespaced Snapshot):

```bash
# может ещё отвечать по имени snapshot (retained) — но не полагаться:
kubectl get --raw \
  "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/${DEMO_NS}/snapshots/${SNAP}/manifests" 2>&1 \
  | head -1

# надёжный retained-read архива по MCP:
kubectl get --raw \
  "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/manifestcheckpoints/${MCP}/manifests" \
  | jq 'length'
```

### 10.3. Дождаться экспирации TTL (рекомендуемый путь, без патча)

> **Проверено на кластере (2026-06-01):** ObjectKeeper снапшота создаётся уже с
> `spec.ttl=1m0s`, и `kubectl auth can-i patch objectkeepers` для обычного admin
> kubeconfig = **no**. Поэтому **ручной патч TTL не нужен и обычно запрещён** —
> просто удалите root Snapshot (§10.2) и **подождите ~1.5 минуты**: после того
> как followed `Snapshot` исчез, ObjectKeeper экспирирует по своему `1m` TTL и
> каскадно удаляет всё дерево. В прогоне каскад завершился через **~75–90 c**
> после удаления Snapshot.

```bash
# убедиться, что TTL у ObjectKeeper дерева маленький (обычно 1m) — патч не нужен
kubectl get objectkeepers.deckhouse.io -o json | jq -r --arg ns "$DEMO_NS" \
  '.items[] | select(.spec.followObjectRef.namespace==$ns) | "\(.metadata.name)\tttl=\(.spec.ttl)"'
```

> **Forced expiry (только cluster-admin, опционально):** если очень нужно
> ускорить и есть права (`can-i patch objectkeepers = yes`), можно выставить
> `ttl=0s`. Для обычного admin это **forbidden** — тогда пропустите этот шаг и
> просто дождитесь естественного TTL выше.
>
> ```bash
> for ok in $(kubectl get objectkeepers.deckhouse.io -o json | jq -r --arg ns "$DEMO_NS" \
>               '.items[] | select(.spec.followObjectRef.namespace==$ns) | .metadata.name'); do
>   echo "patch $ok ttl=0s"
>   kubectl patch objectkeepers.deckhouse.io "$ok" --type=merge -p '{"spec":{"ttl":"0s"}}'
> done
> ```

### 10.4. Наблюдать каскадное удаление

> Каскад стартует **через ~1 мин после удаления Snapshot** (TTL ObjectKeeper) и
> завершается за **~75–90 c** суммарно. Не пугайтесь, что первые ~60 c дерево
> «висит» retained — это и есть демонстрируемое поведение.

```bash
# дерево контентов исчезает: content → MCP → chunks → VSC
watch -n3 '
  echo "== SnapshotContents (ns-*) ==";  kubectl get snapshotcontents.storage.deckhouse.io | grep -E "ns-" || echo none
  echo "== ManifestCheckpoints ==";      kubectl get manifestcheckpoints.state-snapshotter.deckhouse.io 2>/dev/null | tail -n +1 || echo none
  echo "== ObjectKeepers (demo) ==";     kubectl get objectkeepers.deckhouse.io 2>/dev/null | grep -E "ret-snap-'"$DEMO_NS"'" || echo none
'
# Ctrl+C когда всё пусто

# точечная проверка после GC
kubectl get snapshotcontents.storage.deckhouse.io "$BOUND" 2>&1 || echo "root content удалён"
kubectl get manifestcheckpoints.state-snapshotter.deckhouse.io "$MCP" 2>&1 || echo "root MCP удалён"
kubectl get objectkeepers.deckhouse.io "$OK_NAME" 2>&1 || echo "root ObjectKeeper удалён"
# aggregated read по старому URL теперь должен падать (content нет)
kubectl get --raw \
  "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/${DEMO_NS}/snapshots/${SNAP}/manifests" 2>&1 \
  | head -1
```

Критерий финала: все `SnapshotContent`/`ManifestCheckpoint`/chunks/`ObjectKeeper`
дерева удалены, aggregated read больше не отвечает.

---

## 11. Очистка

```bash
kubectl delete customsnapshotdefinition "$CSD_NAME" --ignore-not-found
kubectl delete namespace "$DEMO_NS" --wait=true
# восстановленный PV (ReclaimPolicy Delete) уйдёт с PVC; при Retain — вручную
```

---

## 12. Траблшутинг

| Симптом | Причина | Что делать |
|---|---|---|
| root `Ready=False` + `SubtreeManifestCapturePending`, MCR нет | webhook SA не читает demo inventory | redeploy `templates/webhooks/rbac-for-us.yaml`; см. §1.1 |
| `childrenSnapshotRefs` пусто | CSD не Ready / не RBACReady | проверить §3.4; логи controller |
| `dataRefs` пусто, VCR `Ready=False` `PVC … is not bound` | PVC не Bound (WFFC без Pod) | bind Pod есть в `01-source.yaml`; дождаться Bound до Snapshot |
| restore PVC `Pending`, `VolumeMismatch` | PV без `storageClassName` (старый provisioner) | §9 обход `kubectl patch pv`; пересобрать executor |
| TTL-патч `objectkeepers` forbidden | нет прав | cluster-admin или временный ClusterRole; либо дождаться реального TTL |

Логи (порядок диагностики):

```bash
kubectl logs -n d8-state-snapshotter -l app=controller --tail=300
kubectl logs -n d8-storage-foundation -l app=controller --tail=200   # restore lifecycle
# restore executor: имя контейнера узнать из §9.0 (provisioner | csi-provisioner | external-provisioner)
kubectl -n "$CSI_NS" logs "$POD" -c "$PROV_CONT" --tail=200
```

---

## Связанные файлы

| Файл | Назначение |
|---|---|
| [`snapshot-tree-demo/`](snapshot-tree-demo/) | Скачиваемые YAML (source, CSD, snapshot, VRR template) |
| [`snapshot-live-demo-runbook.md`](snapshot-live-demo-runbook.md) | Snapshot-only live demo, речь, pitfalls, тайминг |
| [`snapshot-live-demo-commands.md`](snapshot-live-demo-commands.md) | Команды треков A/C/D/B (источник проверенных блоков) |
| `hack/demo-e2e.sh` | Автоматический трек B; stage 08 = forced TTL GC |
| `hack/snapshot-graph.sh` | Граф дерева |
| `storage-foundation/hack/apply-vrr-rbac.sh` | RBAC provisioner SA для restore |
