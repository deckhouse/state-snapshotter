# Unified Snapshot Controller Lifecycle

**Status:** design note for demo-domain-dsc implementation, non-normative.  
**Normative lifecycle and aggregated read contract:** [`../../spec/system-spec.md`](../../spec/system-spec.md) and [`../../spec/snapshot-aggregated-read.md`](../../spec/snapshot-aggregated-read.md).

## Lifecycle

Every `XxxSnapshotController` follows the same lifecycle:

1. Load its own `XxxSnapshot`.
2. Ensure its own `SnapshotContent`.
3. Compute its own scope.
4. Create child snapshots when the domain model requires them.
5. Write the complete direct-child list to its own `status.childrenSnapshotRefs`.
6. Materialize its own scope through MCR/MCP.
7. Write `status.manifestCheckpointName` on its own content.
8. Read child `Ready` conditions from `childrenSnapshotRefs`.
9. Set own `Ready=True Completed` only when own MCP is ready and all children are ready.

A snapshot with no children is valid. Its `Ready` depends only on its own materialization.

v0 content migration note: steps that mention `SnapshotContent` describe the current runtime.
The target model moves content creation/lifecycle to the common state-snapshotter layer and a single
cluster-scoped `storage.deckhouse.io/SnapshotContent`. Domain controllers keep ownership of
`XxxSnapshot` validation, `sourceRef`, child snapshot creation, and `Ready` aggregation until the
runtime migration is explicitly performed.

## Rules

**Own materialization:** each controller materializes only its own scope. Every DSC-registered `XxxSnapshot` in the parent-owned graph carries `spec.sourceRef` (`apiVersion`, `kind`, `name`) for the namespace-local source object it captures; the parent publishes tree membership through `status.childrenSnapshotRefs`, and ownerRef is lifecycle/back-reference only. Demo materialization captures the existing source domain object directly: `DemoVirtualDiskSnapshot` requires `sourceRef` to `DemoVirtualDisk`; `DemoVirtualMachineSnapshot` requires `sourceRef` to `DemoVirtualMachine`. Legacy source annotations, demo-specific source fields, fallback source resolution, and placeholder ConfigMap payloads are not part of the target model. Parent MCPs do not contain child manifests.

**Child snapshot creation:** parent controller creates child snapshots and owns graph edges. Child controllers do not patch parent status.

**Lifecycle ownerRefs:** any `XxxSnapshot` can be root. Its root `SnapshotContent` has `ownerRef -> root ObjectKeeper`. A root ObjectKeeper follows the root Snapshot via `spec.followObjectRef`. The root Snapshot itself is not owned by ObjectKeeper because Snapshot is namespaced and ObjectKeeper is cluster-scoped. A child Snapshot is owned by its parent Snapshot. A child `SnapshotContent` is owned by its parent `SnapshotContent`. `SnapshotContent` is never owned by a short-lived Snapshot and is not ownerless after graph convergence. MCP / future VSC artifacts are owned by the `SnapshotContent` they belong to.

**OwnerReference filtering:** a controller excludes resources owned by a different domain object from its own MCR. Those resources belong to a child snapshot or to namespace-level fallback.

**Readiness aggregation:** parent snapshot `Ready` mirrors bound `SnapshotContent Ready`; snapshot controllers publish `SnapshotContent.status.childrenSnapshotContentRefs` from `status.childrenSnapshotRefs`, and `SnapshotContentController` derives parent content readiness by validating those persisted child content refs. Child state is not copied into the parent list.

**Reference domain controller contract (current runtime):** a domain snapshot controller validates its own `spec.sourceRef`, creates its own cluster-scoped `SnapshotContent`, creates an MCR for its own source object, publishes request names / `GraphReady`, and mirrors bound content `Ready`. A domain parent controller (for example VM) also creates child snapshots for nested domain resources, sets ownerRefs on them, and publishes its own `status.childrenSnapshotRefs`. User spec errors are represented as `Ready=False` conditions (for example `InvalidSourceRef`) rather than reconcile errors, and must not create content, MCR, or child snapshots.

**Common content contract:** snapshot/domain controllers create/bind `SnapshotContent` and publish durable result refs into `SnapshotContent.status` (`manifestCheckpointName`, future `dataRef`, `childrenSnapshotContentRefs`). `SnapshotContentController` is a validator/lifecycle controller: it validates persisted refs, performs artifact ownerRef handoff, and owns only content readiness conditions. It is not a domain planner/executor: MCR/VCR/DataExport/VolumeSnapshot request creation stays in snapshot/domain controllers. Domain modules register source resource and snapshot CRD through DSC; content GVK is fixed to `storage.deckhouse.io/v1alpha1, Kind=SnapshotContent`.

**Not owned by a domain controller:** DSC `RBACReady`, RBAC creation, and parent status. Child controllers do not patch parent status. `SnapshotContent.status` is field-owned: result refs are publisher-owned by snapshot/domain controllers; `Ready` is validator-owned by `SnapshotContentController`.

**RBAC:** reference domain controllers intentionally omit kubebuilder RBAC markers. Required domain permissions are part of the controller contract, but they are granted externally by the Deckhouse RBAC controller/hook before DSC `RBACReady=True`, not generated from controller source comments.

## Examples

**Disk:** `DemoVirtualDiskSnapshotController` owns disk-level materialization. Its own MCP contains the source `DemoVirtualDisk`. The disk currently has no child snapshots.

**VM:** `DemoVirtualMachineSnapshotController` owns VM-level materialization and creates disk child snapshots for disk resources. Its own MCP contains the source `DemoVirtualMachine`; VM-owned disks are delegated to child `DemoVirtualDiskSnapshot` nodes.

**Namespace:** `SnapshotController` owns namespace-level materialization for namespace-scoped allowlist resources only. It does not capture the cluster-scoped Kubernetes `Namespace` object. Empty own scope is represented by an empty MCP with `status.manifestCheckpointName` set. The `Snapshot` CR remains namespaced; resolved target namespace is `Snapshot.metadata.namespace`, and this change does not add `spec.namespace` / `spec.targetNamespace`. It uses DSC/registry only to discover top-level domain resources and create their child snapshots.

**E5 exclude:** root capture excludes objects already present in descendant content-node MCPs, including dedicated `SnapshotContent` nodes reached through `childrenSnapshotContentRefs`.

## Aggregated Read

Aggregated read is generic traversal over the content graph. The normative API, traversal, duplicate handling, and MCP-local vs aggregated-read boundary are defined in [`../../spec/snapshot-aggregated-read.md`](../../spec/snapshot-aggregated-read.md).

## Invariants

- `childrenSnapshotRefs` is the complete parent-published graph for direct child snapshots.
- `childrenSnapshotRefs` contains `apiVersion`, `kind`, and `name`; namespace is implicit from the parent.
- Parent owns child graph edges.
- Child owns only its own snapshot, content, MCR/MCP, and `Ready`.
- Aggregated read is the only place where parent and child MCPs are combined.
- `manifestCheckpointName` is required for materialized content traversal; empty scope is represented by an empty MCP.
- `Snapshot` differs only by DSC-based discovery, not by special root semantics.
- Generic/usecase code remains domain-agnostic.
