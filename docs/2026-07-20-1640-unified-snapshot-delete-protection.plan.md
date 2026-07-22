---
name: unified-snapshot-delete-protection
created: 2026-07-20
updated: 2026-07-22
overview: "API-уровневая защита unified-снапшот дерева от прямого удаления по МОДЕЛИ rev.2 (design/delete-protection-contract.md): вводится ОДИН канонический marker state-snapshotter.deckhouse.io/delete-protected: \"true\"; admission проверяет ТОЛЬКО его (+ exempt-акторы + break-glass), НЕ вычисляя принадлежность по ownerRef/finalizer/followObjectRef и НЕ зная про Kind'ы. Инцидент-первопричина: пользователь под super-admin удалил из UI дочерний CSI VolumeSnapshot, root Snapshot деградировал (Ready=False/ChildSnapshotDeleted), durable-слой цел, но in-cluster restore TBD → практически необратимо. Финализаторы НЕ являются механизмом запрета (объект виснет в Terminating; KEP-2839 Liens не реализован). Модель rev.2 (что изменилось относительно первой редакции плана): убрана дизъюнкция per-Kind предикатов (managed==true ИЛИ ownerRef; controller-ownerRef; followObjectRef-префикс; artifact-protect ИЛИ ownerRef) — заменена единым marker'ом, проставляемым на write-пути тем, кто вводит объект в дерево; спец-исключение root по Kind убрано (root marker НЕ получает: root — член дерева, но НЕ delete-protected, его DELETE = штатный teardown). Membership и delete-protection разведены: marker отвечает ТОЛЬКО за политику удаления, идентичность дерева держат логические рёбра. ЗАЩИТА ОТ ОБХОДА: admission защищает не только DELETE, но и UPDATE marker'а (нельзя снять/изменить delete-protected==true, кроме exempt-актора; единственный поддерживаемый обход — аннотация deckhouse.io/allow-delete, при которой marker ОСТАЁТСЯ, а DELETE разрешается из-за аннотации). Объём защиты (какие узлы получают marker): дочерние Snapshot/доменные снапшот-CR (demo.state-snapshotter.deckhouse.io и sds-unified-snapshots-poc.deckhouse.io), CSI VolumeSnapshot нашего дерева (marker при адопции managed=true, storage-foundation VS-домен), SnapshotContent (все наши), ManifestCheckpoint+ManifestCheckpointContentChunk (все наши), managed CSI VolumeSnapshotContent (marker в ИСХОДНОМ CREATE-payload у storage-foundation — НЕ post-create patch: между Create и patch нет ordering, объект мог бы кратко существовать в API без защиты; успешный Create — атомарная точка защищённого VSC. Нейтральная причина «объект появился в API → уже должен быть защищён», БЕЗ VCR/CSI-execution семантики), наш ObjectKeeper. Ordering invariant (P8): создаваемые нами узлы несут marker в CREATE-payload, adopted (CSI VS) — patch ДО публикации edge. NON-GOAL: план НЕ меняет контракт VCR, его retry/terminal-семантику, CSI operation ownership и восстановление утраченного VSC — это отдельный VCR-план (volume-capture-request-contract.md). Cross-repo (Вариант A): VS/VSC marker обязателен для полноты защиты и живёт в storage-foundation → в scope плана входит минимальная storage-foundation-часть (marker write-path на VS-адопции и VSC-создании) + необходимый bump версии SDK/констант + сборка/тесты + отдельный коммит; полный переезд прочих SDK-consumer'ов остаётся вне scope. Break-glass — аннотация deckhouse.io/allow-delete: \"true\" (литерал/полярность как у deckhouse-controller). Exempt-акторы (username, КРИТЕРИЙ — подтверждать инициатора каждой штатной delete-операции по audit/event/admission-request, а не только по ServiceAccount в Deployment): generic-garbage-collector, namespace-controller, d8-system:deckhouse (TTL ObjectKeeper), d8-state-snapshotter:controller, d8-storage-foundation:controller, verified d8-storage-foundation:snapshot-controller (VSC reclaim/lifecycle). system:masters НЕ exempt. Backfill (rollout-gate): ЕДИНЫЙ механизм — cluster-wide list-and-patch (List каждого защищаемого Kind → строгая legacy-классификация «наш объект» → patch marker → повторный полный проход), ИДЕМПОТЕНТЕН ПО КОНТРАКТУ (можно запускать неограниченное число раз — нужно для rollback/фикса классификатора). Gate доказуемый: «ВСЕ classifier-protected объекты имеют marker» (повторный проход не находит своего немаркированного), а НЕ недоказуемое «uncovered не существует вообще». Opportunistic reconcile-backfill допустим как ДОПОЛНЕНИЕ, но НЕ как gate. Rollout Audit→Deny: admission поставляется с Helm-value deleteGuard.enforcement=Audit|Deny (default Audit); переключение на Deny — пользовательское rollout-действие ПОСЛЕ достижения доказуемого gate; admission с ПЕРВОГО дня проверяет ТОЛЬКО marker (никакого derived-fallback по эвристикам в guard — до готовности backfill работает в Audit, а не читает старые сигналы). Enforcement-режимы контракта — Audit|Deny (Warn возможен как продуктовая rollout-политика, не архитектурный инвариант). Marker authority: steady-state — первую запись marker делает ТОЛЬКО authoritative write-path (P9); ЕДИНСТВЕННОЕ исключение — versioned migration backfill для legacy-объектов (не нарушение контракта); migration — legacy ownerRef/dataRef/followObjectRef/finalizer используются ТОЛЬКО для backfill-классификации, admission их НИКОГДА не читает. Терминология: delete-protected — authoritative protection state (authority/immutable/write-path-owned), а НЕ служебная метка; delete protection — часть snapshot protocol (защищаем корректность дерева, а не «важность» объекта — потому ObjectKeeper/VSC защищены, root нет). Marker lifecycle (P10): marker НЕ снимается в штатном жизненном цикле — легальный teardown/GC/reclaim удаляет объект как EXEMPT-актор при СОХРАНЁННОМ marker'е (пути «снять marker → удалить» нет; новый штатный удаляющий актор → в exempt, а не снятие marker). Break-glass в rev.3 — PERSISTENT/REVERSIBLE (НЕ одноразовый): allow-delete — сохраняемое явное разрешение, действует пока пользователь не снимет или объект не удалён; до начала DELETE полностью обратима. Одноразовость нереализуема без отдельного механизма (admission рассматривает DELETE, а не предварительный UPDATE; ни VAP, ни пользователь не снимут аннотацию «в той же операции»); токен/expiring-approval/delete-request CR — future hardening, вне scope. Формулировка для docs/операторов: «Persistent break-glass is accepted for rev.3. Operators should set it immediately before DELETE and remove it if DELETE is abandoned. Expiring or single-use authorization is a future hardening item.» Fail-fast по deletionTimestamp (сохранено): Terminating-объект обречён → наши CR self-report'ят Ready=False/Deleting при собственном Terminating, fold lost_children трактует deletionTimestamp!=nil на ребёнке как удалённого (Deleted при Ready-контенте / Lost иначе); каскад не деградирует ложно (гейт на живого owner'а). Семантика сознательного удаления НЕ меняется (ChildSnapshotDeleted/ChildSnapshotLost). ADR overview — правка в рабочем дереве БЕЗ коммита; spec — в том же коммите, что код. Prerequisite P1 — верификация уже сделанной DomainCaptureStatus-миграции (базовая линия SDK), НЕ workstream миграции. Деплой admission/редеплой и прогон e2e/переключение Audit→Deny на кластере — вне DoD, делает пользователь."
todos:
  - id: p1-verify-sdk-baseline
    content: "Prerequisite P1 (gate, НЕ workstream миграции) — верифицировать базовую линию SDK/namespace. DomainCaptureStatus и удаление старых lifecycle verbs (MarkPlanned/ConfirmConsistent/Fail/Reject/ReportProgress/FailSpec) УЖЕ реализованы и закоммичены (state-snapshotter@refactor/domain-capture-status, af28c95); namespace_capture_run.go на builder; старых verbs в дереве нет. Действия: (1) cd images/state-snapshotter-controller && go build ./... && go test ./pkg/... ./internal/...; (2) сверить, что публичный API SDK и README (pkg/snapshotsdk/README.md + README.ru.md) отражают единый builder — ТОЛЬКО сверка, НЕ переписывать и НЕ «доводить миграцию» (новых namespace-правок в scope НЕТ). Этот gate — фундамент: marker (P2) ложится на тот же write-путь, где домен пишет DomainCaptureStatus. Merge ветки в main и релиз версии SDK для внешних consumer'ов — отдельная задача переезда SDK, в этот план НЕ входит (кроме минимального bump для storage-foundation-части P2b — см. cross-repo)."
    status: pending
  - id: p2a-marker-writepath-core
    content: "P2a — проставление единого marker state-snapshotter.deckhouse.io/delete-protected: \"true\" на write-пути в CORE state-snapshotter (модель rev.2, design/delete-protection-contract.md §4,§6). Marker — НОВАЯ Go-константа (labels-конвенция репо); НЕ переиспользовать три-состоянийный …/managed. ПРАВИЛО ЗАПИСИ (create-vs-adopt): создаваемые нами узлы получают marker В ИСХОДНОМ CREATE-payload (metadata.labels), НЕ отдельным post-create patch (между Create и patch нет гарантированного ordering); patch допустим ТОЛЬКО для adoption существующего объекта и обязан завершиться ДО публикации graph edge/status. Ставит authoritative write-path (steady state) в момент введения узла: (a) дочерние Snapshot/доменные снапшот-CR — SDK EnsureChildren (children.Reconcile/ownerref.Ensure): marker в объекте ПЕРЕД Create; (b) SnapshotContent — GenericSnapshotBinderController: marker в объекте перед Create; (c) ManifestCheckpoint + чанки — ManifestCheckpointController: marker перед Create; (d) наш ObjectKeeper — snaphelpers: marker перед Create; (e) namespace-adopt дочерние — snaphelpers/parent_graph: ADOPT существующего → patch marker ДО публикации edge. Ordering: marker ДО публикации узла в tree edges/status (P8). Root Snapshot marker НЕ получает. Юнит/envtest: у создаваемых узлов marker присутствует УЖЕ В CREATE (перехват create / проверка объекта до Create), НЕ post-create; НЕ присутствует на root/чужих; для adopt — до edge; идемпотентность; повторный reconcile не дублирует/не снимает. gofmt; go build + go test ./pkg/... ./internal/... в images/state-snapshotter-controller."
    status: pending
  - id: p2b-marker-writepath-storagefoundation
    content: "P2b — CROSS-REPO (Вариант A, обязателен для полноты защиты) marker для CSI VS/VSC в storage-foundation. (f) managed CSI VolumeSnapshot: VS-домен reconciler ставит delete-protected в ветке managed=true ДО первой публикации узла (vetoed managed=false marker НЕ получает — §8.1/§8.5 контракта). (g) managed CSI VolumeSnapshotContent: storage-foundation ОБЯЗАН создавать управляемый VSC УЖЕ с delete-protected=true (в исходном CREATE-payload, metadata.labels). Причина простая, БЕЗ VCR-семантики: объект появился в Kubernetes API → он уже должен быть защищён; между Create и последующим PATCH нет гарантированного ordering, поэтому post-create patch оставил бы короткое незащищённое окно. Успешный Create — атомарная точка появления защищённого VSC, что атомарно устраняет окно между появлением VSC в API и применением delete-protection. Место записи — там, где storage-foundation создаёт VSC (volumecapturerequest_snapshot.go, VSC-only). ЯВНО: изменение НЕ меняет lifecycle, retry, terminal-семантику VCR, CSI operation ownership или восстановление утраченного VSC (см. Non-goals; отдельный VCR-план). Требует минимального bump версии SDK/констант (общая константа marker'а) — согласовать с задачей переезда SDK. Тест (ОБЯЗАТЕЛЬНО, на CREATE-объекте): перехват Create VSC (fake client reactor / envtest) → создаваемый объект УЖЕ содержит delete-protected=true в labels; прямой DELETE такого VSC запрещён. НЕ моделировать CSI execution window. gofmt; go build + relevant unit в storage-foundation; отдельный коммит в repo storage-foundation."
    status: pending
  - id: p3-marker-backfill
    content: "P3 — ЕДИНЫЙ backfill-механизм (rollout-gate для strict deny). Cluster-wide list-and-patch (job/one-shot controller): для КАЖДОГО защищаемого Kind — List → строгая legacy-классификация «наш объект» (ownerRef/finalizer/followObjectRef/dataRef — ТОЛЬКО здесь, как migration-классификатор; admission их НЕ читает) → patch marker немаркированным нашим узлам → ПОВТОРНЫЙ полный проход. ИДЕМПОТЕНТНОСТЬ — ЧАСТЬ КОНТРАКТА (не только реализации): допускается запускать неограниченное число раз (повторный запуск не трогает уже помеченные и непринадлежащие узлы) — обязательно, т.к. backfill перезапускают после rollback/частичного обновления/фикса классификатора. Обязан перечислить набор, включая узлы БЕЗ активного reconcile: старые MCP chunks, retained VSC без активного namespace snapshot, ObjectKeeper завершённых деревьев, durable SnapshotContent без событий, объекты, пережившие удаление namespace. Opportunistic reconcile-backfill допустим как ДОПОЛНЕНИЕ, но НЕ как gate. GATE ФОРМУЛИРУЕТСЯ ДОКАЗУЕМО (P6): доказываем «ВСЕ объекты, которые классификатор считает protected, ИМЕЮТ marker» (повторный проход не находит своего немаркированного), а НЕ недоказуемое «uncovered не существует вообще». Тесты классификатора: наш объект → marker; чужой/standalone → без marker; идемпотентность повторного прохода (второй проход = 0 патчей)."
    status: pending
  - id: p4-delete-guard-admission
    content: "P4 — admission delete-guard, проверяющий ТОЛЬКО marker (модель rev.2), с ДВУМЯ инвариантами. Новый файл templates (стиль {{ .Chart.Name }}, labels heritage/module). Механизм apiserver-enforced (VAP/CEL — деталь реализации; кластер v1.34). matchConstraints по защищаемым kinds (snapshot.storage.k8s.io volumesnapshots/volumesnapshotcontents; state-snapshotter.deckhouse.io snapshots/snapshotcontents/manifestcheckpoints/manifestcheckpointcontentchunks; demo.state-snapshotter.deckhouse.io + sds-unified-snapshots-poc.deckhouse.io demovirtualmachinesnapshots/demovirtualdisksnapshots; deckhouse.io objectkeepers). ИНВАРИАНТ 1 — DELETE: deny если label delete-protected==\"true\", КРОМЕ (1) exempt-актор (username в списке; system:masters НЕ входит); (2) break-glass annotation deckhouse.io/allow-delete==\"true\" (nil-безопасно). Полярность: пропустить если exempt || allowAnnotation || !hasMarker. Root marker'а не имеет → штатный DELETE; спец-исключение по Kind НЕ нужно. ИНВАРИАНТ 2 — UPDATE (защита marker от обхода): если oldObject.delete-protected==\"true\", запретить удаление/изменение этого label, КРОМЕ exempt-актора. Break-glass-семантика явно: аннотацию поставить РАЗРЕШЕНО; marker при этом ОСТАЁТСЯ; DELETE разрешается из-за аннотации; вручную снимать marker не требуется и не разрешается; аннотация обратима, пока DELETE не начался (её можно снять). MARKER LIFECYCLE (P10): marker НЕ снимается в штатном жизненном цикле — легальный teardown/GC/reclaim удаляет объект как EXEMPT-актор при СОХРАНЁННОМ marker'е (путь «снять marker → удалить» отсутствует); если удаление идёт от нового актора — добавить его в exempt, а НЕ учить контроллер снимать marker. Admission НЕ читает ownerRef/finalizer/followObjectRef, НЕ ветвится по Kind, НЕ имеет derived-fallback. ROLLOUT: Helm-value deleteGuard.enforcement=Audit|Deny, DEFAULT Audit (поставка модуля ничего не ломает); validationActions выводятся из value; переключение Audit→Deny — пользовательское rollout-действие после достижения доказуемого backfill-gate (P3: все classifier-protected имеют marker). Явно указать, что смена Audit→Deny — пользовательское действие, не часть кода. message — англ., ясный (объект — внутренний элемент unified snapshot; удалять через root Snapshot либо аннотировать deckhouse.io/allow-delete). Синхронно обновить spec (system-spec.md) — раздел «Delete protection (admission)»: единый marker, DELETE+UPDATE инварианты, enforcement-режимы, exempt, break-glass, финализаторы NOT-запрет; ссылка на design/delete-protection-contract.md как SSOT-резюме (не копия). Пользовательская заметка в docs модуля. Helm/werf render чист. Верификация exempt (см. критерий в overview): подтвердить инициатора каждой delete-операции (VSC reclaim, chunks/keeper GC, namespace deletion, cascading GC) по audit/admission-request; особо — реальный SA snapshot-controller."
    status: pending
  - id: p5-deleting-failfast
    content: "P5 — fail-fast деградация по deletionTimestamp (Terminating-ребёнок обречён; гибрид self-condition + родительский fold). Сохранено без изменений семантики. (1) internal/controllers/snapshotcontent/lost_children.go: child CR с deletionTimestamp!=nil трактовать как отсутствующего (имя честно скорректировать, напр. childOwningSnapshotAlive; message «is being deleted»); declared-refs (detectLostFromDeclaredRefs) — deletionTimestamp!=nil = терминальный ChildSnapshotLost; frozen-edges (detectLostFromFrozenEdges) — child SnapshotContent с deletionTimestamp!=nil = исчезнувший (Lost). Гейт на живого owner'а (owner.GetDeletionTimestamp()!=nil → skip) НЕ трогать. Reason'ы НЕ меняются. (2) Self-report на наших CR: genericbinder уже ставит Ready=False/Deleting при deleting-контенте (~714); добавить симметрично — снапшот-CR с СОБСТВЕННЫМ deletionTimestamp!=nil → Ready=False/Deleting. CSI VolumeSnapshot self-condition невозможен → покрывается fold'ом. (3) Юнит/envtest: lost_children_test.go — child с deletionTimestamp (finalizer-заглушка) + контент Ready → owner Ready=False/ChildSnapshotDeleted ПОКА объект существует; не-Ready → Lost; declared-refs → Lost; каскад (owner Terminating) → fold молчит; self-heal не регрессирует. Binder-тест на self-report Deleting. (4) Spec: «deletionTimestamp!=nil на управляемом ребёнке/артефакте = удаление (fail-fast); Terminating не пригоден для restore/download». gofmt; go build + go test ./internal/... ./pkg/... в images/state-snapshotter-controller."
    status: pending
  - id: p6-e2e
    content: "P6 — e2e (новые + адаптация существующих). НОВОЕ e2e/tests/delete_guard_test.go (Ginkgo; gating E2E_DELETE_GUARD; ns snap-e2e-<runID>-<role>). DELETE: (1) managed VolumeSnapshot ребёнка → отклонён (message-подстрока); (2) после annotate deckhouse.io/allow-delete=true → DELETE проходит, ДАЛЕЕ ADR-семантика: root Ready=False/ChildSnapshotDeleted (не-терм.), durable SnapshotContent+VSC живы; затем annotate+DELETE SnapshotContent → root Ready=False/ChildSnapshotLost (терм.); (3) дочерний доменный CR (внук) → отклонён; (4) SnapshotContent/ObjectKeeper → отклонён; (5) durable-артефакты (MCP, чанк, managed VSC) → отклонён; чужой/standalone VSC → удаляется; (6) каскад цел: DELETE root Snapshot → дети исчезают (GC exempt), namespace удаляется (ns-controller exempt); (7) standalone user-VolumeSnapshot без marker → удаляется (нет ложных срабатываний); (8) fail-fast: тестовый финализатор e2e.state-snapshotter.deckhouse.io/hold + allow-delete → Terminating, но root УЖЕ Ready=False/ChildSnapshotDeleted (деградация ДО исчезновения); снять финализатор → исчезает, root неизменен; (9) managed VSC, созданный storage-foundation, СРАЗУ содержит delete-protected=true, и его прямой DELETE запрещён (проверяем только сам объект и запрет DELETE; НЕ моделируем исполнение CSI). MARKER IMMUTABILITY (UPDATE-инвариант): (10) обычный пользователь НЕ может снять delete-protected marker; (11) НЕ может изменить \"true\" на другое значение; (12) exempt-controller МОЖЕТ поддерживать/ставить marker; (13) break-glass оставляет marker: чтобы проверить ПОСЛЕ разрешённого DELETE (объекта уже нет), удерживаем hold-finalizer: поставить тестовый finalizer e2e.state-snapshotter.deckhouse.io/hold → annotate allow-delete=true → DELETE → объект в Terminating → ПРОВЕРИТЬ deletionTimestamp установлен И delete-protected=true СОХРАНИЛСЯ (доказывает: admission разрешил из-за break-glass, а НЕ из-за снятия marker) → снять finalizer → исчезает; (14) root без marker свободно обновляется и удаляется; (15) пользователь может УДАЛИТЬ break-glass annotation, пока DELETE не начался (persistent/reversible, не необратима). АДАПТАЦИЯ существующих сознательных удалений (allow-delete перед delete; хелпер annotateAllowDelete в e2e_shared_test.go): namespace_capture_rbac_test.go (E3), manifest_checkpoint_loss_test.go, volumedata_gc_test.go, backup_restore_test.go (grep по e2e на Delete защищённых kinds — править ВСЕ); e2e/Makefile clean-env (annotate перед kubectl delete objectkeepers/vsc/contents/mcp). ВАЖНО: e2e КОМПИЛИРУЮТСЯ (cd e2e && go vet ./tests/... или go build ./...); прогон НЕ делать (admission на кластере ещё нет / Audit-режим — пользователь). gofmt."
    status: pending
  - id: p7-review
    content: "Deep review (P2a+P2b+P3+P4+P5+P6): сабагент deep-reviewer (read-only), незакоммиченный дифф repo state-snapshotter + storage-foundation (передать список файлов явно). Акценты: marker — ОДИН признак; admission НЕ читает ownerRef/finalizer/followObjectRef, НЕ ветвится по Kind, НЕТ derived-fallback (per-Kind эвристики старой модели НЕ вернулись); ДВА инварианта (DELETE + UPDATE-защита marker), break-glass оставляет marker; root без marker и без спец-исключения; ordering (P8) — VS marker patch до публикации (vetoed без marker), СОЗДАВАЕМЫЕ нами узлы (наши CR/SnapshotContent/MCP/чанки/ObjectKeeper/VSC) несут marker в ИСХОДНОМ CREATE-payload, НЕ post-create patch (тест бьёт по create-объекту, нет post-create окна); NON-GOAL СОБЛЮДЁН — в план НЕ просочилась VCR/CSI-execution семантика (никаких CreateSnapshot/readyToUse/DataRef/re-drive/§3.4); marker lifecycle (P10) — не снимается штатно, teardown/GC через exempt при сохранённом marker'е; первую запись делает write-path (P9), единственное исключение — versioned migration backfill; backfill — единый list-and-patch, идемпотентен, gate доказуемый («все classifier-protected имеют marker», не «uncovered не существует»), классификатор не читается admission'ом; break-glass persistent/reversible (не «одноразовый»); enforcement default Audit (Audit|Deny) + переключение вне кода; exempt полон, БЕЗ system:masters, инициаторы подтверждены по admission/audit (не только Deployment SA); fail-fast fold гейтится на живого owner'а, reason'ы не меняются; e2e покрыли DELETE + marker-immutability (10-15) + все сознательные удаления (grep); spec синхронно коду и ссылается на design-контракт (docs-ssot). Находки правит implementer; цикл до «НАХОДОК НЕТ»."
    status: pending
  - id: p8-commit
    content: "Коммиты. state-snapshotter (ветка по git-состоянию — НЕ переключать без нужды): (A) marker core write-path (P2a) + backfill (P3); (B) admission delete-guard DELETE+UPDATE + enforcement value + spec-раздел + docs-заметка + e2e (P4,P6); (C) fail-fast (P5) + юнит/envtest + spec-раздел + e2e сценарий 8. storage-foundation: отдельный коммит VS/VSC marker (P2b) + bump версии SDK/константы — согласовать версию. Сообщения — минимальный plain text без трейлеров. Перед коммитом go-lint НЕ на грязном дереве: коммит → GO_BUILD_TAGS=\"ce ee se seplus csepro\" ./go-lint.sh → при --fix amend → до чистого выхода (в каждом репо по его правилам). PUSH НЕ делать (по явному запросу; редеплой/включение Deny — пользователь)."
    status: pending
  - id: adr-delete-guard
    content: "ADR overview (arch/.../state-snapshotter/2026-06-29-unified-snapshots-overview.md) — правка В РАБОЧЕМ ДЕРЕВЕ, БЕЗ КОММИТА. Под модель rev.2: (1) прямое удаление managed-узлов блокируется admission по ЕДИНОМУ marker delete-protected (не per-Kind); marker защищён и от UPDATE (снятие — не поддерживаемый обход); root не защищён (штатный DELETE); break-glass deckhouse.io/allow-delete (marker остаётся); ChildSnapshotDeleted/Lost — для обходных путей; fail-fast: deletionTimestamp!=nil = удалён немедленно (Deleted vs Lost по контенту). (2) Корзина/reclaim: SnapshotContent, наши ObjectKeeper, durable-артефакты (MCP/чанки/managed VSC) защищены тем же marker'ом; managed VSC создаётся storage-foundation сразу с marker'ом (в API уже защищён). (3) финализаторы намеренно НЕ запрет (KEP-2839); parent-protect/artifact-protect — teardown. (4) membership vs delete-protection разведены; enforcement Audit→Deny — rollout; сослаться на design/delete-protection-contract.md (не дублировать). NON-GOAL: НЕ описывать здесь контракт/retry/CSI-execution VCR — это отдельный VCR-трек. Стиль как в документе; соседние разделы не переписывать. Консистентность с SDK-ADR (2026-06-29-domain-snapshot-sdk.md)."
    status: pending
  - id: adr-delete-guard-review
    content: "Deep review ADR-диффа: сабагент deep-reviewer, read-only, дифф рабочего дерева ADR-репо ТОЛЬКО по файлам этого блока (список наших файлов передать явно). Акценты: семантика ChildSnapshotDeleted/Lost не сломана; литералы (delete-protected, deckhouse.io/allow-delete, exempt-акторы) совпадают с реализацией; DELETE+UPDATE-инварианты и enforcement-режимы отражены; нет дублирования контракта overview↔SDK-ADR↔design-doc (SSOT). Находки правит implementer. Цикл до «НАХОДОК НЕТ». КОММИТА НЕТ (правило воркспейса); о незакоммиченных ADR-правках сообщить пользователю в финальном отчёте."
    status: pending
isProject: false
---

# Единый план: unified-snapshot delete protection (marker-модель rev.2)

## Контекст (инцидент и диагноз)

Пользователь удалил из console-UI (под `kubernetes-super-admin`) дочерний CSI `VolumeSnapshot`
(ребёнок root `Snapshot`); на ребёнке был только CSI-финалайзер, удаление прошло беспрепятственно.
Итог: root деградировал в `Ready=False/ChildSnapshotDeleted`; durable-слой цел, но namespaced-поверхность
неполна, снимок не скачивается, in-cluster restore TBD → практически необратимая деградация.

Финализаторы **не запрещают** удаление (объект виснет в `Terminating`; KEP-2839 Liens не реализован).
Индустриальный стандарт запрета — admission DENY. Кластер v1.34.

## Scope и Non-goals (жёсткая граница)

**В scope этого плана ровно две вещи:**

1. правки SDK / переход namespace snapshot-кода на новую семантику (`DomainCaptureStatus`) — как
   prerequisite-верификация (P1);
2. защита объектов unified snapshot tree от прямого удаления (marker + admission + backfill + fail-fast).

`VolumeSnapshot`/`VolumeSnapshotContent` входят сюда **как защищаемые узлы snapshot-протокола**, а не из-за
VCR: допустима только delete-protection-интеграция (managed VS/VSC получает `delete-protected`; admission
блокирует прямой `DELETE`; штатный GC/reclaim идёт через exempt-актора).

**Non-goals (отдельный VCR-план, `volume-capture-request-contract.md`):** этот план **не** изменяет
контракт `VolumeCaptureRequest`, его retry/terminal-семантику, CSI operation ownership, идентичность/
re-drive CSI-снапшота, commit point `DataRef` и восстановление утраченного VSC. Любые формулировки про
`CreateSnapshot` in-flight, `readyToUse`, CSI execution window сознательно **вынесены** отсюда, чтобы
delete-protection-план не менял контракт VCR незаметно.

## Prerequisite P1 (не workstream)

`p1-verify-sdk-baseline` — только верификация уже сделанной миграции на единый `DomainCaptureStatus`
(старые lifecycle verbs удалены; `af28c95`). Новых namespace-правок в scope **нет**. Это фундамент:
marker (P2) ставится на том же write-пути.

## Терминология и рамка (важно для implementer)

- **`delete-protected` — это authoritative protection state, а не «служебная метка».** Он authority
  (единственный источник истины guard), immutable для пользователя (P7), с единственным владельцем записи
  (write-path, P9). Слово «marker» ниже — краткий синоним именно этого authoritative-состояния
  (контракт §0.1).
- **Delete protection — часть snapshot protocol, а не свойство «важного объекта».** Защищается
  **корректность дерева**: узел нельзя вынуть из графа помимо штатного teardown. Ответ на «почему защищён
  этот Kind?» всегда один — «он узел протокола, введённый в граф write-path'ом», а не оценка важности
  (контракт §0.2). Это объясняет, почему `ObjectKeeper`/VSC защищены, а root — нет.

## Модель (rev.2 — единый marker, без эвристик, + защита от обхода)

> **rev.2 / rev.3:** `rev.2` — архитектурная marker-модель (её и реализует план). `rev.3` — текущая
> редакция самого контракта `delete-protection-contract.md` (полировка формулировок: терминология,
> §6.5 lifecycle, инварианты P7–P10, идемпотентный backfill, CREATE-payload для VSC, persistent break-glass).
> Это не разные модели, а модель (rev.2) и версия её контракт-документа (rev.3).

Нормативный контракт — `docs/internal/state-snapshotter-rework/design/delete-protection-contract.md`.
Здесь — применение:

1. **Один канонический marker** `state-snapshotter.deckhouse.io/delete-protected: "true"` (новая
   Go-константа; **не** `…/managed`).
2. **Marker ставит authoritative write-путь** в момент введения узла. **Create-vs-adopt:** создаваемые нами
   узлы (наши CR, `SnapshotContent`, MCP/чанки, `ObjectKeeper`, CSI VSC) несут marker **в исходном
   CREATE-payload** (не post-create patch — между `Create` и `patch` нет ordering); **patch — только для
   adoption** существующего (CSI VS), до публикации edge. Ordering: **до** публикации узла в дерево (P8);
   для VSC причина простая — **объект появился в API → уже должен быть защищён** (marker в CREATE-payload),
   без завязки на VCR/CSI-execution.
3. **Admission проверяет только marker**, с ДВУМЯ инвариантами: DELETE-deny защищённого и UPDATE-защита
   самого marker'а (нельзя снять/изменить, кроме exempt). **Не** читает ownerRef/finalizer/followObjectRef,
   **не** ветвится по Kind, **нет** derived-fallback.
4. **Root `Snapshot` marker НЕ получает** → штатный DELETE; спец-исключение по Kind не нужно.
5. **Marker не снимается в штатном жизненном цикле** (P10): легальный teardown/GC/reclaim удаляет объект
   как **exempt-актор** при СОХРАНЁННОМ marker'е; путь «снять marker → удалить» не существует. Если удаление
   идёт от нового актора — его добавляют в exempt, а не учат контроллер снимать marker.
6. **Backfill** — единый cluster-wide list-and-patch; **идемпотентен по контракту** (можно запускать
   сколько угодно раз); gate доказуемый (см. ниже).
7. **Rollout** — enforcement `Audit|Deny` (default Audit); `Audit→Deny` — пользовательское действие после
   достижения backfill-gate.

**Membership ≠ delete-protection:** marker — только политика удаления; идентичность дерева держат
логические рёбра. **Marker authority:** steady-state — только authoritative write-path; legacy-сигналы
используются **только** backfill-классификатором и **никогда** — admission'ом.

## Защита от обхода (почему UPDATE-инвариант обязателен)

Только DELETE-deny обходится тривиально: `kubectl label … delete-protected-` затем `kubectl delete …` —
менее явно, чем break-glass. Поэтому admission защищает и `UPDATE`: при `oldObject.delete-protected=="true"`
удаление/изменение label запрещено (кроме exempt). Единственный поддерживаемый обход — аннотация
`deckhouse.io/allow-delete: "true"`: её поставить **разрешено**, marker при этом **остаётся**, DELETE
проходит из-за аннотации; снимать marker вручную не требуется и не разрешается.

## VS/VSC protection на write-path (без изменения контракта VCR)

`VolumeSnapshot`/`VolumeSnapshotContent` попадают сюда **не из-за VCR**, а как защищаемые узлы
snapshot-протокола. Единственное требование этого плана:

> `storage-foundation` создаёт управляемый `VolumeSnapshotContent` **уже с `delete-protected=true`**
> (в исходном `CREATE`, `metadata.labels`). Это атомарно устраняет окно между появлением VSC в
> Kubernetes API и применением delete-protection.

Причина простая: **объект появился в API → он уже должен быть защищён**. Между `Create` и последующим
`PATCH` нет гарантированного ordering, поэтому post-create patch оставил бы короткое незащищённое окно;
`CREATE` с marker'ом — единственная атомарная точка. То же для managed CSI `VolumeSnapshot`: marker
проставляется при adoption (`managed=true`) **до** публикации graph edge.

**Это изменение НЕ меняет** lifecycle, retry, terminal-семантику VCR, CSI operation ownership или
восстановление утраченного VSC — см. **Non-goals**.

## Cross-repo (Вариант A — минимум в scope)

Без VS/VSC marker защита не закрывает инцидентный путь, а VS/VSC-код в **storage-foundation**. Поэтому
в план входит **минимальная** storage-foundation-часть (`p2b`): marker на VS-адопции и VSC-создании +
необходимый bump версии SDK/константы + сборка/тесты + отдельный коммит. Полный переезд прочих
SDK-consumer'ов — вне scope. `Deny` не включать до готовности P2b (иначе часть пути не покрыта).

## Rollout Audit → Deny

- **Фаза 1:** admission установлен с `deleteGuard.enforcement=Audit` (default); marker write-path работает;
  backfill выполняется (можно перезапускать — идемпотентен).
- **Фаза 2:** повторный проход не находит ни одного своего объекта без marker (доказуемый gate — «все
  classifier-protected имеют marker», НЕ недоказуемое «uncovered не существует вообще») → пользователь
  переключает `enforcement=Deny`.

Смена `Audit→Deny` — **пользовательское rollout-действие**, не часть кода. Манифест по умолчанию — `Audit`,
чтобы поставка модуля ничего не ломала.

## Принятые решения (зафиксированы с пользователем)

1. Механизм — admission (не webhook, не финализаторы).
2. Единый marker вместо per-Kind предикатов (rev.2); DELETE + UPDATE инварианты.
3. Break-glass — `deckhouse.io/allow-delete: "true"` (marker остаётся).
4. Exempt-акторы — критерий подтверждения инициатора по admission/audit; `system:masters` НЕ exempt.
5. Fail-fast по `deletionTimestamp` — гибрид (сохранено).
6. Семантика сознательного удаления НЕ меняется.
7. ADR — правка в дереве без коммита; spec — в коммите с кодом.
8. Cross-repo VS/VSC — Вариант A (минимум в scope).
9. Backfill — единый list-and-patch; enforcement Audit default.

## Прогресс

> Зеркало фронтматтера; при смене статуса обновлять обе части. `[ ]` pending · `[~]` in_progress · `[x]` completed.

- [ ] `p1-verify-sdk-baseline` — верификация базовой линии SDK/namespace (без правок).
- [ ] `p2a-marker-writepath-core` — marker на write-пути в core.
- [ ] `p2b-marker-writepath-storagefoundation` — marker VS/VSC (cross-repo, VSC — в CREATE-payload) + bump SDK.
- [ ] `p3-marker-backfill` — единый list-and-patch backfill (идемпотентен) + доказуемый gate «все classifier-protected имеют marker».
- [ ] `p4-delete-guard-admission` — DELETE-deny + UPDATE-защита marker; enforcement Audit|Deny; spec.
- [ ] `p5-deleting-failfast` — fold Terminating-детей + self-report Deleting + юнит/envtest + spec.
- [ ] `p6-e2e` — deny/allow/каскад/Terminating/VSC-window + marker-immutability + адаптация существующих.
- [ ] `p7-review` — deep review до «НАХОДОК НЕТ».
- [ ] `p8-commit` — логические коммиты (core marker+backfill; guard+spec+e2e; fail-fast) + storage-foundation; go-lint; push НЕ делать.
- [ ] `adr-delete-guard` — правка overview-ADR в рабочем дереве (без коммита).
- [ ] `adr-delete-guard-review` — deep review ADR-диффа до «НАХОДОК НЕТ» (коммита нет by rule).

## Зависимости блоков

- `p1` → `p2a` → `p2b` (общая константа marker'а) → `p3` → `p4` → `p5` → `p6` → `p7` → `p8`
  (последовательно; e2e-сценарии 8 зависят от P5, 9 — от P2b, 10-15 — от UPDATE-инварианта P4).
- `adr-delete-guard` — после `p5` (финальные литералы/семантика), другой репо ⇒ может идти параллельно
  `p6`/`p7`. `adr-delete-guard-review` — после `adr-delete-guard`.
- **Cross-repo:** `p2b` в storage-foundation требует согласования версии SDK/констант (общая константа
  marker'а) — отметить при выполнении.

## Правила и источники (аудит)

- **state-snapshotter/CLAUDE.md + .cursor/rules:** gofmt на тронутое; `./go-lint.sh` только в коммит-цикле,
  `GO_BUILD_TAGS="ce ee se seplus csepro"`; тесты после правок; redeploy-гейт — редеплой вне плана;
  коммит-сообщение — минимальный plain text; docs-ssot: контракт меняется ⇒ spec в том же чейнджсете,
  нормативная модель — SSOT-резюме со ссылкой на `design/delete-protection-contract.md`; labels-конвенция:
  marker — Go-константа.
- **storage-foundation** (`p2b`): свои правила репо (gofmt/lint/build); отдельный коммит; версия SDK.
- **Воркспейс/CLAUDE.md:** ADR-репо — правки в дереве, коммит только по явному запросу; UI-слой — отдельный
  план; d8 CLI не трогаем.
- Выкладка admission, переключение `Audit→Deny` и прогон e2e — ПОЛЬЗОВАТЕЛЬ.

## Что изменилось (rev.2 → полировка контракта)

Первая редакция плана → rev.2:

- **Единый marker** вместо дизъюнкции per-Kind предикатов; **root не помечается** (спец-исключение убрано).
- **Добавлен UPDATE-инвариант** (защита marker от снятия/изменения) — иначе защита обходится тривиально.
- **VSC marker** — в исходном CREATE-payload у storage-foundation (объект в API уже защищён).
- **Backfill — единый list-and-patch**; **derived-fallback в guard убран** (до backfill — Audit).
- **Rollout Audit→Deny** через Helm-value (default Audit); переключение — пользовательское действие.
- **Cross-repo — Вариант A**: минимальная storage-foundation-часть в scope.
- **WS1 → Prerequisite P1** (verification gate). **Файл/`name` переименованы** в `unified-snapshot-delete-protection`.

Полировка контракта (эта ревизия, по ревью — терминология/инварианты, чтобы через год не трактовали иначе):

- **Терминология:** `delete-protected` явно назван **authoritative protection state** (authority/immutable/
  write-path-owned), а не «служебная метка» (контракт §0.1).
- **Философия:** delete protection — **часть snapshot protocol**, защищает **корректность дерева**, а не
  «важность» объекта; отсюда «почему защищён этот Kind» = «он узел протокола» (контракт §0.2).
- **Marker lifecycle (P10):** явно зафиксировано — marker **не снимается** в штатном цикле; teardown/GC/
  reclaim идёт через **exempt-актора** при сохранённом marker'е; нового удаляющего актора добавляют в exempt,
  а не учат снимать marker (закрывает вопрос «кто снимает marker перед GC»).
- **VSC/наши узлы — marker в исходном CREATE-payload**, а **не** post-create patch: между `Create` и `patch`
  нет ordering → короткое незащищённое окно в API. Причина нейтральная («объект в API → уже защищён»), без
  VCR/CSI-execution семантики. Patch — только для adoption (CSI VS), до публикации edge. Тест — на
  CREATE-объекте.
- **Жёсткая граница с VCR:** добавлен раздел Non-goals; из плана убраны `CreateSnapshot`/`readyToUse`/
  `DataRef`/re-drive/§3.4 и «VSC construction protocol как часть контракта VCR» — эволюция VCR в отдельном
  плане (`volume-capture-request-contract.md`).
- **Backfill — идемпотентность по контракту** (запуск сколько угодно раз) и **доказуемый gate** («все
  classifier-protected имеют marker» вместо недоказуемого «uncovered не существует»).
- **Break-glass — честно persistent/reversible** (не «одноразовый»): одноразовость нереализуема без
  отдельного механизма (admission видит DELETE, не предварительный UPDATE); токен/expiring — future hardening.
- **Новые инварианты P7–P10** выписаны явно в контракте §9: P7 (UPDATE-защита), P8 (обязателен до графа),
  P9 (первая запись — write-path; исключение — versioned migration backfill), P10 (не снимается штатно).
- **rev.2/rev.3:** rev.2 — архитектурная marker-модель (её и реализуем); rev.3 — текущая редакция контракта
  (полировка формулировок), см. `delete-protection-contract.md`.

## Definition of Done

Выполняет `start-plan` по завершении: (1) все `todos` → `status: completed` + зеркало `- [x]`;
(2) `completed: <YYYY-MM-DD>`; (3) файл → `plans/done/` (slug и дата-префикс не менять). Вне DoD: push,
merge SDK в main и релиз версии, выкладка модуля/admission, переключение `Audit→Deny`, прогон e2e на
кластере, коммит ADR — по явному запросу пользователя.
