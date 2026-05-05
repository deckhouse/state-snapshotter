# Snapshot Read API

Normative contract: [`../spec/snapshot-aggregated-read.md`](../spec/snapshot-aggregated-read.md).

## Generic Aggregated Endpoint

```text
GET /apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/{namespace}/{resource}/{name}/manifests
```

Examples:

```bash
curl -s \
  https://<api-host>/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/ns/snapshots/root/manifests

curl -s \
  https://<api-host>/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/ns/demovirtualmachinesnapshots/vm-1/manifests

curl -s \
  https://<api-host>/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/ns/demovirtualdisksnapshots/disk-a/manifests
```

The response is `application/json`: an array of Kubernetes manifest objects from the requested snapshot subtree.

## Legacy Snapshot Endpoint

This endpoint remains supported:

```text
GET /apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/{namespace}/snapshots/{name}/manifests
```

It has the same aggregated read semantics for `Snapshot` and is kept for backward compatibility.

## MCP-Level Endpoints

MCP endpoints return only one `ManifestCheckpoint` local archive:

```text
GET /api/v1/checkpoints/{mcp-name}/archive
GET /api/v1/checkpoints/{mcp-name}/info
GET /apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/manifestcheckpoints/{mcp-name}/manifests
```

Use the generic aggregated endpoint when the expected result is the full snapshot subtree.

## Errors

| Case | Status |
|------|--------|
| Snapshot not found | `404 Not Found` |
| `status.boundSnapshotContentName` is empty | `400 Bad Request` |
| Resource is not a registered snapshot resource | `400 Bad Request` |
| Cluster-scoped snapshot resource | `400 Bad Request` |
| Duplicate object in subtree | `409 Conflict` |
| Graph registry unavailable | `503 Service Unavailable` |

Duplicate object errors include:

```text
duplicate object detected in snapshot tree
```
