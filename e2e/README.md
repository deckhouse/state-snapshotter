# state-snapshotter Snapshot export e2e

A nested-cluster end-to-end suite (Ginkgo v2 + Gomega) built on the shared
[`storage-e2e`](../../../../e2e/repos/storage-e2e) framework. It is a separate Go
module (`github.com/deckhouse/state-snapshotter/e2e`) with a local `replace` onto
the framework checkout, mirroring `sds-elastic/e2e`.

This is distinct from the in-process envtest suite under
`images/state-snapshotter-controller/test/e2e/` — that one runs against a fake
apiserver; this one runs against a real Deckhouse cluster.

## What the suite does

In one ordered spec it:

1. Provisions a Thin `sds-local-volume` StorageClass via
   `testkit.CreateDefaultStorageClass` (LVMType `Thin`).
2. Creates a test namespace with a random name (`snap-e2e-<rand>`).
3. Applies a `CustomSnapshotDefinition` mapping DemoVirtualMachine/DemoVirtualDisk
   to their `*Snapshot` kinds and waits for it to become eligible
   (`Accepted=True`, `RBACReady=True`).
4. Creates a `Pod` + `PersistentVolumeClaim` (the residual/orphan PVC that becomes
   the export's data leg) and waits for the PVC to bind and the pod to be Ready.
5. Creates a `DemoVirtualMachine` with an owned `DemoVirtualDisk` plus a standalone
   (unowned) `DemoVirtualDisk`.
6. Snapshots the namespace via a `Snapshot` (`spec: {}`) and waits for `Ready=True`.
7. Creates a `SnapshotExport` referencing the root `Snapshot` and waits for
   `Ready=True` (reason `Published`) and `DataReady=True`.

The suite intentionally does NOT delete the test namespace or tear the cluster
down — inspect the artifacts afterwards.

> **Module dependency order.** During development `e2e/go.mod` carries a local
> `replace github.com/deckhouse/storage-e2e => ../../../../e2e/repos/storage-e2e`.
> Once `storage-e2e` is tagged, drop the `replace` and pin the suite to the
> published pseudo-version.

## Supported run mode

Only the nested-cluster mode driven by `storage-e2e` is supported
(`TEST_CLUSTER_CREATE_MODE` must be set).

## Requirements

- Go **1.26+**
- A base Deckhouse cluster with the `virtualization` module enabled (the nested
  cluster runs as VMs on it).
- SSH access to the master node of the base cluster.
- A Deckhouse license and a docker config for the dev registry.
- A `StorageClass` on the base cluster for VM disks (OS disk + the raw disks the
  thin `sds-local-volume` helper attaches at runtime).
- Dev builds (via `modulePullOverride`) of `state-snapshotter`,
  `storage-foundation` and `storage-volume-data-manager` that carry the
  SnapshotExport CRD/controller, the `volumeMode` VRR/VCR contract and the
  DataExport transport. See [Module image overrides](#module-image-overrides).

## Environment variables

### `storage-e2e` (nested cluster)

- `TEST_CLUSTER_CREATE_MODE` (**required**):
  one of `alwaysCreateNew`, `alwaysUseExisting`, `commander`.
- `TEST_CLUSTER_CLEANUP`:
  set to `true` to delete the VMs after the run. This suite never deletes the
  test namespace or the SnapshotExport regardless of this value.
- `TEST_CLUSTER_NAMESPACE`:
  the VM namespace on the base cluster. Also used as the base for the thin-SC
  disk attach (`VMNamespace`). The in-cluster test namespace itself is random
  (`snap-e2e-<rand>`), independent of this value.
- `TEST_CLUSTER_STORAGE_CLASS`:
  base-cluster `StorageClass` for VM disks (OS + raw disks for the thin pool).
- `YAML_CONFIG_FILENAME`:
  defaults to `cluster_config.yml`.
- `SSH_HOST`, `SSH_USER`, `SSH_PRIVATE_KEY`
- `SSH_PUBLIC_KEY`:
  path to (or inline content of) the SSH public key injected as the VMs'
  authorized key. **Required in `alwaysCreateNew` mode** — set it explicitly
  (e.g. `~/.ssh/id_rsa.pub`); the suite errors with `SSH_PUBLIC_KEY is not set`
  during VM creation if it is empty.
- `SSH_VM_USER`:
  SSH user inside the created VMs (must match the VM image, usually `cloud`).
  **Required in `alwaysCreateNew` mode** — `storage-e2e` does not apply a
  default, so an empty value makes the post-create SSH fail with
  `unable to authenticate [none publickey]` and `SSH_VM_USER (current="")`.
- `SSH_JUMP_HOST`, `SSH_JUMP_USER`, `SSH_JUMP_KEY_PATH`:
  jump-host (bastion) SSH settings used by `alwaysUseExisting`. When the target
  cluster master is only reachable through a bastion, set `SSH_HOST`/`SSH_USER`
  to the **target cluster master** and these to the bastion. Ignored by
  `alwaysCreateNew`.
- `TEST_CLUSTER_FORCE_LOCK_RELEASE`:
  set to `true` to steal a stale `e2e-cluster-lock` ConfigMap left in the
  `default` namespace by a previous crashed/`Ctrl+C` run. Only use when you are
  sure no other run is using the cluster.
- `DKP_LICENSE_KEY`
- `REGISTRY_DOCKER_CFG`

### Module image overrides

`storage-e2e` resolves a per-module env override for the `modulePullOverride`
field in `tests/cluster_config.yml` (which keeps a literal `main` default). The
env key is `<MODULE>_MODULE_PULL_OVERRIDE`, where the module name is upper-cased
and every non-`[A-Z0-9]` character becomes `_`. When set, it replaces the static
tag at config-load time (storage-e2e logs both the static tag and the override);
when unset, the literal `main` from the YAML is used. In-YAML `${VAR}`
placeholders are rejected, so this env channel is the only way to override.

The three product modules under test must point at a build that carries the new
contract (the SnapshotExport CRD/controller, the `volumeMode` VRR/VCR executors,
and the DataExport transport) — `main` will NOT have them yet:

- `STATE_SNAPSHOTTER_MODULE_PULL_OVERRIDE`:
  overrides `state-snapshotter` (the `SnapshotExport` CRD + controller). Set to
  the branch/PR build of `snapshot-export-import-crs` (e.g. `prN` on GitHub,
  `mrN` on GitLab).
- `STORAGE_FOUNDATION_MODULE_PULL_OVERRIDE`:
  overrides `storage-foundation` (VolumeRestoreRequest / VolumeCaptureRequest
  executors with the new `volumeMode`/`fsType`/`accessModes` contract).
- `STORAGE_VOLUME_DATA_MANAGER_MODULE_PULL_OVERRIDE`:
  overrides `storage-volume-data-manager` (DataExport transport).

The supporting modules default to `main` and rarely need an override, but the
same convention applies:

- `SNAPSHOT_CONTROLLER_MODULE_PULL_OVERRIDE`
- `SDS_NODE_CONFIGURATOR_MODULE_PULL_OVERRIDE`
- `SDS_LOCAL_VOLUME_MODULE_PULL_OVERRIDE`

### Suite knobs

- `E2E_THIN_STORAGE_CLASS`:
  name of the Thin `sds-local-volume` StorageClass the suite creates and binds
  the app PVC to. Defaults to `e2e-thin-local`.
- `E2E_PROBE_IMAGE`:
  container image (must ship `sh`) for the PVC-mounting pod that supplies the
  export's data leg. Defaults to `busybox:1.36`.
- `E2E_PVC_SIZE`:
  size of the app PVC. Defaults to `1Gi`.
- `E2E_NS_PREFIX`:
  prefix of the random test namespace. Defaults to `snap-e2e-`.
- `CI`:
  when set (non-empty), the suite bounds the whole run with a 180m Ginkgo
  timeout.

## Quick start

```bash
export TEST_CLUSTER_CREATE_MODE=alwaysCreateNew
export TEST_CLUSTER_CLEANUP=true
export TEST_CLUSTER_NAMESPACE=e2e-state-snapshotter
export TEST_CLUSTER_STORAGE_CLASS=linstor-r2

export SSH_HOST=<master-ip>
export SSH_USER=<ssh-user>
export SSH_PRIVATE_KEY=~/.ssh/id_rsa
export SSH_PUBLIC_KEY=~/.ssh/id_rsa.pub   # required for alwaysCreateNew (VM authorized key)
export SSH_VM_USER=cloud                  # required for alwaysCreateNew (SSH user inside the VMs)

export DKP_LICENSE_KEY=<license>
export REGISTRY_DOCKER_CFG=<base64-docker-config>

# Point the three product modules at the snapshot-export-import-crs build.
# (Replace prN/mrN with the actual PR/MR/tag; "main" will not have the new CRDs.)
export STATE_SNAPSHOTTER_MODULE_PULL_OVERRIDE=prN
export STORAGE_FOUNDATION_MODULE_PULL_OVERRIDE=prN
export STORAGE_VOLUME_DATA_MANAGER_MODULE_PULL_OVERRIDE=prN

# Optional suite knobs (defaults shown):
export E2E_THIN_STORAGE_CLASS=e2e-thin-local
export E2E_PROBE_IMAGE=busybox:1.36
export E2E_PVC_SIZE=1Gi
export E2E_NS_PREFIX=snap-e2e-

cd e2e
make deps
make test
```

To run against an already-provisioned cluster, swap the create mode (the
`SSH_PUBLIC_KEY`/`SSH_VM_USER` requirements drop, jump-host vars may apply):

```bash
export TEST_CLUSTER_CREATE_MODE=alwaysUseExisting
export SSH_HOST=<master-ip> SSH_USER=<ssh-user> SSH_PRIVATE_KEY=~/.ssh/id_rsa
# ... module overrides as above ...
make test
```

For local debugging you can run a subset of specs:

```bash
make test-focus FOCUS="exports a namespace snapshot"
```

Run `make check-env` to print the resolved values of the relevant variables.

## Compile check (no cluster)

```bash
make build
make vet
```
