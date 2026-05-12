# Pre-E2E Smoke Validation

Ручной smoke-checklist через `kubectl` и `curl` перед полноценным e2e.

Цель проверки — не совпадение generated names, а модель:

```text
CSD -> snapshot graph -> sourceRef -> MCP -> aggregated read
```

В текущем v0 есть один runtime mode: `SnapshotController`, common `SnapshotContentController`, CSD reconciler, graph registry, generic runtime syncer and dynamic hot-add watches always enabled.

Все имена child snapshots получайте из `status.childrenSnapshotRefs`. Не используйте фиксированные имена вроде `DemoVirtualMachineSnapshot/vm-1` или `DemoVirtualDiskSnapshot/disk-vm`.

## 0. Контекст и переменные

```shell
kubectl cluster-info
kubectl get ns d8-state-snapshotter
kubectl get pods -n d8-state-snapshotter -o wide
kubectl get deploy -n d8-state-snapshotter

NS=nss-smoke
CTRL_NS=d8-state-snapshotter
CTRL_DEPLOY=controller
```

Для одноразового прогона можно использовать фиксированный `NS=nss-smoke`. Для
частых повторных прогонов лучше перейти на уникальное имя, чтобы retained
`ObjectKeeper` / `SnapshotContent` от предыдущего run не влияли на новый run:

```shell
NS="nss-smoke-$(date +%Y%m%d%H%M%S)"
```

Если в конкретном окружении deployment называется иначе, выставьте `CTRL_DEPLOY` по выводу `kubectl get deploy -n "$CTRL_NS"`.

Для JSON-проверок ниже нужен `jq`.

```shell
jq --version
```

Smoke graph artifacts are written under `ARTIFACT_DIR`:

```shell
ARTIFACT_DIR="${ARTIFACT_DIR:-/tmp/state-snapshotter-smoke-artifacts-$(date +%Y%m%d-%H%M%S)}"
mkdir -p "$ARTIFACT_DIR"
echo "Artifacts: $ARTIFACT_DIR"
```

The graph helper requires `kubectl` and `jq`. If Graphviz `dot` is installed it also renders SVG; otherwise it keeps `.dot`, `.objects.yaml`, and `.summary.txt` and prints a warning without failing smoke.

Subresource API опубликован как Kubernetes aggregated APIService. В smoke вызывайте его через kube-apiserver:

```shell
API_PATH_BASE="/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/$NS"
```

Direct `port-forward` на `svc/controller` не является обычным smoke path: controller API работает по HTTPS/mTLS и требует front-proxy client certificate с допустимым CN (`system:kube-apiserver`, `kubernetes`, `front-proxy-client`). Для ручного `curl` используйте kubeconfig client cert против Kubernetes API server, а не прямой Service controller.

Для справки проверьте Service/APIService:

```shell
kubectl -n "$CTRL_NS" get svc
kubectl get apiservice v1alpha1.subresources.state-snapshotter.deckhouse.io
```

Пример `curl` через kube-apiserver (если нужен именно `curl`; `kubectl get --raw` предпочтительнее):

```shell
CTX=$(kubectl config current-context)
CLUSTER=$(kubectl config view --raw -o jsonpath="{.contexts[?(@.name=='$CTX')].context.cluster}")
USER=$(kubectl config view --raw -o jsonpath="{.contexts[?(@.name=='$CTX')].context.user}")
SERVER=$(kubectl config view --raw -o jsonpath="{.clusters[?(@.name=='$CLUSTER')].cluster.server}")
TMP_CERT_DIR=$(mktemp -d /tmp/ss-smoke-certs.XXXXXX)

kubectl config view --raw -o jsonpath="{.clusters[?(@.name=='$CLUSTER')].cluster.certificate-authority-data}" \
  | base64 -d > "$TMP_CERT_DIR/ca.crt"
kubectl config view --raw -o jsonpath="{.users[?(@.name=='$USER')].user.client-certificate-data}" \
  | base64 -d > "$TMP_CERT_DIR/client.crt"
kubectl config view --raw -o jsonpath="{.users[?(@.name=='$USER')].user.client-key-data}" \
  | base64 -d > "$TMP_CERT_DIR/client.key"

curl --cacert "$TMP_CERT_DIR/ca.crt" \
  --cert "$TMP_CERT_DIR/client.crt" \
  --key "$TMP_CERT_DIR/client.key" \
  "$SERVER$API_PATH_BASE/snapshots/root-no-csd/manifests"
```

## 1. CRD установлены

```shell
kubectl get crd | grep -E 'snapshots|snapshotcontents|demovirtual|customsnapshotdefinitions|manifestcapture|manifestcheckpoints'
kubectl get crd customsnapshotdefinitions.state-snapshotter.deckhouse.io

kubectl get crd domainspecificsnapshotcontrollers.state-snapshotter.deckhouse.io 2>/dev/null && {
  echo "unexpected legacy custom snapshot definition CRD exists" >&2
  exit 1
} || true
```

Ожидаемо есть CRD для:

- `Snapshot` / `SnapshotContent`;
- `ManifestCaptureRequest` / `ManifestCheckpoint`;
- `CustomSnapshotDefinition`;
- demo VM/Disk resources and snapshots.

Dedicated SnapshotContent / Demo*SnapshotContent CRDs are not expected in the cluster.
Only common `storage.deckhouse.io/SnapshotContent` is expected:

```shell
kubectl get crd snapshotcontents.storage.deckhouse.io

kubectl get crd snapshotcontents.storage.deckhouse.io 2>/dev/null && {
  echo "unexpected legacy SnapshotContent CRD exists" >&2
  exit 1
} || true
kubectl get crd demovirtualmachinesnapshotcontents.demo.state-snapshotter.deckhouse.io 2>/dev/null && {
  echo "unexpected legacy DemoVirtualMachineSnapshotContent CRD exists" >&2
  exit 1
} || true
kubectl get crd demovirtualdisksnapshotcontents.demo.state-snapshotter.deckhouse.io 2>/dev/null && {
  echo "unexpected legacy DemoVirtualDiskSnapshotContent CRD exists" >&2
  exit 1
} || true
```

After applying current manifests to a real cluster that previously had the old API installed, delete the superseded CRD explicitly:

```shell
kubectl delete crd domainspecificsnapshotcontrollers.state-snapshotter.deckhouse.io --ignore-not-found
```

## 2. Schema `childrenSnapshotRefs`: без namespace

```shell
kubectl explain snapshot.status.childrenSnapshotRefs
kubectl explain snapshot.status.childrenSnapshotRefs.apiVersion
kubectl explain snapshot.status.childrenSnapshotRefs.kind
kubectl explain snapshot.status.childrenSnapshotRefs.name

# Ожидаемо поле не существует:
kubectl explain snapshot.status.childrenSnapshotRefs.namespace

# Ожидаемо поле не существует:
kubectl explain snapshotcontent.spec.snapshotRef && {
  echo "unexpected SnapshotContent.spec.snapshotRef exists" >&2
  exit 1
} || true
```

`childrenSnapshotRefs` содержит только `apiVersion`, `kind`, `name`; namespace child snapshot не хранится и не должен появляться в schema. `SnapshotContent.spec.snapshotRef` также не должен существовать: retained content self-contained и не имеет live reverse dependency на snapshot.

## 3. Логи контроллера до smoke

```shell
kubectl logs -n "$CTRL_NS" deploy/"$CTRL_DEPLOY" --tail=300 \
  | grep -Ei 'panic|fatal|stacktrace|error' || true
```

Ожидаемо нет новых `panic`, `fatal`, `stacktrace` и повторяющихся reconcile error loop. Старые строки leader election оценивайте отдельно, они не равны падению приложения.

## 4. Helpers для polling и ref discovery

Предпочитайте polling через `kubectl -o json | jq` вместо `kubectl wait`, если заранее не подтверждено, что `kubectl wait` корректно работает для данного CRD.

```shell
wait_snapshot_ready() {
  local kind="$1"
  local name="$2"
  local timeout="${3:-120}"
  local elapsed=0
  until kubectl -n "$NS" get "$kind" "$name" -o json \
    | jq -e '.status.conditions[]? | select(.type=="Ready" and .status=="True")' >/dev/null; do
    if [ "$elapsed" -ge "$timeout" ]; then
      echo "timeout waiting for $kind/$name Ready=True" >&2
      kubectl -n "$NS" get "$kind" "$name" -o yaml >&2 || true
      return 1
    fi
    sleep 2
    elapsed=$((elapsed + 2))
  done
}

wait_content_mcp() {
  local content="$1"
  local timeout="${2:-120}"
  local elapsed=0
  until kubectl get snapshotcontent "$content" -o json \
    | jq -e '.status.manifestCheckpointName | select(. != null and . != "")' >/dev/null; do
    if [ "$elapsed" -ge "$timeout" ]; then
      echo "timeout waiting for SnapshotContent/$content manifestCheckpointName" >&2
      kubectl get snapshotcontent "$content" -o yaml >&2 || true
      return 1
    fi
    sleep 2
    elapsed=$((elapsed + 2))
  done
}

wait_content_ready() {
  local content="$1"
  local timeout="${2:-120}"
  local elapsed=0
  until kubectl get snapshotcontent "$content" -o json \
    | jq -e '.status.conditions[]? | select(.type=="Ready" and .status=="True")' >/dev/null; do
    if [ "$elapsed" -ge "$timeout" ]; then
      echo "timeout waiting for SnapshotContent/$content Ready=True" >&2
      kubectl get snapshotcontent "$content" -o yaml >&2 || true
      return 1
    fi
    sleep 2
    elapsed=$((elapsed + 2))
  done
}

child_ref_name() {
  local parent_kind="$1"
  local parent_name="$2"
  local child_kind="$3"
  kubectl -n "$NS" get "$parent_kind" "$parent_name" -o json \
    | jq -r --arg kind "$child_kind" '.status.childrenSnapshotRefs[]? | select(.kind==$kind) | .name' \
    | head -n 1
}

# Apply test-only domain RBAC before setting RBACReady=True. If the
# project/environment already has a helper or hook that makes a CSD
# RBACReady/eligible, use that instead. This manual status replace is only
# a smoke fallback and preserves existing conditions such as Accepted.
mark_csd_rbac_ready() {
  local name="$1"
  local gen now
  gen=$(kubectl get customsnapshotdefinition "$name" -o jsonpath='{.metadata.generation}')
  now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  kubectl get customsnapshotdefinition "$name" -o json \
    | jq --argjson gen "$gen" --arg now "$now" '
        .status.conditions =
          ((.status.conditions // []) | map(select(.type != "RBACReady")) + [{
            "type": "RBACReady",
            "status": "True",
            "reason": "Smoke",
            "message": "manual smoke approval",
            "lastTransitionTime": $now,
            "observedGeneration": $gen
          }])
      ' \
    | kubectl replace --subresource=status -f -
}

snapshot_graph_artifact() {
  local snapshot="$1"
  local name="$2"
  local title="${3:-$name}"
  local description="${4:-$title}"
  hack/snapshot-graph.sh \
    --namespace "$NS" \
    --snapshot "$snapshot" \
    --output-dir "$ARTIFACT_DIR" \
    --name "$name" \
    --mode lifecycle \
    --title "$title" \
    --description "$description"
  hack/snapshot-graph.sh \
    --namespace "$NS" \
    --snapshot "$snapshot" \
    --output-dir "$ARTIFACT_DIR" \
    --name "$name" \
    --mode logical \
    --title "$title" \
    --description "$description"
}

snapshotcontent_graph_artifact() {
  local content="$1"
  local name="$2"
  local title="${3:-$name}"
  local description="${4:-$title}"
  hack/snapshot-graph.sh \
    --snapshotcontent "$content" \
    --output-dir "$ARTIFACT_DIR" \
    --name "$name" \
    --mode lifecycle \
    --title "$title" \
    --description "$description"
}

assert_source_ref() {
  local kind="$1"
  local name="$2"
  local api_version="$3"
  local source_kind="$4"
  local source_name="$5"
  kubectl -n "$NS" get "$kind" "$name" -o json \
    | jq -e \
      --arg apiVersion "$api_version" \
      --arg kind "$source_kind" \
      --arg name "$source_name" '
        .spec.sourceRef.apiVersion == $apiVersion and
        .spec.sourceRef.kind == $kind and
        .spec.sourceRef.name == $name
      '
}

apply_demo_domain_rbac() {
  local controller_namespace="${1:-d8-state-snapshotter}"
  local controller_sa="${2:-controller}"
  cat <<EOF | kubectl apply -f -
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: state-snapshotter-smoke-demo-domain-rbac
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
  name: state-snapshotter-smoke-demo-domain-rbac
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: state-snapshotter-smoke-demo-domain-rbac
subjects:
- kind: ServiceAccount
  name: ${controller_sa}
  namespace: ${controller_namespace}
EOF

  kubectl auth can-i list demovirtualmachinesnapshots.demo.state-snapshotter.deckhouse.io \
    --as="system:serviceaccount:${controller_namespace}:${controller_sa}" --all-namespaces
  kubectl auth can-i list demovirtualdisksnapshots.demo.state-snapshotter.deckhouse.io \
    --as="system:serviceaccount:${controller_namespace}:${controller_sa}" --all-namespaces
  kubectl auth can-i create demovirtualdisksnapshots.demo.state-snapshotter.deckhouse.io \
    --as="system:serviceaccount:${controller_namespace}:${controller_sa}" -n "$NS"
}
```

Если JSONPath filtering работает стабильно в вашем `kubectl`, можно использовать короткую форму:

```shell
kubectl -n "$NS" get snapshot root-full \
  -o jsonpath='{.status.childrenSnapshotRefs[?(@.kind=="DemoVirtualMachineSnapshot")].name}'
```

Fallback через `jq` обязателен для случаев, когда массив пустой, содержит несколько элементов или JSONPath filter ведёт себя нестабильно:

```shell
kubectl -n "$NS" get snapshot root-full -o json \
  | jq -r '.status.childrenSnapshotRefs[]? | select(.kind=="DemoVirtualMachineSnapshot") | .name'
```

## 4.1 Smoke artifacts

Each graph artifact call writes compact graph images plus sidecar details. For `Snapshot` roots the helper emits both lifecycle and logical views by default:

- `<name>.lifecycle.dot` / `<name>.lifecycle.svg` — owner/lifecycle chain: `ObjectKeeper`, `Snapshot`, `SnapshotContent`, live MCR when present, MCP, archived manifest chunks;
- `<name>.logical.dot` / `<name>.logical.svg` — status/logical refs: bound content, live MCR when present, child refs, MCP/chunk refs, data refs, source refs, and `ObjectKeeper.spec.followObjectRef` when present;
- `<name>.<mode>.details.json` — full names, UIDs, ownerReferences, extracted refs, conditions, labels/annotations, warnings;
- `<name>.<mode>.objects.yaml` — raw traversed objects separated by comments;
- `<name>.<mode>.summary.txt` — counts and invariant checks;
- `<name>.<mode>.stage.txt` — the exact test-stage caption rendered at the top of the DOT/SVG.

Open the SVG from `$ARTIFACT_DIR` in a browser or image viewer. Main node labels stay compact (`SC/root-full`, `[Ready]`); full details live in `.details.json` and node tooltips in SVG. Node colors: snapshots are light blue, `SnapshotContent` is light green, `ObjectKeeper` is pink, MCR/MCP/chunks are light yellow/orange, source/domain objects are gray, and missing/problem objects have red dashed/red borders with badges such as `[MISSING]`, `[ORPHAN]`, `[BAD OWNER]`, or `[CONFLICT]`. Edges are styled by role: red bold solid `ownerRef`, blue dashed status refs, green solid child refs, orange artifact refs (`status.chunks`), gray dotted `spec.followObjectRef`, purple dashed `spec.sourceRef`, and orange `status.dataRef`. Both lifecycle and logical views use the same top-to-bottom layout. Graphs are grouped into namespaced and cluster-scoped clusters and include a compact legend.

For live `--snapshot` graphs, the helper also includes point-in-time inventory: all currently existing namespaced resources in `$NS` (except noisy events), plus cluster-scoped `CustomSnapshotDefinition` resources because they define the active graph registry context for the test. Objects already deleted before rendering are not shown. This makes smoke artifacts reflect the namespace and CSD state at the exact draw moment, not historical resources from earlier reconcile steps. Use `--no-include-namespace-resources` only for focused/debug fixture rendering.

For stricter local diagnosis, run the helper manually with `--strict`; invariant violations then return non-zero:

```shell
hack/snapshot-graph.sh \
  --namespace "$NS" \
  --snapshot root-full \
  --output-dir "$ARTIFACT_DIR" \
  --name "strict-root-full" \
  --mode full \
  --strict
```

## 5. Подготовка namespace и demo resources

```shell
kubectl delete ns "$NS" --ignore-not-found --wait=true --timeout=180s
kubectl delete customsnapshotdefinition smoke-demo-disk-only --ignore-not-found
kubectl delete customsnapshotdefinition smoke-demo-vm-disk --ignore-not-found

# Future preferred cleanup once smoke-created ObjectKeepers are labeled:
kubectl delete objectkeeper \
  -l smoke.state-snapshotter.deckhouse.io/run="$NS" \
  --ignore-not-found

# Best-effort cleanup for repeated fixed-namespace runs. Execution ObjectKeepers
# for MCR are UID-aware, so stale request keepers should not block a recreated
# same-name MCR, but cleaning old smoke artifacts keeps reports easier to read.
kubectl get objectkeepers -o name 2>/dev/null \
  | while read -r ok; do
      kubectl get "$ok" -o json 2>/dev/null \
        | jq -e --arg ns "$NS" '
            .metadata.name | startswith("ret-mcr-")
            and .spec.followObjectRef.kind == "ManifestCaptureRequest"
            and .spec.followObjectRef.namespace == $ns
          ' >/dev/null \
        && kubectl delete "$ok"
    done

kubectl create ns "$NS"

kubectl -n "$NS" create configmap smoke-cm \
  --from-literal=key=value

cat <<'EOF' | kubectl -n "$NS" apply -f -
apiVersion: demo.state-snapshotter.deckhouse.io/v1alpha1
kind: DemoVirtualMachine
metadata:
  name: vm-1
spec: {}
EOF

VM_UID=$(kubectl -n "$NS" get demovirtualmachine vm-1 -o jsonpath='{.metadata.uid}')

cat <<EOF | kubectl -n "$NS" apply -f -
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
```

PVC/VCR в этот smoke не добавляйте.

v0 common-content note: this smoke validates only `storage.deckhouse.io/SnapshotContent`
as the active content resource. Dedicated `SnapshotContent` /
`Demo*SnapshotContent` CRDs are not expected in the cluster. Only common
`storage.deckhouse.io/SnapshotContent` is expected.

Для повторного прогона с теми же именами учитывайте Retain/ObjectKeeper модель: старые `SnapshotContent` и `ObjectKeeper` могут ещё существовать в `Expiring`. Execution ObjectKeepers for MCR are UID-aware: recreating an MCR with the same name creates a different `ret-mcr-*` ObjectKeeper, so stale request keepers should not block the new request. Это допустимо, если новый run сходится и в логах нет устойчивого error loop. Возможен transient reconcile error вида `ObjectKeeper ... already exists` для `ret-snap-nss-smoke-*`; фиксируйте его в отчёте, но не считайте блокером без повторяющейся деградации.

## 6. CSD registration: базовый flow без CSD

Создайте root snapshot без demo CSD:

```shell
cat <<EOF | kubectl apply -f -
apiVersion: storage.deckhouse.io/v1alpha1
kind: Snapshot
metadata:
  name: root-no-csd
  namespace: ${NS}
spec: {}
EOF

wait_snapshot_ready snapshot root-no-csd 120
```

Проверка binding root -> content:

```shell
ROOT_NO_CSD_CONTENT=$(kubectl -n "$NS" get snapshot root-no-csd -o jsonpath='{.status.boundSnapshotContentName}')
test -n "$ROOT_NO_CSD_CONTENT"
kubectl get snapshotcontent "$ROOT_NO_CSD_CONTENT" -o yaml
ROOT_NO_CSD_OK=$(kubectl get objectkeepers -o json \
  | jq -r --arg ns "$NS" --arg name "root-no-csd" '
      .items[]
      | select(.spec.followObjectRef.kind=="Snapshot")
      | select(.spec.followObjectRef.namespace==$ns)
      | select(.spec.followObjectRef.name==$name)
      | .metadata.name
    ' | head -n 1)
test -n "$ROOT_NO_CSD_OK"
kubectl get objectkeeper "$ROOT_NO_CSD_OK" -o json \
  | jq -e '(.metadata.ownerReferences // []) | length == 0'
kubectl -n "$NS" get snapshot root-no-csd -o json \
  | jq -e --arg ok "$ROOT_NO_CSD_OK" 'all(.metadata.ownerReferences[]?; .kind!="ObjectKeeper" or .name!=$ok)'
kubectl get snapshotcontent "$ROOT_NO_CSD_CONTENT" -o json \
  | jq -e --arg ok "$ROOT_NO_CSD_OK" '.metadata.ownerReferences[]? | select(.kind=="ObjectKeeper" and .name==$ok)'

wait_content_mcp "$ROOT_NO_CSD_CONTENT" 120
wait_content_ready "$ROOT_NO_CSD_CONTENT" 180
ROOT_NO_CSD_MCP=$(kubectl get snapshotcontent "$ROOT_NO_CSD_CONTENT" -o jsonpath='{.status.manifestCheckpointName}')
kubectl get manifestcheckpoint "$ROOT_NO_CSD_MCP" -o yaml

snapshot_graph_artifact \
  root-no-csd \
  "01-root-no-csd-ready" \
  "01 root-no-csd ready" \
  "CSD registration baseline: no DemoVirtualMachine/DemoVirtualDisk CSD is registered; the root Snapshot is Ready and source objects are included only as live namespace inventory/source refs."
```

Ожидаемо:

- `Snapshot/root-no-csd` имеет `Ready=True Completed`;
- `status.boundSnapshotContentName` установлен;
- `SnapshotContent.status.manifestCheckpointName` всегда установлен;
- если root own scope пустой, MCP существует и содержит `0` objects;
- `childrenSnapshotRefs` пустой, потому что demo kinds не активированы в graph registry без eligible CSD.
- root MCR не содержит `v1/Namespace`; cluster-scoped targets в MCR запрещены.

```shell
kubectl -n "$NS" get snapshot root-no-csd -o json \
  | jq -e '(.status.childrenSnapshotRefs // []) | length == 0'
```

Aggregated read через legacy/generic `snapshots` route:

TODO: retained manifest read API. This route is a live Snapshot route. After the Snapshot is deleted, the long-term contract should return `404 Snapshot not found` here and read retained manifests through durable `/snapshotcontents/{contentName}/manifests`. Current deleted-Snapshot-name resolution through root ObjectKeeper is temporary behavior / implementation detail and should not be used as the long-term retained read identifier.

```shell
kubectl get --raw \
  "$API_PATH_BASE/snapshots/root-no-csd/manifests" \
  | tee /tmp/root-no-csd-manifests.json \
  | jq .
```

Ожидаемо:

- response содержит root scope namespace-scoped allowlist objects (например `ConfigMap/smoke-cm`, `ServiceAccount/default`, RBAC objects);
- response не содержит Kubernetes `Namespace` object;
- response namespace-relative: у namespaced objects нет `metadata.namespace`;
- response не содержит demo child domain objects (`DemoVirtualMachine`, `DemoVirtualDisk`), потому что demo kinds не активированы в graph registry без eligible CSD.

## 7. Test-only domain RBAC emulation

Production target model: RBAC for domain/custom snapshot resources is granted by an external Deckhouse RBAC controller/hook. The state-snapshotter controller does not grant these permissions to itself, and static production RBAC stays domain-agnostic.

In real-cluster smoke/e2e, apply explicit test-only RBAC before setting any CSD `RBACReady=True`. This emulates the external RBAC controller. If the controller is started or restarted during smoke, apply this RBAC before that start/restart whenever possible; otherwise cache sync for demo informers can fail with `forbidden` list/watch errors. Keep this RBAC applied until smoke is finished if the controller may restart.

```shell
apply_demo_domain_rbac "$CTRL_NS" controller
```

Invariant for the rest of this smoke: `RBACReady=True` means the test-only RBAC above is already effective.

## 8. CSD eligibility: Disk-only CSD + ownerRef filtering

Создайте eligible CSD только для disk snapshot kind:

```shell
cat <<'EOF' | kubectl apply -f -
apiVersion: state-snapshotter.deckhouse.io/v1alpha1
kind: CustomSnapshotDefinition
metadata:
  name: smoke-demo-disk-only
spec:
  ownerModule: smoke
  snapshotResourceMapping:
  - resourceCRDName: demovirtualdisks.demo.state-snapshotter.deckhouse.io
    snapshotCRDName: demovirtualdisksnapshots.demo.state-snapshotter.deckhouse.io
EOF
```

Дождитесь `Accepted=True`, затем выставьте `RBACReady=True` тем же способом, который используется в текущем окружении (hook/controller/manual patch для smoke). Перед этим test-only RBAC из раздела 7 уже должен быть применён. Manual patch должен сохранять существующие conditions, включая `Accepted`; не заменяйте массив `status.conditions` целиком.

```shell
until kubectl get customsnapshotdefinition smoke-demo-disk-only -o json \
  | jq -e '.status.conditions[]? | select(.type=="Accepted" and .status=="True")' >/dev/null; do
  sleep 2
done

mark_csd_rbac_ready smoke-demo-disk-only
```

Дождитесь, пока graph registry refresh увидит CSD (обычно через reconcile/logs; при необходимости перезапустите controller только если это предусмотрено текущим runbook). This is the CSD dynamic watch activation boundary: after eligibility, new root snapshots may see the newly registered snapshot kind without a controller restart.

Создайте snapshot:

```shell
cat <<EOF | kubectl apply -f -
apiVersion: storage.deckhouse.io/v1alpha1
kind: Snapshot
metadata:
  name: root-disk-only
  namespace: ${NS}
spec: {}
EOF

wait_snapshot_ready snapshot root-disk-only 180

snapshot_graph_artifact \
  root-disk-only \
  "02-disk-only-ready" \
  "02 disk-only ready" \
  "CSD eligibility: only the disk CSD is registered; disk snapshots are eligible, while VM remains a source/inventory object without custom snapshot expansion."
```

Ожидаемо детерминированно:

- `disk-vm` с `ownerReference -> DemoVirtualMachine/vm-1` не становится direct child root;
- `disk-vm` не представлен в `root.childrenSnapshotRefs`;
- root own MCP не включает `disk-vm`;
- root aggregated read не включает `disk-vm`;
- `disk-standalone` без ownerRef при disk-only CSD становится top-level child.

Получите generated disk child refs из root:

```shell
kubectl -n "$NS" get snapshot root-disk-only -o json \
  | jq '.status.childrenSnapshotRefs // []'

DISK_ONLY_CHILDREN=$(kubectl -n "$NS" get snapshot root-disk-only -o json \
  | jq -r '.status.childrenSnapshotRefs[]? | select(.kind=="DemoVirtualDiskSnapshot") | .name')

test -n "$DISK_ONLY_CHILDREN"
```

Для каждого direct disk child проверьте snapshot и subtree:

```shell
for child in $DISK_ONLY_CHILDREN; do
  kubectl -n "$NS" get demovirtualdisksnapshot "$child" -o yaml
  assert_source_ref \
    demovirtualdisksnapshot "$child" \
    demo.state-snapshotter.deckhouse.io/v1alpha1 \
    DemoVirtualDisk \
    disk-standalone
done
```

Проверка root aggregated read:

```shell
kubectl get --raw \
  "$API_PATH_BASE/snapshots/root-disk-only/manifests" \
  | tee /tmp/root-disk-only-manifests.json \
  | jq .
```

Ожидаемо в `/tmp/root-disk-only-manifests.json` нет VM-owned `DemoVirtualDisk/disk-vm` ни как direct child subtree, ни как root MCP payload. Direct disk child MCP должен содержать `DemoVirtualDisk/disk-standalone`.

## 9. CSD graph activation: VM + Disk CSD полный parent/child graph

Disk-only CSD удаляем перед VM+Disk CSD, чтобы не получить ожидаемый `KindConflict` на `DemoVirtualDiskSnapshot` mapping.

```shell
kubectl delete customsnapshotdefinition smoke-demo-disk-only --ignore-not-found
```

Создайте или обновите CSD так, чтобы eligible mappings включали VM и Disk:

```shell
cat <<'EOF' | kubectl apply -f -
apiVersion: state-snapshotter.deckhouse.io/v1alpha1
kind: CustomSnapshotDefinition
metadata:
  name: smoke-demo-vm-disk
spec:
  ownerModule: smoke
  snapshotResourceMapping:
  - resourceCRDName: demovirtualmachines.demo.state-snapshotter.deckhouse.io
    snapshotCRDName: demovirtualmachinesnapshots.demo.state-snapshotter.deckhouse.io
  - resourceCRDName: demovirtualdisks.demo.state-snapshotter.deckhouse.io
    snapshotCRDName: demovirtualdisksnapshots.demo.state-snapshotter.deckhouse.io
EOF
```

Доведите CSD до eligible состояния (`Accepted=True`, `RBACReady=True` с актуальным `observedGeneration`) тем же способом, что в предыдущем разделе. Test-only RBAC из раздела 7 должен оставаться применённым до конца smoke:

```shell
until kubectl get customsnapshotdefinition smoke-demo-vm-disk -o json \
  | jq -e '.status.conditions[]? | select(.type=="Accepted" and .status=="True")' >/dev/null; do
  sleep 2
done

mark_csd_rbac_ready smoke-demo-vm-disk
```

This step also validates CSD dynamic watch activation for the full demo graph: after VM+Disk CSD eligibility, newly created root snapshots discover VM children and VM snapshots discover Disk children through CSD-backed graph registry state.

Создайте snapshot:

```shell
cat <<EOF | kubectl apply -f -
apiVersion: storage.deckhouse.io/v1alpha1
kind: Snapshot
metadata:
  name: root-full
  namespace: ${NS}
spec: {}
EOF

wait_snapshot_ready snapshot root-full 240
```

Получите generated refs. Сначала root -> VM:

```shell
CHILD_VM=$(child_ref_name snapshot root-full DemoVirtualMachineSnapshot)
test -n "$CHILD_VM"
kubectl -n "$NS" get demovirtualmachinesnapshot "$CHILD_VM" -o yaml
assert_source_ref \
  demovirtualmachinesnapshot "$CHILD_VM" \
  demo.state-snapshotter.deckhouse.io/v1alpha1 \
  DemoVirtualMachine \
  vm-1
wait_snapshot_ready demovirtualmachinesnapshot "$CHILD_VM" 180
```

Потом VM -> VM-owned Disk:

```shell
CHILD_DISK=$(child_ref_name demovirtualmachinesnapshot "$CHILD_VM" DemoVirtualDiskSnapshot)
test -n "$CHILD_DISK"
kubectl -n "$NS" get demovirtualdisksnapshot "$CHILD_DISK" -o yaml
assert_source_ref \
  demovirtualdisksnapshot "$CHILD_DISK" \
  demo.state-snapshotter.deckhouse.io/v1alpha1 \
  DemoVirtualDisk \
  disk-vm
wait_snapshot_ready demovirtualdisksnapshot "$CHILD_DISK" 180
```

Проверка direct root refs:

```shell
kubectl -n "$NS" get snapshot root-full -o json \
  | jq '.status.childrenSnapshotRefs // []'

# Root должен иметь VM child.
kubectl -n "$NS" get snapshot root-full -o json \
  | jq -e '.status.childrenSnapshotRefs[]? | select(.kind=="DemoVirtualMachineSnapshot")'

# Root не должен иметь direct child для VM-owned disk-vm.
# Direct DemoVirtualDiskSnapshot child допустим только для standalone disk без ownerRef.
ROOT_DISK_CHILDREN=$(kubectl -n "$NS" get snapshot root-full -o json \
  | jq -r '.status.childrenSnapshotRefs[]? | select(.kind=="DemoVirtualDiskSnapshot") | .name')
for child in $ROOT_DISK_CHILDREN; do
  kubectl -n "$NS" get demovirtualdisksnapshot "$child" -o yaml
done
```

Content checks:

```shell
ROOT_FULL_CONTENT=$(kubectl -n "$NS" get snapshot root-full -o jsonpath='{.status.boundSnapshotContentName}')
wait_content_mcp "$ROOT_FULL_CONTENT" 180
wait_content_ready "$ROOT_FULL_CONTENT" 180
kubectl get snapshotcontent "$ROOT_FULL_CONTENT" -o json \
  | jq '.status.manifestCheckpointName, .status.childrenSnapshotContentRefs'
kubectl get snapshotcontent "$ROOT_FULL_CONTENT" -o json \
  | jq -e '.status.childrenSnapshotContentRefs | length >= 1'

VM_CONTENT=$(kubectl -n "$NS" get demovirtualmachinesnapshot "$CHILD_VM" -o jsonpath='{.status.boundSnapshotContentName}')
wait_content_mcp "$VM_CONTENT" 180
wait_content_ready "$VM_CONTENT" 180
kubectl get snapshotcontent "$VM_CONTENT" -o json \
  | jq '.status.manifestCheckpointName, .status.childrenSnapshotContentRefs'
kubectl get snapshotcontent "$VM_CONTENT" -o json \
  | jq -e '.status.childrenSnapshotContentRefs | length >= 1'

DISK_CONTENT=$(kubectl -n "$NS" get demovirtualdisksnapshot "$CHILD_DISK" -o jsonpath='{.status.boundSnapshotContentName}')
wait_content_mcp "$DISK_CONTENT" 180
wait_content_ready "$DISK_CONTENT" 180
kubectl get snapshotcontent "$DISK_CONTENT" -o json \
  | jq '.status.manifestCheckpointName, .status.childrenSnapshotContentRefs'
kubectl get snapshotcontent "$DISK_CONTENT" -o json \
  | jq -e '(.status.childrenSnapshotContentRefs // []) | length == 0'

ROOT_FULL_OK=$(kubectl get objectkeepers -o json \
  | jq -r --arg ns "$NS" --arg name "root-full" '
      .items[]
      | select(.spec.followObjectRef.kind=="Snapshot")
      | select(.spec.followObjectRef.namespace==$ns)
      | select(.spec.followObjectRef.name==$name)
      | .metadata.name
    ' | head -n 1)
test -n "$ROOT_FULL_OK"
kubectl get objectkeeper "$ROOT_FULL_OK" -o json \
  | jq -e '(.metadata.ownerReferences // []) | length == 0'
kubectl get objectkeeper "$ROOT_FULL_OK" -o json \
  | jq -e --arg ns "$NS" '.spec.followObjectRef.kind == "Snapshot" and .spec.followObjectRef.namespace == $ns and .spec.followObjectRef.name == "root-full"'
kubectl -n "$NS" get snapshot root-full -o json \
  | jq -e --arg ok "$ROOT_FULL_OK" 'all(.metadata.ownerReferences[]?; .kind!="ObjectKeeper" or .name!=$ok)'
kubectl -n "$NS" get demovirtualmachinesnapshot "$CHILD_VM" -o json \
  | jq -e --arg parent "root-full" '.metadata.ownerReferences[]? | select(.kind=="Snapshot" and .name==$parent)'
kubectl -n "$NS" get demovirtualdisksnapshot "$CHILD_DISK" -o json \
  | jq -e --arg parent "$CHILD_VM" '.metadata.ownerReferences[]? | select(.kind=="DemoVirtualMachineSnapshot" and .name==$parent)'
kubectl get snapshotcontent "$ROOT_FULL_CONTENT" -o json \
  | jq -e --arg ok "$ROOT_FULL_OK" '.metadata.ownerReferences[]? | select(.kind=="ObjectKeeper" and .name==$ok)'
kubectl get snapshotcontent "$VM_CONTENT" -o json \
  | jq -e --arg parent "$ROOT_FULL_CONTENT" '.metadata.ownerReferences[]? | select(.kind=="SnapshotContent" and .name==$parent)'
kubectl get snapshotcontent "$DISK_CONTENT" -o json \
  | jq -e --arg parent "$VM_CONTENT" '.metadata.ownerReferences[]? | select(.kind=="SnapshotContent" and .name==$parent)'
for content in "$ROOT_FULL_CONTENT" "$VM_CONTENT" "$DISK_CONTENT"; do
  kubectl get snapshotcontent "$content" -o json \
    | jq -e 'all(.metadata.ownerReferences[]?; (.kind | endswith("Snapshot")) | not)'
done

snapshot_graph_artifact \
  root-full \
  "03-root-full-ready" \
  "03 root-full ready" \
  "CSD graph activation: VM and disk CSDs are registered; the root Snapshot is Ready with parent/child SnapshotContent, MCP, ObjectKeeper, and chunk artifacts."
```

Ожидаемо:

- snapshots имеют `Ready=True Completed`;
- content objects имеют `status.manifestCheckpointName`;
- common `SnapshotContent` objects have `Ready=True`;
- content objects с children имеют `status.childrenSnapshotContentRefs`;
- disk leaf content has empty `childrenSnapshotContentRefs`.
- root `ObjectKeeper` follows root Snapshot and has no ownerReferences;
- root Snapshot is not owned by root `ObjectKeeper`;
- root `SnapshotContent` has ownerRef to root `ObjectKeeper`;
- child Snapshots have ownerRef to their parent Snapshot;
- child `SnapshotContent` ownerRef points to parent `SnapshotContent`, never to child Snapshot.

## 10. Aggregated read API checks

Duplicate identity в namespace-relative aggregated output is checked as:

```text
apiVersion | kind | name
```

MCP storage may keep namespace for internal identity, but API output strips `metadata.namespace`.

### 9.1 Root full

```shell
kubectl get --raw \
  "$API_PATH_BASE/snapshots/root-full/manifests" \
  | tee /tmp/root-full-manifests.json \
  | jq .
```

Ожидаемо:

- response содержит root scope namespace-scoped objects (`ConfigMap/smoke-cm`, RBAC smoke artifacts, etc.);
- response содержит VM subtree: `DemoVirtualMachine/vm-1`;
- response содержит disk subtree для VM-owned disk: `DemoVirtualDisk/disk-vm`;
- response может содержать standalone disk subtree: `DemoVirtualDisk/disk-standalone`, если standalone disk есть direct root child;
- response не содержит placeholder ConfigMap payload from old demo materialization;
- нет duplicate identity `apiVersion|kind|namespace|name`.

Пример проверки duplicate identity:

```shell
jq -r '
  .[]
  | [
      (.apiVersion // ""),
      (.kind // ""),
      (.metadata.name // "")
    ]
  | @tsv
' /tmp/root-full-manifests.json | sort | uniq -d
```

Ожидаемо вывод пустой.

### 9.2 VM subtree

```shell
kubectl get --raw \
  "$API_PATH_BASE/demovirtualmachinesnapshots/$CHILD_VM/manifests" \
  | tee /tmp/vm-subtree-manifests.json \
  | jq .
```

Ожидаемо:

- содержит `DemoVirtualMachine/vm-1`;
- содержит VM-owned `DemoVirtualDisk/disk-vm`;
- не содержит root-level namespace-scoped objects such as `ConfigMap/smoke-cm`;
- не содержит standalone disk subtree.

### 9.3 Disk subtree

```shell
kubectl get --raw \
  "$API_PATH_BASE/demovirtualdisksnapshots/$CHILD_DISK/manifests" \
  | tee /tmp/disk-subtree-manifests.json \
  | jq .
```

Ожидаемо:

- содержит только `DemoVirtualDisk/disk-vm`;
- не содержит VM parent subtree;
- не содержит root-level namespace-scoped objects such as `ConfigMap/smoke-cm`.

## 11. Negative generic API checks

Для `kubectl get --raw` проверяйте Kubernetes Status `reason` либо отрендеренный `kubectl` error (`Error from server (...)`); команда вернёт non-zero на error response. Если нужен HTTP status, используйте `curl` к Kubernetes API server с kubeconfig client cert из раздела 0.

```shell
set +e
kubectl get --raw \
  "$API_PATH_BASE/demovirtualdisksnapshots/not-found/manifests" \
  > /tmp/not-found-response.txt 2>&1
status=$?
set -e

test "$status" -ne 0
grep -Eq '("reason"[[:space:]]*:[[:space:]]*"NotFound"|Error from server \(NotFound\))' /tmp/not-found-response.txt
```

```shell
set +e
kubectl get --raw \
  "$API_PATH_BASE/notasnapshots/x/manifests" \
  > /tmp/bad-request-response.txt 2>&1
status=$?
set -e

test "$status" -ne 0
grep -Eq '("reason"[[:space:]]*:[[:space:]]*"BadRequest"|Error from server \(BadRequest\))' /tmp/bad-request-response.txt
```

Duplicate `409 Conflict` можно оставить optional/manual-hard, если нет удобного ручного способа создать duplicate MCP contents.

## 12. Cleanup

```shell
kubectl -n "$NS" delete snapshot root-no-csd --ignore-not-found --wait=false
kubectl -n "$NS" delete snapshot root-disk-only --ignore-not-found --wait=false
kubectl -n "$NS" delete snapshot root-full --ignore-not-found --wait=false

if [ -n "${ROOT_FULL_CONTENT:-}" ]; then
  snapshotcontent_graph_artifact \
    "$ROOT_FULL_CONTENT" \
    "04-root-full-after-delete" \
    "04 root-full after delete" \
    "Retained read path after root Snapshot deletion: SnapshotContent, ObjectKeeper, MCP, and archived chunks remain available for diagnostics."
fi

kubectl delete customsnapshotdefinition smoke-demo-disk-only --ignore-not-found
kubectl delete customsnapshotdefinition smoke-demo-vm-disk --ignore-not-found

kubectl delete ns "$NS" --wait=false
```

Cleanup не должен требовать, чтобы вообще ничего не осталось. Текущая Retain/ObjectKeeper модель может намеренно оставлять cluster-scoped artifacts. Root `ObjectKeeper` with TTL is the lifecycle anchor; after TTL expiry / root ObjectKeeper removal, Kubernetes GC may remove root `SnapshotContent`, child content tree, and artifacts owned by contents. Snapshot deletion alone is not the retained content lifecycle anchor.

Test-only RBAC из раздела 7 можно удалить только после завершения smoke и финальной проверки controller logs. Если controller может рестартовать сразу после smoke, оставьте RBAC применённым до появления внешнего RBAC controller/hook.

```shell
# Optional after final log checks:
kubectl delete clusterrolebinding state-snapshotter-smoke-demo-domain-rbac --ignore-not-found
kubectl delete clusterrole state-snapshotter-smoke-demo-domain-rbac --ignore-not-found
```

Проверьте:

```shell
kubectl get ns "$NS" -o yaml || true
kubectl get snapshotcontents
kubectl get objectkeepers 2>/dev/null || true

kubectl logs -n "$CTRL_NS" deploy/"$CTRL_DEPLOY" --tail=500 \
  | grep -Ei 'panic|fatal|stacktrace|error' || true
```

Ожидаемо:

- нет stuck `Terminating` без понятного condition/log;
- нет неожиданных зависших finalizers;
- retained objects либо удалены, либо ожидаемо остались по текущей Retain policy;
- финальные логи без новых `panic`, `fatal`, `stacktrace` и без повторяющегося error loop;
- warning про имя finalizer (`snapshot.finalizers.deckhouse.io` should include a path) сейчас не блокирует smoke, но должен быть отражён в отчёте как follow-up, если появился.

## Definition of Done для ручного smoke

- CRD установлены.
- Schema `childrenSnapshotRefs` не содержит `namespace`.
- Без CSD `Snapshot` готов и не создаёт demo children.
- Test-only domain RBAC applied before `RBACReady=True`; controller remains restart-safe during smoke.
- Disk-only CSD: VM-owned disk не становится direct root child и не протекает в root aggregated read; standalone disk становится top-level child.
- VM+Disk CSD: root создаёт VM child, VM создаёт disk child, generated names получены через refs.
- Generated child snapshots have correct `spec.sourceRef`.
- Aggregated read работает для root / VM subtree / Disk subtree.
- Negative generic API checks возвращают ожидаемые HTTP code и Kubernetes Status `reason`.
- Cleanup не оставляет stuck finalizers/Terminating без объяснения.
