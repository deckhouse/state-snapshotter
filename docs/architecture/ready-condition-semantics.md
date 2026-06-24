# ТЗ: Изменение semantics Condition Ready в state-snapshotter

## Контекст / Проблема

В текущей реализации state-snapshotter condition:

```yaml
type: Ready
status: False
```

интерпретируется пользователями как **ошибка** или **неуспешное состояние**.

Фактически же после последних архитектурных изменений `Ready=False` используется в нескольких разных смыслах:
- операция ещё не началась
- операция в процессе выполнения
- операция завершилась с ошибкой

Это создаёт неоднозначность:
- пользователь не понимает, жив ли контроллер
- невозможно отличить «всё работает, но ещё обрабатывается» от «что-то сломалось»
- UX и CLI/GUI интерпретация состояния становятся неверными

### Пример проблемы

```bash
$ kubectl get mcr my-request
NAME           READY   STATUS
my-request     False   # Что это значит? Ошибка? Обработка? Не началось?
```

Пользователь видит `Ready=False` и думает, что что-то сломалось, хотя на самом деле операция просто выполняется.

---

## Цель изменений

Сделать состояние ресурса **однозначно интерпретируемым** пользователем, так чтобы:

1. Было явно видно, что операция началась
2. Было понятно, что контроллер работает и ресурс обрабатывается
3. `Ready=False` перестал автоматически означать фатальную ошибку

---

## Предлагаемое решение

### 1. Расширить semantics Ready через reason

Добавить новое значение `reason=Processing` для condition `Ready`.

**Новая интерпретация:**

| `Ready.status` | `Ready.reason` | Значение для пользователя |
|----------------|----------------|---------------------------|
| `False` | `Processing` | Операция началась, выполняется |
| `True` | `Completed` | Операция успешно завершена |
| `False` | `Failed` | Операция завершилась с ошибкой |
| `False` | `InvalidSpec` | Некорректный ресурс (валидация не прошла) |
| `False` | `InternalError` | Внутренняя ошибка контроллера |

### 2. Обновлённый контракт Condition Ready

#### Allowed values

```yaml
conditions:
- type: Ready
  status: True | False
  reason: Processing | Completed | Failed | InvalidSpec | InternalError
  message: string
  observedGeneration: int
```

**❗️ Важно:** `status=False` больше не является синонимом ошибки

#### Семантика reason

- **`Processing`**: Операция началась и выполняется. Контроллер активно обрабатывает ресурс.
- **`Completed`**: Операция успешно завершена. Ресурс в финальном успешном состоянии.
  - **Контракт:** `Completed` является единственным допустимым `reason` для `Ready=True`.
  - `Ready=True` с любым другим `reason` запрещено.
- **`Failed`**: Операция завершилась с ошибкой. Ресурс в финальном неуспешном состоянии.
- **`InvalidSpec`**: Спецификация ресурса некорректна. Операция не может быть начата.
- **`InternalError`**: Внутренняя ошибка контроллера (устойчивое имя `reason` для сбоев внутри reconcile).

---

## Поведение контроллера

### 3.1 Начало reconcile / старт операции

При первом переходе ресурса в обработку контроллер **обязан** установить:

```yaml
type: Ready
status: False
reason: Processing
message: "Operation started"
```

**Это состояние:**
- должно выставляться **один раз** (при первом reconcile после создания ресурса)
- должно сохраняться на протяжении всей активной обработки
- не должно перезаписываться на каждом reconcile

**❗️ Критически важно:** Контроллер **обязан** опубликовать состояние `Processing` в status **до выполнения длительных операций** (создание ресурсов, ожидание внешних API, и т.д.). Это UX-критично для пользователя, чтобы видеть, что операция началась.

**Логика установки:**

```go
// Проверка: если Ready condition отсутствует или имеет reason != Processing
readyCondition := meta.FindStatusCondition(mcr.Status.Conditions, ConditionTypeReady)
if readyCondition == nil || readyCondition.Reason != ConditionReasonProcessing {
    // Установить Processing
    setSingleCondition(&mcr.Status.Conditions, metav1.Condition{
        Type:               ConditionTypeReady,
        Status:             metav1.ConditionFalse,
        Reason:             ConditionReasonProcessing,
        Message:            "Operation started",
        LastTransitionTime: metav1.Now(),
    })
}
```

### 3.2 Успешное завершение

При успешном завершении операции:

```yaml
type: Ready
status: True
reason: Completed
message: "Operation completed successfully"
```

**Переход:** `Processing` → `Completed`

### 3.3 Ошибки

#### Фатальные ошибки (операция не может быть продолжена)

```yaml
type: Ready
status: False
reason: Failed
message: "<human-readable error>"
```

**Переход:** `Processing` → `Failed`

#### Ошибки спецификации / валидации

```yaml
type: Ready
status: False
reason: InvalidSpec
message: "<what is wrong>"
```

**Переход:** `*` → `InvalidSpec` (ранний выход, до установки Processing)

---

## Требования к логике reconcile

### 4.1 Запреты

1. **Запрещено перезаписывать `reason=Processing` на каждом reconcile**
   - `Processing` устанавливается только при переходе в состояние обработки
   - Последующие reconcile не должны менять `reason=Processing`, если операция всё ещё выполняется

2. **Контроллер не должен:**
   - трактовать `Ready=False` как автоматический failure
   - завершать reconcile с ошибкой только из-за `Ready=False`
   - устанавливать `Ready=False` без явного `reason`

### 4.2 Детерминированные переходы

Все переходы должны быть явными и детерминированными:

```
[новый ресурс] → Processing → Completed
                              ↘ Failed
[новый ресурс] → InvalidSpec (ранний выход)
```

**Запрещены переходы:**
- `Completed` → `Processing` (ресурс уже завершён)
- `Failed` → `Processing` (ресурс уже завершён)
- `Processing` → `InvalidSpec` (валидация должна быть до Processing)

### 4.3 Проверка терминального состояния

**❗️ Важно:** Терминальность определяется **в первую очередь по reason, а не по status**.

Терминальные состояния (не требуют дальнейшей обработки):
- `Ready=True` (всегда с `reason=Completed`)
- `Ready=False` с `reason=Failed`
- `Ready=False` с `reason=InvalidSpec`
- `Ready=False` с `reason=InternalError`

**НЕ терминальное состояние:**
- `Ready=False` с `reason=Processing` (требует продолжения обработки)
- Отсутствие condition `Ready` (ресурс ещё не обрабатывался)

**Запрещено:**
- `Ready=True` с `reason != Completed` (нарушение контракта)
- `Ready=False` с `reason=Completed` (логическая ошибка)

---

## Обратная совместимость

### 5.1 Совместимость с существующим кодом

- Тип `Ready` не меняется
- Изменяется только semantics `reason`
- Старые клиенты, которые смотрят только на `status`, продолжат работать
- Новые клиенты получают корректный UX через `reason`

### 5.2 Миграция существующих ресурсов

Существующие ресурсы с `Ready=False` без `reason` или с `reason=Error`:
- Могут быть оставлены как есть (обратная совместимость)
- Или автоматически мигрированы при следующем reconcile (если ресурс не terminal)

**Рекомендация:** Оставить как есть для обратной совместимости. Новые ресурсы будут использовать новую семантику.

**❗️ Важно:** Новая семантика гарантируется только для ресурсов, созданных после внедрения изменений. Существующие ресурсы могут иметь старую семантику (`Ready=False` без `reason` или с `reason=InternalError`), и это нормально.

---

## Обновления, которые необходимо внести

### Обязательно

1. **API (`api/v1alpha1/conditions.go`):**
   - Добавить константу `ConditionReasonProcessing = "Processing"`
   - Добавить константу `ConditionReasonFailed = "Failed"`
   - Добавить константу `ConditionReasonInvalidSpec = "InvalidSpec"`
   - Обновить комментарии к константам

2. **Контроллер (`images/state-snapshotter-controller/internal/controllers/manifestcheckpoint_controller.go`):**
   - Обновить логику установки `Ready` condition в начале reconcile
   - Добавить проверку: если `Ready` отсутствует или `reason != Processing`, установить `Processing`
   - Обновить все места установки `Ready=False` для использования правильного `reason`
   - Обновить `isTerminal()` для учета `reason=Processing` (не terminal)

3. **Тесты:**
   - Обновить unit tests для проверки установки `Processing`
   - Добавить тесты на переходы состояний
   - Обновить integration tests

4. **Документация:**
   - Обновить CRD комментарии (если есть)
   - Обновить README с примерами состояний
   - Обновить `docs/architecture/controller-pattern.md`

### Желательно

1. **Примеры состояний в документации:**
   ```yaml
   # Операция выполняется
   conditions:
   - type: Ready
     status: "False"
     reason: Processing
     message: "Operation started"
   
   # Операция успешно завершена
   conditions:
   - type: Ready
     status: "True"
     reason: Completed
     message: "Checkpoint created successfully"
   
   # Операция завершилась с ошибкой
   conditions:
   - type: Ready
     status: "False"
     reason: Failed
     message: "Failed to create checkpoint: <error>"
   ```

2. **Проверка отображения:**
   - Проверить, как состояния отображаются в `kubectl get -o wide`
   - Проверить отображение в UI (если есть)

---

## Ожидаемый эффект

### До изменений

```bash
$ kubectl get mcr my-request
NAME           READY   STATUS
my-request     False   # Непонятно: ошибка? обработка? не началось?
```

### После изменений

```bash
$ kubectl get mcr my-request
NAME           READY   STATUS      REASON
my-request     False   Processing  Operation started
```

**Пользователь видит:**
- ✅ Операция началась (`reason=Processing`)
- ✅ Контроллер работает (`status=False` + `reason=Processing` = нормально)
- ✅ Чёткое разделение: processing / success / failure

### Преимущества

1. **Пользователь видит, что операция началась**
   - Нет ложного ощущения «всё сломалось»
   - Понятно, что контроллер работает

2. **Чёткое разделение состояний:**
   - `Processing` — операция выполняется
   - `Completed` — успешно завершено
   - `Failed` — завершилось с ошибкой

3. **Контроллер выглядит живым и предсказуемым:**
   - Пользователь понимает текущее состояние
   - Нет неоднозначности в интерпретации

---

## Реализация

### Шаг 1: Обновить API константы

```go
// api/v1alpha1/conditions.go

// Condition reason constants
const (
	// ConditionReasonProcessing indicates operation is in progress
	ConditionReasonProcessing = "Processing"
	
	// ConditionReasonProcessing indicates operation is in progress
	ConditionReasonProcessing = "Processing"
	
	// ConditionReasonCompleted indicates successful completion
	// This is the ONLY allowed reason for Ready=True
	ConditionReasonCompleted = "Completed"
	
	// ConditionReasonFailed indicates operation failed
	ConditionReasonFailed = "Failed"
	
	// ConditionReasonInvalidSpec indicates invalid resource specification
	ConditionReasonInvalidSpec = "InvalidSpec"
	
	// ConditionReasonInternalError indicates internal controller fault during reconcile
	ConditionReasonInternalError = "InternalError"
)
```

### Шаг 2: Обновить логику reconcile

```go
// В начале reconcile, после валидации, но до основной логики:

// Set Processing condition if not already set
// CRITICAL: This must be done BEFORE any long-running operations
readyCondition := meta.FindStatusCondition(mcr.Status.Conditions, ConditionTypeReady)
if readyCondition == nil || readyCondition.Reason != ConditionReasonProcessing {
    setSingleCondition(&mcr.Status.Conditions, metav1.Condition{
        Type:               ConditionTypeReady,
        Status:             metav1.ConditionFalse,
        Reason:             ConditionReasonProcessing,
        Message:            "Operation started",
        LastTransitionTime: metav1.Now(),
    })
    // Update status immediately to reflect Processing state
    // This is UX-critical: user must see that operation started
    if err := r.Status().Update(ctx, mcr); err != nil {
        return ctrl.Result{}, err
    }
}
```

### Шаг 3: Обновить isTerminal()

```go
func (r *ManifestCheckpointController) isTerminal(mcr *storagev1alpha1.ManifestCaptureRequest) bool {
	readyCondition := meta.FindStatusCondition(mcr.Status.Conditions, ConditionTypeReady)
	if readyCondition == nil {
		return false // No condition = not terminal
	}
	
	// Terminality is determined by reason, not just status
	// Processing is NOT terminal
	if readyCondition.Reason == ConditionReasonProcessing {
		return false
	}
	
	// Terminal states:
	// - True (always with Completed reason per contract)
	// - False with Failed/InvalidSpec/InternalError
	return readyCondition.Status == metav1.ConditionTrue || 
	       readyCondition.Reason == ConditionReasonFailed ||
	       readyCondition.Reason == ConditionReasonInvalidSpec ||
	       readyCondition.Reason == ConditionReasonInternalError
}
```

### Шаг 4: Обновить finalizeMCR()

```go
func (r *ManifestCheckpointController) finalizeMCR(
	ctx context.Context,
	mcr *storagev1alpha1.ManifestCaptureRequest,
	status metav1.ConditionStatus,
	reason, message string,
) error {
	// Validate reason matches status per contract
	if status == metav1.ConditionTrue {
		// Completed is the ONLY allowed reason for True
		if reason != ConditionReasonCompleted {
			reason = ConditionReasonCompleted // Force Completed for True
		}
	}
	if status == metav1.ConditionFalse {
		// Must have explicit reason for False
		if reason == "" || reason == ConditionReasonCompleted {
			reason = ConditionReasonFailed // Default to Failed for False
		}
	}
	
	// ... rest of finalizeMCR logic
}
```

---

## Тестирование

### Unit Tests

1. Проверка установки `Processing` при первом reconcile
2. Проверка, что `Processing` не перезаписывается на последующих reconcile
3. Проверка переходов: `Processing` → `Completed`, `Processing` → `Failed`
4. Проверка `isTerminal()` для всех состояний

### Integration Tests

1. Создание MCR → проверка установки `Processing`
2. Успешное завершение → проверка перехода в `Completed`
3. Ошибка → проверка перехода в `Failed`
4. Валидация ошибок → проверка установки `InvalidSpec`

---

## Связанные документы

- `docs/architecture/controller-pattern.md` — общий паттерн контроллеров
- `api/v1alpha1/conditions.go` — определение констант conditions

---

## История изменений

- **2025-12-18**: Создание ТЗ на изменение semantics Ready condition

