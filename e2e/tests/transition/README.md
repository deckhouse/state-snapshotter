# Transition e2e — snapshot-controller + svdm → state-snapshotter + storage-foundation

Manual, developer-run scenario that verifies the consolidation of the legacy snapshot stack
(`snapshot-controller` + `storage-volume-data-manager`) into `state-snapshotter` +
`storage-foundation` **on one dev cluster**, without deleting the legacy workload at the flip.

It is a **separate Ginkgo suite** (own `cluster_config.yml`, own bootstrap) because the main
state-snapshotter suite brings its cluster up with `storage-foundation`/`state-snapshotter`
already enabled — the opposite of what this scenario needs. All module lifecycle
(enable / MPO-retag / order) is driven at runtime from the test.

## Scope

- **In scope:** module Helm-guard behaviour (legacy modules stop rendering everything but their
  deprecation alert once storage-foundation is enabled) **and the firing deprecation alerts
  themselves** (built-in `ModuleIsDeprecated` + the custom `D8*ModuleDeprecated` for both modules);
  the svdm legacy→v0.2.0 migration hook (CR migration to the new API group — asserted on a real
  in-flight `DataExport` — legacy CRD removal, legacy finalizer sweep incl. PVCs) plus
  **svdm-D1-standalone serving a new-group export before the flip** and **its clean teardown**
  (deleting the migrated export recovers the source PVC from `Lost`); CSI snapshot / DataExport-
  DataImport / restore data integrity across the flip, **a full new-group DataExport+DataImport
  served by storage-foundation after the flip**, and the existing state-snapshotter e2e on the same
  cluster after the flip.
- **Out of scope (tested by the runtime team, covered by canary channel rollout):** Deckhouse
  `requirements.deckhouse`/`requirements.modules` gating, `ModuleRelease` Pending→activation,
  bundle auto-enable. This suite runs on a **dev** Deckhouse build, which does not enforce
  requirements — so `>= 1.76` and `storage-foundation >= 1.0.0` gates are intentionally NOT
  exercised here.

## Running

```bash
cd e2e
E2E_RUN_TRANSITION=true \
TEST_CLUSTER_CREATE_MODE=<as for the main suite> \
  <plus the image-tag vars below> \
  go test ./tests/transition/... -v -timeout 180m
```

Without `E2E_RUN_TRANSITION=true` the suite is skipped entirely (nothing bootstraps). It is **not**
wired into CI — run it by hand from a workstation with dev-registry access.

### Reading the progress output

Run with `-v` (as above): every long wait is self-narrating. Each `By(...)` step prints the phase it
is in, and the waits emit `[transition HH:MM:SS] …` lines that poll every **3s** and log the current
state **immediately, then every 15s**, and once on success. So a hang is diagnosable from the trail
rather than an opaque timeout. In particular the Phase-B import step reports, at each tick:

- the `DataImport` conditions (e.g. `UploadFinished=True(...) Ready=True(...)`),
- the target PVC phase (`imported-data=Pending|Bound`),
- any populator staging PVC (`prime-<uid>=Pending`),
- the namespace's pods with a not-ready hint (`importer-…=Pending[…:ContainerCreating]`).

On timeout the failure message carries that same last-observed state. The whole import (upload →
importer `UploadFinished` → populator rebind → target PVC Bound) is bounded by
`E2E_TRANSITION_PROBE_TIMEOUT` (default `10m`).

## Environment variables

The scenario pins every module image via `ModulePullOverride.spec.imageTag`. Tags are chosen by
the runner (PR tags such as `pr123`/`mr456`, or `main`); nothing is hard-coded. `snapshot-controller`
and `svdm` need **scenario** variables (svdm needs two slots — the legacy old-group image and the
v0.2.0/D1 new-group image); the rest use storage-e2e's standard `<MODULE>_MODULE_PULL_OVERRIDE`.

| Variable | Type | Phase | Role |
|---|---|---|---|
| `E2E_RUN_TRANSITION` | scenario gate | all | must be `true`, else the whole suite is skipped |
| `SDS_NODE_CONFIGURATOR_MODULE_PULL_OVERRIDE` | standard | A (bootstrap) | sds-node-configurator image |
| `E2E_TRANSITION_SNAPSHOT_CONTROLLER_TAG` | scenario | B | snapshot-controller image — **must be the v0.2.0 handoff build** (`Deprecated` stage + `D8SnapshotControllerModuleDeprecated` alert), NOT `main`: the deprecation-alert assertion requires it |
| `E2E_TRANSITION_SVDM_LEGACY_TAG` | scenario | B | svdm image on the OLD `storage.deckhouse.io` group (pre-D1) |
| `SDS_LOCAL_VOLUME_MODULE_PULL_OVERRIDE` | standard | B | sds-local-volume image (enabled after snapshot-controller) |
| `E2E_TRANSITION_SVDM_TAG` | scenario | C | svdm v0.2.0/D1 image — MPO is retagged to this, triggering the migration hook |
| `STATE_SNAPSHOTTER_MODULE_PULL_OVERRIDE` | standard | C | state-snapshotter image (new stack) |
| `STORAGE_FOUNDATION_MODULE_PULL_OVERRIDE` | standard | C | storage-foundation image (new stack) |
| `E2E_TRANSITION_STORAGE_CLASS` | scenario | B–D | snapshot-capable StorageClass for the data-plane PVCs; **unset ⇒ all data-plane steps are skipped** |
| `E2E_TRANSITION_VS_CLASS` | scenario | B–D | VolumeSnapshotClass for the CSI snapshots; **unset ⇒ all data-plane steps are skipped** |
| `E2E_TRANSITION_PROBE_IMAGE` | scenario | B–D | probe-pod image (needs `sh` + `sha256sum`); default `busybox:1.36` |
| `E2E_TRANSITION_PROBE_TIMEOUT` | scenario | B–D | Go duration bounding how long a probe pod may take to reach Running. For the import probe this budgets the WHOLE import completion (upload → importer `UploadFinished` → populator rebind → target PVC Bound → schedule). Default `10m` |

### Example

```bash
export E2E_RUN_TRANSITION=true

# Phase A (bootstrap): only sds-node-configurator is enabled from cluster_config.yml.
export SDS_NODE_CONFIGURATOR_MODULE_PULL_OVERRIDE="main"

# Phase B (legacy stack, driven at runtime):
# snapshot-controller MUST be the v0.2.0 handoff build (Deprecated stage + deprecation alert),
# e.g. the feat/deprecate-handoff PR tag — NOT main. The suite asserts the deprecation alerts fire.
export E2E_TRANSITION_SNAPSHOT_CONTROLLER_TAG="pr<N of the v0.2.0 handoff PR>"
export E2E_TRANSITION_SVDM_LEGACY_TAG="<dev tag of a pre-D1 svdm build>"
export SDS_LOCAL_VOLUME_MODULE_PULL_OVERRIDE="main"

# Phase C (migrate svdm + flip to the new stack):
export E2E_TRANSITION_SVDM_TAG="pr<N>"                        # svdm D1 branch build
export STATE_SNAPSHOTTER_MODULE_PULL_OVERRIDE="pr<N>"
export STORAGE_FOUNDATION_MODULE_PULL_OVERRIDE="pr<N>"

# Data-plane (needed to exercise PVC/VS/export/import/restore; unset ⇒ those steps are skipped):
export E2E_TRANSITION_STORAGE_CLASS="e2e-thin"
export E2E_TRANSITION_VS_CLASS="e2e-local-thin"
# export E2E_TRANSITION_PROBE_TIMEOUT="15m"                   # default 10m; bump for slow clusters
```

> Legacy-image caveat: after the svdm D1 branch is merged to `main`, no `main` build carries the
> OLD API group any more, and dev-registry cleanup may drop old per-commit tags. Run this scenario
> **before** merging D1 (legacy = a pre-D1 tag, D1 = the PR tag), or pin a still-present legacy tag
> for `E2E_TRANSITION_SVDM_LEGACY_TAG`.

## Phases

- **A — bootstrap:** dev cluster with only `sds-node-configurator`; the four snapshot-stack modules
  are preseeded `enabled: false` (auto-activation blocker); assert the cluster is clean (no
  snapshot-stack workloads/namespaces).
- **B — legacy stack:** enable `snapshot-controller` then `svdm` (legacy image) and
  `sds-local-volume`; create a PVC + pod, write deterministic data (checksum), create a CSI
  `VolumeSnapshot` and wait ready+bound; DataExport a PVC and the VolumeSnapshot and download over
  the svdm HTTP API (not `d8`); DataImport/upload into new PVCs; CSI-restore a PVC from the
  snapshot; verify every checksum; keep everything.
- **C — migrate + flip:** retag the svdm MPO legacy→`E2E_TRANSITION_SVDM_TAG` (D1) and verify the
  migration hook (legacy CRDs removed, legacy finalizers incl. on PVCs swept, **and the real
  in-flight `DataExport` migrated onto the unified group with its `targetRef` preserved**). Then,
  **while storage-foundation is still off**, prove svdm-D1-standalone serves a fresh new-group
  DataExport (download + checksum) and **tear down the migrated export cleanly** — deleting it must
  recover the source PVC from `Lost` to `Bound` (svdm restores the reassigned PV), so no live export
  crosses the flip. Then enable `state-snapshotter` → `storage-foundation` **without disabling** the
  legacy modules; assert the legacy modules render no workload (all Deployments/Services gone) and
  that **all four deprecation ClusterAlerts fire** (built-in `ModuleIsDeprecated` + custom
  `D8*ModuleDeprecated`, for both modules).
- **D — invariants:** every shared CRD (CSI `volumesnapshots`/`…contents`/`…classes` +
  unified `dataexports`/`dataimports.storage-foundation.deckhouse.io`) stays **Established with the
  same UID captured just before the flip** — proving the handoff re-applies them in place, not
  delete+recreate (which would cascade-delete instances). The served schemas are checked for their
  storage-foundation marker fields (`spec.mode` on VolumeSnapshot, `targetRef.group` on DataExport,
  `spec.mode` on DataImport) so the served CRD is the extended/unified shape, not a vanilla
  reinstall. Full byte-for-byte CRD-manifest parity vs the repo YAML is **not** an e2e concern (the
  API server augments the live CRD with defaults/pruning/managedFields, so a manifest hash would
  never match) — it is verified by **storage-foundation CI** (`hack/check-consumer-crds.sh`, which
  clones snapshot-controller/svdm at their latest tag and diffs `crds/`). Additionally:
  the legacy ready+bound VolumeSnapshot is untouched (no new-domain labels/status); all checksums
  still match, incl. a fresh CSI restore from the legacy snapshot after the flip; a brand-new
  PVC/VS reaches ready+bound under the new controller; **a full new-group DataExport+DataImport is
  served end-to-end by storage-foundation** (export → download → import → checksum); then the
  existing state-snapshotter e2e passes on the same cluster. The deeper state-snapshotter *domain*
  path (Snapshot + `processed`/`managed` + SnapshotContent via the d8/domain SDK) is left to that
  suite.
