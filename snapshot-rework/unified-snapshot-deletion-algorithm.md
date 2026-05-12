# Алгоритм каскадного удаления дерева snapshot'ов

## Критические инварианты

### Управление жизненным циклом `XxxxSnapshotContent` осуществляет `ObjectKeeper` через механизм "корзины" с TTL.

**Связь между Snapshot и Content:**
- Логическая связь: `XxxxSnapshotContent.Spec.SnapshotRef` → `XxxxSnapshot`
- Защита: финалайзер `snapshot.deckhouse.io/parent-protect` на `XxxxSnapshotContent`
- Удаление: `ObjectKeeper` удаляет `XxxxSnapshotContent` после истечения TTL

### Всё, что должно удаляться каскадом "по дереву" — должно иметь ownerRef

**Структура ownerRef в дереве snapshot'ов:**
- `YyyySnapshot.ownerRef` → `XxxxSnapshot` (если дочерние snapshot'ы создаются с ownerRef)
- `YyyySnapshotContent.ownerRef` → `XxxxSnapshotContent`
- Артефакты (`ManifestCheckpoint`, `VolumeSnapshotContent`).`ownerRef` → соответствующий `*SnapshotContent`
- **Root retained content** (`XxxxSnapshotContent`, `NamespaceSnapshotContent`): **`ownerRef` → root `ObjectKeeper`** (TTL-якорь); OK следует за root snapshot (`FollowObjectWithTTL`), **без** `ObjectKeeper.ownerReferences` на content — см. design [`namespace-snapshot-controller.md`](../docs/state-snapshotter-rework/design/namespace-snapshot-controller.md) §4.3 для namespace-flow.

### Финалайзеры должны сниматься только контроллерами

Kubernetes GC ждёт, пока финалайзеры будут сняты. Поэтому все шаги "готов к удалению (финалайзеры сняты)" критичны для корректной работы алгоритма.

**Финалайзеры:**
- `snapshot.deckhouse.io/parent-protect` — снимается общим контроллером
- Финалайзеры на артефактах (если есть) — снимаются соответствующими контроллерами

### Conditions: Ready и InProgress

Snapshot и SnapshotContent используют два condition'а:

- `InProgress` — объект находится в процессе первичного создания
- `Ready` — объект пригоден к использованию

#### Правила:

- `InProgress=True` выставляется только на этапе первичного создания объекта и означает "создание/сборка ещё не завершены"
- `InProgress=False` выставляется один раз при достижении терминального состояния (`Ready=True` или `Ready=False`)
- `InProgress` используется только для фазы создания и не отражает процесс удаления; факт удаления определяется по `deletionTimestamp`
- `Ready=True` выставляется только при успешном завершении создания
- `Ready=False` выставляется только для ранее успешных (`Ready=True`) объектов при потере структурных элементов (content, артефакты, дочерние Snapshot'ы)
- `Ready=False` является терминальным состоянием — snapshot не может быть автоматически восстановлен в валидное состояние

#### Условия для Ready:

**Snapshot считается `Ready=True` только если:**
- Существует `XxxxSnapshotContent`, указанный в `status.contentRef`
- `XxxxSnapshotContent.Ready=True`
- `XxxxSnapshotContent.deletionTimestamp == nil`
- Все дочерние Snapshot'ы существуют

**SnapshotContent считается `Ready=True` только если:**
- Все обязательные артефакты существуют

**Важно:** `status.childrenSnapshotContentRefs[]` используются исключительно для каскадного снятия финалайзеров при удалении и не участвуют в вычислении `Ready` для SnapshotContent.

**Дочерние Snapshot'ы:**
Дочерними Snapshot'ами считаются все Snapshot'ы, которые являются дочерними для данного Snapshot в дереве snapshot'ов (например, через `ownerRef` или явные ссылки в статусе). Все дочерние Snapshot'ы равноправны. Любой дочерний Snapshot является обязательной частью структуры Snapshot.

**Примечание:** ADR не фиксирует конкретный механизм хранения списка дочерних Snapshot'ов (`ownerRef`, status-поле и т.п.) — это деталь реализации, но контракт требует, чтобы этот список был однозначно определим.

**Единое дерево:**
С точки зрения консистентности и переходов `Ready` существует одно логическое дерево snapshot'ов. Snapshot и SnapshotContent являются разными API-узлами, но нарушение структуры дерева означает невозможность состояния `Ready=True` для Snapshot. Все правила консистентности, удаления и переходов `Ready` применяются одинаково и определяются исключительно положением узла в дереве, а не типом Kubernetes-объекта.

#### Инвариант Ready по поддереву:

Snapshot может находиться в состоянии `Ready=True` только пока все элементы его поддерева существуют и валидны. Потеря любого элемента поддерева переводит Snapshot в `Ready=False` (если он ранее был `Ready=True`).

**Примечание:** Поддерево Snapshot включает сам Snapshot, его SnapshotContent, все дочерние Snapshot'ы и их поддеревья.

#### Причины для `Ready=False`:

**Для Snapshot:**
- `ContentMissing` — content не найден (только для ранее успешных объектов)
- `ChildSnapshotMissing` — один из дочерних Snapshot был удалён при существующем родителе (только для ранее успешных объектов и только если Snapshot не находится в процессе удаления)

**Для SnapshotContent:**
- `ArtifactMissing` — отсутствуют обязательные артефакты (MCP/VSC) (только для ранее успешных объектов)

**Важно:** `Ready=False` выставляется только если объект ранее был `Ready=True` (см. "Итоговая модель" для полного описания терминальности).

**Итоговая модель:**
Snapshot и SnapshotContent используют два condition'а: `InProgress` и `Ready`. `InProgress=True` выставляется только на этапе первичного создания объекта и сбрасывается при достижении терминального состояния. `InProgress` не используется для отражения процесса удаления. `Ready=True` означает, что объект полностью создан и пригоден к использованию. `Ready=False` является терминальным состоянием и выставляется только для ранее успешных объектов при потере элементов поддерева (см. "Инвариант Ready по поддереву"). Объекты не могут перейти из `Ready=False` обратно в `Ready=True`; для восстановления требуется создание нового Snapshot. Snapshot проверяет связанный SnapshotContent и дочерние Snapshot'ы. SnapshotContent проверяет только наличие обязательных артефактов и не использует children для вычисления `Ready`.

---

## Правило GC

Во всех шагах ниже **"GC инициирует удаление"** означает:
1. Установка `DeletionTimestamp` на объект
2. Ожидание снятия всех финалайзеров контроллерами
3. Физическое удаление объекта из API

GC не снимает финалайзеры — он только ждёт, пока контроллеры их снимут.

---

## Структура дерева для примера

```
XxxxSnapshot (parent-1)
  ├─ XxxxSnapshotContent (parent-sc-1)
  │   ├─ ManifestCheckpoint (mcp-1) ← артефакт
  │   ├─ VolumeSnapshotContent (vsc-1) ← артефакт
  │   ├─ YyyySnapshotContent (child-sc-1) ← дочерний
  │   │   ├─ ManifestCheckpoint (mcp-2) ← артефакт
  │   │   └─ VolumeSnapshotContent (vsc-2) ← артефакт
  │   └─ ObjectKeeper (keeper-parent-1) ← для корневого
  ├─ ManifestCaptureRequest (mcr-1) ← временный request
  └─ VolumeCaptureRequest (vcr-1) ← временный request
```

**Связи ownerRef:**
- `YyyySnapshot.ownerRef` → `XxxxSnapshot/parent-1`
- `YyyySnapshotContent.ownerRef` → `XxxxSnapshotContent/parent-sc-1`
- `ManifestCheckpoint/mcp-1.ownerRef` → `XxxxSnapshotContent/parent-sc-1`
- `VolumeSnapshotContent/vsc-1.ownerRef` → `XxxxSnapshotContent/parent-sc-1`
- `ManifestCheckpoint/mcp-2.ownerRef` → `YyyySnapshotContent/child-sc-1`
- `VolumeSnapshotContent/vsc-2.ownerRef` → `YyyySnapshotContent/child-sc-1`
- `XxxxSnapshotContent/parent-sc-1.ownerRef` → `ObjectKeeper/keeper-parent-1` (**controller**; якорь TTL; после удаления OK по TTL GC снимает content)

**Важно:** `XxxxSnapshotContent` НЕ имеет `ownerRef` на `XxxxSnapshot` (логическая связь — `spec.snapshotRef` / аналог).

---

## Алгоритм удаления

### 1. User deletes Snapshot

**Действия:**
1. Kubernetes API устанавливает `DeletionTimestamp` на `XxxxSnapshot`
2. GC инициирует удаление дочерних `YyyySnapshot` через `ownerRef`

---

### 2. Content becomes orphaned

**Триггер:** изменение `XxxxSnapshotContent` или периодический reconcile

**Действия:**
1. Общий контроллер проверяет существование связанного `XxxxSnapshot` по `spec.snapshotRef`
2. Если `XxxxSnapshot` не найден (`IsNotFound`):
   - Снимает финалайзер `snapshot.deckhouse.io/parent-protect` с `XxxxSnapshotContent`
   - `XxxxSnapshotContent` теряет логическую связь с Snapshot и ожидает TTL

3. **ObjectKeeper:**
   - `ObjectKeeper` в режиме `FollowObjectWithTTL` обнаруживает удаление `XxxxSnapshot`
   - Фиксирует факт удаления и запускает отсчёт TTL для `XxxxSnapshotContent`
   - По истечении TTL инициирует удаление `XxxxSnapshotContent`

**Контракт ObjectKeeper (root retained):**
- **`XxxxSnapshotContent.ownerRef` → `ObjectKeeper`** (удаление content каскадом после удаления OK внешним контроллером по TTL)
- Режим `FollowObjectWithTTL` на root snapshot задаёт TTL-якорь; OK **не** должен ссылаться на content в `ownerReferences` в этой модели

---

### 3. GC deletes Content tree and keeper

**Триггер:** `XxxxSnapshotContent` получил `DeletionTimestamp`

**Действия:**
1. Общий контроллер обнаруживает `DeletionTimestamp != nil`
2. Каскадно снимает финалайзеры с дочерних `YyyySnapshotContent`:
   - Читает `status.childrenSnapshotContentRefs[]`
   - Для каждого дочернего снимает финалайзер `snapshot.deckhouse.io/parent-protect`
   - **Важно:** Контроллер должен обрабатывать случаи, когда часть ссылок битая, чтобы избежать deadlock

3. **Ключевой момент:** Контроллер НЕ инициирует `Delete(child-content)` — он только разблокирует GC, снимая финалайзеры. Удаление дочерних `YyyySnapshotContent` будет инициировано GC через `ownerRef`.

4. GC инициирует удаление дочерних `YyyySnapshotContent` через `ownerRef`

5. Контроллеры дочерних объектов снимают свои финалайзеры (если есть вложенность — рекурсивно)

6. GC инициирует удаление артефактов (MCP, VSC) через `ownerRef`

7. После удаления `ObjectKeeper` (Deckhouse controller по TTL) GC завершает удаление `XxxxSnapshotContent` как dependent

**Важно:** Порядок удаления критичен:
- Если `SnapshotContent` удаляется и имеет финалайзер, GC будет ждать
- Когда финалайзер снят → `SnapshotContent` реально удалится → и только тогда GC гарантированно пойдёт удалять dependents (артефакты, children, keeper)

---

### 4. Requests are cleaned independently

**Важно:** `MCR`/`VCR` удаляются независимо от удаления `SnapshotContent`. Они имеют собственный жизненный цикл и не участвуют в механизме "корзины".

**Варианты удаления:**

1. **Контроллером после Ready:**
   - Request-контроллер обнаруживает `Ready=True`
   - Контроллер инициирует удаление `MCR`/`VCR`

2. **Через TTL scanner:**
   - TTL scanner периодически проверяет терминальные `MCR`/`VCR` (`Ready=True` или `Ready=False`)
   - Проверяет TTL: `completionTimestamp + TTL < now`
   - Если TTL истёк → удаляет `MCR`/`VCR`

**Примечание:** Если `MCR`/`VCR` нужны для диагностики, TTL scanner предпочтительнее немедленного delete после `Ready` (или хотя бы configurable TTL).

---

## Проверка целостности и пропагация ошибок

### Проверка целостности для Snapshot

**В reconcile `XxxxSnapshot`:**

1. Найти `XxxxSnapshotContent` по `status.contentRef`
2. Если content не найден:
   - Если Snapshot ранее был `Ready=True` → `Ready=False` (`Reason=ContentMissing`)
   - Выход

3. Если content найден:
   - Проверить `content.Ready == True`
   - Если `content.Ready == False` и Snapshot ранее был `Ready=True` → `Ready=False` с соответствующим `Reason`

4. Проверить, что все дочерние Snapshot'ы существуют.
   - Если один из дочерних Snapshot'ов отсутствует и данный Snapshot ранее был `Ready=True` и Snapshot не находится в процессе удаления (`deletionTimestamp == nil`), выставить `Ready=False` с `Reason=ChildSnapshotMissing`

**Важно:** Snapshot проверяет свой content и дочерние Snapshot'ы. Артефакты проверяет SnapshotContent.

### Проверка целостности для SnapshotContent

**В reconcile `*SnapshotContent`:**

1. Проверить обязательные артефакты (MCP/VSC), если они обязательны для данного типа snapshot
2. Если артефакт отсутствует и SnapshotContent ранее был `Ready=True` → `Ready=False` (`Reason=ArtifactMissing`)

**Важно:** `Ready=False` выставляется только для ранее успешных объектов. `status.childrenSnapshotContentRefs[]` используются исключительно для каскадного снятия финалайзеров при удалении и не участвуют в вычислении `Ready`.

### Нарушение структуры дерева Snapshot'ов

Если Snapshot был ранее успешно создан (`Ready=True`) и один из дочерних Snapshot'ов был удалён при существующем родителе, такой Snapshot считается структурно неконсистентным и должен быть переведён в `Ready=False` с `Reason=ChildSnapshotMissing`.

Это правило применяется рекурсивно ко всем предкам удалённого Snapshot.

**Важно:** Snapshot не переводится в `Ready=False`, если дочерний Snapshot удаляется в рамках каскадного удаления родительского Snapshot (родитель имеет `deletionTimestamp != nil`). Это предотвращает ложные фейлы и race conditions при GC.

### Пропагация ошибок

Каждый объект выставляет `Ready` на основании локальной проверки:
- `Snapshot` проверяет свой `SnapshotContent` и дочерние Snapshot'ы
- `SnapshotContent` проверяет только свои обязательные артефакты

**Алгоритм при удалении Snapshot:**
- Если у Snapshot есть родитель, и родитель существует и был `Ready=True`, родитель переводится в `Ready=False` с `Reason=ChildSnapshotMissing`
- Операция повторяется рекурсивно вверх по дереву
- Потомки удалённого Snapshot не затрагиваются

**Рекурсивный переход вверх останавливается:**
- При отсутствии родителя
- При `deletionTimestamp != nil` у родителя (каскадное удаление)
- При `Ready=False` у родителя (уже помечен как broken)

**Система НЕ:**
- Восстанавливает `Ready=True` автоматически
- Распространяет `Ready=False` вниз по дереву (только вверх к предкам)
- Фейлит SnapshotContent из-за удаления Snapshot
- Переводит SnapshotContent в `Ready=False` из-за удаления Snapshot

### Условия "дыры" (что считать сломанным)

**Для SnapshotContent:**
- Отсутствует MCP при том, что snapshot типа "manifest-based" (только для ранее успешных объектов)
- Отсутствует VSC при том, что snapshot типа "volume-based" (только для ранее успешных объектов)

**Для Snapshot:**
- `status.contentRef` указывает на несуществующий объект (только для ранее успешных объектов)
- `XxxxSnapshotContent.Ready == False` (только для ранее успешных объектов)
- Один из дочерних Snapshot'ов отсутствует (только для ранее успешных объектов и только если Snapshot не находится в процессе удаления)

**Важно:** Snapshot не должен оставаться `Ready=True`, если хотя бы один обязательный элемент дерева исчез.

### Рекомендации по реализации Conditions

Чтобы это работало предсказуемо и без циклических reconcile:
- **Всегда ставь `observedGeneration`** — обязательно для корректной работы
- **Не обновляй status, если conditions не изменились** (deep-equal) — избегает лишних API-вызовов
- **Если `Ready=False` уже установлен — не перетирать** — это терминальное состояние (новая причина пишется только если Condition отсутствует)
- **`Ready=False` выставляется только для ранее успешных объектов** — это предотвращает ложные терминальные состояния во время создания
- **SnapshotContent не использует children для Ready** — `childrenSnapshotContentRefs[]` используются только для каскадного снятия финалайзеров при удалении
- **Snapshot учитывает дочерние Snapshot'ы** — при удалении дочернего Snapshot родитель переводится в `Ready=False` (рекурсивно вверх), но только если родитель не находится в процессе удаления

---

## Итоговая последовательность удаления

```
1. User deletes Snapshot
   ↓ ownerRef
2. GC deletes dependent Snapshots
   ↓ reconcile
3. Content becomes orphaned
   ↓ TTL
4. ObjectKeeper deletes Content
   ↓ reconcile + ownerRef
5. GC deletes Content tree and keeper
   ↓
6. Requests are cleaned independently
```

---

## Ключевые моменты

1. **Финалайзеры снимаются каскадно:** сначала с дочерних, потом удаляется родитель
2. **OwnerRef обеспечивает каскадное удаление:** артефакты → дочерние → родитель → ObjectKeeper
3. **MCR/VCR удаляются независимо:** контроллером после Ready или через TTL scanner
4. **Порядок критичен:** финалайзеры снимаются до удаления, чтобы не заблокировать GC
5. **GC не снимает финалайзеры:** он только ждёт, пока контроллеры их снимут
6. **Нет ownerRef между Snapshot и Content:** это позволяет реализовать механизм "корзины" с TTL
7. **Два condition'а: InProgress и Ready:** InProgress используется только для фазы создания, Ready — на пригодность к использованию
8. **Ready=False терминален и выставляется только для ранее успешных объектов:** это предотвращает ложные терминальные состояния во время создания
9. **Snapshot проверяет content и дочерние Snapshot'ы:** при удалении дочернего Snapshot родитель переводится в `Ready=False` рекурсивно вверх (но не вниз)
10. **SnapshotContent не использует children для Ready:** `childrenSnapshotContentRefs[]` используются только для каскадного снятия финалайзеров при удалении

Этот алгоритм обеспечивает корректное удаление всего дерева snapshot'ов без "висячих" ресурсов, deadlock'ов и "внешне валидных, но битых" snapshot'ов.