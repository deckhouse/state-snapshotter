## ManifestCaptureRequest / ManifestCheckpoint — инфраструктура для сохранения манифестов объектов

> **Модуль `state-snapshotter`:** MCR/MC и unified registry — разные треки; нормативные выдержки — [`docs/state-snapshotter-rework/spec/system-spec.md`](../docs/state-snapshotter-rework/spec/system-spec.md), план — [`design/implementation-plan.md`](../docs/state-snapshotter-rework/design/implementation-plan.md).

# 1. Контекст и проблема

В Deckhouse появляется всё больше сценариев, где нужно:

* атомарно «сфотографировать» набор Kubernetes-объектов (CR’ы, Service’ы, и т.п.) в рамках одного namespace;  
* сохранить эту фотографию в стабильном виде;  
* затем привязать её к доменному `*SnapshotContent` (например, `VirtualMachineSnapshotContent`);  
* при этом:  
  * не выдавать контроллерам лишних прав на чтение всего кластера;  
  * не ломать RBAC — инициатор не должен получить доступ к объектам, к которым у него нет прав;  
  * **сохранённый снапшот должен переживать удаление namespace** (или полное выпиливание всех объектов в нём).

Важные требования к архитектуре:

* **Единственный namespaced ресурс в этой схеме — `ManifestCaptureRequest`.** Всё, что содержит сами манифесты (checkpoint и chunks), а также инфраструктура удержания (ObjectKeeper), должны быть **cluster-scoped**, чтобы переживать удаление namespace.  
* Chunk’и тяжёлые и служебные:  
  * никто лишний не должен иметь к ним доступ;  
  * по ним категорически нельзя давать `list`/`watch` (никаких ClusterRole/Role с такими правами, кроме минимально необходимого для контроллера — и без `list/watch`).
* Реализация планируется в модуле `state-snapshotter`.

# 2. Цели и ограничения

### Цели

1. Дать контроллерам Deckhouse (например, виртуализации) способ запросить сохранение манифестов набора объектов в **namespace с полезной нагрузкой** через единый API — `ManifestCaptureRequest` (namespaced).  
2. Обеспечить, чтобы контроллер-инициатор запроса **не мог** через этот механизм получить доступ к объектам, на которые у него нет RBAC-прав.  
3. Хранить результат захвата в отдельном кластерном handle — `ManifestCheckpoint` (cluster-scoped), на который можно:  
   * ссылаться из доменных `*SnapshotContent`;  
   * использовать при экспорт/импорт Snapshot’ов.  
4. Поддержать хранение большого количества объектов через чанки (`ManifestCheckpointChunk`), не упираясь в лимит размера одного Kubernetes-объекта.  
5. Обеспечить управляемый жизненный цикл:  
   * `ManifestCaptureRequest` — короткоживущий, автоматически удаляется;  
   * `ManifestCheckpoint` и чанки — cluster-scoped и живут столько, сколько их удерживают ObjectKeeper’ы и/или SnapshotContent.  
6. **Гарантировать, что при удалении namespace сохранённые ManifestCheckpoint/Chunk’и остаются**, и, соответственно, снапшоты можно экспортировать/импортировать даже после удаления исходного namespace.
7. Сохранить прозрачность для внешних компонентов при возможной замене хранения чанков на иную реализацию (например, на уровне etcd) без изменения их контракта.

### Не цели

* Описывать полностью формат экспорт/импорт Snapshot’ов.  
* Делать транзакционный «консистентный» срез по всем объектам namespace на уровне etcd.  
* Описывать реализацию будущего варианта на уровне apiserver/etcd (копирование ключей).

## **3. Принятое решение (high-level)**

Вводим три основных CRD:

1. `ManifestCaptureRequest` — **namespaced**  
   Запрос от контроллера: какие объекты в этом namespace нужно «сфотографировать».  
2. `ManifestCheckpoint` — **cluster-scoped**  
   Handle на сохранённую копию манифестов, с минимумом метаданных и ссылками на чанки.  
3. `ManifestCheckpointContentChunk` — **cluster-scoped**  
   Хранит фактические манифесты в удобном компактном формате (несколько объектов в одном чанке).

Плюс используем уже существующую концепцию:

* `ObjectKeeper` — **cluster-scoped**, описывает, что и до каких пор нужно удерживать (см. в отдельном ADR).

`ManifestCheckpointController`:

1. Следит за `ManifestCaptureRequest`.  
2. По каждому запросу:  
   * проверяет статус, избегает повторной обработки;  
   * читает перечисленные в spec объекты;  
   * создаёт `ManifestCheckpoint` (cluster-scoped) и набор `ManifestCheckpointContentChunk` (cluster-scoped);  
   * создаёт `ObjectKeeper`, связывающий `ManifestCaptureRequest` и `ManifestCheckpoint`;  
  * проставляет `ObjectKeeper` owner’ом у `ManifestCheckpoint`;  
  * обновляет статус `ManifestCaptureRequest` (conditions/Ready + имя `ManifestCheckpoint`).  
3. TTL-логика удаляет `ManifestCaptureRequest` через 10 минут (что приводит к тому, что удаляется `ObjectKeeper`, и если у `ManifestCheckpoint` не добавлены другие owner’ы, то и он).

Использующие контроллеры (например, виртуализация):

1. Создают `ManifestCaptureRequest` в своём namespace (обычно один для одного *Snapshot’а).  
2. Ждут готовности.  
3. Берут имя `ManifestCheckpoint` из статуса MCR.  
4. Создают свой `*SnapshotContent`, ссылаясь на checkpoint.  
5. Проставляют ownerRef в `ManifestCheckpoint` на свой `*SnapshotContent`.

# 4. Детальный дизайн

## 4.1. API: ManifestCaptureRequest (namespaced)

**Scope:** namespaced.

```yaml
apiVersion: deckhouse.io/v1alpha1  
kind: ManifestCaptureRequest  
metadata:  
  name: <задаётся клиентом>  
  namespace: <ns контроллера>  
spec:  
  targets:  
    - apiVersion: <group/version>  
      kind: <Kind>  
      name: <name>  
      # namespace не указывается, подразумевается namespace MCR  
status:  
  checkpointName: <string>   # имя cluster-scoped ManifestCheckpoint  
  observedGeneration: <int64>  
  message: <string>  
  completionTimestamp: <time>  
  conditions: []Condition  # Ready

```

Ограничения:

* `targets` — только namespaced-объекты **в том же namespace**, что и сам `ManifestCaptureRequest`.  
* Количество `targets` может быть большим; манифесты в MCR не хранятся — только ссылки.

### Admission-логика

Validating Admission Webhook для `ManifestCaptureRequest`:

* при `CREATE`:  
  * для каждого `spec.targets[i]` выполняется `SelfSubjectAccessReview` с действием `get` на соответствующий объект (apiGroup, resource, namespace, name);  
  * если хотя бы один объект недоступен инициатору — запрос **отклоняется** (HTTP 403 с понятным сообщением).

Это гарантирует, что контроллер с глобальными правами не станет каналом для обхода RBAC инициатором.

## 4.2. API: ManifestCheckpoint (cluster-scoped)

**Scope:** cluster-scoped. Создаётся **только** `ManifestCheckpointController`.

Имя генерируется автоматически (по аналогии с `VolumeSnapshotContent`):

* непрозрачный рандомный идентификатор, сложный для подбора.

Пример схемы:

```yaml
apiVersion: deckhouse.io/v1alpha1  
kind: ManifestCheckpoint  
metadata:  
  name: mcp-3h5k2l9s-7qfzc  
  # cluster-scoped, namespace отсутствует  
  ownerReferences:  
    - apiVersion: deckhouse.io/v1alpha1  
      kind: ObjectKeeper  
      name: <retainer-name>  
      uid: <...>  
spec:  
  # Логическая привязка к namespace и запросу  
  sourceNamespace: <ns MCR>  
  manifestCaptureRequestRef:  
    name: <mcr-name>  
    namespace: <ns MCR>  
    uid: <uid MCR>  
status:  
  chunks:  
    - name: mcp-3h5k2l9s-7qfzc-0  
      index: 0  
      objectsCount: 10  
      sizeBytes: 12345  
      checksum: <sha256-of-compressed-chunk>  
    - name: mcp-3h5k2l9s-7qfzc-1  
      index: 1  
      objectsCount: 7  
      sizeBytes: 9876  
      checksum: <sha256-of-compressed-chunk>  
  totalObjects: 17  
  totalSizeBytes: 22221  
  conditions: []Condition  # Ready  
  message: <string>
```

Ключевые моменты:

* `ManifestCheckpoint` **не привязан** к namespace на уровне Kubernetes.  
  Вместо этого мы явно храним `sourceNamespace` и `manifestCaptureRequestRef` в `spec`.  
* Это обеспечивает:  
  * сохранность checkpoint’а при удалении namespace;  
  * возможность для экспорт/импорт-логики понимать, откуда он появился.

## 4.3. API: ManifestCheckpointContentChunk (cluster-scoped)

**Scope:** cluster-scoped. Создаётся **только** `ManifestCheckpointController`.

Имя — производное от имени checkpoint + индекс:

* `mcp-<id>-0`, `mcp-<id>-1`, ...

Пример схемы:

```yaml
apiVersion: deckhouse.io/v1alpha1  
kind: ManifestCheckpointContentChunk  
metadata:  
  name: mcp-3h5k2l9s-7qfzc-0  
  # cluster-scoped, namespace отсутствует  
  # shortName: mcpchunk  
  ownerReferences:  
    - apiVersion: deckhouse.io/v1alpha1  
      kind: ManifestCheckpoint  
      name: mcp-3h5k2l9s-7qfzc  
      uid: <...>  
spec:  
  checkpointName: mcp-3h5k2l9s-7qfzc  
  index: 0  
  # формат данных:  
  # список объектов, закодированных как gzipped JSON, base64  
  data: <base64(gzip(json[]))>  
  objectsCount: 10  
  checksum: <sha256-of-compressed-chunk>  
  # Ограничение размера как в реализации: data <= 1MiB (validation MaxLength=1048576)
```

### Subresource `/manifests` ресурса `ManifestCheckpoint`, реализованный через APIService группы `subresources.state-snapshotter.deckhouse`


Назначение: дать внутренним контроллерам удобный способ получить весь снимок без знаний о чанках.

* Доступ: только `get` на сабресурс `manifests` ресурса `manifestcheckpoints` (ClusterRole для наших системных контроллеров; пользователям не выдаётся).
* Результат: отдавать единым ответом список объектов снапшота, но без полей .status и .metadata.managedFields (при этом в архиве манифесты эти поля сохраняются).
  Предпочтительный формат для удобной работы в Go:
  - `application/json`: массив kube-объектов в стандартном JSON (аналог `[]unstructured.Unstructured`, каждый элемент — полноформатный объект). Это легко декодировать через `json.Decoder` в `[]map[string]interface{}` или `[]unstructured.Unstructured`.
  - Допускается `Content-Encoding: gzip` для крупных ответов (прозрачно для клиентов).
* Валидация/логика:
  - Сабресурс собирает данные из всех чанков указанного checkpoint’а в порядке `index`, проверяя checksum при склейке.
  - При отсутствии checkpoint — `NotFound`. При повреждении/несогласованности чанков — `InternalError` с сообщением.
* Группа сабресурса: по паттерну из ADR про subresources — `subresources.state-snapshotter.deckhouse.io` (для ресурса из модуля `state-snapshotter`), версия та же, путь `/apis/subresources.state-snapshotter.deckhouse.io/<version>/manifestcheckpoints/<name>/manifests`.

### Формат хранения манифестов

На MVP:

* собираем список объектов со всеми полями в JSON-массив `[]` (Kubernetes-объекты в стандартном JSON API-формате);  
* выполняем `gzip`;  
* результат кодируем в base64 и кладём в `.spec.data`.

Размер чанка контроллер ограничивает так, чтобы гарантированно не вылезти за лимиты apiserver/etcd.

## 4.4. TTL ManifestCaptureRequest

Отдельная логика (в том же контроллере или отдельном):

* периодически (раз в несколько минут) сканирует MCR;  
* для MCR в состояниях `Ready/Failed`, у которых `completionTimestamp +` 10 минут `< now()`:  
  * удаляет MCR.

Удаление namespace удаляет и MCR, но **не трогает** cluster-scoped `ManifestCheckpoint` и Chunk’и.

## 4.5. Использование другими контроллерами

### Общий алгоритм

1. Контроллер (например, виртуализации) создаёт `ManifestCaptureRequest` в namespace с полезной нагрузкой.  
2. Admission-плагин проверяет права инициатора (этого контроллера) на каждый `target`. При нарушении RBAC запрос отклоняется.  
3. `ManifestCheckpointController`, увидев новый MCR без готового checkpoint:  
   * читает указанные объекты;  
   * в случае ошибок заполняет condition Ready=False с понятным `message`;  
   * при успехе:  
     * формирует набор runtime-объектов;  
     * разбивает их на чанки;  
     * создаёт cluster-scoped `ManifestCheckpoint` с рандомным именем;  
  * создаёт cluster-scoped `ManifestCheckpointContentChunk`’и;  
     * создаёт `ObjectKeeper`, связывающий `ManifestCaptureRequest` и `ManifestCheckpoint`;  
     * проставляет `ObjectKeeper` owner’ом `ManifestCheckpoint`;  
     * обновляет статус MCR:  
       * condition Ready=True;  
       * `checkpointName = <checkpoint-name>`;  
       * `completionTimestamp = now()`.

### Детальный пример

Сценарий:

1. Контроллер виртуализации хочет сделать `VirtualMachineSnapshot`.  
2. Сначала он делает всю свою логику по дискам (не часть этого ADR).  
3. Для сохранения метаданных:  
   * создаёт `ManifestCaptureRequest`:

```yaml
apiVersion: deckhouse.io/v1alpha1  
kind: ManifestCaptureRequest  
metadata:  
  name: vm-<snapshot-id>  
  namespace: <vm-namespace>  
spec:  
  targets:  
    - apiVersion: virtualization.deckhouse.io/v1alpha1  
      kind: VirtualMachine  
      name: my-vm  
    - apiVersion: virtualization.deckhouse.io/v1alpha1  
      kind: VirtualMachineIPAddress  
      name: my-vm-ip  
    - apiVersion: virtualization.deckhouse.io/v1alpha1  
      kind: VirtualMachineBlockDeviceAttachment  
      name: my-vm-da-1  
    # ...
```

4. Ждёт, пока condition `Ready` станет `True` (с таймаутом/ретраями).  
5. Забирает `checkpointName` из `status.checkpointName`.

Создаёт cluster-scoped `VirtualMachineSnapshotContent`:

```yaml
spec:  
  manifestCheckpointName: <checkpointName>  # cluster-scoped`
```

6. После успешного создания `VirtualMachineSnapshotContent`:  
   * делает `get` cluster-scoped `ManifestCheckpoint` по имени;  
   * добавляет ownerRef на `VirtualMachineSnapshotContent`:

```yaml
ownerReferences:  
  - apiVersion: virtualization.deckhouse.io/v1alpha1  
    kind: VirtualMachineSnapshotContent  
    name: <vm-snap-content-name>  
    uid: <...>
```

Таким образом:

* доменный контроллер **знает имя** своего checkpoint’а только через собственный MCR;  
* при удалении namespace MCR может исчезнуть, но cluster-scoped VirtualMachineSnapshotContent и ManifestCheckpoint останутся.

## 4.6. RBAC-модель

### ManifestCaptureRequest (namespaced)

Потребляющие контроллеры (виртуализация и т.п.):

* в своём namespace получают:  
  * `create`, `get`, `list`, `watch` на `ManifestCaptureRequest`.

MCR не содержит содержимого манифестов, только ссылки и имя checkpoint’а, живёт недолго.

### ManifestCheckpoint (cluster-scoped)

Потребляющие контроллеры:

* получают **ClusterRole** с правами:  
  * `get`, `update` на ресурс `manifestcheckpoints.deckhouse.io` (cluster-scoped);  
  * `get` на сабресурс `manifests` ресурса `manifestcheckpoints`;  
  * **без** `list` и `watch`.

Особенность: с точки зрения RBAC такой ClusterRole даёт `get` на любой checkpoint, но:

* имена checkpoint’ов рандомизированы и трудно подбираемы (по аналогии с VolumeSnapshotContent);  
* единственный источник имени checkpoint’а — собственный `ManifestCaptureRequest`, который контроллер сам же создал.

Тем самым:

* даже при наличии `get` на все checkpoint’ы контроллер практически ограничен только своими, поскольку чужие имена он не знает;  
* отсутствие `list`/`watch` не позволяет ему обойти эту модель.

### ManifestCheckpointContentChunk (cluster-scoped)

**Особые требования безопасности и производительности:**

* Chunk’и тяжёлые (могут содержать много данных) и служебные.  
* К ним должен иметь доступ только **единственный технический контроллер** (`ManifestCheckpointController`), который:  
  * создаёт Chunk’и;  
  * удаляет их при удалении соответствующего checkpoint’а;  
  * для этого ему достаточно прав:  
    * `create`, `get`, `delete` на `manifestcheckpointcontentchunks.deckhouse.io` (cluster-scoped).  
  * **Ему не нужны** `list` и `watch`:  
    * имена чанков он сам генерирует при создании;  
    * при удалении он получает их список из `ManifestCheckpoint.status.chunks`.

**Жёсткое требование ADR:**

* **Никакие другие ClusterRole/Role (включая потребляющие контроллеры) не должны содержать ресурс `ManifestCheckpointContentChunk`.**  
* На этот ресурс **не должно быть прав `list` и `watch` ни у кого** (кроме, при необходимости, сугубо технического обслуживания, но по умолчанию — вообще ни у кого).  
  * Нет ролей с:

    * `verbs: ["list"]` или `verbs: ["watch"]` для `manifestcheckpointcontentchunks`.  
* Даже `ManifestCheckpointController` может работать без `list`/`watch` по Chunk’ам, опираясь только на `create/get/delete` по известным именам.

Это важно:

* и для безопасности (запрещаем массовое чтение чужих манифестов);  
* и для производительности (никаких watch’ей по тяжёлым ресурсам, никаких массовых list’ов).

## **5. Обоснование решения**

Ключевые моменты:

* Разделение `ManifestCaptureRequest` (namespaced, короткоживущий запрос) и `ManifestCheckpoint` (cluster-scoped, стабильный артефакт) даёт:  
  * локальную точку входа для контроллера (в его namespace);  
  * независимое от namespace хранилище снапшота.  
* Cluster-scoped `ManifestCheckpoint` и Chunk’и:  
  * сохраняются при удалении namespace;  
  * могут использоваться для экспорт/импорт после удаления исходного окружения.  
* Admission-проверка на MCR гарантирует, что инициатор не обходит RBAC.  
* Жёсткая политика по Chunk’ам:  
  * никто не может делать `list`/`watch`;  
  * только системный контроллер умеет их создавать/удалять по имени;  
  * устраняет риск тяжёлых list/watch по огромным объектам.

## **6. Открытые вопросы и будущая работа**

* Возможно ужесточить список типов, которые можно сохранять (например, отдельная политика для Secrets).  
* Вопросы консистентности (resourceVersion и т.п.) — можно развивать отдельно.  
* Экспорт/импорт Snapshot’ов, формат ссылок и политики ObjectKeeper’ов — в отдельных ADR.  
* Исследовать и, при необходимости, позже заменить реализацию Chunk’ов на прямую работу с etcd или внешним хранилищем, сохранив текущий cluster-scoped API.
