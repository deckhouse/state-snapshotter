# E2E tests for state-snapshotter (+ demo domain)

Real-cluster end-to-end coverage for the unified snapshot flows: capture of a
demo VM/disk snapshot tree, the aggregated subresource APIs
(`manifests-download` / `manifests-with-data-restoration` /
`manifests-and-children-refs-upload`), manifest-level restore, the export ->
import round-trip, TTL/GC cascade, the full volume-data flow (phase 3), backup-system
HTTP download via aggregated manifests + SVDM `DataExport` (phase 4), and backup-system
restore import via `DataImport` into another namespace (phase 5) — all without d8-cli.

The suite installs the `state-snapshotter` module with `enableDemoDomain: true`
on a nested Deckhouse cluster brought up by
[storage-e2e](../../../../e2e/repos/storage-e2e), mirroring the structure of the
`sds-elastic` e2e suite.

## Phases

The specs run inside a single ordered `Describe`, registered by builder
functions in dependency order:

1. **Phase 1 & 2 - manifest-only flow** (`captureSpecs`, `aggregatedApiSpecs`,
   `namespaceCaptureReworkSpecs`, `restoreSpecs`, `importSpecs`, `gcSpecs`): apply
   the manifest-only source (an ownerless ConfigMap plus a single manifest-only
   `DemoVirtualMachine`), create a root `Snapshot`, assert the `Snapshot` /
   `SnapshotContent` and the demo child snapshot reach Ready, read the aggregated
   APIs, restore the manifests into a fresh namespace, run the export -> import
   round-trip, and exercise the root TTL/GC cascade. Generic-object discovery
   (RBAC/Service/Deployment/etc.) is covered by `namespaceCaptureReworkSpecs`, not
   the capture fixture. All these cheap specs share one `captured` tree (gc uses
   its own short-TTL sub-tree) and need only `state-snapshotter` (no volume-data
   leg).
2. **Phase 3 - full volume-data flow** (`volumeDataSpecs`, env-gated by
   `E2E_VOLUME_DATA`): provision a thin, snapshot-capable StorageClass via
   `storage-e2e/pkg/testkit.EnsureDefaultStorageClass` (which auto-enables
   `sds-node-configurator` + `sds-local-volume`), capture PVC data, restore via
   `VolumeRestoreRequest`, and assert the marker bytes survive. Needs
   `storage-foundation` (enabled in `tests/cluster_config.yml`).
3. **Phase 4 - backup-system HTTP download** (`backupDownloadSpecs`, env-gated by
   `E2E_VOLUME_DATA`): provision Block volumes (orphan PVC + two
   `DemoVirtualDisk` scratch PVCs), write data via dedicated block-writer pods,
   attach `DemoVirtualMachine` to one disk while the other stays standalone,
   capture a snapshot, download manifests via the aggregated `manifests-download`
  API (compared to live cluster objects), and download volume bytes via
  storage-foundation `DataExport` from an in-cluster backup-client pod (Bearer
  auth + `GET /api/v1/block`, sha256 compared to source). Needs
  `storage-foundation` (enabled in `tests/cluster_config.yml`).
4. **Phase 5 - backup-system restore import** (`backupRestoreSpecs`, env-gated by
   `E2E_VOLUME_DATA`, chained from phase 4): reshape the captured tree for the
   import upload path (VM manifest folded into root; three data leaves), POST
   manifests via `manifests-and-children-refs-upload`, upload volume bytes via
   storage-foundation `DataImport` (`PUT /api/v1/block` + `POST /api/v1/finished`
   from the phase-4 backup pod), wait for the imported tree Ready, restore-apply
   into a fresh namespace, and verify manifests + block checksums. Needs
   `storage-foundation`.

## Module dependency note

`e2e/go.mod` consumes `storage-e2e` via a committed **local** `replace` while the
testkit StorageClass helpers and `ApplyClient`/`ExecInPod` it relies on are not
yet in a tagged release. Once they are published, drop the `replace` and pin a
pseudo-version. `state-snapshotter/api` is always consumed via
`replace github.com/deckhouse/state-snapshotter/api => ../api`.

## Requirements

- Go **1.26+**
- A base Deckhouse cluster with the `virtualization` module enabled.
- SSH access to the master node of the base cluster.
- A Deckhouse license and a docker config for the dev registry.
- For phase 3: a block-mode `StorageClass` on the base cluster for VM disks (the
  thin in-cluster SC is then built on top by the suite).

## Environment variables

### `storage-e2e` (nested cluster)

- `TEST_CLUSTER_CREATE_MODE` (**required**): one of `alwaysCreateNew`,
  `alwaysUseExisting`, `commander`.
- `TEST_CLUSTER_CLEANUP`: set to `true` to delete the VMs after the run.
- `TEST_CLUSTER_NAMESPACE`: the VM namespace on the base cluster (used by the
  phase-3 VirtualDisk attach).
- `TEST_CLUSTER_STORAGE_CLASS`: base-cluster `StorageClass` for VM disks (phase 3).
- `YAML_CONFIG_FILENAME`: defaults to `cluster_config.yml`.
- `SSH_HOST`, `SSH_USER`, `SSH_PRIVATE_KEY`
- `SSH_PUBLIC_KEY`: SSH public key injected as the VMs' authorized key.
  **Required in `alwaysCreateNew`**.
- `SSH_VM_USER`: SSH user inside the created VMs. **Required in `alwaysCreateNew`**.
- `SSH_JUMP_HOST`, `SSH_JUMP_USER`, `SSH_JUMP_KEY_PATH`: jump-host settings for
  `alwaysUseExisting`.
- `DKP_LICENSE_KEY`, `REGISTRY_DOCKER_CFG`
- `<MODULE>_MODULE_PULL_OVERRIDE`: per-module override of the `modulePullOverride`
  tag declared in `tests/cluster_config.yml` (storage-e2e's
  `ApplyModulePullOverrideEnv`; module name upper-cased, `-`/`.` → `_`). Each
  config module that pins a literal `main` default can be independently pointed
  at a `prN` / `mrN` / `main` image **without editing the committed YAML**:
  - `STATE_SNAPSHOTTER_MODULE_PULL_OVERRIDE` — the module under test; this is the
    one you normally set (to your PR/MR tag).
  - `STORAGE_FOUNDATION_MODULE_PULL_OVERRIDE` — only when co-developing
    `storage-foundation` (the extended-VS fork + phase-3 data-leg backend +
    the `DataImport`/`DataExport` volume data-transport engine).

  Unset modules keep the literal `main` from the YAML. The phase-3 storage
  backends (`sds-node-configurator` + `sds-local-volume`) are enabled at runtime
  by `testkit.EnsureDefaultStorageClass` from the release channel and carry **no**
  `modulePullOverride`, so they are not pinned by these vars.

### state-snapshotter suite knobs

- `E2E_SNAPSHOTTER_NS_PREFIX`: prefix for the source/restore namespaces the suite
  creates. Defaults to `snap-e2e`.
- `E2E_SNAPSHOT_READY_TIMEOUT`: Go duration bounding Snapshot/SnapshotContent
  readiness waits. Defaults to `10m`.
- `E2E_MODULE_READY_TIMEOUT`: Go duration bounding module + demo CSD readiness.
  Defaults to `15m`.
- `E2E_GC_TTL`: `snapshotRootOkTtl` applied for the GC spec. Defaults to `60s`.
- `E2E_VOLUME_DATA`: when truthy (`true`/`1`/`yes`), runs phases 3-5 (full
  volume-data flow, backup download, and backup restore). Off by default (phases 1-2 only).
- `E2E_GET_LOAD`: when truthy, runs the opt-in GET-load measurement spec (REST
  GET-load delta across the capture wave, scraped from the leader controller's
  `/metrics`). Off by default even when `E2E_VOLUME_DATA` is set, because the
  repeat-and-average run adds several minutes. It provisions its own thin
  StorageClass, so it does not need `E2E_VOLUME_DATA`, but it does need the same
  base-cluster knobs (`TEST_CLUSTER_NAMESPACE` / `TEST_CLUSTER_STORAGE_CLASS`).
  Tuning knobs:
  - `E2E_GET_LOAD_ITERATIONS`: capture waves to run back-to-back over a shared
    source (default `5`).
  - `E2E_GET_LOAD_WARMUP`: leading waves run but excluded from the mean to drop
    cold-cache bias (default `1`).
  - `E2E_GET_LOAD_MAX_PER_SEC`: when set, hard-bound the MEAN GET/sec (leave unset
    for the baseline run; set it to the baseline figure for the new run).
- `E2E_STORAGE_CLASS`: the thin, snapshot-capable StorageClass the suite
  provisions/uses for phase 3. Defaults to `e2e-thin`.
- `E2E_PROBE_IMAGE`: container image (must ship `sh` + `cat`) for the PVC
  round-trip probe Pods. Defaults to `busybox:1.36`.
- `E2E_BACKUP_CLIENT_IMAGE`: container image for the in-cluster backup-client pod
  (must ship `sh`, `curl`, `head`, and `sha256sum`). Used by phases 4-5. Defaults to
  `curlimages/curl:8.11.1` (Alpine/busybox, which provides all four).
- `E2E_KEEP_CLUSTER_ON_FAILURE`: when truthy, and at least one spec failed, skip
  nested-cluster teardown so the live cluster can be inspected. Off by default.
- `E2E_KEEP_CLUSTER`: when truthy, skip per-spec resource cleanup (namespaces, pods,
  import trees, DataExports, etc.) regardless of pass/fail, so restored/imported
  resources survive for live inspection. Off by default. Note: the nested-cluster
  connection (SSH tunnels, cluster lock) is still torn down; inspect via your own
  kubeconfig.

## Quick start

```bash
export TEST_CLUSTER_CREATE_MODE=alwaysCreateNew
export TEST_CLUSTER_CLEANUP=true
export TEST_CLUSTER_NAMESPACE=e2e-state-snapshotter
export TEST_CLUSTER_STORAGE_CLASS=linstor-r2

export SSH_HOST=<master-ip>
export SSH_USER=<ssh-user>
export SSH_PRIVATE_KEY=~/.ssh/id_rsa
export SSH_PUBLIC_KEY=~/.ssh/id_rsa.pub
export SSH_VM_USER=cloud

export DKP_LICENSE_KEY=<license>
export REGISTRY_DOCKER_CFG=<base64-docker-config>
export STATE_SNAPSHOTTER_MODULE_PULL_OVERRIDE=main   # module under test; or prN / mrN
# Optional — only when co-developing these dependency modules (default main):
# export STORAGE_VOLUME_DATA_MANAGER_MODULE_PULL_OVERRIDE=prN
# export STORAGE_FOUNDATION_MODULE_PULL_OVERRIDE=prN

# Phases 1-2 only:
cd e2e
make deps
make test

# Phases 3-5 (volume-data + backup download + backup restore) as well:
E2E_VOLUME_DATA=true make test

# Opt-in GET-load measurement only (provisions its own SC; baseline = log only):
E2E_GET_LOAD=true make test-focus FOCUS="GET-load measurement"
# New image, hard-bound the mean against the baseline figure:
E2E_GET_LOAD=true E2E_GET_LOAD_MAX_PER_SEC=<baseline> make test-focus FOCUS="GET-load measurement"
```

Run a subset:

```bash
make test-focus FOCUS="captures the demo snapshot tree"
```

## Compile check (no cluster)

```bash
make deps
make build   # go test -c (compiles the spec binary)
make vet
```
