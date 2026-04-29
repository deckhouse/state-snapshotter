# Snapshot Read Model

**Status:** design note, non-normative.  
**Normative contract:** [`../spec/snapshot-aggregated-read.md`](../spec/snapshot-aggregated-read.md).

## Concept

Every snapshot resource is a restore point. A root `NamespaceSnapshot`, a child domain snapshot, and a leaf snapshot all point to a `SnapshotContent`-like object through `status.boundSnapshotContentName`.

The content graph is the read model:

```text
Snapshot -> SnapshotContent -> ManifestCheckpoint -> childrenSnapshotContentRefs -> child SnapshotContent ...
```

`NamespaceSnapshot` is not special for aggregation. It uses the same model as any other `XxxSnapshot`; the legacy `NamespaceSnapshot` endpoint remains for compatibility.

## Aggregation

Aggregated read starts from one content node and walks the subtree through `status.childrenSnapshotContentRefs`.

For each visited content node:

1. Read `status.manifestCheckpointName`.
2. Load the referenced `ManifestCheckpoint` archive.
3. Append its Kubernetes manifest objects to the response.
4. Recurse into `childrenSnapshotContentRefs`.

Heterogeneous children are resolved through the snapshot graph registry. Snapshot Kind must be unique across registered snapshot types in the graph registry. The API layer resolves `resource/name` to a snapshot GVK, maps it to the registered content GVK, then reuses `BuildAggregatedJSONFromContent(...)`.

Aggregation is an API/usecase responsibility. Controllers materialize their own MCPs and publish graph refs; clients do not reconstruct the tree themselves.

## Namespace-Relative Output

Namespace root capture is namespaced-only. It does not capture the Kubernetes `Namespace` object and does not use an annotation-based MCR webhook exception for cluster-scoped targets. Aggregated read output is namespace-relative: namespaced objects omit `metadata.namespace`, and restore namespace lifecycle is outside the snapshot.

## Duplicate Objects

Object identity is:

```text
apiVersion | kind | namespace | name
```

If the same object appears in more than one MCP in the subtree, aggregation fails. The API must not merge, overwrite, or silently deduplicate objects.

The expected HTTP response is `409 Conflict` with a message containing:

```text
duplicate object detected in snapshot tree
```

## Scope

The generic read endpoint supports namespaced snapshot resources. Cluster-scoped snapshot resources are not implemented for this API surface yet.
