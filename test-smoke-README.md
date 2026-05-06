# Smoke Test для Unified Snapshots Controller

## Описание

Bash-скрипт для проверки работы контроллера unified snapshots в реальном кластере.

## Использование

```bash
./test-smoke.sh [namespace] [snapshot-kind]
```

### Примеры

```bash
# Тест с Snapshot в namespace default
./test-smoke.sh default Snapshot

# Тест с Snapshot в namespace d8-backup
./test-smoke.sh d8-backup Snapshot
```

## Что проверяет скрипт

1. **Создание Snapshot** - создает тестовый Snapshot
2. **Создание SnapshotContent** - проверяет автоматическое создание SnapshotContent
3. **Finalizer** - проверяет добавление finalizer на SnapshotContent
4. **Custom Snapshot Controller Simulation** - устанавливает condition `HandledByCustomSnapshotController`
5. **Ready State** - устанавливает Ready=True на SnapshotContent и проверяет propagation на Snapshot
6. **ObjectKeeper** - проверяет создание ObjectKeeper (best-effort, для root snapshots)
7. **Orphaning** - удаляет Snapshot и проверяет удаление finalizer с SnapshotContent

## Требования

- Доступ к кластеру через `kubectl`
- CRD для Snapshot и SnapshotContent установлены
- Контроллер запущен в namespace `d8-state-snapshotter`

## Очистка

Скрипт автоматически очищает созданные ресурсы при завершении (включая удаление finalizers).

## Выходные коды

- `0` - все тесты прошли успешно
- `1` - один или несколько тестов провалились

