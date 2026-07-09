# state-snapshotter-controller rules

> Migrated from `.cursor/rules/controller-*.mdc`, `labels-finalizers.mdc`,
> `intent-classification.mdc`, `testing-levels-and-strategy.mdc`.
> Repo-wide rules (lint, redeploy gate, tests, distroless, RBAC) are in the root `CLAUDE.md`.

## Envtest (local)

Integration tests under `test/integration/` use envtest (etcd + kube-apiserver); they don't run without binaries.

```bash
export KUBEBUILDER_ASSETS="$HOME/.envtest-bin/k8s/1.33.0-darwin-arm64"   # adjust version/arch/path
export PATH="$HOME/go/bin:$PATH"                                          # optional: run setup-envtest anywhere
cd images/state-snapshotter-controller && go test -tags integration ./test/integration/... -count=1
```

`KUBEBUILDER_ASSETS` points at the dir containing `etcd`, `kube-apiserver`, `kubectl` (e.g. `setup-envtest use 1.33.0 --arch amd64`). If assets are missing, BeforeSuite fails with a clear message (no silent skip). See `docs/internal/state-snapshotter-rework/testing/e2e-testing-strategy.md`.

## Controller file structure (`internal/controllers/`)

- One `<name>_controller.go` per controller; `Reconcile(...)` and `SetupWithManager(...)` in the same file.
- In-file sections (SHOULD): `// SetupWithManager` (wiring only), `// Reconcile` (orchestration), `// Helpers`.
- **Patch-base discipline (MUST):** before any patch/update capture `base := obj.DeepCopy()` and patch from the base to avoid clobbering unrelated fields.
- **Job idempotency (MUST):** identify Jobs/ephemeral resources by **labels** (+ optional owner refs), not name alone. Cleanup selectors must be restart-safe (label-based); label keys are constants in `pkg/snapshot` or the spec.
- Shared code: `helpers.go` / `constants.go` / `errors.go`.
- Do NOT introduce a split `controller.go` / `reconciler.go` layout.

## Runtime conventions (`internal/controllers/`, `pkg/snapshot/`)

- **Enum-like values (MUST):** all string enums (condition Types/Reasons, phase/status strings, finalizers) are constants in `pkg/snapshot` (or a domain package) — no string literals in controllers or tests.
- **CRD status (MUST):** our CRD statuses use **conditions only** (no custom phase/status strings). First-class resources: unified Snapshot / SnapshotContent (unstructured), ManifestCheckpoint / MCR — use `pkg/snapshot` condition & finalizer constants; follow `docs/internal/architecture/controller-pattern.md`.
- **CSI VolumeSnapshot (MUST):** keep state in **annotations** (phase strings). Do NOT write custom state into `VolumeSnapshot.status`; do NOT emulate conditions. Update only metadata (labels/annotations/finalizers) via `client.Patch(ctx, obj, client.MergeFrom(base))` with `base := obj.DeepCopy()` — no `Update()` unless a comment explains why it's safe.
- **System metadata precedence (MUST):** system labels/annotations are authoritative; user templates must not override them (system wins on conflict).
- **Label/value limits (MUST):** shorten any label value that can exceed limits via a deterministic hash; same for VS names (≤253).
- **Events (MUST):** on a Failed terminal condition on our CRDs, emit a Warning Event with a short reason (+ useful refs/selectors). Running events optional.
- **Restore success** follows the bound user VolumeSnapshot state, not annotations alone; `readyToUse` may gate work but is not a long-lived phase field we own.

## Reconcile gating / intent classification

- Preconditions for progressing unified Snapshot / SnapshotContent reconcile (domain handled, refs valid, optional CRDs present) are defined in `docs/internal/state-snapshotter-rework/spec/system-spec.md`, the ADR under `snapshot-rework/`, and `docs/internal/architecture/controller-pattern.md`.
- When preconditions are NOT met: do NOT run the heavy path (artifact creation, readiness-assuming status writes); prefer early return / no-op; emit a Warning Event when it aids operators.
- Ambiguous/invalid input: same rule — no partial inconsistent state. Use `pkg/snapshot` constants for condition types/reasons and finalizers; spec + architecture docs are the source of truth for classification.

## Labels & finalizers (`pkg/snapshot/`, `internal/controllers/`)

- Define label/annotation keys as **constants in `pkg/snapshot`** (or a single domain package), never string literals in controllers. Reuse prefixes: `storage.deckhouse.io/`, `snapshot.deckhouse.io/`, `state-snapshotter.deckhouse.io/`. Do NOT change existing label string values (stability).
- Define finalizers as constants in `pkg/snapshot`; use suffix `<name>.finalizers.deckhouse.io`. Do NOT change existing finalizer values (stability) — exception: before first release/GA renaming is allowed ("existing" = released/used in production, not merely present in a branch).
- Reasons/states/condition Types/Reasons MUST be constants (no inline literals except in constant-definition tests). If the spec or API adds a new key, add the constant in the same change set.

## Testing levels

- Execution policy (where, redeploy, local/remote, e2e suite): root `CLAUDE.md` + `docs/internal/state-snapshotter-rework/testing/e2e-testing-strategy.md`.
- Levels: Unit (logic), Envtest (controllers), E2E (real cluster). Unit/envtest run locally under `test/`; cluster smoke (`./test-smoke.sh`) when applicable; remote-only suites only if documented in the strategy doc or CI.
- Scenario groups: unified snapshots, manifest/MCR, future DSC/registry, webhooks. Adding tests: check `e2e-testing-strategy.md`, avoid duplicates, add to the right group. Removing: only if the scenario is gone/replaced, then update the strategy doc.
- E2E strategy must support: automated remote validation, environment prep (MinIO/S3, controller ns, storage class), and generation of ordered demo YAML artifacts. Do not remove diagnostic/inspection scenarios without an explicit replacement.
