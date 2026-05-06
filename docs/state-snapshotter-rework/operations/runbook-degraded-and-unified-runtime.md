# Runbook: деградация при исчезновении CRD и unified runtime (D3)

Практическое руководство для эксплуатации **state-snapshotter-controller** в части optional unified CRD, REST discovery и **additive** unified watches (R2 2b / R3). Нормативные выдержки — [`spec/system-spec.md`](../spec/system-spec.md); ADR — [`snapshot-rework/`](../../../snapshot-rework/) (в т.ч. `2026-01-23-unified-snapshots-registry.md`).

---

## 1. Исчезновение или отсутствие CRD (unified snapshot types)

**Что происходит в процессе**

- На старте и при каждом пересчёте реестра пары GVK проходят через **RESTMapper** / discovery: если CRD (или storage version) для snapshot или snapshot content **нет в API**, эта пара **просто не попадает** в resolved-набор. Процесс **не падает** (S1–S2).
- Это **fail-open на уровне процесса**: контроллер остаётся живым; «пустой» или урезанный набор типов — **нормальный режим**, если в кластере выключены модули или CRD ещё не установлены.

**Деградация смысла (degraded)**

- Для типов, которых нет в API, unified-контроллер **не обрабатывает** соответствующие ресурсы (их просто нет в resolved).
- Если CRD **исчез после** того, как процесс уже поднял на них informer (watches), поведение определяется **additive-моделью** (см. §2–3): informer мог остаться активным до рестарта pod.

**Что делать оператору**

- Проверить, что ожидаемые CRD установлены и в статусе Established.
- После появления CRD новые типы обычно подхватываются **без рестарта** через CSD → `Sync` (если CSD watch-eligible). Если тип только в bootstrap и не описан в CSD — достаточно появления CRD в API и следующего цикла `Sync` после reconcile CSD.
- Если после удаления модуля/CRD нужна **гарантированно чистая** картина подписок — см. §3 (рестарт pod).

---

## 2. Метрики `state_snapshotter_unified_runtime_*`

Метрики регистрируются в registry **controller-runtime** (тот же endpoint, что и остальные метрики менеджера). Полные имена:

| Метрика | Смысл |
|---------|--------|
| `state_snapshotter_unified_runtime_resolved_snapshot_gvk_count` | Сколько **snapshot** GVK сейчас в **resolved** слое layered state (есть в API по mapper). |
| `state_snapshotter_unified_runtime_active_monotonic_snapshot_gvk_count` | Сколько ключей в **monotonic active**: для них **хотя бы раз** в жизни процесса успешно поднялись **оба** watch (Snapshot + SnapshotContent). Значение **не уменьшается**, когда тип выпал из resolved. |
| `state_snapshotter_unified_runtime_stale_active_snapshot_gvk_count` | Сколько ключей из monotonic active **нет** в текущем resolved. Это главный сигнал «хвоста» additive-модели. |

Обновление: после каждого успешного **`unifiedruntime.Syncer.Sync`** (вызов из CSD reconciler после успешного reconcile).

**Логи**

- **V(2):** краткая сводка счётчиков после каждого `Sync` (шумно — включать при отладке).
- **V(1):** отдельное сообщение, когда snapshot GVK **выпал из resolved** (переход между снимками).
- **Info:** если `stale_active_snapshot_gvk_count > 0` — структурированный лог со списком `staleSnapshotGVKKeys` и явной рекомендацией рассмотреть **рестарт pod** (см. §3).

Код: `images/state-snapshotter-controller/pkg/unifiedruntime/syncer.go`, `metrics.go`.

---

## 3. Что такое «stale» и когда перезапускать pod

**Stale** — это snapshot GVK, который:

- уже **однажды** успешно прошёл полный путь регистрации обоих watches в этом процессе (**active monotonic**), и  
- **сейчас** **не входит** в **resolved** (например, убрали CRD, изменили eligibility CSD, temporary discovery glitch — конкретика зависит от среды).

Из-за **additive** семантики **снятие watch без рестарта не гарантируется**: controller-runtime может продолжать держать старый informer. Тогда:

- **resolved** и реальные подписки **расходятся**;
- метрика **stale** > 0 и **Info**-лог дают явный операционный сигнал.

**Когда имеет смысл рестарт pod**

- **Stale > 0** после осознанного удаления CRD или смены набора типов, и нужна **строгая** согласованность «что в конфиге/CSD» ↔ «что реально смотрит процесс».
- Подозрение на застрявшие informer’ы или редкие гонки после больших изменений CRD.

**Когда рестарт часто не обязателен**

- Stale кратковременно 0 после восстановления CRD и повторного попадания типа в resolved (если ключ остаётся в monotonic active, но снова совпадает с resolved — stale обнулится).
- Только добавление новых типов без удаления старых — типичный happy path без рестарта.

---

## 4. Bootstrap (R5): always-on runtime

Unified/generic runtime в v0 имеет один режим: CSD reconciler, graph registry, `unifiedruntime.Syncer`, generic watches и hot-add путь всегда инициализируются. Переменные окружения контроллера (часть дублируется Helm values `stateSnapshotter.*`):

| Переменная | Смысл |
|------------|--------|
| `STATE_SNAPSHOTTER_UNIFIED_BOOTSTRAP_PAIRS` | Пусто — встроенный unified-runtime bootstrap (`DefaultUnifiedRuntimeBootstrapPairs()`, legacy alias `DefaultDesiredUnifiedSnapshotPairs()`). Это не graph registry built-ins. Литералы `empty` / `none` / `csd-only` — пустой статический bootstrap (только eligible CSD). Иначе кастом: пары через `;`, внутри пары `snapGVK|contentGVK`, каждый GVK как `group/version/Kind`. |
| `STATE_SNAPSHOTTER_SNAPSHOT_ROOT_OK_TTL` | Опционально: длительность `spec.ttl` у корневого ObjectKeeper в режиме **`FollowObjectWithTTL`** (root `Snapshot` и unified `XxxxSnapshot`). Формат Go `time.ParseDuration` (`24h`, `168h`, …). Не задана или ≤0 — используется встроенный дефолт контроллера (см. `pkg/config`). Алиас: `STATE_SNAPSHOTTER_NS_ROOT_OK_TTL` (если основная не задана). Value Helm: `stateSnapshotter.snapshotRootOkTtl`. |

Неверная строка bootstrap → в лог пишется предупреждение и используется **дефолтный** список.

---

## 5. Связанные документы

- Обзор линий продукта и ссылок: [`../README.md`](../README.md) (D1).
- CSD, RBAC, MCR: [`csd-rbac-and-mcr.md`](csd-rbac-and-mcr.md) (D2).
- Техдизайн runtime: [`../design/r2-phase-2b-r3-runtime-registry.md`](../design/r2-phase-2b-r3-runtime-registry.md).
