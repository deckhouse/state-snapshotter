# ТЗ: Уточнение scope и семантики Ready condition для request-style ресурсов

## Контекст

В API state-snapshotter определены общие константы `ConditionReason*`, используемые в Ready condition.

На данный момент из кода и комментариев неочевидно, для каких ресурсов эти condition и reason предназначены:
- request-ресурсов (например, `ManifestCaptureRequest`)
- или артефакт-ресурсов (например, `ManifestCheckpoint`)

Это создаёт архитектурную неоднозначность:
- ресурсы выглядят логически связанными через общий condition-контракт
- появляется риск ошибочного переиспользования reason в других CRD
- в будущем это может осложнить независимое развитие и версионирование ресурсов

## Цель

Явно зафиксировать, что текущая семантика Ready condition и соответствующие reason:
- предназначены только для request-style ресурсов
- описывают жизненный цикл долгоживущей операции, управляемой контроллером
- не являются универсальными и не должны применяться к артефакт-ресурсам

Без изменения текущего API и поведения контроллера.

## Область действия

- API: `api/v1alpha1/conditions.go`
- Контроллер: `ManifestCaptureRequestController`
- Документация / комментарии

## Требования

### 1. Явно указать scope condition

В `conditions.go` необходимо добавить верхнеуровневый комментарий, который:
- указывает, что Ready condition и `ConditionReason*`:
  - относятся к request-style ресурсам
  - применяются, в частности, к `ManifestCaptureRequest`
- подчёркивает, что:
  - артефакт-ресурсы (например, `ManifestCheckpoint`) имеют собственную семантику
  - совпадение имён condition не означает общий контракт

**Пример формулировки (рекомендуемая):**

```go
// Ready condition semantics defined in this file are intended for
// request-style resources (e.g. ManifestCaptureRequest).
//
// They describe the lifecycle of a long-running controller-driven operation:
// accepted → processing → completed / failed.
//
// These reasons are NOT intended to be universal for all resources
// and MUST NOT be reused for artifact-style resources
// (e.g. ManifestCheckpoint, VolumeSnapshotContent, etc.).
//
// Other resources may define their own Ready/Conditions semantics
// even if condition names overlap.
```

### 2. Зафиксировать текущий контракт Ready

Текущий контракт считается окончательным в рамках данного ТЗ:

| Ready.status | Ready.reason | Семантика | Терминальность |
|--------------|--------------|-----------|----------------|
| False | Processing | Операция выполняется | ❌ нет |
| True | Completed | Операция успешно завершена | ✅ да |
| False | Failed | Операция не выполнена | ✅ да |

- `Processing` — единственное нетерминальное состояние
- Все ошибки сводятся к `Failed`
- Детализация ошибок передаётся через `message`

### 3. Не менять поведение существующих ресурсов

В рамках данного ТЗ запрещено:
- разделять API на разные файлы / пакеты
- переименовывать существующие константы
- менять поведение контроллеров
- вводить новые reason

ТЗ носит уточняющий и фиксирующий характер, а не рефакторинговый.

## Не входит в scope (явно)

- Разделение condition-контрактов для разных CRD
- Введение resource-specific reason enum'ов
- Изменение CRD схемы
- Изменение версионирования API

## Ожидаемый результат

- Из кода и комментариев однозначно понятно, что:
  - данный Ready condition относится к `ManifestCaptureRequest` и подобным request-ресурсам
  - `ManifestCheckpoint` и другие артефакты не обязаны следовать этому контракту
- Снята архитектурная неоднозначность
- Устранён риск неправильного переиспользования condition-семантики

## Критерии приёмки

- В `conditions.go` присутствует явный комментарий о scope
- Комментарий читается без знания внутренней архитектуры
- Поведение контроллера не изменено
- Тесты проходят без изменений

