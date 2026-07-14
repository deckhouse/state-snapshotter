# snapshot SDK code-quality contract

> Migrated from `.cursor/rules/demo-code-quality.mdc`. The SDK and the demo domain-controller share
> one contract — the **full text lives in [`images/domain-controller/CLAUDE.md`](../../images/domain-controller/CLAUDE.md)**.
> Read it before editing here. SDK-specific highlights below.

`pkg/snapshotsdk` is **reference-implementation / executable architecture documentation** — cleaner than production, impossible to misread. Names must reveal owner/lifecycle/persistence/domain-vs-infra without explanation; no dumping-ground files (`helpers.go`, `utils.go`, `common.go`); comments explain WHY not WHAT.

## Domain / SDK separation (MUST)

The boundary must be visually obvious:
- **SDK owns:** conditions, ownerRefs, capture orchestration, request lifecycle.
- **Domain owns:** source validation, child planning, domain-specific objects.

## No hidden magic (MUST)

No reflection / `unstructured.Unstructured` / raw GVK assembly / `map[string]any` bodies / magic-string dispatch / silent fallbacks. The only sanctioned exceptions are typed, documented integration seams — `pkg/snapshotsdk/internal/storagefoundation` and `pkg/snapshotsdk/transform`.

## Capture invariants (MUST)

- **Data capture:** `VolumeCaptureSpec.DataRef` is a single optional pointer (≤1 PVC per node), never a slice; extra volumes are child snapshot nodes.
- **Manifest capture:** the SDK does NOT inject the source object — supplying ≥1 manifest target is the domain's responsibility for an ordinary node. An empty final target set is legal only for the special aggregator case (an empty MCR converges to an empty, Ready ManifestCheckpoint — see `api/v1alpha1` ManifestCaptureRequestSpec.Targets).

See the full 8-point contract (naming bans, file layout, no accidental abstractions, the 30-minute litmus) in `images/domain-controller/CLAUDE.md`.
