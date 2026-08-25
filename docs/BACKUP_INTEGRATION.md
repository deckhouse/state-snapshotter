---
title: "Backup integration"
description: "The HTTP and YAML contract an external backup product implements to create a snapshot, confirm it succeeded, download it, and load it back into a cluster."
d8Edition: ee
weight: 30
---

## Who this document is for

This document is the client contract for an **external backup product** that stores Deckhouse
namespace snapshots outside the cluster. It covers exactly four operations:

1. create a snapshot;
2. confirm that the snapshot succeeded;
3. download the snapshot;
4. load the snapshot back into the cluster.

Those four operations are the whole HTTP contract. One more thing is described here, and it is not an
operation: the [archive format](#the-snapshot-archive), the file tree a snapshot sits in on disk. It is
the format our own tools — `d8` and the web console — write and read when they take a snapshot out of a
cluster and put one back, and they do it over the very API described here. Storing snapshots in the
same format is a **recommendation**: an archive your product assembles can then be uploaded by `d8` or
by the web console, and an archive they export can be taken into your storage as it is. It is a
recommendation and not a requirement — the four operations work without it.

Everything described here is ordinary HTTP to the Kubernetes API server plus Kubernetes objects: no
library of ours is needed, and any language will do.

**Restoring a captured namespace** is not described here. A read that hands back a subtree in the shape
it would be applied in is granted — what it gives and what it does not is in
[Restoration, in brief](#restoration-in-brief) — but resolving conflicts while applying it is out of
scope, and the recommended place for that is the Deckhouse web console.

## Model

A snapshot is a namespaced `Snapshot` object. Creating one in a namespace captures **that namespace**:
the manifests of its user objects and its volume data, frozen into a durably stored artifact that
outlives the request object. A `Snapshot` never captures another namespace and takes no target
namespace.

A snapshot is a **tree of nodes**:

- the root node is the `Snapshot` object itself;
- every node lists its direct children in `status.childrenSnapshotRefs`, as
  `apiVersion` / `kind` / `name` triples. Children live in the same namespace as their parent, so the
  reference carries no namespace;
- a node holds the manifests of its own objects and at most one volume of data. When a namespace is
  captured, the nodes that carry data are `VolumeSnapshot` objects;
- reading a snapshot goes one node per request: the download endpoint returns the manifests belonging
  to the addressed node and never walks the tree, so walking it is the client's job. The one endpoint
  that answers with a whole subtree is the restoration read, and it is not part of this contract — see
  [Restoration, in brief](#restoration-in-brief).

A `Snapshot` is **single-use and immutable**: its spec is frozen at creation, the namespace is captured
exactly once, and there is no re-capture — a new snapshot is a new object.

Everything the client addresses is **namespaced**: the objects and the endpoints alike. The durable
artifacts holding the captured bytes are cluster-scoped internals of the module: they are not part of
this contract, the roles in [Permissions](#permissions) do not open them, and a client never reads them.

## API groups and versions

| What is addressed | API group | Version |
|---|---|---|
| `Snapshot` objects | `state-snapshotter.deckhouse.io` | `v1alpha1` |
| Endpoints of `Snapshot` nodes | `subresources.state-snapshotter.deckhouse.io` | `v1alpha1` |
| `VolumeSnapshot` objects | `snapshot.storage.k8s.io` | `v1` |
| Endpoints of `VolumeSnapshot` nodes | `subresources.storage-foundation.deckhouse.io` | `v1` |
| `DataExport` / `DataImport` objects (volume data) | `storage-foundation.deckhouse.io` | `v1alpha1` |

**The most common client mistake is the version of a node's endpoints.** `Snapshot` nodes are served by
`subresources.state-snapshotter.deckhouse.io/v1alpha1`, `VolumeSnapshot` nodes by
`subresources.storage-foundation.deckhouse.io/v1`. One group or version cannot stand in for the other:
the API server serves no such path, and a request with the wrong group or version fails — with which
status exactly, see the `403` and `404` rows in [Errors and retries](#errors-and-retries).

The endpoint names are the same in both groups (`manifests-download`,
`manifests-and-children-refs-upload`, `manifests-with-data-restoration`), so are the bodies and the
statuses, and both groups answer at the same API server address: between a `Snapshot` node and a
`VolumeSnapshot` node only the group, the version and the resource name change.

## Permissions

Give the product a `ServiceAccount` of its own in every namespace it works in, and bind one role to it:
`d8:use:capability:module:state-snapshotter:backup_agent`. It is a `ClusterRole` that aggregates the six
capability roles from the table below — five of the snapshot module and one of the module that exports
and imports volume data — so the whole set arrives through a single binding. A `RoleBinding` makes it
effective in one namespace only; to cover one more namespace, create an account and a binding there —
the role itself does not change.

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
  name: d8:use:capability:module:state-snapshotter:backup_agent
  apiGroup: rbac.authorization.k8s.io
```

The rights can also be granted one role at a time. That path is for a product that is not meant to have
the whole set; here is what the set is made of.

| Role | What it buys the client |
|---|---|
| `d8:use:capability:module:state-snapshotter:view_snapshots` | Read `Snapshot` and `VolumeSnapshot` objects — the nodes of the tree. Walking the tree is reading node statuses, so this is the role that makes the walk possible. |
| `d8:use:capability:module:state-snapshotter:manage_snapshots` | Create a `Snapshot` and delete it. There is no update: the spec is immutable, and cancelling a snapshot means deleting it, which takes the whole tree with it. |
| `d8:use:capability:module:state-snapshotter:download_snapshots` | Read a node's manifests — in both groups of node endpoints. |
| `d8:use:capability:module:state-snapshotter:upload_snapshots` | Write a node's manifests back — in both groups of node endpoints — and create the `VolumeSnapshot` objects of an imported tree: the client creates them, not the module. |
| `d8:use:capability:module:state-snapshotter:restore_snapshots` | Read a whole subtree at once, addressed into the namespace of the requested snapshot — in both groups of node endpoints. This is a read for restoration, not an apply: see [Restoration, in brief](#restoration-in-brief). |
| `d8:use:capability:module:storage-foundation:transfer_volume_data` | Create `DataExport` and `DataImport` objects and move volume data through their HTTP endpoints. |

**Bind the role only in the namespaces the product has actually been given, and bind only the roles it
uses.** The set grants deleting snapshots and creating objects in a live namespace, so every binding
beyond what is needed is one more way to lose data.

A token for the service account is obtained the usual way:

```shell
d8 k -n my-app create token backup-agent --duration=24h
```

Every request below carries it in the `Authorization: Bearer <token>` header and goes to the cluster's
API server. Validate the server certificate against the cluster CA: the responses carry the namespace's
secrets exactly as they were captured.

## 1. Create a snapshot

Create a `Snapshot` in the namespace to be captured:

```yaml
apiVersion: state-snapshotter.deckhouse.io/v1alpha1
kind: Snapshot
metadata:
  name: my-namespace-snapshot
  namespace: my-app
spec:
  mode: Capture
```

The spec has exactly one field the client sets, `spec.mode`:

| Value | Meaning |
|---|---|
| `Capture` (default) | Capture the live namespace the object sits in. |
| `Import` | Capture nothing: the snapshot is materialized from what the client uploads. This is the mode of [operation 4](#4-load-a-snapshot-back-into-a-cluster). |

`spec: {}` is equivalent to `spec.mode: Capture`.

> **Spell the field name exactly, and read it back.** The API server prunes fields it does not know
> **before** validation, so an object with a misspelled name is accepted, the field is dropped, and a
> typo in the import marker gives a live capture of the namespace along with a "created" answer. After
> creating the object, read `spec.mode` back from it and compare with what you sent — there is no other
> way to see the difference.

To keep individual objects out of a capture, label them before the snapshot is created: the label and
its semantics are in
[Excluding objects from a snapshot](USER_GUIDE.md#excluding-objects-from-a-snapshot) of the user guide.
The capture is single-use, so labelling an object after the snapshot exists achieves nothing.

## 2. Confirm that the snapshot succeeded

Poll the `Snapshot` object. Success is its `Ready` condition:

```console
NAME                    READY   REASON      CONTENT                        AGE
my-namespace-snapshot   True    Completed   nss-content-1f4b0c9d2a3e5f76   30s
```

- `status.conditions[type=Ready]` with `status: "True"` and `reason: Completed` — the capture is
  finished and durably stored. This is the only sign of success.
- `status.boundSnapshotContentName` is the name of the `SnapshotContent`, the cluster-scoped object that
  brings a node's capture result together: the reference to the stored manifests and, if the node has a
  volume, the reference to the artifact holding its data. Both kinds of node have this field and it
  always names a `SnapshotContent`. You never address that object yourself, but an empty field means the
  node has nothing to download yet: until a `SnapshotContent` is bound, `manifests-download` answers
  with a refusal — see [Errors and retries](#errors-and-retries).
- `status.childrenSnapshotRefs` lists the root's direct children, and it is what
  [operation 3](#3-download-a-snapshot) walks. The list is legitimately empty at times.

The capture is **fail-closed**: there is no such thing as a partial snapshot. If any part of the
namespace could not be read or could not be stored, the object never reaches `Ready=True`; "not ready
yet" never means "ready enough".

While `Ready` is `False`, the condition's `reason` says what state the capture is in:

| Reason | Class | What it means |
|---|---|---|
| `ArtifactMissing` | Terminal | The durable artifact with the captured volume's data is missing or being deleted; that data cannot be recovered for this snapshot. |
| `ChildSnapshotLost` | Terminal | A node of the tree is irrecoverably lost. |
| `ChildrenFailed` | Terminal | A node below finished in terminal failure; its own reason says why. |
| `CreateChildFailed` | Terminal | A node of the tree could not be created. |
| `DomainCaptureFailed` | Terminal | The capture of a subtree contributed by another module failed; the message carries that module's own reason. |
| `DuplicateCoveredPVCUID` | Terminal | The capture plan covered the same volume twice, and the capture was refused rather than carried out with the ambiguity. |
| `GraphPlanningFailed` | Terminal | Planning the snapshot tree failed. |
| `ListFailed` | Terminal | One of the namespace's resource kinds could not be listed, which means the capture would have been incomplete. |
| `ManifestCheckpointFailed` | Terminal | Storing the captured manifests failed. |
| `NamespaceNotFound` | Terminal | The namespace being captured does not exist. |
| `VolumeCaptureFailed` | Terminal | Capturing a volume's data failed. |
| `ChildSnapshotDeleted` | Recoverable | The object of a tree node was deleted while its stored data is intact. The capture is complete and no data is lost, but the node is no longer reachable through the namespaced surface; bringing it back needs a human. |

- **Terminal** — the snapshot will never become ready. Stop polling and report the failure; if a retry
  is wanted, create a new `Snapshot`: the spec is immutable, there is nothing to fix in place.
- **Recoverable** — the captured data is intact and the object degraded rather than failed. It will not
  right itself and polling will not help — a human is needed.
- **Any other reason is outside the contract.** The two sets above are closed and stable; everything
  else is either progress or a free-form reason from another module's controller, and new ones may
  appear without notice. Treat an unfamiliar reason as "in progress" only while something is actually
  changing: the reason or the message changes, `status.boundSnapshotContentName` appears, children
  appear. If nothing changes at all, act on a timeout of your own and report the failure.

## 3. Download a snapshot

Downloading is a walk of the tree. Start at the root `Snapshot` and, for every node:

1. read the node object and take its `status.childrenSnapshotRefs`;
2. read the node's own manifests through `manifests-download`;
3. repeat for every child reference — in the same namespace.

`manifests-download` returns the manifests of **that node only**, and returns them **as they were
captured**, `status` and other runtime fields included.

The endpoint is chosen by the kind of the node:

| Node | Request |
|---|---|
| `Snapshot` node | `GET /apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/{namespace}/snapshots/{name}/manifests-download` |
| `VolumeSnapshot` node | `GET /apis/subresources.storage-foundation.deckhouse.io/v1/namespaces/{namespace}/volumesnapshots/{name}/manifests-download` |

```shell
curl -sS -H "Authorization: Bearer ${TOKEN}" \
  "https://{apiserver}/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/{namespace}/snapshots/{name}/manifests-download"
```

A node contributed by another Deckhouse module (a virtual machine and its disks, for instance) is
addressed through the endpoint group of the module that owns its kind: `subresources.` plus the node's
own API group, taken from the `apiVersion` of the child reference that led to the node.

**A `VolumeSnapshot` node is the exception, and deriving its group that way is wrong.**
`VolumeSnapshot` lives in the upstream CSI group `snapshot.storage.k8s.io`, while its endpoints live in
`subresources.storage-foundation.deckhouse.io/v1`, as every table here states. No rule derives that —
take it from this document.

### Volume data

Manifests describe a volume but do not contain its bytes. To read the bytes of a `VolumeSnapshot` node,
create a `DataExport` for it and download the data over the exporter's HTTP endpoint:

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

`DataExport.spec.targetRef` names the `VolumeSnapshot` node, not the original volume. Wait for the
object to become ready, take the address out of its status, and read the data over HTTP presenting the
same service-account token you use against the API server (see [Permissions](#permissions)).

**Which address appears depends on `DataExport.spec.publish`.** With it off, as above, the transfer is
reachable from inside the cluster only, at the address in `status.url`; the server there presents a
certificate of its own, and it is validated against `status.ca` — a PEM in base64. An agent working
from outside needs `publish: true`: it reads `status.publicURL`, which stays empty while `publish` is
off. The public address is served under the same name and with the same certificate as the published
API server, so from outside it is validated against the ordinary cluster CA — the one already in your
kubeconfig. No separate CA is needed for it, and `status.ca` is not needed on that path either.

On the load path, `publish` works the same way for a `DataImport`: off gives an address reachable from
inside the cluster only, on gives a public one.

`DataExport.spec.ttl` is an idle window: it starts running when the object becomes ready and is pushed
back while bytes actually move. An open but silent connection does not hold it. `DataImport.spec.ttl`
on the load path works the same way, counting the bytes going the other way — the ones you upload.

**The details of this path are in the documentation of the `storage-foundation` module:** the
endpoints, the file and block modes, the exact status fields. On the Deckhouse documentation site, the
configuration examples page, section "Exporting and importing volume contents over HTTP": "HTTP API:
exporting data" for [operation 3](#3-download-a-snapshot), "HTTP API: importing data" for
[operation 4](#4-load-a-snapshot-back-into-a-cluster).

**Exporting and importing volume data is provided by the `storage-foundation` module.** In a cluster
where it is not enabled this path does not exist at all: there are no `DataExport` and `DataImport`
kinds, there is no endpoint group serving `VolumeSnapshot` nodes, and such a node offers neither data
nor manifests.

Namespace snapshots still work — but only those that carry no data and consist of manifests alone: such
a snapshot downloads and loads back in full. The limitation is about volumes exactly, and the refusal
arrives on the first request to a `VolumeSnapshot` node.

### The volume size

**The size of a volume is one number, and it comes from the node's status.** It is the capacity of the
volume that has to appear on the receiving side, in Kubernetes quantity format; it is compared by value
and not by spelling — `10Gi` and `10240Mi` are the same thing to every check. It lives in
`status.data.size` of the node that carries the volume: a `VolumeSnapshot` leaf, or a domain snapshot
node with a volume of its own. A node without data — the root namespace snapshot, an aggregator — has
no `status.data` field at all.

A downloaded `PersistentVolumeClaim` manifest will not do instead:
`PersistentVolumeClaim.spec.resources.requests.storage` is what was requested, not what was
provisioned, and it is usually smaller.

From there the number travels unchanged and is never recomputed:

```text
status.data.size of the node  →  volumes[].size in snapshot.yaml  →  DataImport.spec.storageParams.size
```

**If you have no node status** — because you are assembling an archive of your own, for instance — take
the number from our archive, from `volumes[].size`, as it is. A block volume has a second source: the
same value is reported by a `HEAD` of the block endpoint in `Content-Length`. A filesystem volume has
no second source — the exporter reports the sizes of individual files but never publishes the volume's
capacity, and files cannot be added up: the volume is larger than their sum.

On a `VolumeSnapshot` the `status.data` field is a Deckhouse extension: upstream CSI does not have it,
and looking for the value in upstream documentation is pointless. It mirrors the `SnapshotContent`,
which your permissions do not open, so that the whole descriptor of a captured volume can be read with
an ordinary `GET` in your own namespace.

## 4. Load a snapshot back into a cluster

Loading mirrors [downloading](#3-download-a-snapshot): there you walked a finished tree top down, here
you build the same tree again, object by object, and upload each node's manifests back. Nothing is
inferred from the content — a node exists in the cluster because you created its object.

1. **Create the root** `Snapshot` in the target namespace with `spec.mode: Import`. Nothing is captured
   for it: it waits for what you will upload.

   ```yaml
   apiVersion: state-snapshotter.deckhouse.io/v1alpha1
   kind: Snapshot
   metadata:
     name: my-namespace-snapshot
     namespace: my-app-restored
   spec:
     mode: Import
   ```

2. **Create an object for every non-root node** — in the same namespace and in import mode as well. A
   node with volume data is a `VolumeSnapshot` object:

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
         uid: <uid of the root Snapshot object>
   spec:
     mode: Import
   ```

   The `ownerReferences` entry pointing at the **parent node** is required, and it is required for
   garbage collection. The tree is single-use: deleting the root `Snapshot` takes everything below it
   and frees the uploaded bytes, without a confirmation and without an undo. The Kubernetes garbage
   collector does that, and `ownerReferences` is the only thing it sees the relation through; a node
   without that entry survives the deletion of the root and stays in the namespace on its own.

   **Leave `VolumeSnapshot.spec.source` of such a node empty** — as in the example above. On an ordinary CSI
   `VolumeSnapshot` that field is required and names what to snapshot: a live `PersistentVolumeClaim`
   or content prepared in advance. A node in import mode has nothing to snapshot: its data arrives
   through a `DataImport` (step 4), so no source is given.

3. **Upload each node's manifests** — one request per node:

   | Node | Request |
   |---|---|
   | `Snapshot` node | `POST /apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/{namespace}/snapshots/{name}/manifests-and-children-refs-upload` |
   | `VolumeSnapshot` node | `POST /apis/subresources.storage-foundation.deckhouse.io/v1/namespaces/{namespace}/volumesnapshots/{name}/manifests-and-children-refs-upload` |

   The body carries the node's own manifests and the references to its **direct** children; the
   children themselves are uploaded by their own requests. The exact shape is in
   [Request and response bodies](#request-and-response-bodies).

   Until the module has bound the node you are uploading into, the request answers `409` with reason
   `ImportContentNotBound`. **This is the normal course of events, not an error**: binding happens
   asynchronously, so retry with a delay. Re-uploading is safe (see the same section).

4. **Move the volume data.** For every `VolumeSnapshot` node: create its `DataImport`, wait for it to
   become ready and for its address to appear, upload the data over that HTTP endpoint, **tell the
   importer the upload is over** — a separate request, described below in this step — and wait for the
   `DataImport` to report completion. Completion is `status.phase: Completed`. Do not wait for
   `Ready=True` along with it: on a finished import `Ready` is `False` with reason `Completed`, because
   the endpoint is already closed.

   **Create each `DataImport` right before writing into it:** the `ttl` window starts when the import
   becomes ready, not at the first byte written, so one created in advance spends its window while you
   are busy with the previous volume. Imports of different nodes may run in parallel. Two things are
   forbidden: two `DataImport` objects for one node — the node is then not bound at all and receives no
   data — and two writers into one endpoint at the same time, where the second gets a `409` with no
   headers, and that is not an offset mismatch, so retrying from a different offset will not help.

   The end of the upload is an explicit `POST` to the importer's `finished` endpoint. Nobody infers it
   from the last byte written: a client that uploads the data and waits for the object to notice will
   wait forever. That request, both transfer modes and the addresses they use are described in the
   "HTTP API: importing data" section of the `storage-foundation` module's documentation.

   ```yaml
   apiVersion: storage-foundation.deckhouse.io/v1alpha1
   kind: DataImport
   metadata:
     name: my-volume-node-import-1
     namespace: my-app-restored
   spec:
     ttl: 15m
     publish: false
     mode: PopulateData
     snapshotRef:
       apiVersion: snapshot.storage.k8s.io/v1
       kind: VolumeSnapshot
       name: my-volume-node
     storageParams:
       storageClassName: my-storage-class
       size: 1Gi
       volumeMode: Filesystem
   ```

   `DataImport.spec.snapshotRef` names the `VolumeSnapshot` node this import fills, and the relation is
   derived from that field alone: `DataImport.metadata.name` takes no part in the matching and may be
   anything.
   But **the name of a `DataImport` is also claimed as the name of a `PersistentVolumeClaim`** — the
   transit volume is named after the import and is deleted once captured, so a name under which this
   namespace already has, or will later get, someone else's `PersistentVolumeClaim` must not be used:
   the import will take that claim over and destroy it along with its data. Give the import a name
   with a suffix unique to this load, and check before creating it that no `PersistentVolumeClaim` of
   that name exists in the namespace.

   `DataImport.spec.storageParams` describes the transit volume the bytes are put into before durable
   content is made out of them:

   | Field | Where to get it |
   |---|---|
   | `storageClassName` | Required. The class of the transit volume in the target cluster: choose it by the storage that will hold this cluster's volume snapshot and that the volume will later be restored from. The class of the captured `PersistentVolumeClaim`, taken from its manifest, is the right answer when the target cluster has the same storage. The class must exist and must support snapshots. It is not compared with the original: the node records the class you named. |
   | `size` | Required. The capacity of the captured volume — see [The volume size](#the-volume-size); it must not be understated, and running out of room looks different in the two modes. On a filesystem volume the space runs out mid-upload: a `500` arrives, indistinguishable from any other server failure, and a half-written file stays in storage. On a block volume a write past the end of the device is refused with `416`, and exceeding the declared total with `422`. |
   | `volumeMode` | Optional: if it is not set, the import runs in `Filesystem` mode. Set the mode the captured `PersistentVolumeClaim` had. |

   **`DataImport.spec.ttl` is an idle window, not a deadline for the whole transfer.** It is counted
   from this import's last activity, and an upload in progress is activity, so a transfer that takes
   longer than the value is not cut off; a pause is what spends the window. When the window expires the
   import is marked expired and its serving side is torn down: the snapshot node it was to fill will not
   get its data, and the way forward is a new `DataImport` for that node, uploaded from the beginning
   (nothing survives of the expired import's partial upload). Size the value for the longest pause you
   expect, and create each import shortly before writing into it.

5. **Wait** for the root `Snapshot` to reach `Ready=True` with reason `Completed` — exactly as in
   [operation 2](#2-confirm-that-the-snapshot-succeeded). The root becomes ready only once every node
   has both its manifests and its data.

## Restoration, in brief

`manifests-with-data-restoration` (`GET`) returns a whole subtree prepared for applying: without
`status` and runtime metadata, with the objects addressed into the namespace of the requested
`Snapshot`. For a tree that was loaded back into a cluster that is the namespace it was loaded into, not
the one its objects were captured from. The caller cannot choose a different namespace: an attempt to is
refused with `400`. The endpoint is granted by the `restore_snapshots` role from
[Permissions](#permissions). It is not one of this document's four operations, and the rest of its
request contract is not specified here.

**Restoring into another namespace means moving the snapshot there first.** The route is to export the
snapshot from the namespace it lives in ([operation 3](#3-download-a-snapshot)) and load it into the
target one ([operation 4](#4-load-a-snapshot-back-into-a-cluster)); only then does that namespace hold a
`Snapshot` you can ask `manifests-with-data-restoration` for. There is no cheap way around it: this
endpoint has no "hand me the same tree, addressed elsewhere" switch, and moving a snapshot is a full
re-upload, volume data included.

**This is a read, not an apply.** What comes back is a list of objects in the shape they would be
applied in. Applying them is a separate action carried out with the rights of whoever applies: no role
of the set grants creating arbitrary objects in a namespace, so your product's service account — the one
holding the roles from [Permissions](#permissions) — will receive the subtree but cannot apply it.

**The answer hands back the whole subtree, nodes of other Deckhouse modules included.** Your rights are
checked once, at the entrance, for the request as a whole; the content of the answer is not checked
against them afterwards, and it is assembled by the module with its own rights rather than yours. A role
bound in a namespace is therefore read access to everything in that namespace that ended up in the
snapshot, including objects the account may not read directly. Hence the requirement to bind it only in
the namespaces the product has actually been given (see [Permissions](#permissions)).

**Conflicts are resolved by a human.** The recommended place to resolve them is the Deckhouse web
console.

## Request and response bodies

### `manifests-download`

**Response.** `Content-Type: application/json`; the body is a **plain JSON array of objects** (not a
Kubernetes `List` and not a YAML stream) holding the addressed node's own objects verbatim as captured,
`status` and runtime metadata included:

```json
[
  {
    "apiVersion": "v1",
    "kind": "ConfigMap",
    "metadata": { "name": "app-config", "uid": "...", "resourceVersion": "...", "managedFields": [ ... ] },
    "data": { ... }
  },
  ...
]
```

### `manifests-and-children-refs-upload`

**Request body.** `Content-Type: application/json`:

```json
{
  "manifests": [ { "apiVersion": "v1", "kind": "ConfigMap", "metadata": {} } ],
  "childRefs": [
    { "apiVersion": "snapshot.storage.k8s.io/v1", "kind": "VolumeSnapshot", "name": "my-volume-node" }
  ]
}
```

- `manifests` is required and must be a JSON array — the very one `manifests-download` returned for this
  node. Missing, `null` or not an array is refused with `400`.
- `childRefs` lists the node's **direct** children only, and each of them requires all three fields
  `apiVersion`, `kind` and `name`; a missing field is refused with `400`. The reference carries no
  namespace: the children of a namespace snapshot lie in the same namespace as the snapshot itself. The
  list may be empty — a node may have no children.
- A `VolumeSnapshot` node never has children: a non-empty `childRefs` is refused with `400`.

**Response.** HTTP `200` and a Kubernetes `Status` object — the same for a `Snapshot` node and a
`VolumeSnapshot` node:

```json
{
  "kind": "Status",
  "apiVersion": "v1",
  "status": "Success",
  "manifestCheckpointName": "..."
}
```

The name it carries is the module's internal record of the stored manifests; a client needs nothing from
it beyond the `"Success"` itself.

**Retries are safe.** Re-uploading the same body is harmless, so a request whose result you never saw (a
dropped connection, a timeout) can simply be sent again. What is not safe is sending a *different* body
for a node after one has been stored; consider a node's content fixed from the first send.

**Body size.** An upload body is limited to 64 MiB, and a larger one is refused with `413`. The limit is
about one node's own manifests, not about the whole tree.

## Errors and retries

An error shaped by the Kubernetes API comes back as a `Status` object with `code`, `reason` and a
`message` naming the object involved; a transport error may have no such body. Branch on `reason`, not
on the text of the message: messages are written for people.

The table below describes the answers of the **aggregated node endpoints** — those listed in
[Download a snapshot](#3-download-a-snapshot) and
[Load a snapshot back into a cluster](#4-load-a-snapshot-back-into-a-cluster), plus the restoration
read. Ordinary work with Kubernetes objects — creating a `Snapshot`, polling its status, creating a
`DataImport` — follows the general rules of the API server. And volume data does not travel through the
aggregated API at all: it goes through a separate HTTP server of the `storage-foundation` module, whose
address you take from the status of a `DataExport` or a `DataImport`. That is why `416`, `422` and `500`
are not in this table: their contract lives in the documentation of the `storage-foundation` module (see
[Volume data](#volume-data)).

| Code | When | What to do |
|---|---|---|
| `400` | A malformed upload body (`manifests` missing or not an array, a child reference without a field, a non-empty children list on a `VolumeSnapshot` node); an invalid request argument. | Fix the body or the request. |
| `403` | The request is not covered by the rights: a role not granted or not yet filled in, a wrong HTTP method, **any mistake in the address** — a typo in an endpoint name, a request through a group that does not serve it. An address is refused here rather than as `404` because the roles are granted on specific subresources in specific groups, and the authorization check is built from the URL and runs before routing. Separately from rights, the same answer comes from a node that references a `SnapshotContent` which does not reference the node back. | Check in this order: the method, the address, the bindings — against [Permissions](#permissions) and the endpoint tables in [Download a snapshot](#3-download-a-snapshot). If every request in a row is refused on a freshly installed module, retry: the aggregated role fills itself in. The content case is a refusal by construction, and retrying will not help. |
| `404` | No object of that name exists in this namespace. A mistake in the address does not lead here: with the roles from [Permissions](#permissions) it gives `403`, see the row above. | Check the namespace and the object name. |
| `405` | A wrong HTTP method (reads are `GET`, uploads are `POST`), requested by someone allowed both on that endpoint. | Fix the method. With the roles from [Permissions](#permissions) the same mistake arrives as `403`, see below. |
| `409` | The node has no `SnapshotContent` bound yet, that is, `status.boundSnapshotContentName` is empty: an upload is answered with reason `ImportContentNotBound` (expected at the start of an import), a read with reason `Conflict`. Reason `Conflict` also carries the remaining state conflicts: an upload into an object that is not in `Import` mode, a node whose stored manifests are not readable yet, the same object twice in a node's manifests. | Everything except the duplicate should be retried with a delay: those states are temporary and clear by themselves. A duplicated object is a defect of the content and will not clear. |
| `413` | The upload body is larger than 64 MiB. | Not retryable in that shape. |
| `502` | On the endpoints of any node other than a namespace snapshot: `VolumeSnapshot` nodes and domain nodes are served not by `state-snapshotter` but by the module the node belongs to, and that module fetches the manifests from the aggregated API of `state-snapshotter`. This code means no answer at all came back from the aggregated API of `state-snapshotter`. | Retry with a delay. |
| `503` | The aggregated API you addressed is not serving requests: the module is starting, restarting, or its snapshot registry is not warm yet. For a namespace snapshot that is `state-snapshotter`; for `VolumeSnapshot` and domain nodes it is either the module serving them, or `state-snapshotter` answering that way behind it (there is an answer, unlike with `502`). | Retry with a delay. |

**A wrong method usually arrives as `403` rather than `405`.** Every request is authorized against the
verb its method maps to (`GET` to `get`, `POST` to `create`), and the roles from
[Permissions](#permissions) grant `get` on the download endpoints and `create` on the upload endpoints —
exactly those. So with the rights described here, a `403` on an address that looks correct is worth
checking against the method first, and against the bindings second.

## The snapshot archive

A snapshot can be exported from a cluster as an archive and loaded back from the same archive. Our own
tools do that — the `d8` client and the Deckhouse web console — over the very API described above, and
the format of the archive is specified by this document. **We recommend storing snapshots in this
format.** Then:

- an archive assembled by your product can be uploaded into a cluster by `d8` or by the web console;
- an archive exported from a cluster by our tools can be taken into your storage.

This is a recommendation and not an obligation: the four operations of this document are
self-sufficient, and the archive adds nothing to
[operation 4](#4-load-a-snapshot-back-into-a-cluster). What matching formats buy is something else — a
snapshot becomes a file, which can be handed over on a medium, through file storage or by hand, without
wiring your product to the cluster and without requiring both to be running at once. Diverge from the
format and you lose that: the only way left to exchange snapshots is your own HTTP requests.

The tree is handed over either as a directory or as a single `.zip` file with the same layout inside;
the `.zip` name is recognized in any case. There are no other containers.

### How to assemble one

1. Lay out the tree: one directory per node, direct children under `snapshots/`.
2. Put each node's manifests into it, and its single payload if the node has a volume.
3. Fill in everything in every `snapshot.yaml` except the digests.
4. Compute each node's `checksum`.
5. **Going from the leaves to the root**, compute `childrenChecksum`, then `metadataChecksum`, and write
   `snapshot.yaml`. Bottom up, because a child's digests go into its parent's digest.
6. Check the directory: `d8 snapshot local describe <directory>`.
7. If a single file is wanted, pack the checked tree into a `.zip`.

### The tree

Exactly one top-level directory, named anything at all — it is the directory of the root node. Every
node, the root and a descendant alike, is a directory of one and the same shape:

```text
my-namespace-snapshot/                      # the root node
├── snapshot.yaml                           # the node's description and its three digests
├── manifests/                              # the node's own manifests, one object per file
│   └── configmap_app.yaml
└── snapshots/                              # direct children; a node without children has no such directory
    ├── volumesnapshot_data-0/              # a VolumeSnapshot node with a block volume
    │   ├── snapshot.yaml
    │   ├── manifests/
    │   │   └── persistentvolumeclaim_data-0.yaml
    │   └── data.bin
    └── volumesnapshot_files-0/             # a VolumeSnapshot node with a filesystem volume
        ├── snapshot.yaml
        ├── manifests/
        │   └── persistentvolumeclaim_files-0.yaml
        └── data.tar
```

**When assembling an archive:**

- put a `snapshot.yaml` into every node. For the root the rule is strict: an archive whose root does
  not have that file is refused as a whole;
- store in `manifests/` exactly the array [`manifests-download`](#3-download-a-snapshot) returned for
  this node — **each object as a separate file**. Every node has that directory and it is never empty:
  the root holds at least the manifest of the `Namespace` itself, and a `VolumeSnapshot` node holds the
  manifest of the captured `PersistentVolumeClaim`, exactly one;
- give a manifest file a name ending **in exactly `.yaml`**. Neither `.yml` nor any other suffix counts
  as a manifest: such a file is not part of `checksum`, is not uploaded, and does not make it into the
  list of what was skipped — it disappears silently;
- give a manifest file a name **unique within its directory**. A scheme built from the object's kind and
  name is almost always enough — one namespace never holds two objects with the same group, kind and
  name — but the same kind can come from different API groups, and there such a scheme gives one file
  for two objects. Our tools write `<kind>_<name>.yaml` and in that case add the group:
  `<kind>.<group>_<name>.yaml`;
- put **exactly one non-empty YAML or JSON document** into a file (JSON here is a special case of YAML).
  No second document behind a `---`, no empty document, no repeated keys in one mapping: a manifest is
  parsed on upload rather than carried across as bytes;
- do not rename a manifest after computing `checksum` — the relative path goes into the digest;
- put every **direct** child into its own subdirectory of `snapshots/`; a node without children has no
  such directory. The subdirectory's name tells a reader nothing — what node is inside comes from its
  `snapshot.yaml` — but among its siblings the name must be unique. Our tools keep to the scheme
  `<kind>_<name>` and, when that name is already taken by another node, append
  `__<first 8 characters of its checksum>`;
- do not put more than one payload into a node — `data.bin`, `data.bin.zst` or `data.tar`. See
  [The volume payload](#the-volume-payload);
- do not create a `data/` directory: it reads as a node with several volumes, and such a node is
  refused.

**When reading someone else's archive, do not infer identity from names.** What object is in a manifest
is stated by that manifest alone; what node is in a subdirectory, by its `snapshot.yaml` alone. A
subdirectory of `snapshots/` without that file is not a child: it is skipped rather than refused, and
it goes into the list of skipped entries shown after the walk. But skipping a directory and accepting
the archive are two different things: if that child is counted in the parent's `childrenChecksum`, the
digest stops matching and the archive is refused all the same. A node cannot be taken out of a finished
tree by deleting its `snapshot.yaml`. And `data.bin.d/`, `data.tar.d/` and `identity.json` are traces
of a download our tools did not finish; they do not affect the node's content.

**Anything extra is ignored silently.** An unknown field in `snapshot.yaml` and a stray file in a node's
directory do not get the archive refused, but they do not appear in any report either — do not count on
smuggling anything of your own through them. The exception is names from the payload namespace
(`data.bin*`, `data.tar*`, `data`): those are parsed strictly and can get the node refused.

**These violations refuse the whole archive before the cluster is touched at all:**

| Violation | Rule |
|---|---|
| The root is not a namespace snapshot | When the whole archive is uploaded, the root must carry `apiVersion: state-snapshotter.deckhouse.io/v1alpha1`, `kind: Snapshot`. A `VolumeSnapshot` root gives `archive root VolumeSnapshot/<name> is not a core Snapshot`. This is a restriction of the mode, not of the format — see below. |
| A `VolumeSnapshot` node without bytes | A node with `apiVersion: snapshot.storage.k8s.io/v1`, `kind: VolumeSnapshot` must carry `data.bin`, `data.bin.zst` or `data.tar`. |
| `volumes` disagrees with the payload | With a payload, exactly one entry, and it describes that very payload; without a payload, none. Two entries never occur on any node. |

**A single volume can be uploaded on its own — by naming the node.** The requirement on the root in the
table above applies to uploading a whole archive. Any `VolumeSnapshot` node of an archive is uploaded by
itself once the tool is told which node to take:
`d8 snapshot upload -n <namespace> -i <directory> --node VolumeSnapshot/<name>`, and in the web console
by picking that node in the tree instead of "the whole archive". The checks on `snapshot.yaml`, on the
payload and on the digests are the same as for a whole archive.

The node arrives in the cluster as a **new object of its own**: it cannot be attached to a tree that
already lives there. Ancestors left outside the chosen node are verified against the digests in their
`snapshot.yaml`, but they are neither looked up nor created in the cluster.

### The volume payload

A node carries at most one payload, and the `volumes` entry in its `snapshot.yaml` describes exactly
that payload:

| Node | Payload | `volumes` |
|---|---|---|
| No volume | none | the field is absent, or `[]` — the two are the same thing |
| Block volume | `data.bin` or `data.bin.zst` | exactly one entry, `volumeMode: Block` |
| Filesystem volume | `data.tar` | exactly one entry, `volumeMode: Filesystem` |

A `VolumeSnapshot` node can only be one of the last two rows. An empty `volumes: []` does not affect
`metadataChecksum` either: the canonical form drops it just as it drops an absent field.

**A block volume is `data.bin`**, the volume's bytes exactly as the export handed them over; for a raw
block export that is the whole device. A compressed payload keeps its codec in the name:
`data.bin.zst`. Compression is either zstd or none: no other codec belongs in an archive.

**On a block node, `size` in `snapshot.yaml` must equal the number of uncompressed payload bytes
exactly.** The export hands over the whole device, so the volume's capacity and the payload's length are
one and the same number. This is checked on upload: the uncompressed length is taken from the file size
when the payload is not compressed, and from the declared content size of the stream for
`data.bin.zst` — a discrepancy of a single byte brings the archive down.
Where to take the value itself from is in [The volume size](#the-volume-size). A filesystem node has no
such equality: its payload is unrelated to the volume's capacity.

#### zstd stream rules

They are the same for `data.bin.zst` and for compressed entries inside `data.tar`:

- every frame declares its `Frame_Content_Size`;
- no skippable frames and no dictionary;
- the framing of every frame is intact;
- the declared sizes add up to `size` from `snapshot.yaml`, and for an entry in the tar to that entry's
  own `D8.snapshot.fs.rawSize`.

**Multiple frames are what makes a transfer resumable.** An interrupted download or upload continues
from an uncompressed offset, and the only way to find that offset in a compressed file is to walk the
frame headers: the declared sizes give the boundaries, and one frame has to be decompressed rather than
the whole prefix up to the point of the break. A single frame for the whole volume does not break the
format, but every resume then decompresses the stream from the beginning. The frame size is not
specified by the contract — pick your own.

**The first rule is easiest to break with a pipe.** Reading its input from a pipe — `cat data.bin | zstd`
and any similar pipeline — the compressor does not know the length in advance and leaves
`Frame_Content_Size` unwritten. Pass the file by name, `zstd data.bin`: the length is known and the
field makes it into the frame header.

The frames are walked before the bytes leave the machine, so a violation brings the upload down: some
of these rules are checked before it starts, others already mid-transfer. Such a payload still passes
the three digests and `d8 snapshot local describe`: the bytes on disk are intact, they are merely
unmeasurable.

#### A filesystem volume is `data.tar`

The tar container itself is never compressed, and what is inside it is contract too. A filesystem volume
is handed over by the export one file per request, so the container is assembled by whoever writes the
archive — and an ordinary `tar` of the downloaded files is refused, because it does not carry the
entries described below.

Every regular file entry carries three PAX records:

| PAX record | Value |
|---|---|
| `D8.snapshot.fs.codec` | The codec applied to this entry: `none` or `zstd`. The codec must be one and the same throughout the tar — a mixed one is refused. |
| `D8.snapshot.fs.originalPath` | The file's path inside the volume: relative and portable — no leading `/`, no backslash, no drive letter, no empty segments and no `.` or `..` segments, no characters below `0x20` and no `0x7f`, never starting with the reserved segment `.d8-meta`, and already normalized, that is, unchanged by collapsing `.` and `..`. |
| `D8.snapshot.fs.rawSize` | The number of bytes of the entry before compression, as a decimal number without leading zeros. For codec `none` the entry's own size in the tar must equal it; for a compressed entry the uncompressed bytes are counted and must come to exactly this number. |

The entry's name is not arbitrary either: `originalPath` plus the codec's extension — nothing for
`none` and `.zst` for `zstd`. The `originalPath` itself must be unique among regular files: two entries
with the same path are refused. PAX fields beyond these three are allowed, but they carry no guaranteed
semantics — a reader does not interpret them.

Directories travel as ordinary tar directory entries and carry no PAX records, but their names obey the
same rules as `originalPath` above, the trailing `/` aside. That is why `tar -C /volume -cf data.tar .`
will not do: it writes entries as `./`, `./sub/` and so on, and each of them is refused on the `.`
segment. Entry names have to be built from the root of the volume, with no leading `./`.

The order of entries in the tar is arbitrary, and explicit entries for parent directories are not
needed: the entry `a/b/file` is accepted even when there are no `a/` and `a/b/` entries in the tar —
the missing directories are created when the file itself is written.

**What reaches the restored volume and what does not.** For a regular file the numeric `mode`, `uid`,
`gid` and `mtime` are restored. Owner and group names (`uname`, `gname`) are not carried over. Directory
attributes are not carried over either: permissions, owner and time are lost even for an explicit
directory entry — the directory is created as the parent of the file written into it, and nothing
beyond that is taken from the entry.

**Anything that is neither a regular file nor a directory is refused** — a symlink, a hard link, a
device, a fifo — and so is an empty directory: the upload protocol carries files, and a directory with
no file under it has nothing to be recreated from on the receiving side. The only way around this rule
is an explicit action by whoever uploads: in the `d8` client that is the
`--skip-unsupported-fs-entries` flag, in the web console a checkbox in the upload dialog, cleared by
default. What was skipped is listed after the upload, because skipping is data loss: whatever stood
behind those entries will not be in the restored volume.

The single exception is `lost+found`: an empty directory of that name is always skipped, no permission
from the uploader is needed for it, and it does not count as a loss. Such a directory is created when
any ext4 filesystem is formatted and is empty in a healthy volume, so without the exception no such
volume would upload at all. The last segment of the name is what is matched, so the exception works at
any depth and not only at the root of the volume.

### The `snapshot.yaml` file

This file records what node the reader is looking at and certifies its content. It is checked before
the first write to the cluster, and an archive whose file does not add up is refused as a whole.

```yaml
formatVersion: 2
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
name: data-0
namespace: my-app
uid: 5f6bd1ca-2c1a-4a2f-9f3f-000000000001
checksum:
  algorithm: sha256
  hex: 311996ce65e34f57fb9748e53a35c1f31df523ed533436ebce32a13f02f0f984
  short: 311996ce
childrenChecksum:
  algorithm: sha256
  hex: 926e82507890b7d5c7ad50f8cb7fd2172f81bfd2cf1d2ca86e3b644f66b02701
  short: 926e8250
metadataChecksum:
  algorithm: sha256
  hex: 46b8d72137d6cd0d149ca2455cd2f9809a590fdabe6e0362554ccad84fa00734
  short: 46b8d721
volumes:
  - target:
      apiVersion: v1
      kind: PersistentVolumeClaim
      name: data-0
      namespace: my-app
      uid: 5f6bd1ca-2c1a-4a2f-9f3f-000000000002
    artifact:
      apiVersion: snapshot.storage.k8s.io/v1
      kind: VolumeSnapshotContent
      name: snapcontent-1
    volumeMode: Block
    storageClassName: my-storage-class
    size: 10Gi
```

**Write plain YAML scalars, and omit a field you have no value for:**

- `formatVersion` is **a number without quotes**: `formatVersion: 2`, not `"2"`;
- an optional field you have no value for is **omitted entirely** — no `null`, no `~`, no empty string.
  `metadataChecksum` is computed over the canonical form of the file, and there is no empty value in
  it: an explicitly written null does not reproduce the digest, and the archive is refused;
- the order of keys is arbitrary — what the digests cover is not the YAML document itself but the values
  in it.

| Field | Rule |
|---|---|
| `formatVersion` | `2`, required. Any other value is refused, so do not count on an archive "from the future": an unfamiliar version is not waved through by the reader, it is turned away. A file without this field is an archive of the pre-versioning format, whose metadata is certified by nothing; do not assemble such an archive. |
| `apiVersion`, `kind`, `name` | The node object's own identity. For a child, in the form it has in the parent's reference that led to it; for the root, which no reference leads to, the identity of the `Snapshot` object whose snapshot it is. All three are required. For the kinds `Snapshot` and `VolumeSnapshot` they also decide what the node must carry — see the table of refusals in [The tree](#the-tree). |
| `namespace`, `uid` | The namespace and the UID of the node object. Optional, but everything written here goes into the parent's `childrenChecksum` (see [Computing the digests](#computing-the-digests)), so write it consistently. |
| `sourceName`, `sourceObjectRef` | Where the node came from: the name of the captured source object and its `apiVersion`, `kind`, `name` triple. Both are optional — which is why the example above has neither; `sourceObjectRef` either sets all three fields or is absent entirely. Assembling an archive yourself, you may leave them out; re-uploading an archive of ours, keep them as they are — they go into `metadataChecksum`. |
| `checksum`, `childrenChecksum`, `metadataChecksum` | The digests of the node's own content, of the exact set of its direct children, and of this very file. All three are required — `childrenChecksum` on a node without children included. |
| `volumes` | Follows the payload: see the matrix in [The volume payload](#the-volume-payload). |

All three digests are written the same way: `algorithm` is `sha256`, `hex` is 64 lower-case hex
characters, `short` is exactly the first 8 characters of `hex`.

The `volumes` entry describes the volume that lies in the payload, and it is what the load path builds a
`DataImport` from:

| Field | Rule |
|---|---|
| `target` | The identity of the captured claim: `apiVersion`, `kind`, `name` are required, `namespace` and `uid` are as on the claim. This is the same object whose manifest lies in this node's `manifests/`. |
| `artifact` | The identity of the stored content the node was bound to at capture time: on a `VolumeSnapshot` node that is the `VolumeSnapshotContent` named in `status.boundVolumeSnapshotContentName`. `apiVersion`, `kind` and `name` must be non-empty; their values are compared with nothing, and the load path does not read them. The structure of `artifact` is shared with `target`, so `namespace` and `uid` are parsed in it as well and go into `metadataChecksum` — but content is cluster-scoped, those fields mean nothing there, and they should not be written. |
| `volumeMode` | Required, and exactly `Block` for a `data.bin` or `data.bin.zst` payload, or `Filesystem` for `data.tar`. An empty value is as much of a mismatch as any other. |
| `storageClassName` | Required and non-empty. |
| `size` | The capacity of the captured volume: required and positive — see [The volume size](#the-volume-size). On a block node it must additionally equal the exact number of uncompressed payload bytes — see [The volume payload](#the-volume-payload). On a filesystem node nobody verifies it, but it is precisely this number that is taken on load as the size of the volume to create, so it must not be understated. |

### Computing the digests

All three digests are SHA-256. Below is exactly what the tools recompute and verify.

**`checksum` — the node's content.** Covered are every `manifests/*.yaml` file and the single payload
(`data.bin`, `data.bin.zst` or `data.tar`). Not covered are `snapshot.yaml` itself, the whole
`snapshots/` subdirectory with all the children, and anything in `manifests/` whose name does not end
in `.yaml`.

1. For every covered file compute `sha256(<path> + 0x00 + <file bytes>)`, where the path is the file's
   path relative to the node's own directory (`manifests/configmap_app.yaml`, `data.bin`) in UTF-8, then
   one zero byte, then the file's bytes unchanged.
2. Sort the paths of the covered files bytewise.
3. Feed the final SHA-256 the raw 32 bytes of each per-file digest in that order.
4. Write the lower-case hex into `checksum.hex`.

**`childrenChecksum` — the exact set of direct children.** Fed into one SHA-256, in order:

1. the ASCII string `d8-snapshot-children-checksum-v1` and one zero byte;
2. the number of direct children as a decimal string, length-prefixed;
3. for every child, in canonical order (see below), seven length-prefixed strings: its `apiVersion`,
   `kind`, `namespace`, `name`, `uid`, then its own `checksum.hex` and `childrenChecksum.hex`.

*Length-prefixed* means: the length of the string in UTF-8 bytes as an eight-byte big-endian integer,
followed by the bytes themselves. An absent `namespace` or `uid` gives an empty string — zero length and
no bytes. Canonical order sorts the children by the tuple `apiVersion`, `kind`, `namespace`, `name`,
`uid` joined with the byte `0x1F`, compared bytewise; two children with the same tuple are refused.

A node without children commits the digest of the empty set, and that is always
`926e82507890b7d5c7ad50f8cb7fd2172f81bfd2cf1d2ca86e3b644f66b02701`. Reproducing this constant is the
cheapest check that your implementation of the digest is right.

A child's digests go into its parent's digest, which is why `snapshot.yaml` files are written bottom up:
the leaves first, then their parents, then the root.

**`metadataChecksum` — `snapshot.yaml` itself.** It is serialized into compact JSON, with no whitespace
between tokens and WITHOUT the `metadataChecksum` field — that one is dropped entirely — and SHA-256 is
taken of those bytes. The order of keys is fixed, and it is not alphabetical:

| Level | Key order |
|---|---|
| Top level of the file | `formatVersion`, `apiVersion`, `kind`, `name`, `namespace`, `uid`, `sourceName`, `sourceObjectRef`, `checksum`, `childrenChecksum`, `volumes` |
| Any digest | `algorithm`, `hex`, `short` |
| `sourceObjectRef` | `apiVersion`, `kind`, `name` |
| A `volumes` entry | `target`, `artifact`, `volumeMode`, `storageClassName`, `size` |
| `target`, `artifact` | `apiVersion`, `kind`, `name`, `namespace`, `uid` |

Two more rules:

- a field that may be omitted is dropped entirely when it is empty, rather than written as `null`. The
  full set is: `formatVersion`, `namespace`, `uid`, `sourceName`, `sourceObjectRef`,
  `childrenChecksum`, `volumes`, then `volumeMode`, `storageClassName` and `size` inside a `volumes`
  entry, and `namespace` and `uid` inside `target` and `artifact`. This is a serialization rule and not
  a list of optional fields: `formatVersion` and `childrenChecksum` must be in the file, and their
  absence is refused before the digest is computed at all. What really disappears from a correct file is
  only `volumes` — on a node without a payload — and the optional identity fields. Every other key is
  written even when its value is empty;
- in strings, `"` and `\` are escaped, and — this is the part people forget — `<`, `>` and `&` are
  escaped as `\u003c`, `\u003e` and `\u0026`. Everything else, non-ASCII included, is written as UTF-8.

The example in [The `snapshot.yaml` file](#the-snapshotyaml-file) hashes into exactly the
`metadataChecksum` written in it — that is, it works as a test vector for an implementation of this
serialization.

### Checking an archive before you ship it

`d8 snapshot local describe <directory>` walks an archive: it needs no cluster and changes nothing.

**It checks:** all three digests of every node, the fields of `snapshot.yaml` against the rules above,
and the agreement of `volumes` with the payload that lies on disk. Every byte of the payload is read
along the way — the content digest covers it — so the command costs a full SHA-256 pass over the whole
archive, a multi-terabyte volume included.

**It does not check:** `size` against the payload's length, the contents of `data.tar`, the zstd frames,
the requirement that a `VolumeSnapshot` node carry a payload, the kind of the archive's root, and the
packing into a `.zip` — the command looks at a directory on disk. Its success is therefore no promise
that the archive will upload: an archive with a rounded `size`, with an ordinary `tar`, or with a
`data.bin.zst` compressed through a pipe passes this command and is refused on upload.

### Packing the tree into a `.zip`

A `.zip` is the same tree packed into one file; the layout inside does not change: an entry's path
equals the file's path in the tree above, and the separator is `/` only. Most of the requirements below
are checked when the file is opened, and an archive violating any of them is refused as a whole:

1. The file name ends in `.zip`, in any case.
2. The archive has exactly one top-level directory — the directory of the root node. **A rule for
   whoever writes the archive:** give it a directory entry of its own. A reader also derives the root
   from the path `<root>/snapshot.yaml`, but that should not be relied on.
3. Entry paths are unique. **A rule for whoever writes the archive:** a repeated path is not refused by
   the reader but silently collapsed, keeping the last central-directory entry — so a substituted file
   goes through unnoticed.
4. Every entry is **stored**: compression method `0` both in the local header and in the central
   directory, and the size in the archive equals the size of the data. A compressed entry is refused,
   however small it may be.
5. No entry is encrypted — neither with ordinary nor with strong encryption.
6. The archive is a single part: disk numbers `0`, the number of entries in the end-of-central-directory
   record equals the total, and there is no ZIP64 locator declaring a second part.
7. Entry names are valid UTF-8, and any name with a byte `0x80` or above has the UTF-8 flag set (bit
   11). Setting it on every entry, ASCII names included, is always correct.
8. Entry paths contain no `..`, no `.` and no empty segments, do not start with `/`, contain no drive
   letter and no backslash, and contain no characters below `0x20` and no `0x7f`.
9. An entry's local header agrees with its central-directory record: the same name bytes, the same
   compression method, the same state of the data-descriptor flag (bit 3). Sizes and `crc32` may travel
   in a data descriptor after the entry — the real sizes must then be carried by the central directory,
   because that is where they are read from and nowhere else.
10. A directory entry has a trailing `/` and zero length — both in the archive and in the data. **A rule
    for whoever writes the archive:** a directory with something in it is recreated from the paths of
    what lies in it, so a separate entry is the only way an empty directory can exist; a reader cannot
    tell a missing entry from a directory that never existed.
11. Where a 32-bit field is saturated (`0xFFFFFFFF`, and `0xFFFF` for a count of entries), the real
    value is given by the corresponding ZIP64 record: extra field `0x0001` on the entry plus a ZIP64
    end-of-central-directory record with a locator in the tail.
12. The central directory holds exactly as many entries as it declares, entry data does not run into it,
    and the end-of-central-directory record ends the file: the comment length declared in it reaches
    exactly to the end — a non-empty, correctly declared comment is accepted by the reader. **A rule for
    whoever writes the archive:** write no comment, append nothing after that record — and nothing
    before the first one either, neither a self-extracting stub nor any other prefix.

The `crc32` in the central directory is compared with nothing: the integrity of an archive is held by
the three SHA-256 digests above, not by the container.

### Where this format is defined

The format belongs to the two client tools that read an archive — the `d8` client and the Deckhouse web
console — rather than to the module's HTTP API the rest of this document is about. When the format
changes, `formatVersion` in `snapshot.yaml` changes. Treat `d8 snapshot local describe` run against a
real archive, plus one upload through the console, as the authority, and repeat both after a tool
update.

## What this document does not cover

- **Resolving conflicts during a restore.** The read that hands back a subtree in the shape it would be
  applied in is granted and is described in [Restoration, in brief](#restoration-in-brief); what to do
  with an object that already exists is not described here, and the recommended place to decide it is
  the web console.
- **The wire protocol volume bytes travel over.** Creating a transfer and watching it are here; the
  requests carrying the bytes — the endpoints, the two transfer modes, the explicit `finished` call and
  the status fields publishing the address — belong to the `storage-foundation` module and are described
  in its documentation, on the page named in [Volume data](#volume-data). For a tree of `Snapshot` and
  `VolumeSnapshot` nodes that is the only document needed besides this one. Two others may be needed
  depending on circumstances: the module's user guide, if you exclude objects from a capture, and the
  documentation of a domain module, if the tree contains its nodes (the next item).
- **Nodes contributed by other modules.** A subtree created by another Deckhouse module (virtual
  machines and their disks, for instance) is walked through the endpoint group of its own kind; what
  such nodes contain and how their data is moved is described by the module that creates them.
- **Moving a snapshot between clusters with nodes renamed.** Loading recreates the tree under the node
  names it was captured with. Renaming nodes along the way is not part of this contract.
- **The module's internal layer.** The cluster-scoped artifacts holding the captured bytes, their format
  and their garbage collection are internals; the roles in [Permissions](#permissions) do not address
  them, and their design is not a compatibility promise.
- **A directory `d8` is still downloading into.** Until a download is finished, the client's working
  files sit next to the finished ones — the `data.bin.d` and `data.tar.d` staging directories, the
  auxiliary `.part` and `.tmp` files the resume works through. The format does not describe them and
  they may change in any version, and a tree containing them is not an archive yet: its digests will not
  add up. An archive is what is left after a successful finish, and it is described in
  [The snapshot archive](#the-snapshot-archive).
