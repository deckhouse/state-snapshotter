# Unified Snapshot Controller Lifecycle

**Status:** design note for demo-domain-dsc implementation, non-normative.  
**Normative lifecycle and aggregated read contract:** [`../../spec/system-spec.md`](../../spec/system-spec.md) and [`../../spec/snapshot-aggregated-read.md`](../../spec/snapshot-aggregated-read.md).

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

**Own materialization:** each controller materializes only its own scope. Every DSC-registered `XxxSnapshot` in the parent-owned graph carries `spec.sourceRef` (`apiVersion`, `kind`, `name`) for the namespace-local source object it captures, while `spec.parentSnapshotRef` only describes the tree parent. Demo materialization captures the existing source domain object directly: `DemoVirtualDiskSnapshot` requires `sourceRef` to `DemoVirtualDisk`; `DemoVirtualMachineSnapshot` requires `sourceRef` to `DemoVirtualMachine`. Legacy source annotations, demo-specific source fields, fallback source resolution, and placeholder ConfigMap payloads are not part of the target model. Parent MCPs do not contain child manifests.

**Child snapshot creation:** parent controller creates child snapshots and owns graph edges. Child controllers do not patch parent status.

**OwnerReference filtering:** a controller excludes resources owned by a different domain object from its own MCR. Those resources belong to a child snapshot or to namespace-level fallback.

**Readiness aggregation:** parent `Ready` is derived by reading every child listed in `status.childrenSnapshotRefs`. Child state is not copied into the parent list.

## Examples

**Disk:** `DemoVirtualDiskSnapshotController` owns disk-level materialization. Its own MCP contains the source `DemoVirtualDisk`. The disk currently has no child snapshots.

**VM:** `DemoVirtualMachineSnapshotController` owns VM-level materialization and creates disk child snapshots for disk resources. Its own MCP contains the source `DemoVirtualMachine`; VM-owned disks are delegated to child `DemoVirtualDiskSnapshot` nodes.

**Namespace:** `NamespaceSnapshotController` owns namespace-level materialization for namespace-scoped allowlist resources only. It does not capture the cluster-scoped Kubernetes `Namespace` object. Empty own scope is represented by an empty MCP with `status.manifestCheckpointName` set. The `NamespaceSnapshot` CR remains namespaced; resolved target namespace is `NamespaceSnapshot.metadata.namespace`, and this change does not add `spec.namespace` / `spec.targetNamespace`. It uses DSC/registry only to discover top-level domain resources and create their child snapshots.

**E5 exclude:** root capture excludes objects already present in descendant content-node MCPs, including dedicated `XxxSnapshotContent` nodes reached through `childrenSnapshotContentRefs`.

## Aggregated Read

Aggregated read is generic traversal over the content graph. The normative API, traversal, duplicate handling, and MCP-local vs aggregated-read boundary are defined in [`../../spec/snapshot-aggregated-read.md`](../../spec/snapshot-aggregated-read.md).

## Invariants

- `childrenSnapshotRefs` is the complete parent-published graph for direct child snapshots.
- `childrenSnapshotRefs` contains `apiVersion`, `kind`, and `name`; namespace is implicit from the parent.
- Parent owns child graph edges.
- Child owns only its own snapshot, content, MCR/MCP, and `Ready`.
- Aggregated read is the only place where parent and child MCPs are combined.
- `manifestCheckpointName` is required for materialized content traversal; empty scope is represented by an empty MCP.
- `NamespaceSnapshot` differs only by DSC-based discovery, not by special root semantics.
- Generic/usecase code remains domain-agnostic.
