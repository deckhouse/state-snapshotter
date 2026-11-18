---
title: "Working with Snapshots"
description: "How to create, inspect, download, upload, and restore namespace snapshots with Snapshot and d8."
weight: 20
---

The `state-snapshotter` module lets you create snapshots of namespace state and use the saved state for subsequent
reading or restoration.

## Snapshot overview

A snapshot is created after a [Snapshot](cr.html#snapshot-v1alpha1-spec) object appears in the cluster. A snapshot
records the desired state of a namespace at a specific point in time. The module captures manifests of user objects and
stores them in an immutable form for later reading or restoration. The namespace state is captured once. To create a new
snapshot, create a new Snapshot object.

## Snapshot operation

When a snapshot is created, the module:

1. **Discovers** all namespaced resources in the cluster through the Kubernetes discovery API instead of using a fixed
   list. This also allows arbitrary custom resources to be captured.
1. **Plans the capture** of namespace objects. It creates child snapshots for objects whose snapshots are managed by
   other modules. It selects the remaining objects according to the rules in [What gets captured](#what-gets-captured).
1. **Captures and stores** manifests and, where supported, volume data. Retention of snapshot data after a Snapshot is
   deleted is described in [Lifecycle and retention](#lifecycle-and-retention).
1. **Publishes the readiness status** of the snapshot. Status checks are described in
   [Tracking status](#tracking-status).

If any resource kind cannot be read, the module does not create a partial snapshot. The Snapshot does not reach
`Ready=True`, and capture is retried until the resource becomes readable. This happens if RBAC permissions have not
been applied yet or an aggregated API is unavailable.

The module records the cause in
[`status.captureState.domainSpecificController.message`](cr.html#snapshot-v1alpha1-status-capturestate-domainspecificcontroller-message)
and in a `Warning` event with reason `NamespacePlanUnreadable`.

### Snapshots managed by other modules

For some Kubernetes objects, a manifest alone is not sufficient to restore their state. The module managing those
objects can define its own snapshot kind and its contents. To do this, the module registers a mapping from the source
object kind to the snapshot kind using
[CustomSnapshotDefinition](cr.html#customsnapshotdefinition-v1alpha1-spec). `state-snapshotter` uses the definition only
when the `Accepted=True` and `AccessGranted=True` conditions are set for its current generation.

For example, the [`virtualization`](/modules/virtualization/) module can register VirtualMachineSnapshot as the snapshot kind for VirtualMachine
objects. During planning, `state-snapshotter` creates a child VirtualMachineSnapshot for each virtual machine (VM). The
`virtualization` module then determines the contents of that snapshot. This may include the VirtualMachine manifest,
manifests and data of attached VirtualDisks, manifests of Secrets with cloud-init settings, and manifests of other
related objects, such as disk attachment or IP address assignment objects. The exact contents depend on the
`virtualization` module's implementation.

If no CustomSnapshotDefinition for an object kind meets the conditions above, `state-snapshotter` does not create a
child snapshot for objects of that kind. The regular namespace capture can save the object's manifest, and related
objects are captured by the general rules: through their own child snapshots, if their kinds have such
definitions, or as namespace manifests and PVC volume snapshots. A saved VirtualMachine manifest therefore does not
guarantee that the full state of the VM is saved.

If an object is already covered by a child snapshot, such as a disk included in a VirtualMachineSnapshot, no separate
child snapshot is created for it. The root Snapshot saves namespace manifests not already captured by child snapshots.
Object selection also considers the exclusion label. For details, see
[Excluding objects from a snapshot](#excluding-objects-from-a-snapshot).

## What gets captured

Objects of the target namespace are captured unless they are excluded by built-in rules or explicitly. Objects with
their own snapshot logic are captured through child snapshots of the corresponding modules. How to exclude an object is
described in [Excluding objects from a snapshot](#excluding-objects-from-a-snapshot). There is no separate include list
of objects.

The tables below describe automatic selection of namespace manifests. Each module determines the contents of its child
snapshots, so these tables do not cover all of their capture rules.

**Captured (examples):**

| Category | Examples |
|----------|----------|
| Configuration and data objects | ConfigMap, Secret, Service, PersistentVolumeClaim (PVC), ServiceAccount, standalone Pod |
| Top-level workloads | Deployment, StatefulSet, DaemonSet, Job, CronJob |
| Custom resources | Namespaced custom resources |
| Networking and RBAC | Ingress, NetworkPolicy, Role, RoleBinding |

A standalone Pod is captured only if it has no controller owner. To save manifests, the module selects resource kinds
that support the `get`, `list`, and `watch` operations. Resources of aggregated APIs with persistent storage can also be
captured if they meet this condition. Secrets of type `kubernetes.io/service-account-token` and `ServiceAccount/default`
are not captured.

**Not captured:**

| Category | Examples | Reason for exclusion |
|----------|----------|----------------------|
| Controller-managed objects | Objects with `ownerReference.controller: true` | Recreated by the controller on restore |
| Kubernetes and CNI system objects | Event, Endpoints, Lease, CiliumEndpoint, `ConfigMap/kube-root-ca.crt` | Regenerated by the control plane or CNI, not part of user desired state |
| Computed objects | PodMetrics, NodeMetrics, and metrics API objects without `watch` | Not stored and cannot be restored |
| Snapshot machinery | CSI VolumeSnapshot and objects of registered snapshot kinds, regardless of who created them | Excluded to avoid capturing the snapshot machinery itself |
| Objects managed by Deckhouse Platform | Objects with the `heritage: deckhouse` or `heritage: multitenancy-manager` label, copies of Secrets made by the `secret-copier` module | Created and restored by Deckhouse Platform modules |
| Module machinery | The `state-snapshotter.deckhouse.io` group and the request and transfer kinds of the `storage-foundation.deckhouse.io` group | Used for internal operations |
| Backup operations | Backup, BackupPlan, and BackupRestore of the `backup` module (`storage.deckhouse.io` group) | Restored, they would run again against the restored namespace |

Controller-managed objects include, for example, ReplicaSets created by Deployments and Pods created by those
ReplicaSets. Computed objects without `watch` support include resources from the `metrics.k8s.io`,
`custom.metrics.k8s.io`, and `external.metrics.k8s.io` APIs. The `storage-foundation.deckhouse.io` request and
transfer kinds are VolumeCaptureRequest, VolumeRestoreRequest, DataExport, and DataImport.

Objects with the `heritage: deckhouse` or `heritage: multitenancy-manager` label are not captured: they are created by
Deckhouse Platform modules. The same applies to the copies that the `secret-copier` module makes of a Secret of the
`default` namespace in every other namespace, such as a wildcard TLS certificate: a Secret with the
`secret-copier.deckhouse.io/enabled` label outside the `default` namespace is not captured, whatever the value of the
label. Such a copy cannot be restored: an admission policy of the module allows only Deckhouse to create it, and the
module creates it in the restored namespace by itself. The source Secret in the `default` namespace is captured. All
other objects in the namespace, including objects configured on top of modules, are treated as desired state and
captured.

### Excluding objects from a snapshot

A snapshot covers the entire namespace state. To keep an object out of the snapshot, add the
`state-snapshotter.deckhouse.io/exclude` label **before** creating the snapshot. A capture is one-shot, so adding the
label later does not change an existing snapshot:

```shell
d8 k -n myns label configmap scratch-cache state-snapshotter.deckhouse.io/exclude=""
```

{{< alert level="info" >}}
The label's value is ignored: the object is excluded whatever the value is, including `false`. To include the object in
snapshots again, remove the label.
{{< /alert >}}

The exclusion label is considered at several levels of the snapshot tree, but different components are responsible for
excluding objects:

- **A regular object**, such as a ConfigMap or a Secret, is excluded from the captured namespace manifests.
- **An object with its own snapshot kind**, such as a VirtualMachine, is excluded during planning. If a
  VirtualMachine object carries the label, its VirtualMachineSnapshot is not created.
- **Exclusion of an object inside a child snapshot**, such as a VirtualDisk or a VM's Secret, is handled by the module
  forming that snapshot.
- **PVCs not covered by child snapshots of other modules** are captured neither as manifests nor as volume data if they
  carry the exclusion label. A user-created CSI VolumeSnapshot for such a PVC remains an ordinary CSI snapshot; the
  module does not adopt it.

`state-snapshotter` itself enforces exclusion for top-level objects with their own snapshot kind, namespace manifests,
and PVCs not covered by child snapshots of other modules.

**The exclusion is not inherited.** If a VirtualMachine carries the exclusion label, `state-snapshotter` does not create
a VirtualMachineSnapshot for it. Related VirtualDisks, PVCs, Secrets, and other objects are not excluded automatically.
They can still be captured through their own child snapshots or as namespace manifests and PVC volume snapshots. To
exclude them, add the label to each of them.

**Exclusions are recorded in the snapshot status.** The root Snapshot lists the excluded objects in
[`status.excludedRefs`](cr.html#snapshot-v1alpha1-status-excludedrefs): objects with their own snapshot kind, such as
VirtualMachine, objects excluded by modules inside their child snapshots, and the namespace objects and PVCs the root
Snapshot leaves out of its own manifests and volume data. Use this field to find which objects a snapshot does not
hold, for example the ConfigMaps, Secrets and PVCs a restored workload refers to. Only objects that would otherwise be
captured are listed: an object the built-in rules exclude anyway is not listed even if it carries the label, and
neither is a labeled PVC already covered by a captured child snapshot. The list is fixed when the snapshot finishes
planning: an object labeled or created after that is still not captured, but is not added to the list.

**Like the built-in exclusions, the label can only exclude objects from the capture.** There is no separate mechanism
for including objects in a snapshot: a snapshot contains the objects of the namespace except the excluded ones. To get
snapshots with different sets of objects, use different namespaces.

## Required permissions

Starting with Deckhouse Platform 1.78, namespace roles provide the following permissions:

| Role | Snapshot operations |
|---|---|
| `viewer` | View snapshots and their status |
| `manager` | Create and delete snapshots and transfer volume data |
| `admin` | Download snapshot manifests, upload and restore snapshots. Required for full snapshot download and upload with `d8` |

In the current role model (`ClusterAuthorizationRule` and `AuthorizationRule`), the access levels give the matching
permissions in every Deckhouse Platform version: `User` views snapshots and their status, `Editor` also creates and
deletes snapshots, and `Admin` also downloads snapshot manifests and uploads and restores snapshots.

The cluster-scoped objects of a snapshot tree are not part of these permissions. A SnapshotContent can be read with the
`ClusterEditor` access level and higher, and a ManifestCheckpoint with the `SuperAdmin` access level only (in the new
role model, with the roles of the `storage` subsystem). Work with a snapshot through the Snapshot, its status and its
subresources.

Downloading the manifests of a snapshot returns every object captured in the namespace, including the kinds the subject
cannot read directly, for example custom resources of other operators. Grant it only to subjects that may read all
objects of the namespace.

In Deckhouse Platform 1.76.9 through 1.77, the namespace use roles of the new role model include viewing, creating and
deleting snapshots only. To download, upload or restore snapshots there, bind the
`d8:use:capability:module:state-snapshotter:download_snapshots`, `d8:use:capability:module:state-snapshotter:upload_snapshots`
and `d8:use:capability:module:state-snapshotter:restore_snapshots` ClusterRoles in the namespace.

Volume data is transferred through the [`storage-foundation`](/modules/storage-foundation/) module, so downloading or
uploading a snapshot that contains volumes also requires its `transfer_volume_data` capability (a named set of module
permissions granted by a separate ClusterRole). The `Editor` and higher access levels of the current role model include
it, and so do the namespace `manager` and `admin` roles (in Deckhouse Platform 1.76.9 through 1.77, the `manager` and
`admin` use roles). To grant it to another subject, bind the
`d8:namespace-capability:storage-foundation:transfer_volume_data` ClusterRole in the namespace (in Deckhouse Platform
1.76.9 through 1.77, `d8:use:capability:module:storage-foundation:transfer_volume_data`).

The `d8 snapshot restore` command needs the following permissions:

- To apply application objects: `patch` on existing objects and `create` on objects to be created of the corresponding
  kinds. The command creates objects other than PVCs through Server-Side Apply and creates missing PVCs separately. The
  `update` permission alone is insufficient.
- For restore checks: `get` on PVCs, child volume snapshots, and PersistentVolumes.
- For the `--wait` flag: `get` on StorageClasses, `list` on StorageClasses to find the default one, and `list` on Pods
  and Events when checking volumes with the `WaitForFirstConsumer` mode.

Grant these permissions in the target namespace or at cluster scope depending on the object kind.

## Creating a snapshot

Create a Snapshot in the namespace whose state you want to capture:

```shell
d8 snapshot create my-namespace-snapshot -n my-app --wait
```

With the `--wait` flag, the command waits until the Snapshot reaches the `Ready` state. Without it, the command returns
right after the object is created, and you can track readiness as described in [Tracking status](#tracking-status).

The command creates a Snapshot equivalent to the following one. The same object can be created with `d8 k apply`:

```yaml
apiVersion: state-snapshotter.deckhouse.io/v1alpha1
kind: Snapshot
metadata:
  name: my-namespace-snapshot
  namespace: my-app
spec: {}
```

The Snapshot spec is immutable. An empty `spec: {}` starts dynamic capture of the state of the namespace where the
Snapshot is created. In the example, this is `my-app`. The default mode is
[`spec.mode`](cr.html#snapshot-v1alpha1-spec-mode) `Capture`. There is no need to specify the target namespace
separately.

{{< alert level="warning" >}}
Do not use the `-l`/`--selector` flag of `d8 snapshot create`: a Snapshot always captures the whole namespace, and the
API server rejects a Snapshot with a resource selector. To keep objects out of a snapshot, label them as described in
[Excluding objects from a snapshot](#excluding-objects-from-a-snapshot).
{{< /alert >}}

## Snapshot modes

The source of a Snapshot's content is set by the `spec.mode` field. It is immutable, like the rest of the spec:

| Mode | `spec` | Behavior |
|------|--------|----------|
| `Capture` (default) | `spec: {}`, or `spec.mode: Capture` explicitly | The module captures the current state of the namespace |
| `Import` | `spec.mode: Import` | The Snapshot gets its content from a previously downloaded snapshot. The objects of the namespace are not captured |

You do not need to create a Snapshot in `Import` mode yourself: `d8 snapshot upload` creates it, as described in
[Moving a snapshot to another namespace or cluster](#moving-a-snapshot-to-another-namespace-or-cluster).

## Tracking status

Use the following command to view snapshots in the target namespace:

```shell
d8 k -n my-app get snap
```

Example output:

```console {.nowrap-default}
NAME                    READY   REASON      CONTENT                        AGE
my-namespace-snapshot   True    Completed   nss-content-1f4b0c9d2a3e5f76   30s
```

To inspect a particular snapshot and its tree, use:

```shell
d8 snapshot describe my-namespace-snapshot -n my-app
```

Detailed information about the captured state is available in
[`status.conditions`](cr.html#snapshot-v1alpha1-status-conditions):

```shell
d8 k -n my-app get snap my-namespace-snapshot -o jsonpath='{.status.conditions}' | jq
```

A snapshot is ready when the `Ready` condition is `True` with reason `Completed`. Only this combination means that the
capture is finished and its result is stored. While the
[`status.boundSnapshotContentName`](cr.html#snapshot-v1alpha1-status-boundsnapshotcontentname) field is empty, the
snapshot cannot yet be downloaded. The field names a cluster-scoped SnapshotContent, which can be read with the
`ClusterEditor` access level and higher.

When the `Ready` condition is `False`, act according to its reason:

| Reason | What it means | What to do |
|---|---|---|
| `ArtifactMissing`, `ChildSnapshotLost`, `ChildrenFailed`, `CreateChildFailed`, `DomainCaptureFailed`, `DuplicateCoveredPVCUID`, `GraphPlanningFailed`, `ListFailed`, `ManifestCheckpointFailed`, `NamespaceNotFound`, `VolumeCaptureFailed` | The capture failed and will not be retried. The snapshot will not become ready | Read the condition message, eliminate the cause, delete the Snapshot and create a new one |
| `ChildSnapshotDeleted` | The object of a tree node was deleted. The stored data is kept, but the snapshot is degraded and does not recover by itself | Create a new snapshot if you need a complete one. To avoid this state, do not delete tree nodes directly, see [Delete protection](#delete-protection) |
| Any other reason | The capture is in progress | Wait. If the status does not change for a long time, check the condition message and the events of the Snapshot |

While a capture is waiting for a resource kind that cannot be read, for example because RBAC permissions have not been
applied yet, the cause is reported in
[`status.captureState.domainSpecificController.message`](cr.html#snapshot-v1alpha1-status-capturestate-domainspecificcontroller-message)
and in a `Warning` event with reason `NamespacePlanUnreadable`. The detailed meaning of each reason is given in
[Checking the snapshot state](backup_integration.html#checking-the-snapshot-state).

A volume snapshot that the CSI driver refuses, for example because the storage pool has no free space for it, does not
fail the capture: the CSI snapshot controller keeps retrying it, and the capture continues once the cause is eliminated.
While the capture waits, the condition message of the Snapshot names the VolumeSnapshot and quotes the error it reports
in `status.error.message`. When several volume snapshots wait, the message quotes one of them and lists the waiting
nodes.

## Restoring a snapshot

The `d8 snapshot restore` command applies captured application objects in the namespace that contains a ready Snapshot.
First check that the restored manifests can be applied without saving any changes:

```shell
d8 snapshot restore my-namespace-snapshot -n my-app --dry-run
```

With the `--dry-run` flag, the Kubernetes API server checks the manifests with validation and admission checks but does
not save them. The command does not show the differences between the current and the restored objects. Before restoring
without `--dry-run`, review the restored manifests and the existing objects separately. Then restore the application
objects:

```shell
d8 snapshot restore my-namespace-snapshot -n my-app --wait
```

The command prepares the captured manifests and applies them without their original service metadata.
Restored PVCs refer to the corresponding volume snapshots through `PersistentVolumeClaim.spec.dataSourceRef`.
The storage driver creates volumes with data from those snapshots.
With the `--wait` flag, the command waits for the corresponding PVCs to become `Bound`. The command does not check that
the application is ready.

Restoration cannot target a different namespace directly. To restore in another namespace or cluster, follow
[Moving a snapshot to another namespace or cluster](#moving-a-snapshot-to-another-namespace-or-cluster).
Do not create internal module objects such as VolumeRestoreRequest manually.

External clients can retrieve prepared manifests without applying them through the endpoint described in
[Reading a snapshot for restoration](backup_integration.html#reading-a-snapshot-for-restoration).
The endpoint only returns the objects, while `d8 snapshot restore` also applies them.

## Moving a snapshot to another namespace or cluster

A snapshot is moved through a directory on the machine where `d8` runs: you download the snapshot from the source
namespace, upload it into the target namespace, and restore it there. The downloaded directory can also be kept as a
copy outside the cluster: in that case, perform steps 1 and 2 now and the remaining steps when you need to restore.

To move a snapshot, follow these steps:

1. Download a ready snapshot:

   ```shell
   d8 snapshot download my-namespace-snapshot -n my-app -o ./my-snapshot
   ```

   The command downloads the whole snapshot tree, including captured manifests and volume data, rather than only the
   root node's objects. When run outside the cluster, the command chooses the way to access the volume exporter itself.
   The published endpoint requires token-based authentication.

1. Check the downloaded directory without connecting to the cluster:

   ```shell
   d8 snapshot local describe ./my-snapshot
   ```

   What this check covers and what it does not is described in
   [Checking an archive before uploading](snapshot_archives.html#checking-an-archive-before-uploading).

1. Upload the directory into the target namespace. For another cluster, use a context configured for it:

   ```shell
   d8 snapshot upload -n my-app-restored -i ./my-snapshot
   ```

   The target namespace must exist. Use a namespace without conflicting application objects. The `-i` flag accepts a
   directory, not a ZIP file. The command creates a Snapshot in `Import` mode and recreates its tree, including volume
   snapshots and volume data. The command does not capture the state of the target namespace and does not apply the
   application manifests.

1. Check that the uploaded Snapshot is ready as described in [Tracking status](#tracking-status):

   ```shell
   d8 snapshot describe my-namespace-snapshot -n my-app-restored
   ```

1. Restore the application objects in the target namespace as described in [Restoring a snapshot](#restoring-a-snapshot):

   ```shell
   d8 snapshot restore my-namespace-snapshot -n my-app-restored --dry-run
   d8 snapshot restore my-namespace-snapshot -n my-app-restored --wait
   ```

The archive structure and compatibility requirements are described in
[Working with snapshot archives](snapshot_archives.html). For developers of their own backup systems, the node API
contract and the volume size rules are described in [Backup integration](backup_integration.html).

## Lifecycle and retention

Snapshot lifecycle is defined by immutability and retention of stored content after the Snapshot object is deleted:

- **One-shot / immutable.** The spec is frozen at creation. `metadata.generation` never advances, and there is no
  re-capture.
- **Retention after deletion.** After the Snapshot is deleted, its stored data is kept for the period set in the
  [`snapshotTtlAfterDelete`](configuration.html#parameters-snapshotttlafterdelete) module parameter (30 days by
  default) and then removed. The Snapshot can instead be created with `spec.deletionPolicy: Delete`, see
  [Deletion policy](#deletion-policy).
- **No background load.** The module captures only in response to explicit requests. It runs no continuous background
  capture; the only scheduled work is removing data whose retention period has expired.

### Deleting a snapshot

Delete the root Snapshot to delete the whole snapshot tree:

```shell
d8 snapshot delete my-namespace-snapshot -n my-app --wait
```

With the `--wait` flag, the command waits until the module deletes the Snapshot and its tree. The module deletes the
child nodes of the tree together with the root Snapshot. The stored data is kept for the period set in the
`snapshotTtlAfterDelete` parameter, unless the Snapshot was created with `spec.deletionPolicy: Delete`. Do not delete
the child nodes directly: they are protected as
described in [Delete protection](#delete-protection).

### Deletion policy

The `spec.deletionPolicy` field is set at creation and cannot be changed. It sets what deleting the Snapshot does:

| Value | On deletion of the Snapshot |
|---|---|
| `Retain` (default) | The stored data is kept for the period set in the `snapshotTtlAfterDelete` parameter. Its volume snapshots keep occupying space on the storage for that period |
| `Delete` | The stored data is deleted right away, and the volume snapshots the module created for it are deleted from the storage |

Use `Delete` when the snapshot is copied out of the cluster, for example by a backup system, and the copy in the
cluster is not needed after the export:

```yaml
apiVersion: state-snapshotter.deckhouse.io/v1alpha1
kind: Snapshot
metadata:
  name: my-namespace-snapshot
  namespace: my-app
spec:
  deletionPolicy: Delete
```

The field is accepted only in `Capture` mode: deleting an imported Snapshot always deletes the loaded data. A
VolumeSnapshot you created in the namespace yourself keeps the deletion policy of its own class.

### Delete protection

The internal objects that make up a snapshot tree are protected from accidental direct deletion by an admission
delete-guard. Without it, deleting a child object, such as a managed CSI VolumeSnapshot, in the Deckhouse web UI puts
the root Snapshot into a degraded state. The stored data remains in place.

The protection works as follows:

- **What is protected.** Namespaced objects that represent tree nodes: child Snapshots and the managed CSI
  VolumeSnapshot. They carry the `state-snapshotter.deckhouse.io/delete-protected: "true"` label.
- **What is NOT protected.** The root Snapshot does not have this label: deleting the root Snapshot is the normal way
  to delete the whole tree. The label is not set on the following cluster-scoped objects either:
  - SnapshotContent
  - ManifestCheckpoint and its chunks
  - ObjectKeeper
  - The managed CSI VolumeSnapshotContent

  Deleting these objects directly is not blocked by the admission policy and can degrade the snapshot tree. Objects
  outside a snapshot tree are not covered by this protection.
- **Force-delete.** To delete a protected object directly, set the `deckhouse.io/allow-delete: "true"` annotation,
  then delete the object. A regular user cannot remove or change the label itself. The annotation is the only supported
  way to bypass the protection. The annotation can be removed as long as the object is not deleted.
- **Always enabled.** There is no module parameter that disables or weakens the protection. Use the annotation above
  for an exceptional direct deletion.

## Notes and limitations

Keep the following limitations in mind when using snapshots:

- A Snapshot captures objects only in its own namespace. The only cluster-scoped object included in the snapshot is
  the corresponding Namespace object.
- Manifests are stored as-is, including `status`. Service fields such as `status`, `resourceVersion` and `uid` are
  removed only when manifests are read for restore.
- Secret objects are stored as-is. The exception is service-account-token Secrets: they are not included in the
  snapshot, according to the capture rules.
