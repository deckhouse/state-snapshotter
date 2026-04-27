# Unified XxxSnapshotController Lifecycle and Aggregated Read Checklist

**Status:** working implementation checklist, not a normative contract.  
**Normative source of truth:** [`../../spec/system-spec.md`](../../spec/system-spec.md).

## Goal

Build one lifecycle model for every `XxxSnapshotController`:

- every `XxxSnapshot` has its own `XxxSnapshotContent`;
- every content node stores `status.manifestCheckpointName`;
- every controller materializes its own scope through MCR/MCP;
- parent controller creates child snapshots and writes `childrenSnapshotRefs`;
- child controller does not self-register in parent status;
- parent `Ready` is aggregated by reading child `Ready` conditions;
- aggregated read can start from any content node and return YAML/JSON for the whole subtree.

## Controller Template

Every domain snapshot controller follows the same lifecycle:

1. **Load own snapshot**: snapshot may exist standalone; missing or unavailable parent must not block own materialization.
2. **Ensure own content**: create or find `XxxSnapshotContent`; write `status.boundSnapshotContentName`.
3. **Compute own scope**: determine Kubernetes resources this controller backs up itself.
4. **Compute child snapshots**: create/ensure child `YyySnapshot`; write own `status.childrenSnapshotRefs`; write own content `status.childrenSnapshotContentRefs` where applicable.
5. **Own materialization**: create/ensure MCR for own scope; wait for `ManifestCheckpoint Ready=True`; write `XxxSnapshotContent.status.manifestCheckpointName`.
6. **Aggregate children**: read each child from `status.childrenSnapshotRefs`; inspect `child.status.conditions[Ready]`.
7. **Set Ready**: `Ready=False` while own MCP is not ready or any child is missing/not ready/failed; `Ready=True Completed` only when own MCP is ready and all children are ready.

## Own Scope

Generic code does not know which resources belong to a domain snapshot. Only the concrete domain controller defines its own scope.

- `DemoVirtualDiskSnapshotController`: if PVC is declared and allowed by scope filtering, include PVC manifest in own MCR; later data capture can add VCR. Missing PVC is not fatal; minimal/empty materialization is valid.
- `DemoVirtualMachineSnapshotController`: includes VM-level resources such as Pod/qemu only when they belong to the VM; disk PVCs belong to disk snapshots and must be delegated to child disk snapshots.
- `NamespaceSnapshotController`: captures the Kubernetes `Namespace` object plus standard namespace resources, discovers domain-owned resources only through eligible DSC mappings in the graph registry, creates top-level child snapshots for them, and excludes covered child subtree resources from root own MCR.

Dedicated demo controllers may run and reconcile manual demo snapshots before any DSC exists. That only proves the controller process is active; it does not make demo resources discoverable by `NamespaceSnapshot` until the DSC mapping is eligible.

## OwnerReference / Scope Filtering

When building own scope, a controller must not blindly include every resource it finds.

Rule:

- if a resource has `ownerReferences`;
- and the owner reference points to an object different from the domain object currently being snapshotted;
- then the resource is skipped from this controller's own MCR.

The controller may include a resource only when:

- the resource has no `ownerReferences`;
- or an owner reference points to the current domain object;
- or the resource is explicitly allowed as root/namespace-level scope.

Examples:

- VM snapshot includes Pod/qemu when it has ownerRef to the VM.
- VM snapshot does not include PVC when the PVC has ownerRef to a disk object.
- Disk snapshot includes PVC when it has ownerRef to the disk object.
- NamespaceSnapshot first creates child snapshots for DSC-covered resources, then its own MCR keeps only uncovered standard resources.
- E5 exclude reads MCPs from every descendant content-node reached through `childrenSnapshotContentRefs`, including dedicated `XxxSnapshotContent` nodes; child manifests stay in child MCPs and are not copied into parent MCPs.

## Ownership Rules

Parent owns graph edges.

If a controller creates a child snapshot, it writes:

- `status.childrenSnapshotRefs`;
- `content.status.childrenSnapshotContentRefs` when content graph traversal must include the child content node.

Child controllers must not patch parent status. A child controller owns only:

- its own snapshot;
- its own content;
- its own MCR/MCP;
- its own `Ready`.

## Aggregated Read

Aggregated read is generic and domain-agnostic.

Algorithm:

1. start from any content node;
2. read `status.manifestCheckpointName`;
3. load objects from MCP archive;
4. recurse through `status.childrenSnapshotContentRefs`;
5. fail as one operation on incomplete graph.

Aggregation is the only step that combines parent and child MCPs. Write-path materialization keeps each MCP scoped to its own node.

Fail-closed cases:

- content node without `manifestCheckpointName`;
- missing child content;
- missing or NotReady child MCP;
- duplicate object identity across MCPs;
- missing registry for heterogeneous content traversal.

## NamespaceSnapshot Specifics

`NamespaceSnapshotController` follows the same lifecycle. Its only difference is discovery: it uses DSC/registry to decide which top-level resources must be delegated to domain child snapshots. Other domain controllers usually compute children from their own domain model.

The minimal own MCP for `NamespaceSnapshot` contains the Kubernetes `Namespace` object named by the resolved target namespace. Currently resolved target namespace = `NamespaceSnapshot.metadata.namespace`; a future cluster-scoped `NamespaceSnapshot` may resolve it from `spec.targetNamespace`. `NamespaceSnapshot` remains namespaced in this change; no `spec.namespace` / `spec.targetNamespace` field is introduced.

## Do Not Reintroduce

- the old expected/current dual-list field;
- the old graph-pending condition reason;
- the old expected-vs-current child-ref set helpers;
- child self-registration;
- ready-list;
- namespace in `childrenSnapshotRefs`;
- demo-specific branches in generic `internal/usecase`.

## Tests

Unit coverage:

- own MCP absent means `Ready=False`;
- no children plus own MCP ready means `Ready=True`;
- child missing means parent pending;
- child `Ready=False` terminal means parent failed;
- child `Ready=True` allows parent completion;
- aggregated read starts from arbitrary content node;
- content node without MCP fails closed.

Integration coverage:

- `NamespaceSnapshot` creates top-level child snapshots through DSC;
- `DemoVirtualMachineSnapshot` creates child disk snapshots;
- `DemoVirtualDiskSnapshot` works standalone;
- child degradation is reflected by parent;
- aggregated read returns root and child subtree manifests;
- root own MCP contains the Namespace object and excludes child MCP objects;
- read from dedicated content returns only that node and descendants;
- child controller does not patch parent graph.

## Final Checks

```shell
bash hack/generate_code.sh
cd api && go test ./... -count=1
cd ../images/state-snapshotter-controller
go test ./internal/usecase ./internal/controllers -count=1
go test -tags=integration ./test/integration/... -count=1
```

```shell
rg "<old expected/current graph model identifiers>"
rg "childrenSnapshotRefs.*namespace|NamespaceSnapshotChildRef.*Namespace"
rg "DemoVirtual" images/state-snapshotter-controller/internal/usecase
rg "patch.*childrenSnapshotRefs" images/state-snapshotter-controller/internal/controllers
```

The last search is not an absolute ban: graph patches are valid only on parent-controller paths, not on child self-registration paths.

## Documentation Final Validation

After implementation:

1. Review all related documents and ensure they do not contradict the new model:
   - no old expected/current graph model;
   - no child self-registration;
   - no old lifecycle patterns;
   - all current text uses one `XxxSnapshotController` lifecycle template.
2. Check terminology consistency:
   - `own scope`;
   - `child snapshots`;
   - `childrenSnapshotRefs`;
   - `manifestCheckpointName`;
   - `aggregated read`.
3. Ensure `NamespaceSnapshot` is described as a normal controller with a separate DSC discovery mechanism, not as a special root type.

Docs are part of the result, not a side artifact. After the change, code and docs must describe the same model.

## Final Model

- Own materialization is the responsibility of the current controller.
- Own materialization scope is resources owned by the current domain object, excluding resources delegated to child snapshots.
- Child graph is the responsibility of the parent controller.
- Child state is the responsibility of the child controller.
- Parent readiness is aggregation by the parent controller.
- Aggregated read is generic traversal over the content graph.
