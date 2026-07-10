# state-snapshotter — project rules

> Migrated from `.cursor/rules/*.mdc`. Repo-wide (always-apply) rules live here;
> path-scoped rules live in nested `CLAUDE.md` files (`api/`, `images/state-snapshotter-controller/`,
> `images/domain-controller/`, `pkg/snapshotsdk/`).

## Go formatting (MUST)

- After changing Go code, run `gofmt` (or `go fmt`) on the modified files before finalizing.

## Go tests (MUST) — applies to any `*_test.go`

- Embed static fixtures with `//go:embed` into `[]byte`; do NOT read fixtures from disk at runtime unless embedding is impossible.
- Payload minimalism: only include fields the test asserts. Prefer small explicit bodies over helpers until a helper is reused in 3+ places.
- Use **Ginkgo** for tests in this project.
- Struct tags: include only the codec the test actually uses. Do NOT duplicate `json` and `yaml` tags unless both are parsed in the same path. Prefer field names; add a `yaml` tag only when the YAML key differs.
- Topology tests: parse YAML fixtures into existing structs without adding extra tags; embed `testdata/…` and unmarshal directly (no runtime I/O).

## Tooling (allowed without confirmation)

- `bash hack/generate_code.sh` (codegen after API changes)
- `cd api && go test ./...` (API tests)

When to run codegen / linter / e2e: see the API rules in `api/CLAUDE.md` and the redeploy/e2e section below.

## go-lint.sh (repo root) — MUST

1. Do NOT run a full `./go-lint.sh` on an **uncommitted** batch of tracked changes unless you intend to finish with a commit (incl. `git commit --amend` after `--fix`). The script uses `--fix` then fails if `git status --porcelain` is non-empty — a dirty tree is indistinguishable from "linter produced edits", so the exit is ambiguous.
2. Preferred order: commit (or a small WIP commit) → run `./go-lint.sh` → if `--fix` changed files, review `git diff` → `git commit --amend` (or follow-up commit) → rerun until it exits 0 and the tree is clean.
3. For linter feedback **while** editing without committing: run `golangci-lint run` directly from the module (see below), NOT `./go-lint.sh`. For compile checks use `go test`/`go build`.
4. If you won't commit soon, don't run `./go-lint.sh` at all.
5. On non-zero exit / "Linter requests changes", inspect `git diff`: `--fix` may have applied fixes — commit/amend/revert. The script does NOT run `git checkout -f` (no longer wipes the tree).

**CI parity — `GO_BUILD_TAGS`:** CI (`.github/workflows/go_checks.yaml`) sets `GO_BUILD_TAGS: "ce ee se seplus csepro"`. `./go-lint.sh` only lints inside `for edition in $GO_BUILD_TAGS`; if unset/empty the loop doesn't run and `images/*` are effectively not linted. Locally match CI:

```bash
export GO_BUILD_TAGS="ce ee se seplus csepro"
./go-lint.sh
```

Linter without the wrapper (WIP, single edition):

```bash
cd images/state-snapshotter-controller
golangci-lint run --build-tags ce ./...
```

## Run tests after edits (MUST)

- After code changes, run the appropriate tests before the final response. If tests can't be run, state why and what's missing.
- Levels: **Unit** (logic), **Envtest** (controllers), **E2E** (real cluster). When: business logic → unit + envtest; reconcile → unit + envtest + e2e subset; data-path → full e2e.
- Do NOT use `./go-lint.sh` on a dirty tracked tree as a substitute for committing.

## Controller redeploy & remote e2e (SSOT)

- **Where tests live:** integration + envtest-based e2e under `images/state-snapshotter-controller/test/` (there is no top-level `tests/e2e-go`). Orchestration and when to use cluster smoke: `docs/internal/state-snapshotter-rework/testing/e2e-testing-strategy.md`.
- **Before redeploy:** (1) unit tests `cd images/state-snapshotter-controller && go test ./pkg/... ./internal/...`; (2) linter `./go-lint.sh` with CI `GO_BUILD_TAGS`; (3) **if controller code changed: commit AND push first** (below), then redeploy and wait for rollout success.
- **After redeploy:** integration/e2e or cluster smoke as required by the strategy doc / CI.
- **Kubeconfig:** standard dev location (`~/.kube/config` / `KUBECONFIG`) — but for e2e nested-cluster debugging use the persistent tunnel below.

### Controller redeploy gate (MUST)

- Before redeploying the controller: create a git commit with the required changes, then push it to the remote of the current repository.
- Commit message MUST be minimal plain text — no extra tags/trailers/labels.
- Do NOT redeploy if commit or push hasn't completed.

## Distroless runtime policy (MUST)

- Runtime containers (controllers/helpers/jobs) MUST be distroless-compatible.
- Do NOT use shell entrypoints/wrappers (`/bin/sh`, `bash`, `sh -c`) in Pod/Job specs — invoke binaries directly (e.g. the controller binary from `images/state-snapshotter-controller/cmd`).
- Multi-step behavior → Go/helper binaries, not inline shell. For ad-hoc debug probes needing a shell, use a shell-friendly image (e.g. BusyBox), never the distroless controller image.

## RBAC source of truth (MUST)

- Do NOT use `// +kubebuilder:rbac` markers as an RBAC source in this module (prevents stale generated-RBAC hints). No such markers under `images/state-snapshotter-controller/internal/controllers/`.
- Static production controller RBAC is maintained by hand in `templates/controller/rbac-for-us.yaml` — update that file for core-permission needs.
- Domain/custom-resource RBAC is granted externally by the Deckhouse RBAC controller/hook and signaled via `DomainSpecificSnapshotController.status.conditions[AccessGranted=True]`. Do NOT add demo/domain CRs to production static RBAC — document them as external RBAC requirements.

## Restore rollout guard — CSI VolumeSnapshot (MUST)

Applies when the module implements/relies on CSI VolumeSnapshot restore. Restore intent with empty `spec.source` is **rollout-dependent** — do not assume it's safe without snapshot-controller support. Docs (spec/design/status) MUST document and satisfy:
- allow creating a VolumeSnapshot with empty `spec.source` for restore intent;
- allow one-shot fill of `spec.source` by the controller (e.g. `volumeSnapshotContentName`) after restore, then immutable;
- reconcile is a no-op while `spec.source` is empty.

Any change relying on this must preserve and document the snapshot-controller dependency.

## Delivery gating (MUST)

- Implement strictly within the currently agreed stage model in `docs/internal/state-snapshotter-rework/operations/project-status.md`. Each change declares its stage. Do NOT pull features from a later stage.
- After codegen (if API touched) and before moving to the next stage, run & pass the full test plan for the current stage.
- Regression guarantee: on Stage N, all tests from stages 0..N must pass. Fix regressions in the same change set — no "fix later".

## Docs: SSOT & boundaries (MUST)

Single source of truth per information type; others reference it. Root: `docs/internal/state-snapshotter-rework/`.

| Type | Responsibility |
|------|----------------|
| `spec/` | Normative contract only (state machines, keys, invariants) |
| `architecture/` | Diagrams and high-level flow; not normative |
| `design/` | Redesign/migration/rollout plan; does not become status |
| `adr/` | Why a decision was made; not edited as current spec |
| `testing/` | Test strategy/scenarios/run modes; references spec, does not copy it |
| `operations/` | High-level status only; no spec/design copy |

- Do NOT copy contract across documents. Separate clearly: **implemented / current target / planned / legacy**.
- Extended ADR drafts under `snapshot-rework/` (repo root) must keep normative summaries in sync with `spec/system-spec.md`; `snapshot-rework/` alone is NOT SSOT for implementable contract.
- Development order: **design → spec → tests → code.** If code and spec disagree, fix one — never leave inconsistent.
- Before changing code, read `spec/system-spec.md` (redesign → `design/`; touching a decision → `adr/`). On contract change: update spec, run e2e checks, add ADR if needed.
- Cross-doc consistency: when changing spec / architecture overview / implementation-plan / e2e-testing-strategy / project-status, check for contradictions (registry vs runtime watch activation; DSC conditions `Accepted`/`AccessGranted`/derived `Ready`; unified CRD bootstrap vs DSC-driven registry; manifest/MCR vs unified snapshot registry; stage/progress across the docs). Fix in the same change or document a temporary divergence + follow-up.
- Naming: docs `kebab-case.md`; ADR `adr-XXXX-short-title.md`; e2e scripts `NN_description.sh`.

## Operations status (MUST)

- Update `docs/internal/state-snapshotter-rework/operations/project-status.md` only when a change is **critical** to plan/status — not for minor/cosmetic changes.
- Keep it **high-level only**: stage status table, short implemented summary, in progress, planned, blockers/rollout dependencies. Do NOT put there: full state machine, long rollout steps, file-level TODOs, or spec/design/testing copy — those belong in `spec/` `design/` `testing/` `adr/`.

## E2E cluster access — persistent tunnel (127.0.0.1:6445)

When inspecting the e2e nested cluster (kubectl / debugging a failed/hung run), reach the API server through the **user-maintained** SSH tunnel at `https://127.0.0.1:6445`, NOT the transient e2e auto-tunnel.

- Auto-generated `/tmp/e2e/kubeconfig-<ip>.yml` point at a transient port that dies when the test process exits — they stop working right after a failing run.
- The persistent tunnel on `:6445` stays up after failure. Prefer it for post-failure debugging.

```bash
export KUBECONFIG=/tmp/e2e/kubeconfig-tunnel.yml
kubectl get ns
```

(Re)generate after a new run/cluster (certs rotate; SAN won't match `127.0.0.1`, so skip TLS verify):

```bash
for src in $(ls -t /tmp/e2e/kubeconfig-*.yml | grep -v tunnel); do
  sed -E -e 's#server: https://127\.0\.0\.1:[0-9]+#server: https://127.0.0.1:6445#' \
         -e '/certificate-authority-data:/d' \
         -e 's#( *)server: https://127.0.0.1:6445#\1server: https://127.0.0.1:6445\n\1insecure-skip-tls-verify: true#' \
         "$src" > /tmp/e2e/kubeconfig-tunnel.yml
  KUBECONFIG=/tmp/e2e/kubeconfig-tunnel.yml kubectl get ns >/dev/null 2>&1 && break
done
export KUBECONFIG=/tmp/e2e/kubeconfig-tunnel.yml
```

`/tmp/e2e/kubeconfig-tunnel.yml` holds credentials — lives only under `/tmp`, never commit it.

## Debugging e2e/controller issues — controller logs first

Start by reading state-snapshotter controller logs, not test output alone.

- Namespace (Deckhouse): `d8-state-snapshotter` (chart template `d8-{{ .Chart.Name }}`; if module name differs, use the actual namespace).
- Steps: (1) `kubectl logs -n d8-state-snapshotter … --tail=500`; (2) look for reconcile lines (Snapshot / SnapshotContent / ManifestCheckpoint, unified-bootstrap / skip GVK, `RESTMapping` / API errors); (3) correlate with cluster objects (CRDs, statuses, finalizers) via `kubectl describe`.
