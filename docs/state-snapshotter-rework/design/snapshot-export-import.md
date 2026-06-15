# Snapshot Export / Import (SnapshotExport / SnapshotImport)

Реализация экспорта/импорта иерархии снимков. Нормативные решения и согласование с
предыдущими ADR — в ADR `dkp/storage/state-snapshotter/2026-06-15-snapshot-export-import.md`
(репозиторий architecture-decision-records). Этот документ — SSOT по фактической
реализации в коде `images/state-snapshotter-controller/`.

`d8 snapshot export|import` остаётся тонким клиентом: вся оркестрация — в контроллерах
модуля. Транспорт (`DataExport`/`DataImport` из storage-volume-data-manager) и
захват/восстановление томов (`VolumeCaptureRequest`/`VolumeRestoreRequest` из
storage-foundation) переиспользуются как per-leaf примитивы.

---

## 1. API-поверхность

Два namespaced CR в `storage.deckhouse.io/v1alpha1` (CRD в `crds/`, типы в
`api/storage/v1alpha1/`):

- **`SnapshotExport`** (`spec.snapshotRef.name -> Snapshot` в том же namespace).
  `status`: `indexURL`, `manifestsURL`, `dataSnapshots[]` (`snapshotID`, `dataURL`,
  `dataCA`, `ready`), условия `Ready`/`DataReady` с причинами
  (`Published`/`DataPending`/`DataExportFailed`/...; пустой `volumeMode` тоже
  surface-ится как `DataExportFailed`).
- **`SnapshotImport`** (`spec.snapshotName` — имя будущего корневого `Snapshot`;
  `spec.storageClassMapping` опционально; `spec.publish`). `status`:
  `indexUploadURL`, `manifestsUploadURL`, `dataSnapshots[]` (`uploadURL`, `uploadCA`,
  `uploadReady`, `uploaded`, `capturedSnapshotContentName`), условия
  `Ready`/`UploadsPrepared`/`IndexReceived`/`ManifestsReceived`/`DataReceived`/
  `Captured` с причинами (`Imported`/`AllCaptured`/`StorageClassMappingRequired`/
  `DataSizeUnknown`/`IndexUnreadable`/...).

Асимметрия `snapshotRef` (export — ссылка на существующий `Snapshot`) vs
`snapshotName` (import — имя создаваемого `Snapshot`) намеренная.

Расширения существующих типов:

- **`SnapshotContent.status.dataRefs[]`** (`SnapshotDataBinding`) — добавлены
  `volumeMode`, `fsType`, `accessModes`, `storageClassName` (строковые типы —
  `accessModes` это `[]string`, остальные `string` — чтобы не тащить
  `k8s.io/api/core/v1` в лёгкий api-модуль). Метаданные исходного тома (CSI
  снапшот mode-agnostic) нужны для унифицированного экспорта.
- **Статическая привязка (pre-provisioning):** `Snapshot.spec.source.snapshotContentName`
  (required+MinLength при заданном `source`) и обратная ссылка
  `SnapshotContent.spec.snapshotRef` (`apiVersion/kind/name/namespace/uid`, immutable).

Метаданные тома заполняются при захвате (`EnrichDataBindingsWithVolumeMetadata`:
исходный PVC + дочитка PV для `fsType` через direct reader) и при импорте.

---

## 2. Поток экспорта (`SnapshotExportReconciler`)

Унифицированный per-leaf путь, без спец-таргетов в `DataExport`:

1. резолв дерева `SnapshotContent`; на каждый snapshot-узел берётся ровно один
   data-лист (`collectDataLeaves` — один data-binding на узел, уникальный `snapshotID`);
2. `VolumeRestoreRequest(VSC) -> restored PVC` с `volumeMode`/`fsType`/`accessModes`
   из `dataRefs[]` (fail-closed при пустом `volumeMode`);
3. `SnapshotExport` берёт restored PVC под non-controller hold-owner
   (`Controller=false`, `BlockOwnerDeletion=false`), чтобы PVC пережил SF-TTL-scan VRR;
4. `DataExport(targetRef.kind=PersistentVolumeClaim)` -> публикация `dataURL`/`dataCA`;
5. `publishStatus`: `indexURL` + единый `manifestsURL` (всё дерево) + per-snapshot
   `dataURL`; терминальные ошибки VRR/DataExport поднимаются в `DataReady=False`
   c `DataExportFailed` и backoff-requeue (30s терминальные / 5s pending).

Имена промежуточных ресурсов — `resourceBaseName` с `export.UID` в хэше (нет коллизий
между разными `SnapshotExport`).

Per-node манифесты доступны симметрично импорту: `GET .../snapshots/{name}/manifests?node=<id>`.

---

## 3. Поток импорта (`SnapshotImportReconciler`)

Стадийный latching-конвейер (после `Captured=True` промежуточные ресурсы не
пересоздаются — записи берутся из `status.dataSnapshots`):

1. публикация `indexUploadURL`/`manifestsUploadURL`; ожидание `IndexReceived`;
2. чтение/парс индекса; резолв target StorageClass (identity или
   `storageClassMapping`); fail-closed при неразрешённом SC
   (`StorageClassMappingRequired`) и при неизвестном размере (`DataSizeUnknown`);
3. `DataImport(targetRef=PVC)` на каждый data-узел; приём данных (возобновляемо,
   `X-Offset` / `POST /api/v1/finished`);
4. на наполненный PVC — `VolumeCaptureRequest(mode=Snapshot)` -> durable
   `VolumeSnapshotContent` (имя из `vcr.status.dataRefs[].artifact.name`);
5. `Captured=True` -> cleanup: удалить `DataImport` (SVDM снимает finalizer и сносит
   importer pod/ingress), затем PVC (порядок важен, иначе PVC завис в `Terminating`);
6. после `ManifestsReceived` — реконструкция per-node `ManifestCheckpoint`
   (`ReconstructManifestCheckpoint`) и pre-provisioning дерева: cluster-scoped
   `SnapshotContent` (`dataRefs[].artifact=VSC` + метаданные тома + mapped
   `storageClassName` + `manifestCheckpointName` + `spec.snapshotRef`) и статически
   привязанные `Snapshot`/доменные CR (`spec.source.snapshotContentName`). Корневому
   узлу присваивается `spec.snapshotName`.

`reconcileDelete` явно удаляет per-node upload-blob'ы (`ManifestCheckpointContentChunk`
индекса и манифестов).

Чтения/записи `SnapshotContent` идут через cache-bypassing direct client
(read-after-create консистентность).

---

## 4. Aggregated API: загрузка индекса и манифестов

`snapshotimports/{name}/index` и `snapshotimports/{name}/manifests` — идемпотентные
возобновляемые upload-хендлеры (`X-Offset` / `HEAD X-Next-Offset` / финализация).
Манифесты загружаются per-node (`?node=<snapshotId>`), глобальный `finalize`
сигнализирует завершение. Данные складываются в `ImportBlobStore` поверх
cluster-scoped `ManifestCheckpointContentChunk` (поле `rawBytes` для O(N) resume,
SHA-256 checksum проверяется при чтении, label/annotation для GC сирот). `/index`
(read) на `Snapshot` отдаёт машиночитаемую иерархию + per-snapshot
`volumeMode/fsType/accessModes/storageClassName/size`.

---

## 5. Namespace промежуточных ресурсов и RBAC

`SnapshotExport`/`SnapshotImport` — пользовательские ресурсы; авторизация транспорта
в SVDM namespace-scoped, data-pod монтирует PVC в своём namespace. Поэтому **все**
промежуточные ресурсы (`DataExport`/`DataImport`, restored/populated PVC, `VRR`, `VCR`)
создаются в namespace CR (= namespace пользователя); cluster-scoped
`VolumeSnapshotContent`/`SnapshotContent` — вне namespace.

RBAC контроллера — единый источник в `templates/controller/rbac-for-us.yaml`
(`+kubebuilder:rbac` маркеры убраны): `get/list/watch/update/patch` + `/status` на
`SnapshotExport`/`SnapshotImport` (без `create`/`delete` — CR создаёт и удаляет CLI;
эти глаголы живут в admin-роли `templates/rbac-for-us.yaml`), CRUD на
`DataExport`/`DataImport`, update/patch/delete на `PersistentVolumeClaim`.
Пользовательский RBAC (`templates/rbac-for-us.yaml`) включает функциональный грант
`create dataexports/download` и `create dataimports/download` (SAR транспорта SVDM).

---

## 6. CLI (`deckhouse-cli`)

`d8 snapshot export create|download|delete` и `d8 snapshot import create|upload|delete`.
Download — возобновляемый HTTP `Range`; upload — `X-Offset`/`X-Next-Offset` + `/finished`
с bounded 409-конвергенцией. On-disk layout: `index.json`, `manifests/<nodeID>.json`,
`data/<nodeID>.img` (+ проверка `Index.Version`). v0 — Block-режим; Filesystem —
fail-fast TODO.

---

## 7. Тесты

- Unit: `ImportBlobStore`, `ImportUploadHandler`, `ReconstructManifestCheckpoint`,
  `EnrichDataBindingsWithVolumeMetadata`, static-bind helpers, `resourceBaseName`/
  naming/`uploadsReason`/`nodesMissingSize`/`recreatedName` и пр.
- CLI httptest: возобновляемые download/upload state-machine (`data_test.go`,
  `transport_test.go`).
- envtest integration (`test/integration/`): `snapshot_export_test.go` (happy +
  volumeMode fail-closed), `snapshot_static_bind_test.go` (bind/misbound/missing),
  `snapshot_import_test.go` (index/manifests pre-seed -> DataImport -> VCR ->
  pre-provision + cleanup PVC). Внешние контроллеры (VRR/DataExport/DataImport/VCR)
  симулируются через preserve-unknown CRD (`integration_exportimport_crd.go`) и
  прямую запись `status`.

---

## 8. Открытые вопросы

- Чек-суммы данных в `/index` — вне v1.
- VRR + `WaitForFirstConsumer` StorageClass — наследуется из unified-snapshots.
- Перенос scratch в служебный namespace (брокер-доступ SVDM) — вне области.
- `dataRefs[].snapshotRef` (per-binding ссылка на `VolumeSnapshot`) — остаётся TODO.
- Filesystem-режим в CLI download/upload — fail-fast, не реализован.
