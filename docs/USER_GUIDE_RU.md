---
title: "Работа со снимками (Snapshot)"
description: "Как работает ресурс Snapshot: что он захватывает, его жизненный цикл, как создать снимок, отслеживать его, читать захваченное состояние и восстанавливать."
d8Edition: ee
weight: 20
---

## Что такое Snapshot?

`Snapshot` (`storage.deckhouse.io/v1alpha1`, короткое имя `snap`) — это namespaced-ресурс, **снимок desired-state namespace на определённый момент времени**. Вы создаёте `Snapshot` в namespace, и модуль захватывает манифесты пользовательских объектов этого namespace в неизменяемый, надёжно сохранённый артефакт, который позже можно прочитать или восстановить в другой namespace.

`Snapshot` — **одноразовый и неизменяемый**: спека замораживается при создании, namespace захватывается ровно один раз, повторного захвата нет. Чтобы сделать новый снимок, создайте новый `Snapshot`.

| Свойство | Значение |
|----------|----------|
| API-группа / версия | `storage.deckhouse.io/v1alpha1` |
| Kind | `Snapshot` (короткое имя `snap`) |
| Scope | Namespaced (захватывает свой namespace) |
| Спека | Неизменяема после создания |

## Как это работает

При создании `Snapshot` контроллер:

1. **Перечисляет** все namespaced-виды ресурсов в кластере через discovery API Kubernetes (не по захардкоженному списку) — поэтому захватываются и произвольные кастомные ресурсы.
2. **Листит** объекты каждого вида в namespace снимка и **отбирает** те, что представляют пользовательский desired-state (см. [Что попадает в снимок](#что-попадает-в-снимок)).
3. **Сохраняет** отобранные манифесты в надёжный cluster-scoped артефакт (`SnapshotContent` + внутренний `ManifestCheckpoint`, с разбиением на чанки для больших объёмов).
4. **Публикует статус**: когда захват завершён и сохранён, `Snapshot` становится `Ready=True`, а `status.boundSnapshotContentName` указывает на сохранённый контент.
5. **Удерживает** сохранённый контент после удаления `Snapshot` в течение настраиваемого TTL — чтобы захваченное состояние пережило удаление объекта-запроса.

Захват выполняется **fail-closed**: если какой-то вид ресурса нельзя прочитать (например, RBAC ещё не распространился или сломан агрегированный API), контроллер не создаёт частичный снимок — он выставляет `Ready=False` / `NamespaceCaptureIncomplete` и повторяет попытку.

## Что попадает в снимок

Модель — **default-include**: в снимок попадает **любой** namespaced-объект целевого namespace, **кроме** явно исключённого. Отдельного allowlist «снимать только это» нет.

**Попадает (примеры):**

- пользовательские объекты конфигурации и данных: `ConfigMap`, `Secret` (любого типа, кроме service-account-token), `Service`, `PersistentVolumeClaim`, `ServiceAccount` (кроме `default`), standalone `Pod` (без controller-владельца);
- workload-объекты верхнего уровня, которые создаёте вы: `Deployment`, `StatefulSet`, `DaemonSet`, `Job`, `CronJob` и т. п. Их производные (`ReplicaSet`/`Pod`) исключаются — владелец пересоздаёт их при restore;
- любые **кастомные ресурсы** в namespace, в т. ч. агрегированные API с реальным хранилищем;
- сеть и RBAC: `Ingress`, `NetworkPolicy`, `Role`, `RoleBinding` и др.

**Не попадает:**

| Категория | Примеры | Почему |
|-----------|---------|--------|
| Производные (controller-owned) | объекты с `ownerReference.controller: true` (`ReplicaSet`/`Pod` от `Deployment`) | владелец пересоздаёт их при restore |
| Control-plane noise | `Event`, `Endpoints`, `Lease`, `CiliumEndpoint`, `ConfigMap/kube-root-ca.crt`, `ServiceAccount/default`, service-account-token `Secret` | регенерируется контрол-плейном / CNI, не пользовательский desired-state |
| Виртуальные / вычисляемые | `metrics.k8s.io` (`PodMetrics`/`NodeMetrics`), `custom`/`external.metrics.k8s.io` | не хранятся (нет verb `watch`), невосстановимы |
| Снапшотная машинерия | CSI `VolumeSnapshot`, snapshot/content-виды, создаваемые самим модулем | self-referential |
| Машинерия модуля | вся группа `state-snapshotter.deckhouse.io` и request/transfer-виды `storage.deckhouse.io` (`VolumeCaptureRequest`, `VolumeRestoreRequest`, `DataExport`, `DataImport`) | внутренние execution-объекты |

> Специального правила «исключать объекты, управляемые Deckhouse» нет. Deckhouse-managed объекты отсекаются теми же общими сигналами (controller-owned, control-plane noise или машинерия модуля). Всё остальное в namespace — в том числе ресурсы, которые вы лишь *настроили* поверх модулей, — считается desired-state и попадает в снимок.

Полные нормативные правила — в design-доке [`state-snapshotter-rework/design/snapshot-controller.md` §4.5](state-snapshotter-rework/design/snapshot-controller.md).

## Создание Snapshot

Создайте `Snapshot` в том namespace, который хотите захватить. Режим по умолчанию (без `spec.source`) выполняет динамический захват namespace:

```yaml
d8 k apply -f - <<EOF
apiVersion: storage.deckhouse.io/v1alpha1
kind: Snapshot
metadata:
  name: my-namespace-snapshot
  namespace: my-app
spec: {}
EOF
```

> Спека неизменяема. `spec: {}` выбирает динамический захват namespace `my-app`. Не добавляйте поле целевого namespace — `Snapshot` всегда захватывает свой namespace.

## Отслеживание статуса

```bash
d8 k -n my-app get snap
```

```console
NAME                    READY   REASON      CONTENT                       AGE
my-namespace-snapshot   True    Captured    snap-content-my-app-...       30s
```

Подробности — в conditions:

```bash
d8 k -n my-app get snap my-namespace-snapshot -o jsonpath='{.status.conditions}' | jq
```

Ключевые поля статуса:

- `status.conditions[type=Ready]` — общая готовность. `Ready=True` означает, что захват завершён и сохранён.
- `status.boundSnapshotContentName` — надёжный cluster-scoped контент с захваченным состоянием.
- `status.childrenSnapshotRefs` — дочерние снимки, когда доменные контроллеры формируют дерево снимков (например, виртуальные машины/диски).

## Чтение захваченного состояния

Захваченные манифесты читаются через контролируемый агрегированный subresource (сырые payload'ы во внутренних чанках напрямую не доступны):

```bash
d8 k get --raw \
  "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/my-app/snapshots/my-namespace-snapshot/manifests" \
  | jq 'length'
```

Возвращается список захваченных объектов. Этот же endpoint — точка входа для инструментов восстановления.

## Восстановление

Захваченный namespace восстанавливается через контролируемый read-path модуля — внутренние объекты-запросы (например, `VolumeRestoreRequest`) вы **не** создаёте вручную: это машинерия модуля, она не доступна пользователям.

- **Манифесты** — прочитать захваченные объекты через endpoint `/manifests` выше и применить их в целевой namespace.
- **Состояние с данными** — чтобы восстановить объекты вместе с их персистентными данными, модуль предоставляет data-restoration read-path на снимке. Материализацию данных в целевой namespace модуль выполняет внутри себя; результат вы получаете через агрегированный API, а не создавая объекты переноса данных самостоятельно.

Полный сквозной сценарий восстановления — в runbook [`state-snapshotter-rework/testing/snapshot-tree-demo-runbook.md`](state-snapshotter-rework/testing/snapshot-tree-demo-runbook.md).

## Режимы снимка

Источник захвата выбирается через `spec.source` (неизменяем, ровно один член при заданном source):

| Режим | `spec` | Поведение |
|-------|--------|-----------|
| Динамический захват (по умолчанию) | `spec: {}` | Контроллер захватывает живой namespace. |
| Импорт | `spec.source.import: {}` | Snapshot материализуется из загруженного payload вместо живого namespace — используется инструментами миграции/восстановления между кластерами. |
| Статическое связывание | `spec.source.snapshotContentName: <name>` | Snapshot привязывается к уже существующему pre-provisioned `SnapshotContent` (аналог CSI `volumeSnapshotContentName`). |

## Жизненный цикл и удержание

- **Одноразовый / неизменяемый.** Спека замораживается при создании; `metadata.generation` не растёт; повторного захвата нет.
- **Удержание после удаления.** Сохранённый `SnapshotContent` удерживается в течение TTL (настраивается на контроллере) после удаления объекта `Snapshot`, через якорь `ObjectKeeper`. Это позволяет захваченному состоянию пережить объект-запрос. После истечения TTL контент и его checkpoints собираются сборщиком мусора.
- **Нет фоновой нагрузки.** Модуль работает только в ответ на явные запросы; непрерывного фонового захвата нет.

## Замечания и ограничения

- `Snapshot` захватывает **только namespaced**-ресурсы своего namespace; сам cluster-scoped объект `Namespace` не захватывается.
- Манифесты захватываются **как есть** (включая `status`); очистка полей (`status`, `resourceVersion`, `uid` и т. п.) выполняется на read-path восстановления, а не при захвате.
- Объекты `Secret` захватываются **дословно** (данные сохраняются как есть), кроме service-account-token-секретов, которые исключаются правилом включения. Шифрование хранилища снимков at-rest — отдельная будущая задача.
