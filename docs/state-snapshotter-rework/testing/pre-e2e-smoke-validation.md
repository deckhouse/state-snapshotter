Ниже короткий smoke-checklist через `kubectl` перед полноценным e2e.

```shell
# 0. Контекст
kubectl cluster-info
kubectl get ns d8-state-snapshotter
kubectl get pods -n d8-state-snapshotter -o wide
kubectl get deploy -n d8-state-snapshotter
```

```shell
# 1. CRD установлены
kubectl get crd | grep -E 'namespacesnapshots|namespacesnapshotcontents|demovirtual|domainspecificsnapshotcontrollers|manifestcapture'
```

```shell
# 2. Проверить schema childrenSnapshotRefs: без namespace
kubectl explain namespacesnapshot.status.childrenSnapshotRefs
kubectl explain namespacesnapshot.status.childrenSnapshotRefs.apiVersion
kubectl explain namespacesnapshot.status.childrenSnapshotRefs.kind
kubectl explain namespacesnapshot.status.childrenSnapshotRefs.name

# В explain НЕ должно быть:
# namespacesnapshot.status.childrenSnapshotRefs.namespace
```

```shell
# 3. Логи контроллера без panic/fatal
kubectl logs -n d8-state-snapshotter deploy/state-snapshotter-controller --tail=300 \
  | grep -Ei 'panic|fatal|stacktrace|error'
```

```shell
# 4. Создать тестовый namespace и простой объект
kubectl create ns nss-smoke

kubectl -n nss-smoke create configmap smoke-cm \
  --from-literal=key=value
```

```shell
# 5. Создать root NamespaceSnapshot
cat <<'EOF' | kubectl apply -f -
apiVersion: storage.deckhouse.io/v1alpha1
kind: NamespaceSnapshot
metadata:
  name: root
  namespace: nss-smoke
spec: {}
EOF
```

```shell
# 6. Проверить binding root → NamespaceSnapshotContent
kubectl -n nss-smoke get namespacesnapshot root -o yaml

kubectl -n nss-smoke get namespacesnapshot root \
  -o jsonpath='{.status.boundSnapshotContentName}{"\n"}'

ROOT_NSC=$(kubectl -n nss-smoke get namespacesnapshot root -o jsonpath='{.status.boundSnapshotContentName}')
kubectl get namespacesnapshotcontent "$ROOT_NSC" -o yaml
```

Ожидаемо: у `NamespaceSnapshot` есть `status.boundSnapshotContentName`, у `NamespaceSnapshotContent` есть ссылка обратно на root snapshot.

```shell
# 7. Проверить MCR/MCP manifest capture
kubectl -n nss-smoke get manifestcapturerequests
kubectl get manifestcheckpoints

kubectl get namespacesnapshotcontent "$ROOT_NSC" \
  -o jsonpath='{.status.manifestCheckpointName}{"\n"}'

MCP=$(kubectl get namespacesnapshotcontent "$ROOT_NSC" -o jsonpath='{.status.manifestCheckpointName}')
kubectl get manifestcheckpoint "$MCP" -o yaml
```

Ожидаемо: MCP существует и `Ready=True`.

```shell
# 8. Проверить root Ready
kubectl -n nss-smoke get namespacesnapshot root \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}{"\n"}{end}'

kubectl get namespacesnapshotcontent "$ROOT_NSC" \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}{"\n"}{end}'
```

Ожидаемо: `Ready=True Completed`.

```shell
# 9. Проверить childrenSnapshotRefs: для простого root их быть не должно
kubectl -n nss-smoke get namespacesnapshot root \
  -o jsonpath='{.status.childrenSnapshotRefs}{"\n"}'
```

Ожидаемо: пусто.

```shell
# 10. Если demo CRD есть — создать demo child snapshot и проверить graph refs
cat <<'EOF' | kubectl apply -f -
apiVersion: demo.state-snapshotter.deckhouse.io/v1alpha1
kind: DemoVirtualDiskSnapshot
metadata:
  name: disk-a
  namespace: nss-smoke
spec:
  parentSnapshotRef:
    apiVersion: storage.deckhouse.io/v1alpha1
    kind: NamespaceSnapshot
    name: root
EOF
```

```shell
kubectl -n nss-smoke get demovirtualdisksnapshot disk-a -o yaml

kubectl -n nss-smoke get namespacesnapshot root \
  -o jsonpath='{.status.childrenSnapshotRefs}{"\n"}'
```

Ожидаемо: в `childrenSnapshotRefs` есть только:

```shell
apiVersion: demo.state-snapshotter.deckhouse.io/v1alpha1
kind: DemoVirtualDiskSnapshot
name: disk-a
```

и **нет `namespace`**.

```shell
# 11. Проверить child content refs на root NSC
kubectl get namespacesnapshotcontent "$ROOT_NSC" \
  -o jsonpath='{.status.childrenSnapshotContentRefs}{"\n"}'
```

```shell
# 12. Проверить, что parent проснулся от child status
kubectl -n nss-smoke get namespacesnapshot root \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}{"\n"}{end}'
```

Ожидаемо: root не завис в `ChildSnapshotPending`, если demo child уже `Ready=True`.

```shell
# 13. Удаление root и проверка cleanup
kubectl -n nss-smoke delete namespacesnapshot root --wait=false

kubectl -n nss-smoke get namespacesnapshot root
kubectl get namespacesnapshotcontent "$ROOT_NSC" -o yaml
```

Ожидаемо зависит от deletionPolicy/retain-модели, но не должно быть бесконечного stuck без понятного condition/log.

```shell
# 14. Финальные логи после smoke
kubectl logs -n d8-state-snapshotter deploy/state-snapshotter-controller --tail=500 \
  | grep -Ei 'panic|fatal|stacktrace|error'
```

```text
Smoke-check выполнен, кластер и контроллер в рабочем состоянии. Ниже фактический результат по чеклисту.

- **0. Контекст**
  - API доступен (`kubectl cluster-info` OK).
  - Namespace `d8-state-snapshotter` активен.
  - Pod’ы `controller` и `webhooks` в `Running`.
  - Deployments `controller`/`webhooks` готовы `1/1`.

- **1. CRD установлены**
  - В кластере есть:
    - `namespacesnapshots.storage.deckhouse.io`
    - `namespacesnapshotcontents.storage.deckhouse.io`
    - `manifestcapturerequests.state-snapshotter.deckhouse.io`
    - `manifestcheckpoints.state-snapshotter.deckhouse.io`
    - demo CRD (включая `demovirtualdisksnapshots`, `demovirtualmachinesnapshots`)
    - `domainspecificsnapshotcontrollers...`

- **2. Schema childrenSnapshotRefs (без namespace)**
  - `kubectl explain namespacesnapshot.status.childrenSnapshotRefs` показывает только required:
    - `apiVersion` (required)
    - `kind` (required)
    - `name` (required)
  - `kubectl explain namespacesnapshot.status.childrenSnapshotRefs.namespace` → `field "namespace" does not exist` (ожидаемо).

- **3. Логи контроллера (до smoke)**
  - В хвосте нашли старые строки leader-election (`leader election lost`) — не panic/stacktrace приложения.
  - После smoke (п.14) — `panic|fatal|stacktrace|error` не найдено.

- **4–6. Root NamespaceSnapshot**
  - Создан `nss-smoke` + `ConfigMap smoke-cm`.
  - Создан `NamespaceSnapshot/root`.
  - Появился `status.boundSnapshotContentName`:
    - `ns-4414190b366e44a9bd50a77845d80576`
  - `NamespaceSnapshotContent` существует и корректно ссылается обратно:
    - `spec.namespaceSnapshotRef = storage.deckhouse.io/v1alpha1, NamespaceSnapshot, nss-smoke/root`.

- **7. MCR/MCP**
  - `ManifestCaptureRequest` в `nss-smoke` уже отсутствует (ожидаемая очистка после успешного capture).
  - `ManifestCheckpoint` существует:
    - `mcp-ca752c09efdf9b7f`
    - Condition: `Ready=True Completed`.
  - На `NamespaceSnapshotContent.status.manifestCheckpointName` стоит тот же MCP.

- **8. Root Ready**
  - `NamespaceSnapshot/root`:
    - `Bound=True ContentCreated`
    - `Ready=True Completed`
  - `NamespaceSnapshotContent`:
    - `Ready=True Completed`

- **9. childrenSnapshotRefs для простого root**
  - Без demo DSC — пусто даже если в namespace есть demo resource. Это проверяет, что resource присутствует в API, но kind не активирован в `NamespaceSnapshot` discovery.

- **10–12. Demo child + graph refs**
  - Актуальная parent-owned модель: demo child создаётся parent-контроллером только после регистрации eligible DSC и обнаружения matching resource; вручную создавать child snapshot для нормального flow не нужно.
  - `DemoVirtualDiskSnapshotController` / `DemoVirtualMachineSnapshotController` при этом стартуют всегда: manual demo snapshot может materialize без DSC, но это не включает demo kind в root discovery.
  - Child snapshot spec использует обычный `spec.parentSnapshotRef` (`apiVersion/kind/name`), без legacy root-specific поля.
  - В `root.status.childrenSnapshotRefs` появляется strict ref **без namespace**:
    - `apiVersion: demo.state-snapshotter.deckhouse.io/v1alpha1`
    - `kind: DemoVirtualDiskSnapshot`
    - `name: disk-a`
  - `DemoVirtualDiskSnapshot` становится `Ready=True Completed` только после собственного MCR/MCP.
  - В `root NSC` появились `childrenSnapshotContentRefs` с demo content.
  - Root не завис в `ChildSnapshotPending`, остался `Ready=True Completed`.

- **13. Удаление root**
  - `kubectl delete namespacesnapshot root --wait=false` → root удалён быстро (`NotFound`).
  - `NamespaceSnapshotContent ns-441...` остался (Retain-модель):
    - `spec.deletionPolicy: Retain`
    - `ownerReference` на `ObjectKeeper`
    - статус `Ready=True Completed`.

- **14. Финальные логи**
  - В хвосте `deploy/controller` за последние 500 строк:
    - `panic|fatal|stacktrace|error` — **не найдено**.

Итог: минимальный критерий перед e2e выполнен — root flow рабочий, MCP/Ready сходятся, parent-owned demo child корректно встраивается в `childrenSnapshotRefs` в формате `apiVersion/kind/name` без `namespace`.
```
