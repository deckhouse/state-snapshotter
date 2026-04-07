To use this module, execute
```
go get github.com/deckhouse/MODULE/api@main. 
```

The pseudo-tag will be generated automatically.

Please note that model changes will NOT BE APPLIED in dependent modules until you rerun go get (the pseudo-tag points to a specific commit).
Therefore, it is important to remember to apply go get in all external modules that use this models.

!Also, DO NOT FORGET to update the models when the CRD are changed!

## Regenerating `DeepCopy` and CRD YAML

After changing types under `api/v1alpha1`, run from the **repository root**:

```bash
./hack/generate_code.sh
```

This runs `controller-gen` (`object` for `zz_generated.deepcopy.go`, `crd` for manifests under `crds/`). Ensure `controller-gen` is on `PATH`, or the script installs `sigs.k8s.io/controller-tools/cmd/controller-gen@v0.14.0` into `$(go env GOPATH)/bin`.