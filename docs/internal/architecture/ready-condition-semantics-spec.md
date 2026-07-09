# ТЗ: Уточнение семантики Ready condition в state-snapshotter

## Цель изменений

Повысить прозрачность и предсказуемость пользовательского статуса Request-ресурсов (ManifestCaptureRequest и аналогичных), чтобы:
- пользователь явно видел, что операция принята в работу контроллером;
- `Ready=False` не интерпретировался автоматически как ошибка;
- статус позволял отличить:
  - «нужно подождать»
  - от «операция завершилась неуспешно».

Это критично при высокой нагрузке на control plane, сетевых задержках и деградации контроллеров.

---

## Область изменений

- **API**: `api/v1alpha1/conditions.go`
- **Контроллер**: `state-snapshotter-controller`
- **Ресурс**: `ManifestCaptureRequest` (далее MCR)
- **Паттерн должен быть применим** и к другим Request-ресурсам при необходимости.

---

## Контракт Ready condition (новая семантика)

Используется только один condition: `Ready`

### Значения и смыслы

| `Ready.Status` | `Ready.Reason` | Терминальность | Семантика |
|----------------|----------------|----------------|-----------|
| `False` | `Processing` | ❌ нет | Контроллер принял ресурс в работу, операция выполняется |
| `True` | `Completed` | ✅ да | Операция успешно завершена |
| `False` | `Failed` | ✅ да | Операция завершилась ошибкой |

---

## Удалённые / неиспользуемые причины

### ❌ InvalidSpec

- **Удаляется из API.**
- **Причина:**
  - семантическая валидация должна выполняться до reconcile:
    - CRD validation
    - validating webhook
  - пользователю не важно, почему операция не выполнилась (spec или исполнение) — важно, что она не выполнилась.

### ❌ InternalError

- **Удаляется или объединяется с Failed.**
- **Причина:**
  - с точки зрения пользователя:
    - «сломался контроллер»
    - «сломалась инфраструктура»
    - «сломался API»
  - это одна и та же категория: операция не выполнена.
  - различие имеет смысл только для логов и метрик, но не для user-facing API.

---

## Итоговый список Reason

```go
const (
    // ConditionReasonProcessing indicates that the controller has accepted the request
    // and the operation is currently in progress.
    // This is the ONLY non-terminal Ready=False state.
    ConditionReasonProcessing = "Processing"

    // ConditionReasonCompleted indicates successful completion of the operation.
    // This is the ONLY allowed reason for Ready=True.
    ConditionReasonCompleted = "Completed"

    // ConditionReasonFailed indicates that the operation could not be completed.
    // Covers all terminal failure cases regardless of root cause.
    ConditionReasonFailed = "Failed"
)
```

---

## Поведение контроллера

### 1. Установка Processing

- `Ready=False` + `reason=Processing` обязателен:
  - устанавливается в начале обработки
  - до любых потенциально долгих операций
- **Обновление статуса:**
  - идемпотентно
  - не перезаписывает `Processing`, если он уже установлен
  - `CompletionTimestamp` НЕ устанавливается

### 2. Завершение операции

#### Успех
- `Ready=True` + `reason=Completed`
- Устанавливается `CompletionTimestamp`

#### Ошибка
- `Ready=False` + `reason=Failed`
- Устанавливается `CompletionTimestamp`
- Ошибка прокидывается в `message`

---

## Терминальность ресурса

Ресурс считается терминальным, если:
- `Ready=True`
- или `Ready=False` и `reason ≠ Processing`

Терминальные ресурсы:
- не обрабатываются повторно
- участвуют в TTL cleanup
- считаются immutable

---

## Пользовательский UX контракт

С точки зрения пользователя:
- **Processing** → операция началась, нужно подождать
- **Completed** → всё успешно
- **Failed** → операция не удалась, смотри `message`

Причины внутренних различий (spec, API, infra, bug) не выносятся в API, а отражаются:
- в `message`
- в логах контроллера
- в метриках

---

## Причины принятия решения

- Минимизация когнитивной нагрузки для пользователя
- Устранение неоднозначности `Ready=False`
- Чёткий и простой контракт
- Отсутствие «полулегаси» причин в новом API
- Совместимость с fire-and-forget моделью Request-ресурсов

---

## Не входит в это ТЗ

- Доработка validating webhook (отдельная задача)
- Расширение CRD validation
- Детализация ошибок в structured status (возможное будущее)

---

## Миграция существующих ресурсов

Существующие ресурсы с `reason=InvalidSpec` или `reason=InternalError`:
- Могут быть автоматически мигрированы в `reason=Failed` при следующем reconcile (если ресурс не terminal)
- Или оставлены как есть для обратной совместимости

**Рекомендация:** Оставить как есть для обратной совместимости. Новые ресурсы будут использовать новую семантику.

---

## История изменений

- **2025-12-19**: Создание ТЗ на упрощение семантики Ready condition

