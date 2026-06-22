# E2E tests for state-snapshotter (+ demo domain)

Real-cluster end-to-end coverage for the unified snapshot flows: capture of a
demo VM/disk snapshot tree, the aggregated subresource APIs
(`manifests-download` / `manifests-with-data-restoration` /
`manifests-and-children-refs-upload`), manifest-level restore, the export ->
import round-trip, TTL/GC cascade, and (phase 3) the full volume-data flow.

The suite installs the `state-snapshotter` module with `enableDemoDomain: true`
on a nested Deckhouse cluster brought up by
[storage-e2e](../../../../e2e/repos/storage-e2e), mirroring the structure of the
`sds-elastic` e2e suite.

## Phases

The specs run inside a single ordered `Describe`, registered by builder
functions in dependency order:

1. **Phase 1 - manifest-only capture/restore** (`captureSpecs`,
   `aggregatedApiSpecs`, `restoreSpecs`): apply a PVC-free demo source, create a
   root `Snapshot`, assert the `Snapshot` / `SnapshotContent` and demo child
   snapshots reach Ready, read the aggregated APIs, and restore the manifests
   into a fresh namespace. Needs only `state-snapshotter` + `snapshot-controller`
   (no CSI snapshot support).
2. **Phase 2 - export -> import + GC/TTL** (`importSpecs`, `gcSpecs`): walk the
   captured tree, export each node's manifests, reconstruct it through the import
   upload path, then exercise the root TTL/GC cascade.
3. **Phase 3 - full volume-data flow** (`volumeDataSpecs`, env-gated by
   `E2E_VOLUME_DATA`): provision a thin, snapshot-capable StorageClass via
   `storage-e2e/pkg/testkit.EnsureDefaultStorageClass` (which auto-enables
   `sds-node-configurator` + `sds-local-volume`), capture PVC data, restore via
   `VolumeRestoreRequest`, and assert the marker bytes survive. Needs
   `storage-foundation` (enabled in `tests/cluster_config.yml`).

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
- `STATE_SNAPSHOTTER_MODULE_PULL_OVERRIDE`: overrides `modulePullOverride` for
  `state-snapshotter` from `tests/cluster_config.yml` (which keeps a literal
  `main` default). Set to `prN` / `mrN` / `main`. This is storage-e2e's generic
  per-module convention `<MODULE>_MODULE_PULL_OVERRIDE`.

### state-snapshotter suite knobs

- `E2E_SNAPSHOTTER_NS_PREFIX`: prefix for the source/restore namespaces the suite
  creates. Defaults to `snap-e2e`.
- `E2E_SNAPSHOT_READY_TIMEOUT`: Go duration bounding Snapshot/SnapshotContent
  readiness waits. Defaults to `10m`.
- `E2E_MODULE_READY_TIMEOUT`: Go duration bounding module + demo CSD readiness.
  Defaults to `15m`.
- `E2E_GC_TTL`: `snapshotRootOkTtl` applied for the GC spec. Defaults to `60s`.
- `E2E_VOLUME_DATA`: when truthy (`true`/`1`/`yes`), runs phase 3 (full
  volume-data flow). Off by default (phases 1-2 only).
- `E2E_STORAGE_CLASS`: the thin, snapshot-capable StorageClass the suite
  provisions/uses for phase 3. Defaults to `e2e-thin`.
- `E2E_PROBE_IMAGE`: container image (must ship `sh` + `cat`) for the PVC
  round-trip probe Pods. Defaults to `busybox:1.36`.
- `E2E_KEEP_CLUSTER_ON_FAILURE`: when truthy, and at least one spec failed, skip
  nested-cluster teardown so the live cluster can be inspected. Off by default.

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
export STATE_SNAPSHOTTER_MODULE_PULL_OVERRIDE=main   # or prN / mrN

# Phases 1-2 only:
cd e2e
make deps
make test

# Phase 3 (full volume-data flow) as well:
E2E_VOLUME_DATA=true make test
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
