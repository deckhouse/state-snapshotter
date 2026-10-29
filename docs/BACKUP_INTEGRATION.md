---
title: "Backup integration"
description: "How to configure external-system access, create and download a backup, load it into a cluster, and restore state."
weight: 30
---

An external backup system uses the `state-snapshotter` module through the Kubernetes API server to create and upload
backups and restore state from an uploaded snapshot.

## Integration scope

This section lists what the integration described on this page covers and what it leaves to the external system or to
other documentation.

### What the integration covers

The integration consists of four operations:

1. [Create a snapshot](#creating-a-snapshot).
1. [Check that the snapshot succeeded](#checking-the-snapshot-state).
1. [Download the snapshot](#downloading-a-snapshot): node manifests and volume data.
1. [Load the snapshot back into a cluster](#loading-a-backup-into-a-cluster).

In addition, the module lets you read a snapshot subtree prepared for applying. This is described in
[Reading a snapshot for restoration](#reading-a-snapshot-for-restoration).

All operations are ordinary HTTP requests to the Kubernetes API server and ordinary Kubernetes objects. No client
library is required.

A snapshot is created by a Snapshot object and captures the namespace in which the object is created, once. The Snapshot
spec is immutable: a new capture requires a new Snapshot. The external system addresses only namespaced objects and
their subresources, with two exceptions: it reads the cluster-scoped CustomSnapshotDefinition objects to learn which
resources can be [captured individually](#snapshots-of-individual-resources), and it requests the API discovery of the
API group of a node kind, `GET /apis/{group}/{version}`, which every authenticated client may call without additional
roles.

### What the integration does not cover

The following is not part of the integration:

- **Applying restored objects.** The read described in
  [Reading a snapshot for restoration](#reading-a-snapshot-for-restoration) returns objects prepared for applying.
  The client applies the objects itself, chooses the order, and decides what to do with objects that already exist in
  the namespace. To restore the state without this logic, use the Deckhouse web UI or the `d8 snapshot restore`
  command.
- **The protocol for transferring volume bytes.** This page describes creating a DataExport or DataImport and tracking
  their state. The HTTP endpoints, the transfer modes, the `finished` call and the status fields with the address belong
  to the [`storage-foundation`](/modules/storage-foundation/) module and are described in
  [Exporting and importing volume data over HTTP](/modules/storage-foundation/usage.html#exporting-and-importing-volume-data-over-http).
- **The contents of nodes contributed by other modules.** A subtree created by another Deckhouse Platform module, for
  example virtual machines and their disks, is traversed, downloaded and loaded through the endpoint group of its kind,
  as described in [Snapshot tree and node API](#snapshot-tree-and-node-api) and [Nodes of kinds not known in
  advance](#nodes-of-kinds-not-known-in-advance), and its volume data is transferred in the same way as the data of a
  VolumeSnapshot node. What such nodes contain is described in the documentation of that module.
- **Renaming nodes while loading.** Load the tree under the node names it was captured with. Loading with other
  node names is not described.
- **The internal objects of the module.** The cluster-scoped objects that hold the captured data, their format and their
  cleanup can change in any version. The roles from
  [Configuring external-system access to the cluster](#configuring-external-system-access-to-the-cluster) do not grant
  access to them.
- **The storage format.** The operations above do not require a particular format: the external system can keep the
  transferred content in its own storage. The archive format shared by `d8` and the Deckhouse web UI is described in
  [Working with snapshot archives](snapshot_archives.html). If the external system stores snapshots in it, an archive
  it assembles can be uploaded with `d8` or the web UI, and an archive exported by them can be put into its storage as
  is.

## Configuring external-system access to the cluster

Create a dedicated ServiceAccount in every namespace the external system must access and bind the backup agent
ClusterRole to it with a RoleBinding. The RoleBinding limits the granted permissions to its namespace.

The ClusterRole name depends on the Deckhouse Platform version: use `d8:state-snapshotter:backup-agent` for version 1.78
and later, and `d8:use:capability:module:state-snapshotter:backup_agent` for versions 1.76.9 through 1.77.

Example for the `my-app` namespace:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: backup-agent
  namespace: my-app
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: backup-agent
  namespace: my-app
subjects:
  - kind: ServiceAccount
    name: backup-agent
    namespace: my-app
roleRef:
  kind: ClusterRole
  name: <BACKUP_AGENT_CLUSTER_ROLE>
  apiGroup: rbac.authorization.k8s.io
```

Replace `<BACKUP_AGENT_CLUSTER_ROLE>` with the ClusterRole name for the installed Deckhouse Platform version. This role
grants the `state-snapshotter` permissions to create and read snapshots, download and upload snapshot nodes, and read
snapshot contents for restoration. The role also grants the same permissions for snapshots of other kinds, such as
snapshots of virtual machines and their disks, if the modules that provide these snapshot kinds are enabled. Volume-data
transfer additionally requires the `storage-foundation` capability `transfer_volume_data` (a named set of module
permissions granted by a separate ClusterRole). If the external system needs only some operations, bind the individual
roles from the table:

| Rights | Deckhouse Platform 1.78 and later | Deckhouse Platform 1.76.9 through 1.77 |
|---|---|---|
| Read Snapshot, VolumeSnapshot and their statuses to walk the snapshot tree | `d8:namespace-capability:state-snapshotter:view_snapshots` | `d8:use:capability:module:state-snapshotter:view_snapshots` |
| Create and delete a Snapshot. Update is not supported because the Snapshot spec is immutable. Deleting a Snapshot deletes the whole snapshot tree | `d8:namespace-capability:state-snapshotter:manage_snapshots` | `d8:use:capability:module:state-snapshotter:manage_snapshots` |
| Read a node's manifests through the endpoints of both groups | `d8:namespace-capability:state-snapshotter:download_snapshots` | `d8:use:capability:module:state-snapshotter:download_snapshots` |
| Upload node manifests through the endpoints of both groups and create VolumeSnapshot objects during import | `d8:namespace-capability:state-snapshotter:upload_snapshots` | `d8:use:capability:module:state-snapshotter:upload_snapshots` |
| Read a snapshot subtree for restoration through the endpoints of both groups | `d8:namespace-capability:state-snapshotter:restore_snapshots` | `d8:use:capability:module:state-snapshotter:restore_snapshots` |
| Create DataExport and DataImport objects and transfer volume data through their HTTP endpoints | `d8:namespace-capability:storage-foundation:transfer_volume_data` | `d8:use:capability:module:storage-foundation:transfer_volume_data` |

The `transfer_volume_data` capability is provided by the [`storage-foundation`](/modules/storage-foundation/) module.
In both role schemes it is aggregated into the backup agent ClusterRole. Starting with Deckhouse Platform 1.78, the
capability is also part of the namespace `admin` role, but not of `manager`. In earlier versions it is not part of the
namespace user roles. If such roles are used instead of the backup agent ClusterRole, bind the capability separately
for volume-data transfer.

After an upgrade to Deckhouse Platform 1.78, bindings to the former ClusterRole
`d8:use:capability:module:state-snapshotter:backup_agent` keep working as a deprecated alias. Use
`d8:state-snapshotter:backup-agent` for new bindings. The `roleRef` field of a RoleBinding cannot be changed, so to
switch to the new name, create a new RoleBinding and then delete the old one.

CustomSnapshotDefinition objects, used to [discover the resources that can be captured individually](#snapshots-of-individual-resources),
are cluster-scoped, and a RoleBinding does not grant access to them. Bind the `d8:state-snapshotter:backup-agent-system`
ClusterRole with a ClusterRoleBinding. It only grants reading CustomSnapshotDefinition objects, and its name is the
same in all supported Deckhouse Platform versions:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: backup-agent-system
subjects:
  - kind: ServiceAccount
    name: backup-agent
    namespace: my-app
roleRef:
  kind: ClusterRole
  name: d8:state-snapshotter:backup-agent-system
  apiGroup: rbac.authorization.k8s.io
```

{{< alert level="warning" >}}
Bind the roles only in the namespaces the external system must access, and only the roles it uses. The full set allows
deleting snapshots and creating objects in a live namespace, so every binding beyond what is needed is one more way to
lose data.
{{< /alert >}}

Obtain a ServiceAccount token, for example:

```shell
d8 k -n my-app create token backup-agent --duration=24h
```

Send the token in the `Authorization: Bearer <TOKEN>` header and validate the API server certificate against the cluster
CA. Responses can contain Secrets from the snapshot, so the external client must protect the received data in the same
way as data from the source namespace.

## Snapshot API

The snapshot API is intended for reading and transferring snapshot contents through aggregated Kubernetes API
endpoints. External backup systems and other clients that need to work with snapshot contents directly use it.

To create snapshots and track their state, use Snapshot.

### Snapshot object operations

Create the root Snapshot and check its state through the Kubernetes resource API:

```text
POST /apis/state-snapshotter.deckhouse.io/v1alpha1/namespaces/{namespace}/snapshots?fieldValidation=Strict
GET  /apis/state-snapshotter.deckhouse.io/v1alpha1/namespaces/{namespace}/snapshots/{name}
```

Send a Snapshot JSON object with the `Content-Type: application/json` header in the `POST` request.
`{namespace}` is the namespace to capture and `{name}` is the name from `metadata.name`.
A Snapshot with `spec: {}` captures the namespace state once.
A Snapshot with `spec.mode: Import` accepts externally supplied data.
The spec is immutable after creation. Readiness is checked through the `status.conditions` field of the created object.

When creating a Snapshot, use the `fieldValidation=Strict` parameter in the `POST` request so that the API server
rejects unknown and repeated fields. Without strict validation, the API server discards a field with a typo in its
**name** (`mod: Import` instead of `mode: Import`). The Snapshot then captures the live namespace in the default
`Capture` mode. An invalid **value**, such as `spec.mode: Imprt`, is rejected by the CRD enum validation. After
creation, read back the stored `spec.mode` value and check `status.conditions`.

### Snapshot tree and node API

A snapshot is a tree of linked nodes. The root node is a Snapshot in the snapshot namespace. Child nodes are created
during state capture and are listed in
[`status.childrenSnapshotRefs`](cr.html#snapshot-v1alpha1-status-childrensnapshotrefs). All nodes in the tree are in the
same namespace, so a child reference contains only `apiVersion`, `kind`, and `name`. Nodes that contain the data of
regular PersistentVolumeClaim (PVC) objects are represented by VolumeSnapshot objects.

Each node contains its own captured manifests and may contain the data of one volume. Downloading is node-by-node:
`manifests-download` returns only the manifests of the addressed node, and the client traverses child nodes through
`status.childrenSnapshotRefs`. The restoration endpoint `manifests-with-data-restoration`, by contrast, returns the
whole subtree recursively.

Snapshot and VolumeSnapshot nodes use different API groups:

| Addressed object | API group | Version | Resource |
|---|---|---|---|
| Snapshot object | `state-snapshotter.deckhouse.io` | `v1alpha1` | `snapshots` |
| Snapshot subresources | `subresources.state-snapshotter.deckhouse.io` | `v1alpha1` | `snapshots` |
| VolumeSnapshot object | `snapshot.storage.k8s.io` | `v1` | `volumesnapshots` |
| VolumeSnapshot subresources | `subresources.storage-foundation.deckhouse.io` | `v1` | `volumesnapshots` |

The following node operations are available:

- `manifests-download` (`GET`): get the node's own manifests as captured.
- `manifests-and-children-refs-upload` (`POST`): upload the node's own manifests and the references to its direct child
  nodes during import.
- `manifests-with-data-restoration` (`GET`): get a subtree with manifests prepared for applying.

For nodes contributed by other Deckhouse Platform modules, the endpoint group is determined by the `apiVersion` of the
child node reference. For virtual machines, their disks, and other such nodes, the `subresources.` prefix is added to
the node's API group, and the version stays the same as the node's. The API resource of the node kind is found through
API discovery, as described in [Nodes of kinds not known in advance](#nodes-of-kinds-not-known-in-advance).

{{< alert level="warning" >}}
This rule does not apply to VolumeSnapshot. VolumeSnapshot belongs to the `snapshot.storage.k8s.io` API group, but its
endpoints are in the `subresources.storage-foundation.deckhouse.io/v1` group listed in the table above.
{{< /alert >}}

The following paths use the same node name and namespace but different API groups. The HTTP methods are part of the
contract:

```text
GET  /apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/{namespace}/snapshots/{name}/manifests-download
POST /apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/{namespace}/snapshots/{name}/manifests-and-children-refs-upload
GET  /apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/{namespace}/snapshots/{name}/manifests-with-data-restoration
GET  /apis/subresources.storage-foundation.deckhouse.io/v1/namespaces/{namespace}/volumesnapshots/{name}/manifests-download
POST /apis/subresources.storage-foundation.deckhouse.io/v1/namespaces/{namespace}/volumesnapshots/{name}/manifests-and-children-refs-upload
GET  /apis/subresources.storage-foundation.deckhouse.io/v1/namespaces/{namespace}/volumesnapshots/{name}/manifests-with-data-restoration
```

`{namespace}` is the namespace of the addressed node and `{name}` is that node's object name.

Request and response formats are specified in [Request and response bodies](#request-and-response-bodies), and error
codes in [Errors and retries](#errors-and-retries).

The cluster-scoped objects used to store snapshot content are an implementation detail. Read and transfer snapshots
through namespaced objects and their subresources instead of accessing SnapshotContent, ManifestCheckpoint, or other
internal objects directly.

## Creating a backup

To create a backup, [create a snapshot](#creating-a-snapshot), [check its state](#checking-the-snapshot-state), and
[download its contents](#downloading-a-snapshot).

### Creating a snapshot

Create a new Snapshot in the namespace whose state must be captured. Its spec is immutable and a snapshot captures that
namespace once. An empty spec selects `Capture` mode:

```yaml
apiVersion: state-snapshotter.deckhouse.io/v1alpha1
kind: Snapshot
metadata:
  name: my-namespace-snapshot
  namespace: my-app
spec: {}
```

Objects with the `state-snapshotter.deckhouse.io/exclude` label are excluded if the label is set before capture.
The selection rules are described in [What gets captured](user_guide.html#what-gets-captured).

### Checking the snapshot state

Read the Snapshot `status.conditions` through the Kubernetes API. Treat the snapshot as successfully created only when
the condition is `Ready=True` with reason `Completed`. The table lists the terminal and recoverable reasons while
`Ready=False`:

| Reason | Class | Meaning |
|---|---|---|
| `ArtifactMissing` | Terminal | The artifact with the captured volume's data is missing or being deleted. The volume data cannot be restored from this snapshot |
| `ChildSnapshotLost` | Terminal | A node of the tree is irrecoverably lost |
| `ChildrenFailed` | Terminal | The capture of a child node failed with an error that is not retried. The cause is given in that node's reason |
| `CreateChildFailed` | Terminal | A node of the tree could not be created |
| `DomainCaptureFailed` | Terminal | The capture of a subtree contributed by another module failed. The cause returned by that module is given in the condition message |
| `DuplicateCoveredPVCUID` | Terminal | The same volume got into the capture plan twice, so the capture was refused |
| `GraphPlanningFailed` | Terminal | The snapshot tree could not be planned |
| `ListFailed` | Terminal | The list of namespace objects to capture could not be built because of an error that is not retried. An unreadable resource kind, for example without RBAC permissions, and temporary API server errors do not cause this reason. In these cases the capture waits and is retried |
| `ManifestCheckpointFailed` | Terminal | The captured manifests could not be stored |
| `NamespaceNotFound` | Terminal | The namespace being captured does not exist |
| `VolumeCaptureFailed` | Terminal | The volume data could not be captured |
| `ChildSnapshotDeleted` | Recoverable | The object of a tree node was deleted, but the stored data is not lost. The capture remains complete, but the node is no longer reachable in the namespace. Access to the node can only be restored manually |

Act according to the class of the reason:

- **Terminal.** The snapshot will no longer reach `Ready=True`. Stop polling and report the failure. To capture again,
  create a new snapshot.
- **Recoverable.** The captured data is preserved, but the object is in a degraded state, and further polling will not
  change it. The state can only be restored manually.
- **Values not listed in the table.** These are intermediate states or values returned by another module's controller.
  New values may appear without notice. Keep polling while at least one of the following changes:

  - The reason or the condition message
  - The `status.boundSnapshotContentName` field
  - The list of child nodes

  If nothing changes, stop polling after a timeout of your own and report the failure.

### Downloading a snapshot

To download it, traverse the snapshot tree through
[`status.childrenSnapshotRefs`](cr.html#snapshot-v1alpha1-status-childrensnapshotrefs) and get each node's own manifests
through `manifests-download`. The addressing rules and endpoint methods are listed in
[Snapshot tree and node API](#snapshot-tree-and-node-api).

#### Reading node manifests

Captured manifests are read through the module's aggregated subresource. The raw data of the internal storage chunks is
not accessible directly. The following command gets the manifests of the root Snapshot node and prints their count:

```shell
d8 k get --raw "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/my-app/snapshots/my-namespace-snapshot/manifests-download" | jq 'length'
```

The `manifests-download` endpoint returns the manifests of its own node only and does not walk the snapshot tree. The
manifests are returned as they were captured, including `status` and other runtime fields.

The rest of the tree is read one request per node. The
[`status.childrenSnapshotRefs`](cr.html#snapshot-v1alpha1-status-childrensnapshotrefs) field of the node you read
contains references to its direct child nodes. Each child node is read the same way, through the aggregated API group of
its own kind, and its own `status.childrenSnapshotRefs` lets you continue the walk.

The API group and version of a child node depend on its kind, not on the API group of the parent node. For example, a
child node of kind CSI VolumeSnapshot that holds the data of a plain PVC is served by the
[`storage-foundation`](/modules/storage-foundation/) module. It uses the group
`subresources.storage-foundation.deckhouse.io`, version `v1` and resource `volumesnapshots`. The following command gets
the manifests of a child VolumeSnapshot node and prints their count:

```shell
d8 k get --raw "/apis/subresources.storage-foundation.deckhouse.io/v1/namespaces/my-app/volumesnapshots/<NODE_NAME>/manifests-download" | jq 'length'
```

where `<NODE_NAME>` is the name of the child node from the parent node's `status.childrenSnapshotRefs`.

The endpoint names are the same for Snapshot nodes and volume nodes, but the API group, version and resource differ.
Snapshot nodes use version `v1alpha1`, volume nodes use `v1`. A path with the wrong version is not served.

#### Nodes of kinds not known in advance

Besides Snapshot and VolumeSnapshot, a snapshot tree can contain nodes of the snapshot kinds of other Deckhouse Platform
modules, such as snapshots of virtual machines and their disks. A module can add a new snapshot kind in any release, and
from then on namespace snapshots contain nodes of that kind, with no change on the client side. Do not keep a list of
supported kinds in the client: process every child node by the same rules, using only the `apiVersion`, `kind` and
`name` of its reference in `status.childrenSnapshotRefs`.

For each child node reference:

1. Find the API resource of the node kind. Request the API discovery of the API group and version from the `apiVersion`
   of the reference, `GET /apis/{group}/{version}`, and take the `name` of the entry in `resources` whose `kind` equals
   the `kind` of the reference and whose `name` contains no `/`. An entry with a `/`, such as `{resource}/status`, is a
   subresource of the same kind. If the API group is not served or no entry matches, the kind is not served in the
   cluster: stop the download and report the node.
2. Read the node object: `GET /apis/{group}/{version}/namespaces/{namespace}/{resource}/{name}`. Its
   `status.childrenSnapshotRefs` field lists the child nodes of this node, and its `status.data` field is used in
   step 4.
3. Get the manifests of the node through `manifests-download` in the endpoint group of the node, as described in
   [Snapshot tree and node API](#snapshot-tree-and-node-api), with the API resource found in step 1.
4. If the node has `status.data`, transfer its volume data as described in [Transferring volume
   data](#transferring-volume-data).
5. Process the child nodes from step 2 in the same way.

If one of these requests fails for a node with an error that [Errors and retries](#errors-and-retries) does not tell you
to retry, stop the download and report the node: the downloaded snapshot is incomplete without it.

The following example processes a child node of a fictional kind `DatabaseSnapshot` from the API group
`databases.example.com`, version `v1alpha1`. Its reference in `status.childrenSnapshotRefs` of the parent node:

```yaml
status:
  childrenSnapshotRefs:
    - apiVersion: databases.example.com/v1alpha1
      kind: DatabaseSnapshot
      name: <NODE_NAME>
```

The commands take only the values of the reference, so they work the same way for a node of any kind except
VolumeSnapshot:

```shell
NAMESPACE=my-app
API_VERSION=databases.example.com/v1alpha1
KIND=DatabaseSnapshot
NAME="<NODE_NAME>"

GROUP="${API_VERSION%/*}"
VERSION="${API_VERSION#*/}"

# Step 1: the API resource of the node kind, databasesnapshots in this example.
RESOURCE=$(d8 k get --raw "/apis/${GROUP}/${VERSION}" \
  | jq -r --arg kind "$KIND" '.resources[] | select(.kind == $kind and (.name | contains("/") | not)) | .name')

# Step 2: the child nodes and the volume data of the node.
d8 k get --raw "/apis/${GROUP}/${VERSION}/namespaces/${NAMESPACE}/${RESOURCE}/${NAME}" \
  | jq '{children: .status.childrenSnapshotRefs, data: .status.data}'

# Step 3: the manifests of the node.
d8 k get --raw "/apis/subresources.${GROUP}/${VERSION}/namespaces/${NAMESPACE}/${RESOURCE}/${NAME}/manifests-download" | jq 'length'
```

Replace `<NODE_NAME>` with the `name` of the reference. If `data` in the output of step 2 is not `null`, create a
DataExport whose `targetRef` contains the group `databases.example.com`, the kind `DatabaseSnapshot` and the name of
the node. Repeat the commands for each reference in `children`, with the values of that reference.

### Transferring volume data

A node contains volume data if it has the `status.data` field, whatever its kind: a VolumeSnapshot node or a node of
another module, such as a snapshot of a virtual machine disk. Manifests are not enough for such a node: transfer the
volume data through a DataExport from the `storage-foundation` module. Create a DataExport for the node, wait until it
is ready, and download the data from the address in the DataExport status. In `DataExport.spec.targetRef`, specify the
node itself, not the source PVC: the API group from the `apiVersion` of the node, its `kind` and its `name`. Example for
a VolumeSnapshot node:

```yaml
apiVersion: storage-foundation.deckhouse.io/v1alpha1
kind: DataExport
metadata:
  name: export-my-volume-node
  namespace: my-app
spec:
  ttl: 15m
  publish: false
  targetRef:
    group: snapshot.storage.k8s.io
    kind: VolumeSnapshot
    name: my-volume-node
```

The export addresses, the publication mode for access from outside the cluster and certificate validation are described
in
[Exporting and importing volume data over HTTP](/modules/storage-foundation/usage.html#exporting-and-importing-volume-data-over-http)
in the `storage-foundation` module documentation.

Save the volume size, mode and StorageClass from the node's `status.data.size`, `status.data.volumeMode` and
`status.data.storageClassName` fields: you will need them when loading the backup. The rules for determining the size
are described in [Volume size](#volume-size).

{{< alert level="warning" >}}
If the `storage-foundation` module is not enabled, the volume data of nodes cannot be downloaded or uploaded. Namespace
state snapshots without volumes remain available.
{{< /alert >}}

### Volume size

The size of a captured volume is needed when a snapshot is loaded into a cluster and when an archive is assembled. Take
it from the `status.data.size` field of the node that contains the volume data. The field is present on a VolumeSnapshot
node and on a node contributed by another module if that node has a volume of its own. Nodes without volume data, such
as the root Snapshot, have no `status.data` field.

The value is in Kubernetes quantity format and defines the capacity of the volume on the receiving side. The size is
compared, not its notation: for example, `10Gi` and `10240Mi` are considered equal.

Do not determine the size from the `PersistentVolumeClaim.spec.resources.requests.storage` field of the downloaded
manifest. It is the requested size of the volume, and it may be smaller than the provisioned one.

Save the size when you download the node, and when you load the backup, take the saved value: the source node may no
longer exist by then. In the archive format, the saved value is `volumes[].size` in `snapshot.yaml`. The value is
passed on unchanged and is never recomputed:

```text
status.data.size of the node  →  volumes[].size in snapshot.yaml  →  DataImport.spec.storageParams.size
```

On a VolumeSnapshot, the `status.data` field is a Deckhouse Platform extension that is absent from the CSI API. It
contains data from the SnapshotContent, which the client cannot access, and lets you get the description of a captured
volume with an ordinary `GET` request in the snapshot's namespace.

Archive assembly is described in [Working with snapshot archives](snapshot_archives.html).

### Snapshots of individual resources

Besides a whole namespace, other Deckhouse Platform modules can capture a single resource, for example a virtual
machine. Such a module registers the pair "resource kind → snapshot kind" with a cluster-scoped CustomSnapshotDefinition
object. Read the list of these objects to learn which resources can be captured individually, instead of hardcoding the
kinds in the external system:

```text
GET /apis/state-snapshotter.deckhouse.io/v1alpha1/customsnapshotdefinitions
```

The read requires the role described in
[Configuring external-system access to the cluster](#configuring-external-system-access-to-the-cluster).

| Field | Meaning |
|---|---|
| `CustomSnapshotDefinition.spec.source.apiVersion`, `CustomSnapshotDefinition.spec.source.kind` | The kind of the resource that can be captured |
| `CustomSnapshotDefinition.spec.apiVersion`, `CustomSnapshotDefinition.spec.kind` | The kind of its snapshot |

Use only the definitions whose `status.conditions` contain `Ready=True`. The other fields of the object are not part of
the integration. Example of a definition registered by the virtualization module:

```yaml
apiVersion: state-snapshotter.deckhouse.io/v1alpha1
kind: CustomSnapshotDefinition
metadata:
  name: virtualization-virtual-machine-snapshot
spec:
  apiVersion: virtualization.deckhouse.io/v1alpha2
  kind: VirtualMachineSnapshot
  requiresDataArtifact: false
  source:
    apiVersion: virtualization.deckhouse.io/v1alpha2
    kind: VirtualMachine
status:
  conditions:
    - type: Ready
      status: "True"
```

To capture a resource, create an object of the snapshot kind in the namespace of the resource. The API resource of the
snapshot kind, needed in the request path, is found as described in [Nodes of kinds not known in
advance](#nodes-of-kinds-not-known-in-advance), with the group, version and kind taken from
`CustomSnapshotDefinition.spec.apiVersion` and `CustomSnapshotDefinition.spec.kind`. Every snapshot kind registered with
a CustomSnapshotDefinition accepts the same spec: the `mode` field with the value `Capture` and the `sourceRef` field
with the `apiVersion`, `kind` and `name` of the resource from the same namespace. The other spec fields are optional,
and their meaning is described in the documentation of the module that owns the snapshot kind:

```yaml
apiVersion: virtualization.deckhouse.io/v1alpha2
kind: VirtualMachineSnapshot
metadata:
  name: my-vm-snapshot
  namespace: my-app
spec:
  mode: Capture
  sourceRef:
    apiVersion: virtualization.deckhouse.io/v1alpha2
    kind: VirtualMachine
    name: my-vm
```

Use the `fieldValidation=Strict` parameter when creating it, as for a Snapshot. The created object is the root node of
its own snapshot tree. Track its state and download it in the same way as a Snapshot: the condition `Ready=True` with
reason `Completed` in `status.conditions` means a completed capture, the child nodes are listed in
`status.childrenSnapshotRefs`, and the node endpoints are addressed as described in
[Snapshot tree and node API](#snapshot-tree-and-node-api).

A snapshot of an individual resource is loaded back in the same way as a namespace snapshot, as described in
[Loading a backup into a cluster](#loading-a-backup-into-a-cluster), except that the root node is an object of the
snapshot kind with `spec.mode: Import` instead of a Snapshot. The `d8 snapshot download` command does not download
such a snapshot yet, and `d8 snapshot upload` accepts only a Snapshot as the root of a whole archive: a node with volume data can be
uploaded as a root of its own only with `--node`.

## Loading a backup into a cluster

Load the saved state into the target namespace before restoring it. In `Import` mode, the Snapshot is built from data
supplied by the external system and does not capture the live namespace.

1. Create the root Snapshot with `spec.mode: Import`:

   ```yaml
   apiVersion: state-snapshotter.deckhouse.io/v1alpha1
   kind: Snapshot
   metadata:
     name: my-namespace-snapshot
     namespace: my-app-restored
   spec:
     mode: Import
   ```

   For the create request and field validation, see [Snapshot object operations](#snapshot-object-operations).
   Before creating child nodes, verify that the stored `spec.mode` is `Import`.

1. Create objects for child nodes in the same namespace. For volume nodes, create a VolumeSnapshot in import mode and
   link it to the parent node through `ownerReferences`:

   ```yaml
   apiVersion: snapshot.storage.k8s.io/v1
   kind: VolumeSnapshot
   metadata:
     name: my-volume-node
     namespace: my-app-restored
     ownerReferences:
       - apiVersion: state-snapshotter.deckhouse.io/v1alpha1
         kind: Snapshot
         name: my-namespace-snapshot
         uid: <ROOT_SNAPSHOT_UID>
   spec:
     mode: Import
   ```

   Replace `<ROOT_SNAPSHOT_UID>` with the UID of the parent node. Through the `ownerReferences` entry, the Kubernetes
   garbage collector deletes the child nodes and their data when the root Snapshot is deleted. The deletion happens
   without confirmation and cannot be undone. A node without this reference is not deleted together with the tree and
   remains in the namespace.

   Leave the `VolumeSnapshot.spec.source` field empty, as in the example: the volume data is uploaded through a
   DataImport, so no data source is specified.

   {{< alert level="warning" >}}
   A VolumeSnapshot with an empty `VolumeSnapshot.spec.source` is supported only with the extended VolumeSnapshot CRD
   and `snapshot-controller` from the `storage-foundation` module. After the import is complete, the module binds the
   VolumeSnapshot to the VolumeSnapshotContent created during the import and records its name in
   `status.boundVolumeSnapshotContentName`. With the upstream `snapshot-controller`, which does not support `Import`
   mode, this scenario does not work.
   {{< /alert >}}

   For a node of another module, create an object of its kind in the same way: with `spec.mode: Import` and an
   `ownerReferences` entry pointing to its parent node. Every child node needs this entry, not only for deletion: the
   module uses it to link the node to its parent. A child node without it is loaded as a separate root, and its parent
   never reaches `Ready=True`. The API resource of the node kind, needed in the request path, is found as described in
   [Nodes of kinds not known in advance](#nodes-of-kinds-not-known-in-advance).

1. Upload each node's manifests and the references to its direct child nodes to `manifests-and-children-refs-upload`.
   The request body contains the JSON arrays `manifests` and `childRefs`. For a node without child nodes, `childRefs`
   is empty. The endpoint of the root Snapshot:

   ```text
   POST /apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/{namespace}/snapshots/{name}/manifests-and-children-refs-upload
   ```

   For a VolumeSnapshot, use the `subresources.storage-foundation.deckhouse.io/v1` group and the `volumesnapshots`
   resource. For a node of another module, use the API group of the node with the `subresources.` prefix, as described
   in [Snapshot tree and node API](#snapshot-tree-and-node-api), and the resource of its kind, found as described in
   [Nodes of kinds not known in advance](#nodes-of-kinds-not-known-in-advance). The request format is
   given in [Request and response bodies](#request-and-response-bodies). If the node
   is not bound to the stored content yet, the server may return `409`. Retry the request after a delay. Uploading the
   same data again is allowed.

1. For each node with volume data, create one DataImport in the target namespace, then transfer its bytes through the
   `storage-foundation` HTTP API. For example, for a VolumeSnapshot child named `my-volume-node`:

   ```yaml
   apiVersion: storage-foundation.deckhouse.io/v1alpha1
   kind: DataImport
   metadata:
     name: import-my-volume-node
     namespace: my-app-restored
   spec:
     mode: PopulateData
     ttl: 1h
     snapshotRef:
       apiVersion: snapshot.storage.k8s.io/v1
       kind: VolumeSnapshot
       name: my-volume-node
     storageParams:
       storageClassName: my-storage-class
       size: 10Gi
       volumeMode: Block
   ```

   Requirements for the DataImport:

   - `DataImport.spec.snapshotRef` points to the imported node, not to the source PVC. For a node of another module,
     specify its `apiVersion`, `kind` and `name`.
   - Create only one DataImport per node. Several DataImports for the same node prevent the node from being linked to
     the imported data.
   - The `storageClassName` and `size` fields are required. Take the size saved when downloading the node, as described
     in [Volume size](#volume-size).
   - Preserve the original `volumeMode` (`Block` or `Filesystem`): take the value saved from the captured node's
     `status.data.volumeMode`, in the archive format `volumes[].volumeMode` in `snapshot.yaml`. The field is optional
     in the DataImport schema, but without it `Filesystem` is used. **For a block volume, always set `Block`
     explicitly.**
   - Choose a StorageClass in the target cluster that supports snapshots and has the
     `storage.deckhouse.io/volumesnapshotclass` annotation linking it to a VolumeSnapshotClass. The original
     StorageClass is the value saved from the captured node's `status.data.storageClassName`, in the archive format
     `volumes[].storageClassName` in `snapshot.yaml`. If it is unavailable, choose a suitable one. The configuration is
     described in the [storage-foundation documentation](/modules/storage-foundation/).

   After the data transfer, call the `finished` endpoint of the DataImport. Transferring the last byte does not by
   itself complete the import.
   Wait until `DataImport.status.phase` becomes `Completed`, then check the root Snapshot.
   The details of the HTTP upload, TTL and error handling are given in
   [Exporting and importing volume data over HTTP](/modules/storage-foundation/usage.html#exporting-and-importing-volume-data-over-http).

1. Wait until the root Snapshot reaches `Ready=True` with reason `Completed`.

Before loading an archive compatible with `d8`, validate it according to
[Working with snapshot archives](snapshot_archives.html).

## Reading a snapshot for restoration

When integrating restoration through the API, request prepared manifests with `manifests-with-data-restoration`.
The response contains the manifests and volume references needed for restoration. Do not create internal module
objects such as VolumeRestoreRequest manually.

`manifests-download` returns objects as they were captured, including `status` and runtime metadata, and is not intended
to be applied directly. Use it to inspect or download the contents of a snapshot.

To get objects ready for applying, read the snapshot through the restoration endpoint
`/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/{namespace}/snapshots/{name}/manifests-with-data-restoration`
(`GET`). By default, the endpoint is **recursive**: it returns the addressed node and its descendants in one response.
Unlike `manifests-download`, it prepares the manifests for restoration. For example:

```shell
d8 k get --raw "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/my-app/snapshots/my-namespace-snapshot/manifests-with-data-restoration" | jq .
```

Before they are returned, the manifests are prepared for applying:

- Runtime metadata (`uid`, `resourceVersion`, `ownerReferences`, `managedFields` and others) is removed.
- `status` is removed.
- Fields assigned by the cluster are removed, for example `clusterIP` on a Service and `nodeName` on a Pod.
- Objects are addressed into the namespace of the requested snapshot.
- Each PVC references its corresponding VolumeSnapshot through `PersistentVolumeClaim.spec.dataSourceRef`.

The endpoint cannot exclude volume snapshot references from the prepared manifests. A GET request only
reads and prepares the objects; it neither applies the manifests nor restores volume data.

The default subtree read crosses module boundaries, so it is enough to call the endpoint once for the Snapshot node.
Child nodes, including VolumeSnapshots, are processed by the corresponding modules and returned in the same response,
so you do not need to walk the tree yourself. The result is formed as a whole. If at least one node of the subtree is
not ready, the request fails without returning partial content.

All objects in the response are addressed into the namespace of the requested Snapshot, and a different target namespace
cannot be chosen. A request with a different namespace returns `400`. To restore the contents into another namespace,
first download the snapshot, load it into the required namespace and read it there. Volume data cannot be re-addressed
separately either. `PersistentVolumeClaim.spec.dataSourceRef` carries no namespace, so the PVC looks for the referenced
VolumeSnapshot in its own namespace, while the VolumeSnapshot stays in the snapshot's namespace.

The response is a plain JSON array of objects, not a `List` object or a YAML stream. The client must process the
array elements and apply them itself instead of passing the whole response body directly to `apply -f -`.

The module only provides the prepared objects. Applying them is up to the client, see
[What the integration does not cover](#what-the-integration-does-not-cover).

Applying the returned objects requires permissions to create or modify objects of the relevant kinds. The
`restore_snapshots` role grants the right to read the prepared manifests but does not grant permission to apply them.
For the permissions needed by `d8 snapshot restore`, see
[Required permissions](user_guide.html#required-permissions).

{{< alert level="warning" >}}
The endpoint response contains the whole snapshot subtree, including the nodes of other Deckhouse Platform modules.
Rights are checked once, when the endpoint is called, and the subtree content is not filtered by the caller's rights. So
the `restore_snapshots` role, which is also part of the backup agent ClusterRole, grants the right to read all snapshot
content in the namespace, including objects the ServiceAccount cannot read directly. Bind this role only in the
namespaces whose content the external system needs access to.
{{< /alert >}}

## Request and response bodies

Node endpoints accept and return JSON. The request and response bodies for downloading and uploading a node are
described below.

### manifests-download

The response is sent with `Content-Type: application/json`. The response body is a JSON array of objects, not a
Kubernetes `List` object and not a YAML stream. The array contains the addressed node's own objects as they were
captured, including `status` and runtime metadata:

```json
[
  {
    "apiVersion": "v1",
    "kind": "ConfigMap",
    "metadata": {
      "name": "app-config",
      "namespace": "my-app",
      "uid": "<UID>",
      "resourceVersion": "<RESOURCE_VERSION>",
      "managedFields": []
    },
    "data": {
      "example": "value"
    }
  }
]
```

### manifests-and-children-refs-upload

The request is sent with `Content-Type: application/json`. Example request body:

```json
{
  "manifests": [ { "apiVersion": "v1", "kind": "ConfigMap", "metadata": {} } ],
  "childRefs": [
    { "apiVersion": "snapshot.storage.k8s.io/v1", "kind": "VolumeSnapshot", "name": "my-volume-node" }
  ]
}
```

The request body must meet the following requirements:

- The `manifests` field is required. Its value is the JSON array that `manifests-download` returned for this node. If
  the field is missing, `null` or of another type, the request is rejected with `400`.
- The `childRefs` field contains references to direct child nodes only. Each reference contains `apiVersion`, `kind` and
  `name`, and if any of these fields is missing, the request is rejected with `400`. The reference carries no namespace:
  child nodes are in the snapshot's namespace. For a node without child nodes, the list is empty.
- A VolumeSnapshot node cannot have child nodes. A non-empty `childRefs` for it is rejected with `400`.
- The request body must not exceed 64 MiB. The limit applies to one node's own manifests, not to the whole tree. A
  request with a larger body is rejected with `413`.

Snapshot and VolumeSnapshot nodes return the same response: code `200` and a Kubernetes `Status` object:

```json
{
  "kind": "Status",
  "apiVersion": "v1",
  "status": "Success",
  "manifestCheckpointName": "..."
}
```

The `manifestCheckpointName` field is the module's internal record of the stored manifests. Only `"status": "Success"`
matters to the client.

Uploading the same body again does not cause an error, so a request with an unknown result, for example after a dropped
connection or a timeout, can be uploaded again. Once a body for a node is stored, do not upload a different body for
that node: the node's content is fixed at the first upload.

## Errors and retries

An error produced by the Kubernetes API is described by a `Status` object with the `code`, `reason` and `message`
fields. The `message` names the object the error relates to. Handle errors by the `reason` field, not by the text of
`message`: the message is intended for people. A transport error may have no `Status` body.

The responses of the node endpoints `manifests-download`, `manifests-and-children-refs-upload` and
`manifests-with-data-restoration` are listed below. Creating a snapshot and reading its status follow the general rules
of the Kubernetes API server. Volume data is transferred through the HTTP server of the `storage-foundation` module, and
its response codes are described in
[Exporting and importing volume data over HTTP](/modules/storage-foundation/usage.html#exporting-and-importing-volume-data-over-http)
in that module's documentation.

| Code | When it is returned | What to do |
|---|---|---|
| `400` | A malformed upload request body or an invalid request parameter | Fix the body or the request parameters |
| `403` | Insufficient rights, a wrong HTTP method or a mistake in the address. Also returned when a node references a SnapshotContent that does not reference the node back | Check the method, the address and the role bindings. If the cause is an inconsistent SnapshotContent reference, retrying will not help |
| `404` | No object with the specified name exists in the namespace | Check the namespace and the object name |
| `405` | A wrong HTTP method from a caller that has rights for both methods. Reads use `GET`, uploads use `POST` | Fix the method |
| `409` | The node is not ready for the operation yet, or there is a state conflict. Possible causes: the SnapshotContent is not bound yet, the object is not in `Import` mode, the node's stored manifests are not readable yet, one object appears twice in the node's manifests. Before the SnapshotContent is bound, an upload returns `reason: ImportContentNotBound` and a read returns `reason: Conflict` | Retry the request with a delay. If an object appears twice, fix the content |
| `413` | The upload request body exceeds 64 MiB | Reduce the request body. Retrying without changes will not help |
| `500` | An internal error of the module while serving the request, with `reason: InternalError` | Retry the request with a delay. If the error repeats, check the logs of the `state-snapshotter` controller |
| `502` | Only for VolumeSnapshot nodes and nodes of other modules: the module serving the node got no response from the `state-snapshotter` aggregated API | Retry the request with a delay |
| `503` | The aggregated API is temporarily not serving requests: the module is starting, restarting, or has not loaded its snapshot registry yet | Retry the request with a delay |

A mistake in the address or the HTTP method usually returns `403`, not `404` or `405`. Rights are granted on specific
subresources in specific API groups and are checked by URL before the request is routed. The HTTP method is checked as
an RBAC verb: `GET` as `get`, `POST` as `create`. The module roles grant only `get` on the download endpoints and only
`create` on the upload endpoints. So a typo in an endpoint name, a request through a group that does not serve it, and a
wrong method all fail the rights check.

If `403` is returned for an address that looks correct, check the HTTP method, the address and the role bindings in that
order. The roles are listed in
[Configuring external-system access to the cluster](#configuring-external-system-access-to-the-cluster). If all requests
return `403` right after the module is installed, retry them after a while: the aggregated role is filled in
automatically.
