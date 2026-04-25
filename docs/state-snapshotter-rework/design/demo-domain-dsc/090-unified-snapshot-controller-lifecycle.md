# Unified Snapshot Controller Lifecycle

**Status:** target model.  
**Normative source of truth:** [`../../spec/system-spec.md`](../../spec/system-spec.md).

## Lifecycle

Every `XxxSnapshotController` follows the same lifecycle:

1. Load its own `XxxSnapshot`.
2. Ensure its own `XxxSnapshotContent`.
3. Compute its own scope.
4. Create child snapshots when the domain model requires them.
5. Write the complete direct-child list to its own `status.childrenSnapshotRefs`.
6. Materialize its own scope through MCR/MCP.
7. Write `status.manifestCheckpointName` on its own content.
8. Read child `Ready` conditions from `childrenSnapshotRefs`.
9. Set own `Ready=True Completed` only when own MCP is ready and all children are ready.

A snapshot with no children is valid. Its `Ready` depends only on its own materialization.

## Rules

**Own materialization:** each controller materializes only its own scope. Missing optional domain resources do not make the snapshot invalid; an empty or minimal MCP is a valid materialization result.

**Child snapshot creation:** parent controller creates child snapshots and owns graph edges. Child controllers do not patch parent status.

**OwnerReference filtering:** a controller excludes resources owned by a different domain object from its own MCR. Those resources belong to a child snapshot or to namespace-level fallback.

**Readiness aggregation:** parent `Ready` is derived by reading every child listed in `status.childrenSnapshotRefs`. Child state is not copied into the parent list.

## Examples

**Disk:** `DemoVirtualDiskSnapshotController` owns disk-level materialization. A PVC owned by the disk is in disk own scope. The disk currently has no child snapshots.

**VM:** `DemoVirtualMachineSnapshotController` owns VM-level materialization and creates disk child snapshots for disk resources. PVCs owned by disks are delegated to disk snapshots.

**Namespace:** `NamespaceSnapshotController` owns namespace-level materialization. It uses DSC/registry only to discover top-level domain resources and create their child snapshots.

## Aggregated Read

Aggregated read is generic traversal over the content graph:

```text
start content
  -> status.manifestCheckpointName
  -> objects from MCP
  -> status.childrenSnapshotContentRefs
  -> child content nodes
```

It can start from any content node. Missing MCP, missing child content, registry gaps for heterogeneous content, and duplicate object identities fail the whole read.

## Invariants

- `childrenSnapshotRefs` is the complete parent-published graph for direct child snapshots.
- `childrenSnapshotRefs` contains `apiVersion`, `kind`, and `name`; namespace is implicit from the parent.
- Parent owns child graph edges.
- Child owns only its own snapshot, content, MCR/MCP, and `Ready`.
- `manifestCheckpointName` is required for materialized content traversal.
- `NamespaceSnapshot` differs only by DSC-based discovery, not by special root semantics.
- Generic/usecase code remains domain-agnostic.
