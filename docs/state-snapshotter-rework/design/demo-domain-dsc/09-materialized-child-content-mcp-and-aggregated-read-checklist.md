# Materialized Child Content MCP and Aggregated Read Checklist

**Status:** implementation checklist, non-normative.

Normative contracts:

- [`../../spec/system-spec.md`](../../spec/system-spec.md)
- [`../../spec/snapshot-aggregated-read.md`](../../spec/snapshot-aggregated-read.md)

Related design note:

- [`./090-unified-snapshot-controller-lifecycle.md`](./090-unified-snapshot-controller-lifecycle.md)

## Implementation Checklist

- Every `XxxSnapshot` has its own `XxxSnapshotContent`.
- Every materialized content node writes `status.manifestCheckpointName`.
- Parent controllers create child snapshots and publish complete `status.childrenSnapshotRefs`.
- Parent controllers publish content edges through `status.childrenSnapshotContentRefs` where the read model needs child content traversal.
- Child controllers do not self-register in parent status.
- Parent `Ready` is derived from own MCP state and direct children state.
- Aggregated read combines MCPs only in the API/usecase layer.
- Dedicated demo controllers can reconcile manual demo snapshots without DSC, but `NamespaceSnapshot` discovery sees demo resources only through eligible DSC mappings.

## OwnerRef Filtering Examples

- VM snapshot may include resources owned by the VM when they are part of VM own scope.
- VM snapshot must not include resources owned by a disk object; they belong to disk snapshot scope.
- Disk snapshot may include resources owned by the disk object when they are part of disk own scope.
- `NamespaceSnapshot` top-level DSC discovery must skip resources with `ownerReferences`; owned resources are covered by the owner domain subtree when that owner is registered and skipped fail-closed when it is not.
- `NamespaceSnapshot` root own MCP must not accidentally include domain-owned resources delegated to child snapshots or skipped by ownerRef filtering.

## Validation Cases

- Disk-only DSC with `DemoVirtualDisk ownerReference -> DemoVirtualMachine` does not create a direct `DemoVirtualDiskSnapshot` under root.
- VM+Disk DSC creates `DemoVirtualMachineSnapshot` as a direct root child and creates `DemoVirtualDiskSnapshot` only under the VM subtree.
- Root `childrenSnapshotRefs` does not contain VM-owned disk snapshots directly.
- Root own MCP contains the namespace-level own scope and does not include child snapshot MCP objects.
- Aggregated read returns the expected subtree and fails on duplicate object identity.
- MCP-level endpoints return only local manifests and do not aggregate children.

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
- disk-only DSC skips a VM-owned disk instead of creating it as a direct root child;
- VM+Disk DSC keeps disk under VM subtree, not directly under root;
- child degradation is reflected by parent;
- aggregated read returns root and child subtree manifests;
- root own MCP contains only namespace-scoped allowlist objects and excludes child MCP objects;
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
4. Ensure lifecycle and aggregated-read algorithm details are linked to normative/design documents instead of being copied here.

Docs are part of the result, not a side artifact. After the change, code and docs must describe the same model.
