To use this module, execute
```
go get github.com/deckhouse/MODULE/api@main. 
```

The pseudo-tag will be generated automatically.

Please note that model changes will NOT BE APPLIED in dependent modules until you rerun go get (the pseudo-tag points to a specific commit).
Therefore, it is important to remember to apply go get in all external modules that use this models.

!Also, DO NOT FORGET to update the models when the CRD are changed!

## Regenerating `DeepCopy` and CRD YAML

After changing types under `api/v1alpha1`, `api/storage/v1alpha1`, or `api/demo/v1alpha1`:

```bash
./hack/generate_code.sh
```

Run from the **repository root**, or invoke this script by path from any directory — it resolves `api/` and `crds/` relative to the repo root (same idea as in the `backup` module).

Both generators are pinned in `hack/tools/go.mod` and built into a temporary directory, so the version matches `controller-gen.kubebuilder.io/version` on the generated CRDs regardless of what exists on `PATH`, and the pin is covered by `hack/tools/go.sum`. To bump one, see the comment at the top of that file.

The CRD half of the generation lives in `hack/generate_crds.sh`, which `generate_code.sh` calls: it renders the manifests with `controller-gen` and then runs [`crd-enricher`](https://github.com/deckhouse/deckhouse/tree/main/pkg/crd-enricher) over the same packages to fold in what `controller-gen` cannot emit (the `x-doc-*` documentation fields, description overrides for vendored subtrees such as `metav1.Condition`, and CRD-level normalisations), driven by `+crd-enricher:` markers next to the structs. With no such markers present the enricher is a no-op.

That script name is a contract: the shared `.crds_generated_check` CI job runs `hack/generate_crds.sh` and fails the pipeline when the regenerated `crds/` differs from what is committed, so the manifests cannot drift away from the types.