# Приведение VolumeSnapshot к паттерну unified-snapshots

> **Модуль `state-snapshotter`:** нормативный контракт и план — [`docs/state-snapshotter-rework/spec/system-spec.md`](../docs/state-snapshotter-rework/spec/system-spec.md), [`design/implementation-plan.md`](../docs/state-snapshotter-rework/design/implementation-plan.md).

## 1. Контекст и цель

В Kubernetes `VolumeSnapshot/VolumeSnapshotContent` уже дают снапшот данных PVC, но не содержат манифестов. Согласно ADR `2025-11-30-unified-snapshots.md` каждый snapshot-content должен иметь ManifestCheckpoint (MC) с манифестами исходного объекта. Цель: добавить MC к VSC, не меняя основную логику CSI snapshotter (создание VSC/readyToUse остаётся как есть).

<!-- ## 2. Вариант A: патч snapshot-controller, расширяем VSC
- При обработке `VolumeSnapshot` (VS):
  1. Создать `ManifestCaptureRequest` (MCR) в том же namespace, таргет — исходный PVC VS; дождаться Ready → получить `manifestCheckpointName`.
  2. Добавить в **VSC** поле `status.manifestCheckpointName` и заполнить его.
  3. Поставить **VSC** owner'ом в ManifestCheckpoint.
  4. Не ставить Ready VS, пока не завершена обработка MCR (MC создан и привязан).
- При удалении VS: VSC удаляется стандартной логикой snapshot-controller, MC удаляется, потому что его owner — VSC. -->

## 2. Принятое решение: VolumeSnapshot через VCR + MCR (единый поток)

Чтобы привести стандартный Kubernetes `VolumeSnapshot` к паттерну unified-snapshots, мы патчим компоненты CSI стека (`snapshot-controller` и `external-snapshotter/provisioner`) так, чтобы при создании `VolumeSnapshot` использовались служебные ресурсы `VolumeCaptureRequest` (VCR) и `ManifestCaptureRequest` (MCR).

### 2.1 Обзор патчей CSI стека

Патчи вносятся в компоненты, поставляемые модулем **storage-foundation**:

| Компонент | Изменение |
|-----------|-----------|
| **snapshot-controller** | При обработке VS создаёт VCR + MCR вместо прямого CSI CreateSnapshot |
| **external-snapshotter sidecar** | Поддержка VSC без VolumeSnapshot (создание/удаление через VCR) |
| **external-provisioner sidecar** | Без изменений в рамках данного ADR |

### 2.2 Алгоритм создания VolumeSnapshot

При создании `VolumeSnapshot` (VS) пропатченный **snapshot-controller** выполняет следующий алгоритм:

1. **Создание VCR и MCR** (параллельно):
   - Создаёт `VolumeCaptureRequest` (VCR) с `spec.mode: Snapshot` для исходного PVC, указанного в `VS.spec.source.persistentVolumeClaimName`. Имя VCR связано с VS (например, `vcr-<vs-name>`).
   - Создаёт `ManifestCaptureRequest` (MCR) в том же namespace с таргетом на исходный PVC.

2. **Ожидание завершения**:
   - Ждёт, пока VCR перейдёт в состояние `Ready=True` → получает из `status.dataRef` ссылку на созданный `VolumeSnapshotContent` (VSC).
   - Ждёт, пока MCR перейдёт в состояние `Ready=True` → получает из `status.manifestCheckpointName` имя созданного `ManifestCheckpoint`.

3. **Формирование итоговых объектов**:
   - Создаёт `ObjectKeeper` для управления жизненным циклом контента (реализация «корзины» с TTL).
   - Обновляет VSC:
     - Добавляет `status.manifestCheckpointName` (ссылка на ManifestCheckpoint).
     - Устанавливает `ownerReference` на `ObjectKeeper`.
   - Устанавливает `ownerReference` на VSC для ManifestCheckpoint.
   - Обновляет VS:
     - Проставляет `status.boundVolumeSnapshotContentName` (ссылка на VSC).
     - Устанавливает `readyToUse: true` только после успешного завершения обоих запросов.

4. **Очистка временных ресурсов**:
   - VCR и MCR удаляются по истечении TTL (~10 мин) после перехода в Ready.

### 2.3 Расширение VolumeSnapshotContent

В VSC добавляется новое поле в status:

```yaml
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshotContent
metadata:
  name: snapcontent-<uuid>
  ownerReferences:
    - apiVersion: state-snapshotter.deckhouse.io/v1alpha1
      kind: ObjectKeeper
      name: <object-keeper-name>
spec:
  # ... стандартные поля VSC
status:
  # ... стандартные поля
  manifestCheckpointName: string  # НОВОЕ: ссылка на ManifestCheckpoint (cluster-scoped)
```

### 2.4 Удаление VolumeSnapshot

При удалении VS:
1. VS удаляется стандартной логикой snapshot-controller.
2. ObjectKeeper фиксирует удаление VS и запускает отсчёт TTL.
3. По истечении TTL ObjectKeeper удаляется → каскадное удаление VSC.
4. VSC удаляется → каскадное удаление ManifestCheckpoint (через ownerReference).

### 2.5 Диаграмма потока

```
VolumeSnapshot (VS)
       │
       ▼
┌──────────────────────────────────────────────────────────────┐
│                 snapshot-controller (патч)                   │
├──────────────────────────────────────────────────────────────┤
│  1. Создаёт VCR (mode: Snapshot)  ──► VSC (через CSI)        │
│  2. Создаёт MCR ───────────────────► ManifestCheckpoint      │
│  3. Ждёт Ready (VCR + MCR)                                   │
│  4. Создаёт ObjectKeeper                                     │
│  5. Связывает: VSC ◄─owner─ ObjectKeeper                     │
│                MC  ◄─owner─ VSC                              │
│  6. Обновляет VS: readyToUse=true, boundVSCName              │
└──────────────────────────────────────────────────────────────┘
       │
       ▼
VolumeSnapshotContent (VSC)
├── status.manifestCheckpointName ──► ManifestCheckpoint
└── ownerReference ──► ObjectKeeper
```

### 2.6 Связь с VolumeCaptureRequest

VCR (`VolumeCaptureRequest`) — это служебный ресурс, описанный в ADR `2025-11-30-volume-capture-and-restore-request.md`. Он предоставляет:
- Унифицированный способ создания CSI-снапшота без публичного VolumeSnapshot.
- Результат в `status.dataRef` (ссылка на VSC или PV).
- Управление жизненным циклом через ObjectKeeper.

При использовании в контексте VolumeSnapshot, VCR выступает как execution-слой, а VS остаётся пользовательским API.

### 2.7 Преимущества и недостатки

**Преимущества:**
- Единый поток создания снимков с `ObjectKeeper` и `ManifestCheckpoint`, без дублирования логики.
- Переиспользование инфраструктуры VCR/MCR, уже реализованной для внутренних контроллеров.
- VS остаётся стандартным Kubernetes API для пользователей.
- Консистентность с паттерном unified-snapshots: каждый VSC имеет ManifestCheckpoint.

**Недостатки:**
- Более глубокое изменение snapshot-controller по сравнению с минимальным патчем.
- Требуются патчи external-snapshotter (уже планируются в CSI foundation refactor).
- VS становится "тонкой прокладкой" над VCR — увеличивается количество создаваемых объектов.

### 2.8 Связанные ADR

- `2025-11-30-unified-snapshots.md` — паттерн XxxxSnapshot/XxxxSnapshotContent
- `2025-11-30-volume-capture-and-restore-request.md` — VCR/VRR служебная инфраструктура
- `2025-11-30-manifest-capture-checkpoint.md` — ManifestCaptureRequest/ManifestCheckpoint