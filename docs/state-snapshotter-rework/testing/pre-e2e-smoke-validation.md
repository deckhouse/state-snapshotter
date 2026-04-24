# Pre-E2E Smoke Validation (Passed)

Дата: 2026-04-24  
Статус: **pre-e2e-passed**

Короткий smoke-check через `kubectl` выполнен перед полноценным e2e. Результат: кластер и контроллер в рабочем состоянии, базовый namespace-local flow подтверждён.

## Использованный чеклист

1. Контекст кластера и `d8-state-snapshotter` (`cluster-info`, `ns`, `pods`, `deploy`).
2. Проверка наличия CRD (`namespacesnapshot*`, `demovirtual*`, `domainspecificsnapshotcontrollers`, `manifestcapture*`).
3. Проверка схемы `childrenSnapshotRefs` через `kubectl explain`:
   - есть только `apiVersion`, `kind`, `name` (required);
   - поля `namespace` нет.
4. Предварительный просмотр логов контроллера на `panic|fatal|stacktrace|error`.
5. Создание smoke namespace и простого объекта.
6. Создание root `NamespaceSnapshot`.
7. Проверка bind `NamespaceSnapshot -> NamespaceSnapshotContent`.
8. Проверка `MCR/MCP` и `manifestCheckpointName`.
9. Проверка `Ready` у root snapshot и content.
10. Проверка, что `childrenSnapshotRefs` пуст для простого root.
11. Создание demo child (`DemoVirtualDiskSnapshot`) и проверка graph refs.
12. Проверка `childrenSnapshotContentRefs` на root NSC.
13. Проверка, что parent просыпается от child status.
14. Удаление root и проверка cleanup/retain поведения.
15. Финальная проверка логов контроллера.

## Фактический результат

- **Контекст и доступность**
  - `kubectl cluster-info` OK.
  - `d8-state-snapshotter` существует.
  - Pods `controller` и `webhooks` в `Running`.
  - Deployments готовы (`1/1`).

- **CRD**
  - В кластере присутствуют:
    - `namespacesnapshots.storage.deckhouse.io`
    - `namespacesnapshotcontents.storage.deckhouse.io`
    - `manifestcapturerequests.state-snapshotter.deckhouse.io`
    - `manifestcheckpoints.state-snapshotter.deckhouse.io`
    - demo CRD (`demovirtualdisksnapshots`, `demovirtualmachinesnapshots`)
    - `domainspecificsnapshotcontrollers...`

- **Schema `childrenSnapshotRefs`**
  - `kubectl explain namespacesnapshot.status.childrenSnapshotRefs` подтверждает только required:
    - `apiVersion`
    - `kind`
    - `name`
  - `kubectl explain namespacesnapshot.status.childrenSnapshotRefs.namespace` возвращает `field "namespace" does not exist` (ожидаемо).

- **Логи контроллера**
  - До smoke были исторические строки leader-election (`leader election lost`), без panic/stacktrace.
  - После smoke в хвосте логов не найдено `panic|fatal|stacktrace|error`.

- **Root flow (`nss-smoke`)**
  - Созданы `nss-smoke` и `ConfigMap smoke-cm`.
  - Создан `NamespaceSnapshot/root`.
  - Появился `status.boundSnapshotContentName`:
    - `ns-4414190b366e44a9bd50a77845d80576`
  - Соответствующий `NamespaceSnapshotContent` существует и ссылается на `nss-smoke/root`.

- **MCR/MCP**
  - `ManifestCaptureRequest` после успешного capture отсутствует (очистка).
  - `ManifestCheckpoint` существует:
    - `mcp-ca752c09efdf9b7f`
    - `Ready=True`, `Reason=Completed`
  - `NamespaceSnapshotContent.status.manifestCheckpointName` указывает на тот же MCP.

- **Ready состояние**
  - `NamespaceSnapshot/root`: `Bound=True ContentCreated`, `Ready=True Completed`.
  - `NamespaceSnapshotContent`: `Ready=True Completed`.

- **Graph refs до demo child**
  - `root.status.childrenSnapshotRefs` пусто (ожидаемо).

- **Demo child и namespace-local refs**
  - Короткий манифест child без `rootNamespaceSnapshotRef.apiVersion/kind` отклонён CRD-валидацией (ожидаемо).
  - После применения валидного манифеста (`apiVersion/kind/name` + `persistentVolumeClaimName`) child создан.
  - В `root.status.childrenSnapshotRefs` появился strict ref **без `namespace`**:
    - `apiVersion: demo.state-snapshotter.deckhouse.io/v1alpha1`
    - `kind: DemoVirtualDiskSnapshot`
    - `name: disk-a`
  - `DemoVirtualDiskSnapshot` перешёл в `Ready=True Completed`.
  - В root NSC появились `childrenSnapshotContentRefs`.
  - Root не завис в `ChildSnapshotPending`, остался `Ready=True Completed`.

- **Удаление root**
  - `kubectl delete namespacesnapshot root --wait=false` — root удалён.
  - `NamespaceSnapshotContent ns-441...` сохранился по retain-модели:
    - `spec.deletionPolicy: Retain`
    - `ownerReference` на `ObjectKeeper`
    - `Ready=True Completed`

## Вывод

Минимальный pre-e2e критерий выполнен:

- pod'ы контроллера живы;
- root `NamespaceSnapshot` успешно создаётся и бинится к content;
- manifest capture завершается (`MCP Ready=True Completed`);
- root `Ready=True Completed`;
- demo child корректно добавляется в `childrenSnapshotRefs` в формате `apiVersion/kind/name` без `namespace`;
- parent корректно реагирует на child status;
- критичных ошибок в логах контроллера не обнаружено.
