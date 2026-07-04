# Live demo: snapshot flow (без restore)

Пошаговый сценарий показа **snapshot capture** на кластере: ресурсы, связи, subresource `/manifests`, граф, child-дерево, delete/retain.

**Не показываем:** restore, import, VolumeSnapshot restore intent.

**Нормативка:** [`../spec/system-spec.md`](../spec/system-spec.md) §3, [`../design/snapshot-controller.md`](../design/snapshot-controller.md) §4–5.

**Короткий линейный вариант (только manifest, `default`):** [`snapshot-manual-demo.md`](snapshot-manual-demo.md).

**Команды для самостоятельного прогона (copy-paste):** [`snapshot-live-demo-commands.md`](snapshot-live-demo-commands.md) (треки C/D, chunks, demo CSD).

---

## Что сказать в начале (30 секунд)

> Мы снимаем **логический узел** namespace: манифесты попадают в **ManifestCheckpoint**, данные томов — в **VolumeSnapshotContent**, всё связывается через **Snapshot** (планирование) и **SnapshotContent** (долговечный результат). Пользовательский API чтения — **aggregated manifests** subresource; прямого доступа к chunk payload нет. Жизненный цикл удерживает **ObjectKeeper** с TTL после удаления Snapshot.

---

## Предусловия (проверить до зала)

| Проверка | Команда | Ожидание |
|----------|---------|----------|
| Контекст kubectl | `kubectl config current-context` | нужный кластер |
| Модуль | `kubectl get pods -n d8-state-snapshotter -l app=controller` | `Running` |
| CRD Snapshot | `kubectl get crd snapshots.state-snapshotter.deckhouse.io` | есть |
| Subresource discovery | `kubectl get --raw /apis/subresources.state-snapshotter.deckhouse.io/v1alpha1 \| jq '.resources[] \| select(.name=="snapshots/manifests")'` | `namespaced: true` |
| RBAC aggregated | `kubectl auth can-i get snapshots/manifests.subresources.state-snapshotter.deckhouse.io -n <NS>` | `yes` |
| RBAC chunks (граф) | `kubectl auth can-i get manifestcheckpointcontentchunks.state-snapshotter.deckhouse.io --as system:serviceaccount:d8-state-snapshotter:controller` | `yes` (chunks читаются под controller SA; admin-kubeconfig прямого доступа к payload не имеет — by design) |
| **Track D — webhook SA** | см. ниже | оба `yes` после redeploy |
| **Track D — controller SA** | см. ниже | MCR create + demo `/status` `yes` |
| ObjectKeeper controller | Deckhouse / `objectkeepers.deckhouse.io` отвечает | иначе TTL/GC не показать |

**Track D RBAC preflight** (проверять с `--as=system:serviceaccount:…`, не от admin kubeconfig):

```bash
export DEMO_NS=snapshot-demo-full
export MOD_NS=d8-state-snapshotter

# Blocker MCR admission: webhook must read demo inventory targets
kubectl auth can-i get demovirtualdisks.demo.state-snapshotter.deckhouse.io \
  --as=system:serviceaccount:${MOD_NS}:webhooks -n "$DEMO_NS"
kubectl auth can-i get demovirtualmachines.demo.state-snapshotter.deckhouse.io \
  --as=system:serviceaccount:${MOD_NS}:webhooks -n "$DEMO_NS"

# Child/MCR reconcile (chart: templates/controller/rbac-for-us.yaml)
kubectl auth can-i create manifestcapturerequests.state-snapshotter.deckhouse.io \
  --as=system:serviceaccount:${MOD_NS}:controller -n "$DEMO_NS"
kubectl auth can-i update demovirtualdisksnapshots.demo.state-snapshotter.deckhouse.io/status \
  --as=system:serviceaccount:${MOD_NS}:controller -n "$DEMO_NS"
```

Шаблоны: `templates/webhooks/rbac-for-us.yaml` (demo inventory `get/list/watch`), `templates/controller/rbac-for-us.yaml` (demo snapshots + MCR/MCP). Не полагаться на временный `state-snapshotter-smoke-demo-domain-rbac`.

**Storage для volume-ветки:** на этом кластере рабочий класс для demo-e2e — **`local-thin`** (PVC должен быть **Bound**; у `WaitForFirstConsumer` нужен Pod, иначе volume capture падает).

---

## Треки demo

| Трек | Когда | Namespace | Workload |
|------|-------|-----------|----------|
| **A — manifest-only** | Быстрый показ 2–3 мин | отдельный ns (`snapshot-demo-manifest`) | **Только** ConfigMap; **PVC не создавать** |
| **C — volume-only** | **Основной live demo** — manifest + **один** том | отдельный ns (`snapshot-demo-volume`) | 1× **Bound** PVC + consumer Pod + ConfigMap (опционально) |
| **D — optional full demo** | CSD + Demo VM/Disk + child tree; **не** для зала по умолчанию | `snapshot-demo-full` | **D0:** без PVC; **D1:** + PVC только после D0 |
| **B — полный N5** | Child + residual root + 2 PVC | `demo-e2e-*` или артефакты e2e | 2× Bound PVC + child Snapshot; эталон — `hack/demo-e2e.sh` |

**Правила:**

- Треки A, C, D — **разные namespace**. **Не смешивать C и D** (разные цели: C = стабильный live, D = диагностика domain/CSD).
- Snapshot создаём **только после** preflight (для C и D1 — обязательно Bound PVC).
- **Трек D:** redeploy модуля (webhook + controller RBAC) → preflight webhook/controller SA → **D0 без PVC** → только после успешного D0 опционально **D1** с PVC. Команды — [`snapshot-live-demo-commands.md`](snapshot-live-demo-commands.md) § «Трек D». **Live = трек C**; D не включать до успешного D0.

Ниже — этапы; для A/C/B используйте соответствующие блоки preflight и workload.

---

## Known pitfalls / live demo constraints

Ограничения, проверенные на кластере (2026-05-29). Restore в demo **не** показываем.

### Manifest-only = без PVC

Для быстрого manifest-only demo **нельзя** создавать PVC в namespace snapshot. Root Snapshot включает **residual PVC discovery**: любой PVC в ns попадает в `spec.targets` VCR, даже в фазе **Pending**. Появится volume leg, `status.volumeCaptureRequestName`, красный VCR на графе — это не «поломка», а следствие лишнего объекта в ns.

### Volume demo = только Bound PVC

Для volume demo PVC должен быть **Bound**. На **`local-thin`** (`WaitForFirstConsumer`) нужен **consumer Pod** (или иной workload), иначе PVC остаётся Pending и VCR падает с `PVC <ns>/<name> is not bound`.

### Failed VCR и флап `Snapshot.Ready`

Если PVC Pending:

- VCR: `Ready=False`, reason `InternalError`, message `PVC … is not bound`;
- **Snapshot.Ready** может **флапать** между зеркалом от content и `VolumeCaptureFailed` от volume publish;
- **SnapshotContent** при этом может оставаться **Ready=True**, если manifest leg/MCP успешен, а **`dataRefs` пустые** (data leg N/A для SCC).

Это **не** поломка MCP, chunk или aggregated manifests — объяснять аудитории отдельно: planning (Snapshot) vs durable result (SnapshotContent) при незавершённом volume leg.

### Aggregated manifests и Pending PVC

Если Pending PVC всё же был в namespace, aggregated `/manifests` **может** вернуть его в списке объектов. Это шум demo-подготовки, не ошибка API.

### Retain

Retain показывать **только на успешном** snapshot (трек A без PVC или трек B) и **сразу после** `delete` root Snapshot:

```bash
kubectl get snapshotcontents.state-snapshotter.deckhouse.io "${BOUND}"
kubectl get objectkeepers.deckhouse.io "${OK_NAME}"
kubectl get --raw ".../snapshots/${SNAP}/manifests" | jq 'length'
```

Позже **ObjectKeeper TTL** (часто **1m** в debug) и GC уберут SnapshotContent → MCP → chunks. На failed volume leg retain на момент delete может быть неочевиден; не строить demo вокруг такого состояния.

### Два сценария для live (кратко)

| Сценарий | Трек | Подготовка |
|----------|------|------------|
| **Быстрый manifest** | A | ConfigMap-only, **без PVC** |
| **Volume на живом ns** | C | Preflight → Bound PVC → Snapshot |
| **Полный N5** | B | `demo-e2e-*` ns и/или `artifacts/.../06-root-ready/graph/*.svg` |

---

## Preflight для volume demo (трек C)

Выполнить **до** `kubectl apply` Snapshot. Любой fail — не начинать demo.

```bash
export DEMO_NS=snapshot-demo-volume   # отдельный ns, не смешивать с A/B
export STORAGE_CLASS=local-thin       # как в hack/demo-e2e.sh
export PVC=demo-pvc
export BIND_IMAGE=registry.k8s.io/pause:3.9

# 1) Namespace без хвостов от прошлых прогонов
kubectl create namespace "$DEMO_NS" --dry-run=client -o yaml | kubectl apply -f -
kubectl -n "$DEMO_NS" get volumecapturerequests.storage-foundation.deckhouse.io 2>/dev/null \
  | awk 'NR==1 || $1!="NAME"' | wc -l | xargs -I{} test {} -eq 0 \
  || { echo "FAIL: старые VCR в namespace"; exit 1; }

# 2) Нет Pending PVC (кроме только что созданного — см. шаг 4)
kubectl -n "$DEMO_NS" get pvc -o json 2>/dev/null | jq -e \
  '[.items[] | select(.status.phase != "Bound")] | length == 0' >/dev/null \
  || echo "WARN: есть не-Bound PVC — удалите или доведите до Bound"

# 3) StorageClass + VolumeSnapshotClass
kubectl get storageclass "$STORAGE_CLASS" >/dev/null
VSC_NAME=$(kubectl get storageclass "$STORAGE_CLASS" -o jsonpath='{.metadata.annotations.storage\.deckhouse\.io/volumesnapshotclass}')
[[ -n "$VSC_NAME" ]] || { echo "FAIL: SC без annotation storage.deckhouse.io/volumesnapshotclass"; exit 1; }
kubectl get volumesnapshotclass "$VSC_NAME" >/dev/null || { echo "FAIL: VolumeSnapshotClass $VSC_NAME"; exit 1; }
echo "OK StorageClass=$STORAGE_CLASS VolumeSnapshotClass=$VSC_NAME"

# 4) Модуль и RBAC (общие предусловия)
kubectl get pods -n d8-state-snapshotter -l app=controller | grep -q Running
kubectl auth can-i get snapshots/manifests.subresources.state-snapshotter.deckhouse.io -n "$DEMO_NS" | grep -q yes
# Chunks читаются графом под controller SA (admin-kubeconfig прямого доступа не имеет — by design):
kubectl auth can-i get manifestcheckpointcontentchunks.state-snapshotter.deckhouse.io \
  --as system:serviceaccount:d8-state-snapshotter:controller | grep -q yes
echo "OK controller + RBAC"
```

После preflight — workload (PVC **до** Snapshot):

```bash
kubectl -n "$DEMO_NS" create configmap demo-snapshot-cm --from-literal=demo=volume \
  --dry-run=client -o yaml | kubectl apply -f -

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

# Consumer Pod обязателен при WaitForFirstConsumer
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

# 5) PVC Bound — gate перед Snapshot
for i in $(seq 1 90); do
  phase=$(kubectl -n "$DEMO_NS" get pvc "$PVC" -o jsonpath='{.status.phase}' 2>/dev/null)
  [[ "$phase" == "Bound" ]] && break
  sleep 2
done
kubectl -n "$DEMO_NS" get pvc "$PVC" -o wide
[[ "$(kubectl -n "$DEMO_NS" get pvc "$PVC" -o jsonpath='{.status.phase}')" == "Bound" ]] \
  || { echo "FAIL: PVC not Bound"; exit 1; }
echo "OK preflight complete — можно создавать Snapshot"
```

---

## Volume happy path — критерии приёмки (трек C)

После создания Snapshot дождаться steady state (~1–3 мин на `local-thin`). **Все** пункты должны выполняться:

| # | Проверка | Команда / ожидание |
|---|----------|-------------------|
| 1 | Snapshot Ready | `Ready=True`, reason `Completed` |
| 2 | SnapshotContent Ready | `Ready=True` |
| 3 | MCP Ready | `status.conditions[Ready]=True` |
| 4 | VCR | **отсутствует** после handoff (или кратко `Ready=True`, затем delete) |
| 5 | `dataRefs[]` | не пустой, `artifact.kind=VolumeSnapshotContent` |
| 6 | VSC | `kubectl get volumesnapshotcontent <name>` — exists |
| 7 | VSC ready | `status.readyToUse=true` |
| 8 | Snapshot `volumeCaptureRequestName` | **пусто** после publish |
| 9 | Graph | `status.dataRefs[].artifact` → `VolumeSnapshotContent` в `.dot` |
| 10 | Aggregated | `GET .../snapshots/<snap>/manifests` → `length >= 1` |

Сводная проверка:

```bash
export SNAP=demo-volume
export BOUND=$(kubectl -n "$DEMO_NS" get snapshots.state-snapshotter.deckhouse.io "$SNAP" -o jsonpath='{.status.boundSnapshotContentName}')

kubectl -n "$DEMO_NS" get snapshots.state-snapshotter.deckhouse.io "$SNAP" -o json | jq '{
  snapReady: (.status.conditions[]|select(.type=="Ready")),
  vcr: .status.volumeCaptureRequestName
}'
kubectl get snapshotcontents.state-snapshotter.deckhouse.io "$BOUND" -o json | jq '{
  contentReady: (.status.conditions[]|select(.type=="Ready")),
  dataRefs: .status.dataRefs,
  mcp: .status.manifestCheckpointName
}'
VSC=$(kubectl get snapshotcontents.state-snapshotter.deckhouse.io "$BOUND" -o jsonpath='{.status.dataRefs[0].artifact.name}')
kubectl get volumesnapshotcontents.snapshot.storage.k8s.io "$VSC" -o jsonpath='{.status.readyToUse}{"\n"}'
MCP=$(kubectl get snapshotcontents.state-snapshotter.deckhouse.io "$BOUND" -o jsonpath='{.status.manifestCheckpointName}')
kubectl get manifestcheckpoints.state-snapshotter.deckhouse.io "$MCP" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}{"\n"}'
kubectl get --raw "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/${DEMO_NS}/snapshots/${SNAP}/manifests" | jq 'length'
```

---

## Do not do this during live demo

- **Не** создавать PVC без consumer Pod на `WaitForFirstConsumer` StorageClass.
- **Не** запускать Snapshot (volume path) пока PVC в **Pending**.
- **Не** смешивать manifest-only (A) и volume (C) в **одном** namespace.
- **Не** использовать namespace с красными/старыми VCR от прошлых прогонов без очистки.
- **Не** брать случайный старый `demo-e2e-*` / `snapshot-demo-live` без preflight и `kubectl get vcr,pvc,snap`.
- **Не** показывать retain на failed snapshot (orphan VCR, `VolumeCaptureFailed`).
- **Не** ждать retain/GC на сцене дольше ~1–2 мин (OK TTL); показать retain **сразу** после delete или объяснить TTL словами.
- **Не** показывать restore / VolumeSnapshot restore intent.

---

## Этап 0 — подготовить namespace и workload

### Что говорить

> Снимаем namespace «как есть». Для volume leg PVC должен быть **Bound до** Snapshot. На `WaitForFirstConsumer` сначала Pod, потом проверка фазы, только потом Snapshot.

### Трек A (manifest-only)

```bash
export DEMO_NS=snapshot-demo-manifest
kubectl create namespace "$DEMO_NS" --dry-run=client -o yaml | kubectl apply -f -
kubectl -n "$DEMO_NS" create configmap demo-snapshot-cm \
  --from-literal=demo=live --dry-run=client -o yaml | kubectl apply -f -
# PVC не создавать — см. Known pitfalls
```

### Трек C (volume-only)

См. секцию **Preflight для volume demo** — полный блок preflight + PVC + `bind-${PVC}` Pod. Snapshot **не** создавать до `PVC Bound`.

### Трек B (полный N5)

```bash
# Эталон: hack/demo-e2e.sh + DEMO_E2E_SKIP_CLEANUP=1
# Или готовый demo-e2e-<run-id> после preflight:
kubectl -n demo-e2e-<id> get pvc -o wide   # оба Bound
kubectl -n demo-e2e-<id> get volumecapturerequests.storage-foundation.deckhouse.io
```

### Техническая проверка перед Snapshot

| Трек | Gate |
|------|------|
| A | есть CM; **нет** PVC в ns |
| C | preflight OK; PVC **Bound**; consumer Pod Running/Completed; нет stale VCR |
| B | оба PVC Bound; child/root Snapshots по сценарию e2e |

---

## Этап 1 — создать root Snapshot

### Что говорить

> **Snapshot** — namespaced «заявка на снимок». Контроллер создаёт **ObjectKeeper** (TTL-якорь), **ManifestCaptureRequest** и **VolumeCaptureRequest** (временные), затем публикует результат в **SnapshotContent**.

```bash
export SNAP=demo-volume          # трек A: demo-manifest; трек C: demo-volume; трек B: demo-root
# Трек C: выполнять только после preflight + PVC Bound
kubectl -n "$DEMO_NS" apply -f - <<EOF
apiVersion: state-snapshotter.deckhouse.io/v1alpha1
kind: Snapshot
metadata:
  name: ${SNAP}
  namespace: ${DEMO_NS}
spec: {}
EOF
```

### Что показать зрителям

`kubectl -n "$DEMO_NS" get snapshots.state-snapshotter.deckhouse.io -w`

### Техническая проверка

Дождаться (обычно 30–120 с на трек A):

```bash
kubectl -n "$DEMO_NS" get snapshots.state-snapshotter.deckhouse.io "${SNAP}" -o json | jq '{
  bound: .status.boundSnapshotContentName,
  mcr: .status.manifestCaptureRequestName,
  vcr: .status.volumeCaptureRequestName,
  children: .status.childrenSnapshotRefs,
  ready: (.status.conditions[] | select(.type=="Ready"))
}'
```

Ожидание при успехе: `bound` не пустой, `Ready=True`, `mcr` и `vcr` после handoff — **null**. Для трека C см. **Volume happy path — критерии приёмки**.

---

## Этап 2 — цепочка ресурсов (основной слайд)

Выпишите имена один раз:

```bash
export BOUND=$(kubectl -n "$DEMO_NS" get snapshots.state-snapshotter.deckhouse.io "${SNAP}" \
  -o jsonpath='{.status.boundSnapshotContentName}')
export MCP=$(kubectl get snapshotcontents.state-snapshotter.deckhouse.io "${BOUND}" \
  -o jsonpath='{.status.manifestCheckpointName}')
export OK_NAME="ret-snap-${DEMO_NS}-${SNAP}"
echo "BOUND=${BOUND} MCP=${MCP} OK=${OK_NAME}"
```

### 2.1 Snapshot

**Говорить:** planning object; после готовности ссылается на content, не хранит payload.

```bash
kubectl -n "$DEMO_NS" get snapshots.state-snapshotter.deckhouse.io "${SNAP}" -o yaml
```

### 2.2 SnapshotContent (cluster)

**Говорить:** единственный долговечный cluster-scoped carrier узла; **ownerRef → ObjectKeeper**, не на Snapshot. Здесь `manifestCheckpointName`, `dataRefs[]`, `childrenSnapshotContentRefs[]`.

```bash
kubectl get snapshotcontents.state-snapshotter.deckhouse.io "${BOUND}" -o wide
kubectl get snapshotcontents.state-snapshotter.deckhouse.io "${BOUND}" -o json | jq '{
  owners: .metadata.ownerReferences,
  mcp: .status.manifestCheckpointName,
  dataRefs: .status.dataRefs,
  children: .status.childrenSnapshotContentRefs
}'
```

### 2.3 ObjectKeeper

**Говорить:** следует за Snapshot (`FollowObjectWithTTL`); после удаления Snapshot удерживает content до TTL, затем Deckhouse удаляет OK → GC content/MCP/chunks.

```bash
kubectl get objectkeepers.deckhouse.io "${OK_NAME}" -o json | jq '{
  mode: .spec.mode, ttl: .spec.ttl, follow: .spec.followObjectRef
}'
```

### 2.4 ManifestCheckpoint + chunks

**Говорить:** MCP — cluster-scoped архив манифестов; chunk — internal payload (get-by-name, не list).

```bash
kubectl get manifestcheckpoints.state-snapshotter.deckhouse.io "${MCP}" -o json | jq '{
  owners: .metadata.ownerReferences,
  chunks: .status.chunks,
  totalObjects: .status.totalObjects,
  ready: (.status.conditions[] | select(.type=="Ready"))
}'
kubectl get manifestcheckpointcontentchunks.state-snapshotter.deckhouse.io "${MCP}-0" \
  -o jsonpath='{.spec.checkpointName}{" objects="}{.spec.objectsCount}{"\n"}'
```

### 2.5 ManifestCaptureRequest

**Говорить:** ephemeral; после успеха **удалён**.

```bash
SNAP_UID=$(kubectl -n "$DEMO_NS" get snapshots.state-snapshotter.deckhouse.io "${SNAP}" -o jsonpath='{.metadata.uid}')
kubectl -n "$DEMO_NS" get manifestcapturerequests.state-snapshotter.deckhouse.io "snap-${SNAP_UID}" 2>&1 \
  || echo "OK: MCR отсутствует"
```

### 2.6 VolumeCaptureRequest → VolumeSnapshotContent

**Говорить:** VCR bulk capture; после publish в `SnapshotContent.status.dataRefs[]` VCR исчезает. Артефакт — **VolumeSnapshotContent** (cluster), не namespaced VolumeSnapshot (в текущем e2e VSC без VS).

```bash
kubectl -n "$DEMO_NS" get volumecapturerequests.storage-foundation.deckhouse.io 2>/dev/null || true
kubectl get snapshotcontents.state-snapshotter.deckhouse.io "${BOUND}" -o jsonpath='{.status.dataRefs}' | jq .
# если dataRefs не пуст:
VSC=$(kubectl get snapshotcontents.state-snapshotter.deckhouse.io "${BOUND}" -o jsonpath='{.status.dataRefs[0].artifact.name}')
kubectl get volumesnapshotcontents.snapshot.storage.k8s.io "${VSC}" -o wide
```

### Схема для аудитории (нарисовать или показать граф)

```
Snapshot (ns)
  ├─[bound]─► SnapshotContent (cluster) ──► ObjectKeeper (follow+TTL)
  │              ├─[manifestCheckpointName]─► ManifestCheckpoint ──► Chunk(s)
  │              ├─[dataRefs[].artifact]────► VolumeSnapshotContent
  │              └─[childrenSnapshotContentRefs]─► child SnapshotContent
  ├─[volumeCaptureRequestName]─► VCR (временно)
  └─[childrenSnapshotRefs]─► child Snapshot (трек B)
```

---

## Этап 3 — snapshot graph (визуализация)

### Что говорить

> Граф строится из live API (`hack/snapshot-graph.sh`): logical mode — status/data refs без restore. Orange solid — artifact (chunk, VSC); orange dashed — PVC target; зелёный — child refs.

На подготовленном прогоне (из корня репозитория, **до** demo или на копии артефактов):

```bash
# Пример путей после demo-e2e:
open artifacts/<run-id>/06-root-ready/graph/root-ready.logical.svg

# Или вручную на живом ns (--chunk-as: chunk-проверка под controller SA, RBAC не меняем):
bash hack/snapshot-graph.sh --namespace "$DEMO_NS" --snapshot "$SNAP" \
  --output-dir /tmp/snapshot-graph --name live --mode logical --title "Live demo" \
  --chunk-as system:serviceaccount:d8-state-snapshotter:controller
open /tmp/snapshot-graph/live.logical.svg
```

### Что показать в SVG

- `Snap → SC → MCP → Chunk` (manifest leg)
- `SC → VSC` через `status.dataRefs[].artifact` (трек C и B)
- `Snap → VCR` только пока capture не завершён
- **Нет** `MISSING` на Chunk (иначе битый MCP или забыли `--chunk-as` для chunk-проверки)

### Техническая проверка

```bash
grep -E 'status.dataRefs|VolumeSnapshotContent|status.chunks' /tmp/snapshot-graph/live.logical.dot
```

---

## Этап 4 — aggregated manifests API

### Что говорить

> Единая точка чтения манифестов снимка — subresource, не прямой get chunk.

```bash
kubectl get --raw \
  "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/${DEMO_NS}/snapshots/${SNAP}/manifests" \
  | jq '[.[] | {kind, name: .metadata.name, ns: .metadata.namespace}] | {count: length, sample: .[0:6]}'
```

Альтернатива по MCP (cluster route):

```bash
kubectl get --raw \
  "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/manifestcheckpoints/${MCP}/manifests" \
  | jq 'length'
```

### Техническая проверка

- `count >= 1`, в списке есть ваш ConfigMap (трек A) или PVC (трек B).
- После delete Snapshot (этап 6) — URL **по имени snapshot** часто ещё отвечает, пока жив OK/content.

---

## Этап 5 — child snapshot flow (трек B)

### Что говорить

> Root агрегирует **остаточные** манифесты/PVC; child узел владеет своим scope (`pvc-a` на child, `pvc-b` на root). Связь: `childrenSnapshotRefs` / `childrenSnapshotContentRefs`.

Показать на готовом namespace (пример с кластера `demo-e2e-20260525-163832`):

```bash
export E2E_NS=demo-e2e-20260525-163832
kubectl -n "$E2E_NS" get snapshots.state-snapshotter.deckhouse.io demo-root -o json | jq '.status.childrenSnapshotRefs'
kubectl get snapshotcontents.state-snapshotter.deckhouse.io \
  $(kubectl -n "$E2E_NS" get snap demo-root -o jsonpath='{.status.boundSnapshotContentName}') \
  -o json | jq '{children: .status.childrenSnapshotContentRefs, dataRefs: .status.dataRefs}'
kubectl -n "$E2E_NS" get snapshots.state-snapshotter.deckhouse.io demo-child -o wide
```

На графе root: зелёная дуга `status.childrenSnapshotRefs`, у каждого content свои `dataRefs` на разные VSC.

**Не создавать child вручную на live** без CSD/registry — только на заранее подготовленном e2e namespace или после `demo-e2e`.

---

## Этап 6 — delete / retain / orphan

### Что говорить

> Удаляем **только** Snapshot. SnapshotContent и MCP остаются; aggregated read по старому URL работает, пока жив **ObjectKeeper**. Руками content/MCP/OK не удаляем — это «прод»-семантика. После TTL OK → GC цепочки.

```bash
kubectl -n "$DEMO_NS" delete snapshots.state-snapshotter.deckhouse.io "${SNAP}" --wait=true
kubectl get snapshotcontents.state-snapshotter.deckhouse.io "${BOUND}" -o wide
kubectl get --raw ".../namespaces/${DEMO_NS}/snapshots/${SNAP}/manifests" | jq 'length'
kubectl get objectkeepers.deckhouse.io "${OK_NAME}" -o jsonpath='{.spec.ttl}{"\n"}'
```

Через TTL+несколько секунд:

```bash
kubectl get objectkeepers.deckhouse.io "${OK_NAME}" 2>&1 || echo "OK удалён"
kubectl get snapshotcontents.state-snapshotter.deckhouse.io "${BOUND}" 2>&1 || echo "content удалён"
kubectl get manifestcheckpoints.state-snapshotter.deckhouse.io "${MCP}" 2>&1 || echo "MCP удалён"
```

### Orphan VCR

Если volume capture **не** завершился, VCR может остаться с `Ready=False` и `volumeCaptureRequestName` на Snapshot — на графе красный VCR без дуги к VSC. Это диагностический «хвост», не штатное steady state.

---

## Порядок показа (тайминг ~15–20 мин)

| Мин | Действие | Экран |
|-----|----------|-------|
| 0–2 | Предусловия, namespace, workload | `kubectl get ns,pvc,cm` |
| 0–3 | Preflight + PVC Bound + Pod (трек C) | `get pvc,pod,vsc` |
| 3–4 | `kubectl apply` Snapshot (после gate) | watch Ready |
| 4–8 | Обход цепочки 2.1–2.6 | yaml/json + схема |
| 8–10 | Graph SVG | logical graph |
| 10–12 | aggregated `/manifests` | jq count + sample |
| 12–15 | Child (трек B) | e2e namespace или артефакт |
| 15–18 | delete Snapshot + retained read | тот же URL manifests |
| 18–20 | TTL/GC (или объяснить без ожидания) | OK → content gone |

---

## Результаты ручной проверки на кластере (2026-05-29)

Прогон выполнен на dev-кластере (`d8-state-snapshotter` controller Running, RBAC chunks `yes`).

### Трек A — ошибочный прогон `snapshot-demo-live` (CM + Pending PVC)

| Шаг | Результат |
|-----|-----------|
| Namespace + CM | OK |
| PVC `local-thin` без Pod | **Pending** (WaitForFirstConsumer) — **ошибка подготовки** |
| Snapshot `demo-root` | Создан |
| MCP | Ready, chunk читается |
| VCR | **Ready=False**, `PVC … is not bound` (ожидаемо при Pending) |
| SnapshotContent | Ready, **dataRefs=null** |
| Snapshot Ready | **Флапает** (см. Known pitfalls) |
| Graph | MCP leg OK; красный orphan VCR |
| Delete Snapshot | retain на этом прогоне **не использовать** для показа |

### Трек A — корректный manifest-only (`demo-manifest-only`, без PVC)

| Шаг | Результат |
|-----|-----------|
| Только ConfigMap | OK |
| VCR | **не создаётся** |
| Snapshot Ready | **True**, стабильно |
| Delete Snapshot | **SnapshotContent + ObjectKeeper остались** сразу после delete; aggregated read работает |

### Трек C — volume happy path (`snapshot-demo-volume`, 2026-05-19)

Ручной прогон по блоку **Preflight для volume demo** → `spec: {}` Snapshot `demo-volume`:

| Критерий | Результат |
|----------|-----------|
| Preflight | чистый ns; SC `local-thin` + VSC `sds-local-volume-snapshot-class`; PVC **Bound** ~4 с после bind pod |
| Snapshot Ready | **True**, reason `Completed` |
| SnapshotContent | `ns-bf29d43ae01c4ff593a2ca3595912e49`, **Ready=True** |
| MCP | `mcp-501c896176d3f3b3`, **Ready=True** |
| VCR после handoff | **отсутствует** (`volumeCaptureRequestName` пусто) |
| `dataRefs[]` | VSC `snapshot-51ade09c-…` → `demo-pvc` |
| VSC `readyToUse` | **true** |
| Graph (`--mode smoke`) | `status.dataRefs[].artifact` → VSC; `status.dataRefs[].target` → PVC; chunk OK |
| Aggregated manifests | **length=5** |

Namespace оставлен на кластере для повторного показа graph/aggregated; перед live — пересоздать ns или пройти preflight заново.

### Трек C — эталон e2e child (`demo-e2e-20260525-163832`, `03-child-ready`)

Полный volume leg также подтверждён на child `demo-child` (PVC `pvc-a` Bound + bind pod):

| Критерий | Результат |
|----------|-----------|
| Snapshot Ready | **True** |
| SnapshotContent Ready | **True** |
| MCP Ready | **True** |
| VCR после handoff | **отсутствует** |
| `dataRefs[]` | VSC `snapshot-9c7a300c-…` → pvc-a |
| VSC `readyToUse` | **true** |
| Graph | `status.dataRefs[].artifact`, VSC node |
| Aggregated manifests | **length > 0** |

### Трек B — `demo-e2e-20260525-163832` (полный N5)

| Ресурс | Результат |
|--------|-----------|
| `demo-root` / `demo-child` | оба **Ready=True** |
| Root content | `dataRefs` → VSC `snapshot-5d00…` → **pvc-b**; child content ref |
| Child content | `dataRefs` → VSC `snapshot-9c7a…` → **pvc-a** |
| VCR / MCR | отсутствуют (после handoff) |
| VolumeSnapshot (namespaced) | **нет** в namespace — только **VolumeSnapshotContent** |
| Aggregated root manifests | **length > 0** (проверено) |
| Graph `e2e-root.logical` | 4× `status.dataRefs`, 2× VSC, child ref, без MISSING chunk |

### Выводы для live demo

**Работает стабильно**

- Manifest-only (без PVC): Snapshot → SC → MCP → chunk → aggregated API; retain сразу после delete.
- Volume-only (трек C): preflight + Bound PVC → `dataRefs[]` + VSC `readyToUse=true`; graph volume edges.
- Полный N5 на `demo-e2e-*`: child tree + `dataRefs[]` + VSC.
- `hack/snapshot-graph.sh` logical mode: manifest и volume рёбра.

**Перед показом**

См. **Known pitfalls**, **Preflight для volume demo**, **Do not do this during live demo**. Отдельные ns для A и C; Snapshot только после Bound PVC.

### Трек D — optional full demo (`snapshot-demo-full`, 2026-05-19)

**Не использовать в live** пока не пройден успешный **D0**. Трек C для зала достаточен.

| Шаг | Результат до fix webhook RBAC | Ожидание после redeploy + D0 |
|-----|------------------------------|------------------------------|
| CSD `demo-live-vm-disk` | Accepted, AccessGranted, Ready | то же |
| Demo VM/Disk | созданы | то же |
| MCR в ns | **нет** (webhook denied) | появляются, затем handoff |
| Child SC | mcp пусто | `manifestCheckpointName` set, MCP Ready |
| Root Snapshot | `SubtreeManifestCapturePending` | `Ready=True` (без PVC: без `dataRefs`) |

**Blocker (2026-05-29):** не controller SA — admission `d8-state-snapshotter-mcr-validation` отклоняет MCR: `DemoVirtualDisk not found in namespace` при существующем disk → **webhook SA** без `get` на `demovirtualmachines` / `demovirtualdisks`. Fix: `templates/webhooks/rbac-for-us.yaml` (`get/list/watch`) + redeploy. Controller demo rules — `templates/controller/rbac-for-us.yaml` (не smoke CR).

**D0 gate (короткий прогон, без долгого wait):**

1. Preflight webhook SA: оба `get` → `yes` (см. таблицу предусловий).
2. Чистый `snapshot-demo-full`, **без PVC**; CSD Ready; root Snapshot.
3. `kubectl -n snapshot-demo-full get mcr` — не пусто; child content `.status.manifestCheckpointName` не пуст; MCP Ready.

**D1** — только после успешного D0 (volume leg отдельно).

### Что подготовить заранее

1. **Live:** трек C — `snapshot-demo-volume`, preflight + `local-thin` + bind pod (см. **Preflight для volume demo**).
2. Трек B: `hack/demo-e2e.sh` + `DEMO_E2E_SKIP_CLEANUP=1` и артефакты `06-root-ready/graph/*.svg`.
3. Трек A: `snapshot-demo-manifest`, только ConfigMap.
4. Трек D (опционально): только после D0 + redeploy controller RBAC; команды в [`snapshot-live-demo-commands.md`](snapshot-live-demo-commands.md).
5. RBAC: chunks читаются графом под controller SA (`--chunk-as system:serviceaccount:d8-state-snapshotter:controller`), admin-kubeconfig прямого `get` на `manifestcheckpointcontentchunks` не имеет (by design); для D — controller SA preflight в commands § D0.
6. Слайд «не показываем»: restore, list chunk, VolumeSnapshot restore intent.

---

## Связанные материалы

| Документ | Назначение |
|----------|------------|
| [`snapshot-manual-demo.md`](snapshot-manual-demo.md) | Линейный N2a в `default` |
| [`e2e-testing-strategy.md`](e2e-testing-strategy.md) | `hack/demo-e2e.sh`, артефакты |
| `artifacts/<run-id>/06-root-ready/graph/` | Готовые SVG после e2e |
