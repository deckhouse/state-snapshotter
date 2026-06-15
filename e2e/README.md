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

## Requirements

The target cluster must run dev builds (via `modulePullOverride` in
`tests/cluster_config.yml`) of:

- `state-snapshotter` (the `SnapshotExport` CRD + controller are new)
- `storage-foundation` (VolumeRestoreRequest / VolumeCaptureRequest executors)
- `storage-volume-data-manager` (DataExport transport)

plus `snapshot-controller`, `sds-node-configurator` (thin provisioning) and
`sds-local-volume`.

## Run

```bash
# compile-check only (no cluster):
make build

# against an existing cluster:
export TEST_CLUSTER_CREATE_MODE=alwaysUseExisting
export SSH_HOST=<master-ip> SSH_USER=<user>
export TEST_CLUSTER_NAMESPACE=<base-vm-ns> TEST_CLUSTER_STORAGE_CLASS=<base-sc>
make test
# or one spec:
make test-focus FOCUS="exports a namespace snapshot"
```

See `make check-env` for the full list of environment variables.
