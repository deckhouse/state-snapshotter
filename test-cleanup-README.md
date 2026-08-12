# Cleanup Script для Unified Snapshots

## Описание

Отдельный скрипт для безопасной очистки тестовых ресурсов Unified Snapshots. Может использоваться независимо от основного smoke-теста.

## Использование

```bash
./test-cleanup.sh [options]
```

### Опции

- `--snapshot-name NAME` - Очистить конкретный Snapshot по имени
- `--namespace NAMESPACE` - Namespace для очистки (по умолчанию: default)
- `--snapshot-kind KIND` - Тип Snapshot (по умолчанию: Snapshot)
- `--all` - Очистить все тестовые ресурсы (по паттерну test-smoke-*)
- `--dry-run` - Показать что будет удалено без фактического удаления
- `--force` - Принудительная очистка (удаление finalizers)
- `--help, -h` - Показать справку

### Примеры

```bash
# Очистить конкретный Snapshot
./test-cleanup.sh --snapshot-name test-smoke-1234567890

# Очистить все test-smoke-* ресурсы в namespace
./test-cleanup.sh --all --namespace default

# Принудительная очистка (удаление finalizers)
./test-cleanup.sh --snapshot-name test-smoke-1234567890 --force

# Dry-run (показать что будет удалено)
./test-cleanup.sh --all --dry-run

# Очистить Snapshot
./test-cleanup.sh --snapshot-name test-smoke-1234567890 --snapshot-kind Snapshot
```

## Что очищает скрипт

1. **Snapshot** - удаляет указанный Snapshot
2. **SnapshotContent** - удаляет связанный SnapshotContent
3. **Finalizers** - удаляет finalizers при использовании `--force`
4. **ObjectKeeper** - удаляет связанный ObjectKeeper (если существует)
5. **Orphaned ресурсы** - при использовании `--all` находит и очищает orphaned SnapshotContents

## Безопасность

- По умолчанию скрипт не удаляет finalizers (безопасно)
- Используйте `--force` только если ресурсы "застряли"
- `--dry-run` позволяет проверить что будет удалено перед фактическим удалением
- Скрипт не удаляет ресурсы, которые не соответствуют паттерну `test-smoke-*`

## Интеграция с test-smoke.sh

Основной скрипт `test-smoke.sh` автоматически использует `test-cleanup.sh` для cleanup, если он доступен. Если скрипт cleanup недоступен, используется fallback inline cleanup.

## Рекомендации

1. **После каждого теста**: Запускайте cleanup для удаления тестовых ресурсов
   ```bash
   ./test-cleanup.sh --snapshot-name <snapshot-name> --force
   ```

2. **Периодическая очистка**: Используйте `--all` для очистки всех тестовых ресурсов
   ```bash
   ./test-cleanup.sh --all --namespace default --force
   ```

3. **Перед удалением**: Используйте `--dry-run` для проверки
   ```bash
   ./test-cleanup.sh --all --dry-run
   ```

4. **При проблемах**: Используйте `--force` для принудительной очистки застрявших ресурсов

