# API package rules (`api/v*/`)

> Migrated from `.cursor/rules/api-*.mdc`. Applies when editing Go under `api/v*/`.

## API change checklist (MUST)

- Review the API schema against the agreed plan: required resources present; drop resources not in the target model.
- Validate field markers/annotations: required fields have **no** `omitempty`; validation markers present where needed.
- Shared types must live in `common_types.go` — no duplication.
- Run codegen (MUST): `bash hack/generate_code.sh` after API changes — blocks moving forward until done.
- Run API tests (MUST): `cd api && go test ./...` — do NOT proceed until green.
- Treat codegen + API tests as automatic follow-up to any API change.

## Codegen & generated files (MUST)

- Any new/changed API object/type or `// +kubebuilder:*` marker (validation, printcolumns, subresources, …) MUST be followed by `bash hack/generate_code.sh` (run from repo root), with regenerated outputs in the same change.
- **API-only refactor stage exception:** if changes outside `api/` are temporarily forbidden (rest of repo won't compile yet), you may defer CRD regeneration (`crds/`) to the cross-repo refactor stage — but keep `api/v1alpha1` internally consistent and compilable; prefer object/deepcopy generation only, never hand-edit generated files.
- Do NOT hand-edit `zz_generated*` files (e.g. `api/v1alpha1/zz_generated.deepcopy.go`). Change the source types/markers and re-run generation.

## Kubebuilder markers (MUST)

- Prefer standard markers (`Enum`, `MinLength`, `Minimum`, `MinItems`, `Required`) when they suffice.
- `+kubebuilder:validation:XValidation` (CEL → `x-kubernetes-validations`) IS allowed and used for
  invariants standard markers can't express — cross-field / transition rules (e.g. immutability on
  `api/storage`, the `ManifestCaptureRequest` non-empty-`targets` rule on `api/v1alpha1`). Reach for CEL
  only when a standard marker cannot express the constraint.
- **CEL needs an integration test (MUST):** a CEL rule is enforced by the kube-apiserver, and a
  fake-client unit test bypasses it — so it proves nothing. Any added/changed CEL rule requires an
  envtest under `images/state-snapshotter-controller/test/integration/` asserting apiserver
  reject-on-violation + accept-on-valid. See `.cursor/rules/cel-requires-integration-test.mdc`.

## File structure (MUST/SHOULD)

- Each API object in its own `<object>_types.go` (e.g. `manifestcheckpoint_types.go`) — one root object per file.
- `doc.go` for package docs + group/version markers; `register.go` for scheme registration.

## Type layout in `<object>_types.go` (MUST)

Order: `type <Obj> struct` → `type <Obj>List struct` → `type <Obj>Spec struct` → spec-local types/helpers → `type <Obj>Status struct` → status-local types/helpers. `<Obj>List` appears immediately after `<Obj>`.

- Helpers stay pure and local to the object type; non-trivial/domain logic belongs outside `*_types.go`.
- A type used by more than one API object MUST live in `common_types.go` — no duplication.
- Required fields MUST NOT use `omitempty`; add validation markers (`MinLength`, `Enum`, …) for required scalars.
- API-level enums/constants live in the `api` package; controller/domain enums live in the `domain` package.
