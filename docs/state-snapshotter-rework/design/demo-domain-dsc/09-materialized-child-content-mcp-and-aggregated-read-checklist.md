# Materialized Child Content MCP and Aggregated Read Checklist

Статус: рабочий чеклист реализации (не нормативный контракт).  
Нормативный источник истины: `docs/state-snapshotter-rework/spec/system-spec.md`.

## Цель

Дотянуть текущий demo-flow до модели:

- каждый child snapshot имеет свой `*SnapshotContent`;
- каждый materialized content-node имеет `status.manifestCheckpointName`;
- `Ready` child/parent зависит от реальных MCP, а не только от stub;
- aggregated read собирает данные по content graph (через `childrenSnapshotContentRefs`) с любого content-node.

## Жесткие ограничения

- не менять архитектуру graph/E5/E6;
- не добавлять synthetic tree;
- не создавать child `NamespaceSnapshot`;
- не возвращать `namespace` в `childrenSnapshotRefs` (форма refs остается `apiVersion/kind/name`);
- generic код не знает про demo-типы (без `if Demo...`);
- все в рамках одного namespace;
- CRD YAML руками не редактировать: только Go API types -> `bash hack/generate_code.sh`.

## Этапы

### 1) API/status + codegen

Файлы:

- `api/demo/v1alpha1/demovirtualdisksnapshotcontent_types.go`
- `api/demo/v1alpha1/demovirtualmachinesnapshotcontent_types.go`

Сделать:

- добавить `status.manifestCheckpointName` в оба `*SnapshotContentStatus`.
- запустить `bash hack/generate_code.sh`.

Сгенерированные изменения ожидаются в:

- `api/demo/v1alpha1/zz_generated.deepcopy.go` (если затронется),
- `crds/demo.state-snapshotter.deckhouse.io_demovirtualdisksnapshotcontents.yaml`,
- `crds/demo.state-snapshotter.deckhouse.io_demovirtualmachinesnapshotcontents.yaml`.

Gate:

- `manifestCheckpointName` отражен в API/CRD.
- CRD изменены только генератором.

Текущий статус: **сделано**.

### 2) Generic traversal for aggregated read

Файлы:

- `images/state-snapshotter-controller/internal/usecase/aggregated_namespace_manifests.go`
- `images/state-snapshotter-controller/internal/usecase/namespacesnapshot_content_graph.go`
- тесты content graph / aggregated usecase.

Сделать:

- traversal по `childrenSnapshotContentRefs` должен читать MCP у любого content-node;
- если materialized node без MCP -> fail-closed;
- без demo-specific веток.

Gate:

- unit: MCP собирается с root+child content;
- unit: отсутствие MCP дает fail-closed.

Текущий статус: **в работе**.

### 3) Disk materialization MCP

Файл:

- `images/state-snapshotter-controller/internal/controllers/demovirtualdisksnapshot_controller.go`

Сделать:

1. ensure content;
2. ensure/create MCR;
3. дождаться MCP `Ready=True`;
4. записать `content.status.manifestCheckpointName`;
5. `snapshot Ready=True Completed` только после MCP.

Gate:

- integration: disk snapshot -> content -> MCP -> Ready.

### 4) VM materialization MCP + Ready by children

Файл:

- `images/state-snapshotter-controller/internal/controllers/demovirtualmachinesnapshot_controller.go`

Сделать:

1. ensure content;
2. ensure VM-level MCR/MCP (минимальный capture допустим);
3. записать MCP в VM content;
4. `VM Ready=True` только если:
   - VM MCP готов;
   - child disk snapshots готовы.

Gate:

- integration VM->Disk: MCP у VM и Disk, refs связаны, root `Ready`.

### 5) E5/E6 regression tests

Файлы:

- `images/state-snapshotter-controller/internal/usecase/root_capture_run_exclude_test.go`
- `images/state-snapshotter-controller/internal/usecase/namespace_snapshot_parent_ready_e6_test.go`
- `images/state-snapshotter-controller/test/integration/namespacesnapshot_graph_e5_e6_integration_test.go`
- `images/state-snapshotter-controller/test/integration/demovirtualmachinesnapshot_pr5b_test.go`
- тесты aggregated read.

Проверки:

- exclude учитывает child MCP;
- child MCP pending -> root `Ready=False` / `SubtreeManifestCapturePending`;
- E6 не дает Completed без MCP-gated readiness;
- refs без `namespace`;
- generic usecase без demo-импортов.

### 6) Документация и статус

Обновить:

- `docs/state-snapshotter-rework/spec/system-spec.md`
- `docs/state-snapshotter-rework/design/implementation-plan.md`
- `docs/state-snapshotter-rework/design/namespace-snapshot-controller.md`
- `docs/state-snapshotter-rework/testing/e2e-testing-strategy.md`
- `docs/state-snapshotter-rework/operations/project-status.md`
- `docs/state-snapshotter-rework/design/demo-domain-dsc/*.md`

Зафиксировать:

- snapshot refs = topology-only;
- content хранит MCP;
- aggregated read идет по content graph;
- exclude основан на child MCP;
- demo создает реальные MCP;
- CSI остается future (если не реализуется в этом шаге).

## Финальные проверки

- `bash hack/generate_code.sh`
- `cd api && go test ./... -count=1`
- `cd images/state-snapshotter-controller && go test ./internal/usecase ./internal/controllers -count=1`
- `cd images/state-snapshotter-controller && go test -tags=integration ./test/integration/... -count=1`
- `rg "childrenSnapshotRefs.*namespace|ref\\.Namespace|NamespaceSnapshotChildRef.*Namespace" .`
- `rg "synthetic" images/state-snapshotter-controller/internal`
- `rg "DemoVirtual" images/state-snapshotter-controller/internal/usecase`
