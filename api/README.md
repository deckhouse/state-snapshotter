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

The script always runs `go install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.18.0` and then invokes **`$(go env GOPATH)/bin/controller-gen`** (same pattern as the `backup` module), so the version matches `controller-gen.kubebuilder.io/version` on generated CRDs even if another `controller-gen` exists elsewhere on `PATH`.