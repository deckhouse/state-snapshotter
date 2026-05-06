# Snapshot Aggregated Read API

**Status:** normative contract.

This document defines the aggregated manifest read contract for snapshot restore points.

## Restore Point Model

Any registered `XxxSnapshot` is a restore point. A root `Snapshot`, a domain snapshot, and a leaf snapshot all resolve to a content node through `status.boundSnapshotContentName`.

Aggregated read MAY start from any snapshot node or directly from any content node. Snapshot-starting reads first resolve:

```text
Snapshot -> status.boundSnapshotContentName -> SnapshotContent
```

For the compatibility `snapshots/{name}/manifests` route, if the live root `Snapshot` is already deleted, retained read MAY resolve by deleted Snapshot identity while the root retention chain exists:

```text
ObjectKeeper.spec.followObjectRef -> deleted Snapshot namespace/name
SnapshotContent.ownerRef -> ObjectKeeper
```

Exactly one root `SnapshotContent` MUST match that ObjectKeeper. Zero matches returns `404 NotFound`; multiple matches return `409 Conflict`. If the ObjectKeeper has `deletionTimestamp` but the content still exists, the route may keep reading until Kubernetes GC removes the content.

Content-starting reads use the given content node as the traversal root.

## HTTP API

The generic namespaced snapshot endpoint is:

```text
GET /apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/{namespace}/{resource}/{name}/manifests
```

`{resource}` is the plural resource name of a registered namespaced snapshot type. The API layer resolves `{resource}/{name}` to the exact registered snapshot GVK, reads the snapshot in `{namespace}`, resolves its bound content GVK through the graph registry, and runs the same aggregated read usecase.

The existing `Snapshot` endpoint remains backward-compatible:

```text
GET /apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/{namespace}/snapshots/{name}/manifests
```

It is a compatibility route over the same aggregated content graph model.

## Traversal

Aggregated read traverses the materialized content graph only:

```text
Snapshot
  -> status.boundSnapshotContentName
  -> SnapshotContent
  -> status.manifestCheckpointName
  -> ManifestCheckpoint archive objects
  -> status.childrenSnapshotContentRefs
  -> child SnapshotContent nodes
```

For each visited content node, the usecase reads `status.manifestCheckpointName`, loads the referenced `ManifestCheckpoint` archive, appends its objects to the response, and then follows `status.childrenSnapshotContentRefs`.

Every materialized content node MUST have an MCP. The MCP MAY contain zero objects; an empty root own scope is represented by an empty MCP, not by a missing `manifestCheckpointName`.

Traversal MUST NOT rediscover the tree by listing resources in a namespace. Controllers materialize the graph and publish refs; aggregated read only consumes the saved graph.

## Duplicate Objects

Object identity for aggregation is:

```text
apiVersion | kind | namespace | name
```

If two MCP archives in one subtree contain the same identity, the whole aggregated read MUST fail. The API MUST NOT merge, overwrite, or silently deduplicate duplicate objects.

The HTTP surface for duplicate object identity is `409 Conflict`.

## Identity Requirements

Aggregated read returns namespace-relative Kubernetes object manifests from MCP archives. Every returned top-level object MUST have:

- `apiVersion`;
- `kind`;
- `metadata.name`;

Aggregated output MUST omit `metadata.namespace` for namespaced objects so the manifest can be applied into the target namespace with `kubectl -n <target> apply -f ...`. Cluster-scoped objects MUST NOT be returned by aggregated read. MCP storage may still keep the original namespace for identity, dedup, and audit.

Synthetic/demo marker objects without stable identity MUST NOT be used as materialized restore manifests. Demo snapshot controllers materialize source domain objects directly (`DemoVirtualDisk`, `DemoVirtualMachine`) so aggregated output uses normal Kubernetes object identity.

## Local MCP Reads

MCP-level archive/read endpoints return only the manifests stored in that MCP. They do not aggregate child MCPs and do not follow `childrenSnapshotContentRefs`.

Combining parent and child MCP objects is the responsibility of the aggregated read API/usecase only.

## Responsibility Boundary

Aggregation is an API/usecase responsibility. Controllers create snapshots, contents, MCR/MCP artifacts, and graph refs. Clients call the API; they must not reconstruct the snapshot tree or merge MCPs themselves.
