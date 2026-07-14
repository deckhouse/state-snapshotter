# Demo / SDK code-quality contract

> Migrated from `.cursor/rules/demo-code-quality.mdc`. Applies to `images/domain-controller/**`
> and `pkg/snapshotsdk/**` (see also `pkg/snapshotsdk/CLAUDE.md`).

Demo (`images/domain-controller`) and the snapshot SDK (`pkg/snapshotsdk`) are **executable architecture documentation / reference implementation**. New engineers read them instead of ADRs and copy-paste them into new domain controllers. A reader must, from this code alone and in ~30 minutes (no ADR/design doc), be able to explain the **happy path, failure paths, ownership boundaries, and lifecycle** (§8). "It works" is not enough — it must be **impossible to misread**. These standards are **stricter than production**: demo code must be cleaner than prod (prod may carry legacy/compat hacks; demo never may).

## 1. Naming is architecture-grade (MUST)

A name must alone reveal: who owns the logic, the entity lifecycle, persistent-vs-temporary, domain-vs-infrastructure. If a name needs a verbal explanation, it's wrong.

- **Litmus:** if a function/type name can't be explained in one domain sentence without `raw`/`scratch`/`temp`/`dynamic`/`somehow`, rename it.
- **Banned outright in identifiers** (unless a real upstream type like `genericapiserver`): `scratch`, `temp`/`tmp`/`temporary`, `raw`, `helper`, `util`, `misc`, `stuff`, `blob`, `manager` (unless it truly owns a lifecycle).
- **Banned only as a bare, unqualified token** (fine when qualifying a concrete noun): `data`, `object`, `resource`, `generic`.
  - Bad: `Data`, `processObject`, `handleResource`, `ResourceManager`, `generic helper`.
  - Good: `SnapshotDataBinding`, `dataRef`, `resourceVersion`, `GenericSnapshotBinderController`, `generic snapshot controller`.
- **Banned magic verbs** unless the contract is obvious: `do`, `handle`, `process`, `manage`, `execute`.
- `scratch` is banned repo-wide unless code literally models CDI scratch-space (it doesn't): an empty/freshly provisioned disk is **blank**, not scratch.
- Examples — Bad: `reconcileScratchDisk`, `processObject`, `handleResource`. Good: `reconcileBlankDisk`, `reconcileRestoredDisk`, `buildBlankPVC`.

## 2. Package & file layout encodes architecture (MUST)

- A file name MUST match the primary type/role it holds (the file with `DemoVirtualDiskSnapshotReconciler` is `virtualdisksnapshot_controller.go`, not `disk_controller.go`).
- One naming convention per kind; don't mix short and long forms for the same concept.
- **Banned dumping-ground files:** `helpers.go`, `utils.go`, `common.go`, `misc.go`, `glue.go`. Group by concept, name the file after it.
- Test-only helpers live in `_test.go`; a non-`_test.go` file MUST NOT import `testing`.

## 3. No hidden magic (MUST)

- Forbidden: reflection, unnecessary dynamic type assertions, `unstructured.Unstructured`, raw GVK assembly, `map[string]any`/`interface{}` object bodies, magic strings for dispatch, silent fallbacks.
- Exception only at an **explicitly documented integration boundary** (foreign CRD with no Go types, the generic restore-manifest transform, the aggregated apiserver) — isolated behind a typed seam and documented as sanctioned (see `pkg/snapshotsdk/internal/storagefoundation`, `pkg/snapshotsdk/transform`).

## 4. Comments explain WHY, not WHAT (MUST)

- A comment deletable without losing meaning is garbage — delete it.
- Explain intent and lifecycle. Bad: `// create pvc`. Good: `// Creates the persistent backing PVC for a blank disk; its lifetime is bound to the DemoVirtualDisk via controller ownerRef.`

## 5. Domain / SDK separation is visually obvious (MUST)

If you ask "is this domain or SDK?", the boundary is leaking.
- **Domain owns:** source validation, child planning, domain-specific objects.
- **SDK owns:** conditions, ownerRefs, capture orchestration, request lifecycle.

## 6. Zero accidental abstractions (MUST)

No abstractions ahead of need. Forbidden: `ResourceManager`, `GenericHandler`, `ControllerHelper` and similar renamed util-bags. Every abstraction has a clear owner, an invariant, and a contract.

## 7. Capture invariants (MUST)

Capture cardinality is a contract, not a convention; code MUST NOT let a degenerate capture plan successfully.
- **Data capture:** at most one PVC per node — `VolumeCaptureSpec.DataRef` is a single optional pointer, never a slice. Multiple volumes are child snapshot nodes, never several data refs on one node.
- **Manifest capture:** the SDK does NOT inject the source object; supplying ≥1 manifest target is the domain's responsibility for an ordinary node. An empty final target set is legal only for the special aggregator case (an empty MCR converges to an empty, Ready ManifestCheckpoint — see `api/v1alpha1` ManifestCaptureRequestSpec.Targets); a domain leaf/single-object snapshot MUST always pass its own source identity.

## 8. Litmus

A new engineer opens the demo controller, reads no ADR, and within 30 minutes can explain **all four**:
- **happy path** — how a snapshot node is normally planned (manifest capture, data capture, child snapshot planning, then the planning barrier);
- **failure paths** — recoverable waiting (`ReportProgress` + requeue), terminal domain failure (`Fail`/`Reject`), core-owned leg failure (terminal Ready reason → stop);
- **ownership boundaries** — domain vs SDK vs core (who writes what);
- **lifecycle** — create/adopt, barrier commit, suppression after the core handoff.

If any of the four isn't clear from the code alone, the demo isn't done.
