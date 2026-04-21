# Coverage / dedup: data + domain resources

**Статус:** Proposed (расширено по ревью).  
**Ключи и схема поля:** нормативно для реализации — [`06-coverage-dedup-keys.md`](06-coverage-dedup-keys.md).

## Жёсткое разделение: ownerRef **≠** dedup

| Механизм | Назначение | Не используется для |
|----------|------------|---------------------|
| **ownerReference** | Иерархия объектов, **GC cascade**, модель **deletion** | Ответа на «ресурс уже покрыт subtree?» / «нельзя второй раз в root MCP» |
| **Coverage / exclude set** (или эквивалентный **data-driven** сигнал на root) | **Dedup**: что уже захвачено доменом или более специфичным leaf | Замены ownerRef |

Dedup требует **явного контракта** (поле статуса, аннотация, или общий механизм без `if demo` в reconcile — только чтение списка исключений).

---

## Два уровня dedup

### 1. Data dedup

**Цель:** не сделать **два snapshot данных** для одного и того же **PVC** / storage backend.

Примеры:

- VM subtree уже создал **VolumeSnapshot** для PVC через **DemoVirtualDiskSnapshot**;
- generic root path **не** создаёт второй **VolumeSnapshot** на тот же PVC и не дублирует data-path.

Проверки: существующий VS с лейблами принадлежности к **root snapshot UID** / disk snapshot; идемпотентность VCR.

### 2. Resource dedup

**Цель:** не включить **один и тот же доменный объект** дважды в разных ветках или в generic root MCP.

Примеры:

- **DemoVirtualDisk** входит в **DemoVirtualMachineSnapshot** и одновременно виден как отдельный ресурс в namespace;
- не создаются два несогласованных пути snapshot для одного диска; в **aggregated** нет дублирующего представления диска (манифест / ссылка — по согласованной политике).

Расширение формулировки ревью:

> **Generic path must not re-capture any resource already covered by a more specific domain subtree.** This applies at least to **PVC-backed data resources** and to **domain objects** such as **VirtualDisk** (и при необходимости другие GVK: например сетевые объекты VM — по продукту).

---

## Как передаётся «уже покрыто»

Черновик (как в предыдущей версии, но **не только PVC**):

| Сигнал | Кто пишет | Кто читает |
|--------|-----------|------------|
| Множество идентификаторов **PVC** (uid / nn) | Disk / VM demo reconciler → поле на **root `NamespaceSnapshot`** | Root MCR target planner (**generic**) |
| Множество идентификаторов **доменных объектов** (например `DemoVirtualDisk` uid / nn) | Demo reconciler | Root manifest planner / агрегатор (чтобы не дублировать объект в root MCP, если он уже в domain MCP) |

Имена полей (`status.domainCoverage`, разбиение на `dataCovered` / `resourcesCovered` — **на ревью**).

---

## Synthetic tree

Не основной путь проверки; после demo-тестов scaffold можно мигрировать / удалить отдельным PR.

## Нерешённое на ревью

- Схема поля coverage на CRD root без ломки v1alpha1 consumers.
- Поведение **aggregated** PR4 при исключённых из root MCP объектах (объект только в domain MCP — ожидаемо для оператора).
