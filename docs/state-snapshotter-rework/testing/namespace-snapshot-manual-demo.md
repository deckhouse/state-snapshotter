# Демо: NamespaceSnapshot в `default` (N2a)

Линейная инструкция для показа на **живом кластере** (модуль **state-snapshotter**, поды в `d8-state-snapshotter`). Думать во время показа почти не нужно: идите **сверху вниз**, копируйте блоки по порядку.

**Зафиксировано в этом документе:** namespace **`default`**, имя снимка **`demo-ns-snap`**. Нужно другое имя — сделайте поиск-замену `demo-ns-snap` → своё имя во всех командах.

Нормативка (зачем так устроено): [`../design/namespace-snapshot-controller.md`](../design/namespace-snapshot-controller.md) §4–§5.

---

## Перед стартом (один раз проговорить)

- `kubectl` настроен на нужный контекст.
- В кластере есть **Deckhouse ObjectKeeper controller** (иначе TTL на root OK не отработает и цепочка не уйдёт сама).
- Снимается **тот же** namespace, где лежит снимок: здесь это **`default`**.

---

## Шаг 1 — положить в `default` объект для capture

Без ресурса из allowlist capture не стартует. Достаточно ConfigMap:

```bash
kubectl -n default create configmap demo-ns-snapshot-cm --from-literal=demo=namespace-snapshot \
  --dry-run=client -o yaml | kubectl apply -f -
```

---

## Шаг 2 — создать NamespaceSnapshot

```bash
kubectl apply -f - <<'EOF'
apiVersion: storage.deckhouse.io/v1alpha1
kind: NamespaceSnapshot
metadata:
  name: demo-ns-snap
  namespace: default
spec: {}
EOF
```

---

## Шаг 3 — дождаться Ready (скопировать целиком; до ~10 минут)

Пока не выйдет из цикла — не переходите к шагу 4.

```bash
deadline=$((SECONDS + 600))
while (( SECONDS < deadline )); do
  kubectl -n default get namespacesnapshots.storage.deckhouse.io demo-ns-snap -o json 2>/dev/null | jq -e \
    '.status.boundSnapshotContentName != null and (.status.boundSnapshotContentName | length > 0) and
     (.status.conditions // [] | map(select(.type == "Ready")) | .[0].status == "True")' >/dev/null 2>&1 && break
  sleep 3
done
kubectl -n default get namespacesnapshots.storage.deckhouse.io demo-ns-snap -o wide
```

Если зависло — `kubectl describe` на снимок и логи контроллера в `d8-state-snapshotter`.

---

## Шаг 4 — выписать имена (один блок, потом все команды ниже без правок)

```bash
export BOUND=$(kubectl -n default get namespacesnapshots.storage.deckhouse.io demo-ns-snap -o jsonpath='{.status.boundSnapshotContentName}')
export MCP=$(kubectl get snapshotcontents.storage.deckhouse.io "${BOUND}" -o jsonpath='{.status.manifestCheckpointName}')
export OK_NAME=ret-nssnap-default-demo-ns-snap
export SNAP_UID=$(kubectl -n default get namespacesnapshots.storage.deckhouse.io demo-ns-snap -o jsonpath='{.metadata.uid}')
export MCR_NAME="nss-${SNAP_UID}"
echo "BOUND=${BOUND}  MCP=${MCP}  OK=${OK_NAME}  MCR(ожидаемо нет)=${MCR_NAME}"
```

---

## Шаг 5 — показать цепочку объектов (порядок рассказа: снимок → контент → OK → MCP)

**5a. Корень**

```bash
kubectl -n default get namespacesnapshots.storage.deckhouse.io demo-ns-snap -o yaml
```

**5b. Cluster content + кто владелец (NSC → OK)**

```bash
kubectl get snapshotcontents.storage.deckhouse.io "${BOUND}" -o wide
kubectl get snapshotcontents.storage.deckhouse.io "${BOUND}" -o jsonpath='{.metadata.ownerReferences}' | jq .
```

**5c. ObjectKeeper (follow на NamespaceSnapshot, TTL)**

```bash
kubectl get objectkeepers.deckhouse.io "${OK_NAME}" -o wide
kubectl get objectkeepers.deckhouse.io "${OK_NAME}" -o jsonpath='{.spec}' | jq .
```

**5d. ManifestCheckpoint (владелец — NSC)**

```bash
kubectl get manifestcheckpoints.state-snapshotter.deckhouse.io "${MCP}" -o wide
kubectl get manifestcheckpoints.state-snapshotter.deckhouse.io "${MCP}" -o jsonpath='{.metadata.ownerReferences}' | jq .
```

**5e. Чанки (по префиксу имени MCP; опционально)**

```bash
kubectl get manifestcheckpointcontentchunks.state-snapshotter.deckhouse.io -o wide | grep -E "^NAME|${MCP}" || true
```

**5f. MCR после успеха быть не должен**

```bash
kubectl -n default get manifestcapturerequests.state-snapshotter.deckhouse.io "${MCR_NAME}" -o yaml 2>&1 || echo "OK: MCR нет (норма после capture)"
```

**5g. Aggregated read (снимок ещё жив)**

```bash
kubectl get --raw "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/default/namespacesnapshots/demo-ns-snap/manifests" | jq 'length'
```

**5h. Один фразовый вывод для аудитории**

- OK следует за **NamespaceSnapshot** (`FollowObjectWithTTL`), **NSC** зависит от **OK**.
- **MCP** зависит от **NSC**. **MCR** был только на время capture.

---

## Шаг 6 — удалить снимок и показать retained read

Снимок из API уходит; **NSC** и **MCP** остаются; aggregated по **тому же URL** обычно ещё отвечает.

```bash
kubectl -n default delete namespacesnapshots.storage.deckhouse.io demo-ns-snap --wait=true
kubectl get snapshotcontents.storage.deckhouse.io "${BOUND}" -o wide
kubectl get --raw "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/default/namespacesnapshots/demo-ns-snap/manifests" | jq 'length'
```

---

## Шаг 7 — ничего не удалять руками: ждём TTL на ObjectKeeper, потом GC

Не вызывайте `kubectl delete` на **NSC**, **MCP** или **OK** — так вы демонстрируете «прод»: после удаления снимка живёт **OK** до **`spec.ttl`**, его снимает **Deckhouse**, затем **GC** убирает **NSC → MCP → chunks**.

Узнать TTL на кластере:

```bash
kubectl get objectkeepers.deckhouse.io "${OK_NAME}" -o jsonpath='{.spec.ttl}{"\n"}'
```

Подождать **чуть дольше этого TTL** (в отладочных сборках контроллера TTL может быть коротким — см. `DefaultSnapshotRootOKTTL` в `pkg/config/config.go`), затем проверки:

```bash
sleep 90
kubectl get objectkeepers.deckhouse.io "${OK_NAME}" 2>&1 || echo "OK удалён"
kubectl get snapshotcontents.storage.deckhouse.io "${BOUND}" 2>&1 || echo "NSC удалён"
kubectl get manifestcheckpoints.state-snapshotter.deckhouse.io "${MCP}" 2>&1 || echo "MCP удалён"
```

Aggregated после полного схода цепочки перестаёт отвечать — как в `PR4_SMOKE_REQUIRE_TTL=1` в `hack/pr4-smoke.sh`.

---

## Шаг 8 — хвост вне снимка

ConfigMap **`demo-ns-snapshot-cm`** к цепочке снимка **не** привязан — убрать вручную:

```bash
kubectl -n default delete configmap demo-ns-snapshot-cm --ignore-not-found
```

---

## Приложение A — автоматический прогон

Из корня репозитория: **`bash hack/pr4-smoke.sh`** (`default`, снимок **`pr4-smoke`**). Уборка без ожидания TTL: **`bash hack/pr4-smoke-cleanup.sh`**.

---

## Приложение B — имена API (если нужно читать сырой OpenAPI)

| Ресурс | `kubectl` group |
|--------|-----------------|
| NamespaceSnapshot | `namespacesnapshots.storage.deckhouse.io` |
| SnapshotContent | `snapshotcontents.storage.deckhouse.io` |
| ObjectKeeper | `objectkeepers.deckhouse.io` |
| ManifestCheckpoint | `manifestcheckpoints.state-snapshotter.deckhouse.io` |
| ManifestCaptureRequest | `manifestcapturerequests.state-snapshotter.deckhouse.io` |
| Chunk | `manifestcheckpointcontentchunks.state-snapshotter.deckhouse.io` |

---

## Приложение C — discovery (опционально, до шага 1)

```bash
kubectl get --raw "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1" | jq '.resources[] | select(.name=="namespacesnapshots/manifests")'
```

Ожидается `"namespaced": true`.
