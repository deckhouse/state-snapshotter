# Pre-E2E Smoke Validation

Ручной smoke-checklist через `kubectl` и `curl` перед полноценным e2e.

Цель проверки — не совпадение generated names, а модель:

```text
DSC -> snapshot graph -> sourceRef -> MCP -> aggregated read
```

Все имена child snapshots получайте из `status.childrenSnapshotRefs`. Не используйте фиксированные имена вроде `DemoVirtualMachineSnapshot/vm-1` или `DemoVirtualDiskSnapshot/disk-vm`.

## 0. Контекст и переменные

```shell
kubectl cluster-info
kubectl get ns d8-state-snapshotter
kubectl get pods -n d8-state-snapshotter -o wide
kubectl get deploy -n d8-state-snapshotter

NS=nss-smoke
CTRL_NS=d8-state-snapshotter
CTRL_DEPLOY=state-snapshotter-controller
```

Для JSON-проверок ниже нужен `jq`.

```shell
jq --version
```

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
  "$SERVER$API_PATH_BASE/namespacesnapshots/root-no-dsc/manifests"
```

## 1. CRD установлены

```shell
kubectl get crd | grep -E 'namespacesnapshots|namespacesnapshotcontents|demovirtual|domainspecificsnapshotcontrollers|manifestcapture|manifestcheckpoints'
```

Ожидаемо есть CRD для:

- `NamespaceSnapshot` / `NamespaceSnapshotContent`;
- `ManifestCaptureRequest` / `ManifestCheckpoint`;
- `DomainSpecificSnapshotController`;
- demo VM/Disk resources, snapshots and contents.

## 2. Schema `childrenSnapshotRefs`: без namespace

```shell
kubectl explain namespacesnapshot.status.childrenSnapshotRefs
kubectl explain namespacesnapshot.status.childrenSnapshotRefs.apiVersion
kubectl explain namespacesnapshot.status.childrenSnapshotRefs.kind
kubectl explain namespacesnapshot.status.childrenSnapshotRefs.name

# Ожидаемо поле не существует:
kubectl explain namespacesnapshot.status.childrenSnapshotRefs.namespace
```

`childrenSnapshotRefs` содержит только `apiVersion`, `kind`, `name`; namespace child snapshot не хранится и не должен появляться в schema.

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
  until kubectl get namespacesnapshotcontent "$content" -o json \
    | jq -e '.status.manifestCheckpointName | select(. != null and . != "")' >/dev/null; do
    if [ "$elapsed" -ge "$timeout" ]; then
      echo "timeout waiting for NamespaceSnapshotContent/$content manifestCheckpointName" >&2
      kubectl get namespacesnapshotcontent "$content" -o yaml >&2 || true
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

# If the project/environment already has a helper or hook that makes a DSC
# RBACReady/eligible, use that instead. This manual status replace is only
# a smoke fallback and preserves existing conditions such as Accepted.
mark_dsc_rbac_ready() {
  local name="$1"
  local gen now
  gen=$(kubectl get domainspecificsnapshotcontroller "$name" -o jsonpath='{.metadata.generation}')
  now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  kubectl get domainspecificsnapshotcontroller "$name" -o json \
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
```

Если JSONPath filtering работает стабильно в вашем `kubectl`, можно использовать короткую форму:

```shell
kubectl -n "$NS" get namespacesnapshot root-full \
  -o jsonpath='{.status.childrenSnapshotRefs[?(@.kind=="DemoVirtualMachineSnapshot")].name}'
```

Fallback через `jq` обязателен для случаев, когда массив пустой, содержит несколько элементов или JSONPath filter ведёт себя нестабильно:

```shell
kubectl -n "$NS" get namespacesnapshot root-full -o json \
  | jq -r '.status.childrenSnapshotRefs[]? | select(.kind=="DemoVirtualMachineSnapshot") | .name'
```

## 5. Подготовка namespace и demo resources

```shell
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

## 6. Базовый flow без DSC

Создайте root snapshot без demo DSC:

```shell
cat <<EOF | kubectl apply -f -
apiVersion: storage.deckhouse.io/v1alpha1
kind: NamespaceSnapshot
metadata:
  name: root-no-dsc
  namespace: ${NS}
spec: {}
EOF

wait_snapshot_ready namespacesnapshot root-no-dsc 120
```

Проверка binding root -> content:

```shell
ROOT_NO_DSC_NSC=$(kubectl -n "$NS" get namespacesnapshot root-no-dsc -o jsonpath='{.status.boundSnapshotContentName}')
test -n "$ROOT_NO_DSC_NSC"
kubectl get namespacesnapshotcontent "$ROOT_NO_DSC_NSC" -o yaml

wait_content_mcp "$ROOT_NO_DSC_NSC" 120
ROOT_NO_DSC_MCP=$(kubectl get namespacesnapshotcontent "$ROOT_NO_DSC_NSC" -o jsonpath='{.status.manifestCheckpointName}')
kubectl get manifestcheckpoint "$ROOT_NO_DSC_MCP" -o yaml
```

Ожидаемо:

- `NamespaceSnapshot/root-no-dsc` имеет `Ready=True Completed`;
- `status.boundSnapshotContentName` установлен;
- `NamespaceSnapshotContent.status.manifestCheckpointName` всегда установлен;
- если root own scope пустой, MCP существует и содержит `0` objects;
- `childrenSnapshotRefs` пустой, потому что demo kinds не активированы в graph registry без eligible DSC.
- root MCR не содержит `v1/Namespace`; cluster-scoped targets в MCR запрещены.

```shell
kubectl -n "$NS" get namespacesnapshot root-no-dsc -o json \
  | jq -e '(.status.childrenSnapshotRefs // []) | length == 0'
```

Aggregated read через legacy/generic `namespacesnapshots` route:

```shell
kubectl get --raw \
  "$API_PATH_BASE/namespacesnapshots/root-no-dsc/manifests" \
  | tee /tmp/root-no-dsc-manifests.json \
  | jq .
```

Ожидаемо:

- response содержит root scope namespace-scoped allowlist objects (например `ConfigMap/smoke-cm`, `ServiceAccount/default`, RBAC objects);
- response не содержит Kubernetes `Namespace` object;
- response namespace-relative: у namespaced objects нет `metadata.namespace`;
- response не содержит demo child domain objects (`DemoVirtualMachine`, `DemoVirtualDisk`), потому что demo kinds не активированы в graph registry без eligible DSC.

## 7. Disk-only DSC + ownerRef filtering

Создайте eligible DSC только для disk snapshot kind:

```shell
cat <<'EOF' | kubectl apply -f -
apiVersion: state-snapshotter.deckhouse.io/v1alpha1
kind: DomainSpecificSnapshotController
metadata:
  name: smoke-demo-disk-only
spec:
  ownerModule: smoke
  snapshotResourceMapping:
  - resourceCRDName: demovirtualdisks.demo.state-snapshotter.deckhouse.io
    snapshotCRDName: demovirtualdisksnapshots.demo.state-snapshotter.deckhouse.io
    contentCRDName: demovirtualdisksnapshotcontents.demo.state-snapshotter.deckhouse.io
EOF
```

Дождитесь `Accepted=True`, затем выставьте `RBACReady=True` тем же способом, который используется в текущем окружении (hook/controller/manual patch для smoke). Manual patch должен сохранять существующие conditions, включая `Accepted`; не заменяйте массив `status.conditions` целиком.

```shell
until kubectl get domainspecificsnapshotcontroller smoke-demo-disk-only -o json \
  | jq -e '.status.conditions[]? | select(.type=="Accepted" and .status=="True")' >/dev/null; do
  sleep 2
done

mark_dsc_rbac_ready smoke-demo-disk-only
```

Дождитесь, пока graph registry refresh увидит DSC (обычно через reconcile/logs; при необходимости перезапустите controller только если это предусмотрено текущим runbook).

Создайте snapshot:

```shell
cat <<EOF | kubectl apply -f -
apiVersion: storage.deckhouse.io/v1alpha1
kind: NamespaceSnapshot
metadata:
  name: root-disk-only
  namespace: ${NS}
spec: {}
EOF

wait_snapshot_ready namespacesnapshot root-disk-only 180
```

Ожидаемо детерминированно:

- `disk-vm` с `ownerReference -> DemoVirtualMachine/vm-1` не становится direct child root;
- `disk-vm` не представлен в `root.childrenSnapshotRefs`;
- root own MCP не включает `disk-vm`;
- root aggregated read не включает `disk-vm`;
- `disk-standalone` без ownerRef при disk-only DSC становится top-level child.

Получите generated disk child refs из root:

```shell
kubectl -n "$NS" get namespacesnapshot root-disk-only -o json \
  | jq '.status.childrenSnapshotRefs // []'

DISK_ONLY_CHILDREN=$(kubectl -n "$NS" get namespacesnapshot root-disk-only -o json \
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
  "$API_PATH_BASE/namespacesnapshots/root-disk-only/manifests" \
  | tee /tmp/root-disk-only-manifests.json \
  | jq .
```

Ожидаемо в `/tmp/root-disk-only-manifests.json` нет VM-owned `DemoVirtualDisk/disk-vm` ни как direct child subtree, ни как root MCP payload. Direct disk child MCP должен содержать `DemoVirtualDisk/disk-standalone`.

## 8. VM + Disk DSC: полный parent/child graph

Создайте или обновите DSC так, чтобы eligible mappings включали VM и Disk:

```shell
cat <<'EOF' | kubectl apply -f -
apiVersion: state-snapshotter.deckhouse.io/v1alpha1
kind: DomainSpecificSnapshotController
metadata:
  name: smoke-demo-vm-disk
spec:
  ownerModule: smoke
  snapshotResourceMapping:
  - resourceCRDName: demovirtualmachines.demo.state-snapshotter.deckhouse.io
    snapshotCRDName: demovirtualmachinesnapshots.demo.state-snapshotter.deckhouse.io
    contentCRDName: demovirtualmachinesnapshotcontents.demo.state-snapshotter.deckhouse.io
  - resourceCRDName: demovirtualdisks.demo.state-snapshotter.deckhouse.io
    snapshotCRDName: demovirtualdisksnapshots.demo.state-snapshotter.deckhouse.io
    contentCRDName: demovirtualdisksnapshotcontents.demo.state-snapshotter.deckhouse.io
EOF
```

Доведите DSC до eligible состояния (`Accepted=True`, `RBACReady=True` с актуальным `observedGeneration`) тем же способом, что в предыдущем разделе:

```shell
until kubectl get domainspecificsnapshotcontroller smoke-demo-vm-disk -o json \
  | jq -e '.status.conditions[]? | select(.type=="Accepted" and .status=="True")' >/dev/null; do
  sleep 2
done

mark_dsc_rbac_ready smoke-demo-vm-disk
```

Создайте snapshot:

```shell
cat <<EOF | kubectl apply -f -
apiVersion: storage.deckhouse.io/v1alpha1
kind: NamespaceSnapshot
metadata:
  name: root-full
  namespace: ${NS}
spec: {}
EOF

wait_snapshot_ready namespacesnapshot root-full 240
```

Получите generated refs. Сначала root -> VM:

```shell
CHILD_VM=$(child_ref_name namespacesnapshot root-full DemoVirtualMachineSnapshot)
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
kubectl -n "$NS" get namespacesnapshot root-full -o json \
  | jq '.status.childrenSnapshotRefs // []'

# Root должен иметь VM child.
kubectl -n "$NS" get namespacesnapshot root-full -o json \
  | jq -e '.status.childrenSnapshotRefs[]? | select(.kind=="DemoVirtualMachineSnapshot")'

# Root не должен иметь direct child для VM-owned disk-vm.
# Direct DemoVirtualDiskSnapshot child допустим только для standalone disk без ownerRef.
ROOT_DISK_CHILDREN=$(kubectl -n "$NS" get namespacesnapshot root-full -o json \
  | jq -r '.status.childrenSnapshotRefs[]? | select(.kind=="DemoVirtualDiskSnapshot") | .name')
for child in $ROOT_DISK_CHILDREN; do
  kubectl -n "$NS" get demovirtualdisksnapshot "$child" -o yaml
done
```

Content checks:

```shell
ROOT_FULL_NSC=$(kubectl -n "$NS" get namespacesnapshot root-full -o jsonpath='{.status.boundSnapshotContentName}')
wait_content_mcp "$ROOT_FULL_NSC" 180
kubectl get namespacesnapshotcontent "$ROOT_FULL_NSC" -o json \
  | jq '.status.manifestCheckpointName, .status.childrenSnapshotContentRefs'

VM_CONTENT=$(kubectl -n "$NS" get demovirtualmachinesnapshot "$CHILD_VM" -o jsonpath='{.status.boundSnapshotContentName}')
kubectl get demovirtualmachinesnapshotcontent "$VM_CONTENT" -o json \
  | jq '.status.manifestCheckpointName, .status.childrenSnapshotContentRefs'

DISK_CONTENT=$(kubectl -n "$NS" get demovirtualdisksnapshot "$CHILD_DISK" -o jsonpath='{.status.boundSnapshotContentName}')
kubectl get demovirtualdisksnapshotcontent "$DISK_CONTENT" -o json \
  | jq '.status.manifestCheckpointName, .status.childrenSnapshotContentRefs'
```

Ожидаемо:

- snapshots имеют `Ready=True Completed`;
- content objects имеют `status.manifestCheckpointName`;
- content objects с children имеют `status.childrenSnapshotContentRefs`;
- не проверяйте `Content Ready=True`, если конкретный content CRD этого не гарантирует.

## 9. Aggregated read API checks

Duplicate identity в namespace-relative aggregated output is checked as:

```text
apiVersion | kind | name
```

MCP storage may keep namespace for internal identity, but API output strips `metadata.namespace`.

### 9.1 Root full

```shell
kubectl get --raw \
  "$API_PATH_BASE/namespacesnapshots/root-full/manifests" \
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

## 10. Negative generic API checks

Для `kubectl get --raw` проверяйте Kubernetes Status `reason`; команда вернёт non-zero на error response. Если нужен HTTP status, используйте `curl` к Kubernetes API server с kubeconfig client cert из раздела 0.

```shell
set +e
kubectl get --raw \
  "$API_PATH_BASE/demovirtualdisksnapshots/not-found/manifests" \
  > /tmp/not-found-response.txt 2>&1
status=$?
set -e

test "$status" -ne 0
grep -q '"reason"[[:space:]]*:[[:space:]]*"NotFound"' /tmp/not-found-response.txt
```

```shell
set +e
kubectl get --raw \
  "$API_PATH_BASE/notasnapshots/x/manifests" \
  > /tmp/bad-request-response.txt 2>&1
status=$?
set -e

test "$status" -ne 0
grep -q '"reason"[[:space:]]*:[[:space:]]*"BadRequest"' /tmp/bad-request-response.txt
```

Duplicate `409 Conflict` можно оставить optional/manual-hard, если нет удобного ручного способа создать duplicate MCP contents.

## 11. Cleanup

```shell
kubectl -n "$NS" delete namespacesnapshot root-no-dsc --ignore-not-found --wait=false
kubectl -n "$NS" delete namespacesnapshot root-disk-only --ignore-not-found --wait=false
kubectl -n "$NS" delete namespacesnapshot root-full --ignore-not-found --wait=false

kubectl delete domainspecificsnapshotcontroller smoke-demo-disk-only --ignore-not-found
kubectl delete domainspecificsnapshotcontroller smoke-demo-vm-disk --ignore-not-found

kubectl delete ns "$NS" --wait=false
```

Cleanup не должен требовать, чтобы вообще ничего не осталось. Текущая Retain/ObjectKeeper модель может намеренно оставлять cluster-scoped artifacts.

Проверьте:

```shell
kubectl get ns "$NS" -o yaml || true
kubectl get namespacesnapshotcontents
kubectl get objectkeepers 2>/dev/null || true

kubectl logs -n "$CTRL_NS" deploy/"$CTRL_DEPLOY" --tail=500 \
  | grep -Ei 'panic|fatal|stacktrace|error' || true
```

Ожидаемо:

- нет stuck `Terminating` без понятного condition/log;
- нет неожиданных зависших finalizers;
- retained objects либо удалены, либо ожидаемо остались по текущей Retain policy;
- финальные логи без новых `panic`, `fatal`, `stacktrace` и без повторяющегося error loop.

## Definition of Done для ручного smoke

- CRD установлены.
- Schema `childrenSnapshotRefs` не содержит `namespace`.
- Без DSC `NamespaceSnapshot` готов и не создаёт demo children.
- Disk-only DSC: VM-owned disk не становится direct root child и не протекает в root aggregated read; standalone disk становится top-level child.
- VM+Disk DSC: root создаёт VM child, VM создаёт disk child, generated names получены через refs.
- Generated child snapshots have correct `spec.sourceRef`.
- Aggregated read работает для root / VM subtree / Disk subtree.
- Negative generic API checks возвращают ожидаемые HTTP code и Kubernetes Status `reason`.
- Cleanup не оставляет stuck finalizers/Terminating без объяснения.
