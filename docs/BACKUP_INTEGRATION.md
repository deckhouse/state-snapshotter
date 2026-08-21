---
title: "Backup integration"
description: "The HTTP and YAML contract an external backup product implements to create a snapshot, confirm it succeeded, download it, and load it back into a cluster."
d8Edition: ee
weight: 30
---

## Who this document is for

This document is the client contract for an **external backup product** that stores Deckhouse
namespace snapshots outside the cluster. It covers four operations:

1. create a snapshot;
2. confirm that the snapshot succeeded;
3. download a snapshot;
4. load a snapshot back into a cluster.

Those four are the whole HTTP contract. One more thing is specified here and it is not an operation:
[the archive format](#the-snapshot-archive) a stored snapshot is handed back to its owner as, so that
the person can load it with the `d8` command-line client or the web console without going through
your product at all.

Everything here is plain HTTP against the Kubernetes API server plus Kubernetes objects, so it can be
implemented in any language: no client library of ours is required, and no part of the contract depends
on one.

What stays outside your product is **restoring a captured namespace**. Applying a snapshot over live
objects raises conflicts — an object that already exists, a name taken by something else, a partially
applied namespace — and those are resolved interactively in the Deckhouse web console, not by an
unattended agent. Your side of the line ends at "the snapshot is stored, and it can be loaded back into
a cluster"; see [Restoration, in brief](#restoration-in-brief).

## Model

A snapshot is a namespaced `Snapshot` object. Creating one in a namespace captures **that namespace**:
the manifests of its user-owned objects and the data of its volumes, frozen into a durable artifact
that outlives the request object. A `Snapshot` never captures a different namespace, and it takes no
target-namespace parameter.

A snapshot is a **tree of nodes**:

- the root node is the `Snapshot` object itself;
- every node lists its direct children in `status.childrenSnapshotRefs`, each as an
  `apiVersion` / `kind` / `name` triple. The children live in the same namespace as their parent — a ref
  carries no namespace because it never differs;
- a node holds the manifests of its own objects and at most one volume's data. In a namespace capture,
  the nodes that carry volume data are `VolumeSnapshot` objects;
- reading a snapshot goes one node per request: the download endpoint returns the addressed node's own
  manifests and never walks the tree, so the walking is the client's. The restoration read is the one
  endpoint that answers with a whole subtree, and it is not part of this contract — see
  [Restoration, in brief](#restoration-in-brief). That is the shape every operation below has.

A `Snapshot` is **one-shot and immutable**. The spec is frozen at creation, the namespace is captured
exactly once, and there is no re-capture: a new snapshot means a new object.

Everything a client addresses is **namespaced** — the objects and the endpoints alike. The durable
artifacts holding the captured bytes are cluster-scoped and are module internals: they are not part of
this contract, the permissions below do not open them, and a client never reads them. If you find
yourself needing a cluster-scoped resource of this module, the operation you are attempting is not one
of the four above.

## API groups and versions

| What you address | API group | Version |
|---|---|---|
| `Snapshot` objects | `state-snapshotter.deckhouse.io` | `v1alpha1` |
| Node endpoints of `Snapshot` nodes | `subresources.state-snapshotter.deckhouse.io` | `v1alpha1` |
| `VolumeSnapshot` objects (volume nodes) | `snapshot.storage.k8s.io` | `v1` |
| Node endpoints of volume nodes | `subresources.storage-foundation.deckhouse.io` | `v1` |
| `DataExport` / `DataImport` objects (volume bytes) | `storage-foundation.deckhouse.io` | `v1alpha1` |

**The two node-endpoint groups are on different versions, and this is the most common client mistake.**
The group that serves `Snapshot` nodes is on `v1alpha1`; the group that serves volume nodes is on `v1`.
Addressing the second with `v1alpha1` does not fall back to anything — it is a path the API server does
not serve, and the request fails. Derive the version from the group, never from the group you used last.

**A volume node's endpoints are served by another module**, the one that owns the `VolumeSnapshot` kind,
which is why their group is named after that module and not after this one. Nothing about a request
changes because of it: the endpoint names, the bodies and the statuses are the same, and both groups are
reached through the same API server address.

The endpoint names are the same in both groups (`manifests-download`,
`manifests-and-children-refs-upload`, `manifests-with-data-restoration`), so the group, the version and
the resource name are the only things that change between a `Snapshot` node and a volume node.

> **Deprecated: `subresources.snapshot.storage.k8s.io`.** The volume-node endpoints were served under
> that group before they moved to the one above. Requests to it are still answered, identically, and the
> answer carries a deprecation warning; the group will be removed in a future release of the platform.
> Move to `subresources.storage-foundation.deckhouse.io` — there is nothing else to change.

## Permissions

Give the product its own `ServiceAccount` in every namespace it works in, and bind four capability
roles of the snapshot module plus one of the volume-data module to it. The roles are `ClusterRole`
objects; a `RoleBinding` makes them effective in one namespace only, which is what you want — a backup
agent has no business in namespaces it was not pointed at.

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
  name: backup-agent-view-snapshots
  namespace: my-app
subjects:
  - kind: ServiceAccount
    name: backup-agent
    namespace: my-app
roleRef:
  kind: ClusterRole
  name: d8:use:capability:module:state-snapshotter:view_snapshots
  apiGroup: rbac.authorization.k8s.io
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: backup-agent-manage-snapshots
  namespace: my-app
subjects:
  - kind: ServiceAccount
    name: backup-agent
    namespace: my-app
roleRef:
  kind: ClusterRole
  name: d8:use:capability:module:state-snapshotter:manage_snapshots
  apiGroup: rbac.authorization.k8s.io
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: backup-agent-download-snapshots
  namespace: my-app
subjects:
  - kind: ServiceAccount
    name: backup-agent
    namespace: my-app
roleRef:
  kind: ClusterRole
  name: d8:use:capability:module:state-snapshotter:download_snapshots
  apiGroup: rbac.authorization.k8s.io
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: backup-agent-upload-snapshots
  namespace: my-app
subjects:
  - kind: ServiceAccount
    name: backup-agent
    namespace: my-app
roleRef:
  kind: ClusterRole
  name: d8:use:capability:module:state-snapshotter:upload_snapshots
  apiGroup: rbac.authorization.k8s.io
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: backup-agent-transfer-volume-data
  namespace: my-app
subjects:
  - kind: ServiceAccount
    name: backup-agent
    namespace: my-app
roleRef:
  kind: ClusterRole
  name: d8:use:capability:module:storage-foundation:transfer_volume_data
  apiGroup: rbac.authorization.k8s.io
```

| Role | What the client needs it for |
|---|---|
| `d8:use:capability:module:state-snapshotter:view_snapshots` | Read `Snapshot` objects and the `VolumeSnapshot` objects that stand for volume nodes. Walking a tree is reading node statuses, so this is what makes the walk possible. |
| `d8:use:capability:module:state-snapshotter:manage_snapshots` | Create a `Snapshot` and delete it. There is no update: the spec is immutable, and undoing a snapshot is deleting it, which takes the whole tree with it. |
| `d8:use:capability:module:state-snapshotter:download_snapshots` | Read a node's manifests, in both node-endpoint groups. |
| `d8:use:capability:module:state-snapshotter:upload_snapshots` | Write a node's manifests back, in both node-endpoint groups, and create the `VolumeSnapshot` objects of an imported tree — those are created by the client, not by the module. |
| `d8:use:capability:module:storage-foundation:transfer_volume_data` | Create `DataExport` and `DataImport` objects and move volume bytes through their HTTP endpoints. |

`upload_snapshots` and `transfer_volume_data` are not folded into any cluster-wide access level: they
take effect exactly where they are bound, and binding them is the only way to obtain them.

The role `d8:use:capability:module:state-snapshotter:restore_snapshots` is **deliberately not in the
set**. It grants the recursive restoration read that rewrites a whole subtree for a target namespace —
the operation that belongs to the web console, where its conflicts are resolved by a person. A backup
product does not need it to store snapshots or to load them back, so it is not granted, and the
restoration endpoint answers `403` to the service account above.

The set covers this module and the transfer of volume data, and nothing else. A snapshot tree may also
contain nodes contributed by other Deckhouse modules — a virtual machine and its disks, for example —
and those nodes are that module's resources. Reading one takes two grants from the module that produces
it: `get` on its own kind, because the node object is where `status.childrenSnapshotRefs` continues the
walk, and `get` on its manifests endpoint in its own aggregated group. Without them the walk stops with
`403` at the first such node, and no role listed above changes that. Where only namespace workloads are
captured, the question does not arise.

Get a token for the service account the usual way:

```shell
d8 k -n my-app create token backup-agent --duration=24h
```

Every request below carries it as `Authorization: Bearer <token>` and goes to the cluster's API server.
Take that server's address and its CA certificate from the kubeconfig you were given — `cluster.server`
and `cluster.certificate-authority-data`. An agent running inside the cluster needs no kubeconfig for
either: the address is in the environment variables `KUBERNETES_SERVICE_HOST` and
`KUBERNETES_SERVICE_PORT`, the CA is the file
`/var/run/secrets/kubernetes.io/serviceaccount/ca.crt`, and its own mounted token is
`/var/run/secrets/kubernetes.io/serviceaccount/token`. Verify the server certificate against that CA:
the responses carry the namespace's secrets in the form they were captured.

## 1. Create a snapshot

Create a `Snapshot` in the namespace you want to capture:

```yaml
apiVersion: state-snapshotter.deckhouse.io/v1alpha1
kind: Snapshot
metadata:
  name: my-namespace-snapshot
  namespace: my-app
spec:
  mode: Capture
```

The `spec` has exactly one field a client sets, `spec.mode`:

| Value | Meaning |
|---|---|
| `Capture` (default) | Capture the live namespace this object lives in. |
| `Import` | Do not capture anything: the snapshot is materialized from a payload the client uploads. This is the mode of [operation 4](#4-load-a-snapshot-back-into-a-cluster). |

`spec: {}` is the same as `spec.mode: Capture`.

> **Spell the field name exactly, and read it back.** The API server prunes fields it does not know
> **before** validation runs, so a misspelled field is not rejected: the object is accepted, the field is
> dropped, and the object behaves as if you had never sent it. Sending a misspelled import marker gets
> you a live capture of the namespace answered with "created". After creating the object, read
> `spec.mode` back from it and compare with what you sent — that is the only way to see the difference.

To keep individual objects out of a capture, they are labelled before the snapshot is created. The label
and its exact semantics are in the [User Guide](USER_GUIDE.md); a capture is one-shot, so labelling an
object afterwards changes nothing.

## 2. Confirm that the snapshot succeeded

Poll the `Snapshot` object. The success signal is its `Ready` condition:

```console
NAME                    READY   REASON      CONTENT                        AGE
my-namespace-snapshot   True    Completed   nss-content-1f4b0c9d2a3e5f76   30s
```

- `status.conditions[type=Ready]` with `status: "True"` and `reason: Completed` — the capture is
  complete and durably stored. This is the only success signal; there is no other flag to check.
- `status.boundSnapshotContentName` becomes non-empty when the durable artifact is bound. It is a
  progress signal, not something a client reads: the artifact it names is cluster-scoped and outside
  this contract.
- `status.childrenSnapshotRefs` lists the direct children of the root and is what
  [operation 3](#3-download-a-snapshot) walks. It may legitimately be empty (a namespace with no
  volumes and no subtrees contributed by other modules).

The capture is **fail-closed**: there is no such thing as a partial snapshot. If some part of the
namespace cannot be read or stored, the object simply never reaches `Ready=True`. A client must never
treat "not ready yet" as "ready enough".

While `Ready` is `False`, the condition's `reason` says what state the capture is in:

| Reason | Class | What it means |
|---|---|---|
| `ArtifactMissing` | Terminal | The durable artifact holding a captured volume's data is gone or is being deleted; that data cannot be recovered for this snapshot. |
| `ChildSnapshotLost` | Terminal | A node of the tree is gone beyond recovery. |
| `ChildrenFailed` | Terminal | A node below this one failed terminally; that node's own reason says why. |
| `CreateChildFailed` | Terminal | Creating a node of the tree failed. |
| `DomainCaptureFailed` | Terminal | Capture of a subtree contributed by another module failed; the message carries that module's own reason. |
| `DuplicateCoveredPVCUID` | Terminal | The capture plan covered the same volume twice, so it was refused instead of producing an ambiguous snapshot. |
| `GraphPlanningFailed` | Terminal | Planning the snapshot tree failed. |
| `ListFailed` | Terminal | A resource type in the namespace could not be listed, so the capture would have been incomplete. |
| `ManifestCheckpointFailed` | Terminal | Storing the captured manifests failed. |
| `NamespaceNotFound` | Terminal | The namespace being captured does not exist. |
| `VolumeCaptureFailed` | Terminal | Capturing a volume's data failed. |
| `ChildSnapshotDeleted` | Recoverable | A node object of the tree was deleted while its stored data survives. The capture is complete and the data is intact, but the node is no longer reachable through the namespaced surface; recovering it takes manual intervention. |

- **Terminal** means this snapshot will never become ready. Stop polling, report the failure, and create
  a new `Snapshot` if you want to retry — the spec is immutable, so there is nothing to fix in place.
- **Recoverable** means the captured data is intact and the object degraded without failing. It will not
  heal by itself and re-polling does not help; it needs a person.
- **Any other reason is not in the contract.** The two sets above are closed and stable; everything else
  is either progress ("still capturing") or a free-form reason coming from another module's controller,
  and new ones may appear without notice. Treat an unknown reason as "in progress" only while something
  is actually progressing — the reason or the message keeps changing, `status.boundSnapshotContentName`
  appears, children appear. If nothing changes, fall back to your own timeout and report a failure;
  do not wait forever on a reason you do not recognize.

## 3. Download a snapshot

A download is a tree walk. Start at the root `Snapshot` and, for every node:

1. read the node object and take its `status.childrenSnapshotRefs`;
2. read the node's own manifests with `manifests-download`;
3. repeat for every child ref, in the same namespace.

`manifests-download` returns the manifests of **that node only** — it never walks the subtree — and it
returns them **as they were captured**, including `status` and other runtime fields. Reading the whole
snapshot means visiting every node yourself.

The endpoint to call is chosen by the node's kind:

| Node | Request |
|---|---|
| The root, or any `Snapshot` node | `GET /apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/{namespace}/snapshots/{name}/manifests-download` |
| A `VolumeSnapshot` node | `GET /apis/subresources.storage-foundation.deckhouse.io/v1/namespaces/{namespace}/volumesnapshots/{name}/manifests-download` |

```shell
curl -sS -H "Authorization: Bearer ${TOKEN}" \
  "https://{apiserver}/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/{namespace}/snapshots/{name}/manifests-download"
```

A node contributed by another Deckhouse module (a virtual machine and its disks, for example) is
addressed through the endpoint group of the module that owns its kind. For a kind whose objects live in
that module's own API group — which is every such node except the volume node — the endpoint group is
`subresources.` followed by the node's own API group, taken from the `apiVersion` in the child ref that
led you to the node.

**The volume node is the exception, and deriving its group that way is wrong.** A `VolumeSnapshot` lives
in the upstream CSI group `snapshot.storage.k8s.io`, which belongs to no Deckhouse module, so its
endpoints cannot be named after it: they are under
`subresources.storage-foundation.deckhouse.io/v1`, as spelled out in every table here. There is no rule
to compute that from — take it from this document.

Asking for a node through the snapshot module's group does not work, and what comes back depends on what
the caller may ask for: with the permissions described above it is `403`, because no role there grants
another module's resource under this module's group; a caller who may ask gets a `404` whose message
states the rule — address the node through the group of the module that owns its kind. The message names
no group: this module knows the names of no other module's groups, so it can point at the rule but not at
an address. Build addresses from the tables here and from the node's own `apiVersion`.

Reading such nodes also takes permissions from the module that produces them (see
[Permissions](#permissions)). They are otherwise outside this document (see
[What this document does not cover](#what-this-document-does-not-cover)), but a walk must still cross
them to reach what is below.

### Volume data

Manifests describe a volume; they do not contain its bytes. To read the bytes of a volume node, create a
`DataExport` for it and download through the exporter's HTTP endpoint:

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

`DataExport.spec.targetRef` names the volume node, not the original volume. Wait for the object to
become ready, take the address and the CA certificate from its status, and read the data over HTTP with
the same bearer token.

**Which address exists depends on `DataExport.spec.publish`.** Left off, as above, the transfer is
reachable from inside the cluster only, at `status.url`. An agent running outside the cluster needs
`publish: true`; it then reads `status.publicURL`, which stays empty while `publish` is off. The same
choice, with the same consequence, applies to the `DataImport` of the load path — an off-cluster agent
that omits it has no address to write to.

`DataExport.spec.ttl` is an idle window: a download in progress keeps pushing it back, because the
timer is fed by the bytes of the transfer itself. Its namesake on the load path is the same window,
counted on the bytes you upload.

The endpoints, the filesystem and block modes, and the exact status fields are documented by the
`storage-foundation` module, in "Exporting and importing volume data over HTTP" — the same section that
names `d8:use:capability:module:storage-foundation:transfer_volume_data`, the role bound above, as the
preferred way to authorize these requests.

## 4. Load a snapshot back into a cluster

Loading is the mirror image of the walk: you recreate the tree object by object and push each node's
manifests back. Nothing is inferred from the payload — a node exists in the cluster because you created
its object.

1. **Create the root** `Snapshot` in the target namespace with `spec.mode: Import`. Nothing is captured
   for it; it waits for what you upload.

   ```yaml
   apiVersion: state-snapshotter.deckhouse.io/v1alpha1
   kind: Snapshot
   metadata:
     name: my-namespace-snapshot
     namespace: my-app-restored
   spec:
     mode: Import
   ```

2. **Create an object for every non-root node**, in the same namespace, also in import mode. A volume
   node is a `VolumeSnapshot`:

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

   The `ownerReferences` entry pointing at the **parent node** is not decoration and not optional: it is
   how the module learns where the node belongs. A node created without it is never bound to anything,
   its uploads keep failing, and nothing in the object says why. It is also what makes the tree
   disposable — deleting the root `Snapshot` deletes everything below it.

   A volume node carries no source: the data comes from the import described below, not from a live
   volume.

3. **Upload each node's manifests**, one request per node:

   | Node | Request |
   |---|---|
   | The root, or any `Snapshot` node | `POST /apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/{namespace}/snapshots/{name}/manifests-and-children-refs-upload` |
   | A `VolumeSnapshot` node | `POST /apis/subresources.storage-foundation.deckhouse.io/v1/namespaces/{namespace}/volumesnapshots/{name}/manifests-and-children-refs-upload` |

   The body carries the node's own manifests and the refs of its **direct** children; the children
   themselves are uploaded by their own requests. The exact shape is in
   [Request and response bodies](#request-and-response-bodies).

   Until the module has bound the node you are uploading to, the request answers `409` with reason
   `ImportContentNotBound`. **This is the normal path, not an error**: the binding happens
   asynchronously, so retry with a backoff. Repeating an upload is safe (see the same section).

4. **Bring in the volume bytes, one node at a time.** For each volume node in turn: create its
   `DataImport`, wait for it to become ready and to publish its address, upload the bytes over that
   HTTP endpoint, **tell the importer the upload is over**, and then wait for the `DataImport` to
   report that it has finished. Only then start the next node.

   The end of the upload is an explicit `POST` to the importer's `finished` endpoint. Nothing infers it
   from the last byte written, so a client that uploads and then polls, waiting for the object to
   notice, waits forever. The request, the two transfer modes and the addresses they use are in the
   "HTTP API: data import" section of the `storage-foundation` module's documentation.

   ```yaml
   apiVersion: storage-foundation.deckhouse.io/v1alpha1
   kind: DataImport
   metadata:
     name: my-volume-node
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

   `DataImport.spec.snapshotRef` names the volume node this import fills — exactly one `DataImport` per
   node — and `DataImport.spec.storageParams` describes the volume to be created for the data. Take
   those values from the captured manifest of the original claim, downloaded with that node's manifests.

   > **`DataImport.spec.ttl` is an idle window, not a budget for the whole transfer.** It is counted
   > from the last activity on this import, and an upload in progress is activity: while your bytes
   > keep arriving the window keeps moving forward, so a transfer longer than the value is not cut
   > off. What runs the window out is a pause — the importer serving and nothing being written to it.
   > When it does run out, the import is marked expired and its serving side is taken down; the node
   > it was to fill never gets its data, and the way on is a new `DataImport` for that node — a fresh
   > one, uploaded from the beginning: nothing of an expired import's partial upload is kept.
   >
   > So size the value for the longest pause you expect rather than for the whole transfer: waiting
   > for the importer to come up, and the gap between a dropped connection and your next attempt.
   > And create each import immediately before writing to it — imports created for a whole tree in
   > advance stand idle waiting their turn, which is exactly what this window measures.

   **Watch the import, not only the root.** Whether a node's data arrived is visible on the `DataImport`
   you created: its `status.phase` ends at `Completed` when the data is in, and at `Expired` (the window
   ran out) or `Failed` (an error) when it is not.

   The tree does say why, but in words only. When an import stops in either of those phases without
   producing the node's data, the message of that node's `Ready` condition names the `DataImport`
   and its phase, and the `Ready` message of the nodes above it, up to the root `Snapshot`, carries
   the same text next to the name of the child it came through — so an operator reading the root is
   not left with an unexplained hang. One such quote per message: when several nodes are waiting at
   once, the rest stay listed by name only. **The reasons do not change:** the node keeps
   `reason: DataCapturePending` and the root keeps `reason: ChildrenPending`, exactly the values
   they carry while the upload is still going. There is no reason meaning "this import will not
   deliver", so do not wait for one to appear, and do not parse the message to recover it — message
   text is written for people and is not part of this contract. A client that has to tell a transfer
   in progress from a dead one reads the `DataImport`'s own `status.phase`, which is the
   machine-readable answer.

   The volume side of a node binds only after its bytes have actually arrived: an importer that was
   created and never written to leaves its node unfinished.

5. **Wait for the root** `Snapshot` to reach `Ready=True` with reason `Completed`, exactly as in
   [operation 2](#2-confirm-that-the-snapshot-succeeded). It becomes ready when every node has both its
   manifests and its data.

> **Order matters.** Waiting for the root to become ready before the volume bytes are uploaded hangs:
> the root cannot be ready while a node below it is still waiting for its data. Manifests first, then
> the data of each volume node in turn, then the wait.

## Restoration, in brief

The module also serves a restoration read: `manifests-with-data-restoration` (`GET`) returns a whole
subtree at once, rewritten for a target namespace given as a query parameter — runtime metadata and
`status` stripped, objects re-addressed into the target — so the response can be applied as-is.

It is not part of this contract. Applying a captured namespace onto a live cluster is where conflicts
live, and those are resolved interactively in the Deckhouse web console. Accordingly the role set above
does not include `restore_snapshots`, and this endpoint answers `403` to the service account described
in [Permissions](#permissions).

## Request and response bodies

**`manifests-download` response.** `Content-Type: application/json`; the body is a **plain JSON array of
objects** — not a Kubernetes `List`, not a YAML stream — holding the addressed node's own objects
verbatim as captured, `status` and runtime metadata included. Iterate the array yourself; do not expect
`items`.

**Upload request body.** `Content-Type: application/json`:

```json
{
  "manifests": [ { "apiVersion": "v1", "kind": "ConfigMap", "metadata": {} } ],
  "childRefs": [
    { "apiVersion": "snapshot.storage.k8s.io/v1", "kind": "VolumeSnapshot", "name": "my-volume-node" }
  ]
}
```

- `manifests` is required and must be a JSON array — the same array `manifests-download` returned for
  that node. An absent, `null` or non-array value is rejected with `400`.
- `childRefs` lists the node's **direct** children only, each with all three of `apiVersion`, `kind` and
  `name`; a missing field is rejected with `400`. The children's namespace is implicit — it is the
  node's own namespace. An empty list is valid and means a leaf.
- A volume node is always a leaf: a non-empty `childRefs` on it is rejected with `400`.

**Upload response.** HTTP `200` with a Kubernetes `Status` object:

```json
{
  "kind": "Status",
  "apiVersion": "v1",
  "status": "Success",
  "manifestCheckpointName": "..."
}
```

The name it reports is the module's internal record of the stored manifests; a client needs nothing from
it beyond the `"Success"`.

**Retries are safe.** Repeating an upload with the same body is harmless: recording the children skips
the write when they already match, and a node whose manifests are already stored is left untouched. So a
request whose outcome you did not observe — a dropped connection, a timeout — can simply be sent again.
What is *not* safe is sending a *different* body for a node that already stored one; treat each node's
payload as fixed once you start.

**Body size.** An upload request body is capped at 64 MiB and a larger one is rejected with `413`. The
cap applies to a single node's own manifests, not to the whole tree, so it is reached only by a node
with an extraordinary number of objects.

**Compression.** Responses honour `Accept-Encoding: gzip` and answer with `Content-Encoding: gzip`.
Manifest payloads compress very well; on a large namespace this is the difference worth taking.

## Errors and retries

Every failure is a Kubernetes `Status` object carrying `code`, `reason` and a `message` that names the
object involved. Read the `reason`, not the message text: messages are written for people.

| Code | When | What to do |
|---|---|---|
| `400` | A malformed upload body (`manifests` absent or not an array, a child ref missing a field, a non-empty child list on a volume node); an invalid query parameter. **Also: reading a volume node whose durable content is not bound yet.** | Fix the body — except the unbound case, which is a timing state: retry with a backoff. |
| `403` | The permissions do not cover this request. Three shapes of that: a genuinely missing binding; a node of another module addressed through this module's group, which no role here grants; and a wrong HTTP method, which is authorized as a different verb (see below). Also: the durable content a node points at does not point back at that node. | Check the address and the method before the bindings, then the bindings against [Permissions](#permissions). The last case is a refusal by design — a node may only read the content that claims it — and does not clear on retry. |
| `404` | No such object. Also — but only for a caller allowed to ask for the address it used — an endpoint name that does not exist, or a node addressed through a group that does not serve it. Both of those arrive as `403` under the permissions above, because the name in the URL is what gets authorized: a misspelled subresource is a subresource no role grants. | Fix the address from the endpoint tables in [Download a snapshot](#3-download-a-snapshot) and the node's own `apiVersion`. The message states the rule but names no group, so there is nothing in it to copy into a URL. |
| `405` | The wrong HTTP method — reads are `GET`, uploads are `POST` — asked by a caller that may use both on that endpoint. | Fix the method. With the permissions above the same mistake arrives as `403`; see below. |
| `409` with reason `ImportContentNotBound` | An upload arrived before the module bound that node. | Expected during an import. Retry with a backoff; it clears on its own. |
| `409` (other reasons) | Reading a `Snapshot` node whose durable content is not bound yet; an upload addressed at an object that is not in `Import` mode; a node whose stored manifests are not readable yet; the same object present twice in a node's manifests. | Retry the first three with a backoff. A duplicate object is a defect in the payload and does not clear. |
| `413` | The upload body exceeds 64 MiB. | Not retryable as-is. |
| `502` | Only on a volume node's endpoints. The module serving them could not reach the snapshot module's stored manifests at all — no answer came back, so nothing is known about the node. It is the upstream-failure counterpart of `503`: the server you addressed is up, the one behind it did not answer. | Retry with a backoff. Nothing about the request needs changing. |
| `503` | The module's aggregated API is not serving right now (it is starting, restarting, or its snapshot registry is not warm yet). | Retry with a backoff. |

**A wrong method usually arrives as `403`, not `405`.** Every request is authorized by the verb its
method maps to — `GET` to `get`, `POST` to `create` — on the endpoint being addressed, and the roles
above grant `get` on the download endpoints and `create` on the upload ones, exactly those. A `POST` to
a download endpoint, or a `GET` to an upload one, is therefore refused by authorization before any
handler is reached. `405` is real, but it is what a caller holding both verbs would see. With the
documented permissions, a `403` on an address that looks correct is worth checking the method for
before checking the bindings.

**The `409` / `400` split is about reads.** On `manifests-download`, a node whose durable content is not
bound yet answers `409` when it is a `Snapshot` node and `400` when it is a volume node. The two mean
the same thing — the module has not finished binding that node — and are retried the same way. On
upload the answer is `409` with reason `ImportContentNotBound` for either kind of node. Do not build a
client that treats `400` as fatal across the board.

## The snapshot archive

A snapshot you store can be handed back to its owner as a file tree, so that the person restores it
themselves with the `d8` command-line client or with the Deckhouse web console instead of going
through your product's API. This section is the format those two tools read: the tree on disk, the
volume payload, the per-node envelope, the digests that authenticate it, and the rules for packing
the tree into a single `.zip`.

Nothing here changes [operation 4](#4-load-a-snapshot-back-into-a-cluster) — that is how *your*
product loads a snapshot back. This section is for the archive you give to a person.

**What the two tools accept today, and nothing beyond it:**

| Tool | Accepts |
|---|---|
| `d8 snapshot upload -n <namespace> -i <directory>` | A local **directory** holding the tree below. It does not read a `.zip` or any other container. |
| Deckhouse web console | Either a **directory** the user picks, or a single **`.zip`** file (the name must end in `.zip`) holding the same tree. |

The two are separate implementations of one format, and they are not identical. Where they differ it
is said below, at the rule it affects; the safe target is whatever both accept.

### The tree

Exactly one top-level directory, whose name is free — it is the root node's directory. Every node,
root and child alike, is a directory with the same shape:

```text
my-namespace-snapshot/                      # the root node
├── snapshot.yaml                           # the node envelope
├── manifests/                              # the node's own manifests, one file per object
│   └── configmap_app.yaml
└── snapshots/                              # direct children; absent when the node has none
    ├── volumesnapshot_data-0/              # a volume node with a block volume
    │   ├── snapshot.yaml
    │   ├── manifests/
    │   │   └── persistentvolumeclaim_data-0.yaml
    │   └── data.bin
    └── volumesnapshot_files-0/             # a volume node with a filesystem volume
        ├── snapshot.yaml
        ├── manifests/
        │   └── persistentvolumeclaim_files-0.yaml
        └── data.tar
```

- **`snapshot.yaml`** — the node envelope, described below. Required on every node.
- **`manifests/`** — the node's own manifests, exactly the array
  [`manifests-download`](#3-download-a-snapshot) returned for that node, one object per file, every
  file name ending in `.yaml`. The names are otherwise free — but they are **hashed** into the node's
  content digest, so once that digest is computed they can no longer change. What the `d8` client
  writes is `<kind in lower case>_<name>.yaml`, qualified as
  `<kind in lower case>.<api group>_<name>.yaml` when a file of that name already holds an object
  from a different API group. Do not lean on that scheme to keep objects apart: two objects of the
  same kind and name from the **core** group both resolve to the unqualified name, and the second
  overwrites the first.
- **`snapshots/`** — one subdirectory per **direct** child, absent when the node has none. The
  directory name is not read by anything: a child is any subdirectory that contains a
  `snapshot.yaml`, and its identity is taken from that envelope. A subdirectory without one is
  skipped rather than refused. The `d8` client happens to name it
  `<kind in lower case>_<source object name>`, falling back to the node's own name and appending
  `__<short>` on a collision — a convention of that client, not a rule of the format, so do not
  validate a tree against it.
- **At most one volume payload** — `data.bin`, `data.bin.<codec>` or `data.tar`, in the node's own
  directory, never two of them. See [The volume payload](#the-volume-payload).

**Three shapes are refused whole rather than partially loaded, and each of them is a natural archive
to build:**

- **The root of an archive uploaded in one piece must be a namespace snapshot** — envelope
  `apiVersion: state-snapshotter.deckhouse.io/v1alpha1`, `kind: Snapshot`. An "archive of one
  volume", whose root is the volume node itself, is not loadable that way. Such a node can be
  uploaded on its own, into a tree that already exists, but it cannot stand as the root of a whole
  archive.
- **A volume node must carry its bytes.** A node whose envelope is
  `apiVersion: snapshot.storage.k8s.io/v1`, `kind: VolumeSnapshot` and whose directory holds neither
  `data.bin` nor `data.tar` fails the whole archive — the manifests-only archive you get by walking
  the tree and skipping the volume data is not a lighter archive, it is an unloadable one.
- **The `volumes` field follows the payload exactly.** A node with a payload carries exactly one
  entry; a node without one omits the field entirely. Either mismatch is refused.

Finally, a matter of hygiene rather than of validity: `data.bin.d/`, `data.tar.d/` and
`identity.json` are the `d8` client's working artifacts of a download that has not finished. Both
tools ignore them, so they will not fail an archive — they will only ship megabytes of nothing to
your customer. A `data/` directory is different and does fail: the web console reads it as a node
carrying several volumes and refuses the node.

### The volume payload

A node with a volume carries exactly one payload, and the single `volumes` entry of its envelope
describes it.

**A block volume is `data.bin`** — the volume's bytes exactly as the export served them, which for a
raw block export is the whole device. A compressed payload keeps the codec in the name:
`data.bin.gz`, `data.bin.lz4`, `data.bin.zst`. The web console decompresses `data.bin.zst` only and
refuses `data.bin.gz` and `data.bin.lz4`, which the `d8` client reads — so an archive meant for a
console user is either uncompressed or compressed with zstd.

**`size` in the envelope must equal the payload's raw byte count exactly.** Both tools recompute it —
from the file's length when the payload is uncompressed, and from the compressed stream's declared
content size for `data.bin.zst` — and refuse the archive when the two disagree by a single byte. A
value rounded to a comfortable quantity is the likeliest way an archive fails after satisfying
everything else. Take the number from the transfer, not from a manifest: `HEAD /api/v1/block` reports
it in `Content-Length`, and the bytes you counted while downloading come to the same figure. See
[Volume data](#volume-data).

**A zstd payload must be a stream both tools can measure, which a shell pipeline does not produce.**
Every frame has to declare its `Frame_Content_Size`, because the sum of those declarations is what is
compared with `size`; a compressor reading from a pipe omits that field, while `zstd` given the file
as an argument writes it. There must also be no skippable frames and no dictionary, and the declared
window and the frame count stay within [Limits](#limits).

**None of that is console-only strictness, though the two tools catch it at different moments.** Both
walk every frame before a byte leaves the machine, so a missing content size, a skippable frame or a
malformed frame layout fails before the upload starts. A dictionary and an over-large window fail
there too in the web console, while the `d8` client meets them later, when its decoder reaches them
part-way through the transfer. Either way the payload satisfies all three digests and is accepted by
`d8 snapshot local describe` — the bytes on disk are intact, they are merely not measurable — and is
then refused by whichever tool uploads it. Passing the file by name, `zstd data.bin`, is the whole
fix.

**A filesystem volume is `data.tar`**, a tar container that is itself never compressed, and what is
inside it is contract too. The export serves a filesystem volume one file per request, so this
container is assembled by whoever writes the archive — and an ordinary `tar` of the downloaded files
is refused, because the records below are missing. Every regular file entry carries three PAX
records:

| PAX record | Value |
|---|---|
| `D8.snapshot.fs.codec` | The codec applied to this entry: `none`, `zstd`, `gzip` or `lz4`. One and the same codec throughout the tar — a mixed one is refused. The web console additionally reads `none` and `zstd` only. |
| `D8.snapshot.fs.originalPath` | The file's path inside the volume: relative and portable — no leading `/`, no backslash, no drive letter, no empty, `.` or `..` segment, no character below `0x20` or equal to `0x7f`, never opening with the reserved segment `.d8-meta`, and already normalized, so that collapsing `.` and `..` leaves it unchanged. |
| `D8.snapshot.fs.rawSize` | The entry's byte count before compression, in plain decimal with no leading zeros. For codec `none` the tar entry's own size must equal it; for a compressed entry the decompressed bytes are counted and must come to exactly this number. |

The entry name is not free either: it must be `originalPath` followed by the codec's extension —
nothing at all for `none`, `.gz`, `.lz4` or `.zst` for the others.

**The same stream rules apply to every zstd-compressed entry inside `data.tar`.** Both tools walk
that entry's frames before uploading it and compare the decompressed length with the entry's
declared `D8.snapshot.fs.rawSize`, exactly as they do for a block payload against `size`. A per-file
pipeline is the natural thing to reach for here, because the export hands the files over one at a
time — and it is the same trap.

Directories travel as ordinary tar directory entries and carry no PAX records, but their names obey
the same rules as `originalPath` above — the trailing `/` aside. That is worth spelling out because of
one habit: `tar -C /volume -cf data.tar .` writes its entries as `./`, `./sub/` and so on, and every
one of them is refused on the `.` segment. Name the entries from the volume root instead, with no
leading `./`.

An entry that is neither a regular file nor a directory — a symlink, a hard link, a device, a fifo —
is refused, and so is an empty directory, unless the person driving the upload explicitly asks for
such entries to be skipped. The one exemption is a directory whose last segment is `lost+found`, at
any depth.

### The node envelope

`snapshot.yaml` records what the node is and commits to what it contains. Both tools verify it
before writing anything into a cluster, and an archive whose envelope does not check out is refused
whole.

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

**Write plain YAML scalars, and omit what you do not have.** The two tools do not read this file
identically, so keep to the forms they agree on. `formatVersion: 2` is a number: the quoted
`formatVersion: "2"`, which the console accepts, the `d8` client refuses outright. An optional field
you have no value for is left out, never written as `null` or `~`: the console reads an explicit null
as the four-character string `null`, and `metadataChecksum` then fails to reproduce. Anchors and
aliases are capped — see [Limits](#limits).

| Field | Rule |
|---|---|
| `formatVersion` | `2`. An envelope without it is a pre-versioned archive whose metadata is unauthenticated: the web console refuses such an archive outright, and the `d8` client refuses it unless it is explicitly told to accept an unauthenticated legacy one. |
| `apiVersion`, `kind`, `name` | The node object's own identity, as it appears in the child ref that led to the node. All three are required, and for the two kinds named above they also decide what the node must carry. |
| `namespace`, `uid` | The node object's namespace and UID. Optional, but whatever you write here is part of the parent's commitment below, so write it consistently. |
| `sourceName`, `sourceObjectRef` | The captured source object, recorded for readability. Optional; `sourceObjectRef` sets all three of `apiVersion`, `kind`, `name` or is omitted. |
| `checksum` | Digest of the node's own content. Required. |
| `childrenChecksum` | The node's commitment to the exact set of its direct children. Required, including on a leaf. |
| `metadataChecksum` | Digest of this envelope. Required. |
| `volumes` | Exactly one entry on a node that has a payload, and the field is omitted entirely on a node that has none. Two entries are refused. |

Every digest is written the same way: `algorithm` is `sha256`, `hex` is 64 lower-case hex
characters, and `short` is exactly the first 8 characters of `hex`.

A `volumes` entry describes the volume the payload holds, and it is what the load path rebuilds the
`DataImport` from — so it must survive the round trip through your storage:

| Field | Rule |
|---|---|
| `target` | Identity of the captured claim: `apiVersion`, `kind`, `name` are required, `namespace` and `uid` follow the claim. It is the same object whose manifest sits in this node's `manifests/`. |
| `artifact` | Identity of the stored content the node was bound to at capture time — for a volume node, the `VolumeSnapshotContent` named in `status.boundVolumeSnapshotContentName`. `apiVersion`, `kind` and `name` must all be non-empty; their values are checked against nothing and the load path does not read them. This is provenance, and it is required so that an archive can always be traced back. |
| `volumeMode` | Required, and exactly `Block` for a `data.bin` payload or `Filesystem` for `data.tar`. An empty value is a mismatch like any other. |
| `storageClassName` | Required and non-empty. |
| `size` | The size of the volume to recreate, as a Kubernetes quantity: required and positive. On a block node it must additionally equal the payload's exact raw byte count — see [The volume payload](#the-volume-payload). On a filesystem node nothing compares it with `data.tar`; it only has to be large enough for the files that will be written into it. |

The YAML document itself is not covered by any digest — only the values in it are, so the keys may
be written in any order.

### Computing the digests

All three digests are SHA-256, and all three are computed from data you already have. Values below
are the ones the tools recompute and compare, so an archive assembled by hand is accepted exactly
when they match.

**`checksum` — the node's content.** It covers the files the node carries and nothing else:

- covered: every `manifests/*.yaml` file and the single payload (`data.bin`, `data.bin.<codec>` or
  `data.tar`);
- not covered: `snapshot.yaml` itself, the whole `snapshots/` subtree with every child in it, and
  anything under `manifests/` whose name does not end in `.yaml`.

For each covered file compute an independent `sha256(<path> + 0x00 + <file bytes>)`, where the path
is the file's path relative to the node's own directory (`manifests/configmap_app.yaml`, `data.bin`)
in UTF-8, followed by one zero byte, followed by the file's bytes verbatim. Then sort the covered
paths by byte order, feed the per-file digests — the raw 32 bytes of each, in that order — into a
final SHA-256, and write its lower-case hex as `checksum.hex`.

**`childrenChecksum` — the exact set of direct children.** This is what makes a swapped, added or
removed child visible on the parent instead of nowhere, so it is mandatory on every node. Feed one
SHA-256, in this order:

1. the ASCII string `d8-snapshot-children-checksum-v1` followed by one zero byte;
2. the number of direct children, as a decimal string, length-prefixed;
3. for every child, in the canonical order below, seven length-prefixed strings: its `apiVersion`,
   `kind`, `namespace`, `name`, `uid`, then its own `checksum.hex` and its own
   `childrenChecksum.hex`.

*Length-prefixed* means: the byte length of the UTF-8 string as an 8-byte big-endian integer,
followed by the bytes. An absent `namespace` or `uid` contributes an empty string — a length of zero
and no bytes. The canonical order sorts children by the tuple `apiVersion`, `kind`, `namespace`,
`name`, `uid` joined with `0x1F`, compared by bytes; two children with the same tuple are refused,
and so is a child count above the bound in [Limits](#limits).

A node with no children commits to the digest of the empty set, which is always
`926e82507890b7d5c7ad50f8cb7fd2172f81bfd2cf1d2ca86e3b644f66b02701`. Reproducing that constant is the
cheapest check that your implementation of this digest is right.

Because a child's digests go into its parent's, the envelopes are written bottom-up: leaves first,
then their parents, then the root.

**`metadataChecksum` — the envelope itself.** Serialize the envelope to JSON with `metadataChecksum`
left out entirely, and take the SHA-256 of those bytes. The serialization is fixed:

- compact, with no whitespace between tokens;
- keys in this order, which is not alphabetical: `formatVersion`, `apiVersion`, `kind`, `name`,
  `namespace`, `uid`, `sourceName`, `sourceObjectRef`, `checksum`, `childrenChecksum`, `volumes`;
- inside a digest: `algorithm`, `hex`, `short`. Inside `sourceObjectRef`: `apiVersion`, `kind`,
  `name`. Inside a `volumes` entry: `target`, `artifact`, `volumeMode`, `storageClassName`, `size`,
  and inside `target` and `artifact`: `apiVersion`, `kind`, `name`, `namespace`, `uid`;
- a field that may be omitted is left out entirely when empty, never written as `null`. The complete
  set is `formatVersion`, `namespace`, `uid`, `sourceName`, `sourceObjectRef`, `childrenChecksum`,
  `volumes`, then `volumeMode`, `storageClassName` and `size` inside a `volumes` entry, and
  `namespace` and `uid` inside `target` and `artifact`. Five of them never actually go missing from a
  valid envelope, because each is required wherever it can appear at all: `formatVersion` is `2`,
  `childrenChecksum` is written even on a leaf, and `volumeMode`, `storageClassName` and `size` are
  required in any entry that exists. What does disappear is `volumes`, on every node without a
  payload, and the optional identity fields. Every other key is written even when its value is
  empty;
- strings escape `"` and `\`, and — this is the part that is easy to miss — escape
  `<`, `>` and `&` as `\u003c`, `\u003e` and `\u0026`. Everything else, non-ASCII
  included, is written as UTF-8. Every field of an envelope is a Kubernetes identifier, a quantity
  or a hex digest, so in practice none of those three characters occurs at all; a value that needs
  more escaping than this has no place in an envelope.

The envelope above hashes to the `metadataChecksum` it carries, so it doubles as a test vector for
an implementation of this serialization.

**Check the result before you ship it.** `d8 snapshot local describe <directory>` walks an archive
and, on every node, verifies the content digest, the commitment to that node's direct children and
the envelope's own digest before printing anything — without a cluster and without changing the
archive. It also checks each envelope against the rules above, `volumes` matching the payload on disk
included.

Two things to know before planning around it. **It reads every payload byte**, because the content
digest covers the payload: the command costs a full SHA-256 pass over everything in the archive, a
multi-terabyte volume included. It is a verification, not a cheap look. And **it does not interpret
what it reads**: it never compares `size` with the payload's length, never parses `data.tar`, never
walks a zstd frame. An archive with a rounded `size`, with a plain `tar`, or with a pipe-compressed
`data.bin.zst` passes this command and is refused at upload — as does a volume node with no payload
at all, and an archive whose root is not a namespace snapshot, since neither is a question about a
single node. Nor does the command answer whether the web console will take the archive: the console is
stricter about codecs, and only opening a `.zip` proves the packing.

### Packing the tree into a `.zip`

The `.zip` is the console's container — the `d8` client neither writes nor reads one. The layout
inside is unchanged: an entry's path is the file's path in the tree above, with `/` as the only
separator. Most requirements below are checked when the file is opened, and a file that breaks one of
them is refused rather than partially read; the two marked as a writer's rule are the ones no reader
can check for you:

1. The file name ends in `.zip`, in any letter case.
2. The archive holds exactly one top-level directory — the root node's directory.
3. Every entry is **stored**: compression method `0`, in both the local header and the central
   directory. A compressed entry is refused, however small.
4. No entry is encrypted, with either the traditional or the strong encryption flag.
5. The archive is one part: disk numbers `0`, the entry count in the end-of-central-directory record
   equal to the total, and no ZIP64 locator claiming another part.
6. Entry names are valid UTF-8, and any name containing a byte `0x80` or above has the UTF-8 flag
   (bit 11) set. Setting it on every entry, ASCII ones included, is always correct and saves deciding
   per name.
7. Entry paths carry no `..`, no `.` and no empty segment, do not start with `/`, carry no drive
   letter and no backslash, contain no character below `0x20` or equal to `0x7f`, and are no longer
   than the path length in [Limits](#limits).
8. Each entry's local header agrees with its central-directory record: the same name bytes, the same
   compression method, and the same setting of the data-descriptor flag (bit 3).
9. A directory entry has a trailing `/` and zero length, compressed and uncompressed alike.
   **A writer's rule:** a directory holding anything is also reconstructed from the paths of what it
   holds, so the explicit entry is the only way an otherwise empty directory exists at all, and a
   reader cannot tell a missing one from a directory that never was.
10. For every entry the compressed size equals the uncompressed size, which is what "stored" means.
11. Where a 32-bit field is saturated (`0xFFFFFFFF`, or `0xFFFF` for the entry count), the matching
    ZIP64 record supplies the real value: extra field `0x0001` on the entry, and a ZIP64
    end-of-central-directory record with its locator at the tail.
12. The archive holds no more entries than [Limits](#limits) allows.
13. The central directory contains exactly the number of entries it declares and nothing after them,
    and no entry's data runs into it.
14. The end-of-central-directory record ends the file: its declared comment length reaches exactly
    the end. Write no comment, and append nothing after it.
15. **A writer's rule:** prepend nothing before the first entry. A stray prefix — a self-extracting
    stub — is read only when every offset in the archive already accounts for it, and the reader does
    not go looking for an archive that starts somewhere else.

Sizes and `crc32` may travel in a data descriptor after an entry (flag bit 3), which is what lets an
entry be written while its bytes are still streaming. Two rules come with that: both headers must
agree that the flag is set, and the central directory must carry the real sizes, because sizes are
read from there and nowhere else. The `crc32` in the central directory is never compared with the
bytes, so it buys nothing — the integrity of an archive rests on the three SHA-256 digests above, not
on the container.

### Limits

An archive that exceeds any of these is refused. Where the two tools set different bounds, the number
below is the smaller one: stay under it and both read the archive.

| What | At most |
|---|---|
| Nodes in one archive | `10000` |
| Depth of the tree, the root counting as zero | `64` |
| Direct children of one node | `10000` |
| Manifests in one node | `10000` |
| One metadata file — `snapshot.yaml` or one manifest | `16 MiB` |
| All metadata files of the archive together | `256 MiB` |
| The envelopes of one node's direct children together | `8 MiB` |
| YAML anchors and aliases in one document | `100` |
| Entries in a `.zip` | `100000` |
| Length of an entry path in a `.zip` | `4096` characters |
| Entries in one `data.tar` | `100000` |
| Length of a path inside `data.tar` | `16384` bytes |
| Frames in one zstd payload | `100000` |
| Window a zstd frame declares | `512 MiB` |

The window bound is the one that surprises people, because a compressor can be asked to exceed it:
`zstd --long=30` declares a window of a gigabyte and is refused. Default settings stay far below it.
On a frame that carries no window descriptor the web console measures the declared content size
against that same ceiling — a tightening by one tool rather than a rule of the format, and one more
reason the bound above is the one to build against.

These are the bounds the tools enforce today, not a promise that anything under them is accepted.

### Where this format is defined

The names and the rules in this section belong to the two client tools that read the archive — the
`d8` command-line client and the Deckhouse web console — and not to the module's HTTP API described
in the rest of this document. They are two implementations, which is why their disagreements are
called out at the rules they affect rather than summarized once. The envelope carries
`formatVersion` for the same reason: it is the field that changes when the format does. Treat
`d8 snapshot local describe` on a real archive, followed by one upload through the console, as the
authority, and repeat both after upgrading the tools rather than assuming that an archive accepted
last year is still accepted today.

## What this document does not cover

- **Restoring with conflict resolution.** Covered above: it belongs to the web console.
- **Nodes contributed by other modules.** A subtree produced by another Deckhouse module (virtual
  machines and their disks, for example) is walked through the endpoint group of its own kind; what
  those nodes contain and how their data is transferred is documented by the module that produces them.
- **Moving a snapshot between clusters with renamed nodes.** The load path recreates a tree under the
  node names it was captured with. Renaming nodes on the way in is not part of this contract.
- **The module's internal layer.** The cluster-scoped artifacts holding the captured bytes, their
  format, and their garbage collection are internals; they are not addressable with the permissions
  above and their shape is not a compatibility promise.
- **The working files of the `d8` command-line client.** Its downloads in progress, their staging
  directories and its resume machinery are its own; what a *finished* archive looks like is in
  [The snapshot archive](#the-snapshot-archive).
