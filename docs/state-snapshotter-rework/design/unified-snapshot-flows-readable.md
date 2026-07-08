# Unified snapshot flows: читабельная версия

Это сокращённая версия документа `unified_snapshot_flows_1b0557d9.md`.

Оригинал остаётся источником деталей. Этот файл нужен, чтобы быстро прочитать
и понять модель без frontmatter, todo-списков, ревью-инструкций, веток,
коммитов и мелких implementation-заметок.

## 1. Главная идея

Каждый узел дерева снапшота хранится через общий cluster-scoped
`SnapshotContent`.

Namespaced-ресурсы вроде `Snapshot`, `VolumeSnapshot`,
`VirtualDiskSnapshot` и других доменных снапшотов — это пользовательские
handle-объекты. Durable-состояние лежит ниже:

- `SnapshotContent` — логический узел дерева;
- `ManifestCheckpoint` — манифесты Kubernetes-объектов для этого узла;
- `dataRef` — ссылка на один durable data-артефакт, обычно
  `VolumeSnapshotContent` (VSC), иногда detached `PersistentVolume`;
- `childrenSnapshotContentRefs` — ссылки на дочерние `SnapshotContent`.

Зафиксированное решение: **в одном `SnapshotContent` не больше одного
`dataRef`**. Если у снапшота несколько томов, они становятся отдельными
дочерними узлами, а не массивом `dataRefs` на одном content.

```text
SnapshotContent
  status.manifestCheckpointName -> ManifestCheckpoint
  status.dataRef                -> VSC или PV, максимум один
  status.childrenSnapshotContentRefs[] -> дочерние SnapshotContent
```

## 2. Роли компонентов

`d8` остаётся «толстым» клиентом. Он обходит дерево, создаёт нужные CR,
запускает `DataImport`/`DataExport`, загружает манифесты и данные.

Серверная часть делится так:

- **Common state-snapshotter controller** создаёт и наполняет
  `SnapshotContent`, ставит ownerReferences, управляет root `ObjectKeeper`,
  пишет binding в leaf-ресурсы (`status.boundSnapshotContentName`).
- **Domain controller** остаётся тонким: реконсайлит свои доменные snapshot CR,
  создаёт доменные capture request'ы или child snapshot'ы, пишет только свой
  status (`manifestCaptureRequestName`, `volumeCaptureRequestName`,
  `childrenSnapshotRefs`, domain-specific conditions).
- **storage-volume-data-manager (SVDM)** владеет `DataImport` и `DataExport`.
  Он производит или читает data-артефакты, но не создаёт `SnapshotContent`.
- **storage-foundation** отвечает за volume-механику: VCR/VRR, CSI и VSC.

Ключевая граница: **`SnapshotContent` создаёт и наполняет common controller**,
и на capture, и на import. Доменный контроллер его не трогает.

## 3. Два типа data-листьев

### Generic PVC leaf

Generic PVC представлен расширенным `VolumeSnapshot`.

В CSI `VolumeSnapshot` добавляется наш слой:

- `status.boundSnapshotContentName` — ссылка на наш `SnapshotContent`;
- `spec.source.dataImportName` — источник данных при import.

Стандартное CSI-поле `status.boundVolumeSnapshotContentName` остаётся и
по-прежнему указывает на настоящий CSI `VolumeSnapshotContent`.

На capture VSC создаёт обычный CSI snapshot-controller. Common controller
только привязывает `VolumeSnapshot` к нашему `SnapshotContent`.

На import `VolumeSnapshot` ссылается на `DataImport`. Форкнутый
snapshot-controller такие VS пропускает, а common controller сам выставляет
наш binding и legacy-поля после того, как `DataImport` создал data-артефакт.

### Domain data leaf

Доменные data-ресурсы, например `VirtualDiskSnapshot`, не содержат вложенный
`VolumeSnapshot`.

Они напрямую связываются с `SnapshotContent` через
`status.boundSnapshotContentName`. Данные идут через VCR/VRR, а нижний артефакт
всё равно VSC или PV.

```text
Generic PVC:
  VolumeSnapshot -> SnapshotContent -> VSC

Domain disk:
  VirtualDiskSnapshot -> SnapshotContent -> VSC
```

## 4. Манифесты и метаданные тома

`ManifestCheckpoint` хранит raw-манифесты, включая `status`.
Поля чистятся не при capture, а на restore-read-path.

Это нужно для import: `DataImport` должен восстановить параметры тома из
оригинального манифеста:

- storage class;
- volume mode;
- реальный размер из `status.capacity`.

Эти параметры не дублируются в `DataImport.spec` и leaf spec. На import
`DataImport` читает их из MCP через `manifests-download`.

Исключение — `Secret`: данные секретов не попадают в snapshot по умолчанию.
Полное сохранение секрета требует opt-in аннотации в домене
`state-snapshotter.deckhouse.io`.

## 5. Capture

Capture — это создание snapshot из живых ресурсов.

Общий поток:

1. Пользователь создаёт корневой snapshot.
2. Domain controller создаёт дочерние snapshot-ресурсы, если доменная модель
   требует дерево.
3. Common controller создаёт `SnapshotContent` для каждого узла.
4. Manifest capture пишет raw-манифесты в `ManifestCheckpoint`.
5. Data capture производит VSC/PV и публикует ссылку в
   `SnapshotContent.status.dataRef`.
6. `SnapshotContent.Ready` становится `True`, когда готовы манифесты, данные и
   дети.

Residual/orphan PVC не кладутся вторым `dataRef` на root. Каждый такой PVC
становится отдельным дочерним volume-узлом.

## 6. Export

Export проходит по существующему snapshot-дереву и выгружает манифесты плюс
данные.

1. `d8` начинает с root snapshot и идёт по `status.childrenSnapshotRefs`.
2. Для каждого узла вызывает `manifests-download`, который отдаёт только
   манифесты этого узла.
3. Для каждого data-листа создаёт `DataExport`.
4. `DataExport` резолвит данные так:

```text
targetRef -> leaf snapshot resource
          -> status.boundSnapshotContentName
          -> SnapshotContent.status.dataRef
          -> VSC/PV
          -> VRR/export PVC
          -> bytes stream
```

`DataExport.targetRef` должен стать generic-ссылкой вида
`{group, resource, name}`. Использовать `kind` хуже: dynamic-клиенту нужен
resource path, а `kind -> resource` не всегда однозначен.

Bare VSC экспортировать напрямую нельзя. Пользователь должен ссылаться на
namespaced snapshot-ресурс, а артефакт берётся из доверенного
`SnapshotContent`.

## 7. Import

Import восстанавливает snapshot-дерево из внешнего bundle.

`d8` создаёт:

- root и child snapshot CR;
- `DataImport` для data-листьев;
- ownerReferences между parent и child snapshot CR.

Для каждого узла `d8` загружает:

- манифесты этого узла;
- ссылки на прямых детей.

Common controller по этому upload создаёт/обновляет `SnapshotContent`:

- пишет `manifestCheckpointName`;
- пишет `childrenSnapshotContentRefs`;
- пишет `childrenSnapshotRefs` в namespaced leaf, чтобы export и restore могли
  пройти импортированное дерево.

Data-лист импортируется в два прохода:

1. Сначала upload манифестов создаёт `SnapshotContent` и MCP. В этот момент
   `dataRef` ещё может быть пустым.
2. `DataImport` читает MCP/манифесты leaf-ресурса, достаёт параметры тома,
   принимает загруженные байты, создаёт VSC/PV и пишет
   `status.dataArtifactRef`.
3. Common controller читает
   `leaf.spec.dataSource -> DataImport.status.dataArtifactRef`, пишет
   `SnapshotContent.status.dataRef` и привязывает leaf к content.

Для доменного data-листа связь выглядит так:

```text
VirtualDiskSnapshot.spec.dataSource -> DataImport
```

Для generic PVC:

```text
VolumeSnapshot.spec.source.dataImportName -> DataImport
```

Целевой контракт `DataImport` один для всех leaf-kind'ов:

```text
DataImport.spec.targetRef         -> leaf snapshot resource
DataImport.spec.dataArtifactType  -> VolumeSnapshotContent или PersistentVolume
DataImport.status.dataArtifactRef -> созданный VSC/PV
```

## 8. Restore

Restore должен стать рекурсивным и per-node.

Вместо одного endpoint'а, который возвращает всё дерево, каждый узел отдаёт
свои данные через `manifests-with-data-restoration`. Вызов на root рекурсивно
собирает детей.

Для одного узла:

1. Прочитать raw-манифесты через `manifests-download`.
2. Очистить их для restore: убрать `status`, runtime-поля, `managedFields`,
   generated PVC-поля и т.п.
3. Применить доменный in-process transform, если он нужен.
4. Рекурсивно пройти детей из `status.childrenSnapshotRefs`.
5. Подключить восстановление данных через `SnapshotContent.status.dataRef`:
   VRR или extended `VolumeSnapshot` connector.

Core маршрутизирует child-вызовы по `apiVersion`/`kind` ребёнка и доверенному
CSD-реестру. Доменный контроллер рекурсит по своим `childrenSnapshotRefs`, но
не читает `SnapshotContent` напрямую.

Старый агрегатный endpoint, который отдаёт всё поддерево сразу, остаётся только
до перехода на этот per-node restore.

## 9. GC и retention

GC строится по ownerReferences, а не по status-полям.

Root content держит root `ObjectKeeper`:

```text
ObjectKeeper(FollowObjectWithTTL -> root snapshot) -> root SnapshotContent
```

После удаления root snapshot данные попадают в TTL-корзину. Когда TTL истекает,
дерево удаляется каскадом по ownerReferences.

Базовый граф:

```text
root ObjectKeeper -> root SnapshotContent
parent SnapshotContent -> child SnapshotContent
SnapshotContent -> ManifestCheckpoint
SnapshotContent -> VSC/PV
```

На import есть ещё временный execution keeper:

```text
ObjectKeeper(FollowObject -> DataImport) -> produced VSC
```

После handoff VSC также получает ownerRef на `SnapshotContent`. Это защищает от
orphan-VSC между upload и durable binding.

VSC форсится в `Retain`, пока он используется, чтобы удаление временного VS или
request'а не удалило backend snapshot. Физический reclaim планируется в
teardown `SnapshotContent`: переключить артефакт в `Delete`, удалить его,
дождаться CSI reclaim и снять finalizer.

## 10. Security

Нижний data-артефакт cluster-scoped, поэтому прямые пользовательские ссылки на
VSC опасны.

Import безопасен так: пользователь создаёт namespaced `DataImport` и leaf
snapshot resource, а cluster-scoped VSC/PV создаёт привилегированный
контроллер. Право импортировать проверяется через create на namespaced
ресурсах.

Export безопасен только если начинается с namespaced snapshot resource:

```text
leaf.status.boundSnapshotContentName -> SnapshotContent.status.dataRef
```

Контроллер не должен принимать bare VSC name от пользователя.

Для API транспорта целевое состояние — нормальный Kubernetes aggregated
apiserver:

- user и inter-server вызовы идут через kube-apiserver;
- authn/authz делегируются стандартными механизмами;
- самописную реализацию requestheader/mTLS нужно заменить genericapiserver,
  сохранив штатную front-proxy security model.

## 11. Что уже сделано

Фундамент уже есть:

- domain controller вынесен в отдельный модуль/процесс;
- common controller владеет `SnapshotContent`;
- domain controller стал тонким;
- root keeper TTL и ownerRef-based GC для capture;
- MCP хранит raw-манифесты со `status`;
- secret data исключаются по умолчанию;
- в `SnapshotContent` один `dataRef`, не массив;
- residual PVC представлены дочерними volume-узлами;
- extended `VolumeSnapshot` уже имеет наш binding status и import source;
- snapshot-controller пропускает `VolumeSnapshot` с источником `DataImport`.

## 12. Что осталось

Оставшиеся работы сгруппированы крупно.

### Import API

Добавить import-mode и per-node upload/download:

- `spec.source.import` для структурных import-узлов;
- `manifests-and-children-refs-upload`;
- per-node `manifests-download`;
- старый aggregated manifests оставить только до перехода restore на
  рекурсивную модель.

### DataImport redesign

Переделать SVDM `DataImport` в generic producer VSC/PV:

- targetRef указывает на snapshot leaf resource, а не на PVC template;
- тип артефакта явный: VSC или PV;
- параметры тома берутся из MCP raw-манифестов;
- результат публикуется как `status.dataArtifactRef`.

Существующие SVDM prototype-ветки полезны только как reference для keeper,
`Retain` и CSI client-кода. Их публичная CRD-схема не должна стать финальным
контрактом.

### Import orchestration и GC

Common controller должен материализовать импортный `SnapshotContent`, заполнить
MCP, children и data binding, привязать extended `VolumeSnapshot` import и
реализовать physical reclaim в teardown.

### DataExport redesign

Сделать `DataExport` resource-agnostic:

- generic targetRef `{group, resource, name}`;
- резолв любого snapshot leaf через `boundSnapshotContentName`;
- VSC/PV и параметры читать из доверенного `SnapshotContent.dataRef`;
- экспортировать через VRR.

### API transport

Перевести core/domain API с custom HTTPS mux/requestheader логики на настоящий
generic apiserver с delegated authn/authz.

### Restore recursion

Заменить whole-subtree aggregated restore на recursive per-node restore:

- каждый узел отдаёт свои манифесты и рекурсивно детей;
- domain controllers рекурсят по `childrenSnapshotRefs`;
- core делегирует доменные узлы в domain apiserver.

## 13. Короткая модель в голове

```text
d8 создаёт snapshot resources и DataImport/DataExport.

Каждый snapshot node:
  namespaced leaf CR
    -> status.boundSnapshotContentName
    -> cluster SnapshotContent
       -> MCP с манифестами
       -> один dataRef на VSC/PV
       -> child SnapshotContent refs

Common controller владеет SnapshotContent.
Domain controller владеет domain status и domain children.
SVDM владеет data upload/download.
GC идёт по ownerReferences, не по status fields.
```
