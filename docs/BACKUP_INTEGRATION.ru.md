---
title: "Интеграция с системами резервного копирования"
description: "Как настроить доступ внешней системы, создать и скачать резервную копию, загрузить её в кластер и восстановить состояние."
weight: 30
---

Внешняя система резервного копирования работает с модулем `state-snapshotter` через API-сервер Kubernetes для создания и
загрузки резервных копий, а также восстановления состояния из загруженного снимка.

## Границы интеграции

В этом разделе перечислено, что входит в интеграцию, описанную на этой странице, а что остаётся на стороне внешней
системы или описано в другой документации.

### Что входит в интеграцию

Интеграция состоит из четырёх операций:

1. [Создание снимка](#создание-снимка).
1. [Проверка успешности снимка](#проверка-состояния-снимка).
1. [Скачивание снимка](#скачивание-снимка): манифестов узлов и данных томов.
1. [Загрузка снимка обратно в кластер](#загрузка-резервной-копии-в-кластер).

Кроме того, модуль позволяет прочитать поддерево снимка, подготовленное к применению. Подробнее описано [в разделе
«Чтение снимка для восстановления»](#чтение-снимка-для-восстановления).

Все операции — это обычные HTTP-запросы к API-серверу Kubernetes и обычные объекты Kubernetes. Клиентская библиотека
не требуется.

Снимок неймспейса создаётся объектом Snapshot и однократно захватывает неймспейс, в котором этот объект создан. Спецификация
Snapshot неизменяема: для нового захвата нужен новый Snapshot. Внешняя система обращается только к объектам в неймспейсе
и их подресурсам, за двумя исключениями: она читает объекты CustomSnapshotDefinition (CSD) уровня кластера, чтобы узнать,
какие ресурсы можно [захватить по отдельности](#снимки-отдельных-ресурсов), и запрашивает обнаружение API (API
discovery) для группы API вида узла, `GET /apis/{group}/{version}`, которое доступно любому аутентифицированному клиенту
без дополнительных ролей.

### Что не входит в интеграцию

В интеграцию не входит следующее:

- **Применение восстанавливаемых объектов.** Чтение, описанное [в разделе
  «Чтение снимка для восстановления»](#чтение-снимка-для-восстановления), возвращает объекты, подготовленные к
  применению. Клиент сам применяет объекты, выбирает порядок и решает, что делать с объектами, которые уже есть в
  неймспейсе. Чтобы восстановить состояние без этой логики, используйте веб-интерфейс Deckhouse или команду
  `d8 snapshot restore`.
- **Протокол передачи байтов тома.** На этой странице описано создание DataExport или DataImport и отслеживание их
  состояния. HTTP-эндпоинты, режимы передачи, вызов `finished` и поля статуса с адресом относятся к модулю
  [`storage-foundation`](/modules/storage-foundation/) и описаны [в разделе «Экспорт и импорт содержимого томов по
  HTTP»](/modules/storage-foundation/usage.html#экспорт-и-импорт-данных-томов).
- **Содержимое узлов, добавленных другими модулями.** Поддерево, созданное другим модулем Deckhouse Platform, например
  виртуальные машины и их диски, обходится, скачивается и загружается через группу эндпоинтов его типа, как описано
  [в разделе «Структура снимка и API узлов»](#структура-снимка-и-api-узлов) и [в разделе «Узлы видов, заранее
  неизвестных клиенту»](#узлы-видов-заранее-неизвестных-клиенту), а данные его томов передаются так же, как
  данные узла VolumeSnapshot. Содержимое таких узлов описано в документации соответствующего модуля.
- **Переименование узлов при загрузке.** Загружайте дерево с теми именами узлов, с которыми оно было захвачено.
  Загрузка с другими именами узлов не описана.
- **Внутренние объекты модуля.** Cluster-wide-объекты, в которых хранятся захваченные данные, их формат и очистка
  могут измениться в любой версии. Роли [из раздела «Настройка доступа внешней системы к
  кластеру»](#настройка-доступа-внешней-системы-к-кластеру) не дают доступа к ним.
- **Формат хранения.** Перечисленные операции не требуют определённого формата: внешняя система может хранить
  переданные данные в собственном хранилище. Формат архива, общий для `d8` и веб-интерфейса Deckhouse, описан [на
  странице «Работа с архивами снимков»](snapshot_archives.html). Если внешняя система хранит снимки в нём, собранный ею
  архив можно загрузить через `d8` или веб-интерфейс, а архив, выгруженный ими, можно поместить в её хранилище без
  изменений.

## Настройка доступа внешней системы к кластеру

Создайте для внешней системы отдельный ServiceAccount в каждом неймспейсе, с которым она должна работать, и назначьте
ему ClusterRole для агента резервного копирования через RoleBinding. RoleBinding ограничивает предоставленные права
своим неймспейсом.

Имя ClusterRole зависит от версии Deckhouse Platform: для версии 1.78 и новее используйте
`d8:state-snapshotter:backup-agent`, а для версий от 1.76.9 до 1.77 включительно —
`d8:use:capability:module:state-snapshotter:backup_agent`.

Пример для неймспейса `my-app`:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: backup-agent
  namespace: my-app
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: backup-agent
  namespace: my-app
subjects:
  - kind: ServiceAccount
    name: backup-agent
    namespace: my-app
roleRef:
  kind: ClusterRole
  name: <BACKUP_AGENT_CLUSTER_ROLE>
  apiGroup: rbac.authorization.k8s.io
```

Вместо `<BACKUP_AGENT_CLUSTER_ROLE>` укажите имя ClusterRole для установленной версии Deckhouse Platform. Эта роль
предоставляет права модуля `state-snapshotter` на создание и чтение снимков, скачивание и загрузку узлов, а также
чтение содержимого снимка для восстановления. Роль также выдаёт те же права на снимки других видов, например на
снимки виртуальных машин и их дисков, если включены модули, которым принадлежат эти виды снимков. Для передачи данных
томов дополнительно нужна capability `transfer_volume_data` модуля `storage-foundation` (именованный набор прав модуля,
который выдаётся отдельной ClusterRole). Если внешней системе требуется только часть операций, назначьте отдельные роли
из таблицы:

| Права | Deckhouse Platform 1.78 и новее | Deckhouse Platform от 1.76.9 до 1.77 |
|---|---|---|
| Чтение Snapshot, VolumeSnapshot и их статусов для обхода дерева снимка | `d8:namespace-capability:state-snapshotter:view_snapshots` | `d8:use:capability:module:state-snapshotter:view_snapshots` |
| Создание и удаление Snapshot. Изменение не поддерживается, поскольку спецификация Snapshot неизменяема. Удаление Snapshot удаляет всё дерево снимка | `d8:namespace-capability:state-snapshotter:manage_snapshots` | `d8:use:capability:module:state-snapshotter:manage_snapshots` |
| Чтение манифестов узла через эндпоинты обеих групп | `d8:namespace-capability:state-snapshotter:download_snapshots` | `d8:use:capability:module:state-snapshotter:download_snapshots` |
| Загрузка манифестов узлов через эндпоинты обеих групп и создание объектов VolumeSnapshot для снимков PVC и при импорте | `d8:namespace-capability:state-snapshotter:upload_snapshots` | `d8:use:capability:module:state-snapshotter:upload_snapshots` |
| Чтение поддерева снимка для восстановления через эндпоинты обеих групп | `d8:namespace-capability:state-snapshotter:restore_snapshots` | `d8:use:capability:module:state-snapshotter:restore_snapshots` |
| Создание DataExport и DataImport и передача данных томов через их HTTP-эндпоинты | `d8:namespace-capability:storage-foundation:transfer_volume_data` | `d8:use:capability:module:storage-foundation:transfer_volume_data` |

Capability `transfer_volume_data` предоставляет модуль [`storage-foundation`](/modules/storage-foundation/).
В обеих схемах ролей она агрегируется в ClusterRole агента резервного копирования. Начиная с Deckhouse Platform 1.78,
capability также входит в роль неймспейса `admin`, но не в `manager`. В более ранних версиях она не входит в
пользовательские роли неймспейса. Если вместо ClusterRole агента резервного копирования используются такие роли,
для передачи данных томов назначьте capability отдельно.

После обновления до Deckhouse Platform 1.78 привязки к прежней ClusterRole
`d8:use:capability:module:state-snapshotter:backup_agent` продолжают работать как устаревший псевдоним. Для новых
привязок используйте `d8:state-snapshotter:backup-agent`. Поле `roleRef` в RoleBinding изменить нельзя, поэтому для
перехода на новое имя создайте новый RoleBinding, а затем удалите старый.

Объекты CustomSnapshotDefinition, по которым [определяются ресурсы, доступные для отдельного захвата](#снимки-отдельных-ресурсов),
относятся к уровню кластера, и RoleBinding доступа к ним не даёт. Назначьте ClusterRole
`d8:state-snapshotter:backup-agent-system` через ClusterRoleBinding. Она даёт только чтение объектов
CustomSnapshotDefinition, и её имя одинаково во всех поддерживаемых версиях Deckhouse Platform:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: backup-agent-system
subjects:
  - kind: ServiceAccount
    name: backup-agent
    namespace: my-app
roleRef:
  kind: ClusterRole
  name: d8:state-snapshotter:backup-agent-system
  apiGroup: rbac.authorization.k8s.io
```

{{< alert level="warning" >}}
Назначайте роли только в тех неймспейсах, к которым внешней системе нужен доступ, и только те роли, которые она
использует. Полный набор позволяет удалять снимки и создавать объекты в рабочем неймспейсе, поэтому каждая лишняя
привязка — ещё один способ потерять данные.
{{< /alert >}}

Получите токен ServiceAccount, например:

```shell
d8 k -n my-app create token backup-agent --duration=24h
```

Передавайте токен в заголовке `Authorization: Bearer <TOKEN>` и проверяйте сертификат API-сервера по CA кластера. Ответы
могут содержать Secret из снимка, поэтому внешний клиент должен защищать полученные данные так же, как данные исходного
неймспейса.

## API снимков

API снимков предназначен для чтения и переноса содержимого снимка через агрегированные эндпоинты Kubernetes API. Его
используют внешние системы резервного копирования и другие клиенты, которым нужно работать с содержимым снимка
напрямую.

Внешняя система резервного копирования (СРК) должна поддерживать создание, проверку, скачивание и загрузку снимков
неймспейсов (Snapshot), отдельных PVC (VolumeSnapshot) и всех видов снимков, объявленных готовыми
CustomSnapshotDefinition в подключённом кластере. Snapshot и VolumeSnapshot — встроенные виды: их поддержка не должна
зависеть от наличия записей в CustomSnapshotDefinition. Доступные доменные виды зависят от версий Deckhouse Platform и
модулей, а также от включённых модулей; определяйте их отдельно в каждом кластере и обновляйте обнаружение перед
операцией. Не используйте фиксированный список доменных видов и не переносите список одного кластера в другой.

Создание отдельных снимков описано [в разделе «Снимки отдельных ресурсов»](#снимки-отдельных-ресурсов).
СРК также должна давать пользователю выбор узлов и поддеревьев для загрузки, как описано
[в разделе «Выбор узлов и поддеревьев для загрузки»](#выбор-узлов-и-поддеревьев-для-загрузки).

### Операции с объектом Snapshot

Создавайте корневой Snapshot и проверяйте его состояние через API ресурсов Kubernetes:

```text
POST /apis/state-snapshotter.deckhouse.io/v1alpha1/namespaces/{namespace}/snapshots?fieldValidation=Strict
GET  /apis/state-snapshotter.deckhouse.io/v1alpha1/namespaces/{namespace}/snapshots/{name}
```

В запросе `POST` передавайте JSON-объект Snapshot с заголовком `Content-Type: application/json`.
`{namespace}` — неймспейс для захвата, `{name}` — имя из `metadata.name`.
Snapshot со `spec: {}` однократно захватывает состояние неймспейса.
Snapshot со `spec.mode: Import` принимает переданные извне данные.
После создания спецификация неизменяема. Готовность проверяется по полю `status.conditions` созданного объекта.

При создании Snapshot используйте параметр `fieldValidation=Strict` в `POST`-запросе, чтобы API-сервер
отклонял неизвестные и повторяющиеся поля. Без строгой проверки API-сервер отбрасывает поле с опечаткой в **имени**
(`mod: Import` вместо `mode: Import`). Тогда Snapshot захватит активный неймспейс в режиме `Capture`
по умолчанию. Неправильное **значение**, например `spec.mode: Imprt`, отклоняется проверкой enum в CRD. После
создания прочитайте сохранённое значение `spec.mode` и проверьте `status.conditions`.

### Структура снимка и API узлов

Снимок состоит из дерева связанных узлов в одном неймспейсе. Корень снимка неймспейса — Snapshot. Корнем снимка
отдельного ресурса или выбранного поддерева может быть VolumeSnapshot или доменный вид снимка. Манифест Namespace
принадлежит только узлу Snapshot неймспейса. Дочерние узлы создаются при захвате состояния и перечисляются [в
`status.childrenSnapshotRefs`](cr.html#snapshot-v1alpha1-status-childrensnapshotrefs). Все узлы дерева находятся в
одном неймспейсе, поэтому ссылка на дочерний узел содержит только `apiVersion`, `kind` и `name`. Узлы, содержащие данные
обычных PersistentVolumeClaim (PVC), представлены объектами VolumeSnapshot.

Каждый узел содержит собственные захваченные манифесты и может содержать данные одного тома. Скачивание выполняется по
одному узлу: эндпоинт `manifests-download` возвращает только манифесты адресованного узла, а клиент обходит дочерние
узлы по `status.childrenSnapshotRefs`. Эндпоинт восстановления `manifests-with-data-restoration`, напротив, рекурсивно
возвращает всё поддерево.

Для узлов Snapshot и VolumeSnapshot используются разные API-группы:

| Что адресуется | API-группа | Версия | Ресурс |
|---|---|---|---|
| Объект Snapshot | `state-snapshotter.deckhouse.io` | `v1alpha1` | `snapshots` |
| Субресурсы Snapshot | `subresources.state-snapshotter.deckhouse.io` | `v1alpha1` | `snapshots` |
| Объект VolumeSnapshot | `snapshot.storage.k8s.io` | `v1` | `volumesnapshots` |
| Субресурсы VolumeSnapshot | `subresources.storage-foundation.deckhouse.io` | `v1` | `volumesnapshots` |

Для узлов поддерживаются следующие операции:

- `manifests-download` (`GET`) — получить собственные манифесты узла в захваченном виде.
- `manifests-and-children-refs-upload` (`POST`) — загрузить собственные манифесты узла и ссылки на его прямые дочерние
  узлы при импорте.
- `manifests-with-data-restoration` (`GET`) — получить поддерево с манифестами, подготовленными к применению.

Группа эндпоинтов узлов, добавленных другими модулями Deckhouse Platform, определяется по `apiVersion` ссылки на
дочерний узел. Для виртуальных машин, их дисков и других таких узлов к API-группе добавляется префикс `subresources.`, а
версия остаётся той же, что у узла. API-ресурс вида узла находится через обнаружение API, как описано [в разделе «Узлы
видов, заранее неизвестных клиенту»](#узлы-видов-заранее-неизвестных-клиенту).

{{< alert level="warning" >}}
Для VolumeSnapshot это правило не действует. VolumeSnapshot относится к API-группе `snapshot.storage.k8s.io`, но его
эндпоинты находятся в группе `subresources.storage-foundation.deckhouse.io/v1`, указанной в таблице выше.
{{< /alert >}}

Следующие пути используют имя узла и его неймспейс, но различаются группами API. HTTP-методы входят в контракт:

```text
GET  /apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/{namespace}/snapshots/{name}/manifests-download
POST /apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/{namespace}/snapshots/{name}/manifests-and-children-refs-upload
GET  /apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/{namespace}/snapshots/{name}/manifests-with-data-restoration
GET  /apis/subresources.storage-foundation.deckhouse.io/v1/namespaces/{namespace}/volumesnapshots/{name}/manifests-download
POST /apis/subresources.storage-foundation.deckhouse.io/v1/namespaces/{namespace}/volumesnapshots/{name}/manifests-and-children-refs-upload
GET  /apis/subresources.storage-foundation.deckhouse.io/v1/namespaces/{namespace}/volumesnapshots/{name}/manifests-with-data-restoration
```

`{namespace}` — неймспейс узла, `{name}` — имя объекта узла.

Форматы запросов и ответов приведены [в разделе «Тела запросов и ответов»](#тела-запросов-и-ответов), а коды ошибок — [в
разделе «Ошибки и повторы»](#ошибки-и-повторы).

Кластерные объекты, в которых модуль хранит содержимое снимка, относятся к внутренней реализации. Для чтения и переноса
снимка обращайтесь к объектам в неймспейсе и их субресурсам, а не к SnapshotContent, ManifestCheckpoint и другим
внутренним объектам.

## Создание резервной копии

Чтобы создать резервную копию, [создайте снимок](#создание-снимка),
[проверьте его состояние](#проверка-состояния-снимка) и [скачайте содержимое](#скачивание-снимка).

### Создание снимка

Создайте новый Snapshot в неймспейсе, состояние которого требуется захватить. Спецификация неизменяема,
а состояние неймспейса захватывается один раз. Пустая спецификация выбирает режим `Capture`:

```yaml
apiVersion: state-snapshotter.deckhouse.io/v1alpha1
kind: Snapshot
metadata:
  name: my-namespace-snapshot
  namespace: my-app
spec: {}
```

Объекты с лейблом `state-snapshotter.deckhouse.io/exclude` исключаются, если лейбл установлен до захвата.
Правила отбора описаны [в разделе «Что попадает в снимок»](user_guide.html#что-попадает-в-снимок).

### Проверка состояния снимка

Проверяйте `status.conditions` Snapshot через API Kubernetes. Снимок считается успешно созданным только при условии
`Ready=True` с reason `Completed`. В таблице перечислены терминальные и восстановимые причины при `Ready=False`:

| Reason | Класс | Значение |
|---|---|---|
| `ArtifactMissing` | Терминальный | Артефакт с данными захваченного тома отсутствует или удаляется. Восстановить данные тома из этого снимка нельзя |
| `ChildSnapshotLost` | Терминальный | Узел дерева утрачен безвозвратно |
| `ChildrenFailed` | Терминальный | Захват дочернего узла завершился ошибкой, после которой захват не повторяется. Причина указана в reason этого узла |
| `CreateChildFailed` | Терминальный | Не удалось создать узел дерева |
| `DomainCaptureFailed` | Терминальный | Захват поддерева, добавленного другим модулем, завершился ошибкой. Причина, которую вернул этот модуль, указана в сообщении условия |
| `DuplicateCoveredPVCUID` | Терминальный | Один и тот же том попал в план захвата дважды, поэтому захват отклонён |
| `GraphPlanningFailed` | Терминальный | Не удалось спланировать дерево снимка |
| `ListFailed` | Терминальный | Не удалось составить список объектов неймспейса для захвата из-за ошибки, после которой захват не повторяется. Нечитаемый тип ресурсов, например без прав RBAC, и временные ошибки API-сервера к этому reason не приводят. В таких случаях захват ждёт и повторяется |
| `ManifestCheckpointFailed` | Терминальный | Не удалось сохранить захваченные манифесты |
| `NamespaceNotFound` | Терминальный | Захватываемый неймспейс не существует |
| `VolumeCaptureFailed` | Терминальный | Не удалось захватить данные тома |
| `ChildSnapshotDeleted` | Восстановимый | Объект узла дерева удалён, но сохранённые данные не потеряны. Захват остаётся завершённым, но узел больше недоступен в неймспейсе. Доступ к узлу восстанавливается только вручную |

Действуйте в зависимости от класса reason:

- **Терминальный.** Снимок уже не перейдёт в состояние `Ready=True`. Прекратите опрос и сообщите об ошибке. Для
  повторного захвата создайте новый снимок.
- **Восстановимый.** Захваченные данные сохранены, но объект находится в деградированном состоянии, и повторный опрос
  его не изменит. Состояние восстанавливается только вручную.
- **Значения, которых нет в таблице.** Это промежуточные состояния или значения, которые вернул контроллер другого
  модуля. Новые значения могут появляться без предупреждения. Продолжайте опрос, пока меняется хотя бы одно из
  следующего:

  - reason или сообщение условия;
  - поле `status.boundSnapshotContentName`;
  - список дочерних узлов.

  Если изменений нет, прекратите опрос по собственному тайм-ауту и сообщите об ошибке.

### Скачивание снимка

Для скачивания обойдите дерево снимка [по
`status.childrenSnapshotRefs`](cr.html#snapshot-v1alpha1-status-childrensnapshotrefs) и получите собственные манифесты
каждого узла через `manifests-download`. Адресация узлов и HTTP-методы описаны [в разделе
«Структура снимка и API узлов»](#структура-снимка-и-api-узлов).

#### Чтение манифестов узлов

Захваченные манифесты читаются через агрегированный субресурс модуля. Необработанные данные внутренних чанков
хранилища напрямую недоступны. Следующая команда получает манифесты корневого узла Snapshot и выводит их количество:

```shell
d8 k get --raw "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/my-app/snapshots/my-namespace-snapshot/manifests-download" | jq 'length'
```

Эндпоинт `manifests-download` возвращает манифесты только своего узла и не обходит дерево снимка. Манифесты
возвращаются в захваченном виде, вместе со `status` и другими служебными полями.

Остальное дерево читается по одному запросу на узел. Поле
[`status.childrenSnapshotRefs`](cr.html#snapshot-v1alpha1-status-childrensnapshotrefs) прочитанного узла содержит ссылки
на его прямые дочерние узлы. Каждый дочерний узел читается так же, через агрегированную API-группу своего вида, а его
собственный `status.childrenSnapshotRefs` позволяет продолжить обход.

Группа и версия API дочернего узла зависят от его вида, а не от API-группы родительского узла. Например, дочерний узел
вида CSI VolumeSnapshot, содержащий данные обычного PVC, обслуживается модулем
[`storage-foundation`](/modules/storage-foundation/). Для него используются группа
`subresources.storage-foundation.deckhouse.io`, версия `v1` и ресурс `volumesnapshots`. Следующая команда получает
манифесты дочернего узла VolumeSnapshot и выводит их количество:

```shell
d8 k get --raw "/apis/subresources.storage-foundation.deckhouse.io/v1/namespaces/my-app/volumesnapshots/<NODE_NAME>/manifests-download" | jq 'length'
```

где `<NODE_NAME>` — имя дочернего узла из `status.childrenSnapshotRefs` родительского узла.

Имена эндпоинтов для узлов Snapshot и узлов тома одинаковые, но различаются группа API, версия и ресурс. Для узлов
Snapshot используется версия `v1alpha1`, для узлов тома — `v1`. Путь с неверной версией не обслуживается.

#### Узлы видов, заранее неизвестных клиенту

Кроме Snapshot и VolumeSnapshot, в дереве снимка могут быть узлы видов снимков других модулей Deckhouse Platform,
например снимки виртуальных машин и их дисков. Модуль может добавить новый вид снимка в любом релизе, и с этого момента
в снимках неймспейсов появятся узлы этого вида без каких-либо изменений на стороне клиента. Не храните в клиенте список
поддерживаемых видов: обрабатывайте каждый дочерний узел по одним и тем же правилам, используя только `apiVersion`,
`kind` и `name` его ссылки в `status.childrenSnapshotRefs`.

Для каждой ссылки на дочерний узел:

1. Найдите API-ресурс вида узла. Запросите обнаружение API для группы и версии из `apiVersion` ссылки,
   `GET /apis/{group}/{version}`, и возьмите `name` той записи в `resources`, у которой `kind` совпадает с `kind`
   ссылки, а `name` не содержит `/`. Запись с `/`, например `{resource}/status`, — это субресурс того же вида. Если
   группа API не обслуживается или подходящей записи нет, вид не обслуживается в кластере: остановите скачивание и
   сообщите об этом узле.
2. Прочитайте объект узла: `GET /apis/{group}/{version}/namespaces/{namespace}/{resource}/{name}`. Поле
   `status.childrenSnapshotRefs` этого объекта перечисляет дочерние узлы этого узла, а поле `status.data` используется
   на шаге 4. Перед скачиванием проверьте готовность этого узла и дождитесь ожидаемого дескриптора данных по
   правилам [передачи данных томов](#передача-данных-томов).
3. Получите манифесты узла через `manifests-download` в группе эндпоинтов узла, как описано [в разделе
   «Структура снимка и API узлов»](#структура-снимка-и-api-узлов), с API-ресурсом, найденным на шаге 1.
4. Если вид узла предполагает данные тома, передайте их после появления полного `status.data`, как описано [в разделе
   «Передача данных томов»](#передача-данных-томов).
5. Обработайте так же дочерние узлы из шага 2.

Если один из этих запросов для узла завершился ошибкой, которую [раздел «Ошибки и повторы»](#ошибки-и-повторы) не
предписывает повторять, остановите скачивание и сообщите об этом узле: без него скачанный снимок неполон.

Следующий пример обрабатывает дочерний узел вымышленного вида `DatabaseSnapshot` из группы API
`databases.example.com`, версия `v1alpha1`. Его ссылка в `status.childrenSnapshotRefs` родительского узла:

```yaml
status:
  childrenSnapshotRefs:
    - apiVersion: databases.example.com/v1alpha1
      kind: DatabaseSnapshot
      name: <NODE_NAME>
```

Команды берут только значения из ссылки, поэтому одинаково работают для узла любого вида, кроме VolumeSnapshot:

```shell
NAMESPACE=my-app
API_VERSION=databases.example.com/v1alpha1
KIND=DatabaseSnapshot
NAME="<NODE_NAME>"

GROUP="${API_VERSION%/*}"
VERSION="${API_VERSION#*/}"

# Шаг 1: API-ресурс вида узла, в этом примере databasesnapshots.
RESOURCE=$(d8 k get --raw "/apis/${GROUP}/${VERSION}" \
  | jq -r --arg kind "$KIND" '.resources[] | select(.kind == $kind and (.name | contains("/") | not)) | .name')

# Шаг 2: дочерние узлы и данные тома узла.
d8 k get --raw "/apis/${GROUP}/${VERSION}/namespaces/${NAMESPACE}/${RESOURCE}/${NAME}" \
  | jq '{children: .status.childrenSnapshotRefs, data: .status.data}'

# Шаг 3: манифесты узла.
d8 k get --raw "/apis/subresources.${GROUP}/${VERSION}/namespaces/${NAMESPACE}/${RESOURCE}/${NAME}/manifests-download" | jq 'length'
```

Замените `<NODE_NAME>` на `name` из ссылки. Если CSD этого вида требует данные тома, дождитесь полного `data`
в выводе шага 2 и создайте DataExport, в `targetRef` которого указаны группа `databases.example.com`, вид
`DatabaseSnapshot` и имя узла. Повторите команды для каждой ссылки из `children`, подставив значения этой ссылки.

### Передача данных томов

Ожидаемость данных тома определяется видом узла: VolumeSnapshot всегда несёт данные тома, доменный вид — если
в его CustomSnapshotDefinition задано `requiresDataArtifact: true`, а Snapshot не имеет собственных данных тома.
Поле `status.data` содержит параметры ожидаемых данных, а не определяет, есть ли у узла том.

Перед скачиванием проверяйте готовность каждого узла (`Ready=True` с причиной `Completed`). Для VolumeSnapshot и видов с
`CustomSnapshotDefinition.spec.requiresDataArtifact: true` дополнительно дождитесь полного `status.data`, включая
`size`, `volumeMode` и `storageClassName`. Отсутствие ожидаемого дескриптора означает незавершённую подготовку или ошибку,
а не отсутствие данных тома.
По тайм-ауту завершайте операцию ошибкой.

Для узла с данными тома манифестов недостаточно: передайте данные тома через DataExport модуля `storage-foundation`. Создайте DataExport для узла,
дождитесь готовности и скачайте данные по адресу из статуса DataExport. В `DataExport.spec.targetRef` укажите сам узел,
а не исходный PVC: группу API из `apiVersion` узла, его `kind` и `name`. Пример для узла VolumeSnapshot:

```yaml
apiVersion: storage-foundation.deckhouse.io/v1alpha1
kind: DataExport
metadata:
  name: export-my-volume-node
  namespace: my-app
spec:
  ttl: 15m
  publish: false
  targetRef:
    group: snapshot.storage.k8s.io
    kind: VolumeSnapshot
    name: my-volume-node
```

Адреса экспорта, режим публикации для доступа извне кластера и проверка сертификата описаны [в разделе
«Экспорт и импорт содержимого томов по HTTP»](/modules/storage-foundation/usage.html#экспорт-и-импорт-данных-томов)
документации модуля `storage-foundation`.

Сохраните размер, режим и StorageClass тома из полей `status.data.size`, `status.data.volumeMode` и
`status.data.storageClassName` узла: они понадобятся при загрузке резервной копии. Правила определения размера описаны
[в разделе «Размер тома»](#размер-тома).

{{< alert level="warning" >}}
Если модуль `storage-foundation` не включён, данные томов узлов скачать или загрузить нельзя. Снимки состояния
неймспейсов без томов при этом остаются доступны.
{{< /alert >}}

### Размер тома

Размер захваченного тома нужен при загрузке снимка в кластер и при сборке архива. Берите его из поля `status.data.size`
узла, который содержит данные тома. Поле есть у узла VolumeSnapshot и у узла, добавленного другим модулем, если у этого
узла есть собственный том. У узлов без данных тома, например у корневого Snapshot, поля `status.data` нет.

Значение задаётся в формате Kubernetes quantity и определяет ёмкость тома на принимающей стороне. Сравнивается размер, а
не его запись: например, `10Gi` и `10240Mi` считаются одинаковыми.

Не определяйте размер по полю `PersistentVolumeClaim.spec.resources.requests.storage` из скачанного манифеста. Это
запрошенный размер тома, и он может быть меньше фактически выделенного.

Сохраните размер при скачивании узла, а при загрузке резервной копии берите сохранённое значение: исходного узла к
этому моменту может уже не быть. В формате архива сохранённое значение — `volumes[].size` в `snapshot.yaml`. Значение
передаётся без изменений и нигде не пересчитывается:

```text
status.data.size узла  →  volumes[].size в snapshot.yaml  →  DataImport.spec.storageParams.size
```

Поле `status.data` у VolumeSnapshot — расширение Deckhouse Platform, в CSI API его нет. Оно содержит данные из
SnapshotContent, к которому у клиента нет доступа, и позволяет получить описание захваченного тома обычным запросом
`GET` в неймспейсе снимка.

Формирование архива описано [на странице «Работа с архивами снимков»](snapshot_archives.html).

### Снимки отдельных ресурсов

#### Отдельный PVC: VolumeSnapshot без CustomSnapshotDefinition

VolumeSnapshot (`snapshot.storage.k8s.io/v1`) не регистрируется через CustomSnapshotDefinition. Предлагайте снимки PVC
как отдельный вариант, даже если список CSD пуст. Для этой интеграции нужны модуль `storage-foundation` с расширенным
API VolumeSnapshot и CSI-драйвер с поддержкой снимков.

Создайте новый VolumeSnapshot в неймспейсе исходного PVC. В отличие от доменных видов из CSD, он использует
`VolumeSnapshot.spec.source.persistentVolumeClaimName`, а не `sourceRef`. Укажите VolumeSnapshotClass, совместимый с
CSI-драйвером PVC; выбор класса описан [в разделе «Использование снапшотов»](/modules/storage-foundation/usage.html#использование-снимков)
документации `storage-foundation`.

```text
POST /apis/snapshot.storage.k8s.io/v1/namespaces/{namespace}/volumesnapshots?fieldValidation=Strict
GET  /apis/snapshot.storage.k8s.io/v1/namespaces/{namespace}/volumesnapshots/{name}
```

Пример объекта (в POST-запросе передайте его JSON-представление):

```yaml
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata:
  name: my-pvc-snapshot
  namespace: my-app
spec:
  volumeSnapshotClassName: <VOLUME_SNAPSHOT_CLASS>
  source:
    persistentVolumeClaimName: my-pvc
```

Замените `<VOLUME_SNAPSHOT_CLASS>` выбранным классом. Создавайте объект после включения `storage-foundation` и
убедитесь, что на исходном PVC нет лейбла `state-snapshotter.deckhouse.io/exclude`: для исключённого PVC создаётся
обычный CSI-снимок без захвата манифестов, необходимого этой интеграции. Для самостоятельного корня не задавайте
в `ownerReferences` ссылку на узел снимка: модуль считает её ссылкой на родителя.
Право `create` на `volumesnapshots` для снимка PVC и для импорта даёт роль `upload_snapshots` из таблицы ролей.
Для готовности резервной копии дождитесь `Ready=True` с причиной `Completed` в `status.conditions`; одного CSI-поля
`status.readyToUse` недостаточно для подтверждения готовности захваченных манифестов. Дополнительно дождитесь полного
`status.data` по правилам [передачи данных томов](#передача-данных-томов). Скачайте манифесты узла через
`subresources.storage-foundation.deckhouse.io/v1`, а данные — через DataExport, как описано
[в разделе «Передача данных томов»](#передача-данных-томов). Загружайте его как самостоятельный VolumeSnapshot в режиме
импорта [по контракту загрузки выбранных узлов](#выбор-узлов-и-поддеревьев-для-загрузки).

#### Доменные виды из CustomSnapshotDefinition

Помимо целого неймспейса, другие модули Deckhouse Platform могут захватывать отдельный ресурс, например виртуальную
машину. Такой модуль регистрирует пару «вид ресурса → вид снимка» объектом CustomSnapshotDefinition уровня кластера.
Чтобы узнать, какие ресурсы можно захватить по отдельности, читайте список этих объектов, а не прописывайте виды во
внешней системе:

```text
GET /apis/state-snapshotter.deckhouse.io/v1alpha1/customsnapshotdefinitions
```

Для чтения нужна роль, описанная [в разделе
«Настройка доступа внешней системы к кластеру»](#настройка-доступа-внешней-системы-к-кластеру).

| Поле | Значение |
|---|---|
| `CustomSnapshotDefinition.spec.source.apiVersion`, `CustomSnapshotDefinition.spec.source.kind` | Вид ресурса, который можно захватить |
| `CustomSnapshotDefinition.spec.apiVersion`, `CustomSnapshotDefinition.spec.kind` | Вид его снимка |
| `CustomSnapshotDefinition.spec.requiresDataArtifact` | При `true` узлы этого вида несут данные тома |

Используйте определение, только если `status.conditions[type=Ready].status` равен `True`, а
`status.conditions[type=Ready].observedGeneration` равен `metadata.generation`.

Пример определения, которое регистрирует модуль виртуализации:

```yaml
apiVersion: state-snapshotter.deckhouse.io/v1alpha1
kind: CustomSnapshotDefinition
metadata:
  name: virtualization-virtual-machine-snapshot
  generation: 1
spec:
  apiVersion: virtualization.deckhouse.io/v1alpha2
  kind: VirtualMachineSnapshot
  requiresDataArtifact: false
  source:
    apiVersion: virtualization.deckhouse.io/v1alpha2
    kind: VirtualMachine
status:
  conditions:
    - type: Ready
      status: "True"
      observedGeneration: 1
```

Чтобы захватить ресурс, создайте объект вида снимка в неймспейсе ресурса. API-ресурс вида снимка, нужный в пути запроса,
находится, как описано [в разделе «Узлы видов, заранее неизвестных клиенту»](#узлы-видов-заранее-неизвестных-клиенту),
по группе, версии и виду из `CustomSnapshotDefinition.spec.apiVersion` и `CustomSnapshotDefinition.spec.kind`. Все виды
снимков, зарегистрированные через CustomSnapshotDefinition, принимают одинаковую спецификацию: поле `mode` со значением
`Capture` и поле `sourceRef` с `apiVersion`, `kind` и `name` ресурса из того же неймспейса. Остальные поля спецификации
могут не требовать явного задания, но значения по умолчанию влияют на захват. Например, для VirtualMachineSnapshot
по умолчанию действует `requiredConsistency: true`: снимок работающей машины без агента не создаётся; машина должна
быть выключена либо агент должен успешно заморозить её. Значение полей описано в документации модуля, которому
принадлежит вид снимка:

```yaml
apiVersion: virtualization.deckhouse.io/v1alpha2
kind: VirtualMachineSnapshot
metadata:
  name: my-vm-snapshot
  namespace: my-app
spec:
  mode: Capture
  sourceRef:
    apiVersion: virtualization.deckhouse.io/v1alpha2
    kind: VirtualMachine
    name: my-vm
```

При создании используйте параметр `fieldValidation=Strict`, как и для Snapshot. Созданный объект — корневой узел
собственного дерева снимка. Отслеживайте его состояние и скачивайте его так же, как Snapshot: условие `Ready=True` с
причиной `Completed` в `status.conditions` означает завершённый захват, дочерние узлы перечислены в
`status.childrenSnapshotRefs`, а эндпоинты узлов адресуются, как описано [в разделе
«Структура снимка и API узлов»](#структура-снимка-и-api-узлов).

Снимок отдельного ресурса загружается обратно так же, как снимок неймспейса, по шагам [из раздела
«Загрузка резервной копии в кластер»](#загрузка-резервной-копии-в-кластер). Отличие одно: корневой узел — объект вида
снимка с `spec.mode: Import`, а не Snapshot. Команда `d8 snapshot download` такой снимок пока не
скачивает, а `d8 snapshot upload` принимает корнем целого архива только Snapshot: узел с данными тома можно загрузить
отдельным корнем только с `--node`.

## Выбор узлов и поддеревьев для загрузки

СРК должна давать пользователю просмотр сохранённого дерева снимка и выбор любого узла как корня загрузки, включая
VolumeSnapshot, снимок виртуальной машины и другие доменные виды. В графическом интерфейсе предоставьте галочки или
другой явный способ выбора. Загрузка всего дерева неймспейса не должна быть единственным вариантом.
Поддерживайте также выбор нескольких независимых поддеревьев в одной операции.

Поддерево состоит из выбранного узла и всех его потомков. Выбор листа загружает только этот узел; выбор нелистового
узла включает всех его потомков вместе с данными их томов. Покажите полный состав выбора до начала загрузки.
Одновременный выбор предка и потомка не должен приводить к повторной загрузке потомка. Невыбранные предки и соседние
ветви не загружаются. Например, выбор одной виртуальной машины из резервной копии неймспейса загружает её снимок и
поддеревья её дисков, без Snapshot неймспейса и других виртуальных машин; выбор одного диска загружает только снимок
этого диска.

Целевой порядок для каждого выбранного поддерева использует [процедуру загрузки](#загрузка-резервной-копии-в-кластер)
по следующим правилам:

1. Создайте выбранный корень в целевом неймспейсе с сохранёнными `apiVersion`, `kind` и именем и с `spec.mode: Import`.
   Не переносите из резервной копии поля, которые при захвате указывали на снимаемый объект: `sourceRef`,
   `VolumeSnapshot.spec.source`, а у видов модуля виртуализации — `virtualMachineName` и `virtualDiskName`. При загрузке
   данные берутся из резервной копии, а не с живого объекта. Не копируйте исходные `ownerReferences` на родителя и не
   создавайте искусственный родительский Snapshot.

   Например, у сохранённого VirtualMachineSnapshot из примера выше в `spec` записаны `mode: Capture` и `sourceRef` на
   машину, а для загрузки создаётся объект только с `mode: Import`:

   ```yaml
   apiVersion: virtualization.deckhouse.io/v1alpha2
   kind: VirtualMachineSnapshot
   metadata:
     name: my-vm-snapshot
     namespace: my-app-restored
   spec:
     mode: Import
   ```

2. Создайте потомков в том же неймспейсе с `ownerReferences` на их новых родителей. Используйте UID новых родителей,
   а не UID из резервной копии.
3. Загрузите собственные сохранённые манифесты каждого узла и его прямые `childRefs`; загрузите данные тома каждого
   узла, у которого они сохранены. Сохраните внутренние связи и имена выбранного поддерева. Не оставляйте ссылки на
   узлы вне загружаемого поддерева и не пропускайте потомка, сохраняя ссылку на него.
4. Дождитесь `Ready=True` с причиной `Completed` у каждого выбранного корня. Проверьте каждый узел поддерева,
   включая полный `status.data` для всех узлов с сохранёнными данными тома, по правилам
   [передачи данных томов](#передача-данных-томов). Явно сообщайте об ошибке или неполной загрузке каждого поддерева.

Например, самостоятельный корень VolumeSnapshot создаётся без `ownerReferences` и без `VolumeSnapshot.spec.source`:

```yaml
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata:
  name: standalone-volume
  namespace: my-app-restored
spec:
  mode: Import
```

Дождитесь привязки узла и загрузите его собственные сохранённые манифесты с пустым `childRefs`:

```text
POST /apis/subresources.storage-foundation.deckhouse.io/v1/namespaces/my-app-restored/volumesnapshots/standalone-volume/manifests-and-children-refs-upload
```

```json
{
  "manifests": [
    {"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": {"name": "my-pvc", "namespace": "my-app"}}
  ],
  "childRefs": []
}
```

В примере манифест PVC сокращён. В `manifests` передайте полный массив объектов из `manifests-download` этого узла
без изменений.
Создайте один DataImport со ссылкой `snapshotRef` на этот VolumeSnapshot:

```yaml
apiVersion: storage-foundation.deckhouse.io/v1alpha1
kind: DataImport
metadata:
  name: import-standalone-volume
  namespace: my-app-restored
spec:
  mode: PopulateData
  ttl: 1h
  snapshotRef:
    apiVersion: snapshot.storage.k8s.io/v1
    kind: VolumeSnapshot
    name: standalone-volume
  storageParams:
    storageClassName: my-storage-class
    size: 10Gi
    volumeMode: Block
```

Подставьте сохранённые размер и режим тома и подходящий StorageClass целевого кластера. Передайте данные через HTTP API
`storage-foundation`, вызовите `finished` и дождитесь `DataImport.status.phase: Completed`, как описано
[в разделе «Загрузка резервной копии в кластер»](#загрузка-резервной-копии-в-кластер). Затем дождитесь `Ready=True`
с причиной `Completed` и полного `status.data`, включая `size`, `volumeMode` и `storageClassName`;
по тайм-ауту завершайте операцию ошибкой. Для этого примера требуется расширенный VolumeSnapshot API модуля
`storage-foundation`, описанный в том же разделе.

Выбор относится к узлам снимка, а не к отдельным манифестам внутри узла. Не обрезайте манифесты доменного снимка и не
удаляйте необходимых ему потомков ради реализации галочек: снимок виртуальной машины может зависеть от снимков её
дисков. Эти требования относятся к интеграции СРК через API узлов; команды работы с архивом и поддерживаемые ими
режимы загрузки описаны отдельно [на странице «Работа с архивами снимков»](snapshot_archives.html).

## Загрузка резервной копии в кластер

Перед восстановлением загрузите сохранённое состояние в целевой неймспейс. В режиме `Import` Snapshot формируется из
данных внешней системы и не захватывает состояние активного неймспейса.

1. Создайте корневой Snapshot с `spec.mode: Import`:

   ```yaml
   apiVersion: state-snapshotter.deckhouse.io/v1alpha1
   kind: Snapshot
   metadata:
     name: my-namespace-snapshot
     namespace: my-app-restored
   spec:
     mode: Import
   ```

   Запрос на создание и проверка полей описаны [в разделе «Операции с объектом Snapshot»](#операции-с-объектом-snapshot).
   Прежде чем создавать дочерние узлы, проверьте, что в сохранённом объекте `spec.mode` равен `Import`.

1. Создайте объекты для дочерних узлов в том же неймспейсе. Для узлов томов создайте VolumeSnapshot в режиме импорта и
   свяжите его с родительским узлом через `ownerReferences`:

   ```yaml
   apiVersion: snapshot.storage.k8s.io/v1
   kind: VolumeSnapshot
   metadata:
     name: my-volume-node
     namespace: my-app-restored
     ownerReferences:
       - apiVersion: state-snapshotter.deckhouse.io/v1alpha1
         kind: Snapshot
         name: my-namespace-snapshot
         uid: <ROOT_SNAPSHOT_UID>
   spec:
     mode: Import
   ```

   Вместо `<ROOT_SNAPSHOT_UID>` укажите UID родительского узла. По ссылке в `ownerReferences` сборщик мусора Kubernetes
   удаляет дочерние узлы и связанные с ними данные при удалении корневого Snapshot. Удаление выполняется без
   подтверждения, и отменить его нельзя. Узел без этой ссылки не удаляется вместе с деревом и остаётся в неймспейсе.

   Оставьте поле `VolumeSnapshot.spec.source` пустым, как в примере: данные тома загружаются через DataImport, поэтому
   источник данных не указывается.

   {{< alert level="warning" >}}
   VolumeSnapshot с пустым `VolumeSnapshot.spec.source` поддерживается только с расширенным CRD VolumeSnapshot и
   `snapshot-controller` из модуля `storage-foundation`. После завершения импорта модуль связывает VolumeSnapshot с
   VolumeSnapshotContent, созданным при импорте, и записывает его имя в `status.boundVolumeSnapshotContentName`. С
   upstream-версией `snapshot-controller`, которая не поддерживает режим `Import`, этот сценарий не работает.
   {{< /alert >}}

   Для узла другого модуля создайте объект его вида так же: с `spec.mode: Import` и записью в `ownerReferences` на
   родительский узел. Эта запись нужна каждому дочернему узлу не только для удаления: по ней модуль
   связывает узел с родителем. Укажите
   `apiVersion`, `kind`, `name` и UID нового родителя: с UID из резервной копии узел не привязывается. Дочерний узел
   без такой записи загружается как самостоятельный корень; присоединение его содержимого к родителю отклоняется.
   Свежий родитель не достигает `Ready=True`, а удаление родителя не распространяется на такого ребёнка. Клиент должен
   проверить `ownerReferences` созданных узлов: успешная загрузка и ранее опубликованное `Ready=True` не подтверждают
   правильность структуры. API-ресурс вида узла, нужный в пути запроса, находится, как описано [в разделе «Узлы видов, заранее
   неизвестных клиенту»](#узлы-видов-заранее-неизвестных-клиенту).

1. Загрузите манифесты каждого узла и ссылки на его прямые дочерние узлы в `manifests-and-children-refs-upload`.
   Тело запроса содержит JSON-массивы `manifests` и `childRefs`. Для узла без дочерних узлов `childRefs` пуст.
   Эндпоинт корневого Snapshot:

   ```text
   POST /apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/{namespace}/snapshots/{name}/manifests-and-children-refs-upload
   ```

   Для VolumeSnapshot используйте группу `subresources.storage-foundation.deckhouse.io/v1` и ресурс `volumesnapshots`.
   Для узла другого модуля используйте группу API узла с префиксом `subresources.`, как описано [в разделе «Структура
   снимка и API узлов»](#структура-снимка-и-api-узлов), и ресурс его вида, найденный, как описано [в разделе «Узлы
   видов, заранее неизвестных клиенту»](#узлы-видов-заранее-неизвестных-клиенту). Формат запроса приведён [в разделе
   «Тела запросов и ответов»](#тела-запросов-и-ответов). Если узел ещё не привязан к сохранённому содержимому, сервер
   может вернуть `409`. Повторяйте запрос с задержкой в пределах заданного времени ожидания; по тайм-ауту
   завершайте загрузку ошибкой. Повторная отправка тех же данных допустима.

1. Для каждого узла с данными тома создайте один DataImport в целевом неймспейсе и передайте данные через HTTP API
   модуля `storage-foundation`. Пример для дочернего VolumeSnapshot с именем `my-volume-node`:

   ```yaml
   apiVersion: storage-foundation.deckhouse.io/v1alpha1
   kind: DataImport
   metadata:
     name: import-my-volume-node
     namespace: my-app-restored
   spec:
     mode: PopulateData
     ttl: 1h
     snapshotRef:
       apiVersion: snapshot.storage.k8s.io/v1
       kind: VolumeSnapshot
       name: my-volume-node
     storageParams:
       storageClassName: my-storage-class
       size: 10Gi
       volumeMode: Block
   ```

   Требования к DataImport:

   - `DataImport.spec.snapshotRef` указывает на импортируемый узел, а не на исходный PVC. Для узла другого модуля
     укажите его `apiVersion`, `kind` и `name`.
   - Для одного узла создавайте только один DataImport. Несколько DataImport для одного узла не позволяют связать узел
     с импортированными данными.
   - Поля `storageClassName` и `size` обязательны. Размер берите сохранённый при скачивании узла, как описано [в разделе
     «Размер тома»](#размер-тома).
   - Сохраняйте исходный `volumeMode` (`Block` или `Filesystem`): берите значение, сохранённое из
     `status.data.volumeMode` захваченного узла, в формате архива — `volumes[].volumeMode` в `snapshot.yaml`. Поле
     необязательное в схеме DataImport, но без него используется `Filesystem`. **Для блочного тома обязательно задавайте
     `Block` явно.**
   - Выберите StorageClass в целевом кластере с поддержкой снимков и аннотацией
     `storage.deckhouse.io/volumesnapshotclass`, связывающей его с VolumeSnapshotClass. Исходный StorageClass —
     значение, сохранённое из `status.data.storageClassName` захваченного узла, в формате архива —
     `volumes[].storageClassName` в `snapshot.yaml`. Если он недоступен, выберите подходящий. Настройка описана [в
     документации storage-foundation](/modules/storage-foundation/).

   После передачи данных вызовите эндпоинт `finished` у DataImport. Передача последнего байта сама по себе не
   завершает импорт.
   Дождитесь, пока `DataImport.status.phase` станет `Completed`, затем проверьте корневой Snapshot.
   Подробности HTTP-загрузки, TTL и обработки ошибок приведены
   [в разделе «Экспорт и импорт содержимого томов по HTTP»](/modules/storage-foundation/usage.html#экспорт-и-импорт-данных-томов).

1. Дождитесь `Ready=True` с причиной `Completed` у корневого Snapshot. Перед проверочным скачиванием проверьте
   готовность каждого узла и полный ожидаемый `status.data`, как описано
   [в разделе «Передача данных томов»](#передача-данных-томов).

Перед загрузкой архива, совместимого с `d8`, проверьте его
[по правилам страницы «Работа с архивами снимков»](snapshot_archives.html).

## Чтение снимка для восстановления

При интеграции восстановления через API запрашивайте подготовленные манифесты через
`manifests-with-data-restoration`. Ответ содержит манифесты и ссылки на снимки томов, необходимые для восстановления. Не создавайте вручную служебные
объекты модуля, например VolumeRestoreRequest.

`manifests-download` возвращает объекты в захваченном виде, вместе со `status` и служебными метаданными, и не
предназначен для прямого применения. Используйте его, чтобы просмотреть или скачать содержимое снимка.

Чтобы получить объекты, готовые к применению, прочитайте снимок через эндпоинт восстановления
`/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/{namespace}/snapshots/{name}/manifests-with-data-restoration`
(`GET`). По умолчанию эндпоинт **рекурсивен**: возвращает адресованный узел и его потомков одним ответом.
В отличие от `manifests-download`, он подготавливает манифесты для восстановления. Например:

```shell
d8 k get --raw "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/my-app/snapshots/my-namespace-snapshot/manifests-with-data-restoration" | jq .
```

Перед возвратом манифесты подготавливаются к применению:

- удаляются служебные метаданные (`uid`, `resourceVersion`, `ownerReferences`, `managedFields` и другие);
- удаляется `status`;
- удаляются поля, которые назначает кластер, например `clusterIP` у Service и `nodeName` у Pod;
- объекты адресуются в неймспейс запрошенного снимка;
- каждый PVC ссылается на соответствующий VolumeSnapshot через `PersistentVolumeClaim.spec.dataSourceRef`.

Эндпоинт не умеет исключать ссылки на снимки томов из подготовленных манифестов. GET-запрос только читает и
подготавливает объекты, но не применяет манифесты и не восстанавливает данные томов.

Чтение поддерева по умолчанию проходит границы модулей, поэтому эндпоинт достаточно один раз вызвать для узла
Snapshot. Дочерние узлы, включая VolumeSnapshot, обрабатываются соответствующими модулями и возвращаются в том же
ответе, так что вручную обходить дерево не требуется. При этом результат формируется целиком. Если хотя бы один узел
поддерева не готов, запрос завершается ошибкой без возврата частичного содержимого.

Все объекты в ответе адресуются в неймспейс запрошенного Snapshot, а выбрать другой целевой неймспейс нельзя. Запрос с
другим неймспейсом возвращает код `400`. Чтобы восстановить содержимое в другом неймспейсе, сначала скачайте снимок,
загрузите его в нужный неймспейс и прочитайте его там. Отдельно переадресовать данные тома также нельзя.
`PersistentVolumeClaim.spec.dataSourceRef` не содержит неймспейс, поэтому PVC ищет указанный VolumeSnapshot в своём
неймспейсе, а VolumeSnapshot остаётся в неймспейсе снимка.

Ответ возвращается как обычный JSON-массив объектов, а не как объект `List` или поток YAML. Клиент должен обработать
элементы массива и применить их самостоятельно, а не передавать всё тело ответа напрямую в `apply -f -`.

Модуль только предоставляет подготовленные объекты. Их применение остаётся на стороне клиента, см. [раздел
«Что не входит в интеграцию»](#что-не-входит-в-интеграцию).

Для применения возвращённых объектов нужны права на создание или изменение объектов соответствующих типов. Роль
`restore_snapshots` даёт право читать подготовленные манифесты, но не права на их применение. Требования к правам
для команды `d8 snapshot restore` описаны [в разделе «Необходимые права»](user_guide.html#необходимые-права).

{{< alert level="warning" >}}
Ответ эндпоинта содержит всё поддерево снимка, включая узлы других модулей Deckhouse Platform. Права проверяются один
раз при обращении к эндпоинту, а содержимое поддерева не фильтруется по правам вызывающей стороны. Поэтому роль
`restore_snapshots`, которая входит и в ClusterRole агента резервного копирования, даёт право читать всё содержимое
снимка в неймспейсе, включая объекты, которые ServiceAccount не может читать напрямую. Назначайте эту роль только в тех
неймспейсах, к содержимому которых внешней системе нужен доступ.
{{< /alert >}}

## Тела запросов и ответов

Эндпоинты узлов принимают и возвращают JSON. Ниже описаны тела запросов и ответов для скачивания и загрузки узла.

### manifests-download

Ответ передаётся с `Content-Type: application/json`. Тело ответа — JSON-массив объектов, а не объект `List` Kubernetes и
не поток YAML. Массив содержит собственные объекты адресованного узла в захваченном виде, вместе со `status` и
служебными метаданными:

```json
[
  {
    "apiVersion": "v1",
    "kind": "ConfigMap",
    "metadata": {
      "name": "app-config",
      "namespace": "my-app",
      "uid": "<UID>",
      "resourceVersion": "<RESOURCE_VERSION>",
      "managedFields": []
    },
    "data": {
      "example": "value"
    }
  }
]
```

### manifests-and-children-refs-upload

Запрос передаётся с `Content-Type: application/json`. Пример тела запроса:

```json
{
  "manifests": [ { "apiVersion": "v1", "kind": "ConfigMap", "metadata": {} } ],
  "childRefs": [
    { "apiVersion": "snapshot.storage.k8s.io/v1", "kind": "VolumeSnapshot", "name": "my-volume-node" }
  ]
}
```

Тело запроса должно соответствовать следующим требованиям:

- Поле `manifests` обязательно. Его значение — JSON-массив, который `manifests-download` вернул для этого узла. Если
  поле отсутствует, равно `null` или имеет другой тип, запрос отклоняется с кодом `400`.
- Поле `childRefs` содержит ссылки только на прямые дочерние узлы. Каждая ссылка содержит `apiVersion`, `kind` и `name`,
  а при отсутствии любого из этих полей запрос отклоняется с кодом `400`. Неймспейс в ссылке не указывается: дочерние
  узлы находятся в неймспейсе снимка. Для узла без дочерних узлов список пуст.
- Узел VolumeSnapshot не может иметь дочерних узлов. Непустой `childRefs` для него отклоняется с кодом `400`.
- Размер тела запроса не должен превышать 64 MiB. Ограничение относится к собственным манифестам одного узла, а не ко
  всему дереву. Запрос с телом большего размера отклоняется с кодом `413`.

Для узлов Snapshot и VolumeSnapshot возвращается одинаковый ответ — код `200` и объект `Status` Kubernetes:

```json
{
  "kind": "Status",
  "apiVersion": "v1",
  "status": "Success",
  "manifestCheckpointName": "..."
}
```

Поле `manifestCheckpointName` — внутренняя запись модуля о сохранённых манифестах. Для клиента значим только
`"status": "Success"`.

Повторная загрузка с тем же телом не приводит к ошибке, поэтому запрос с неизвестным результатом, например после обрыва
соединения или тайм-аута, можно загрузить снова. После того как тело для узла сохранено, не загружайте для этого узла
другое тело: содержимое узла фиксируется при первой загрузке.

## Ошибки и повторы

Ошибку, которую формирует API Kubernetes, описывает объект `Status` с полями `code`, `reason` и `message`. В `message`
указан объект, к которому относится ошибка. Обрабатывайте ошибки по полю `reason`, а не по тексту `message`: сообщение
предназначено для человека. У транспортной ошибки тело `Status` может отсутствовать.

Ниже перечислены ответы эндпоинтов узлов: `manifests-download`, `manifests-and-children-refs-upload` и
`manifests-with-data-restoration`. Создание снимка и чтение его статуса выполняются по общим правилам API-сервера
Kubernetes. Данные томов передаются через HTTP-сервер модуля `storage-foundation`, и его коды ответов описаны [в разделе
«Экспорт и импорт содержимого томов по HTTP»](/modules/storage-foundation/usage.html#экспорт-и-импорт-данных-томов)
документации этого модуля.

| Код | Когда возвращается | Что делать |
|---|---|---|
| `400` | Некорректное тело запроса на загрузку или неверный параметр запроса | Исправьте тело или параметры запроса |
| `403` | Недостаточно прав, неверный HTTP-метод или ошибка в адресе. Также возвращается, если узел ссылается на SnapshotContent, который не содержит обратной ссылки на этот узел | Проверьте метод, адрес и привязки ролей. Если причина в несогласованной ссылке на SnapshotContent, повтор не поможет |
| `404` | В неймспейсе нет объекта с указанным именем | Проверьте неймспейс и имя объекта |
| `405` | Неверный HTTP-метод при наличии прав на оба метода. Для чтения используется `GET`, для загрузки — `POST` | Исправьте метод |
| `409` | Узел ещё не готов к операции или возник конфликт состояния. Возможные причины: SnapshotContent ещё не привязан, объект не в режиме `Import`, сохранённые манифесты узла ещё недоступны для чтения, один объект указан в манифестах узла дважды. До привязки SnapshotContent загрузка возвращает `reason: ImportContentNotBound`, а чтение — `reason: Conflict` | При временной неготовности повторяйте с задержкой в пределах заданного времени, затем завершайте ошибкой. Если объект не в `Import`, создайте новый объект в этом режиме. Дубли объектов или ошибочные сохранённые манифесты требуют нового импорта с исправленным содержимым: повтор не меняет уже сохранённые манифесты |
| `413` | Тело запроса на загрузку превышает 64 MiB | Можно убрать пробелы из JSON без изменения содержимого. Если тело всё ещё превышает лимит, завершите операцию ошибкой. Не удаляйте манифесты и не делите загрузку одного узла на несколько запросов |
| `500` | Внутренняя ошибка модуля при обработке запроса, `reason: InternalError` | Повторите запрос с задержкой. Если ошибка повторяется, проверьте логи контроллера `state-snapshotter` |
| `502` | Только для узлов VolumeSnapshot и узлов других модулей: модуль, который обслуживает узел, не получил ответа от агрегированного API `state-snapshotter` | Повторите запрос с задержкой |
| `503` | Агрегированный API временно не обслуживает запросы: модуль запускается, перезапускается или ещё не загрузил реестр снимков | Повторите запрос с задержкой |

Ошибка в адресе или HTTP-методе обычно возвращает `403`, а не `404` или `405`. Права назначаются на конкретные
субресурсы в конкретных API-группах и проверяются по URL до маршрутизации запроса. HTTP-метод проверяется как глагол
RBAC: `GET` — как `get`, `POST` — как `create`. Роли модуля дают только `get` на эндпоинты скачивания и только `create`
на эндпоинты загрузки. Поэтому опечатка в имени эндпоинта, запрос через группу, которая его не обслуживает, и неверный
метод не проходят проверку прав.

Если `403` возвращается для адреса, который выглядит правильным, проверьте по порядку HTTP-метод, адрес и привязки
ролей. Список ролей приведён [в разделе
«Настройка доступа внешней системы к кластеру»](#настройка-доступа-внешней-системы-к-кластеру). Если сразу после
установки модуля `403` возвращают все запросы, повторите их через некоторое время: агрегированная роль заполняется
автоматически.
