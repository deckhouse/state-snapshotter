# snapshot SDK code-quality contract

> Migrated from `.cursor/rules/demo-code-quality.mdc`, where the **full text lives**
> ([`../../.cursor/rules/demo-code-quality.mdc`](../../.cursor/rules/demo-code-quality.mdc)).
> Read it before editing here. SDK-specific highlights below. (The reference demo domain controller
> that follows the same contract now lives in the `sds-unified-snapshots-poc` repo.)

`pkg/snapshotsdk` is **reference-implementation / executable architecture documentation** — cleaner than production, impossible to misread. Names must reveal owner/lifecycle/persistence/domain-vs-infra without explanation; no dumping-ground files (`helpers.go`, `utils.go`, `common.go`); comments explain WHY not WHAT.

## Domain / SDK separation (MUST)

The boundary must be visually obvious:
- **SDK owns:** conditions, ownerRefs, capture orchestration, request lifecycle.
- **Domain owns:** source validation, child planning, domain-specific objects.

## No hidden magic (MUST)

No reflection / `unstructured.Unstructured` / raw GVK assembly / `map[string]any` bodies / magic-string dispatch / silent fallbacks. The only sanctioned exceptions are typed, documented integration seams — `pkg/snapshotsdk/internal/storagefoundation` and `pkg/snapshotsdk/transform`.

## Capture invariants (MUST)

- **Data capture:** `VolumeCaptureSpec.DataRef` is a single optional pointer (≤1 PVC per node), never a slice; extra volumes are child snapshot nodes.
- **Manifest capture — never empty:** the SDK does NOT inject the source object — supplying ≥1 manifest target is the domain's responsibility. Every snapshot MUST capture at least its own source object's manifest (a single-object domain snapshot passes its own source identity; the namespace-root aggregator always includes its own Namespace object), so the target set is never empty. `EnsureManifestCapture` fails closed with `ErrEmptyManifest` before any cluster mutation, and the MCR CRD rejects an empty `spec.targets` via CEL.
- **Manifest capture — independent leg, frozen set:** the manifest and volume legs are INDEPENDENT declarations; the SDK never derives or injects the data-leg PVC into the manifest targets. A domain that wants a PVC's YAML captured lists that PVC in `Targets` explicitly (alongside its own source object). The set is frozen once the MCR exists: a changed in-flight set is fail-closed `ErrManifestTargetsDrift` (recommended reaction `Fail(GraphPlanningFailed)`); an identical/reordered re-declaration is a no-op.

See the full 8-point contract (naming bans, file layout, no accidental abstractions, the 30-minute litmus) in `.cursor/rules/demo-code-quality.mdc`.
