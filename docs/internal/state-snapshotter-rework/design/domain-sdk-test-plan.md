# SDK v1 (capture-only) — план тестирования и execution

Тест/execution-план для реализации SDK v1 по `design/domain-sdk-plan.md`. Этот документ **не меняет**
дизайн-план; он ссылается на его решения (Р-номера, §0/§5/§8) и описывает порядок проверок и инвариант
поставки. Контракт и формулировки API — в `domain-sdk-plan.md` (SSOT); здесь их не дублируем.

## 0. Инварианты поставки (MUST)

- **Один итоговый коммит/PR.** Реализация попадает в репозиторий **одним целостным результатом**: standalone
  SDK + миграция доменных контроллеров на SDK + зелёный полный test gate. Без промежуточных коммитов.
- **Промежуточные тестовые checkpoints — да.** Внутри работы тесты ниже запускаются локально сколько угодно
  раз (checkpoints 1–7); в репозиторий уходит один коммит, но проходить чекпойнты обязательно.
- **SDK standalone (MUST).** `pkg/snapshotsdk` не зависит от доменных контроллеров: не импортирует их пакеты,
  и в коде SDK нет упоминаний конкретных доменных контроллеров/их API-типов.
- **Доменные контроллеры → на SDK (MUST).** Используют только публичный root `pkg/snapshotsdk` и
  `pkg/snapshotsdk/transform`; прямые patch/condition/ownerRef-хелперы capture-протокола из reconcile уходят.

> Терминология синхронизирована с дизайн-планом: `MarkNotReady`/`NotReadySpec` (Р30), delete-free children
> (Р23; orphan-diff Р29 в v1 не применяется), `ChildSpec{Object}` child-builder seam (Р17.5).

## 1. Baseline (до любых изменений) — no-op refactor reference

Зафиксировать исходное зелёное состояние доменных контроллеров **до** правок:

- прогнать существующие unit/envtest/e2e доменных контроллеров;
- сохранить **список команд и их результат** (вывод, кол-во тестов, pass/fail) как baseline;
- этот baseline — эталон «поведение не изменилось» (S1 — no-op refactor, §8 дизайн-плана).

Команды (envtest — см. `.cursor/rules/controller-envtest-local.mdc` + `testing/e2e-testing-strategy.md`):

```bash
cd images/domain-controller && go test ./...
cd images/domain-controller && go test -tags integration ./test/integration/... -count=1   # если применимо
```

## 2. Standalone-сборка SDK (после создания `pkg/snapshotsdk`)

- собрать и протестировать SDK изолированно:

```bash
cd pkg/snapshotsdk && go test ./...
```

- проверить, что SDK **не импортирует** пакеты доменных контроллеров;
- grep-инвариант: в коде SDK нет упоминаний конкретных доменных API/типов/пакетов (demo и т.п.):

```bash
# 0 совпадений ожидается (исключая _test при необходимости):
rg -n "images/domain-controller" pkg/snapshotsdk
rg -n "api/demo|DemoVirtual|demov1alpha1|controllers/demo" pkg/snapshotsdk
# импорт-граф SDK (только разрешённые зависимости + api):
cd pkg/snapshotsdk && go list -deps ./... | rg "state-snapshotter" 
```

## 3. SDK unit-тесты (internal + публичные value-объекты)

- `internal/conditions`: condition merge **не теряет чужие conditions**;
- `internal/patch`: D4a/optimistic-lock patch **не затирает чужие status-поля**;
- `internal/children`: sort/dedup `SnapshotChildRef`;
- `ChildSpec.Object` deep-copy — **caller-объект не мутируется** (Р17.5);
- create-or-adopt child object;
- ownerRef выставляется **SDK** (parent ownerRef на child);
- `SnapshotChildRef` **деривируется из** child object (single source of truth, Р17.5);
- `EnsureChildren([A,C])` после `[A,B]` → refs `[A,C]`, `B` **detached** (выпал из refs), но **не удалён**
  (delete-free, Р23);
- `EnsureChildren(nil)` публикует пустой список refs; прежние SDK-owned children **не удаляются**
  (delete-free, Р23; empty desired = «детей нет»);
- delete-free: SDK **никогда** не удаляет child objects (ни свои, ни чужие) — реклейм выбывших через ownerRef
  GC (Р23).

## 4. SDK facade — conformance / restart-safe tests

- повтор `EnsureChildren` не создаёт дублей;
- повтор `EnsureVolumeCapture` не создаёт дублей VCR;
- повтор `EnsureManifestCapture` не создаёт дублей MCR;
- partial progress после restart сходится из **durable state** (Р19/Р25);
- desired changed before `MarkPlanningReady`: `[A,B]` → restart → `[A,C]` сходится к `[A,C]` (Р25);
- `MarkPlanningReady` **не** создаёт MCR/VCR и **не** меняет child refs (barrier, не planning);
- `MarkPlanningFailed` **не** rollback-ает уже published legs;
- `MarkNotReady` публикует `Ready=False` с корректным reason (Р30): source invalid → `Ready=False`
  (no requeue); artifact-missing → `Ready=False` + `Requeue=true` intent; (planning failure — отдельный
  барьер `PlanningReady=False` через `MarkPlanningFailed`);
- SDK facade **не читает и не требует** `SnapshotContent` (content-free invariant).

## 5. После миграции доменных контроллеров на SDK

- прогнать unit/envtest доменных контроллеров (см. §1) — **сравнить с baseline**, ожидания не меняются;
- прямые patch/condition/ownerRef-хелперы capture-протокола из reconcile-кода **удалены**;
- reconcile-код **не** использует raw condition-strings / `metav1.Condition` напрямую для capture-протокола;
- source validation **осталась в домене** (свой `SourceValidator`, Р28);
- child objects **строятся доменом** и передаются как `ChildSpec.Object` (Р17.5).

```bash
# в reconcile demo не должно остаться прямой capture-механики:
rg -n "MergeFromWithOptimisticLock|RetryOnConflict|meta.SetStatusCondition|ConditionPlanningReady" \
  images/domain-controller/internal/controllers/demo
```

## 6. Финальная проверка зависимостей (standalone invariant)

- SDK импортирует только разрешённые общие зависимости и `api`; **не** импортирует доменные контроллеры;
- доменные контроллеры импортируют SDK (`pkg/snapshotsdk`, `pkg/snapshotsdk/transform`);
- публичных подпакетов `children`/`status`/`patch`/`conditions`/`ownerref`/`volumecapture` **нет** (Р26);
- публичны только root `pkg/snapshotsdk` и `pkg/snapshotsdk/transform`.

```bash
cd pkg/snapshotsdk && go list ./...        # ожидается: root + transform (+ internal/*, непубличные)
rg -n "snapshotsdk/(children|status|patch|conditions|ownerref|volumecapture)\b" images   # 0 совпадений
rg -n "pkg/snapshotsdk" images/domain-controller   # домен импортирует SDK
```

## 7. Финальный regression gate (перед коммитом)

- `go test ./...` по `pkg/snapshotsdk` — зелёный;
- `go test ./...` по доменным контроллерам — зелёный, ожидания == baseline (§1);
- существующие envtest/e2e — **без изменения ожиданий**;
- grep-проверки standalone-инварианта (§2/§6) — чисто;
- старый `images/domain-controller/pkg/domainsdk` **удалён** (Р14, без shim);
- все exported symbols SDK имеют godoc (Р18).

```bash
cd pkg/snapshotsdk && go test ./...
cd images/domain-controller && go test ./...
rg -n "pkg/domainsdk" images || echo "domainsdk removed"
```

## Итог

Промежуточные тесты (checkpoints 1–7) запускаются локально сколько угодно. В репозиторий попадает **один
целостный результат**: standalone SDK + миграция доменных контроллеров на SDK + зелёный полный test gate.
