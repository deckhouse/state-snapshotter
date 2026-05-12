## Универсальный паттерн XxxxSnapshot / XxxxSnapshotContent

> **Модуль `state-snapshotter`:** для кода и тестов см. [`docs/state-snapshotter-rework/spec/system-spec.md`](../docs/state-snapshotter-rework/spec/system-spec.md) и [`design/implementation-plan.md`](../docs/state-snapshotter-rework/design/implementation-plan.md).

### 1. Контекст и проблема
Мы наращиваем stateful‑функционал платформы, а для stateful всегда очень важна возможность бэкапа и восстановления. Этот ADR определяет стандарт реализации снимков (snapshot’ов) в платформе: создание снимков, удаление снимков,  восстановление из снимка, скачивание снимка (экспорт) и загрузка снимка (импорт).

Ожидаемый эффект: единый простой UX для пользователей (CLI/UI) и чёткое предсказуемое API для систем резервного копирования (СРК).

При этом:
- Настоящий стандарт определяет:
  - общие требования к снимкам, общую логику поведения всех видов снимков и их жизненный цикл;
  - часть функционала и логики, которая реализовывается общей частью механизма снимков;
  - требования к реализации домен-специфичной части логики создания снимков.
- Разные контроллеры (виртуализация, managed services и т.п.) самостоятельно реализовывают домен-специфичной часть в процессе снятия/восстановления снимков.


### 2. High-level решение

Снимок (Snapshot), в общем случае — это снимок состояния какого-то объекта (или набора объектов), достаточный для восстановления такого объекта (или набора объектов) к такому сохраненному состоянию (обычно через пересоздание). Он включает конфигурацию (манифесты) и данные (если объект, для которого делается снимок, имеет данные).

Примеры снимков, которые могут быть реализованы (или уже реализованы):
- На уровне проектов/неймспейсов, для полезной нагрузки:
  - Снимок PersistenVolumeClaim;
  - Снимок VirtualDisk;
  - Снимок VirtualMachine;
  - Снимок Postgres;
  - Снимок всего namespace (или части, выбранной лейблами).
- На уровне системы:
  - Сником ModuleConfig.
  - Снимок конфигурации системы.
  - Снимок мониторинговых данных.

Соответственно снимки это общий подход, применимый как на уровне проектов/неймспейсов, так и для системных задач. Соответственно далее под пользователем подразумевается обе категории пользователей Deckhouse (пользователи/администраторы системы и пользователи/администраторы проектов/неймспейсов). 

Основные кейсы использования снимков:
- Пользователь создает снимок, чтобы "сохраниться" перед "опасной операцией". Обычно, если "операция" прошла успешно — пользователь удаляет такой снимок.
- Пользователь создает снимок, чтобы сделать "резервную копию". Такие снимки могут жить продолжительное время, пока не потеряют актуальность с точки зрения пользователя.
- Пользователь создает снимок, чтобы его скачать и положить во внешнее хранилище "резервных копий", чтобы можно было восстановиться в случае катастрофы.
- Пользователь создает снимок, чтобы "сохранить результаты наработок/экспериментов", а развернутые "наработки" можно было "свернуть" (удалить).
- Пользователь создает снимок, чтобы перенести объект в другой кластер (скачать снимок, загрузить в другой кластер и там восстановить).
- Пользователь создает снимок, чтобы сделать "клон" объекта, потому что объект не поддерживает возмодность прямого клонирования.
- Пользователь создает снимок, чтобы сделать "золотой образ" объекта, который использовать для массового создания объектов.
- Пользователь настраивает стороннее СРК, чтобы оно подключалось по API и создавало снимки, а когда они готовы — скачивало их.
- Пользователь настраивает встроенное СРК, чтобы оно создавало снимки по расписанию (и очищало старые снимки по определенным политикам).
- Пользователь настраивает встроенное СРК, чтобы оно автоматически загружало созданные снимки во внешнее хранилище "резервных копий" (например, по протоколам NFS и S3).

Общие требования для всех снимков:
- Один снимок всегда содержит конфигурацию/манифесты (достаточные для восстановления объекта, для которого делается снимок) и может содержать ОДНИ И ТОЛЬКО ОДНИ данные (если у объекта есть данные). Если объект состоит из нескольких данных (например, у виртуальной машины несколько дисков) — создается иерархия снимков, где родительский снимок имеет список дочерних снимков (например, снимок виртуальной машины содержит ее конфигурацию, а так же список снимков дисков, каждый из которых содержит конфигурацию диска и данные диска).
- Все снимки всегда можно экспортировать и импортировать унифицированным образом.
- Экспорт или восстановление снимка не должны приводить к порче снимка.
- При удалении снимка его содержимое не должно удаляться сразу, а должно сохраняться в "корзине" (время очистки которой регулируется администратором кластера).
- Содержимое снимка сделанного в рамках неймспейса/проекта должно сохраняться за переделами этого неймспейса/проекта, чтобы при удалении неймспейса/проекта сработал механизм "корзины".
- При восстановлении у пользователя должен быть выбор:
  - восстановить только данные — в этом случае он сам создает целевые объекты (описав их конфигурацию/манифесты) и в них ссылается на снимки в качестве источника данных;
  - восстановить только конфигурацию/манифесты — в этом случае пользователь получает конфигурацию/манифесты из снимка и самостоятельно ее применяет (с использованием API, или с использованием CLI/UI, в том числе отдельных удобных дополнительных мастеров восстановления);
  - восстановить и конфигурацию и данные — в этом случае пользователя получает подготовленную конфигурацию/манифесты из снимка, в которых уже преднастроено использование снимка в качестве источника данных и самостоятельно ее применяет.


Краткое описание реализации:
1. Каждый снимок представлен парой CRD: `XxxxSnapshot` (ns или cluster) и `XxxxSnapshotContent` (cluster) с обязательным набором стандартных полей (описаны ниже) для совместимости и единых процессов экспорта/импорта/удаления. Структурные требования:
   - `XxxxSnapshot` может ссылаться на дочерние `XxxxSnapshot` (указываются тип и имя);
   - `XxxxSnapshotContent` содержит одну ссылку на `ManifestCheckpoint` (обязательно) и опционально одну ссылку на `VolumeSnapshotContent` **или** `PersistentVolume`;
   - группы API: `XxxxSnapshot` / `XxxxSnapshotContent` в группе соответствующего модуля (`<module>.deckhouse.io`); сабресурсы `/manifests` и `/manifests-with-data-restoration` — в `subresources.<module>.deckhouse.io`.
2. Удаление `XxxxSnapshot` не удаляет сразу `XxxxSnapshotContent`: они связаны через ObjectKeeper с TTL, который обеспечивает “корзину”. Глобальный TTL по умолчанию `global.modules.snapshotContentRetentionTTL = 30d`, может быть переопределён в настройках модуля (желательно параметр называть тоже snapshotContentRetentionTTL) и, где уместно, в `XxxxClass` (например, `VirtualMachineClass`, `PostgresClass`). Если объект подпадает под несколько классов (например, VirtualDisk — под VMClass и StorageClass, когда снимок делается для VM), берётся максимальный TTL из доступных источников.
3. Восстановление выполняет пользователь/CLI/UI; контроллеры лишь создают снимки и готовят данные, автоматического “применить” нет.

#### Восстановление (контракт и API)

**Entry-point:** всегда через `XxxxSnapshot`.  
Основной путь — `GET .../snapshots/<name>/manifests-with-data-restoration`.

**Как работает `/manifests-with-data-restoration` (MVP):**
- строит дерево по `SnapshotContent.status.childrenSnapshotContentRefs[]`;
- для каждого узла читает манифесты напрямую из `ManifestCheckpoint`/chunks (без HTTP);
- объекты с данными ссылаются на `XxxxSnapshot` (например, PVC с `dataSource` на VSC/PV), остальное сохраняется;
- остальные ресурсы возвращаются без изменений;
- выдача детерминированно сортируется и дедуплицируется.

**Конфликты при применении (аналог `kubectl edit`):**
- при `Conflict` клиент повторно получает манифесты,
  смёрживает свои правки и повторяет apply.

**Важно про API:** `/manifests` и `/manifests-with-data-restoration` — это aggregated API (extension‑apiserver),
используется внешними клиентами (CLI/UI/админ). Внутри контроллера по HTTP не вызывается.

**Будущая часть:** в следующих версиях планируется подключение доменной библиотеки/вебхука
для модульных трансформаций манифестов (VM/DB/custom‑resources и т.п.).

#### ObjectKeeper (Snapshot-keeper)

ObjectKeeper используется для отвязки жизненного цикла SnapshotContent от Snapshot
и реализации механизма "корзины" с TTL.

- ObjectKeeper создаётся ТОЛЬКО для корневого XxxxSnapshot.
- Для дочерних YyyySnapshot ObjectKeeper НЕ создаётся.
- XxxxSnapshotContent всегда имеет ownerReference на ObjectKeeper.
- Удаление XxxxSnapshot не удаляет данные немедленно, а фиксируется ObjectKeeper'ом.
- После удаления XxxxSnapshot ObjectKeeper запускает отсчёт TTL для удаления контента.

### 3. Требования к объектам

#### Требования к `XxxxSnapshot` (если namespaced)

Для создания Snapshot как UX‑объекта (когда `SnapshotContent` уже существует) все поля `spec` могут быть необязательны.
Если нужен явный импорт готового контента, предлагается указывать `spec.existingContentRef.name` — это сигнал привязать Snapshot
к уже существующему `XxxxSnapshotContent` без создания новых артефактов.
- `spec.xxxxName` (присутсвует, если уместно) — имя объекта, для которого создается снимок (если уместно).
- `status.contentName: string` — имя связанного `XxxxSnapshotContent` (cluster).
- `status.manifestCaptureRequestName: string` — имя MCR созданного для снятия снимка манифестов объектов, которые войдут в снимок.
- `status.volumeCaptureRequestName: string` (присутствует, если есть данные) — имя `VolumeCaptureRequest` (VCR), созданного для снятия снимка данных.
- `status.childrenSnapshotRefs[]` (присутствует, если есть дочерние снимки): список дочерних снимков
  - `kind: string`
  - `name: string`
- `status.dataConsistency` (присутствует, если есть данные) — достигнутый уровень консистивности данных (`CrashConsistent`, `FileSystemConsistent` или `ApplicationConsistent`)
- `status.dataSnapshotMethod` (присутствует, если есть данные) — способ, которым был выполнен снимок данных (`Online`, `Offline` или `OfflineAfterConfirmedGracefulShutdown`)
- `status.conditions[]`
  - `ManifestsReady` — выставляется в True, когда MCR успешно отработал
  - `DataReady` (присутствует, если есть данные) — выставляется в True, когда VCR успешно отработал
  - `ChildrenSnapshotsReady` (присутствует, если есть дочерние снимки) — выставляется в True, когда дочерние снимки успешно выполнились
  - `Ready` — — выставляется в True, когда все остальные Ready, и создан и полностью заполнен `XxxxSnapshotContent`.
  - `HandledByDomainSpecificController` - выставляется в True домен-специфичным контроллером после начала обработки
  - `HandledByCommonController` - выставляется в True общим контроллером после начала обработки

#### Требования к `XxxxSnapshot` (если cluster-scoped)

Для создания Snapshot как UX‑объекта (когда `SnapshotContent` уже существует) все поля `spec` могут быть необязательны.
Если нужен явный импорт готового контента, предлагается указывать `spec.existingContentRef.name` — это сигнал привязать Snapshot
к уже существующему `XxxxSnapshotContent` без создания новых артефактов.
- `spec.xxxxName` (присутсвует, если уместно) — имя объекта, для которого создается снимок (если уместно).
- `status.contentName: string` — имя связанного `XxxxSnapshotContent` (cluster).
- `status.clusterManifestCaptureRequestName: string` — имя MCR созданного для снятия снимка манифестов объектов, которые войдут в снимок.
- `status.childrenSnapshotRefs[]` (присутствует, если есть дочерние снимки): список дочерних снимков
  - `kind: string`
  - `name: string`
  - `namespace: string` (опционально)
- `status.dataConsistency` (присутствует, если есть данные) — достигнутый уровень консистивности данных (`CrashConsistent`, `FileSystemConsistent` или `ApplicationConsistent`)
- `status.dataSnapshotMethod` (присутствует, если есть данные) — способ, которым был выполнен снимок данных (`Online`, `Offline` или `OfflineAfterConfirmedGracefulShutdown`)
- `status.conditions[]`
  - `ManifestsReady` — выставляется в True, когда MCR успешно отработал
  - `ChildrenSnapshotsReady` (присутствует, если есть дочерние снимки) — выставляется в True, когда дочерние снимки успешно выполнились
  - `Ready` — — выставляется в True, когда все остальные Ready, и создан и полностью заполнен `XxxxSnapshotContent`.
  - `HandledByDomainSpecificController` - выставляется в True домен-специфичным контроллером после начала обработки
  - `HandledByCommonController` - выставляется в True общим контроллером после начала обработки

#### Требования `XxxxSnapshotContent` (cluster-scoped)

- `spec.snapshotRef` — ссылка на `XxxxSnapshot` (namespace, name).
- `status.childrenSnapshotContentRefs[]` (опционально) — аналогично `status.childrenSnapshotRefs[]` в `XxxSnapshot`, только для `XxxSnapshotContent` дочерних снимков (чтобы при удалении снимков можно было восстановить связи).
  - `kind: string`
  - `name: string`
- `status.manifestCheckpointName: string` — имя `ManifestCheckpoint` (cluster, обязательно).
- `status.dataRef` (опционально, максимум один):
  - `kind: VolumeSnapshotContent | PersistentVolume`
  - `name: string`
- `status.dataConsistency` и `status.dataSnapshotMethod` — копируются из `XxxSnapshot`.
- `status.conditions[]` — `Ready`.
- Никаких других полей не предполагается и ожидается, что схемы у всех `XxxxSnapshotContent` будут идентичными, однако это не является обязательным требованием.

**Примечание:** Snapshot описывает намерение создания снимка (intent) и использует декларативные механизмы отбора (`selector`, `includedResources`, `excludedResources`). `ManifestCaptureRequest` является execution-уровнем и всегда оперирует явным списком ресурсов, полученным в результате разрешения intent. Такое разделение обеспечивает удобный UX, расширяемость API и детерминированность исполнения.

**Важно про status-поля и GC:**

Status-поля, содержащие ссылки на связанные объекты
(childrenSnapshotRefs, childrenSnapshotContentRefs,
manifestCaptureRequestName, volumeCaptureRequestName),
используются исключительно для отображения состояния,
восстановления связей и read-only API.

Эти поля НЕ являются структурными связями
и НЕ участвуют в garbage collection.

**Источник истины для иерархии snapshot'ов:**
- для удаления — ownerReference;
- для восстановления связей — childrenSnapshotContentRefs;
- для отображения в namespace — childrenSnapshotRefs.

### 4. Схемы ресурсов

#### Схема ресурса Snapshot (namespaced)

```yaml
apiVersion: state-snapshotter.deckhouse.io/v1alpha1
kind: Snapshot
metadata:
  name: <snapshot-name>
  namespace: <namespace>
spec:
  # Опциональные поля для создания снимка:
  # Выбор объектов для снимка (декларативный intent):
  selector:  # Селектор объектов по лейблам (декларативный intent, правило выбора)
    matchLabels:
      <key>: <value>
    matchExpressions:
      - key: <key>
        operator: In | NotIn | Exists | DoesNotExist
        values: [<value1>, <value2>]
  includedResources: []  # Whitelist типов ресурсов (если не задан, разрешены все типы)
    - apiGroups: [string]
      kinds: [string]
      names: [string]  # Опционально, для точечного выбора конкретных объектов
      # selector вместо имени?
  existingContentRef:  # Опционально: привязка к уже существующему SnapshotContent
    name: <snapshotcontent-name>
  excludedResources: []  # Blacklist типов ресурсов (всегда имеет приоритет над includedResources)
    - apiGroups: [string]
      kinds: [string]
      names: [string]  # Опционально, для исключения конкретных объектов
      # selector вместо имени?
status:
  contentName: string  # Имя связанного SnapshotContent (cluster-scoped)
  manifestCaptureRequestName: string  # Имя MCR
  volumeCaptureRequestName: string  # Имя VCR (если есть данные)
  childrenSnapshotRefs: []  # Список дочерних снимков (если есть)
    - kind: string
      name: string
  dataConsistency: CrashConsistent | FileSystemConsistent | ApplicationConsistent  # Если есть данные
  dataSnapshotMethod: Online | Offline | OfflineAfterConfirmedGracefulShutdown  # Если есть данные
  conditions: []
    - type: ManifestsReady
      status: "True" | "False"
      reason: string
      message: string
    - type: DataReady  # Если есть данные
      status: "True" | "False"
      reason: string
      message: string
    - type: ChildrenSnapshotsReady  # Если есть дочерние снимки
      status: "True" | "False"
      reason: string
      message: string
    - type: Ready
      status: "True" | "False"
      reason: string
      message: string
    - type: HandledByDomainSpecificController - переименовать!

```

**Правила интерпретации selector + include/exclude:**

1. `selector` применяется первым - определяет какие объекты интересуют по лейблам:
   - Если `selector` не задан или пустой (`{}`) - считается что выбраны все объекты в namespace/cluster (в зависимости от scope ресурса)
   - Если `selector` задан - применяется как label selector для фильтрации объектов по лейблам
   - Если `selector` задан, но не находит объектов - это не ошибка, просто пустой результат (снимок будет создан с пустым списком объектов)
2. `includedResources` - whitelist типов ресурсов:
   - Если не задан - разрешены все типы ресурсов
   - Если задан - применяется как фильтр по типам ресурсов (apiGroups + kinds)
   - Если задан с `names` - дополнительно фильтрует по конкретным именам объектов
   - Если `selector` пустой, а `includedResources` задан - выбираются все объекты указанных типов (весь namespace/cluster по типам)
3. `excludedResources` - blacklist типов ресурсов:
   - Всегда имеет приоритет над `includedResources`
   - Если задан - исключает ресурсы указанных типов (apiGroups + kinds)
   - Если задан с `names` - исключает ресурсы указанного типа (apiGroups + kinds) и с указанными именами
   - Если `selector` пустой, а `includedResources` задан - выбираются все объекты указанных типов (весь namespace/cluster по типам)
4. Итоговый набор объектов: `resolvedTargets = selector ∩ includedResources − excludedResources`
5. **Валидация и обработка ошибок:**
   - Если после применения всех правил получился пустой список объектов - это ошибка и ManifestCheckpoint не будет создан, Snapshot переходит в состояние `Ready=False` с condition `ManifestsReady=False` и reason, указывающим на отсутствие объектов для снимка
   - Если `selector` некорректен (синтаксическая ошибка) - Snapshot переходит в состояние `Ready=False` с condition `ManifestsReady=False` и reason, указывающим на ошибку валидации селектора
   - Если `includedResources` или `excludedResources` содержат некорректные apiGroups/kinds (например, несуществующие CRD) - это ошибка и ManifestCheckpoint не будет создан, Snapshot переходит в состояние `Ready=False` с condition `ManifestsReady=False` и reason, указывающим на ошибку валидации типов ресурсов. Кейс: поменялась версия api ресурса, и указанный в include/exclude тип больше не существует - пользователь может это упустить, поэтому нужно явно ему об этом сообщить.
   - Если указаны и `selector`, и `includedResources` с `names` - `names` применяются к результату селектора (пересечение)

Пример:
```
apiVersion: state-snapshotter.deckhouse.io/v1alpha1
kind: Snapshot
metadata:
  name: all-vms
  namespace: default
spec:
  selector: {}            # пустой = все объекты
  includedResources:
    - apiGroups: ["virtualization.deckhouse.io"]
      kinds: ["VirtualMachine"]
```

#### Схема ресурса ClusterSnapshot (cluster-scoped)

TODO: добавить полную схему, когда утвердим Snapshot. Пока разница только в том, что в ClusterSnapshot можно указывать cluster-wide ресурсы <?и namespace ресурсы из разных namespace.?>

#### Схема ресурса SnapshotContent (cluster-scoped)

```yaml
apiVersion: state-snapshotter.deckhouse.io/v1alpha1
kind: SnapshotContent
metadata:
  name: <snapshot-content-name>
  # namespace отсутствует (cluster-scoped)
spec:
  snapshotRef:  # Ссылка на Snapshot или ClusterSnapshot
    kind: Snapshot | ClusterSnapshot
    name: string
    namespace: string  # Только для Snapshot
status:
  manifestCheckpointName: string  # Имя ManifestCheckpoint (обязательно)
  dataRef:  # Опционально, максимум один
    kind: VolumeSnapshotContent | PersistentVolume
    name: string
  childrenSnapshotContentRefs: []  # Список дочерних SnapshotContent (если есть)
    - kind: string
      name: string
  dataConsistency: CrashConsistent | FileSystemConsistent | ApplicationConsistent  # Копируется из Snapshot/ClusterSnapshot
  dataSnapshotMethod: Online | Offline | OfflineAfterConfirmedGracefulShutdown  # Копируется из Snapshot/ClusterSnapshot
  conditions: []
    - type: Ready
      status: "True" | "False"
      reason: string
      message: string
```


### 5. Детали реализации

В реализации для каждого `XxxxSnapshot` участвует домен-специфичный контроллер (например контроллер виртуализации или контроллер managed-сервиса), который понимает устройство объекта `Xxxx` и что необходимо сделать, для корректного создания снимка, а так же общий-контроллер (расположен в модуле state-snapshotter), который реализует общую часть алгоритма.

#### 5.1 Создание снимка

Общий алгоритм работы:
- Домен-специфичный контроллер обрабатывает `XxxxSnapshot`.
  1. Видит новый объект, который еще им не обработан (`status.conditions.[type=HandledByDomainSpecificController]` НЕ в значении True).
  2. Параллельно выполняет следующие действия:
    - создаёт `ManifestCaptureRequest` (или `ClusterManifestCaptureRequest`) и указывает его имя в `status.manifestCaptureRequestName` (или в `status.clusterManifestCaptureRequestName`);
    - если уместно, создаёт `VolumeCaptureRequest` и указывает его имя в `status.volumeCaptureRequestName`;
    - если уместно, проставляет `status.dataConsistency` и `status.dataSnapshotMethod` (подробнее см. раздел "Консистентность данных")
    - проставляет `status.conditions.[type=HandledByDomainSpecificController]` в True.

ManifestCaptureRequest и VolumeCaptureRequest являются временными request-ресурсами
и подлежат удалению контроллером после достижения состояния Ready.
- Общий контроллер обрабатывает `XxxxSnapshot` и `XxxxSnapshotContent`:
  1. Видит новый объект, который еще им не обработан (`status.conditions.[type=HandledByCommonController]` НЕ в значении True).
  2. Параллельно:
    - Следит за связанным `ManifestCaptureRequest` (или `ClusterManifestCaptureRequest`), когда его имя будет проставлено в status, а когда он перейдет в Ready — проставляет condition `ManifestsReady`.
    - Если уместно, следит за связанным `VolumeCaptureRequest` (когда его имя будет проставлено в status), а когда он перейдет в Ready — проставляет condition `DataReady`.
    - Дожидается, когда `status.conditions.[type=HandledByDomainSpecificController]` перейдет в значение True.
  3. После этого:
    - получает из `ManifestCaptureRequest` (или `ClusterManifestCaptureRequest`) имя `ManifestCheckpoint`
    - если уместно, получает из `VolumeCaptureRequest` имя `VSC` или `PV`
  4. Cоздаёт `ObjectKeeper`, который следит за `XxxxSnapshot` и запускает TTL удаление контента после его удаления.
  5. Создаёт `XxxxSnapshotContent` (cluster):
    - указывает `status.manifestCheckpointName`
    - если уместно, указывает `status.dataRef` (VSC/PV)
    - если уместно, копирует `status.dataConsistency` и `status.dataSnapshotMethod` из `XxxxSnapshot`
    - делает `ObjectKeeper` владельцем для `XxxxSnapshotContent` (добавляет в ownerRef у `XxxxSnapshotContent` созданный `ObjectKeeper`)
  6. Делает `XxxxSnapshotContent` владельцем для `ManifestCheckpoint` и `VSC/PV` (если есть dataRef)
  7. Выставляет у `XxxxSnapshotContent` condition `Ready` в True
  8. Обновляет `XxxxSnapshot`:
    - добавляет ссылку на `XxxxSnapshotContent`
    - ставит condition `Ready` в True
    - ставит `status.conditions.[type=HandledByCommonController]` в True

Если объект Xxxx сложный (например VirtualMachine) и состоит/ссылается на другие объекты-с-данными Yyyy, то контроллер при создании XxxxSnapshot может создавать дочерние YyyySnapshot аналогичным образом. В том числе, могут быть снимки, не содержащие данные, но содержащие дочерние снимки (VirtualMachine как раз пример такого снимка). Алгоритм указанный выше расширяется:
- Домен-специфичный контроллер (до того как проставил `status.conditions.[type=HandledByDomainSpecificController]` в True) создает необходимые `YyyySnapshot` (сразу устанавливая `XxxxSnapshot` их владельцем) и указывает их в поле `status.childrenSnapshotRefs[]`.
- Общий контроллер следит за дочерними снимками и когда они станут все Ready (и `status.conditions.[type=HandledByDomainSpecificController]` станет True):
  - выставляет condition `ChildrenSnapshotsReady` в True
  - заполняет `status.childrenSnapshotContentRefs[]` у `XxxxSnapshotContent`
  - делает `XxxxSnapshotContent` владельцем для всех `YyyySnapshotContent`

Кроме этого, могут быть снимки вообще не привязанные к одному конкретному объекту. Яркий пример — это снимок с типом Snapshot (просто Snapshot, без всяких Xxxx), который по определенной логике делает снимок всего содержимого (или части) namespace (сохраняя много манифестов и порождая множество дочерних снимков).

#### 5.2 Состояние Xxxx и возможность создания снимка для него

Если это имеет смысл, домен-специфичный контроллер может проверять состояние ресурса `Xxxx` и самостоятельно принимать решение, допускается ли создание снимка (например, для VirtualMachine допускается в фазах `Running`, `Stopped`). Если ресурс находится в неподходящем состоянии, `XxxxSnapshot` переходит в состояние `Ready=False` с соответствующим reason и сообщением об ошибке. В этом случае, обычно, никакие другие объекты не создаются (в том числе не создаются `XxxxSnapshot`, `ManifestCaptureRequest`, `VolumeCaptureRequest` и дочерние снимки).

#### 5.3 Консистивность данных

В общем случае возможны следующие уровни консистивности данных:
- `CrashConsistent` — данные эквивалентны внезапному отключению питания. Данные могут требовать дополнительных коррекционно-восстановительных операций при восстановлении: корректировки файловой системы, и корректировки данных приложений.
- `FileSystemConsistent` — данные эквивалентны внезапному отключению приложения. Целостность файловой системы гарантируется, но приложения могут не быть приостановлены. Данные могут требовать дополнительных коррекционно-восстановительных операций при восстановлении: корректировки данных приложений.
- `ApplicationConsistent` — данные эквивалентны корректному завершению (или приостановке) приложений. Целостность данных файловой системы (если уместно) и приложений гарантируется. Данные не требуют никаких коррекционно-восстановительных операций при восстановлении и могут быть использованы сразу.

В общем случае возможны следующие способы снятия снимка данных:
- `Online` — если снимок был создан для работающей нагрузке;
- `Offline` — если снимок был создан для остановленной нагрузки;
- `OfflineAfterConfirmedGracefulShutdown` — если снимок был создан для остановленной нагрузки и есть гарантия, что нагрузка была остановлена полностью корректным образом.


При создании снимка для объекта имеющего данные у пользователя должна быть возможность гибко задать желаемый уровень консистивности данных (`spec.requiredDataConsistency`). При этом в уже созданном снимке должно быть установлено, с каким уровнем консистивности данных его в итоге удалось создать (`status.dataConsistency`) и каким образом был сделан снимок данных (`status.dataSnapshotMethod`).

Реализация:
- Домен-специфичный контроллер, если у объекта `Xxxx` есть данные, должен корректно обрабатывать поле `spec.requiredDataConsistency` из `XxxxSnapshot` и корректно проставлять поля `status.dataConsistency` и `status.dataSnapshotMethod` в `XxxxSnapshot`.
  - Для снимка объекта `Xxxx` в домен-специфичном контроллере должна поддерживаться возможность задавать желаемый уровень консистивности (`spec.requiredDataConsistency`):
    - CrashConsistent — если такой уровень поддерживается/уместен для объекта `Xxxx`
    - FileSystemConsistent — если такой уровень поддерживается/уместен для объекта `Xxxx`
    - ApplicationConsistent — если такой уровень поддерживается/уместен для объекта `Xxxx`
    - хотя бы один из CrashConsistent, FileSystemConsistent или ApplicationConsistent должен поддерживаться
    - BestSupported — максимально возможный уровень консистивности данных, поддерживаемый объектами типа `Xxxx`, вне зависимости от текущего состояния конкретного объекта.
    - BestSupportedNonBlocking — максимально возможный уровень консистивности данных, который не вызывает никаких блокировок, поддерживаемый объектами типа `Xxxx`, вне зависимости от текущего состояния конкретного объекта.
    - BestAvailable — максимально возможный уровень консистивности данных, доступный для объекта `Xxxx` в его текущем состоянии.
    - BestAvailableNonBlocking — максимально возможный уровень консистивности данных, который не вызывает никаких блокировок, доступный для объекта `Xxxx` в его текущем состоянии.
  - Для групповых снимков общего характера (например для `Snapshot`), которые не привязанны к конкретному объекту `Xxxx`, а создают множество дочерних снимков, в домен-специфичном контроллере должна быть реализована возможность указать желаемый уровни консистивности следующим образом (`spec.requiredDataConsistency`):
    - CrashConsistent — для самого снимка и для каждого дочернего снимка должен быть уовень CrashConsistent, если это не возможно, то ошибка.
    - AtLeastCrashConsistent — для самого снимка и для каждого дочернего снимка как минимум CrashConsistent, но можно выше, если реализовано.
    - FileSystemConsistent — для самого снимка и для каждого дочернего снимка должен быть уовень FileSystemConsistent, если это не возможно, то ошибка.
    - AtLeastFileSystemConsistent — для самого снимка и для каждого дочернего снимка как минимум FileSystemConsistent, но можно выше, если реализовано.
    - ApplicationConsistent — для самого снимка и для каждого дочернего снимка обязательно ApplicationConsistent, если это не возможно, то ошибка.
    - а так же BestSupported, BestSupportedNonBlocking, BestAvailable и BestAvailableNonBlocking.
- Общий контроллер не знает никаких деталей про консистентность данных и только переносит `status.dataConsistency` и `status.dataSnapshotMethod` из `XxxxSnapshot` в `XxxxSnapshotContent`.


#### 5.4 Защита от случайного удаления связанных объектов
Общий контроллер защищает объекты от случайного удаления:
- Чтобы администратор кластера не мог случайно удалить `XxxxSnapshotContent`, для которого существует `XxxxSnapshot`, общий контроллер поддерживает (добавляет/снимает) финалайзер на `XxxxSnapshotContent` если есть связанный с ним `XxxxSnapshot`.
- Чтобы администратор кластера не мог случайно удалить `YyyySnapshotContent`, который является дочерним для некоторого `XxxxSnapshotContent`, общий контролер поддерживает (добавляет/снимает) финалайзер на `YyyySnapshotContent`, если он указан в некотором `XxxxSnapshotContent` в `status.childrenSnapshotContentRefs[]`.
- Чтобы пользователь не мог случайно удалить `YyyyySnapshot`, который является дочерним для некоторого `XxxxSnapshot`, общий контроллер поддерживает (добавляет/снимает) на `YyyyySnapshot` финалайзер, если он указан в некотором `XxxxSnapshot` в `status.childrenSnapshotRefs[]`.

Удаление ресурсов обеспечивается ownerReference.
Финалайзеры используются только для защиты от ручного удаления.

#### 5.5 Удаление снимков и работа "корзины"

**Общий принцип:** Для пользователя удаление Snapshot выглядит как обычное удаление ресурса.
Фактическое удаление SnapshotContent и данных происходит асинхронно, после истечения TTL, и обеспечивается каскадным удалением Kubernetes через ownerReference.

При удалении `XxxxSnapshot` связанный `XxxxSnapshotContent` не удаляется сразу, а переходит в состояние «корзины» и удаляется только после истечения TTL.

**Логические состояния SnapshotContent:**
1. Существует связанный `XxxxSnapshot`: финалайзер установлен, ручное удаление заблокировано.
2. `XxxxSnapshot` удалён: финалайзер снят, `XxxxSnapshotContent` находится в корзине и ожидает истечения TTL.
3. TTL истёк: инициируется каскадное удаление через ownerReference.
4. Объект физически удалён из кластера.

**Эффективный TTL SnapshotContent**

Для каждого `XxxxSnapshotContent` вычисляется одно эффективное значение TTL, равное максимальному из следующих источников:
- минимального гарантированного TTL платформы;
- TTL из классов ресурсов;
- TTL из настроек модуля;
- глобального TTL по умолчанию (`global.modules.snapshotContentRetentionTTL = 30d`).

Полученное значение является единственным TTL, применяемым для SnapshotContent.

**TTL и ObjectKeeper**

Эффективный TTL применяется только на уровне ObjectKeeper (см. раздел 2) и определяет момент начала каскадного удаления SnapshotContent и связанных данных.
Отдельных или независимых TTL для SnapshotContent не существует.

**TTL и временные request-ресурсы**

Временные request-ресурсы (`ManifestCaptureRequest`, `VolumeCaptureRequest` и т.п.) имеют собственный контроллер-специфичный жизненный цикл, не участвуют в механизме «корзины» и удаляются самими контроллерами после достижения терминального состояния (`Ready` или `Failed`).

**Расширение источников TTL**
- В `XxxxSnapshot` может быть добавлено поле `status.domainSpecificSnapshotContentRetentionTTL`, участвующее в расчёте эффективного TTL.
- Может быть введён минимальный гарантированный TTL платформы (`minimalSnapshotContentRetentionTTL`), который ограничивает эффективный TTL снизу.

#### 5.6 Восстановление данных контроллером Xxxx из снимка
Каждый объект-с-данными `Xxxx`, для которого создан `XxxxSnapshot`, должен уметь создаваться “из снимка”: в его `spec` должна быть возможность указать снимок. Новый `Xxxx` создаётся по конфигурации, явно заданной пользователем, а “данные” импортируются из снимка (и приводятся, если необходимо, в соответствие с переданной конфигурацией).

1. Контроллер читает `XxxxSnapshot`, достаёт `XxxxSnapshotContent`.
2. Из `XxxxSnapshotContent` берёт `dataRef` (VSC или PV).
3. Создаёт `VolumeRestoreRequest` с необходимым PVC template.
4. Ждёт Ready VRR.
5. Создаёт объект `Xxxx`, который использует восстановленный PVC.

В этом случае манифесты из XxxxSnapshot не используются.

#### 5.7 Полное восстановление (ручное/интерактивное) и API для получения манифестов

Автоматического полного «восстановить и применить» на бекенде нет: слишком сложно разрешать конфликты имён и зависимых ресурсов (storageclass/ingressclass/vmclass/pgclass и т.п.). Вместо этого каждый `XxxxSnapshot` предоставляет «почти готовые» манифесты, которые пользователь/CLI/UI может скачать, подправить и применить.

Метод `/manifests-with-data-restoration` (subresources.<group>):

- берёт манифесты из `ManifestCheckpoint` (его `/manifests`) и модифицирует:
  - объекты с данными ссылаются на `XxxxSnapshot` (например, PVC с `dataSource` на VSC/PV), остальное сохраняется;
  - дочерние снимки разворачиваются рекурсивно и подставляются;
  - или удаляет поля, которые не должны применяться напрямую (runtime-поля, server-generated поля и т.п.);
  - возможны модуль-специфичные корректировки, которые описаны в пункте ["Реализация модификации манифестов доменных контроллеров"](#58-реализация-модификации-манифестов-доменных-контроллеров).

Далее пользователь/CLI/UI может:
- вызвать `/manifests-with-data-restoration` (или `/manifests`), получить манифесты;
- открыть редактор (аналог `kubectl edit`) или сохранить в файл и поправить вручную;
- использовать dry-run (`?dryRun=All`, штатное в Kubernetes) перед применением;
- UI/console делают то же на формах.

Метод `/manifests` (subresources.<group>):

- просто проксирует `/manifests` `ManifestCheckpoint` и используется для экспорта исходных манифестов.

Внимание: **Обработка предназначена для подготовки манифестов к восстановлению, экспорту или ручному применению, не приводит к автоматическому изменению состояния кластера и не изменяет сохранённый ManifestCheckpoint.**

Примеры восстановления снимков и сценарии через CLI смотри в [unified-snapshots-restore-plan.md](unified-snapshots-restore-plan.md).




#### 5.8 Реализация модификации манифестов доменных контроллеров

Внимание: **Модификация манифестов происходит только при вызове метода `/manifests-with-data-restoration` (subresources.<group>), который реализуется общим контроллером в модуле state-snapshotter.**

- В модуле доменного контроллера реализуется http сервис, который принимает манифесты и модифицирует их по своему алгоритму.
- В модуле доменного контроллера создается ресурс `DomainSpecificSnapshotController`, который содержит информацию о том, какие ресурсы относятся к этому модулю:

```yaml
apiVersion: state-snapshotter.deckhouse.io/v1alpha1
kind: DomainSpecificSnapshotController
metadata:
  name: <>
spec:
  snapshotResourceMapping:
    - resourceCRDName: virtualmachines.virtualization.deckhouse.io
      snapshotCRDName: virtualmachinesnapshots.virtualization.deckhouse.io
      priority: 0
    - resourceCRDName: virtualdisks.virtualization.deckhouse.io
      snapshotCRDName: virtualdisksnapshots.virtualization.deckhouse.io
      priority: 1
  modificationWebhookService:
    name: virtualization-snapshot-controller-modification-webhook
    namespace: d8-virtualization
```

<!-- 
Более простой вариант для общего контроллера, но более сложный в поддержании в модулях доменных контроллеров.
```yaml
apiVersion: state-snapshotter.deckhouse.io/v1alpha1
kind: DomainSpecificSnapshotController
metadata:
  name: <>
spec:
  snapshotResourceMapping:
    - resource: 
        kind: VirtualMachine
        group: virtualization.deckhouse.io
        version: v1alpha1
      snapshot:
        kind: VirtualMachineSnapshot
        group: virtualization.deckhouse.io
        version: v1alpha1
      priority: 0
    - resource:
        kind: VirtualDisk
        group: virtualization.deckhouse.io
        version: v1alpha1
      snapshot:
        kind: VirtualDiskSnapshot
        group: virtualization.deckhouse.io
        version: v1alpha1
  modificationWebhookService:
    name: virtualization-snapshot-controller-modification-webhook
    namespace: d8-virtualization
``` -->

- Общий контроллер при обработке ресурса сначала ищет его тип во всех `DomainSpecificSnapshotController` и вызывает HTTP-сервис для модификации манифеста такого ресурса, указанный в `spec.modificationWebhookService` соответствующего этому типу ресурса `DomainSpecificSnapshotController`.

##### Security модель

**Threat model:** доменный контроллер при модификации манифестов может подставлять sensitive данные (credentials, connection strings и т.д.). Необходимо гарантировать, что запросы к webhook приходят только от общего контроллера.

**Решение:**

- **Авторизация пользователя** выполняется на уровне aggregated API: subresource `/manifests-with-data-restoration` защищён стандартным Kubernetes RBAC. Если у пользователя есть право на этот subresource снапшота — он получает все манифесты, включая модифицированные.
- **Аутентификация между контроллерами** выполняется через mTLS: общий контроллер и доменные контроллеры используют client certificates, подписанные общим CA. Доменный контроллер принимает запросы только с валидным client certificate общего контроллера.

##### HTTP API сервиса модификации манифестов

**Base URL:** `https://<service-name>.<namespace>.svc`

**TLS:** mTLS с client certificate общего контроллера. Доменный контроллер проверяет:

- client certificate подписан доверенным CA
- Common Name (CN) или SAN соответствует ожидаемому (например, `state-snapshotter-controller.d8-state-snapshotter.svc`)

---

###### POST /api/v1/manifests:modify

Batch-модификация манифестов ресурсов одного снимка для подготовки к восстановлению. // TODO: нужно ли доменному контроллеру в одном запросе получать все манифесты только дного снимка? Возможно нужны все манифесты из всей иерархии снимков? Или вообще можно обрабатывать манифесты независимо и можно просто выполнять batch модификацию манифестов из разных снимков независимо и одновременно?

**Headers:**

| Header | Значение |
|--------|----------|
| `Content-Type` | `application/json` |

**Request body:**

```json
{
  "snapshotRef": {
    "apiVersion": "virtualization.deckhouse.io/v1alpha1",
    "kind": "VirtualDiskSnapshot",
    "name": "my-disk-snapshot",
    "namespace": "default"
  },
  "manifests": {
    "<metadata.uid>": { /* json манифест ресурса */ },
    "<metadata.uid>": { /* json манифест ресурса */ }
  }
}
```

| Поле | Тип | Описание |
|------|-----|----------|
| `snapshotRef` | `TypedObjectReference` | Ссылка на снимок, из которого берутся манифесты |
| `snapshotRef.apiVersion` | `string` | API версия снимка |
| `snapshotRef.kind` | `string` | Тип снимка (например, `VirtualDiskSnapshot`) |
| `snapshotRef.name` | `string` | Имя снимка |
| `snapshotRef.namespace` | `string` | Namespace снимка (для namespaced снимков) |
| `manifests` | `map[string]object` | Map манифестов, где ключ — `metadata.uid` ресурса |

**Response (200 OK):**

```json
{
  "results": {
    "<metadata.uid>": { "manifest": { /* модифицированный манифест */ } },
    "<metadata.uid>": { "error": "<сообщение об ошибке>" }
  }
}
```

| Поле | Тип | Описание |
|------|-----|----------|
| `results` | `map[string]object` | Map результатов, где ключ — `metadata.uid` ресурса |
| `results[uid].manifest` | `object` | Модифицированный манифест (если успешно) |
| `results[uid].error` | `string` | Сообщение об ошибке (если не удалось обработать) |

**Response codes:**

| Код | Описание |
|-----|----------|
| `200 OK` | Запрос обработан (результаты для каждого манифеста в `results`) Даже если один из манифестов не был обработан успешно, то результаты будут содержать ошибку для этого манифеста. |
| `400 Bad Request` | Невалидный формат запроса |
| `401 Unauthorized` | Невалидный или отсутствующий client certificate |

#### 5.9 Скачивание (экспорт) снимков

**Важно про уровни API:**  
- Источник истины манифестов — aggregated subresource `/manifests` у Snapshot.  
- HTTP `/api/v1/manifests` в этом разделе — **endpoint сервиса DataExport**, а не Kubernetes API.  
DataExport выступает как delivery/publish слой поверх aggregated API.

Ввести в DataExport возможность указывать `Snapshot` в `spec.targetRef.kind` для экспорта его данных. В HTTP API для DataExport будет добавлен новый эндпоинт `/api/v1/manifests` для скачивания манифестов — проксирование subresource `/manifests` Snapshot'а или ClusterSnapshot'а.

- Admission: при создании DataExport с `spec.targetRef.kind=Snapshot|ClusterSnapshot` проверяется наличие прав на чтение ресурса Snapshot/ClusterSnapshot в указанном namespace/кластере.

<!-- Порядок работы (много DataExport'ов для иерархии снимков):
1. Клиент создаёт DataExport с `spec.targetRef.kind=Snapshot|ClusterSnapshot` и указывает имя Snapshot/ClusterSnapshot в `spec.targetRef.name`.
2. Контроллер DataExport обрабатывает DataExport и создает необходимые ресурсы:
  - PVC с данными снимка (если есть) — механизм получения данных будет выбран отдельно
    (например, PVC из VolumeSnapshot/VolumeSnapshotContent или иной путь);
  - deployment с подом http-сервера, который обрабатывает запросы на скачивание манифестов и данных;
  - сервис для доступа к http-серверу;
  - (если publish: true) Ingress/Route для внешнего доступа к сервису.
3. Клиент делает HTTP GET запрос к эндпоинту `/api/v1/manifests` созданного DataExport для скачивания манифестов снимка. К манифестам добавляется манифест самого снимка и его SnapshotContent.
4. (если есть данные) Клиент делает HTTP GET запрос к эндпоинту `/api/v1/files` или `/api/v1/blocks` созданного DataExport для скачивания данных снимка.
5. Клиент создает DataExport для дочерних снимков (если есть) и повторяет шаги 2-4 для каждого дочернего снимка.
6. Данные и манифесты всех снимков из иерархии объединяются в архив на клиентской стороне. -->

Порядок работы (один DataExport для всей иерархии снимков):
В DataExport строку status.url меняем на массив status.urls: [], где каждый элемент содержит ссылку на скачивание манифестов/данных для одного снимка из иерархии.

**Пример status с несколькими URL:**

```yaml
status:
  phase: Ready
  urls:
    - snapshotId: "VirtualMachineSnapshot--default--my-vm-snapshot"
      url: "https://export.example.com/vmsnapshot-abc123"
    - snapshotId: "VirtualDiskSnapshot--default--my-vm-root-disk"
      url: "https://export.example.com/vdsnapshot-def456"
    - snapshotId: "VirtualDiskSnapshot--default--my-vm-data-disk"
      url: "https://export.example.com/vdsnapshot-ghi789"
```

`snapshotId` соответствует идентификатору в `index.yaml` и именам файлов `data/<snapshotId>.tar.gz` в архиве.

1. Клиент создаёт DataExport с `spec.targetRef.kind=Snapshot|ClusterSnapshot` и указывает имя родительского Snapshot/ClusterSnapshot в `spec.targetRef.name`.
2. Контроллер DataExport обрабатывает DataExport и создает необходимые ресурсы:
  - все PVC с данными всех снимков (если есть) — механизм получения данных будет выбран отдельно
    (например, PVC из VolumeSnapshot/VolumeSnapshotContent или иной путь);
  - deployment'ы с подомами http-сервера, который обрабатывает запросы на скачивание манифестов и данных для каждого снимка; ## потом можно будет оптимизировать и поднимать минимальное число http серверов, если хранилище поддерживает монтирование снимков на одном узле
  - сервисы для доступа к каждому http-серверу;
  - (если publish: true) Ingress/Route для внешнего доступа к каждому сервису.
3. Клиент делает HTTP GET запрос к эндпоинту `/api/v1/manifests` к каждому url из `status.urls` созданного DataExport для скачивания манифестов снимка. К манифестам каждого снимка добавляется манифест самого снимка и его SnapshotContent.
4. (если есть данные) Клиент делает HTTP GET запрос к эндпоинту `/api/v1/files` или `/api/v1/blocks` к каждому url из `status.urls` созданного DataExport для скачивания данных снимка (если данных нет - вернется соответствующий ответ, который должен отличаться от ошибки).
5. Данные и манифесты всех снимков из иерархии объединяются в архив на клиентской стороне.

##### Файловая структура архива снимка

Архив организован так, чтобы при импорте можно было:
1. Быстро прочитать только манифесты (без чтения данных) для анализа и создания ресурсов
2. Выборочно восстанавливать данные конкретных снимков
3. Стримить данные параллельно для разных снимков

**Принцип организации:** внешний архив — несжатый `.tar`, внутри которого:
- `manifests.tar.gz` — первый entry, маленький (~КБ-МБ), содержит все манифесты и индекс
- `data/<snapshot-id>.tar.gz` — данные каждого снимка в отдельном сжатом архиве

Такая структура позволяет:
- Прочитать только `manifests.tar.gz` без чтения данных (tar пропускает entry при `Next()`)
- Получить полную информацию о всех снимках из `index.yaml` внутри `manifests.tar.gz`
- Выборочно читать данные конкретного снимка по его snapshot-id

Клиентское приложение (CLI/UI) формирует архив следующей структуры:

```text
snapshot-<snapshot-name>-<timestamp>.tar        # Внешний архив НЕ сжат
├── manifests.tar.gz                            # Первый entry, маленький (~КБ-МБ), сжатый
│   ├── index.yaml                              # Индекс: иерархия, размеры данных, checksums
│   └── snapshots/                              # Все снимки в плоской структуре
│       ├── <snapshot-id>/                      # ID = <kind>--<namespace>--<name>
│       │   ├── snapshot.yaml                   # XxxxSnapshot
│       │   ├── snapshot-content.yaml           # XxxxSnapshotContent
│       │   └── objects/                        # Манифесты объектов из ManifestCheckpoint
│       │       ├── <gvk>--<namespace>--<name>.yaml
│       │       └── ...
│       └── ...
└── data/                                       # Данные снимков
    ├── <snapshot-id-1>.tar.gz                  # Данные первого снимка (отдельный сжатый архив)
    │   ├── meta.yaml                           # Метаданные: тип (block/fs), размер, checksum
    │   └── data.img | data.tar                 # Блочные (.img) или файловые (.tar) данные
    ├── <snapshot-id-2>.tar.gz                  # Данные второго снимка
    │   └── ...
    └── ...
```

**Структура index.yaml:**

```yaml
version: v1                                     # Версия формата архива
createdAt: "2025-01-22T12:00:00Z"
sourceCluster:                                  # Информация об исходном кластере
  name: "prod-cluster"
  # apiServerURL: "https://api.prod.example.com"
  # clusterUUID: 0db7574a-86d4-4d90-b79f-2d99585fcb31 #k -n kube-system get cm d8-cluster-uuid ???
rootSnapshot:                                   # Корневой снимок
  id: "VirtualMachineSnapshot--default--my-vm"
  kind: VirtualMachineSnapshot
  namespace: default
  name: my-vm
snapshots:                                      # Плоский список всех снимков в иерархии
  - id: "VirtualMachineSnapshot--default--my-vm"
    kind: VirtualMachineSnapshot
    namespace: default
    name: my-vm
    parentId: null                              # null для корневого
    hasData: false
    children:                                   # Ссылки на дочерние снимки
      - "VirtualDiskSnapshot--default--my-vm-root-disk"
      - "VirtualDiskSnapshot--default--my-vm-data-disk"
  - id: "VirtualDiskSnapshot--default--my-vm-root-disk"
    kind: VirtualDiskSnapshot
    namespace: default
    name: my-vm-root-disk
    parentId: "VirtualMachineSnapshot--default--my-vm"
    hasData: true
    storageClassName: "ceph-ssd"                # StorageClass из исходного кластера
    dataFile: "data/VirtualDiskSnapshot--default--my-vm-root-disk.tar.gz"
    dataSize: 10737418240                       # 10 GiB (размер данных внутри, не архива)
    dataType: block                             # block | filesystem
    dataChecksum: "sha256:abc123..."            # Checksum данных (не архива)
    children: []
  - id: "VirtualDiskSnapshot--default--my-vm-data-disk"
    kind: VirtualDiskSnapshot
    namespace: default
    name: my-vm-data-disk
    parentId: "VirtualMachineSnapshot--default--my-vm"
    hasData: true
    storageClassName: "ceph-hdd"                # StorageClass из исходного кластера
    dataFile: "data/VirtualDiskSnapshot--default--my-vm-data-disk.tar.gz"
    dataSize: 53687091200                       # 50 GiB
    dataType: block
    dataChecksum: "sha256:def456..."
    children: []
```

**Пример для снимка VirtualMachine с двумя дисками:**

```
snapshot-my-vm-2025-01-22T120000Z.tar
├── manifests.tar.gz
│   ├── index.yaml
│   └── snapshots/
│       ├── VirtualMachineSnapshot--default--my-vm/
│       │   ├── snapshot.yaml
│       │   ├── snapshot-content.yaml
│       │   └── objects/
│       │       └── VirtualMachine.v1.virtualization.deckhouse.io--default--my-vm.yaml
│       ├── VirtualDiskSnapshot--default--my-vm-root-disk/
│       │   ├── snapshot.yaml
│       │   ├── snapshot-content.yaml
│       │   └── objects/
│       │       └── VirtualDisk.v1.virtualization.deckhouse.io--default--my-vm-root-disk.yaml
│       └── VirtualDiskSnapshot--default--my-vm-data-disk/
│           ├── snapshot.yaml
│           ├── snapshot-content.yaml
│           └── objects/
│               └── VirtualDisk.v1.virtualization.deckhouse.io--default--my-vm-data-disk.yaml
└── data/
    ├── VirtualDiskSnapshot--default--my-vm-root-disk.tar.gz
    │   ├── meta.yaml
    │   └── data.img
    └── VirtualDiskSnapshot--default--my-vm-data-disk.tar.gz
        ├── meta.yaml
        └── data.img
```

**Процесс импорта:**

1. Клиент открывает внешний `.tar` и читает только первый entry `manifests.tar.gz` (~КБ-МБ)
2. Клиент распаковывает `manifests.tar.gz` и парсит `index.yaml`
3. Клиент отправляет `index.yaml` (или весь `manifests.tar.gz`) на сервер
4. Сервер анализирует `index.yaml`:
   - Определяет количество снимков с данными (`hasData: true`)
   - Создаёт PVC нужных размеров (`dataSize`) для каждого снимка с данными
   - Поднимает http-серверы для приёма данных
   - Возвращает клиенту `status.dataUrls[]` с URL для каждого snapshot-id
5. Клиент находит в архиве нужные `data/<snapshot-id>.tar.gz` и стримит их на соответствующие URL
   - Можно стримить параллельно
   - Можно выборочно восстановить только часть снимков

**Пример работы в Go:**

```go
// 1. Открываем внешний .tar
tarReader := tar.NewReader(file)

// 2. Читаем первый entry — manifests.tar.gz (маленький, ~КБ-МБ)
header, _ := tarReader.Next()  // "manifests.tar.gz"
gzReader, _ := gzip.NewReader(tarReader)
manifestsTar := tar.NewReader(gzReader)

// 3. Внутри manifests.tar.gz ищем index.yaml
for {
    h, _ := manifestsTar.Next()
    if h.Name == "index.yaml" {
        index := parseIndex(manifestsTar)
        // Знаем всё о снимках: 2 диска, 10+50 GiB, checksums
        break
    }
}
gzReader.Close()

// 4. Если нужны данные конкретного снимка — ищем его entry
targetSnapshotID := "VirtualDiskSnapshot--default--my-vm-root-disk"
for {
    header, err := tarReader.Next()
    if err == io.EOF { break }
    
    // header.Name = "data/VirtualDiskSnapshot--default--my-vm-root-disk.tar.gz"
    if strings.HasSuffix(header.Name, targetSnapshotID + ".tar.gz") {
        // Нашли — распаковываем и стримим на сервер
        gzReader, _ := gzip.NewReader(tarReader)
        // ... стримим данные
        break
    }
    // Не тот снимок — tar.Next() автоматически пропустит его данные
}
```

**CLI для выборочного восстановления:**

```bash
# Показать список данных в архиве (читает только manifests.tar.gz)
$ d8 snapshot info my-vm-snapshot.tar
Snapshot: VirtualMachineSnapshot/default/my-vm
Created:  2025-01-22T12:00:00Z
Cluster:  prod-cluster

Data in archive:
  ID                                              SIZE      TYPE
  VirtualDiskSnapshot--default--my-vm-root-disk   10 GiB    block
  VirtualDiskSnapshot--default--my-vm-data-disk   50 GiB    block

Total: 2 snapshots with data, 60 GiB

# Восстановить только один диск
$ d8 snapshot restore my-vm-snapshot.tar \
    --only-data VirtualDiskSnapshot--default--my-vm-root-disk
```





TODO

Файловая структура
Ресурс SnapshotDataExport (указывается один Snapshot) или достаточно DataExport? Наверное достаточно.
Ресурс ClusterSnapshotDataExport (указывается один ClusterSnapshot) или ClusterDataExport?
Отдельный ресурс SnashotContentDataExport?
Механика кастомной реализации экспорта для определенного storageclass?
Автоматическое создание необходимого количество SnashotContentDataExport при создании SnapshotDataExport/DataExport, чтобы СРК было удобней?

#### 5.10 Загрузка (импорт) снимков

**HTTP /api/v1/* в этом разделе — endpoint сервиса DataImport**, а не Kubernetes API.
Добавить в DataImport поле status.manifestsUrl для импорта манифестов снимка из внешнего источника (URL). Изменить поле status.url на status.dataUrls: [] для указания множества URL для импорта данных снимка (если есть данные). 

##### Маппинг StorageClass при импорте

При импорте снимка в другой кластер может потребоваться маппинг StorageClass, если в целевом кластере нет тех же SC, что были в исходном. Для этого в DataImport добавляется опциональное поле `spec.storageClassMapping`:

```yaml
apiVersion: storage.deckhouse.io/v1alpha1
kind: DataImport
metadata:
  name: my-vm-import
  namespace: default
spec:
  sourceRef:
    kind: Snapshot
    name: my-vm-snapshot
  storageClassMapping:                        # Опционально: маппинг SC исходного → целевого кластера
    ceph-ssd: local-ssd
    ceph-hdd: nfs-storage
```

**Валидация StorageClass:**

- После получения манифестов (шаг 3) контроллер извлекает `storageClassName` из index.yaml для каждого снимка с данными
- Для каждого `storageClassName` проверяется:
  - Если указан маппинг в `spec.storageClassMapping` — используется целевой SC из маппинга
  - Если маппинг не указан — проверяется наличие SC с таким именем в целевом кластере
- Если SC не найден и маппинг не указан — DataImport переходит в состояние `Ready=False` с condition `StorageClassValidationFailed` и сообщением о недостающих SC
- Только после успешной валидации всех SC контроллер переходит к созданию PVC

##### Порядок работы (один DataImport для всей иерархии снимков)

1. Клиент создаёт DataImport с `spec.sourceRef.kind=Snapshot|ClusterSnapshot` и указывает имя Snapshot/ClusterSnapshot в `spec.sourceRef.name`. (Нужно продумать механизм нейминга дочерних снимков, чтобы их имя зависело от родительского снимка и не конфликтовало с уже существующими снимками в кластере)
2. Контроллер DataImport обрабатывает DataImport и добавляет в `status.manifestsUrl` URL для загрузки манифестов снимка.
3. Клиент дожидается появления поля `status.manifestsUrl` в DataImport и делает HTTP PUT запрос к этому URL с архивом manifests.tar.gz из архива снимка на эндпоинт `/api/v1/manifests`.
4. Контроллер анализирует манифесты:
   - Извлекает `storageClassName` для каждого снимка с данными
   - Выполняет валидацию StorageClass (см. выше)
   - Если валидация не прошла — DataImport переходит в Failed, клиент должен добавить `storageClassMapping` и пересоздать DataImport
   - Если валидация прошла — создаёт тома (PVC) с учётом маппинга SC и поды http-серверов для приёма данных каждого тома
5. Контроллер добавляет в `status.dataUrls: []` URL для загрузки данных снимка (если есть данные).
6. Клиент дожидается появления поля `status.dataUrls: []` в DataImport и делает HTTP PUT запросы к каждому URL из этого массива с данными снимка на эндпоинты `/api/v1/files` или `/api/v1/blocks`.
7. Клиент по завершении загрузки данных снимка вызывает метод POST `/api/v1/finished` на каждый URL из `status.dataUrls: []`, чтобы сообщить контроллеру о завершении загрузки данных снимка.
8. Контроллер дожидается вызова метода `/api/v1/finished` для всех URL из `status.dataUrls: []` (если есть данные) и после этого создаёт:

- ManifestCheckpoint'ы и ManifestCheckpointContentChunk'и на основе загруженных манифестов.
- VolumeSnapshotContent's или PersistentVolume's на основе загруженных данных снимка (если есть данные) через VRR.
- ObjectKeeper для управления жизненным циклом загруженного снимка.
- Ресурсы `XxxxSnapshotContent` на основе загруженных манифестов и проставляет в них ссылки на созданные:
  - ManifestCheckpoint'ы
  - VolumeSnapshotContent'ы/PersistentVolume'ы
  - дочерние `YyyySnapshotContent` (если есть дочерние снимки).
  - OwnerReference на ObjectKeeper у родительского `XxxxSnapshotContent`.
  - OwnerReference на родительский `XxxxSnapshotContent` у дочерних `YyyySnapshotContent`.
- Ресурсы `XxxxSnapshot` на основе загруженных манифестов и проставляет в них ссылки на созданные `XxxxSnapshotContent`'ы и сразу ставит им состояние Ready=True.

status.conditions.[type=HandledByDomainSpecificController] = true
status.conditions.[type=handledByCommonController] = true

#### 5.11 Зависимость между модулями

- Модуль в котором расположен домен-специфичный контроллер:
  - Всегда зависит от модуля state-snapshotter.
  - Если `Xxxx` имеет данные, то дополнительно зависит от модуля storage-foundation.
- Общий контролер, находящийся в модуле state-snapshotter, отслеживает состояние включенности модуля storage-foundation, и если он не включен — не пытается подписываться на его ресурсы (VCR).

### 6. Нерешённые вопросы
- Совместимость VRR с StorageClass, где включён WFFC. Это будет решено в рамках отдельного ADR.
<!-- - Кросс-StorageClass восстановление (VSC/PV из одного SC → PVC в другом SC): допускается, но работать сейчас не будет. Будет реализовано в рамках отдельного ADR. --> // TODO: ??
- Восстановление снимков из корзины.

### 7. TODO

1. Регистрация видов снимков (cluster/ns, data?, children?, with-data-hook?) — метаданные или автоматическое определение характеристик снимка
2. CLI для скачивания/загрузки — детали реализации CLI для экспорта/импорта снимков
3. Добавить `status.capturedResources` в ManifestCheckpoint — список захваченных ресурсов (apiVersion/kind/name) для быстрой проверки без разбора chunks. ManifestCaptureRequest удаляется через 10 минут после завершения, и после этого информация о захваченных ресурсах становится недоступной. Это нужно общему контроллеру при определении "ресурс уже в снимке".
4. Продумать регистрацию доменных снимков:
  - RBAC?
    - RBAC для общего контроллера должен приносится в модуле домен специфичного контроллера?
    - go хук в общем контроллере, который выдает права на доменные ресурсы
  - Ресурc-регистрация

```yaml
apiVersion: state-snapshotter.deckhouse.io/v1alpha1
kind: DomainSpecificSnapshotController
metadata:
  name: <>
spec:
  snapshotResourceMapping:
    - resourceCRDName: virtualmachines.virtualization.deckhouse.io
      snapshotCRDName: virtualmachinesnapshots.virtualization.deckhouse.io
      priority: 0
    - resourceCRDName: virtualdisks.virtualization.deckhouse.io
      snapshotCRDName: virtualdisksnapshots.virtualization.deckhouse.io
      priority: 1
  modificationWebhookService:
    name: <>
    namespace: <>
```

5. Автоматические сабресурсы для всех видов снимков в общей части?
6. В какой API группе? Может в общей snapshots.deckhouse.io положить все?
7. CLI для создания снапшотов?
8. Переименовать state-snapshotter в snapshot-foundation?

### Примеры реализации снимков

#### NamespaceSnapshot
Исключаем из снимка ресурсы, которые имеют `ownerReference` на ресурсы, которые уже попали в снимок.
Мы считаем, что в снапшот попали следующие объекты:
  - Все ресурсы, которые находятся в ManifestCheckpoint
  - Все ресурсы, для которых владелец - это ресурс из ManifestCheckpoint (рекурсивно)
  - Тоже самое для дочерних снапшотов
