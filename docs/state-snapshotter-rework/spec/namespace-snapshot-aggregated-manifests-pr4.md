# NamespaceSnapshot aggregated manifests download (N2b PR4)

Normative contract for implementation and tests. **SSOT** for this HTTP surface; do not duplicate long fragments elsewhere — link here.

## 1. Scope

**In scope**

- Read-time aggregation of manifests
- Traversal of the **already materialized** manifests-only subgraph: follow **only** persisted `SnapshotContent` edges (no list-based reconstruction of the tree; two-stage model in [`system-spec.md`](system-spec.md) **§3.0**)
- HTTP endpoint for aggregated manifests
- Error semantics (fail-whole)
- Output format

**Out of scope**

- Data layer (PVC, VolumeSnapshot, etc.)
- Export/import workflows
- Pre-materialized archives
- **Capture-time** domain expansion (which objects belong in the snapshot, child `YyyySnapshot` wiring) — domain controllers; not this SSOT
- Heterogeneous **domain** edges beyond what this document normatively interprets for the SnapshotContent read-path (see PR5 / system-spec **§3**)

## 2. Source of truth

### 2.1 Root object

The request is initiated by **`NamespaceSnapshot`** (namespaced).

Root is resolved as:

```text
NamespaceSnapshot
  → status.boundSnapshotContentName
  → SnapshotContent (root)
```

If `boundSnapshotContentName` is empty → **409 Conflict**.

### 2.2 Traversal graph

This read-path assumes the snapshot graph for the run has **already** been built and published in **`status`** (system-spec **§3.0** v0). The handler **must not** infer subtree membership by listing API objects.

Traversal is performed **only** via:

`SnapshotContent.status.childrenSnapshotContentRefs[]`

Each ref contains **`name`** (cluster-scoped `SnapshotContent`).

**Important**

- `childrenSnapshotRefs` (on `NamespaceSnapshot`) are **not** used for this aggregated-manifests traversal.
- Only the persisted `SnapshotContent` ref graph is canonical for this endpoint.

## 3. Traversal algorithm

Recursive (**DFS** or **BFS**) walk over **`SnapshotContent`** nodes linked by **`childrenSnapshotContentRefs`** only (no other discovery rule in this contract).

### 3.1 Node processing

For each `SnapshotContent`:

1. Must exist → otherwise **404**
2. Must have non-empty `status.manifestCheckpointName` → otherwise **500**
3. **Read artifact:** call **`ArchiveService.GetArchiveFromCheckpoint`** for that ManifestCheckpoint (§4). **404** if ManifestCheckpoint not found; **409** if ManifestCheckpoint exists but is **not Ready** (same semantics as single-MCP `/manifests`, [`namespace-snapshot-controller.md`](../design/namespace-snapshot-controller.md) §8.7.1); **500** for chunk decode / checksum / other archive failures covered in §7.1.

**Relationship to SnapshotContent `Ready`:** the **authoritative** gate for «can we read this node’s manifests» is **ManifestCheckpoint readiness**, enforced inside **`GetArchiveFromCheckpoint`** (step 3). The handler **MAY** additionally return **409** when `SnapshotContent` `Ready` is not `True` **if** that is consistent with N2b status semantics in design (**§4.4**, **§11**); that check is **not** a substitute for step 3.

### 3.2 Cycle protection

Traversal **MUST** maintain `visited` := set of `SnapshotContent` names. If a node is visited twice → **500 InternalError** (cycle detected).

### 3.3 Ordering

- **Traversal order:** children sorted by **`name`** (lexicographically).
- **Final object order:** `[parent MCP objects] + [child1 subtree] + [child2 subtree] + …`
- No additional global sorting is required.

## 4. Data retrieval

For each node, **`ArchiveService.GetArchiveFromCheckpoint`** is used.

### 4.1 Reuse rules

- **MUST** reuse `GetArchiveFromCheckpoint`.
- **MUST NOT** reimplement chunk decoding.
- **MUST NOT** access chunks directly.

## 5. Aggregation

### 5.1 Merge strategy

Each MCP returns a JSON array of objects. Aggregation: **`final = concat(all arrays)`** (in traversal order, §3.3).

### 5.2 Duplicate handling

Objects are identified by **`(apiVersion, kind, namespace, name)`**. If a duplicate is detected → **500 InternalError**.

## 6. HTTP API

### 6.1 Endpoint

```http
GET /apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/{namespace}/namespacesnapshots/{name}/manifests
```

### 6.2 Response

**Success:** `200 OK`, `Content-Type: application/json`

Body:

```json
[
  { "apiVersion": "...", "kind": "...", "metadata": { ... }, ... },
  ...
]
```

### 6.3 Compression

If the request includes `Accept-Encoding: gzip`, the response **MUST** use `Content-Encoding: gzip` (reuse the same pattern as single-MCP `/manifests`).

## 7. Error semantics (fail-whole)

This endpoint is **fail-whole**.

### 7.1 Errors

| Case | Code |
|------|------|
| `NamespaceSnapshot` not found | 404 |
| `SnapshotContent` not found | 404 |
| `ManifestCheckpoint` not found | 404 |
| `ManifestCheckpoint` not Ready (enforced via `GetArchiveFromCheckpoint`) | 409 |
| `SnapshotContent` not ready for capture (optional early check; only if aligned with N2b design §4.4 / §11) | 409 |
| Missing `manifestCheckpointName` | 500 |
| Chunk decode / checksum error | 500 |
| Duplicate objects | 500 |
| Cycle detected | 500 |

### 7.2 Important rule

Failure of **any** subtree node → the **entire** request fails. **No** partial results.

## 8. Limits

### 8.1 Per-checkpoint limits

Inherited from `ArchiveService`:

- `maxObjectsPerCheckpoint`
- `maxArchiveSizeBytes` (warning only)

### 8.2 Aggregated limits

**No** global limits are enforced in PR4 (conscious scope cut: per-checkpoint limits still apply per node via §8.1; aggregate caps / response-size policy are future hardening).

## 9. Non-goals

- No partial subtree responses
- No streaming protocol definition beyond existing patterns
- No pagination
- No data-layer integration
- No optional/required child semantics for this endpoint

## 10. Implementation notes

### 10.1 Reuse

**MUST reuse**

- `ArchiveService`
- JSON archive format (array of objects)

**MUST NOT reuse**

- `restore.Resolver` directly (different resource model)

### 10.2 New components

Implementation requires:

- New use case: aggregated namespace manifests
- New HTTP handler
- Read-path logic: walk persisted **`SnapshotContent`** edges only (system-spec **§3.0** stage 2)

### 10.3 Readiness checks

**MUST:** fail the request with **409** when **`GetArchiveFromCheckpoint`** reports ManifestCheckpoint not Ready (step §3.1.3). **MAY:** fail with **409** on SnapshotContent `Ready!=True` when that matches N2b design; **never** skip step 3 in favor of SnapshotContent `Ready` alone.

## 11. Compatibility

- Does not modify existing endpoints
- Does not change Ready semantics on resources
- Backward compatible for clients that do not call this path

## 12. Future work (not in PR4)

- Partial subtree responses
- Optional/required children semantics for download
- Deterministic **global** ordering of the flat array (beyond §3.3)
- Pre-materialized archives
- Data-layer aggregation
